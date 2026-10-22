import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { Api } from '../lib/api'
import type { Session } from '../lib/auth'
import { AppContext } from '../lib/context'
import type { Config, Fleet as FleetAnswer, Rights } from '../lib/types'
import { Fleet } from './Fleet'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const config = { platform: 'corp', audiences: { nodes: 'hugr-nodes' }, mounts: {}, links: [], scopes: [] } as unknown as Config
const none: Rights = { fleet: true, sessions: true, endSession: false, drain: false, resume: false, tracing: false,
  scale: false, policy: false, infrastructure: false, events: false, drift: false, profile: false, effective: false,
  deployment: 'cluster', bundles: ['observe'], views: [], functions: [] }

const fleet: FleetAnswer = {
  plan: { maxNodes: 12, sizes: ['small'] },
  groups: [{ name: 'etl', size: 'large', replicas: 3, readyReplicas: 2, resourceGroup: 'etl', resourceGroupKnown: true,
    profile: { target: 14, applied: 2, needsRestart: [] }, lost: [], conditions: [], scaling: { mode: 'manual' }, nodes: [],
    identity: { serviceAccount: 'hugr-node-etl', subject: '', clientID: '', tresor: '', tlsName: '' }, memoryLimitBytes: 0,
    quack: false, drainSeconds: 300 }],
  newGroup: { available: true, subjectPrefix: 'system:serviceaccount:hugr:hugr-node-' },
}

function show(rights: Rights, answer: FleetAnswer = fleet) {
  vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify(answer), { status: 200 })))
  const api = new Api({ getToken: async () => 't' }, 'hugr-nodes')
  render(
    <AppContext.Provider value={{ config, session: {} as Session, api, rights }}>
      <MemoryRouter><Fleet /></MemoryRouter>
    </AppContext.Provider>,
  )
}

describe('the fleet page', () => {
  it('shows scale and restart only with the cluster right', async () => {
    show(none)
    await waitFor(() => expect(screen.getByRole('link', { name: 'etl' })).toBeTruthy())
    expect(screen.queryByRole('button', { name: 'Scale' })).toBeNull()
    expect(screen.getByText(/take the cluster right/)).toBeTruthy()
    cleanup()
    show({ ...none, scale: true, bundles: ['observe', 'operate', 'cluster'] })
    await waitFor(() => expect(screen.getByRole('button', { name: 'Scale' })).toBeTruthy())
    expect(screen.getByText('Starting')).toBeTruthy()
    expect(screen.getByText('2 of 3 ready')).toBeTruthy()
  })
  it('offers a new group with the cluster right, when the console has an issuer for its doors (spec 006)', async () => {
    show(none)
    await waitFor(() => expect(screen.getByRole('link', { name: 'etl' })).toBeTruthy())
    expect(screen.queryByRole('button', { name: 'New group' })).toBeNull()
    cleanup()
    show({ ...none, scale: true, bundles: ['observe', 'operate', 'cluster'] })
    await waitFor(() => expect(screen.getByRole('button', { name: 'New group' })).toBeTruthy())
    cleanup()
    show({ ...none, scale: true, bundles: ['observe', 'operate', 'cluster'] },
      { ...fleet, newGroup: { available: false, subjectPrefix: '' } })
    await waitFor(() => expect(screen.getByRole('link', { name: 'etl' })).toBeTruthy())
    expect(screen.queryByRole('button', { name: 'New group' })).toBeNull()
  })
})
