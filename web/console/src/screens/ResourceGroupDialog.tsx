// A resource group (spec 013): its limits, comment, default mark and roles, whether a group of nodes serves it or not
// (duckdb-acl spec 085). Read first; the console's server composes the statements, shows them, and runs them only
// while the group is as it was read - as the grant editor does (spec 007 stage 2c).
import { useEffect, useState } from 'react'
import { Dialog, messageOf } from '../components/ui'
import { ApiError } from '../lib/api'
import { useApp } from '../lib/context'
import type { GrantWritten, ResourceGroupForm, ResourceGroupLimits } from '../lib/types'
import { Written } from './GrantDialog'

const field = 'w-full rounded-sm border border-ink-muted bg-surface px-3 text-base text-ink'
const label = 'text-sm font-semibold'

/** the limits, the grammar's order, each with what 0 means (duckdb-acl's management SQL) */
export const LIMITS: { key: keyof ResourceGroupLimits; title: string; hint: string }[] = [
  { key: 'window_start', title: 'Fetch window at start', hint: "quack's batches ahead at a stream's start; 0: no window" },
  { key: 'window_max', title: 'Fetch window at most', hint: 'how far that window grows; 0: no cap' },
  { key: 'batch_bytes', title: 'Batch bytes', hint: "quack's batch target, in bytes" },
  { key: 'max_result_rows', title: 'Rows a result hands out', hint: 'a Flight stream refuses past them; 0: unlimited' },
  { key: 'queue_priority', title: 'Queue priority', hint: 'higher goes first in the stream budget; may be negative' },
  { key: 'max_sessions', title: 'Sessions at most', hint: 'live sessions charged to the group; 0: unlimited' },
]

export const NO_LIMITS: ResourceGroupLimits = { window_start: null, window_max: null, batch_bytes: null,
  max_result_rows: null, queue_priority: null, max_sessions: null }

/** a limit's text as typed: empty is unnamed (the node's setting); otherwise a whole number, kept as text */
export function limitOf(text: string): string | null | undefined {
  const t = text.trim()
  if (t === '') return null
  return /^-?\d+$/.test(t) ? t : undefined
}

/** what the form lacks before the server composes it (the server and the node judge again) */
export function groupProblem(f: ResourceGroupForm, texts: Record<string, string>): string | undefined {
  if (!f.name.trim()) return 'Name the resource group.'
  for (const l of LIMITS) {
    const v = limitOf(texts[l.key] ?? '')
    if (v === undefined) return `${l.title}: a whole number, or empty for the node's setting.`
    if (v !== null && v.startsWith('-') && l.key !== 'queue_priority') return `${l.title}: 0 or more.`
  }
  return undefined
}

export function ResourceGroupDialog({ name = '', onDone }: { name?: string; onDone: (changed: boolean) => void }) {
  const { api } = useApp()
  const editing = name !== ''
  const [form, setForm] = useState<ResourceGroupForm>({ name, limits: NO_LIMITS, comment: '', default: false, roles: [] })
  const [texts, setTexts] = useState<Record<string, string>>({})
  const [roles, setRoles] = useState<string[]>([])
  const [defaultIs, setDefaultIs] = useState<string>()
  const [nodeGroups, setNodeGroups] = useState<string[]>([])
  const [version, setVersion] = useState<string>()
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)
  const [preview, setPreview] = useState<GrantWritten>()
  const [op, setOp] = useState<'write' | 'drop'>('write')
  const [written, setWritten] = useState<GrantWritten>()
  const [stale, setStale] = useState(false)
  const [reads, setReads] = useState(0)
  useEffect(() => {
    api.resourceGroupRead(name).then((r) => {
      if (editing && !r.exists) {
        setError(`The policy has no resource group ${name} now.`)
        return
      }
      if (editing) {
        setForm(r.group)
        setTexts(Object.fromEntries(LIMITS.map((l) => [l.key, r.group.limits[l.key] ?? ''])))
      }
      setRoles(r.roles)
      setDefaultIs(r.defaultIs)
      setNodeGroups(r.nodeGroups)
      setVersion(editing ? r.version : '')
      setPreview(undefined)
    }, (e) => setError(messageOf(e)))
  }, [api, name, editing, reads])
  const set = (patch: Partial<ResourceGroupForm>) => {
    setForm((f) => ({ ...f, ...patch }))
    setPreview(undefined)
  }
  const problem = groupProblem(form, texts)
  const out: ResourceGroupForm = {
    ...form, limits: Object.fromEntries(LIMITS.map((l) => [l.key, limitOf(texts[l.key] ?? '') ?? null])) as unknown as ResourceGroupLimits,
  }
  const run = async (what: () => Promise<void>) => {
    setBusy(true)
    setError(undefined)
    try {
      await what()
    } catch (e) {
      setStale(e instanceof ApiError && e.code === 'group_changed')
      if (e instanceof ApiError && e.code === 'not_shown') setPreview(undefined)
      setError(messageOf(e))
    } finally {
      setBusy(false)
    }
  }
  const show = (what: 'write' | 'drop') => run(async () => {
    setPreview(await api.resourceGroupPreview({ ...out, op: what }, version ?? ''))
    setOp(what)
  })
  const title = editing ? `The resource group ${name}` : 'A new resource group'
  if (written) {
    return (
      <Dialog open title={title} onClose={() => onDone(true)}>
        <Written written={written} />
        <div className="flex justify-end"><button type="button" className="btn-primary" onClick={() => onDone(true)}>Done</button></div>
      </Dialog>
    )
  }
  const toggle = (role: string) => set({ roles: form.roles.includes(role) ? form.roles.filter((r) => r !== role) : [...form.roles, role] })
  return (
    <Dialog open title={title} onClose={() => onDone(false)}>
      {version === undefined && !error && <p className="text-sm text-ink-muted">Reading the policy on a node…</p>}
      {version !== undefined && (
        <fieldset disabled={busy} className="flex flex-col gap-4">
          {!editing && (
            <div className="flex flex-col gap-2">
              <label htmlFor="rg-name" className={label}>Name</label>
              <input id="rg-name" className={`${field} h-11`} value={form.name} onChange={(e) => set({ name: e.target.value })} spellCheck={false} />
              <p className="text-sm text-ink-muted">Letters, digits, _ and -: a group of nodes may serve it later.</p>
            </div>
          )}
          {nodeGroups.length > 0 && (
            <p className="text-sm">Served by the group{nodeGroups.length > 1 ? 's' : ''} <span className="mono">{nodeGroups.join(', ')}</span>: a change reaches each of their sessions as it opens.</p>
          )}
          <fieldset className="flex flex-col gap-2">
            <legend className={label}>Limits</legend>
            <p className="text-sm text-ink-muted">Empty: the node's setting. On a group's nodes a session runs under that group's limits only - yours too: a low row limit there refuses the console's own reads on them.</p>
            <div className="grid grid-cols-2 gap-4">
              {LIMITS.map((l) => (
                <div key={l.key} className="flex flex-col gap-1">
                  <label htmlFor={`rg-${l.key}`} className="text-sm">{l.title} <span className="mono text-ink-muted">{l.key}</span></label>
                  <input id={`rg-${l.key}`} inputMode="numeric" className={`${field} h-11 font-mono`} value={texts[l.key] ?? ''}
                    onChange={(e) => { setTexts((t) => ({ ...t, [l.key]: e.target.value })); setPreview(undefined) }} />
                  <p className="text-xs text-ink-muted">{l.hint}</p>
                </div>
              ))}
            </div>
          </fieldset>
          <div className="flex flex-col gap-2">
            <label htmlFor="rg-comment" className={label}>Comment</label>
            <input id="rg-comment" className={`${field} h-11`} value={form.comment} onChange={(e) => set({ comment: e.target.value })} />
          </div>
          <label className="flex items-center gap-3 text-sm">
            <input type="checkbox" className="h-5 w-5" checked={form.default} onChange={(e) => set({ default: e.target.checked })} />
            The default group: a principal whose roles are in no group is its member
          </label>
          {form.default && defaultIs && defaultIs !== form.name && (
            <p className="text-sm text-ink-muted">{defaultIs} is the default now: drop its mark first, or the node refuses this one.</p>
          )}
          <fieldset className="flex flex-col gap-2">
            <legend className={label}>Roles in the group</legend>
            <div className="flex flex-wrap gap-x-6 gap-y-2">
              {[...new Set([...roles, ...form.roles])].map((role) => (
                <label key={role} className="flex items-center gap-2 text-sm">
                  <input type="checkbox" className="h-5 w-5" checked={form.roles.includes(role)} onChange={() => toggle(role)} />
                  <span className="mono">{role}</span>
                </label>
              ))}
              {roles.length === 0 && form.roles.length === 0 && <span className="text-sm text-ink-muted">The policy has no role.</span>}
            </div>
          </fieldset>
          {preview && (
            <div className="flex flex-col gap-2">
              <div className={label}>{preview.statements.length ? 'The statements, run in this order' : 'Nothing changes.'}</div>
              {preview.statements.length > 0 && (
                <pre className="overflow-x-auto rounded-md bg-surface-soft p-4 font-mono text-[13px] leading-[22px]"><code>{preview.statements.join(';\n')}</code></pre>
              )}
              {op === 'drop' && <p role="alert" className="text-sm">Dropping the group takes its role bindings too.</p>}
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
        {editing && version && !preview && nodeGroups.length === 0 && !form.default && (
          <button type="button" className="btn-quiet" disabled={busy} onClick={() => show('drop')}>Drop</button>
        )}
        {preview ? (
          <button type="button" className="btn-primary" disabled={busy || preview.statements.length === 0}
            onClick={() => run(async () => setWritten(await api.resourceGroupWrite({ ...out, op }, version ?? '', preview.statements)))}>
            {op === 'drop' ? 'Drop' : 'Run'}
          </button>
        ) : (
          <button type="button" className="btn-primary" disabled={!!problem || busy || version === undefined}
            onClick={() => show('write')}>Show the statements</button>
        )}
      </div>
    </Dialog>
  )
}
