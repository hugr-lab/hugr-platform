// A change of the cluster's profile from a form (spec 010 stage 2): one CLUSTER statement, for the whole cluster or one
// resource group's nodes. The console's server builds the statement from the fields; a node judges it (the cluster
// bundle) and says how the change reaches the nodes.
import { useState } from 'react'
import { Dialog, messageOf } from '../components/ui'
import { useApp } from '../lib/context'
import { actions, changeOf, changeProblem, reachOf, type Action } from '../lib/profile'
import type { ProfileChange, ProfileWritten } from '../lib/types'

const field = 'h-11 w-full rounded-sm border border-ink-muted bg-surface px-3 text-base text-ink'
const label = 'text-sm font-semibold'

function Text({ id, title, value, onChange, hint }: { id: string; title: string; value: string;
  onChange: (v: string) => void; hint?: string }) {
  return (
    <div className="flex flex-col gap-2">
      <label htmlFor={id} className={label}>{title}</label>
      <input id={id} className={field} value={value} onChange={(e) => onChange(e.target.value)} spellCheck={false} />
      {hint && <p className="text-sm text-ink-muted">{hint}</p>}
    </div>
  )
}

function Check({ title, value, onChange }: { title: string; value: boolean; onChange: (v: boolean) => void }) {
  return (
    <label className="flex items-center gap-3 text-sm">
      <input type="checkbox" checked={value} onChange={(e) => onChange(e.target.checked)} className="h-5 w-5" />
      {title}
    </label>
  )
}

export function ProfileDialog({ group, onDone }: { group?: string; onDone: (changed: boolean) => void }) {
  const { api } = useApp()
  const [action, setAction] = useState<Action>('attach')
  const [f, setF] = useState<Partial<ProfileChange>>({})
  const [depends, setDepends] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  const [written, setWritten] = useState<ProfileWritten>()
  const set = (k: keyof ProfileChange) => (v: string | boolean) => setF((x) => ({ ...x, [k]: v }))
  const s = (k: keyof ProfileChange) => String(f[k] ?? '')
  const b = (k: keyof ProfileChange) => f[k] === true
  const change = changeOf(action, { ...f, dependsOn: depends.split(',').map((x) => x.trim()).filter(Boolean) }, group)
  const problem = changeProblem(change)
  const send = async () => {
    setBusy(true)
    setError(undefined)
    try {
      setWritten(await api.changeProfile(change))
    } catch (e) {
      setError(messageOf(e))
    } finally {
      setBusy(false)
    }
  }
  const scope = group ? `the nodes of ${group}` : 'the whole cluster'
  return (
    <Dialog open title={group ? `Change the profile for ${group}` : 'Change the profile'} onClose={() => onDone(!!written)}>
      {written ? (
        <>
          <p role="status">{reachOf(written)}</p>
          {written.note && <p className="text-sm text-ink-muted">{written.note}</p>}
          <div className="flex justify-end">
            <button type="button" className="btn-primary" onClick={() => onDone(true)}>Done</button>
          </div>
        </>
      ) : (
        <>
          <p className="text-ink-muted">One change for {scope}, written as a <span className="mono">CLUSTER</span> statement on a node, which judges it. A source names a secret in tresor, never a credential.</p>
          <div className="flex flex-col gap-2">
            <label htmlFor="pc-action" className={label}>Change</label>
            <select id="pc-action" className={field} value={action} onChange={(e) => setAction(e.target.value as Action)}>
              {actions.map((a) => <option key={a.value} value={a.value}>{a.label}</option>)}
            </select>
          </div>
          {(action === 'install' || action === 'update' || action === 'remove') && (
            <div className="grid grid-cols-2 gap-4">
              <Text id="pc-ext" title="Extension" value={s('extension')} onChange={set('extension')} />
              {action !== 'remove' && <Text id="pc-version" title="Version" value={s('version')} onChange={set('version')} />}
              {action === 'install' && <Text id="pc-repo" title="Repository" value={s('repository')} onChange={set('repository')}
                hint="Its name; with one repository it may stay empty." />}
            </div>
          )}
          {action === 'attach' && (
            <div className="grid grid-cols-2 gap-4">
              <Text id="pc-path" title="Path" value={s('path')} onChange={set('path')} />
              <Text id="pc-alias" title="Alias" value={s('alias')} onChange={set('alias')} />
              <Text id="pc-type" title="Type" value={s('type')} onChange={set('type')} hint="duckdb, postgres, mysql, mssql, hugr ..." />
              <Text id="pc-secret" title="Secret" value={s('secret')} onChange={set('secret')} hint="A database's login: its secret in tresor." />
              <Text id="pc-lineage" title="Lineage name" value={s('lineage')} onChange={set('lineage')} hint="scheme://host:port[/database]" />
              <Text id="pc-depends" title="Depends on" value={depends} onChange={setDepends} hint="Aliases, comma separated." />
              <Check title="Read only" value={b('readOnly')} onChange={set('readOnly')} />
            </div>
          )}
          {action === 'detach' && (
            <div className="flex flex-col gap-4">
              <Text id="pc-alias" title="Alias" value={s('alias')} onChange={set('alias')} />
              <Check title="Cascade: its dependents too" value={b('cascade')} onChange={set('cascade')} />
              <Check title="Force: even while the policy reads through it" value={b('force')} onChange={set('force')} />
            </div>
          )}
          {(action === 'set' || action === 'reset') && (
            <div className="grid grid-cols-2 gap-4">
              <Text id="pc-setting" title="Setting" value={s('setting')} onChange={set('setting')} />
              {action === 'set' && <Text id="pc-value" title="Value" value={s('value')} onChange={set('value')} />}
            </div>
          )}
          {(action === 'install' || action === 'attach') && (
            <Text id="pc-comment" title="Comment" value={s('comment')} onChange={set('comment')} />
          )}
          {problem && <p className="text-sm text-ink-muted">{problem}</p>}
          {error && <p role="alert" className="rounded-md bg-surface-soft px-4 py-3 text-sm font-medium">{error}</p>}
          <div className="flex flex-wrap justify-end gap-2">
            <button type="button" className="btn-outline" disabled={busy} onClick={() => onDone(false)}>Cancel</button>
            <button type="button" className="btn-primary" disabled={!!problem || busy} onClick={send}>Write</button>
          </div>
        </>
      )}
    </Dialog>
  )
}
