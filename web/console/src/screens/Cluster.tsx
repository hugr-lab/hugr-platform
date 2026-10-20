import { useState } from 'react'
import { Header, Out, Problem, Refresh, Segmented, Status } from '../components/ui'
import { useApp, useLoad } from '../lib/context'
import { home, linksOf } from '../lib/format'

/** the cluster's settings, read only (spec 003 round 2, Q10): the profile, where telemetry and lineage go */
export function Cluster() {
  const { api } = useApp()
  const page = useLoad(() => api.cluster(), [api])
  const [scope, setScope] = useState('all')
  const profile = page.data?.profile
  const scopes = ['all', ...new Set((profile?.rows ?? []).map((r) => String(r.scope)))]
  const items = (profile?.rows ?? []).filter((r) => scope === 'all' || r.scope === scope)
  const links = page.data?.links ?? []
  const telemetry = linksOf(links, 'telemetry')
  const lineage = linksOf(links, 'lineage')
  return (
    <>
      <Header title="Cluster"
        lead={<>What every node is built from, and where its telemetry and lineage go. Read only here: the profile is written with <span className="mono">ACL CLUSTER</span> statements, the receivers by a passthrough administrator.</>}
        actions={<Refresh at={page.at} loading={page.loading} onClick={page.reload} />} />
      {page.error !== undefined && <Problem error={page.error} />}
      {page.data && (
        <>
          <section aria-labelledby="profile-h" className="flex flex-col gap-4">
            <div className="flex flex-wrap items-end justify-between gap-4">
              <div className="max-w-[720px]">
                <h2 id="profile-h" className="text-[22px] font-semibold leading-[30px]">The profile</h2>
                <p className="text-sm text-ink-muted">A group's item overrides the cluster's of the same kind and name on that group's nodes. Hot items apply at once, restart and drain items when a node restarts or drains.</p>
              </div>
              {!profile?.error && scopes.length > 2 && (
                <Segmented legend="Scope" name="scope" value={scope} onChange={setScope}
                  options={scopes.map((s) => ({ value: s, label: s === 'all' ? 'All' : s }))} />
              )}
            </div>
            {profile?.error ? (
              <p role="note" className="rounded-md border border-line px-6 py-4 text-sm">
                {profile.error.charAt(0).toUpperCase() + profile.error.slice(1)}.
              </p>
            ) : (
              <div className="overflow-x-auto rounded-md border border-line">
                <table className="w-full min-w-[1000px] border-collapse">
                  <thead>
                    <tr><th className="th">Scope</th><th className="th">Kind</th><th className="th">Name</th><th className="th">What</th>
                      <th className="th">Class</th><th className="th">Version</th><th className="th">Depends on</th></tr>
                  </thead>
                  <tbody>
                    {items.map((r, i) => (
                      <tr key={i}>
                        <td className="td">{String(r.scope)}</td>
                        <td className="td text-ink-muted">{String(r.kind)}</td>
                        <td className="td mono break-all">{String(r.name)}</td>
                        <td className="td mono break-all">{specText(r.spec)}</td>
                        <td className="td"><span className="pill">{String(r.class ?? '-')}</span></td>
                        <td className="td tabular-nums text-ink-muted">{String(r.version ?? '-')}</td>
                        <td className="td mono text-ink-muted">{Array.isArray(r.depends_on) && r.depends_on.length ? r.depends_on.join(', ') : '-'}</td>
                      </tr>
                    ))}
                    {items.length === 0 && <tr><td className="td text-ink-muted" colSpan={7}>The profile is empty.</td></tr>}
                  </tbody>
                </table>
              </div>
            )}
          </section>
          <section aria-labelledby="int-h" className="flex flex-col gap-4">
            <h2 id="int-h" className="text-[22px] font-semibold leading-[30px]">Integrations</h2>
            <div className="grid grid-cols-[repeat(auto-fit,minmax(420px,1fr))] gap-4">
              <div className="flex flex-col gap-6 rounded-lg border border-line p-8">
                <h3 className="text-lg font-semibold">Telemetry</h3>
                {telemetry.length > 0 ? (
                  <div className="flex flex-wrap gap-2">{telemetry.map((l) => { const h = home(l); return h && <Out key={l.name} href={h}>Open in {l.name}</Out> })}</div>
                ) : (
                  <p className="rounded-md bg-surface-soft p-4 text-sm">The installation names no telemetry receiver: each node keeps its recent events in its ring (a node's Audit events), and nothing older is kept.</p>
                )}
                <p className="text-sm text-ink-muted">Whether each node's acl_otel delivers is hugr-node's platform.telemetry, read here once nodes report it.</p>
              </div>
              <div className="flex flex-col gap-6 rounded-lg border border-line p-8">
                <h3 className="text-lg font-semibold">Lineage</h3>
                {lineage.length > 0 && <div className="flex flex-wrap gap-2">{lineage.map((l) => { const h = home(l); return h && <Out key={l.name} href={h}>Open in {l.name}</Out> })}</div>}
                <ul className="flex flex-col text-sm">
                  {page.data.lineage.map((n) => (
                    <li key={n.node} className="grid grid-cols-[minmax(0,1fr)_auto] items-baseline gap-4 border-t border-line py-2.5">
                      <span className="mono break-all">{n.node}</span>
                      {n.error ? <span className="text-ink-muted">{n.error}</span> : (
                        <Status mark={n.row?.sending ? 'ok' : 'off'}>
                          {n.row?.sending ? `sending, ${String(n.row?.namespace)}` : `not sending: ${String(n.row?.status ?? '-')}`}
                        </Status>
                      )}
                    </li>
                  ))}
                </ul>
              </div>
            </div>
          </section>
        </>
      )}
    </>
  )
}

/** a profile item's spec (a MAP: [{key, value}]) as a person reads it */
function specText(spec: unknown): string {
  if (Array.isArray(spec)) return spec.map((e) => `${String((e as { key?: unknown }).key)} ${String((e as { value?: unknown }).value)}`).join(', ') || '-'
  if (spec && typeof spec === 'object') return Object.entries(spec as Record<string, unknown>).map(([k, v]) => `${k} ${String(v)}`).join(', ')
  return spec === null || spec === undefined ? '-' : String(spec)
}
