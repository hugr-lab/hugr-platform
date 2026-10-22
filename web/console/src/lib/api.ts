// The console's API client (spec 003): the nodes' token as the bearer, nothing cached, never a cookie. A 401 renews
// the token once and retries; a second one ends the session (the page shows the sign-in).
import type { AuditEvent, ClusterPage, Config, Fleet, GroupPatch, GroupPolicy, InventoryPage, MemoryPage, NewGroup, NodePage, PodPage, Rights,
  SessionRow, ViewPage } from './types'

export const prefix = '/api/v1/console/'

export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
  ) {
    super(message)
  }
}

export interface Tokens {
  getToken(audience: string, how?: { renew?: boolean }): Promise<string>
}

/** what the API answers on a failure: {error, message} */
async function failure(res: Response): Promise<ApiError> {
  try {
    const body = (await res.json()) as { error?: string; message?: string }
    return new ApiError(res.status, body.error ?? 'error', body.message ?? res.statusText)
  } catch {
    return new ApiError(res.status, 'error', res.statusText)
  }
}

/** a path part from the browser, escaped: a label or an id never changes the route */
const part = (s: string) => encodeURIComponent(s)

/** a query string of the pairs, each escaped */
const query = (pairs: Record<string, string>) => new URLSearchParams(pairs).toString()

export class Api {
  constructor(
    private tokens: Tokens,
    private audience: string,
    private onUnauthorized: () => void = () => undefined,
  ) {}

  async request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const send = async (renew: boolean) => {
      const token = await this.tokens.getToken(this.audience, renew ? { renew: true } : undefined)
      return fetch(prefix + path, {
        method,
        headers: { Authorization: `Bearer ${token}`, ...(body === undefined ? {} : { 'Content-Type': 'application/json' }) },
        body: body === undefined ? undefined : JSON.stringify(body),
        credentials: 'omit',
        cache: 'no-store',
      })
    }
    let res = await send(false)
    if (res.status === 401) {
      try {
        res = await send(true)
      } catch (e) {
        this.onUnauthorized() // the renewal itself failed: the refresh token is gone or refused
        throw e
      }
      if (res.status === 401) this.onUnauthorized()
    }
    if (!res.ok) throw await failure(res)
    // a write answers without a body (201 a group made, 204 a change): nothing to read
    const text = await res.text()
    if (text === '') return undefined as T
    return JSON.parse(text) as T
  }

  me = () => this.request<{ rights: Rights }>('GET', 'me')
  fleet = () => this.request<Fleet>('GET', 'groups')
  scale = (group: string, replicas: number) => this.request<void>('PATCH', `groups/${part(group)}`, { replicas })
  restart = (group: string) => this.request<void>('PATCH', `groups/${part(group)}`, { restart: true })
  // spec 006: the groups themselves, and a group's place in the policy
  createGroup = (group: NewGroup) => this.request<void>('POST', 'groups', group)
  changeGroup = (group: string, patch: GroupPatch) => this.request<void>('PATCH', `groups/${part(group)}`, patch)
  deleteGroup = (group: string) => this.request<void>('DELETE', `groups/${part(group)}`)
  groupPolicy = (group: string) => this.request<GroupPolicy>('GET', `groups/${part(group)}/policy`)
  addToPolicy = (group: string, roles: string[], isDefault: boolean) =>
    this.request<GroupPolicy>('POST', `groups/${part(group)}/policy`, isDefault ? { roles, default: true } : { roles })
  // the resource group the page shows: one that changed since (kubectl) is refused, never another dropped
  removeFromPolicy = (group: string, resourceGroup: string) =>
    this.request<void>('DELETE', `groups/${part(group)}/policy`, { resourceGroup })
  node = (label: string) => this.request<NodePage>('GET', `nodes/${part(label)}`)
  sessions = (label: string) => this.request<{ sessions: SessionRow[] }>('GET', `nodes/${part(label)}/sessions`)
  endSession = (label: string, id: string) =>
    this.request<unknown>('POST', `nodes/${part(label)}/sessions/${part(id)}/end`)
  drain = (label: string) => this.request<unknown>('POST', `nodes/${part(label)}/drain`)
  resume = (label: string) => this.request<unknown>('POST', `nodes/${part(label)}/resume`)
  signOut = () => this.request<void>('POST', 'sign-out')
  // round 2
  pod = (label: string) => this.request<PodPage>('GET', `nodes/${part(label)}/kubernetes`)
  memory = (label: string) => this.request<MemoryPage>('GET', `nodes/${part(label)}/memory`)
  inventory = (label: string) => this.request<InventoryPage>('GET', `nodes/${part(label)}/inventory`)
  logs = (label: string, previous: boolean, lines: number) =>
    this.request<{ text: string }>('GET', `nodes/${part(label)}/logs?previous=${previous}&lines=${lines}`)
  events = (label: string) => this.request<{ events: AuditEvent[] }>('GET', `nodes/${part(label)}/events`)
  tracing = (label: string, id: string, change: { audit?: string; profile?: string }) =>
    this.request<void>('POST', `nodes/${part(label)}/sessions/${part(id)}/tracing`, change)
  cluster = () => this.request<ClusterPage>('GET', 'cluster')
  // spec 007: the policy's pages - a view filtered by its columns, a read function by the node's own names
  policyView = (view: string, filters: Record<string, string> = {}, offset = 0, limit = 500) =>
    this.request<ViewPage>('GET', `policy/views/${part(view)}?${query({ ...filters, offset: String(offset), limit: String(limit) })}`)
  policySources = () => this.request<{ sources: string[] }>('GET', 'policy/sources')
  policyCall = (fn: string, args: Record<string, string>, limit = 5000) =>
    this.request<ViewPage>('GET', `policy/functions/${part(fn)}?${query({ ...args, limit: String(limit) })}`)
}

/** the config the sign-in needs, unauthenticated */
export async function loadConfig(): Promise<Config> {
  const res = await fetch(prefix + 'config', { credentials: 'omit', cache: 'no-store' })
  if (!res.ok) throw await failure(res)
  return (await res.json()) as Config
}
