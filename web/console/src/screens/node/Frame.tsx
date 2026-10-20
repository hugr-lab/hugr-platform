import { useState, type ReactNode } from 'react'
import { CirclePause, Play } from 'lucide-react'
import { NavLink } from 'react-router-dom'
import { Crumbs, Dialog, Out, Problem, Refresh, Status, messageOf } from '../../components/ui'
import { useApp, useLoad, type Loaded } from '../../lib/context'
import { fill, linksOf } from '../../lib/format'
import type { NodePage } from '../../lib/types'
import { nodeState } from '../Group'

/** the node's page frame (spec 003 round 2): who it is, drain or resume, and the tabs its rights open */
export function NodeFrame({ label, children, alsoReload }: { label: string; children: (page: Loaded<NodePage>) => ReactNode;
  alsoReload?: () => void }) {
  const { api, rights, config } = useApp()
  const page = useLoad(() => api.node(label), [api, label])
  const [asking, setAsking] = useState(false)
  const [error, setError] = useState<string>()
  const k = page.data?.kubernetes
  const load = page.data?.load
  const draining = load?.draining === true
  // a pod going away drains itself as it stops: a drain or a resume of it would be undone within the second
  const stopping = k?.pod?.state === 'stopping'
  const act = async (run: () => Promise<unknown>) => {
    setError(undefined)
    try {
      await run()
      page.reload()
    } catch (e) {
      setError(messageOf(e)) // the node judged again: its message
    }
  }
  const base = `/nodes/${encodeURIComponent(label)}`
  const tab = ({ isActive }: { isActive: boolean }) =>
    `inline-flex min-h-[44px] items-center rounded-full px-[18px] text-sm font-semibold transition-colors ${isActive ? 'bg-navy text-white' : 'hover:bg-surface-soft'}`
  const telemetry = linksOf(config.links, 'telemetry')
    .map((l) => ({ name: l.name, href: fill(l, { node: label, group: page.data?.group }) }))
    .filter((l): l is { name: string; href: string } => !!l.href)
  return (
    <>
      <Crumbs items={[{ label: 'Fleet', to: '/' },
        ...(page.data?.group ? [{ label: page.data.group, to: `/groups/${encodeURIComponent(page.data.group)}` }] : []),
        { label: <span className="mono">{label}</span> }]} />
      <header className="flex flex-wrap items-end justify-between gap-4">
        <div className="flex min-w-0 flex-col gap-3">
          <h1 className="break-all font-mono text-[26px] font-medium leading-9">{label}</h1>
          {k && (
            <div className="flex flex-wrap items-center gap-2">
              <Status mark={nodeState(k).mark}>{nodeState(k).text}</Status>
              {k.admitting && <Status mark="ok">Admitting</Status>}
              {draining && <Status mark="half">Draining</Status>}
              <span className="text-sm text-ink-muted">
                group <span className="mono text-ink">{page.data?.group}</span> ·{' '}
                {rights.deployment === 'standalone' ? 'no cluster profile (a standalone node)'
                  : `profile version ${k.target}${k.applied === k.target ? ', applied' : `, ${k.applied} applied`}`}
              </span>
            </div>
          )}
        </div>
        <div className="flex flex-wrap items-center gap-2">
          {telemetry.map((l) => <Out key={l.name} href={l.href}>Open in {l.name}</Out>)}
          <Refresh at={page.at} loading={page.loading} onClick={() => { page.reload(); alsoReload?.() }} />
          {load && !stopping && !draining && rights.drain && (
            <button type="button" className="btn-outline" onClick={() => setAsking(true)}>
              <CirclePause size={18} aria-hidden="true" /> Drain
            </button>
          )}
          {load && !stopping && draining && rights.resume && (
            <button type="button" className="btn-primary" onClick={() => act(() => api.resume(label))}>
              <Play size={18} aria-hidden="true" /> Resume
            </button>
          )}
        </div>
      </header>
      <nav aria-label="This node" className="flex flex-wrap gap-1">
        <NavLink end to={base} className={tab}>Overview</NavLink>
        {rights.sessions && <NavLink to={`${base}/sessions`} className={tab}>Sessions</NavLink>}
        <NavLink to={`${base}/inventory`} className={tab}>Sources and extensions</NavLink>
        <NavLink to={`${base}/logs`} className={tab}>Logs</NavLink>
        {rights.events && <NavLink to={`${base}/events`} className={tab}>Audit events</NavLink>}
      </nav>
      {error && <Problem error={new Error(error)} />}
      {draining && (
        <div role="status" className="rounded-md bg-navy px-6 py-4 text-white">
          <div className="font-semibold">This node drains</div>
          <div className="text-sm text-[#e8f1f2]">New clients go to the group's other nodes; the open sessions keep working. The node never ends them by itself: end the stragglers on Sessions, or resume.</div>
        </div>
      )}
      {children(page)}
      <Dialog open={asking} title="Drain this node?" onClose={() => setAsking(false)}>
        <p className="text-ink-muted">It stops taking new clients; the front sends them to the group's other nodes. The open sessions keep working until they end or you end them. Resume undoes it.</p>
        <div className="flex flex-wrap justify-end gap-2">
          <button type="button" className="btn-outline" onClick={() => setAsking(false)}>Cancel</button>
          <button type="button" className="btn-primary" onClick={() => { setAsking(false); void act(() => api.drain(label)) }}>Drain</button>
        </div>
      </Dialog>
    </>
  )
}

/** a MAP column as duckdb's Arrow hands it: a list of {key, value} */
export function entries(v: unknown): [string, unknown][] {
  if (Array.isArray(v)) return v.map((e) => [String((e as { key?: unknown }).key), (e as { value?: unknown }).value])
  if (v && typeof v === 'object') return Object.entries(v as Record<string, unknown>)
  return []
}
