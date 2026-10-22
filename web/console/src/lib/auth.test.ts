import { afterEach, describe, expect, it, vi } from 'vitest'
import { Api } from './api'
import { Session, SessionEnded } from './auth'
import type { Config } from './types'

afterEach(() => {
  vi.unstubAllGlobals()
  sessionStorage.clear()
})

const config: Config = {
  platform: 'hugr', issuer: 'https://login.example.org/tenant/v2.0', clientID: 'console', scopes: [],
  audiences: { nodes: 'api://nodes', tresor: 'api://tresor' }, mounts: {}, links: [],
}

/** a Session as after a sign-in: the IdP's user in memory, its token endpoint known - no IdP is reached */
function signedIn() {
  const session = new Session(config)
  const internals = session as unknown as {
    user: { access_token: string; refresh_token?: string; expired: boolean }
    m: { metadataService: { getTokenEndpoint(): Promise<string> }; signinSilent(): Promise<unknown> }
  }
  internals.user = { access_token: 'nodes-token', refresh_token: 'refresh', expired: false, profile: { sub: 'alice' } } as never
  internals.m.metadataService.getTokenEndpoint = async () => 'https://login.example.org/token'
  return { session, internals }
}

const tokenAnswer = (token: string, expiresIn: number) =>
  new Response(JSON.stringify({ access_token: token, expires_in: expiresIn }), { status: 200 })

describe('a token per audience', () => {
  it("gives the nodes the sign-in's token and a resource its own, by the refresh token with its .default scope", async () => {
    const { session } = signedIn()
    const bodies: URLSearchParams[] = []
    vi.stubGlobal('fetch', vi.fn(async (_url: string, init: RequestInit) => {
      bodies.push(init.body as URLSearchParams)
      return tokenAnswer('tresor-token', 3600)
    }))
    expect(await session.getToken('api://nodes')).toBe('nodes-token')
    expect(await session.getToken('api://tresor')).toBe('tresor-token')
    expect(bodies).toHaveLength(1)
    expect(bodies[0].get('grant_type')).toBe('refresh_token')
    expect(bodies[0].get('scope')).toBe('api://tresor/.default offline_access')
    expect(bodies[0].get('client_id')).toBe('console')
    // held while it lives, asked again on a renewal
    expect(await session.getToken('api://tresor')).toBe('tresor-token')
    expect(bodies).toHaveLength(1)
    await session.getToken('api://tresor', { renew: true })
    expect(bodies).toHaveLength(2)
  })

  it('asks again within a minute of the held token’s expiry, once for requests refused together', async () => {
    const { session } = signedIn()
    let asked = 0
    vi.stubGlobal('fetch', vi.fn(async () => {
      asked++
      return tokenAnswer(`t${asked}`, 30) // less than the minute's margin
    }))
    expect(await session.getToken('api://tresor')).toBe('t1')
    expect(await session.getToken('api://tresor')).toBe('t2')
    const together = await Promise.all([session.getToken('api://tresor', { renew: true }),
      session.getToken('api://tresor', { renew: true })])
    expect(together).toEqual(['t3', 't3'])
  })

  it('ends the session when the refresh is refused or the user is gone', async () => {
    const { session, internals } = signedIn()
    vi.stubGlobal('fetch', vi.fn(async () => new Response('{"error":"invalid_grant"}', { status: 400 })))
    await expect(session.getToken('api://tresor')).rejects.toBeInstanceOf(SessionEnded)
    internals.user.expired = true
    await expect(session.getToken('api://nodes')).rejects.toBeInstanceOf(SessionEnded)
  })
})

describe('another person in the same page (spec 009)', () => {
  const asSet = (s: Session) => s as unknown as { set(u: unknown): void }
  it("drops the last person's tokens of their own audience when another signs in, keeps them for the same", async () => {
    const { session } = signedIn()
    let asked = 0
    vi.stubGlobal('fetch', vi.fn(async () => tokenAnswer(`t${++asked}`, 3600)))
    expect(await session.getToken('api://tresor')).toBe('t1')
    // the same person signed in again (a renewal): the held token stays
    asSet(session).set({ access_token: 'n2', refresh_token: 'r2', expired: false, profile: { sub: 'alice' } })
    expect(await session.getToken('api://tresor')).toBe('t1')
    // another person: asked afresh, by their refresh token
    asSet(session).set({ access_token: 'n3', refresh_token: 'r3', expired: false, profile: { sub: 'bob' } })
    expect(await session.getToken('api://tresor')).toBe('t2')
  })
  it('never hands out a token asked for the last person once another signed in', async () => {
    const { session } = signedIn()
    let release: (r: Response) => void = () => undefined
    vi.stubGlobal('fetch', vi.fn(() => new Promise<Response>((resolve) => { release = resolve })))
    const pending = session.getToken('api://tresor')
    await Promise.resolve()
    asSet(session).set({ access_token: 'n2', refresh_token: 'r2', expired: false, profile: { sub: 'bob' } })
    release(tokenAnswer('alice-tresor', 3600))
    await expect(pending).rejects.toBeInstanceOf(SessionEnded)
  })
})

describe('the sign-in loop', () => {
  it('knows a sign-in of moments ago, not one of long ago', async () => {
    const { session } = signedIn()
    expect(session.signedInJustNow).toBe(false)
    sessionStorage.setItem('hugr.sign-in-at', String(Date.now() - 1000))
    expect(session.signedInJustNow).toBe(true)
    sessionStorage.setItem('hugr.sign-in-at', String(Date.now() - 10 * 60_000))
    expect(session.signedInJustNow).toBe(false)
  })

  it('a renewal that fails is an unauthorized answer: the page decides, no raw error only', async () => {
    const tokens = { getToken: vi.fn(async (_a: string, how?: { renew?: boolean }) => {
      if (how?.renew) throw new SessionEnded()
      return 'stale'
    }) }
    vi.stubGlobal('fetch', vi.fn(async () => new Response('{"error":"unauthenticated"}', { status: 401 })))
    const unauthorized = vi.fn()
    await expect(new Api(tokens, 'api://nodes', unauthorized).me()).rejects.toBeInstanceOf(SessionEnded)
    expect(unauthorized).toHaveBeenCalledOnce()
  })
})
