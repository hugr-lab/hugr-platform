// The grant editor (spec 007 stage 2c): a catalog, schema or object grant, written as the DBeaver client's editors
// write it (acl-clients spec 008) with the columns of duckdb-acl spec 158. The grant is read first; the console's server
// composes the statements, shows them, then runs them one at a time and stops at the first the node refuses - and only
// while the grant is as it was read.
import { useEffect, useState } from 'react'
import { Dialog, Status, messageOf } from '../components/ui'
import { ApiError } from '../lib/api'
import { Rows } from '../components/ViewTable'
import { useApp } from '../lib/context'
import { calls } from '../lib/policy'
import type { GrantForm, GrantWritten, ViewPage } from '../lib/types'

/** every capability a grant may name, in the node's order (duckdb-acl spec 012 and after) */
export const CAPABILITIES = ['select', 'insert', 'update', 'delete', 'merge', 'create', 'drop', 'temp', 'explain',
  'secrets', 'lineage', 'ingest', 'manage']

const field = 'w-full rounded-sm border border-ink-muted bg-surface px-3 text-base text-ink'
const label = 'text-sm font-semibold'
const levelWord = { catalog: 'catalog', schema: 'schema', object: 'object' }

/** the names of a comma list as a person writes them, each trimmed */
export const namesOf = (text: string) => text.split(',').map((x) => x.trim()).filter(Boolean)

/** what the form lacks before the server composes it (the server and the node judge again) */
export function formProblem(f: GrantForm): string | undefined {
  if (!f.role.trim()) return 'Name the role.'
  if (f.level !== 'catalog' && !f.target?.trim()) return f.level === 'schema' ? 'Name the schema.' : 'Name the object.'
  if (f.columns && f.columns.mode !== 'all' && f.columns.names.length === 0) return 'Name the columns.'
  if ((f.masks ?? []).some((m) => !m.column.trim() || !m.mask.trim())) return 'Each mask names its column and its expression.'
  if (f.into && f.virtualOnly) return 'A schema grant creates in a physical schema or virtually only, not both.'
  return undefined
}

export function GrantDialog({ level, catalog, role = '', target = '', onDone }: { level: GrantForm['level'];
  catalog: string; role?: string; target?: string; onDone: (changed: boolean) => void }) {
  const { api, rights } = useApp()
  const editing = role !== ''
  const [form, setForm] = useState<GrantForm>({ level, catalog, role, target, caps: null })
  const [version, setVersion] = useState<string>()
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)
  const [preview, setPreview] = useState<GrantWritten>()
  // what the shown statements do: the form written, or the grant revoked
  const [op, setOp] = useState<'write' | 'revoke'>('write')
  const [written, setWritten] = useState<GrantWritten>()
  const [check, setCheck] = useState<ViewPage>()
  const [columns, setColumns] = useState('')
  // a 409: the grant changed since it was read - read it again (the form starts from it anew)
  const [stale, setStale] = useState(false)
  const [reads, setReads] = useState(0)
  // the grant as it stands: an edit starts from it, and is written only while it is as read
  useEffect(() => {
    if (!editing) {
      setVersion('')
      return
    }
    api.grantRead(level, role, catalog, target).then((r) => {
      setForm({ ...r.grant, level })
      setColumns(r.grant.columns?.names.join(', ') ?? '')
      setVersion(r.version)
      setPreview(undefined)
      setCheck(undefined)
    }, (e) => setError(messageOf(e)))
  }, [api, level, role, catalog, target, editing, reads])
  const set = (patch: Partial<GrantForm>) => {
    setForm((f) => ({ ...f, ...patch }))
    setPreview(undefined)
    setCheck(undefined)
  }
  const out: GrantForm = form.columns ? { ...form, columns: { ...form.columns, names: namesOf(columns) } } : form
  const problem = formProblem(out)
  const run = async (what: () => Promise<void>) => {
    setBusy(true)
    setError(undefined)
    try {
      await what()
    } catch (e) {
      setStale(e instanceof ApiError && e.code === 'grant_changed')
      setError(messageOf(e))
    } finally {
      setBusy(false)
    }
  }
  const show = (what: 'write' | 'revoke') => run(async () => {
    setPreview(await api.grantPreview({ ...out, op: what }, version ?? ''))
    setOp(what)
  })
  const title = `${editing ? 'The' : 'A new'} ${levelWord[level]} grant${editing ? ` of ${role}` : ''} in ${catalog}`
  if (written) {
    return (
      <Dialog open title={title} onClose={() => onDone(true)}>
        <Written written={written} />
        <div className="flex justify-end"><button type="button" className="btn-primary" onClick={() => onDone(true)}>Done</button></div>
      </Dialog>
    )
  }
  return (
    <Dialog open title={title} onClose={() => onDone(false)}>
      {version === undefined && !error && <p className="text-sm text-ink-muted">Reading the grant on a node…</p>}
      {form.unreadable && (
        <p role="alert" className="rounded-md border border-line px-4 py-3 text-sm">The node could not read this grant's columns or masks. Written anew it would show what they hide, so here it can only be revoked; write it whole from the SQL page.</p>
      )}
      {version !== undefined && (
        // the form stays as shown while the node is asked: what runs is what was shown
        <fieldset disabled={busy || form.unreadable} className="flex flex-col gap-4">
          {!editing && (
            <div className="grid grid-cols-2 gap-4">
              <Text id="g-role" title="Role" value={form.role} onChange={(v) => set({ role: v })} />
              {level !== 'catalog' && <Text id="g-target" title={level === 'schema' ? 'Schema path' : 'Object'} value={form.target ?? ''}
                onChange={(v) => set({ target: v })} hint={level === 'schema' ? 'raw, raw.eu' : 'A table, view or function of the catalog.'} />}
            </div>
          )}
          <Caps caps={form.caps} level={level} onChange={(caps) => set({ caps })} />
          {level === 'catalog' && (
            <label className="flex items-center gap-3 text-sm">
              <input type="checkbox" className="h-5 w-5" checked={!!form.main} onChange={(e) => set({ main: e.target.checked })} />
              Main: the role addresses this catalog's objects unqualified
            </label>
          )}
          {level !== 'schema' && (
            <Area id="g-rls" title="Row filter" value={form.rls ?? ''} onChange={(v) => set({ rls: v })}
              hint="A predicate over the object's columns, such as tenant = acl_claim('tenant'); empty for none." />
          )}
          {level === 'object' && (
            <fieldset className="flex flex-col gap-2">
              <legend className={label}>Columns the role sees</legend>
              {(['all', 'only', 'except'] as const).map((mode) => (
                <label key={mode} className="flex items-center gap-3 text-sm">
                  <input type="radio" name="g-columns" className="h-5 w-5" checked={(form.columns?.mode ?? 'all') === mode}
                    onChange={() => set({ columns: mode === 'all' ? undefined : { mode, names: [] } })} />
                  {mode === 'all' ? 'Every column' : mode === 'only' ? 'Only these' : 'Every column but these'}
                </label>
              ))}
              {form.columns && <Text id="g-names" title="Columns" value={columns}
                onChange={(v) => { setColumns(v); setPreview(undefined); setCheck(undefined) }} hint="Names of the object's columns, comma separated (a name with a comma: from the SQL page)." />}
            </fieldset>
          )}
          {level === 'object' && <Masks masks={form.masks ?? []} onChange={(masks) => set({ masks })} />}
          {level !== 'schema' && (editing && level === 'catalog' ? (
            <p className="text-sm text-ink-muted">Versions: {form.versions || 'current'} (written with the grant).</p>
          ) : (
            <Text id="g-versions" title="Versions" value={form.versions ?? ''} onChange={(v) => set({ versions: v })}
              hint={`Of a time-travel table: CURRENT, ANY, AT (VERSION => 3), BETWEEN … AND …; empty: ${level === 'object' ? "the catalog grant's" : 'the current one'}.`} />
          ))}
          {level === 'schema' && (
            <div className="grid grid-cols-2 gap-4">
              <Text id="g-into" title="Creates into" value={form.into ?? ''} onChange={(v) => set({ into: v })}
                hint="The physical schema the role's CREATE lands in: db.schema." />
              <label className="mt-7 flex items-center gap-3 text-sm">
                <input type="checkbox" className="h-5 w-5" checked={!!form.virtualOnly} onChange={(e) => set({ virtualOnly: e.target.checked })} />
                Virtual only: registers what exists
              </label>
              <Text id="g-comment" title="Comment" value={form.comment ?? ''} onChange={(v) => set({ comment: v })} />
            </div>
          )}
          {check && (
            <div className="flex flex-col gap-2">
              <div className={label}>What the node would say</div>
              <Rows page={check} columns={['item', 'name', 'type', 'status', 'message']} />
            </div>
          )}
          {preview && (
            <div className="flex flex-col gap-2">
              <div className={label}>{preview.statements.length ? 'The statements, run in this order' : 'Nothing changes.'}</div>
              {preview.statements.length > 0 && (
                <pre className="overflow-x-auto rounded-md bg-surface-soft p-4 font-mono text-[13px] leading-[22px]"><code>{preview.statements.join(';\n')}</code></pre>
              )}
              {op === 'revoke' && level === 'catalog' && (
                <p role="alert" className="text-sm">Revoking the catalog grant also takes the role's object grants in {catalog}.</p>
              )}
            </div>
          )}
        </fieldset>
      )}
      {problem && version !== undefined && <p className="text-sm text-ink-muted">{problem}</p>}
      {error && <p role="alert" className="rounded-md bg-surface-soft px-4 py-3 text-sm font-medium">{error}</p>}
      <div className="flex flex-wrap justify-end gap-2">
        <button type="button" className="btn-outline" disabled={busy} onClick={() => onDone(false)}>Cancel</button>
        {stale && <button type="button" className="btn-outline" disabled={busy}
          onClick={() => { setStale(false); setError(undefined); setVersion(undefined); setReads((n) => n + 1) }}>Read it again</button>}
        {preview && <button type="button" className="btn-quiet" disabled={busy} onClick={() => setPreview(undefined)}>Back</button>}
        {editing && version && !preview && <button type="button" className="btn-quiet" disabled={busy} onClick={() => show('revoke')}>Revoke</button>}
        {level !== 'schema' && calls(rights, 'check_grant') && !preview && !form.unreadable && (
          <button type="button" className="btn-outline" disabled={!!problem || busy || version === undefined}
            onClick={() => run(async () => setCheck(await api.grantCheck(out)))}>Check with the node</button>
        )}
        {preview ? (
          <button type="button" className="btn-primary" disabled={busy || preview.statements.length === 0}
            onClick={() => run(async () => setWritten(await api.grantWrite({ ...out, op }, version ?? '', preview.statements)))}>
            {op === 'revoke' ? 'Revoke' : 'Run'}
          </button>
        ) : !form.unreadable && (
          <button type="button" className="btn-primary" disabled={!!problem || busy || version === undefined}
            onClick={() => show('write')}>Show the statements</button>
        )}
      </div>
    </Dialog>
  )
}

/** what each statement answered, and the one the node refused */
export function Written({ written }: { written: GrantWritten }) {
  return (
    <ol className="flex flex-col gap-3">
      {written.statements.map((s, i) => {
        const a = written.answers[i]
        const refused = written.refused?.index === i
        return (
          <li key={i} className="flex flex-col gap-1">
            <code className="mono break-all text-[13px]">{s}</code>
            {i < written.answers.length ? (a ? (
              <div role="status" className="flex items-start gap-2 text-sm">
                <Status mark={a.ok ? 'ok' : 'half'}>{a.ok ? 'Done' : 'Not done'}</Status>
                {a.message && <span className="break-words text-ink-muted">{a.message}</span>}
              </div>
            ) : (
              <div role="status" className="flex items-start gap-2 text-sm">
                <Status mark="half">Ran</Status><span className="text-ink-muted">the node answered nothing of what it did: read the grant again</span>
              </div>
            )) : refused ? (
              <div role="alert" className="flex items-start gap-2 text-sm">
                <Status mark={written.refused!.unknown ? 'half' : 'off'}>{written.refused!.unknown ? 'Unknown' : 'Refused'}</Status>
                <span className="break-words">{written.refused!.message}</span>
              </div>
            ) : <div className="text-sm text-ink-muted">Not run: a statement before it was not written.</div>}
          </li>
        )
      })}
    </ol>
  )
}

function Text({ id, title, value, onChange, hint }: { id: string; title: string; value: string; onChange: (v: string) => void;
  hint?: string }) {
  return (
    <div className="flex flex-col gap-2">
      <label htmlFor={id} className={label}>{title}</label>
      <input id={id} className={`${field} h-11`} value={value} onChange={(e) => onChange(e.target.value)} spellCheck={false} />
      {hint && <p className="text-sm text-ink-muted">{hint}</p>}
    </div>
  )
}

function Area({ id, title, value, onChange, hint }: { id: string; title: string; value: string; onChange: (v: string) => void;
  hint?: string }) {
  return (
    <div className="flex flex-col gap-2">
      <label htmlFor={id} className={label}>{title}</label>
      <textarea id={id} rows={2} className={`${field} py-2 font-mono text-sm`} value={value} onChange={(e) => onChange(e.target.value)} spellCheck={false} />
      {hint && <p className="text-sm text-ink-muted">{hint}</p>}
    </div>
  )
}

/** capabilities: unstated (the level's default), or the ones held */
function Caps({ caps, level, onChange }: { caps: string[] | null; level: GrantForm['level']; onChange: (c: string[] | null) => void }) {
  const unstated = level === 'catalog' ? 'Unstated: every data capability' : "Unstated: inherited from the catalog's grant"
  return (
    <fieldset className="flex flex-col gap-2">
      <legend className={label}>Capabilities</legend>
      <label className="flex items-center gap-3 text-sm">
        <input type="checkbox" className="h-5 w-5" checked={caps === null} onChange={(e) => onChange(e.target.checked ? null : ['select'])} />
        {unstated}
      </label>
      {caps !== null && (
        <div role="group" aria-label="Capabilities held" className="flex flex-wrap gap-x-6 gap-y-2">
          {CAPABILITIES.map((c) => (
            <label key={c} className="flex items-center gap-2 text-sm">
              <input type="checkbox" className="h-5 w-5" checked={caps.includes(c)}
                onChange={(e) => onChange(e.target.checked ? CAPABILITIES.filter((x) => x === c || caps.includes(x)) : caps.filter((x) => x !== c))} />
              <span className="mono">{c}</span>
            </label>
          ))}
          {caps.length === 0 && <span className="text-sm text-ink-muted">None held.</span>}
        </div>
      )}
    </fieldset>
  )
}

/** the MASK items: what the role reads in place of a column - the expression is SQL over the object's columns */
function Masks({ masks, onChange }: { masks: { column: string; mask: string }[]; onChange: (m: { column: string; mask: string }[]) => void }) {
  const put = (i: number, patch: Partial<{ column: string; mask: string }>) => onChange(masks.map((m, j) => (j === i ? { ...m, ...patch } : m)))
  return (
    <fieldset className="flex flex-col gap-2">
      <legend className={label}>Masks</legend>
      {masks.map((m, i) => (
        <div key={i} className="grid grid-cols-[160px_1fr_auto] items-center gap-2">
          <input aria-label={`Mask ${i + 1}: column`} className={`${field} h-11`} value={m.column} onChange={(e) => put(i, { column: e.target.value })} spellCheck={false} />
          <input aria-label={`Mask ${i + 1}: expression`} className={`${field} h-11 font-mono text-sm`} value={m.mask} onChange={(e) => put(i, { mask: e.target.value })} spellCheck={false} />
          <button type="button" className="btn-quiet" aria-label={`Remove mask ${i + 1}`} onClick={() => onChange(masks.filter((_, j) => j !== i))}>Remove</button>
        </div>
      ))}
      <div><button type="button" className="btn-quiet" onClick={() => onChange([...masks, { column: '', mask: '' }])}>Add a mask</button></div>
      <p className="text-sm text-ink-muted">The expression reads the object's columns, such as '***-**-' || right(ssn, 4); the node stores it cast to the column's type.</p>
    </fieldset>
  )
}
