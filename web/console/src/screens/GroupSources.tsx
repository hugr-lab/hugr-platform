// A group's sources (spec 007 stage 2b, hugr-node spec 035): the set its nodes attach, with its lazy marks, and the
// check - what the group's roles reach that the set leaves out, with the repair the node writes. Nothing changes the set
// by itself (the owner, 2026-11-20): a repair runs when the person asks, its text the node's.
import { useState } from 'react'
import { Dialog, Problem, Status, messageOf } from '../components/ui'
import { useApp, useLoad } from '../lib/context'
import { changeOf, changeProblem, lazyText, reachOf } from '../lib/profile'
import type { Group, GroupSource, GroupSources, ProfileWritten } from '../lib/types'

const field = 'h-11 w-full rounded-sm border border-ink-muted bg-surface px-3 text-base text-ink'
const note = 'rounded-md border border-line px-6 py-4 text-sm'

/** the set as a person reads it */
export function setText(page: GroupSources): string {
  const set = page.set
  if (!set) return 'No set: the nodes attach every source of the cluster and of the group.'
  if (set.all) return `Every source, ${lazyText(true, set.idleSeconds)}.`
  return `${set.names.join(', ')}: ${lazyText(set.lazy, set.idleSeconds)}.`
}

/** a row's place in the set: listed, left out (with its repair), or no set at all */
function placeOf(row: GroupSource) {
  if (row.listed === null) return <span className="text-ink-muted">no set</span>
  return row.listed ? <Status mark="ok">listed</Status> : <Status mark="off">left out</Status>
}

export function SourcesSection({ group, read }: { group: Group; read?: number }) {
  const { api, rights } = useApp()
  const offered = rights.functions.includes('group_sources') && rights.deployment !== 'standalone'
  const page = useLoad(() => (offered ? api.groupSources(group.name) : Promise.resolve(undefined)),
    [api, group.name, offered, read])
  const [editing, setEditing] = useState(false)
  const [repairing, setRepairing] = useState<GroupSource>()
  if (!offered) return null
  const done = (changed: boolean) => {
    setEditing(false)
    setRepairing(undefined)
    if (changed) page.reload()
  }
  const missing = page.data?.rows.filter((r) => r.listed === false) ?? []
  return (
    <section aria-labelledby="sources-h" className="flex flex-col gap-4">
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div>
          <h2 id="sources-h" className="text-[22px] font-semibold leading-[30px]">Sources</h2>
          <p className="text-sm text-ink-muted">The sources the group's nodes attach, and what its roles read that the set leaves out. Nothing changes the set by itself: a grant's answer names what it leaves out, and a repair runs when you ask for it.</p>
        </div>
        {rights.infrastructure && page.data && (
          <button type="button" className="btn-outline" onClick={() => setEditing(true)}>Change the set</button>
        )}
      </div>
      {page.error !== undefined && <Problem error={page.error} />}
      {page.loading && !page.data && <p className="text-sm text-ink-muted">Reading on a node…</p>}
      {page.data && (
        <>
          <p role="note" className={note}>{setText(page.data)}</p>
          {missing.length > 0 && (
            <p role="status" className="text-sm">
              {missing.length === 1 ? `${missing[0].source} is read by the group's roles and left out of its set.`
                : `${missing.map((r) => r.source).join(', ')} are read by the group's roles and left out of its set.`}
            </p>
          )}
          <div className="overflow-x-auto rounded-md border border-line">
            <table className="w-full border-collapse text-sm">
              <thead><tr>{['Source', 'Read by', 'In the set', ''].map((c, i) => <th key={i} className="th">{c}</th>)}</tr></thead>
              <tbody>
                {page.data.rows.map((row) => (
                  <tr key={row.source}>
                    <td className="td mono break-all">{row.source}</td>
                    <td className="td">{row.roles.length ? row.roles.join(', ') : <span className="text-ink-muted">no role of the group</span>}</td>
                    <td className="td whitespace-nowrap">{placeOf(row)}</td>
                    <td className="td text-right">
                      {row.repair && rights.infrastructure && (
                        <button type="button" className="btn-quiet" onClick={() => setRepairing(row)}>Repair</button>
                      )}
                    </td>
                  </tr>
                ))}
                {page.data.rows.length === 0 && (
                  <tr><td className="td text-ink-muted" colSpan={4}>The group's roles read no source, and the set lists none.</td></tr>
                )}
              </tbody>
            </table>
          </div>
        </>
      )}
      {editing && page.data && <SetDialog group={group} page={page.data} onDone={done} />}
      {repairing && <RepairDialog group={group} row={repairing} onDone={done} />}
    </section>
  )
}

/** what the node did, as the profile's dialog says it */
function Written({ written, onDone }: { written: ProfileWritten; onDone: () => void }) {
  return (
    <>
      <p role="status">{reachOf(written)}</p>
      {written.message && <p className="text-sm text-ink-muted">{written.ok ? written.message : `Not done: ${written.message}`}</p>}
      <div className="flex justify-end">
        <button type="button" className="btn-primary" onClick={onDone}>Done</button>
      </div>
    </>
  )
}

/** a repair is the node's statement: shown before it runs, read again by the console where it is written */
export function RepairDialog({ group, row, onDone }: { group: Group; row: GroupSource; onDone: (changed: boolean) => void }) {
  const { api } = useApp()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  const [written, setWritten] = useState<ProfileWritten>()
  const run = async () => {
    setBusy(true)
    setError(undefined)
    try {
      setWritten(await api.repairGroupSource(group.name, row.source))
    } catch (e) {
      setError(messageOf(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <Dialog open title={`Repair ${row.source}`} onClose={() => onDone(!!written)}>
      {written ? <Written written={written} onDone={() => onDone(true)} /> : (
        <>
          <p className="text-ink-muted">The node's repair: what {group.resourceGroup}'s set leaves out, written as it suggests. The console reads it again on the node that writes it.</p>
          <pre className="overflow-x-auto rounded-md bg-surface-soft p-4 font-mono text-[13px] leading-[22px]"><code>{row.repair}</code></pre>
          {error && <p role="alert" className="rounded-md bg-surface-soft px-4 py-3 text-sm font-medium">{error}</p>}
          <div className="flex flex-wrap justify-end gap-2">
            <button type="button" className="btn-outline" disabled={busy} onClick={() => onDone(false)}>Cancel</button>
            <button type="button" className="btn-primary" disabled={busy} onClick={run}>Run</button>
          </div>
        </>
      )}
    </Dialog>
  )
}

type Mode = 'every' | 'list' | 'all'

/** the set: every source (no set), the sources listed, or every source lazy - with the lazy marks */
export function SetDialog({ group, page, onDone }: { group: Group; page: GroupSources; onDone: (changed: boolean) => void }) {
  const { api } = useApp()
  const set = page.set
  const [mode, setMode] = useState<Mode>(!set ? 'every' : set.all ? 'all' : 'list')
  const [picked, setPicked] = useState<string[]>(set?.names ?? [])
  const [lazy, setLazy] = useState(set?.lazy ?? false)
  const [idle, setIdle] = useState(!set || set.idleSeconds <= 0 ? ''
    : set.idleSeconds % 60 === 0 ? `${set.idleSeconds / 60} min` : `${set.idleSeconds} seconds`)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  const [written, setWritten] = useState<ProfileWritten>()
  // the sources to pick from: the profile's for the group, those its roles read, those its set lists
  const known = [...new Set([...page.profile, ...page.rows.map((r) => r.source), ...(set?.names ?? [])])].sort()
  const change = mode === 'every' ? changeOf('reset-sources', {}, group.resourceGroup)
    : changeOf('sources', { all: mode === 'all', sources: picked, lazy: mode === 'all' || lazy, idle: (mode === 'all' || lazy) ? idle.trim() : '' }, group.resourceGroup)
  const problem = mode === 'every' && !set ? 'The group has no set: its nodes attach every source already.' : changeProblem(change)
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
  const radio = (value: Mode, title: string, hint: string) => (
    <label className="flex items-start gap-3 text-sm">
      <input type="radio" name="set-mode" className="mt-0.5 h-5 w-5" checked={mode === value} onChange={() => setMode(value)} />
      <span><span className="font-semibold">{title}</span><br /><span className="text-ink-muted">{hint}</span></span>
    </label>
  )
  return (
    <Dialog open title={`The sources of ${group.resourceGroup}`} onClose={() => onDone(!!written)}>
      {written ? <Written written={written} onDone={() => onDone(true)} /> : (
        <>
          <fieldset className="flex flex-col gap-3">
            <legend className="sr-only">The set</legend>
            {radio('every', 'Every source', 'No set: the nodes attach every source of the cluster and of the group.')}
            {radio('list', 'These sources', 'The nodes attach only the sources listed; a node of the group writes no policy.')}
            {radio('all', 'Every source, lazy', 'Nothing to keep listed: each source is attached on its first read.')}
          </fieldset>
          {mode === 'list' && (
            <div role="group" aria-label="Sources" className="flex flex-col gap-2">
              {known.map((name) => (
                <label key={name} className="flex items-center gap-3 text-sm">
                  <input type="checkbox" className="h-5 w-5" checked={picked.includes(name)}
                    onChange={(e) => setPicked((p) => e.target.checked ? [...p, name] : p.filter((x) => x !== name))} />
                  <span className="mono">{name}</span>
                </label>
              ))}
              {known.length === 0 && <p className="text-sm text-ink-muted">The profile attaches no source for the group yet.</p>}
              <label className="mt-2 flex items-center gap-3 text-sm">
                <input type="checkbox" className="h-5 w-5" checked={lazy} onChange={(e) => setLazy(e.target.checked)} />
                Lazy: each attached on its first read
              </label>
            </div>
          )}
          {(mode === 'all' || (mode === 'list' && lazy)) && (
            <div className="flex flex-col gap-2">
              <label htmlFor="set-idle" className="text-sm font-semibold">Idle time</label>
              <input id="set-idle" className={field} value={idle} onChange={(e) => setIdle(e.target.value)} spellCheck={false} />
              <p className="text-sm text-ink-muted">Detached after this long unused, such as 30 min; empty keeps each source's own. A source in a DEPENDS ON chain is attached at once.</p>
            </div>
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
