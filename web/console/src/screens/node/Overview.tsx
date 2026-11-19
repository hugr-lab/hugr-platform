import { useEffect, useRef, useState } from 'react'
import { useParams } from 'react-router-dom'
import { Bar, Problem, Ring } from '../../components/ui'
import { useApp, useLoad } from '../../lib/context'
import { bytes, share } from '../../lib/format'
import { NodeFrame, entries } from './Frame'

const sampleWait = 20_000 // ms: the metrics API samples a pod about once a minute
const samplesAsked = 6

export function Overview() {
  const { label = '' } = useParams()
  const { api } = useApp()
  const pod = useLoad(() => api.pod(label), [api, label])
  // the engine's memory by use is duckdb_memory(): a statement, whose session's end is the node's activity - read only
  // when asked, so looking at a node leaves its idle time alone (spec 012); the totals come with the load report
  // asked for one node: another node's page starts without it, in the same render (never a read of the new node first)
  const [byUseOf, setByUseOf] = useState<string>()
  const byUse = byUseOf === label
  const memory = useLoad(() => (byUse ? api.memory(label) : Promise.resolve(undefined)), [api, label, byUse])
  // a new pod has no sample for up to a minute: read it again until it has one (a few times, then the person's Refresh)
  const asked = useRef(0)
  useEffect(() => {
    asked.current = 0 // another node: its own few reads
  }, [label])
  const pending = pod.data?.usagePending === true
  useEffect(() => {
    if (!pending || asked.current >= samplesAsked) return
    const timer = setTimeout(() => {
      asked.current++
      pod.reload()
    }, sampleWait)
    return () => clearTimeout(timer)
  }, [pending, pod.data])
  return (
    <NodeFrame label={label} alsoReload={() => { pod.reload(); if (byUse) memory.reload() }}>
      {(page) => {
        const load = page.data?.load
        const p = pod.data
        const m = memory.data
        const tags = (m?.tags.rows ?? [])
          .map((r) => ({ tag: String(r.tag), used: Number(r.memory_usage_bytes ?? 0) }))
          .filter((t) => t.used > 0)
          .sort((a, b) => b.used - a.used)
        const engineUsed = tags.reduce((n, t) => n + t.used, 0)
        const size = (v: unknown) => (typeof v === 'number' ? v : null)
        const used = size(load?.memory_used)
        const working = size(load?.memory_working)
        const limit = size(load?.memory_limit)
        const spill = size(load?.temp_used)
        const spillLimit = size(load?.temp_limit)
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
                    {load.group ? <div className="mono text-lg">{String(load.group)}</div> : <div className="text-lg font-semibold">none</div>}
                    <div className="text-sm text-ink-muted">{!load.group ? 'it serves everyone' : load.group_default ? 'the default group' : load.group_known === false ? 'not in the policy' : ''}</div>
                  </div>
                  <div className="panel flex flex-col gap-2">
                    <div className="eyebrow">Profile</div>
                    {load.config_target === null || load.config_target === undefined ? (
                      <><div className="text-lg font-semibold">none</div><div className="text-sm text-ink-muted">a standalone node has no cluster profile</div></>
                    ) : (
                      <><div className="text-[32px] font-semibold leading-10">{String(load.config_target)}</div>
                        <div className="text-sm text-ink-muted">applied {String(load.config_applied ?? '-')}</div></>
                    )}
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
                        <Ring percent={p.usage && p.limits.memoryBytes ? share(p.usage.memoryBytes, p.limits.memoryBytes) : undefined}
                          label="the share of the pod's memory limit" />
                        <dl className="flex flex-col gap-2 text-sm">
                          <div><dt className="eyebrow">Memory</dt><dd className="text-lg font-semibold">{p.usage ? `${bytes(p.usage.memoryBytes)} of ${bytes(p.limits.memoryBytes || null)}` : '-'}</dd></div>
                          <div><dt className="eyebrow">CPU</dt><dd className="font-semibold">{p.usage ? `${(p.usage.cpuMilli / 1000).toFixed(1)} of ${(p.requests.cpuMilli / 1000).toFixed(1)} cores requested` : '-'}</dd></div>
                        </dl>
                      </div>
                      {p.usageError && (
                        <p className="text-sm text-ink-muted">
                          {p.usageError}{p.usagePending && asked.current >= samplesAsked ? '. Refresh to read it again.' : ''}
                        </p>
                      )}
                      <p className="text-sm text-ink-muted">
                        Restarted {p.restarts} {p.restarts === 1 ? 'time' : 'times'}
                        {p.lastTermination && <>, last <span className="font-semibold text-ink">{p.lastTermination.reason}</span> (exit {p.lastTermination.exitCode})</>}
                      </p>
                    </>
                  )}
                </div>
                <div className="flex flex-col gap-6 rounded-lg border border-line p-8">
                  <div className="flex items-baseline justify-between gap-4"><h3 className="text-lg font-semibold">The engine</h3><span className="text-sm text-ink-muted">from its load report</span></div>
                  {load && used !== null ? (
                    <>
                      <div className="flex items-center gap-6">
                        <Ring percent={working !== null && limit ? share(working, limit) : undefined}
                          label="the share of the memory limit the engine works with" />
                        <dl className="flex flex-col gap-2 text-sm">
                          <div><dt className="eyebrow">Working</dt><dd className="text-lg font-semibold">{bytes(working)}{limit ? ` of ${bytes(limit)}` : ''}</dd></div>
                          <div><dt className="eyebrow">Buffer pool</dt><dd className="font-semibold">{bytes(used)}</dd></div>
                          <div><dt className="eyebrow">Spill</dt><dd className="font-semibold">{bytes(spill)}{spillLimit ? ` of ${bytes(spillLimit)}` : ''}</dd></div>
                        </dl>
                      </div>
                      {load.memory_critical === true && (
                        <p role="note" className="text-sm font-semibold">Past the critical level: the node admits no new session or stream.</p>
                      )}
                      <p className="text-sm text-ink-muted">The buffer pool also holds what the engine has cached and drops when it needs room; working memory leaves that out.</p>
                    </>
                  ) : (
                    <p className="text-sm text-ink-muted">{load ? "This node's load report carries no memory figures." : 'No load report.'}</p>
                  )}
                  {!byUse && (
                    <div className="flex flex-col gap-2">
                      <button type="button" className="btn-outline self-start" onClick={() => setByUseOf(label)}>By use</button>
                      <p className="text-sm text-ink-muted">Reads <span className="mono">duckdb_memory()</span> - a statement on the node, which counts as its activity.</p>
                    </div>
                  )}
                  {byUse && memory.error !== undefined && <Problem error={memory.error} />}
                  {byUse && m?.tags.error && <p role="note" className="text-sm">{m.tags.error} - reading the engine's memory takes the policy's meta category.</p>}
                  {byUse && m && !m.tags.error && (
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
