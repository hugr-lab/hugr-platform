import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { Api } from '../lib/api'
import type { Session } from '../lib/auth'
import { AppContext } from '../lib/context'
import type { Config, GrantForm, Rights } from '../lib/types'
import { GrantDialog, Written, formProblem, namesOf } from './GrantDialog'

// jsdom has no showModal: a <dialog> stays closed there, and its content is found with hidden: true

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const config = { platform: 'corp', audiences: { nodes: 'hugr-nodes' }, mounts: {}, links: [], scopes: [] } as unknown as Config
const rights: Rights = { fleet: true, sessions: false, endSession: false, drain: false, resume: false, tracing: false,
  scale: false, policy: true, infrastructure: false, events: false, drift: false, profile: false, effective: false, order: false,
  deployment: 'cluster', bundles: ['observe', 'policy'], views: ['grants', 'object_grants'], functions: ['check_grant'] }

const hint = 'group etl does not list source pg, which role analyst reads - CLUSTER SET SOURCES (sales, pg) IN GROUP etl'

function show(props: Omit<Parameters<typeof GrantDialog>[0], 'onDone'>, read?: object) {
  const sent: { url: string; body: unknown }[] = []
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    if (init?.method === 'POST') {
      const body = JSON.parse(String(init.body))
      sent.push({ url, body })
      if (url.endsWith('policy/grant/check')) {
        return new Response(JSON.stringify({ columns: ['item', 'name', 'type', 'status', 'message'],
          rows: [{ item: 'rls', name: 'a = 1', type: null, status: 'ok', message: null }], total: 1 }), { status: 200 })
      }
      const statements = body.form.op === 'revoke' ? ['REVOKE OBJECT "c"."t" FROM ROLE "r"'] : ['GRANT OBJECT "c"."t" TO ROLE "r"']
      return new Response(JSON.stringify(url.endsWith('preview') ? { statements, answers: [] }
        : { statements, answers: [{ ok: true, message: hint }] }), { status: 200 })
    }
    return new Response(JSON.stringify(read), { status: 200 })
  }))
  const api = new Api({ getToken: async () => 't' }, 'hugr-nodes')
  const done = vi.fn()
  render(
    <AppContext.Provider value={{ config, session: {} as Session, api, rights }}>
      <MemoryRouter><GrantDialog {...props} onDone={done} /></MemoryRouter>
    </AppContext.Provider>,
  )
  return { sent, done }
}

describe('the grant editor (spec 007 stage 2c)', () => {
  it('tells a statement with no answer and one that may have run from a refusal', () => {
    render(<Written written={{ statements: ['A', 'B', 'C'], answers: [{ ok: true, message: '' }, null],
      refused: { index: 2, message: 'the node did not answer in time', unknown: true } }} />)
    expect(screen.getByText('Ran')).toBeTruthy()
    expect(screen.getByText('Unknown')).toBeTruthy()
    expect(screen.queryByText('Refused')).toBeNull()
  })

  it('says what a form lacks', () => {
    const f: GrantForm = { level: 'object', role: 'r', catalog: 'c', target: 't', caps: null }
    expect(formProblem({ ...f, role: ' ' })).toMatch(/role/)
    expect(formProblem({ ...f, target: '' })).toMatch(/object/)
    expect(formProblem({ ...f, columns: { mode: 'only', names: [] } })).toMatch(/columns/)
    expect(formProblem({ ...f, masks: [{ column: 'ssn', mask: '' }] })).toMatch(/mask/)
    expect(formProblem(f)).toBeUndefined()
    expect(namesOf(' id, amount ,, ssn')).toEqual(['id', 'amount', 'ssn'])
  })

  it('writes a new object grant: the statements shown first, then run, and what the node said', async () => {
    const { sent } = show({ level: 'object', catalog: 'c' })
    fireEvent.change(screen.getByLabelText('Role'), { target: { value: 'r' } })
    fireEvent.change(screen.getByLabelText('Object'), { target: { value: 't' } })
    fireEvent.click(screen.getByLabelText('Every column but these'))
    fireEvent.change(screen.getByLabelText('Columns'), { target: { value: 'salary, ssn' } })
    fireEvent.click(screen.getByRole('button', { name: 'Add a mask', hidden: true }))
    fireEvent.change(screen.getByLabelText('Mask 1: column'), { target: { value: 'tail' } })
    fireEvent.change(screen.getByLabelText('Mask 1: expression'), { target: { value: "'***'" } })
    fireEvent.click(screen.getByRole('button', { name: 'Show the statements', hidden: true }))
    await waitFor(() => expect(screen.getByText('GRANT OBJECT "c"."t" TO ROLE "r"')).toBeTruthy())
    expect(sent[0].body).toEqual({ version: '', form: { level: 'object', catalog: 'c', role: 'r', target: 't', caps: null, op: 'write',
      columns: { mode: 'except', names: ['salary', 'ssn'] }, masks: [{ column: 'tail', mask: "'***'" }] } })
    fireEvent.click(screen.getByRole('button', { name: 'Run', hidden: true }))
    await waitFor(() => expect(screen.getByText(hint)).toBeTruthy())
    expect(sent[1].url).toMatch(/policy\/grant$/)
    // the write carries the statements shown: the server runs them only while the form composes exactly those
    expect(sent[1].body).toMatchObject({ version: '', statements: ['GRANT OBJECT "c"."t" TO ROLE "r"'] })
    expect(screen.getAllByText('Done')).toHaveLength(2) // the statement's status and the dialog's button
  })

  it('edits a grant from its version, and revokes it', async () => {
    const read = { exists: true, version: 'v1', grant: { level: 'object', role: 'r', catalog: 'c', target: 't', caps: ['select'], rls: 'a = 1' } }
    const { sent } = show({ level: 'object', catalog: 'c', role: 'r', target: 't' }, read)
    await waitFor(() => expect((screen.getByLabelText('Row filter') as HTMLTextAreaElement).value).toBe('a = 1'))
    fireEvent.click(screen.getByRole('button', { name: 'Check with the node', hidden: true }))
    await waitFor(() => expect(screen.getByText('What the node would say')).toBeTruthy())
    expect(sent[0].url).toMatch(/grant\/check$/)
    fireEvent.click(screen.getByRole('button', { name: 'Revoke', hidden: true }))
    await waitFor(() => expect(screen.getByText('REVOKE OBJECT "c"."t" FROM ROLE "r"')).toBeTruthy())
    // the button says what it does
    fireEvent.click(screen.getByRole('button', { name: 'Revoke', hidden: true }))
    await waitFor(() => expect(sent).toHaveLength(3))
    expect(sent[2].body).toMatchObject({ version: 'v1', form: { op: 'revoke', role: 'r', target: 't' },
      statements: ['REVOKE OBJECT "c"."t" FROM ROLE "r"'] })
  })

  it('offers only the revoke of a grant the node could not read', async () => {
    const read = { exists: true, version: 'v2', grant: { level: 'object', role: 'r', catalog: 'c', target: 't', caps: null, unreadable: true } }
    show({ level: 'object', catalog: 'c', role: 'r', target: 't' }, read)
    await waitFor(() => expect(screen.getByText(/could not read this grant's columns or masks/)).toBeTruthy())
    expect(screen.queryByRole('button', { name: 'Show the statements', hidden: true })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Check with the node', hidden: true })).toBeNull()
    expect(screen.getByRole('button', { name: 'Revoke', hidden: true })).toBeTruthy()
  })
})
