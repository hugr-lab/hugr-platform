import { afterEach, describe, expect, it, vi } from 'vitest'
import { Api, ApiError } from './api'
import { localPath, ownTokenScope } from './auth'
import { groupState, room } from '../screens/Fleet'
import type { Fleet, Group } from './types'

afterEach(() => vi.unstubAllGlobals())

const answer = (status: number, body: unknown = {}) =>
  new Response(status === 204 ? null : JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

describe('the API client', () => {
  it('carries the nodes token, renews it once on a 401, then gives up to the sign-in', async () => {
    const tokens = { getToken: vi.fn(async (_a: string, how?: { renew?: boolean }) => (how?.renew ? 'fresh' : 'stale')) }
    const seen: string[] = []
    vi.stubGlobal('fetch', vi.fn(async (_url: string, init: RequestInit) => {
      const auth = (init.headers as Record<string, string>).Authorization
      seen.push(auth)
      return auth === 'Bearer fresh' ? answer(200, { rights: { fleet: true } }) : answer(401, { error: 'unauthenticated' })
    }))
    const unauthorized = vi.fn()
    const api = new Api(tokens, 'hugr-nodes', unauthorized)
    expect((await api.me()).rights.fleet).toBe(true)
    expect(seen).toEqual(['Bearer stale', 'Bearer fresh'])
    expect(tokens.getToken).toHaveBeenCalledWith('hugr-nodes', undefined)
    expect(tokens.getToken).toHaveBeenCalledWith('hugr-nodes', { renew: true })
    expect(unauthorized).not.toHaveBeenCalled()

    vi.stubGlobal('fetch', vi.fn(async () => answer(401, { error: 'unauthenticated', message: 'sign in' })))
    await expect(api.me()).rejects.toBeInstanceOf(ApiError)
    expect(unauthorized).toHaveBeenCalledOnce()
  })

  it('passes the server message and code, and escapes path parts', async () => {
    const urls: string[] = []
    vi.stubGlobal('fetch', vi.fn(async (url: string) => {
      urls.push(url)
      return answer(503, { error: 'no_node', message: 'no node answers' })
    }))
    const api = new Api({ getToken: async () => 't' }, 'a')
    const e = (await api.endSession('node/../x', "id'; DROP").catch((x: unknown) => x)) as ApiError
    expect(e).toBeInstanceOf(ApiError)
    expect([e.status, e.code, e.message]).toEqual([503, 'no_node', 'no node answers'])
    expect(urls[0]).toBe("/api/v1/console/nodes/node%2F..%2Fx/sessions/id'%3B%20DROP/end")
  })

  it('sends no cookie and caches nothing', async () => {
    const inits: RequestInit[] = []
    vi.stubGlobal('fetch', vi.fn(async (_u: string, init: RequestInit) => {
      inits.push(init)
      return answer(204)
    }))
    await new Api({ getToken: async () => 't' }, 'a').scale('etl', 3)
    expect(inits[0].credentials).toBe('omit')
    expect(inits[0].cache).toBe('no-store')
    expect(inits[0].body).toBe('{"replicas":3}')
  })
})

describe('the sign-in', () => {
  it('goes back only inside the console', () => {
    expect(localPath('/groups/etl')).toBe('/groups/etl')
    for (const p of ['https://evil.example', '//evil.example', '/\\evil', 'groups', undefined, 42]) {
      expect(localPath(p)).toBeUndefined()
    }
  })
  it('asks a token of its own for an Entra resource only', () => {
    expect(ownTokenScope('api://duckdb-secrets')).toBe('api://duckdb-secrets/.default')
    expect(ownTokenScope('duckdb-secrets')).toBeUndefined()
  })
})

const group = (over: Partial<Group>): Group => ({
  name: 'g', size: 'small', replicas: 2, readyReplicas: 2, resourceGroup: 'g', resourceGroupKnown: true,
  profile: { target: 1, applied: 2, needsRestart: [] }, lost: [], conditions: [], scaling: { mode: 'manual' }, nodes: [],
  ...over,
})

describe('the fleet', () => {
  it('names a group state by its conditions and nodes', () => {
    expect(groupState(group({})).text).toBe('Ready')
    expect(groupState(group({ readyReplicas: 1 })).mark).toBe('half')
    expect(groupState(group({ replicas: 0, readyReplicas: 0, scaling: { mode: 'auto', min: 0, max: 3 } })).note)
      .toBe('Starts on the first client')
    expect(groupState(group({ profile: { target: 1, applied: 1, needsRestart: ['n'] } })).text).toBe('Restart pending')
    expect(groupState(group({ conditions: [{ type: 'Draining', status: 'True', reason: '', message: '' }] })).text)
      .toBe('Draining')
  })
  it('lets a group grow to the plan less what the others keep, an autoscaled one at its max', () => {
    const fleet: Fleet = {
      plan: { maxNodes: 12, sizes: [] },
      groups: [group({ name: 'a', replicas: 3 }), group({ name: 'b', replicas: 2, scaling: { mode: 'auto', min: 1, max: 5 } }),
        group({ name: 'c', replicas: 1 })],
    }
    expect(room(fleet, fleet.groups[0])).toBe(12 - 5 - 1)
    expect(room(fleet, fleet.groups[2])).toBe(12 - 3 - 5)
  })
})
