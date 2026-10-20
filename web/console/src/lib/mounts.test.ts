import { afterEach, describe, expect, it, vi } from 'vitest'
import type { Session } from './auth'
import { mayUse, mountTokens } from './mounts'
import type { Config } from './types'

afterEach(() => vi.unstubAllGlobals())

const config = {
  audiences: { nodes: 'hugr-nodes', tresor: 'duckdb-secrets', kista: 'kista' },
  mounts: { tresor: { base: '/tresor', audience: 'duckdb-secrets' }, kista: { base: '/kista', audience: 'kista', tenant: 'corp' } },
} as unknown as Config
const session = { getToken: vi.fn(async (aud: string) => `token-for-${aud}`) } as unknown as Session

describe('the mounted consoles', () => {
  it('get a token for their own audience only', async () => {
    const tokens = mountTokens(config, session)
    await expect(tokens('duckdb-secrets')).resolves.toBe('token-for-duckdb-secrets')
    await expect(tokens('kista')).resolves.toBe('token-for-kista')
    await expect(tokens('hugr-nodes')).rejects.toThrow(/no token/)
    await expect(tokens('evil')).rejects.toThrow(/no token/)
    await expect(tokens(undefined)).rejects.toThrow(/no token/)
  })

  it('show by the service own whoami', async () => {
    const answers: Record<string, unknown> = {
      '/tresor/v1/whoami': { permissions: { create: true } },
      '/kista/api/v1/tenants/corp/whoami': { holds_admin: false, holds_audit: false },
    }
    const seen: [string, string][] = []
    vi.stubGlobal('fetch', vi.fn(async (url: string, init: RequestInit) => {
      seen.push([url, (init.headers as Record<string, string>).Authorization])
      return new Response(JSON.stringify(answers[url] ?? {}), { status: answers[url] ? 200 : 404 })
    }))
    expect(await mayUse('tresor', config, session)).toBe(true)
    expect(await mayUse('kista', config, session)).toBe(false)
    expect(seen).toEqual([['/tresor/v1/whoami', 'Bearer token-for-duckdb-secrets'],
      ['/kista/api/v1/tenants/corp/whoami', 'Bearer token-for-kista']])
    answers['/kista/api/v1/tenants/corp/whoami'] = { holds_audit: true }
    expect(await mayUse('kista', config, session)).toBe(true)
    vi.stubGlobal('fetch', vi.fn(async () => { throw new Error('down') }))
    expect(await mayUse('tresor', config, session)).toBe(false)
    expect(await mayUse('kista', { ...config, mounts: {} } as Config, session)).toBe(false)
  })
})
