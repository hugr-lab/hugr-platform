import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { Api } from '../../lib/api'
import type { Session } from '../../lib/auth'
import { AppContext } from '../../lib/context'
import type { Config, Rights } from '../../lib/types'
import { Overview } from './Overview'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const config = { platform: 'corp', audiences: { nodes: 'hugr-nodes' }, mounts: {}, links: [], scopes: [] } as unknown as Config
const rights: Rights = { fleet: true, sessions: true, endSession: false, drain: false, resume: false, tracing: false,
  scale: false, policy: false, infrastructure: false, events: false, drift: false, profile: false, effective: false,
  deployment: 'cluster', bundles: ['observe'], views: [], functions: [] }

const node = {
  kubernetes: { label: 'hugr-node-etl-a', ready: true, serving: true, leaseAge: 4, current: true, sessions: 0, seatsLeft: 9,
    admitting: true, draining: false, target: 2, applied: 2, needsRestart: false, usage: null, pod: null },
  group: 'etl',
  // the load report as the console flattens it (spec 012): the engine's memory comes with it
  load: { draining: false, sessions_live: 0, sessions_max: 1000, sessions_by_door: { flight: 0 }, admit_new_session: false,
    admit_new_quack_client: false, admit_new_stream: false, memory_used: 62914560, memory_working: 1048576,
    memory_limit: 104857600, temp_used: 0, temp_limit: null, memory_critical: true },
}
const memory = { tags: { rows: [{ tag: 'BASE_TABLE', memory_usage_bytes: 61865984 }] }, spillBytes: 0, limits: { rows: [] } }

function show() {
  const asked: string[] = []
  vi.stubGlobal('fetch', vi.fn(async (url: string) => {
    const path = String(url).replace(/^.*\/api\/v1\/console\//, '')
    asked.push(path)
    if (path === 'nodes/hugr-node-etl-a') return new Response(JSON.stringify(node), { status: 200 })
    if (path === 'nodes/hugr-node-etl-a/memory') return new Response(JSON.stringify(memory), { status: 200 })
    return new Response(JSON.stringify({ error: 'not_found', message: 'none' }), { status: 404 })
  }))
  const api = new Api({ getToken: async () => 't' }, 'hugr-nodes')
  render(
    <AppContext.Provider value={{ config, session: {} as Session, api, rights }}>
      <MemoryRouter initialEntries={['/nodes/hugr-node-etl-a']}>
        <Routes><Route path="/nodes/:label" element={<Overview />} /></Routes>
      </MemoryRouter>
    </AppContext.Provider>,
  )
  return asked
}

describe("a node's overview", () => {
  it('shows the engine memory from the load report, and reads duckdb_memory() only when asked', async () => {
    const asked = show()
    await waitFor(() => expect(screen.getByText('1.0 MiB of 100 MiB')).toBeTruthy())
    expect(screen.getByText('60 MiB')).toBeTruthy() // the buffer pool, cache included
    expect(screen.getByText(/Past the critical level/)).toBeTruthy()
    // looking at the node runs no statement on it: its idle time stays (spec 012)
    expect(asked.filter((p) => p.endsWith('/memory'))).toHaveLength(0)
    fireEvent.click(screen.getByRole('button', { name: 'By use' }))
    await waitFor(() => expect(screen.getByRole('list', { name: 'The buffer pool by use' })).toBeTruthy())
    expect(screen.getByText('BASE_TABLE')).toBeTruthy()
    expect(asked.filter((p) => p.endsWith('/memory'))).toHaveLength(1)
  })
})
