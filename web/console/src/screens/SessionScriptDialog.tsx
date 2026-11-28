// A session's script (spec 016; duckdb-acl spec 181): a resource group's, a role's or a role's grant of a group - the
// SQL a node runs as itself on a session's connection when it opens. Read first; tried first if the person asks (the
// node runs what such a session would run with this text in place, duckdb-acl spec 183); the server composes the
// statement, shows it, and runs it only while the script is as it was read - as the resource group editor does.
import { useEffect, useState } from 'react'
import { Dialog, messageOf } from '../components/ui'
import { ApiError } from '../lib/api'
import { useApp } from '../lib/context'
import type { GrantWritten, SessionScriptForm, SessionScriptRead, SessionScriptTried } from '../lib/types'
import { Written } from './GrantDialog'

const field = 'w-full rounded-sm border border-ink-muted bg-surface px-3 text-base text-ink'
const label = 'text-sm font-semibold'

/** the script's owner, as the dialog's title and the node's statement name it */
export function ownerOf(group: string, role: string): string {
  if (group && role) return `${role}'s grant of ${group}`
  return group ? `the resource group ${group}` : `the role ${role}`
}

/** what the form lacks before the server composes it (the server and the node judge again) */
export function scriptProblem(sql: string): string | undefined {
  if (!sql.trim()) return 'Write the script, or drop it.'
  if (new TextEncoder().encode(sql).length > 64 << 10) return 'The script is at most 64 KiB.'
  // eslint-disable-next-line no-control-regex
  if (/[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f-\u009f]/.test(sql)) return 'No control character but a line break and a tab.'
  return undefined
}

/** a trial's rows: each script that ran, then the session's verdict */
function Tried({ tried, session }: { tried: SessionScriptTried; session: string }) {
  const verdict = tried.rows.find((r) => r.level === 'session')
  const ran = tried.rows.filter((r) => r.level !== 'session')
  return (
    <div role="status" className="flex flex-col gap-2">
      <div className={label}>{verdict?.ok ? 'The session would be served.' : `The session would be refused: ${String(verdict?.message ?? '')}.`}</div>
      <table className="w-full text-sm">
        <thead><tr className="text-left text-ink-muted"><th>#</th><th>Script</th><th>On error</th><th>Result</th></tr></thead>
        <tbody>
          {ran.map((r) => (
            <tr key={String(r.pos)} className={r.edited ? 'font-semibold' : ''}>
              <td className="pr-3 align-top">{String(r.pos)}</td>
              <td className="pr-3 align-top">{r.edited ? 'this one' : ownerOf(String(r.group ?? ''), String(r.role ?? ''))}</td>
              <td className="pr-3 align-top">{String(r.on_error)}</td>
              <td className="align-top">{r.ok ? 'ran' : (
                <span><span className="mono">{String(r.error_class)}</span>{r.line ? ` at line ${String(r.line)}, column ${String(r.column)}` : ''}: {String(r.message)}</span>
              )}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {session && tried.group !== session && (
        <p className="text-sm text-ink-muted">No node of {session} took the trial: it ran on {tried.node}{tried.group ? ` of ${tried.group}` : ''}, whose sources and settings are its own.</p>
      )}
    </div>
  )
}

export function SessionScriptDialog({ group = '', role = '', onDone }: { group?: string; role?: string;
  onDone: (changed: boolean) => void }) {
  const { api, rights } = useApp()
  const [read, setRead] = useState<SessionScriptRead>()
  const [sql, setSql] = useState('')
  const [onError, setOnError] = useState<SessionScriptForm['onError']>('fail')
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)
  const [preview, setPreview] = useState<GrantWritten>()
  const [op, setOp] = useState<'set' | 'drop'>('set')
  const [written, setWritten] = useState<GrantWritten>()
  const [stale, setStale] = useState(false)
  const [reads, setReads] = useState(0)
  const [session, setSession] = useState({ role, group })
  const [tried, setTried] = useState<{ text: string; result: SessionScriptTried }>()
  useEffect(() => {
    api.sessionScriptRead(group, role).then((r) => {
      setRead(r)
      setSql(r.sql)
      setOnError(r.onError === 'continue' ? 'continue' : 'fail')
      setPreview(undefined)
      // the session tried: a grant's is its own; a role's on its first group; a group's with no role until one is picked
      setSession((s) => ({ role: role || s.role, group: group || s.group || r.groups[0] || '' }))
    }, (e) => setError(messageOf(e)))
  }, [api, group, role, reads])
  const form: SessionScriptForm = { group, role, sql, onError }
  const text = JSON.stringify([sql, onError])
  const problem = scriptProblem(sql)
  const run = async (what: () => Promise<void>) => {
    setBusy(true)
    setError(undefined)
    try {
      await what()
    } catch (e) {
      setStale(e instanceof ApiError && e.code === 'script_changed')
      if (e instanceof ApiError && e.code === 'not_shown') setPreview(undefined)
      setError(messageOf(e))
    } finally {
      setBusy(false)
    }
  }
  const show = (what: 'set' | 'drop') => run(async () => {
    setPreview(await api.sessionScriptPreview({ ...form, op: what }, read?.version ?? ''))
    setOp(what)
  })
  const title = `The session script of ${ownerOf(group, role)}`
  if (written) {
    return (
      <Dialog open title={title} onClose={() => onDone(true)}>
        <Written written={written} />
        <p className="text-sm text-ink-muted">Sessions opened from now on run it; a session already open keeps what it ran.</p>
        <div className="flex justify-end"><button type="button" className="btn-primary" onClick={() => onDone(true)}>Done</button></div>
      </Dialog>
    )
  }
  const refusedLast = tried?.text === text && !tried.result.rows.find((r) => r.level === 'session')?.ok
  const canTry = rights.functions.includes('try_session_script')
  return (
    <Dialog open title={title} onClose={() => onDone(false)}>
      {!read && !error && <p className="text-sm text-ink-muted">Reading the policy on a node…</p>}
      {read && (
        <fieldset disabled={busy} className="flex flex-col gap-4">
          <p className="text-sm">Run as the node, with its full rights, on the connection of every session {group && role ? `of ${role} on ${group}'s nodes` : group ? `on ${group}'s nodes` : `of ${role}`} when it opens, before its first statement. Stored as written: nothing checks it, and whoever holds the policy bundle reads it - name a secret in tresor, never a credential.</p>
          <div className="flex flex-col gap-2">
            <label htmlFor="ss-sql" className={label}>SQL</label>
            <textarea id="ss-sql" rows={8} className="w-full rounded-sm border border-ink-muted bg-surface px-3 py-2 font-mono text-[13px] leading-[22px] text-ink"
              value={sql} onChange={(e) => { setSql(e.target.value); setPreview(undefined) }} spellCheck={false} />
            <p className="text-sm text-ink-muted">It may read <span className="mono">getvariable('acl_session_roles')</span>, <span className="mono">acl_session_groups</span>, <span className="mono">acl_session_group</span> and <span className="mono">acl_session_subject</span>: whose session it is.</p>
          </div>
          <div className="flex flex-col gap-2">
            <label htmlFor="ss-on-error" className={label}>On error</label>
            <select id="ss-on-error" className={`${field} h-11`} value={onError}
              onChange={(e) => { setOnError(e.target.value as SessionScriptForm['onError']); setPreview(undefined) }}>
              <option value="fail">Refuse the session</option>
              <option value="continue">Log and serve</option>
            </select>
          </div>
          {canTry && !preview && (
            <fieldset className="flex flex-col gap-2">
              <legend className={label}>Try it first</legend>
              {!role && (
                <div className="flex flex-col gap-1">
                  <label htmlFor="ss-try-role" className="text-sm">A session of the role</label>
                  <select id="ss-try-role" className={`${field} h-11`} value={session.role} onChange={(e) => setSession((s) => ({ ...s, role: e.target.value }))}>
                    <option value="">none</option>
                    {read.roles.map((r) => <option key={r} value={r}>{r}</option>)}
                  </select>
                </div>
              )}
              {!group && read.groups.length > 0 && (
                <div className="flex flex-col gap-1">
                  <label htmlFor="ss-try-group" className="text-sm">Served on the group</label>
                  <select id="ss-try-group" className={`${field} h-11`} value={session.group} onChange={(e) => setSession((s) => ({ ...s, group: e.target.value }))}>
                    {read.groups.map((g) => <option key={g} value={g}>{g}</option>)}
                  </select>
                </div>
              )}
              <p className="text-sm text-ink-muted">The node runs what such a session would run, with this text in place of the stored one, on a connection it drops after - nothing is stored. It is a run as the node: what a script does beyond its connection (a write to a source, SET GLOBAL, ATTACH, INSTALL or LOAD, COPY … TO, an HTTP call) it does, once.</p>
              <div><button type="button" className="btn-outline" disabled={!!problem || busy}
                onClick={() => run(async () => setTried({ text, result: await api.sessionScriptTry({ ...form, op: 'set' }, session) }))}>Try</button></div>
              {tried && tried.text === text && <Tried tried={tried.result} session={session.group || group} />}
            </fieldset>
          )}
          {preview && (
            <div className="flex flex-col gap-2">
              <div className={label}>{preview.statements.length ? 'The statement' : 'Nothing changes.'}</div>
              {preview.statements.length > 0 && (
                <pre className="overflow-x-auto rounded-md bg-surface-soft p-4 font-mono text-[13px] leading-[22px]"><code>{preview.statements.join(';\n')}</code></pre>
              )}
              {refusedLast && op === 'set' && <p role="alert" className="text-sm">Its last trial refused the session.</p>}
            </div>
          )}
        </fieldset>
      )}
      {problem && read && op === 'set' && <p className="text-sm text-ink-muted">{problem}</p>}
      {error && <p role="alert" className="rounded-md bg-surface-soft px-4 py-3 text-sm font-medium">{error}</p>}
      <div className="flex flex-wrap justify-end gap-2">
        <button type="button" className="btn-outline" disabled={busy} onClick={() => onDone(false)}>Cancel</button>
        {stale && <button type="button" className="btn-outline" disabled={busy}
          onClick={() => { setStale(false); setError(undefined); setRead(undefined); setReads((n) => n + 1) }}>Read it again</button>}
        {preview && <button type="button" className="btn-quiet" disabled={busy} onClick={() => setPreview(undefined)}>Back</button>}
        {read?.exists && !preview && (
          <button type="button" className="btn-quiet" disabled={busy} onClick={() => show('drop')}>Drop</button>
        )}
        {preview ? (
          <button type="button" className="btn-primary" disabled={busy || preview.statements.length === 0}
            onClick={() => run(async () => setWritten(await api.sessionScriptWrite({ ...form, op }, read?.version ?? '', preview.statements)))}>
            {op === 'drop' ? 'Drop' : 'Write'}
          </button>
        ) : (
          <button type="button" className="btn-primary" disabled={!!problem || busy || !read}
            onClick={() => show('set')}>Show the statement</button>
        )}
      </div>
    </Dialog>
  )
}
