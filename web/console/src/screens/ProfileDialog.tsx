// A change of the cluster's profile from a form (spec 010 stage 2): one CLUSTER statement, for the whole cluster or one
// resource group's nodes. The console's server builds the statement from the fields; a node judges it (the cluster
// bundle) and says how the change reaches the nodes.
import { useState } from 'react'
import { Dialog, messageOf } from '../components/ui'
import { useApp } from '../lib/context'
import { actions, changeOf, changeProblem, places, policiesOf, reachOf, type Action } from '../lib/profile'
import type { Place, Policy, ProfileChange, ProfileWritten } from '../lib/types'

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

/** a boot script's SQL: the one field over lines - stored and run as written, never read by the console */
function Script({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  return (
    <div className="flex flex-col gap-2">
      <label htmlFor="pc-sql" className={label}>SQL</label>
      <textarea id="pc-sql" rows={8} className="w-full rounded-sm border border-ink-muted bg-surface px-3 py-2 font-mono text-[13px] leading-[22px] text-ink"
        value={value} onChange={(e) => onChange(e.target.value)} spellCheck={false} />
      <p className="text-sm text-ink-muted">Run as the node at its boot, outside the ACL, as written: nothing checks it. Whoever holds the cluster bundle reads it - name a secret in tresor, never a credential.</p>
    </div>
  )
}

/** the form opened on an item: a script's row opens its change or its drop */
export interface ProfileStart {
  action: Action
  fields?: Partial<ProfileChange>
}

export function ProfileDialog({ group, start, onDone }: { group?: string; start?: ProfileStart;
  onDone: (changed: boolean) => void }) {
  const { api } = useApp()
  const [action, setAction] = useState<Action>(start?.action ?? 'attach')
  const [f, setF] = useState<Partial<ProfileChange>>(start?.fields ?? {})
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
          {written.message && <p className="text-sm text-ink-muted">{written.ok ? written.message : `Not done: ${written.message}`}</p>}
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
              <Check title="Lazy: attached on its first read, not at the node's start" value={b('lazy')}
                onChange={(v) => setF((x) => ({ ...x, lazy: v, idle: v ? x.idle : undefined }))} />
              {b('lazy') && <Text id="pc-idle" title="Idle time" value={s('idle')} onChange={set('idle')}
                hint="Detached after this long unused, such as 30 min; empty keeps it until the node stops." />}
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
          {(action === 'add-script' || action === 'alter-script' || action === 'drop-script') && (
            <div className="flex flex-col gap-4">
              <div className="grid grid-cols-2 gap-4">
                <Text id="pc-script" title="Name" value={s('script')} onChange={set('script')}
                  hint="Letters, digits, _: a place's scripts run in their names' order (10_vars before 20_views)." />
                {action !== 'drop-script' && (
                  <div className="flex flex-col gap-2">
                    <label htmlFor="pc-place" className={label}>Runs</label>
                    <select id="pc-place" className={field} value={s('place')}
                      onChange={(e) => setF((x) => ({ ...x, place: (e.target.value || undefined) as Place | undefined, onError: undefined }))}>
                      <option value="">{action === 'add-script' ? 'Pick where' : 'Where it runs now'}</option>
                      {places.map((p) => <option key={p.value} value={p.value}>{p.label}</option>)}
                    </select>
                  </div>
                )}
                {action !== 'drop-script' && s('place') === 'after_source' && (
                  <Text id="pc-source" title="Source" value={s('source')} onChange={set('source')}
                    hint="Its alias: the script runs at each attach of it." />
                )}
                {action !== 'drop-script' && (
                  <div className="flex flex-col gap-2">
                    <label htmlFor="pc-on-error" className={label}>On error</label>
                    <select id="pc-on-error" className={field} value={s('onError')}
                      onChange={(e) => setF((x) => ({ ...x, onError: (e.target.value || undefined) as Policy | undefined }))}>
                      <option value="">{action === 'alter-script' && !s('place') ? 'As it is now' : `The place's default${f.place ? `: ${policiesOf(f.place)[0].label.split(':')[0].toLowerCase()}` : ''}`}</option>
                      {policiesOf(f.place).map((p) => <option key={p.value} value={p.value}>{p.label}</option>)}
                    </select>
                  </div>
                )}
              </div>
              {action !== 'drop-script' && <Script value={s('sql')} onChange={set('sql')} />}
              {action === 'alter-script' && <p className="text-sm text-ink-muted">What you leave empty stays as it is.</p>}
              {action === 'drop-script' && <p className="text-sm text-ink-muted">The script goes from the profile; a node that ran it keeps what it made until its restart.</p>}
            </div>
          )}
          {(action === 'install' || action === 'attach' || action === 'add-script' || action === 'alter-script') && (
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
