import { afterEach, describe, expect, it, vi } from 'vitest'
import { Api, ApiError } from './api'
import { localPath, ownTokenScope } from './auth'
import { groupState, room, wanted } from '../screens/Fleet'
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
  identity: { serviceAccount: 'hugr-node-g', subject: '', clientID: '', tresor: '', tlsName: '' }, memoryLimitBytes: 0,
  ...over,
})

describe('the fleet', () => {
  it('names a group state by its conditions and nodes', () => {
    expect(groupState(group({})).text).toBe('Ready')
    expect(groupState(group({ readyReplicas: 1 })).mark).toBe('half')
    expect(groupState(group({ readyReplicas: 1 })).note).toBe('1 of 2 nodes ready: the operator makes the rest')
    expect(groupState(group({ readyReplicas: 3 })).text).toBe('Scaling down')
    expect(groupState(group({ readyReplicas: 1, problem: 'a pod cannot be made: exceeded quota' })).text).toBe('Not starting')
    // at its count, a roll's extra pod that is not made is a new node waiting, not the group down
    expect(groupState(group({ problem: 'a pod cannot be made: exceeded quota' })).mark).toBe('half')
    const node = (label: string, ready: boolean, state: 'starting' | 'running' | 'failing', reason?: string, message?: string) => ({
      label, ready, serving: ready, leaseAge: null, current: false, sessions: 0, seatsLeft: -1, admitting: ready, draining: false,
      target: 0, applied: 0, needsRestart: false, usage: null, pod: { state, phase: 'Pending', reason, message, restarts: 0 } })
    const scaled = group({ replicas: 3, readyReplicas: 1, nodes: [node('a', true, 'running'), node('b', false, 'starting', 'ContainerCreating'),
      node('c', false, 'failing', 'Unschedulable', '0/1 nodes are available')] })
    expect(groupState(scaled)).toEqual({ mark: 'off', text: 'A node does not start', note: 'c: Unschedulable - 0/1 nodes are available' })
    expect(wanted(scaled)).toEqual(['ok', 'half', 'off'])
    // a node that served and went unready is no starting one
    const served = group({ replicas: 2, readyReplicas: 1, nodes: [node('a', true, 'running'), { ...node('b', false, 'starting', 'NotReady'), leaseAge: 4 }] })
    expect(wanted(served)).toEqual(['ok', 'off'])
    // a drain of a scale down: one stopping while the ready count already meets the ask
    const down = group({ replicas: 1, readyReplicas: 1, nodes: [node('a', true, 'running'), { ...node('b', true, 'running'), pod: { state: 'stopping' as const, phase: 'Running', restarts: 0 } }] })
    expect(groupState(down)).toEqual({ mark: 'half', text: 'Scaling down', note: 'b drains, then goes' })
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
