// The mounted consoles (spec 003): tresor-server's and kista's micro-frontends, served under the console's origin
// (/tresor, /kista) and given a token for their own audience only.
import type { Session } from './auth'
import type { Config } from './types'

export type Which = 'tresor' | 'kista'

/** the contract the shell speaks: an addition keeps it, a break makes it 2 (both products' rule) */
export const contract = 1

export interface Handle {
  update(o: { theme?: string; path?: string; tenant?: string }): void
  unmount(): void
}

export interface Module {
  contract: number
  mountTresor?: (el: HTMLElement, o: Record<string, unknown>) => Handle
  mountKista?: (el: HTMLElement, o: Record<string, unknown>) => Handle
}

/** a token for a mounted console: only an audience this installation names (kista's spec: the host allow-lists) */
export function mountTokens(config: Config, session: Session) {
  const allowed = new Set([config.audiences.tresor, config.audiences.kista].filter((a): a is string => !!a))
  return (audience: string | undefined, how?: { renew?: boolean }): Promise<string> => {
    if (!audience || !allowed.has(audience)) return Promise.reject(new Error(`the console issues no token for "${audience ?? ''}"`))
    return session.getToken(audience, how)
  }
}

/** whether the person may use the mounted console: the service's own answer (spec 003's rights table) */
export async function mayUse(which: Which, config: Config, session: Session): Promise<boolean> {
  const mount = config.mounts[which]
  if (!mount) return false
  try {
    const audience = mount.audience ?? ''
    const token = await mountTokens(config, session)(audience)
    const path = which === 'tresor' ? '/v1/whoami'
      : mount.tenant ? `/api/v1/tenants/${encodeURIComponent(mount.tenant)}/whoami` : '/api/v1/whoami'
    const res = await fetch(mount.base + path, { headers: { Authorization: `Bearer ${token}` }, credentials: 'omit', cache: 'no-store' })
    if (!res.ok) return false
    const me = (await res.json()) as { permissions?: { create?: boolean }; administrator?: boolean; holds_admin?: boolean; holds_audit?: boolean }
    if (which === 'tresor') return me.permissions?.create === true
    return mount.tenant ? !!(me.holds_admin || me.holds_audit) : me.administrator === true
  } catch {
    return false
  }
}
