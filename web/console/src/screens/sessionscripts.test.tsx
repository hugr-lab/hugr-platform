import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { Api } from '../lib/api'
import type { Session } from '../lib/auth'
import { AppContext } from '../lib/context'
import type { Config, Rights } from '../lib/types'
import { NO_LIMITS, ResourceGroupDialog } from './ResourceGroupDialog'
import { ResourceGroups, Role } from './Policy'
import { SessionScriptDialog, ownerOf, scriptProblem } from './SessionScriptDialog'

// spec 016: a session's scripts - the dialog per level, its statement shown and sent, the trial, the scripts in the
// policy pages folded with their actions, the order on a role's page, and the resource group editor's warning
// (jsdom has no showModal: a <dialog>'s content is found with hidden: true)

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const config = { platform: 'corp', audiences: { nodes: 'hugr-nodes' }, mounts: {}, links: [], scopes: [] } as unknown as Config
const rights: Rights = { fleet: true, sessions: false, endSession: false, drain: false, resume: false, tracing: false,
  scale: false, policy: true, infrastructure: false, events: false, drift: false, profile: false, effective: false, order: false,
  deployment: 'cluster', bundles: ['observe', 'policy'], views: ['resource_groups', 'role_resource_groups', 'roles'],
  functions: ['session_scripts', 'try_session_script'] }

type Sent = { url: string; body: unknown }

/** the console's answers by path; every POST recorded */
function serve(answers: (url: string, body?: unknown) => unknown): Sent[] {
  const sent: Sent[] = []
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    const body = init?.method === 'POST' ? JSON.parse(String(init.body)) : undefined
    if (body !== undefined) sent.push({ url, body })
    return new Response(JSON.stringify(answers(String(url), body)), { status: 200 })
  }))
  return sent
}

function mount(node: React.ReactNode) {
  const api = new Api({ getToken: async () => 't' }, 'hugr-nodes')
  render(
    <AppContext.Provider value={{ config, session: {} as Session, api, rights }}>
      <MemoryRouter>{node}</MemoryRouter>
    </AppContext.Provider>,
  )
}

const tokyo = "SET TimeZone = 'Asia/Tokyo';\nSET VARIABLE layer = 'role'"

describe("a session's scripts (spec 016)", () => {
  it('names the owner and says what a text lacks', () => {
    expect([ownerOf('etl', ''), ownerOf('', 'analyst'), ownerOf('etl', 'analyst')])
      .toEqual(['the resource group etl', 'the role analyst', "analyst's grant of etl"])
    expect(scriptProblem(' \n')).toMatch(/Write the script/)
    expect(scriptProblem('SELECT \u0001')).toMatch(/control character/)
    expect(scriptProblem('SELECT 1;\n\tSELECT 2')).toBeUndefined()
  })

  it("writes a role's script as shown, its policy chosen", async () => {
    const statement = "ALTER ROLE \"analyst\" SET SESSION SCRIPT '...' ON ERROR CONTINUE"
    const sent = serve((url) => url.includes('session-script?')
      ? { exists: true, sql: 'SELECT 1', onError: 'fail', version: 'v1', roles: [], groups: ['etl'] }
      : { statements: [statement], answers: [{ ok: true, message: '' }] })
    mount(<SessionScriptDialog role="analyst" onDone={vi.fn()} />)
    await waitFor(() => expect((screen.getByLabelText('SQL') as HTMLTextAreaElement).value).toBe('SELECT 1'))
    fireEvent.change(screen.getByLabelText('SQL'), { target: { value: tokyo } })
    fireEvent.change(screen.getByLabelText('On error'), { target: { value: 'continue' } })
    fireEvent.click(screen.getByRole('button', { name: 'Show the statement', hidden: true }))
    await waitFor(() => expect(screen.getByText(statement)).toBeTruthy())
    expect(sent[0]).toEqual({ url: expect.stringMatching(/session-script\/preview$/),
      body: { version: 'v1', form: { group: '', role: 'analyst', sql: tokyo, onError: 'continue', op: 'set' } } })
    fireEvent.click(screen.getByRole('button', { name: 'Write', hidden: true }))
    await waitFor(() => expect(sent).toHaveLength(2))
    expect(sent[1]).toMatchObject({ url: expect.stringMatching(/session-script$/), body: { version: 'v1', statements: [statement] } })
    await waitFor(() => expect(screen.getByText(/Sessions opened from now on run it/)).toBeTruthy())
  })

  it('tries a script on the group picked, and shows each script and the verdict', async () => {
    const sent = serve((url) => {
      if (url.includes('session-script?')) return { exists: false, sql: '', onError: '', version: '', roles: [], groups: ['etl', 'general'] }
      return { node: 'hugr-node-general-a', group: 'general', rows: [
        { pos: 1, level: 'group', group: 'general', role: null, on_error: 'fail', edited: false, ok: true },
        { pos: 2, level: 'role', group: null, role: 'analyst', on_error: 'fail', edited: true, ok: false, error_class: 'Parser',
          message: 'syntax error at end of input', line: 2, column: 16 },
        { pos: null, level: 'session', ok: false, message: 'refused by the role script' }] }
    })
    mount(<SessionScriptDialog role="analyst" onDone={vi.fn()} />)
    await waitFor(() => expect(screen.getByLabelText('Served on the group')).toBeTruthy())
    fireEvent.change(screen.getByLabelText('SQL'), { target: { value: 'SET a = 1;\nSET TimeZone =' } })
    fireEvent.change(screen.getByLabelText('Served on the group'), { target: { value: 'general' } })
    fireEvent.click(screen.getByRole('button', { name: 'Try', hidden: true }))
    await waitFor(() => expect(sent).toHaveLength(1))
    expect(sent[0]).toEqual({ url: expect.stringMatching(/session-script\/try$/), body: {
      form: { group: '', role: 'analyst', sql: 'SET a = 1;\nSET TimeZone =', onError: 'fail', op: 'set' },
      session: { role: 'analyst', group: 'general' } } })
    const verdict = await screen.findByRole('status', { hidden: true })
    expect(verdict.textContent).toContain('The session would be refused: refused by the role script')
    expect(verdict.textContent).toContain('Parser at line 2, column 16: syntax error at end of input')
    expect(verdict.textContent).toContain('the resource group general')
    // the text changed: the trial no longer stands for it
    fireEvent.change(screen.getByLabelText('SQL'), { target: { value: 'SET TimeZone = 1' } })
    expect(screen.queryByRole('status', { hidden: true })).toBeNull()
  })

  it("drops a grant's script; a group's trial takes a role of the group", async () => {
    const sent = serve((url) => url.includes('session-script?')
      ? { exists: true, sql: 'SELECT 1', onError: 'fail', version: 'v3', roles: ['analyst'], groups: [] }
      : { statements: ['ALTER RESOURCE GROUP "etl" FOR ROLE "analyst" DROP SESSION SCRIPT'], answers: [{ ok: true, message: '' }] })
    mount(<SessionScriptDialog group="etl" role="analyst" onDone={vi.fn()} />)
    await waitFor(() => expect(screen.getByRole('button', { name: 'Drop', hidden: true })).toBeTruthy())
    // a grant's session is its own: no picker
    expect(screen.queryByLabelText('A session of the role')).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'Drop', hidden: true }))
    await waitFor(() => expect(sent).toHaveLength(1))
    fireEvent.click(screen.getByRole('button', { name: 'Drop', hidden: true }))
    await waitFor(() => expect(sent).toHaveLength(2))
    expect(sent[1].body).toMatchObject({ version: 'v3', form: { group: 'etl', role: 'analyst', op: 'drop' } })
    cleanup()
    serve(() => ({ exists: false, sql: '', onError: '', version: '', roles: ['analyst', 'ops'], groups: [] }))
    mount(<SessionScriptDialog group="etl" onDone={vi.fn()} />)
    await waitFor(() => expect(screen.getByLabelText('A session of the role')).toBeTruthy())
    expect(screen.queryByLabelText('Served on the group')).toBeNull()
  })

  it('folds a script in the resource groups table and opens its dialog from the row', async () => {
    const long = 'SELECT 1;\nSELECT 2;\nSELECT 3;\nSELECT 4;\nSELECT 5'
    serve((url) => {
      if (url.includes('views/resource_groups')) {
        return { columns: ['group', 'session_script', 'session_on_error'], rows: [{ group: 'etl', session_script: long, session_on_error: 'fail' }], total: 1 }
      }
      if (url.includes('views/role_resource_groups')) {
        return { columns: ['role', 'group', 'session_script'], rows: [{ role: 'analyst', group: 'etl', session_script: null }], total: 1 }
      }
      return { exists: false, sql: '', onError: '', version: '', roles: [], groups: [] }
    })
    mount(<ResourceGroups />)
    await waitFor(() => expect(screen.getByText('(5 lines)')).toBeTruthy())
    expect(screen.getByRole('button', { name: 'Session script of the resource group etl' }).textContent).toBe('Change')
    fireEvent.click(await screen.findByRole('button', { name: "Session script of analyst's grant of etl" }))
    await waitFor(() => expect(screen.getByText("The session script of analyst's grant of etl")).toBeTruthy())
  })

  it("lists on a role's page what its session runs, on each of its groups", async () => {
    const calls: string[] = []
    serve((url) => {
      if (url.includes('functions/session_scripts')) {
        calls.push(url)
        return { columns: ['pos', 'level', 'group', 'role', 'on_error', 'sql'], total: 1,
          rows: [{ pos: 1, level: 'role', group: null, role: 'analyst', on_error: 'fail', sql: 'SELECT 1' }] }
      }
      if (url.includes('session-script?')) return { exists: false, sql: '', onError: '', version: '', roles: [], groups: ['etl', 'general'] }
      return { columns: [], rows: [], total: 0 }
    })
    mount(<Role name="analyst" />)
    const order = await screen.findByRole('region', { name: 'What a session runs' })
    await waitFor(() => expect(calls).toHaveLength(1))
    expect(calls[0]).toMatch(/role=analyst&group=etl/)
    fireEvent.click(screen.getByRole('button', { name: 'general' }))
    await waitFor(() => expect(calls).toHaveLength(2))
    expect(calls[1]).toMatch(/group=general/)
    expect(order.textContent).toContain('SELECT 1')
  })

  it('warns before the resource group editor takes out a role whose grant has a script', async () => {
    serve(() => ({ exists: true, version: 'v1', roles: ['analyst', 'ops'], nodeGroups: [], scripted: ['analyst'],
      group: { name: 'etl', limits: NO_LIMITS, comment: '', default: false, roles: ['analyst', 'ops'] } }))
    mount(<ResourceGroupDialog name="etl" onDone={vi.fn()} />)
    await waitFor(() => expect(screen.getByLabelText('analyst')).toBeTruthy())
    expect(screen.queryByText(/drops the session script/)).toBeNull()
    fireEvent.click(screen.getByLabelText('analyst'))
    expect(screen.getByText(/drops the session script of its grant of this group too/)).toBeTruthy()
  })
})
