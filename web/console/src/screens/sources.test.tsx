import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { Api } from '../lib/api'
import type { Session } from '../lib/auth'
import { AppContext } from '../lib/context'
import { changeOf, changeProblem, lazyText } from '../lib/profile'
import type { Config, Group, GroupSources, Rights } from '../lib/types'
import { SourcesSection, onceEach, setText } from './GroupSources'

// jsdom has no showModal: a <dialog> stays closed there, and its content is found with hidden: true

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const config = { platform: 'corp', audiences: { nodes: 'hugr-nodes' }, mounts: {}, links: [], scopes: [] } as unknown as Config
const rights: Rights = { fleet: true, sessions: true, endSession: false, drain: false, resume: false, tracing: false,
  scale: true, policy: false, infrastructure: true, events: false, drift: true, profile: true, effective: true,
  deployment: 'cluster', bundles: ['observe', 'operate', 'cluster'], views: ['cluster_items'], functions: ['group_sources'] }

const group = { name: 'etl-k8s', resourceGroup: 'etl', nodes: [] } as unknown as Group

const page: GroupSources = {
  resourceGroup: 'etl',
  set: { all: false, names: ['sales'], lazy: true, idleSeconds: 1800 },
  rows: [
    { source: 'sales', roles: ['analyst'], listed: true },
    { source: 'crm', roles: ['analyst', 'ops'], listed: false, repair: 'CLUSTER SET SOURCES (sales, crm) IN GROUP etl' },
    { source: 'only_other', roles: ['ops'], listed: false, elsewhere: true,
      repair: `CLUSTER ATTACH ':memory:' AS "only_other" (TYPE duckdb) IN GROUP etl` },
  ],
  profile: ['sales', 'crm', 'archive'],
}

function show(r: Rights = rights, answer: GroupSources = page) {
  const sent: { url: string; body: unknown }[] = []
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    if (init?.method === 'POST') {
      sent.push({ url, body: JSON.parse(String(init.body)) })
      return new Response(JSON.stringify({ version: 9, class: 'drain', appliedHere: false, ok: true, message: '', node: 'n1' }), { status: 200 })
    }
    return new Response(JSON.stringify(answer), { status: 200 })
  }))
  const api = new Api({ getToken: async () => 't' }, 'hugr-nodes')
  render(
    <AppContext.Provider value={{ config, session: {} as Session, api, rights: r }}>
      <MemoryRouter><SourcesSection group={group} /></MemoryRouter>
    </AppContext.Provider>,
  )
  return sent
}

describe("a group's sources (spec 007 stage 2b)", () => {
  it('reads the set and its lazy marks', () => {
    expect(setText(page)).toBe('sales: attached on its first read, detached after 30 minutes unused.')
    expect(setText({ ...page, set: null })).toBe('No set: the nodes attach every source of the cluster and of the group.')
    expect(setText({ ...page, set: { all: true, names: [], lazy: true, idleSeconds: 0 } }))
      .toBe('Every source, attached on its first read, kept until the node stops (or its own idle time).')
    expect(lazyText(false, 0)).toBe('attached at once')
  })

  it('composes a set: a list, all lazy, a reset - never all at once nor an idle time alone', () => {
    expect(changeOf('sources', { sources: ['sales', 'crm'], lazy: true, idle: '1 hour' }, 'etl'))
      .toEqual({ action: 'sources', sources: ['sales', 'crm'], lazy: true, idle: '1 hour', group: 'etl' })
    expect(changeOf('sources', { all: true, sources: ['ignored'], lazy: true }, 'etl'))
      .toEqual({ action: 'sources', all: true, lazy: true, group: 'etl' })
    expect(changeProblem(changeOf('sources', { all: true }, 'etl'))).toMatch(/reset the group's set/)
    expect(changeProblem(changeOf('sources', { sources: [] }, 'etl'))).toMatch(/Pick the sources/)
    expect(changeProblem(changeOf('sources', { sources: ['a'], idle: '5 min' }, 'etl'))).toMatch(/mark it lazy/)
    expect(changeProblem(changeOf('sources', { sources: ['a'] }))).toMatch(/a group's/)
    expect(changeProblem(changeOf('reset-sources', {}, 'etl'))).toBeUndefined()
    // an attach's own marks
    expect(changeOf('attach', { path: ':memory:', alias: 'a', type: 'duckdb', lazy: true, idle: '30 min' }))
      .toEqual({ action: 'attach', path: ':memory:', alias: 'a', type: 'duckdb', lazy: true, idle: '30 min' })
    expect(changeProblem(changeOf('attach', { path: ':memory:', alias: 'a', type: 'duckdb', idle: '30 min' }))).toMatch(/mark it lazy/)
  })

  it('shows what the set leaves out, and a repair sends the source with the repair shown', async () => {
    const sent = show()
    await waitFor(() => expect(screen.getByText('left out')).toBeTruthy())
    expect(screen.getByText('listed')).toBeTruthy()
    expect(screen.getByText(/crm is read by the group's roles and left out/)).toBeTruthy()
    // another group's own source reads as such, not as left out of the set
    expect(screen.getByText("another group's source")).toBeTruthy()
    expect(screen.getByText(/only_other is read by the group's roles and attached only for another group/)).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Repair crm' }))
    expect(screen.getByText('CLUSTER SET SOURCES (sales, crm) IN GROUP etl')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Run', hidden: true }))
    await waitFor(() => expect(sent).toHaveLength(1))
    expect(sent[0].url).toMatch(/groups\/etl-k8s\/sources\/repair$/)
    expect(sent[0].body).toEqual({ source: 'crm', repair: 'CLUSTER SET SOURCES (sales, crm) IN GROUP etl' })
  })

  it('keeps the set and its editor when the check fails', async () => {
    show(rights, { ...page, rows: [], checkError: 'acl: no resource group "etl"' })
    await waitFor(() => expect(screen.getByText(/The check could not be read: acl: no resource group/)).toBeTruthy())
    expect(screen.getByText(/sales: attached on its first read/)).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Change the set' })).toBeTruthy()
  })

  it('offers a source once, in the node\'s fold', () => {
    expect(onceEach(['sales', 'Sales', 'crm', 'SALES', 'Crm'])).toEqual(['sales', 'crm'])
  })

  it('changes the set from the profile and the check', async () => {
    const sent = show()
    await waitFor(() => expect(screen.getByRole('button', { name: 'Change the set' })).toBeTruthy())
    fireEvent.click(screen.getByRole('button', { name: 'Change the set' }))
    // a source of the profile nobody reads yet is offered too; another group's own is not
    expect(screen.queryByLabelText('only_other')).toBeNull()
    fireEvent.click(screen.getByLabelText('archive'))
    fireEvent.click(screen.getByRole('button', { name: 'Write', hidden: true }))
    await waitFor(() => expect(sent).toHaveLength(1))
    expect(sent[0].url).toMatch(/cluster\/profile$/)
    expect(sent[0].body).toEqual({ action: 'sources', sources: ['sales', 'archive'], lazy: true, idle: '30 min', group: 'etl' })
  })

  it('is offered with the cluster bundle function only, repairs with the infrastructure right', async () => {
    show({ ...rights, functions: [] })
    expect(screen.queryByText('Sources')).toBeNull()
    cleanup()
    show({ ...rights, infrastructure: false })
    await waitFor(() => expect(screen.getByText('left out')).toBeTruthy())
    expect(screen.queryByRole('button', { name: 'Repair' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Change the set' })).toBeNull()
  })
})
