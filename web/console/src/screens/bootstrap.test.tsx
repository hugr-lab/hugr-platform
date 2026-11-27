import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { Api } from '../lib/api'
import type { Session } from '../lib/auth'
import { AppContext, type Loaded } from '../lib/context'
import type { BootstrapPage, Config, Group, InventoryPage, Rights } from '../lib/types'
import { BootstrapSection } from './Bootstrap'

// spec 015: the group's boot in its order, a script's row changed or dropped where it is, the SQL sent as written

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const config = { platform: 'corp', audiences: { nodes: 'hugr-nodes' }, mounts: {}, links: [], scopes: [] } as unknown as Config
const rights: Rights = { fleet: true, sessions: true, endSession: false, drain: false, resume: false, tracing: false,
  scale: true, policy: false, infrastructure: true, events: false, drift: true, profile: true, effective: true, order: true,
  deployment: 'cluster', bundles: ['observe', 'operate', 'cluster'], views: ['cluster_effective'], functions: ['bootstrap_order'] }

const group = {
  name: 'b', resourceGroup: 'etl', replicas: 1,
  profile: { target: 4, applied: 1, needsRestart: ['hugr-node-b-1'], failing: [], failingMore: 0 },
  nodes: [{ label: 'hugr-node-b-1', serving: true, ready: true, applied: 4, leaseAge: 1 }],
} as unknown as Group

const script = 'SELECT *\nFROM missing\nWHERE a = 1\nAND b = 2\nAND c = 3'
const inventory: InventoryPage = {
  extensions: { columns: [], rows: [] },
  drift: { columns: [], rows: [] },
  effective: { columns: [], rows: [
    { scope: '', kind: 'source', name: 'lake', class: 'drain', version: 2 },
    { scope: 'etl', kind: 'script', name: '20_after', class: 'restart', version: 4 },
  ] },
  order: { columns: [], rows: [
    { pos: 1, step: '1 node settings', sql: 'SET GLOBAL threads = 4' },
    { pos: 9, step: '6 profile: script 20_after (after_sources, on error retry)', sql: script },
  ] },
} as unknown as InventoryPage

function show() {
  const sent: unknown[] = []
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    if (init?.method === 'POST') {
      sent.push(JSON.parse(String(init.body)))
      return new Response(JSON.stringify({ version: 5, class: 'restart', appliedHere: false, ok: true, message: '', node: 'n1' }), { status: 200 })
    }
    if (String(url).endsWith('/inventory')) return new Response(JSON.stringify(inventory), { status: 200 })
    return new Response('{}', { status: 200 })
  }))
  const api = new Api({ getToken: async () => 't' }, 'hugr-nodes')
  const page = { loading: false, reload: () => {} } as Loaded<BootstrapPage>
  render(
    <AppContext.Provider value={{ config, session: {} as Session, api, rights }}>
      <MemoryRouter><BootstrapSection group={group} page={page} onChanged={() => {}} /></MemoryRouter>
    </AppContext.Provider>,
  )
  return sent
}

describe("a group's boot scripts (spec 015)", () => {
  it('lists the boot in order, a long script folded, and says the nodes run what they booted with', async () => {
    show()
    const order = await screen.findByRole('region', { name: 'The boot in order' })
    expect(order.textContent).toContain('6 profile: script 20_after (after_sources, on error retry)')
    expect(order.textContent).toContain('(5 lines)')
    expect(order.textContent).toContain('hugr-node-b-1 still runs what it held at its boot')
    // a script's row has its actions, another item's none
    expect(screen.getByRole('button', { name: 'Change script 20_after' })).toBeTruthy()
    expect(screen.queryByRole('button', { name: /script lake/ })).toBeNull()
  })

  it("drops a script in its own scope, and an alter sends only what it changes", async () => {
    const sent = show()
    fireEvent.click(await screen.findByRole('button', { name: 'Drop script 20_after' }))
    fireEvent.click(screen.getByRole('button', { name: 'Write', hidden: true }))
    await waitFor(() => expect(sent).toHaveLength(1))
    expect(sent[0]).toEqual({ action: 'drop-script', script: '20_after', group: 'etl' })
    cleanup()

    const again = show()
    fireEvent.click(await screen.findByRole('button', { name: 'Change script 20_after' }))
    fireEvent.change(screen.getByLabelText('SQL'), { target: { value: "SELECT 'a'\n\tFROM t" } })
    fireEvent.click(screen.getByRole('button', { name: 'Write', hidden: true }))
    await waitFor(() => expect(again).toHaveLength(1))
    expect(again[0]).toEqual({ action: 'alter-script', script: '20_after', sql: "SELECT 'a'\n\tFROM t", group: 'etl' })
    expect((await screen.findByRole('status', { hidden: true })).textContent).toMatch(/at a restart/)
  })
})
