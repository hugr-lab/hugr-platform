import { useState } from 'react'
import { useParams } from 'react-router-dom'
import { Problem, Segmented, Status, type Mark } from '../../components/ui'
import { useApp, useLoad } from '../../lib/context'
import { fill, linksOf, traceOf } from '../../lib/format'
import { NodeFrame } from './Frame'

type Kind = 'all' | 'statement' | 'session' | 'door' | 'policy'

function verdictMark(verdict: string | null): Mark {
  if (verdict === 'denied' || verdict === 'refused' || verdict === 'error') return 'half'
  if (!verdict) return 'off'
  return 'ok'
}

export function Events() {
  const { label = '' } = useParams()
  const { api, config } = useApp()
  const events = useLoad(() => api.events(label), [api, label])
  const [kind, setKind] = useState<Kind>('all')
  const telemetry = linksOf(config.links, 'telemetry')
  const rows = (events.data?.events ?? []).filter((e) =>
    kind === 'all' || e.kind === kind || (kind === 'statement' && (e.kind === 'profile' || e.kind === 'admin')))
  return (
    <NodeFrame label={label} alsoReload={events.reload}>
      {() => (
        <section aria-labelledby="ev-h" className="flex flex-col gap-4">
          <div className="flex flex-wrap items-end justify-between gap-4">
            <div className="max-w-[640px]">
              <h2 id="ev-h" className="text-[22px] font-semibold leading-[30px]">Recent events</h2>
              <p className="text-sm text-ink-muted">The node's own ring: the latest events since it started, never a statement's text or a claim's value. The history is the telemetry receiver's.</p>
            </div>
            <Segmented legend="Which events" name="kind" value={kind} onChange={setKind}
              options={[{ value: 'all', label: 'All' }, { value: 'statement', label: 'Statements' }, { value: 'session', label: 'Sessions' },
                { value: 'door', label: 'Doors' }, { value: 'policy', label: 'Policy' }]} />
          </div>
          {events.error !== undefined && <Problem error={events.error} />}
          <div className="overflow-x-auto rounded-md border border-line">
            <table className="w-full min-w-[1080px] border-collapse">
              <thead>
                <tr>
                  <th className="th">Time</th><th className="th">Kind</th><th className="th">Verdict</th><th className="th">Principal</th>
                  <th className="th">Door</th><th className="th">What</th><th className="th">Took</th>
                  {telemetry.length > 0 && <th className="th"><span className="sr-only">Trace</span></th>}
                </tr>
              </thead>
              <tbody>
                {rows.map((e) => {
                  const trace = traceOf(e.traceparent)
                  const link = telemetry.map((l) => ({ name: l.name, href: fill(l, { node: label, session: e.session ?? undefined, trace }) }))
                    .find((l) => l.href)
                  return (
                    <tr key={e.seq}>
                      <td className="td tabular-nums text-ink-muted">{new Date(e.ts).toLocaleTimeString()}</td>
                      <td className="td font-semibold">{e.kind}</td>
                      <td className="td">{e.verdict ? <Status mark={verdictMark(e.verdict)}>{e.verdict}</Status> : '-'}</td>
                      <td className="td break-all">{e.subject ?? '-'}</td>
                      <td className="td">{e.door ?? '-'}</td>
                      <td className="td">
                        <div>{[e.statement, e.rows !== null ? `${e.rows} rows` : '', e.detail].filter(Boolean).join(' · ') || '-'}</div>
                        {e.reason_code && <div className="mono text-xs text-ink-muted">{e.reason_code}</div>}
                      </td>
                      <td className="td tabular-nums text-ink-muted">{e.duration_us !== null ? `${(e.duration_us / 1000).toFixed(1)} ms` : '-'}</td>
                      {telemetry.length > 0 && (
                        <td className="td text-right">
                          {link && <a className="link" href={link.href} target="_blank" rel="noopener noreferrer">Trace</a>}
                        </td>
                      )}
                    </tr>
                  )
                })}
                {events.data && rows.length === 0 && <tr><td className="td text-ink-muted" colSpan={8}>No such events in the ring.</td></tr>}
              </tbody>
            </table>
          </div>
        </section>
      )}
    </NodeFrame>
  )
}
