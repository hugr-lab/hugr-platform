import { useParams } from 'react-router-dom'
import { Bar, Problem, Ring } from '../../components/ui'
import { useApp, useLoad } from '../../lib/context'
import { bytes, share } from '../../lib/format'
import { NodeFrame, entries } from './Frame'

export function Overview() {
  const { label = '' } = useParams()
  const { api } = useApp()
  const pod = useLoad(() => api.pod(label), [api, label])
  const memory = useLoad(() => api.memory(label), [api, label])
  return (
    <NodeFrame label={label} alsoReload={() => { pod.reload(); memory.reload() }}>
      {(page) => {
        const load = page.data?.load
        const p = pod.data
        const m = memory.data
        const tags = (m?.tags.rows ?? [])
          .map((r) => ({ tag: String(r.tag), used: Number(r.memory_usage_bytes ?? 0) }))
          .filter((t) => t.used > 0)
          .sort((a, b) => b.used - a.used)
        const engineUsed = tags.reduce((n, t) => n + t.used, 0)
        const limits = m?.limits.rows[0] as Record<string, string> | undefined
        return (
          <>
            {page.error !== undefined && <Problem error={page.error} />}
            {page.data?.loadError && <Problem error={new Error(`${page.data.loadError}: what Kubernetes knows is below.`)} />}
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
            <section aria-labelledby="memory-h" className="flex flex-col gap-4">
              <div>
                <h2 id="memory-h" className="text-[22px] font-semibold leading-[30px]">Memory</h2>
                <p className="text-sm text-ink-muted">Now, as read. The history is in the telemetry backend.</p>
              </div>
              <div className="grid grid-cols-[repeat(auto-fit,minmax(360px,1fr))] gap-4">
                <div className="flex flex-col gap-6 rounded-lg border border-line p-8">
                  <div className="flex items-baseline justify-between gap-4"><h3 className="text-lg font-semibold">The pod</h3><span className="text-sm text-ink-muted">from Kubernetes</span></div>
                  {pod.error !== undefined && <Problem error={pod.error} />}
                  {p && (
                    <>
                      <div className="flex items-center gap-6">
                        <Ring percent={share(p.usage?.memoryBytes, p.limits.memoryBytes)} label="the share of the pod's memory limit" />
                        <dl className="flex flex-col gap-2 text-sm">
                          <div><dt className="eyebrow">Memory</dt><dd className="text-lg font-semibold">{p.usage ? `${bytes(p.usage.memoryBytes)} of ${bytes(p.limits.memoryBytes || null)}` : '-'}</dd></div>
                          <div><dt className="eyebrow">CPU</dt><dd className="font-semibold">{p.usage ? `${(p.usage.cpuMilli / 1000).toFixed(1)} of ${(p.requests.cpuMilli / 1000).toFixed(1)} cores requested` : '-'}</dd></div>
                        </dl>
                      </div>
                      {p.usageError && <p className="text-sm text-ink-muted">{p.usageError}</p>}
                      <p className="text-sm text-ink-muted">
                        Restarted {p.restarts} {p.restarts === 1 ? 'time' : 'times'}
                        {p.lastTermination && <>, last <span className="font-semibold text-ink">{p.lastTermination.reason}</span> (exit {p.lastTermination.exitCode})</>}
                      </p>
                    </>
                  )}
                </div>
                <div className="flex flex-col gap-6 rounded-lg border border-line p-8">
                  <div className="flex items-baseline justify-between gap-4"><h3 className="text-lg font-semibold">The engine</h3><span className="mono text-ink-muted">duckdb_memory()</span></div>
                  {m?.tags.error && <p role="note" className="text-sm">{m.tags.error} - reading the engine's memory takes the policy's meta category.</p>}
                  {m && !m.tags.error && (
                    <>
                      <dl className="flex flex-wrap gap-x-10 gap-y-2 text-sm">
                        <div><dt className="eyebrow">Buffer pool</dt><dd className="text-lg font-semibold">{bytes(engineUsed)}{limits?.memory_limit ? ` of ${limits.memory_limit}` : ''}</dd></div>
                        <div><dt className="eyebrow">Spill</dt><dd className="font-semibold">{bytes(m.spillBytes)}{limits?.max_temp_directory_size ? ` of ${limits.max_temp_directory_size}` : ''}</dd></div>
                      </dl>
                      <ul className="flex flex-col gap-2 text-sm" aria-label="The buffer pool by use">
                        {tags.slice(0, 6).map((t) => (
                          <li key={t.tag} className="grid grid-cols-[140px_minmax(0,1fr)_72px] items-center gap-3">
                            <span className="mono text-xs">{t.tag}</span>
                            <Bar dark percent={share(t.used, engineUsed)} />
                            <span className="text-right tabular-nums">{bytes(t.used)}</span>
                          </li>
                        ))}
                        {tags.length === 0 && <li className="text-ink-muted">Nothing held now.</li>}
                      </ul>
                    </>
                  )}
                </div>
              </div>
            </section>
            {p && (
              <section aria-labelledby="this-h" className="flex flex-col gap-4">
                <h2 id="this-h" className="text-[22px] font-semibold leading-[30px]">This node</h2>
                <dl className="panel grid grid-cols-[repeat(auto-fit,minmax(260px,1fr))] gap-x-8 gap-y-6 p-8 text-sm">
                  <div><dt className="eyebrow">Pod address</dt><dd className="mono">{p.podIP || '-'}</dd></div>
                  <div><dt className="eyebrow">Runs on</dt><dd className="mono break-all">{p.nodeName || '-'}</dd></div>
                  <div><dt className="eyebrow">Image</dt><dd className="mono break-all">{p.image}</dd></div>
                  <div><dt className="eyebrow">Image digest</dt><dd className="mono break-all">{p.imageID || '-'}</dd></div>
                  <div><dt className="eyebrow">Started</dt><dd>{p.startedAt ? new Date(p.startedAt).toLocaleString() : '-'}</dd></div>
                  <div><dt className="eyebrow">Doors' certificate</dt><dd>{p.certificateExpiry ? `expires ${new Date(p.certificateExpiry).toLocaleDateString()}` : 'not known yet'}</dd></div>
                </dl>
              </section>
            )}
          </>
        )
      }}
    </NodeFrame>
  )
}
