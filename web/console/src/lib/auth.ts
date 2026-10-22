// Sign-in (spec 003): authorization code with PKCE at the organisation's IdP, the console's public client. Tokens stay
// in memory, never in localStorage; only the PKCE state rides in sessionStorage across the redirect. A reload signs in
// again through the IdP's own session. Renewal is by the refresh token, never in a frame (the CSP has no frame-src),
// as tresor-server's and kista's consoles do it.
import { InMemoryWebStorage, UserManager, WebStorageStateStore, type User } from 'oidc-client-ts'
import type { Config } from './types'

const signedInMark = 'hugr.signed-in' // this tab signed in before: sign in again after a reload, no secret
const signInAtMark = 'hugr.sign-in-at' // when this tab last went to the IdP: a refusal right after is not met by another
const freshSignIn = 120_000 // ms
const popupName = 'hugr-signin'

/** an audience with a token of its own: Entra's resources (api://...) - one token per resource, by the refresh token
 * with the resource's .default scope. Any other audience must already be in the sign-in's token (Keycloak's audience
 * mapper). */
export function ownTokenScope(audience: string): string | undefined {
  return audience.startsWith('api://') ? `${audience}/.default` : undefined
}

/** a path inside the console, or nothing: a sign-in never sends the person elsewhere */
export function localPath(p: unknown): string | undefined {
  if (typeof p !== 'string' || !p.startsWith('/') || p.startsWith('//') || p.includes('\\')) return undefined
  return p
}

interface Held {
  token: string
  expiresAt: number // seconds since the epoch
}

export class Session {
  private m: UserManager
  private user: User | null = null
  private listeners = new Set<() => void>()
  private others = new Map<string, Held>() // tokens of their own, by audience
  private renewing = new Map<string, Promise<string>>()
  private completing?: Promise<string | undefined>

  constructor(private config: Config) {
    const callback = new URL('/callback', window.location.origin).href
    const scopes = new Set(['openid', 'profile', 'offline_access', ...config.scopes])
    const nodes = ownTokenScope(config.audiences.nodes)
    if (nodes) scopes.add(nodes) // the sign-in's token is the nodes' one
    this.m = new UserManager({
      authority: config.issuer,
      client_id: config.clientID,
      redirect_uri: callback,
      popup_redirect_uri: callback,
      popupWindowTarget: popupName,
      post_logout_redirect_uri: new URL('/', window.location.origin).href,
      response_type: 'code',
      scope: [...scopes].join(' '),
      userStore: new WebStorageStateStore({ store: new InMemoryWebStorage() }),
      stateStore: new WebStorageStateStore({ store: window.sessionStorage }),
      automaticSilentRenew: true,
      monitorSession: false,
      loadUserInfo: false,
    })
    this.m.events.addUserLoaded((u) => this.set(u))
    this.m.events.addAccessTokenExpired(() => this.set(null))
    this.m.events.addSilentRenewError(() => this.notify())
  }

  private set(u: User | null) {
    // another person (a popup's sign-in as someone else) holds none of the last one's tokens of their own
    if (!u || u.profile?.sub !== this.user?.profile?.sub) {
      this.others.clear()
      this.renewing.clear()
    }
    this.user = u
    this.notify()
  }

  private notify() {
    this.listeners.forEach((l) => l())
  }

  subscribe(l: () => void): () => void {
    this.listeners.add(l)
    return () => this.listeners.delete(l)
  }

  get signedIn(): boolean {
    return !!this.user && !this.user.expired
  }

  get displayName(): string | undefined {
    const p = this.user?.profile
    return (p?.name || p?.preferred_username || p?.email || p?.sub) as string | undefined
  }

  /** this tab signed in before the reload */
  get remembered(): boolean {
    return sessionStorage.getItem(signedInMark) === '1'
  }

  async signIn(returnTo: string): Promise<void> {
    sessionStorage.setItem(signedInMark, '1')
    sessionStorage.setItem(signInAtMark, String(Date.now()))
    await this.m.signinRedirect({ state: { returnTo: localPath(returnTo) ?? '/' } })
  }

  /** this tab came back from the IdP moments ago: a refusal now is not one another sign-in mends (the IdP's own session
   * answers without a prompt, and the nodes refuse the same token again) */
  get signedInJustNow(): boolean {
    const at = Number(sessionStorage.getItem(signInAtMark))
    return at > 0 && Date.now() - at < freshSignIn
  }

  /** completes a sign-in: a redirect's (the path to go back to), or a popup's (undefined: the window closes) */
  callback(): Promise<string | undefined> {
    this.completing ??= this.complete()
    return this.completing
  }

  private async complete(): Promise<string | undefined> {
    if (window.opener && window.name === popupName) {
      await this.m.signinPopupCallback()
      return undefined
    }
    const user = await this.m.signinRedirectCallback()
    this.set(user)
    const state = user.state as { returnTo?: string } | undefined
    return localPath(state?.returnTo) ?? '/'
  }

  /** signs in again in a popup: the page stays as it is (a mounted console's onUnauthorized) */
  async again(): Promise<void> {
    this.set(await this.m.signinPopup())
  }

  /** a token for the audience (the contract tresor's and kista's consoles call, getToken(audience, {renew})) */
  async getToken(audience: string, how?: { renew?: boolean }): Promise<string> {
    if (!this.user || this.user.expired) {
      this.notify()
      throw new SessionEnded()
    }
    const scope = audience === this.config.audiences.nodes ? undefined : ownTokenScope(audience)
    if (!scope) {
      if (how?.renew) await this.renew('', () => this.m.signinSilent().then((u) => u?.access_token ?? ''))
      if (!this.user?.access_token) throw new SessionEnded()
      return this.user.access_token
    }
    const held = this.others.get(audience)
    if (!how?.renew && held && held.expiresAt - 60 > Date.now() / 1000) return held.token
    return this.renew(audience, () => this.exchange(audience, scope))
  }

  /** one renewal per audience at a time: requests refused together share it */
  private renew(key: string, run: () => Promise<string>): Promise<string> {
    let p = this.renewing.get(key)
    if (!p) {
      const mine: Promise<string> = run().finally(() => {
        // a newer renewal (after another person's sign-in cleared these) is not this one's to end
        if (this.renewing.get(key) === mine) this.renewing.delete(key)
      })
      p = mine
      this.renewing.set(key, p)
    }
    return p
  }

  /** a resource's own token by the refresh token (Entra: one token per resource) */
  private async exchange(audience: string, scope: string): Promise<string> {
    const refresh = this.user?.refresh_token
    const who = this.user?.profile?.sub
    if (!refresh) throw new SessionEnded()
    const endpoint = await this.m.metadataService.getTokenEndpoint()
    if (!endpoint) throw new Error('the identity provider names no token endpoint')
    const body = new URLSearchParams({ grant_type: 'refresh_token', client_id: this.config.clientID,
      refresh_token: refresh, scope: `${scope} offline_access` })
    const res = await fetch(endpoint, { method: 'POST', body, credentials: 'omit', cache: 'no-store' })
    if (!res.ok) throw new SessionEnded()
    const answer = (await res.json()) as { access_token?: string; expires_in?: number }
    if (!answer.access_token) throw new SessionEnded()
    // another person signed in meanwhile: the last one's token is theirs, never handed out now
    if (this.user?.profile?.sub !== who) throw new SessionEnded()
    const held = { token: answer.access_token, expiresAt: Date.now() / 1000 + (answer.expires_in ?? 300) }
    this.others.set(audience, held)
    return held.token
  }

  /** ends the session here and at the IdP, the refresh token revoked first */
  async signOut(): Promise<void> {
    sessionStorage.removeItem(signedInMark)
    sessionStorage.removeItem(signInAtMark)
    await this.m.revokeTokens(['refresh_token']).catch(() => undefined)
    try {
      await this.m.signoutRedirect({ id_token_hint: this.user?.id_token })
    } catch {
      await this.m.removeUser()
      this.set(null)
    }
  }
}

export class SessionEnded extends Error {
  constructor() {
    super('the session ended: sign in again')
  }
}
