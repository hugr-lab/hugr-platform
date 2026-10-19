// The console's API client (spec 003): the nodes' token as the bearer, nothing cached, never a cookie. A 401 renews
// the token once and retries; a second one ends the session (the page shows the sign-in).
import type { Config, Fleet, NodePage, Rights, SessionRow } from './types'

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
      res = await send(true)
      if (res.status === 401) this.onUnauthorized()
    }
    if (!res.ok) throw await failure(res)
    if (res.status === 204) return undefined as T
    return (await res.json()) as T
  }

  me = () => this.request<{ rights: Rights }>('GET', 'me')
  fleet = () => this.request<Fleet>('GET', 'groups')
  scale = (group: string, replicas: number) => this.request<void>('PATCH', `groups/${part(group)}`, { replicas })
  restart = (group: string) => this.request<void>('PATCH', `groups/${part(group)}`, { restart: true })
  node = (label: string) => this.request<NodePage>('GET', `nodes/${part(label)}`)
  sessions = (label: string) => this.request<{ sessions: SessionRow[] }>('GET', `nodes/${part(label)}/sessions`)
  endSession = (label: string, id: string) =>
    this.request<unknown>('POST', `nodes/${part(label)}/sessions/${part(id)}/end`)
  drain = (label: string) => this.request<unknown>('POST', `nodes/${part(label)}/drain`)
  resume = (label: string) => this.request<unknown>('POST', `nodes/${part(label)}/resume`)
  signOut = () => this.request<{ closed: number }>('POST', 'sign-out')
}

/** the config the sign-in needs, unauthenticated */
export async function loadConfig(): Promise<Config> {
  const res = await fetch(prefix + 'config', { credentials: 'omit', cache: 'no-store' })
  if (!res.ok) throw await failure(res)
  return (await res.json()) as Config
}
