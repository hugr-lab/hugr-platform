import { afterEach, describe, expect, it, vi } from 'vitest'
import { Api, ApiError } from './api'
import { localPath, ownTokenScope } from './auth'
import { groupState, room, wanted } from '../screens/Fleet'
import { changesOf, drainOK, freeNodes, groupName, newGroupProblem } from '../screens/GroupActions'
import { caps, cell, readsPolicy } from './policy'
import { BYTES_MAX, ROWS_MAX, TRESOR, bounded, doorSecret, doorURI, enter, literal, personOf, reset, statementOf, tresorService, type Conn } from './duckdb'
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

  it('reads a write answered without a body, whatever its status (201 a group made)', async () => {
    const sent: { url: string; method?: string; body?: unknown }[] = []
    vi.stubGlobal('fetch', vi.fn(async (url: string, init: RequestInit) => {
      sent.push({ url, method: init.method, body: init.body })
      return new Response(null, { status: url.endsWith('/groups') ? 201 : 204 })
    }))
    const api = new Api({ getToken: async () => 't' }, 'a')
    await expect(api.createGroup({ name: 'r', size: 'small', replicas: 1, clientID: 'c', quack: false })).resolves.toBeUndefined()
    await expect(api.deleteGroup('r/../x')).resolves.toBeUndefined()
    await expect(api.removeFromPolicy('r', 'r')).resolves.toBeUndefined()
    expect(sent.map((s) => `${s.method} ${s.url}`)).toEqual(['POST /api/v1/console/groups',
      'DELETE /api/v1/console/groups/r%2F..%2Fx', 'DELETE /api/v1/console/groups/r/policy'])
    expect(sent[2].body).toBe('{"resourceGroup":"r"}')
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
  quack: false, drainSeconds: 300,
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
      newGroup: { available: false, subjectPrefix: '' },
      groups: [group({ name: 'a', replicas: 3 }), group({ name: 'b', replicas: 2, scaling: { mode: 'auto', min: 1, max: 5 } }),
        group({ name: 'c', replicas: 1 })],
    }
    expect(room(fleet, fleet.groups[0])).toBe(12 - 5 - 1)
    expect(room(fleet, fleet.groups[2])).toBe(12 - 3 - 5)
  })
})

describe('a group from the console (spec 006)', () => {
  it('sends one change per field changed, nothing when none is', () => {
    const g = group({ size: 'small', quack: false, drainSeconds: 300 })
    expect(changesOf(g, 'small', false, 300)).toEqual([])
    expect(changesOf(g, 'large', true, 60)).toEqual([{ size: 'large' }, { quack: true }, { drainSeconds: 60 }])
    expect(changesOf(g, 'small', false, 0)).toEqual([{ drainSeconds: 0 }])
  })
  it('takes a name the webhook takes', () => {
    for (const ok of ['etl', 'a', 'reports-2', 'x'.repeat(36)]) expect(groupName.test(ok)).toBe(true)
    for (const bad of ['', 'ETL', '-etl', 'etl-', 'e_tl', 'x'.repeat(37), 'a.b']) expect(groupName.test(bad)).toBe(false)
  })
  it("says why a new group cannot be made yet, the plan's room among it", () => {
    const f = { name: 'r', size: 'small', replicas: 1, clientID: 'c', drain: 300, resourceGroup: '' }
    expect(newGroupProblem(f, 2)).toBeUndefined()
    expect(newGroupProblem({ ...f, replicas: 3 }, 2)).toBe('Past the plan: 2 more nodes fit.')
    expect(newGroupProblem({ ...f, clientID: ' ' }, 2)).toBe("Give the identity's client id.")
    expect(newGroupProblem({ ...f, drain: 86401 }, 2)).toBe('The drain time is 0 to 86400 seconds.')
    expect(newGroupProblem({ ...f, resourceGroup: 'a"b' }, 2)).toMatch(/resource group's name/)
    expect(newGroupProblem({ ...f, resourceGroup: 'Reports_1' }, 2)).toBeUndefined()
    expect(newGroupProblem({ ...f, resourceGroup: 'reports.eu' }, 2)).toMatch(/resource group's name/)
    expect(drainOK(0) && drainOK(86400) && !drainOK(-1) && !drainOK(1.5)).toBe(true)
    const fleet: Fleet = { plan: { maxNodes: 12, sizes: ['small'] }, newGroup: { available: true, subjectPrefix: '' },
      groups: [group({ replicas: 3 }), group({ name: 'b', replicas: 1, scaling: { mode: 'auto', min: 1, max: 5 } })] }
    expect(freeNodes(fleet)).toBe(4)
  })
})

describe('the policy pages (spec 007)', () => {
  it('shows a value as a cell: a list joined, a set of flags as its true ones, nothing as a dash', () => {
    expect(cell(null)).toBe('-')
    expect(cell(true)).toBe('yes')
    expect(cell(['a', 'b'])).toBe('a, b')
    expect(cell({ select: true, insert: false })).toBe('select')
    expect(cell({ select: false })).toBe('none')
    expect(cell({ a: 1 })).toBe('{"a":1}')
    expect(cell(3)).toBe('3')
  })
  it("says what unstated caps mean, never a dash that reads as none", () => {
    expect(caps(null, 'unstated: every data capability')).toBe('unstated: every data capability')
    expect(caps([], 'x')).toBe('none')
    expect(caps({ select: true }, 'x')).toBe('select')
  })
  it('offers the Policy by the parts a person reads', () => {
    const r = (views: string[], functions: string[] = []) => ({ fleet: true, sessions: false, endSession: false,
      drain: false, resume: false, tracing: false, scale: false, policy: false, infrastructure: false, events: false,
      drift: false, profile: false, effective: false, deployment: 'cluster', bundles: [], views, functions })
    expect(readsPolicy(r(['node_load', 'sessions']))).toBe(false)
    expect(readsPolicy(r(['catalogs']))).toBe(true)
    expect(readsPolicy(r(['clients']))).toBe(true)
    expect(readsPolicy(r(['keys']))).toBe(false) // read only from a catalog's page
    expect(readsPolicy(r([], ['exposures']))).toBe(true)
  })
})

describe('the SQL page (spec 007)', () => {
  it("names the platform's door as the console's own host", () => {
    expect(doorURI({ hostname: 'console.example.org', port: '', protocol: 'https:' })).toBe('quack:console.example.org:443')
    expect(doorURI({ hostname: '127.0.0.1', port: '18644', protocol: 'https:' })).toBe('quack:127.0.0.1:18644')
  })
  it('runs the text as written, or behind ACL NATIVE', () => {
    expect(statementOf('SELECT 1', false)).toBe('SELECT 1')
    expect(statementOf('SELECT 1', true)).toBe('ACL NATIVE SELECT 1')
    expect(literal("o'hara")).toBe("'o''hara'")
  })
  it('cuts an answer at its rows or its bytes, whole rows only, and says so', () => {
    const many = Array.from({ length: ROWS_MAX + 1 }, (_, i) => [String(i)])
    const rows = bounded(['n'], many)
    expect(rows.rows.length).toBe(ROWS_MAX)
    expect(rows.cut).toBe(`the first ${ROWS_MAX} rows`)
    const wide = Array.from({ length: 10 }, () => ['x'.repeat(BYTES_MAX / 4)])
    const bytes = bounded(['t'], wide)
    expect(bytes.rows.length).toBe(4) // four quarters fit exactly; the fifth is past the bound
    expect(bytes.cut).toMatch(/MiB/)
    expect(bounded(['n'], [['1'], [null]]).cut).toBeUndefined()
  })
})

/** a connection that records what the page asks of DuckDB: which_secret answers the storages given */
function fakeConn(storages: string[]) {
  const asked: { sql: string; params: unknown[] }[] = []
  const conn = {
    prepare: async (sql: string) => ({
      query: async (...params: unknown[]) => {
        asked.push({ sql, params })
        return { toArray: () => (sql.includes('which_secret') ? storages.map((storage) => ({ storage })) : []) }
      },
      close: async () => undefined,
    }),
    query: async (sql: string) => {
      asked.push({ sql, params: [] })
      return undefined
    },
  }
  return { conn: conn as unknown as Conn, asked }
}

const at = { host: 'console.example.org', hostname: 'console.example.org', port: '', protocol: 'https:' }
const jwt = (claims: object) => `e30.${btoa(JSON.stringify(claims)).replace(/=+$/, '').replace(/\+/g, '-').replace(/\//g, '_')}.sig`

describe('the door through tresor (spec 009)', () => {
  it('hands tresor the token as a parameter with the issuer named, attaches once, and finds the door there', async () => {
    reset()
    const { conn, asked } = fakeConn([TRESOR])
    const issuer = 'https://idp.example/realms/hugr'
    await enter(conn, 'the-token', issuer, at)
    await enter(conn, 'a-renewed-token', issuer, at)
    expect(asked.map((a) => a.sql)).toEqual([
      'FROM tresor_web_token(?, ?, issuer := ?)',
      "ATTACH IF NOT EXISTS 'tresor:console.example.org/tresor' AS hugr_tresor (ISSUER 'https://idp.example/realms/hugr')",
      "SELECT storage FROM which_secret(?, 'quack')",
      'FROM tresor_web_token(?, ?, issuer := ?)',
      "SELECT storage FROM which_secret(?, 'quack')",
    ])
    expect(asked[0].params).toEqual(['tresor:console.example.org/tresor', 'the-token', issuer])
    expect(asked[3].params).toEqual(['tresor:console.example.org/tresor', 'a-renewed-token', issuer])
    expect(asked[2].params).toEqual(['quack:console.example.org:443'])
    expect(asked.some((a) => a.sql.includes('the-token'))).toBe(false)
  })
  it('refuses without tresor\'s secret for the door, whatever else DuckDB holds', async () => {
    reset()
    const { conn } = fakeConn(['memory'])
    await expect(enter(conn, 't', 'https://idp.example', at)).rejects.toThrow(/tresor serves you no secret/)
  })
  it('tells one person from another by the token, unverified, and an opaque token from everyone', () => {
    expect(personOf(jwt({ iss: 'https://idp', sub: 'alice' }))).toBe('https://idp|alice')
    expect(personOf(jwt({ iss: 'https://idp', sub: 'bob' }))).not.toBe(personOf(jwt({ iss: 'https://idp', sub: 'alice' })))
    expect(personOf('opaque')).toBe('')
    expect(personOf(jwt({ sub: 'no-issuer' }))).toBe('')
  })
  it("names tresor as the console serves it: its own origin's /tresor", () => {
    expect(tresorService({ host: 'console.example.org' })).toBe('tresor:console.example.org/tresor')
    expect(tresorService({ host: '127.0.0.1:18644' })).toBe('tresor:127.0.0.1:18644/tresor')
  })
  it("runs with tresor's secret only, and says what is missing without one", () => {
    expect(doorSecret('quack:c.example:443', [TRESOR])).toBe(TRESOR)
    expect(doorSecret('quack:c.example:443', ['HUGR_TRESOR'])).toBe(TRESOR)
    for (const storages of [[], ['memory'], ['local_file']]) {
      expect(() => doorSecret('quack:c.example:443', storages)).toThrow(
        /tresor serves you no secret for quack:c\.example:443: .*door\.scopes.*door\.grants/)
    }
  })
})
