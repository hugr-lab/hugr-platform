import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { Api } from '../lib/api'
import type { Session } from '../lib/auth'
import { AppContext } from '../lib/context'
import type { Config, ResourceGroupForm, Rights } from '../lib/types'
import { NO_LIMITS, ResourceGroupDialog, groupProblem, limitOf } from './ResourceGroupDialog'

// jsdom has no showModal: a <dialog> stays closed there, and its content is found with hidden: true

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const config = { platform: 'corp', audiences: { nodes: 'hugr-nodes' }, mounts: {}, links: [], scopes: [] } as unknown as Config
const rights: Rights = { fleet: true, sessions: false, endSession: false, drain: false, resume: false, tracing: false,
  scale: false, policy: true, infrastructure: false, events: false, drift: false, profile: false, effective: false,
  deployment: 'cluster', bundles: ['observe', 'policy'], views: ['resource_groups'], functions: [] }

function show(name: string, read: object, statements: string[]) {
  const sent: { url: string; body: unknown }[] = []
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    if (init?.method === 'POST') {
      sent.push({ url, body: JSON.parse(String(init.body)) })
      return new Response(JSON.stringify(url.endsWith('preview') ? { statements, answers: [] }
        : { statements, answers: statements.map(() => ({ ok: true, message: '' })) }), { status: 200 })
    }
    return new Response(JSON.stringify(read), { status: 200 })
  }))
  const api = new Api({ getToken: async () => 't' }, 'hugr-nodes')
  render(
    <AppContext.Provider value={{ config, session: {} as Session, api, rights }}>
      <MemoryRouter><ResourceGroupDialog name={name} onDone={vi.fn()} /></MemoryRouter>
    </AppContext.Provider>,
  )
  return { sent }
}

describe('the resource group editor (spec 013)', () => {
  it('says what a form lacks', () => {
    const f: ResourceGroupForm = { name: 'batch', limits: NO_LIMITS, comment: '', default: false, roles: [] }
    expect(groupProblem({ ...f, name: ' ' }, {})).toMatch(/Name/)
    expect(groupProblem(f, { max_sessions: '1.5' })).toMatch(/whole number/)
    expect(groupProblem(f, { max_sessions: '-1' })).toMatch(/0 or more/)
    expect(groupProblem(f, { queue_priority: '-1', max_sessions: '' })).toBeUndefined()
    expect([limitOf(''), limitOf(' 8 '), limitOf('8MiB')]).toEqual([null, 8, undefined])
  })

  it('makes a new group: its limits, mark and roles shown as statements, then run as shown', async () => {
    const read = { exists: false, version: '', group: { name: '', limits: NO_LIMITS, comment: '', default: false, roles: [] },
      roles: ['analyst', 'ops'], defaultIs: 'general', nodeGroups: [] }
    const statement = 'CREATE RESOURCE GROUP "batch" (max_sessions 4)'
    const { sent } = show('', read, [statement])
    await waitFor(() => expect(screen.getByLabelText('Name')).toBeTruthy())
    fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'batch' } })
    fireEvent.change(screen.getByLabelText(/Sessions at most/), { target: { value: '4' } })
    fireEvent.click(screen.getByLabelText('analyst'))
    fireEvent.click(screen.getByLabelText(/The default group/))
    // another group is the default: the node refuses a second, and the form says so first
    expect(screen.getByText(/general is the default now/)).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Show the statements', hidden: true }))
    await waitFor(() => expect(screen.getByText(statement)).toBeTruthy())
    expect(sent[0].body).toEqual({ version: '', form: { name: 'batch', comment: '', default: true, roles: ['analyst'], op: 'write',
      limits: { ...NO_LIMITS, max_sessions: 4 } } })
    fireEvent.click(screen.getByRole('button', { name: 'Run', hidden: true }))
    await waitFor(() => expect(sent).toHaveLength(2))
    expect(sent[1].url).toMatch(/policy\/resource-group$/)
    expect(sent[1].body).toMatchObject({ version: '', statements: [statement] })
  })

  it('edits a group from its version; a group a group of nodes serves offers no drop', async () => {
    const read = { exists: true, version: 'v1', roles: ['analyst'], nodeGroups: ['etl'],
      group: { name: 'etl', limits: { ...NO_LIMITS, window_start: 8 }, comment: 'c', default: false, roles: ['analyst'] } }
    const { sent } = show('etl', read, ['CREATE OR REPLACE RESOURCE GROUP "etl" COMMENT \'c\''])
    await waitFor(() => expect((screen.getByLabelText(/Fetch window at start/) as HTMLInputElement).value).toBe('8'))
    expect(screen.getByText(/Served by the group/)).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Drop', hidden: true })).toBeNull()
    fireEvent.change(screen.getByLabelText(/Fetch window at start/), { target: { value: '' } })
    fireEvent.click(screen.getByRole('button', { name: 'Show the statements', hidden: true }))
    await waitFor(() => expect(sent).toHaveLength(1))
    // a limit cleared is sent as none: the node's setting
    expect(sent[0].body).toMatchObject({ version: 'v1', form: { name: 'etl', limits: { window_start: null } } })
  })

  it('drops a group no group of nodes serves', async () => {
    const read = { exists: true, version: 'v2', roles: [], nodeGroups: [],
      group: { name: 'adhoc', limits: NO_LIMITS, comment: '', default: false, roles: [] } }
    const { sent } = show('adhoc', read, ['DROP RESOURCE GROUP "adhoc"'])
    await waitFor(() => expect(screen.getByRole('button', { name: 'Drop', hidden: true })).toBeTruthy())
    fireEvent.click(screen.getByRole('button', { name: 'Drop', hidden: true }))
    await waitFor(() => expect(screen.getByText('DROP RESOURCE GROUP "adhoc"')).toBeTruthy())
    fireEvent.click(screen.getByRole('button', { name: 'Drop', hidden: true }))
    await waitFor(() => expect(sent).toHaveLength(2))
    expect(sent[1].body).toMatchObject({ version: 'v2', form: { op: 'drop' }, statements: ['DROP RESOURCE GROUP "adhoc"'] })
  })
})
