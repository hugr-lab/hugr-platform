import { useState } from 'react'
import { X } from 'lucide-react'
import { useParams } from 'react-router-dom'
import { Dialog, Problem, Segmented, age, messageOf } from '../../components/ui'
import { useApp, useLoad } from '../../lib/context'
import { fill, linksOf } from '../../lib/format'
import type { SessionRow } from '../../lib/types'
import { NodeFrame } from './Frame'

const sources: Record<string, string> = { instance: 'the node', policy: 'a policy rule', override: 'an operator override' }
const auditOptions = [
  { value: '', label: 'Inherit' },
  { value: 'off', label: 'Off' },
  { value: 'denied', label: 'Denied' },
  { value: 'decisions', label: 'Decisions' },
  { value: 'all', label: 'All' },
]
const profileOptions = [
  { value: '', label: 'Inherit' },
  { value: 'sampled', label: 'Sampled' },
  { value: 'all', label: 'All' },
]

export function Sessions() {
  const { label = '' } = useParams()
  const { api, rights, config } = useApp()
  const list = useLoad(() => api.sessions(label), [api, label])
  const [ending, setEnding] = useState<SessionRow | null>(null)
  const [tracing, setTracing] = useState<SessionRow | null>(null)
  const [error, setError] = useState<string>()
  const act = async (run: () => Promise<unknown>) => {
    setError(undefined)
    try {
      await run()
      list.reload()
    } catch (e) {
      setError(messageOf(e))
    }
  }
  const sessions = list.data?.sessions ?? []
  const telemetry = linksOf(config.links, 'telemetry')
  const actions = rights.endSession || rights.tracing
  return (
    <NodeFrame label={label} alsoReload={list.reload}>
      {() => (
        <>
          {list.error !== undefined && <Problem error={list.error} />}
          {error && <Problem error={new Error(error)} />}
          <section aria-labelledby="sessions-h" className="flex flex-col gap-4">
            <div className="flex flex-wrap items-baseline justify-between gap-2">
              <h2 id="sessions-h" className="text-[22px] font-semibold leading-[30px]">Sessions</h2>
              <span className="text-sm text-ink-muted">The id is for operations; a session's handle never leaves the node.</span>
            </div>
            <div className="overflow-x-auto rounded-md border border-line">
              <table className="w-full min-w-[1080px] border-collapse">
                <thead>
                  <tr>
                    <th className="th">Session</th>
                    <th className="th">Principal</th>
                    <th className="th">Door</th>
                    <th className="th">Idle</th>
                    <th className="th">Audit</th>
                    <th className="th">Profile</th>
                    {actions && <th className="th text-right">Actions</th>}
                  </tr>
                </thead>
                <tbody>
                  {sessions.map((s) => {
                    const links = telemetry.map((l) => ({ name: l.name, href: fill(l, { node: label, session: s.id }) }))
                      .filter((l) => l.href)
                    return (
                      <tr key={s.id}>
                        <td className="td mono">{s.id}</td>
                        <td className="td">
                          <div className="break-all">{s.subject}</div>
                          <div className="mono text-xs text-ink-muted">{(s.roles ?? []).join(', ')}</div>
                          {links.map((l) => (
                            <a key={l.name} className="link text-[13px]" href={l.href} target="_blank" rel="noopener noreferrer">In {l.name}</a>
                          ))}
                        </td>
                        <td className="td">{s.door}</td>
                        <td className="td tabular-nums text-ink-muted">{age(s.idle_seconds)}</td>
                        <td className="td">
                          <div className="font-semibold">{s.audit_level ?? '-'}</div>
                          <div className="text-[13px] text-ink-muted">{sources[s.audit_level_source ?? ''] ?? s.audit_level_source}</div>
                        </td>
                        <td className="td">
                          <div className="font-semibold">{s.profile_level ?? '-'}</div>
                          <div className="text-[13px] text-ink-muted">{sources[s.profile_source ?? ''] ?? s.profile_source}</div>
                        </td>
                        {actions && (
                          <td className="td">
                            <div className="flex flex-wrap justify-end gap-2">
                              {rights.tracing && (
                                <button type="button" className="btn-quiet" aria-label={`Tracing of session ${s.id}`} onClick={() => setTracing(s)}>Tracing</button>
                              )}
                              {rights.endSession && (
                                <button type="button" className="btn-quiet" aria-label={`End session ${s.id}`} onClick={() => setEnding(s)}>
                                  <X size={16} aria-hidden="true" /> End
                                </button>
                              )}
                            </div>
                          </td>
                        )}
                      </tr>
                    )
                  })}
                  {list.data && sessions.length === 0 && (
                    <tr><td className="td text-center text-ink-muted" colSpan={7}>No open sessions. A draining node with none left can stop.</td></tr>
                  )}
                </tbody>
              </table>
            </div>
            <p className="text-sm text-ink-muted">
              A profile is a statement's execution - time, rows, memory, the plan; acl_otel sends it as spans where the cluster's settings turn profile spans on. An override lives on this node's session record.
            </p>
            {!actions && <p className="text-sm text-ink-muted">Tracing a session and ending one take the operate right; the node judges each again with your token.</p>}
          </section>
          <Dialog open={!!ending} title={<>End session <span className="mono text-xl">{ending?.id}</span>?</>} onClose={() => setEnding(null)}>
            <p className="text-ink-muted">{ending?.subject} is disconnected from this node; a statement it runs stops and an open transaction is rolled back. Its client may connect again.</p>
            <div className="flex flex-wrap justify-end gap-2">
              <button type="button" className="btn-outline" onClick={() => setEnding(null)}>Cancel</button>
              <button type="button" className="btn-primary" onClick={() => { const s = ending; setEnding(null); if (s) void act(() => api.endSession(label, s.id)) }}>End session</button>
            </div>
          </Dialog>
          {tracing && <TracingDialog session={tracing} onDone={(change) => {
            setTracing(null)
            if (change) void act(() => api.tracing(label, tracing.id, change))
          }} />}
        </>
      )}
    </NodeFrame>
  )
}

/** the levels a session is traced at; Inherit gives the decision back to the policy's rules and the node */
function TracingDialog({ session, onDone }: { session: SessionRow; onDone: (change?: { audit: string; profile: string }) => void }) {
  const [audit, setAudit] = useState(session.audit_level_source === 'override' ? (session.audit_level ?? '') : '')
  const [profile, setProfile] = useState(session.profile_source === 'override' ? (session.profile_level ?? '') : '')
  return (
    <Dialog open title={<>Tracing <span className="mono text-xl">{session.id}</span></>} onClose={() => onDone()}>
      <p className="text-ink-muted">{session.subject} on this node. Inherit goes back to what the policy's rules or the node say.</p>
      <Segmented legend="Audit: which decisions are recorded" name="audit" options={auditOptions} value={audit} onChange={setAudit} />
      <div className="flex flex-col gap-2">
        <Segmented legend="Profile: which executions are measured" name="profile" options={profileOptions} value={profile} onChange={setProfile} />
        <span className="text-sm text-ink-muted">Sampled follows the client's traceparent; all measures every statement.</span>
      </div>
      <div className="flex flex-wrap justify-end gap-2">
        <button type="button" className="btn-outline" onClick={() => onDone()}>Cancel</button>
        <button type="button" className="btn-primary" onClick={() => onDone({ audit, profile })}>Apply</button>
      </div>
    </Dialog>
  )
}
