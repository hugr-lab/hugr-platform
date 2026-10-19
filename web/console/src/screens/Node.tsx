import { useState, type ReactNode } from 'react'
import { CirclePause, Play, X } from 'lucide-react'
import { NavLink, useParams } from 'react-router-dom'
import { Crumbs, Dialog, Problem, Refresh, Status, age, messageOf } from '../components/ui'
import { useApp, useLoad } from '../lib/context'
import type { NodePage, SessionRow } from '../lib/types'
import { nodeState } from './Group'

/** a MAP column as duckdb's Arrow hands it: a list of {key, value} */
function entries(v: unknown): [string, unknown][] {
  if (Array.isArray(v)) return v.map((e) => [String((e as { key?: unknown }).key), (e as { value?: unknown }).value])
  if (v && typeof v === 'object') return Object.entries(v as Record<string, unknown>)
  return []
}

function NodeFrame({ label, page, tab, actions, children }: { label: string; page?: NodePage; tab: string;
  actions?: ReactNode; children: ReactNode }) {
  const k = page?.kubernetes
  const tabClass = ({ isActive }: { isActive: boolean }) =>
    `inline-flex min-h-[44px] items-center rounded-full px-[18px] text-sm font-semibold transition-colors ${isActive ? 'bg-navy text-white' : 'hover:bg-surface-soft'}`
  const base = `/nodes/${encodeURIComponent(label)}`
  return (
    <>
      <Crumbs items={[{ label: 'Fleet', to: '/' }, ...(page ? [{ label: page.group, to: `/groups/${encodeURIComponent(page.group)}` }] : []),
        { label: <span className="mono">{label}</span> }]} />
      <header className="flex flex-wrap items-end justify-between gap-4">
        <div className="flex min-w-0 flex-col gap-3">
          <h1 className="break-all font-mono text-[26px] font-medium leading-9">{label}</h1>
          {k && (
            <div className="flex flex-wrap items-center gap-2">
              <Status mark={nodeState(k).mark}>{nodeState(k).text}</Status>
              {k.admitting && <Status mark="ok">Admitting</Status>}
              <span className="text-sm text-ink-muted">group <span className="mono text-ink">{page.group}</span> · profile version {k.target}{k.applied === k.target ? ', applied' : `, ${k.applied} applied`}</span>
            </div>
          )}
        </div>
        {actions}
      </header>
      <nav aria-label="This node" className="flex flex-wrap gap-1" data-tab={tab}>
        <NavLink end to={base} className={tabClass}>Overview</NavLink>
        <NavLink to={`${base}/sessions`} className={tabClass}>Sessions</NavLink>
      </nav>
      {children}
    </>
  )
}

export function Node() {
  const { label = '' } = useParams()
  const { api, rights } = useApp()
  const page = useLoad(() => api.node(label), [api, label])
  const [asking, setAsking] = useState(false)
  const [error, setError] = useState<string>()
  const load = page.data?.load
  const draining = load?.draining === true
  const act = async (run: () => Promise<unknown>) => {
    setError(undefined)
    try {
      await run()
      page.reload()
    } catch (e) {
      setError(messageOf(e)) // the node judged again: its message
    }
  }
  const actions = (
    <div className="flex flex-wrap items-center gap-2">
      <Refresh at={page.at} loading={page.loading} onClick={page.reload} />
      {load && !draining && rights.drain && (
        <button type="button" className="btn-outline" onClick={() => setAsking(true)}>
          <CirclePause size={18} aria-hidden="true" /> Drain
        </button>
      )}
      {load && draining && rights.resume && (
        <button type="button" className="btn-primary" onClick={() => act(() => api.resume(label))}>
          <Play size={18} aria-hidden="true" /> Resume
        </button>
      )}
    </div>
  )
  return (
    <NodeFrame label={label} page={page.data} tab="overview" actions={actions}>
      {page.error !== undefined && <Problem error={page.error} />}
      {error && <Problem error={new Error(error)} />}
      {page.data?.loadError && <Problem error={new Error(`${page.data.loadError}: what Kubernetes knows is above.`)} />}
      {draining && (
        <div role="status" className="rounded-md bg-navy px-6 py-4 text-white">
          <div className="font-semibold">This node drains</div>
          <div className="text-sm text-[#e8f1f2]">New clients go to the group's other nodes; the open sessions keep working. The node never ends them by itself: end the stragglers, or resume.</div>
        </div>
      )}
      {load && (
        <section aria-labelledby="load-h" className="flex flex-col gap-4">
          <h2 id="load-h" className="text-[22px] font-semibold leading-[30px]">Load</h2>
          <div className="grid grid-cols-[repeat(auto-fit,minmax(220px,1fr))] gap-4">
            <div className="panel flex flex-col gap-2">
              <div className="eyebrow">Sessions</div>
              <div className="flex items-baseline gap-2"><span className="text-[32px] font-semibold leading-10">{String(load.sessions_live ?? '-')}</span><span className="text-ink-muted">of {String(load.sessions_max ?? '-')}</span></div>
              <div className="text-sm text-ink-muted">{entries(load.sessions_by_door).map(([d, n]) => `${d} ${String(n)}`).join(' · ')}</div>
            </div>
            <div className="panel flex flex-col gap-2">
              <div className="eyebrow">Admits</div>
              <ul className="flex flex-col gap-1 text-sm">
                {[['New sessions', load.admit_new_session], ['New quack clients', load.admit_new_quack_client], ['New streams', load.admit_new_stream]].map(([n, v]) => (
                  <li key={String(n)} className="flex justify-between gap-2"><span>{String(n)}</span><span className="font-semibold">{v ? 'yes' : 'no'}</span></li>
                ))}
              </ul>
            </div>
            <div className="panel flex flex-col gap-2">
              <div className="eyebrow">Resource group</div>
              <div className="mono text-lg">{String(load.group ?? 'none: serves everyone')}</div>
              <div className="text-sm text-ink-muted">{load.group_default ? 'the default group' : load.group_known === false ? 'not in the policy' : ''}</div>
            </div>
            <div className="panel flex flex-col gap-2">
              <div className="eyebrow">Profile</div>
              <div className="text-[32px] font-semibold leading-10">{String(load.config_target ?? '-')}</div>
              <div className="text-sm text-ink-muted">applied {String(load.config_applied ?? '-')}</div>
            </div>
          </div>
        </section>
      )}
      <Dialog open={asking} title="Drain this node?" onClose={() => setAsking(false)}>
        <p className="text-ink-muted">It stops taking new clients; the front sends them to the group's other nodes. The open sessions keep working until they end or you end them. Resume undoes it.</p>
        <div className="flex flex-wrap justify-end gap-2">
          <button type="button" className="btn-outline" onClick={() => setAsking(false)}>Cancel</button>
          <button type="button" className="btn-primary" onClick={() => { setAsking(false); void act(() => api.drain(label)) }}>Drain</button>
        </div>
      </Dialog>
    </NodeFrame>
  )
}

export function NodeSessions() {
  const { label = '' } = useParams()
  const { api, rights } = useApp()
  const page = useLoad(() => api.node(label), [api, label])
  const list = useLoad(() => api.sessions(label), [api, label])
  const [ending, setEnding] = useState<SessionRow | null>(null)
  const [error, setError] = useState<string>()
  const end = async (s: SessionRow) => {
    setEnding(null)
    setError(undefined)
    try {
      await api.endSession(label, s.id)
      list.reload()
    } catch (e) {
      setError(messageOf(e))
    }
  }
  const sessions = list.data?.sessions ?? []
  return (
    <NodeFrame label={label} page={page.data} tab="sessions" actions={<Refresh at={list.at} loading={list.loading} onClick={list.reload} />}>
      {list.error !== undefined && <Problem error={list.error} />}
      {error && <Problem error={new Error(error)} />}
      <section aria-labelledby="sessions-h" className="flex flex-col gap-4">
        <div className="flex flex-wrap items-baseline justify-between gap-2">
          <h2 id="sessions-h" className="text-[22px] font-semibold leading-[30px]">Sessions</h2>
          <span className="text-sm text-ink-muted">The id is for operations; a session's handle never leaves the node.</span>
        </div>
        <div className="overflow-x-auto rounded-md border border-line">
          <table className="w-full min-w-[1000px] border-collapse">
            <thead>
              <tr>
                <th className="th">Session</th>
                <th className="th">Principal</th>
                <th className="th">Door</th>
                <th className="th">Idle</th>
                <th className="th">Audit</th>
                <th className="th">Profile</th>
                {rights.endSession && <th className="th text-right">Actions</th>}
              </tr>
            </thead>
            <tbody>
              {sessions.map((s) => (
                <tr key={s.id}>
                  <td className="td mono">{s.id}</td>
                  <td className="td">
                    <div className="break-all">{s.subject}</div>
                    <div className="mono text-xs text-ink-muted">{(s.roles ?? []).join(', ')}</div>
                  </td>
                  <td className="td">{s.door}</td>
                  <td className="td tabular-nums text-ink-muted">{age(s.idle_seconds)}</td>
                  <td className="td"><div className="font-semibold">{s.audit_level ?? '-'}</div><div className="text-[13px] text-ink-muted">{s.audit_level_source ?? ''}</div></td>
                  <td className="td"><div className="font-semibold">{s.profile_level ?? '-'}</div><div className="text-[13px] text-ink-muted">{s.profile_source ?? ''}</div></td>
                  {rights.endSession && (
                    <td className="td text-right">
                      <button type="button" className="btn-quiet" aria-label={`End session ${s.id}`} onClick={() => setEnding(s)}>
                        <X size={16} aria-hidden="true" /> End
                      </button>
                    </td>
                  )}
                </tr>
              ))}
              {list.data && sessions.length === 0 && (
                <tr><td className="td text-center text-ink-muted" colSpan={7}>No open sessions. A draining node with none left can stop.</td></tr>
              )}
            </tbody>
          </table>
        </div>
        {!rights.endSession && (
          <p className="text-sm text-ink-muted">Ending a session takes the operate right; the node judges it again with your token.</p>
        )}
      </section>
      <Dialog open={!!ending} title={<>End session <span className="mono text-xl">{ending?.id}</span>?</>} onClose={() => setEnding(null)}>
        <p className="text-ink-muted">{ending?.subject} is disconnected from this node; a statement it runs stops and an open transaction is rolled back. Its client may connect again.</p>
        <div className="flex flex-wrap justify-end gap-2">
          <button type="button" className="btn-outline" onClick={() => setEnding(null)}>Cancel</button>
          <button type="button" className="btn-primary" onClick={() => ending && end(ending)}>End session</button>
        </div>
      </Dialog>
    </NodeFrame>
  )
}
