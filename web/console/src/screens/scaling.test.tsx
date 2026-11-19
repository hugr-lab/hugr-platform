import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { Api } from '../lib/api'
import type { Session } from '../lib/auth'
import { AppContext } from '../lib/context'
import type { Config, Fleet as FleetAnswer, Group, Rights } from '../lib/types'
import { Fleet, scalingProblem } from './Fleet'

// jsdom has no showModal: a <dialog> stays closed there, and its content is found with hidden: true'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const config = { platform: 'corp', audiences: { nodes: 'hugr-nodes' }, mounts: {}, links: [], scopes: [] } as unknown as Config
const rights: Rights = { fleet: true, sessions: true, endSession: false, drain: false, resume: false, tracing: false,
  scale: true, policy: false, infrastructure: false, events: false, drift: false, profile: false, effective: false,
  deployment: 'cluster', bundles: ['observe', 'operate', 'cluster'], views: [], functions: [] }

const group = (over: Partial<Group>): Group => ({
  name: 'etl', size: 'large', replicas: 2, readyReplicas: 2, resourceGroup: 'etl', resourceGroupKnown: true,
  profile: { target: 1, applied: 2, needsRestart: [] }, lost: [], conditions: [], scaling: { mode: 'manual' }, nodes: [],
  identity: { serviceAccount: 'hugr-node-etl', subject: '', clientID: '', tresor: '', tlsName: '' }, memoryLimitBytes: 0,
  quack: false, drainSeconds: 300, ...over } as Group)

function show(groups: Group[]) {
  const answer: FleetAnswer = { plan: { maxNodes: 6, sizes: ['large'] }, groups,
    newGroup: { available: false, subjectPrefix: '' } } as FleetAnswer
  const sent: { method: string; body: unknown }[] = []
  vi.stubGlobal('fetch', vi.fn(async (_url: string, init?: RequestInit) => {
    if (init?.method === 'PATCH') {
      sent.push({ method: 'PATCH', body: JSON.parse(String(init.body)) })
      return new Response(null, { status: 204 })
    }
    return new Response(JSON.stringify(answer), { status: 200 })
  }))
  const api = new Api({ getToken: async () => 't' }, 'hugr-nodes')
  render(
    <AppContext.Provider value={{ config, session: {} as Session, api, rights }}>
      <MemoryRouter><Fleet /></MemoryRouter>
    </AppContext.Provider>,
  )
  return sent
}

describe('scaling a group (spec 011 stage 4)', () => {
  it('turns a manual group to its load: min, max, idle', async () => {
    const sent = show([group({})])
    await waitFor(() => expect(screen.getByRole('button', { name: 'Scale' })).toBeTruthy())
    fireEvent.click(screen.getByRole('button', { name: 'Scale' }))
    fireEvent.click(screen.getByLabelText('By its load'))
    fireEvent.change(screen.getByLabelText('Minimum'), { target: { value: '0' } })
    fireEvent.change(screen.getByLabelText('Maximum'), { target: { value: '4' } })
    fireEvent.change(screen.getByLabelText('Idle, minutes'), { target: { value: '45' } })
    fireEvent.click(screen.getByRole('button', { name: 'Scale between 0 and 4', hidden: true }))
    await waitFor(() => expect(sent).toHaveLength(1))
    expect(sent[0].body).toEqual({ scaling: { mode: 'auto', min: 0, max: 4, idle: '45m' } })
  })

  it('refuses a max past the plan and a min past the max', async () => {
    show([group({}), group({ name: 'bi', replicas: 3 })])
    await waitFor(() => expect(screen.getAllByRole('button', { name: 'Scale' })).toHaveLength(2))
    fireEvent.click(screen.getAllByRole('button', { name: 'Scale' })[1]) // etl: bi keeps 3 of 6
    fireEvent.click(screen.getByLabelText('By its load'))
    fireEvent.change(screen.getByLabelText('Maximum'), { target: { value: '5' } })
    expect(screen.getByRole('alert', { hidden: true }).textContent).toContain('Past the plan')
    fireEvent.change(screen.getByLabelText('Maximum'), { target: { value: '2' } })
    fireEvent.change(screen.getByLabelText('Minimum'), { target: { value: '3' } })
    expect(screen.getByRole('alert', { hidden: true }).textContent).toContain('above the maximum')
    expect((screen.getByRole('button', { name: 'Scale between 3 and 2', hidden: true }) as HTMLButtonElement).disabled).toBe(true)
  })

  it('shows an auto group\'s last step and its drained node, and switches it back to manual', async () => {
    const sent = show([group({
      scaling: { mode: 'auto', min: 0, max: 3, idle: '30m0s', drained: 'hugr-node-etl-b',
        last: { at: '2026-11-19T12:00:00Z', from: 3, to: 2, reason: 'hugr-node-etl-c has had no session for 31m0s' } },
    })])
    await waitFor(() => expect(screen.getByText(/last 3 → 2: hugr-node-etl-c has had no session/)).toBeTruthy())
    expect(screen.getByText(/hugr-node-etl-b takes no new connection/)).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Scale' }))
    expect((screen.getByLabelText('Idle, minutes') as HTMLInputElement).value).toBe('30')
    fireEvent.click(screen.getByLabelText('Manual'))
    fireEvent.click(screen.getByRole('button', { name: 'Switch to manual', hidden: true }))
    await waitFor(() => expect(sent).toHaveLength(1))
    expect(sent[0].body).toEqual({ scaling: { mode: 'manual' } })
  })
})

describe('scalingProblem', () => {
  it('says what an auto group\'s form may not be', () => {
    expect(scalingProblem({ min: 0, max: 0, idle: 30 }, 6)).toContain('at most one node or more')
    expect(scalingProblem({ min: 0, max: 2, idle: 0 }, 6)).toContain('idle time')
    expect(scalingProblem({ min: 0, max: 2, idle: 30 }, 6)).toBeUndefined()
  })
})
