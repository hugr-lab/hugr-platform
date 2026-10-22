// A group's bootstrap (spec 010): what its nodes start with - the operator's document, and the profile's items that
// apply to them as one of its nodes computes them. Seen only: the document's parts are the install's values (the chart
// owns the platform's spec), the profile's are CLUSTER statements.
import { useState } from 'react'
import { CircleAlert } from 'lucide-react'
import { Problem } from '../components/ui'
import { behind, documentParts, documentTitle } from '../lib/bootstrap'
import { useApp, useLoad, type Loaded } from '../lib/context'
import { sentence } from '../lib/format'
import type { BootstrapPage, Group } from '../lib/types'
import { ProfileDialog } from './ProfileDialog'

/** the operator refusing the group's document: a node of the group does not start - the group page's first alert */
export function BootstrapRefused({ page }: { page?: BootstrapPage }) {
  if (!page || page.served) return null
  return (
    <div role="alert" className="flex items-start gap-4 rounded-md border border-line px-6 py-4">
      <CircleAlert size={20} aria-hidden="true" className="mt-0.5 flex-none" />
      <div>
        <div className="font-semibold">The operator refuses this group's bootstrap document: a node of the group does not start</div>
        <div className="break-words text-sm text-ink-muted">{page.refused}</div>
      </div>
    </div>
  )
}

const note = 'rounded-md border border-line px-6 py-4 text-sm'

export function BootstrapSection({ group, page, read, onChanged }: { group: Group; page: Loaded<BootstrapPage>;
  read?: number; onChanged: () => void }) {
  const { api, rights } = useApp()
  const [changing, setChanging] = useState(false)
  // the node's own rule of a group's item over the cluster's: read on one of the group's serving nodes, never redone here
  const serving = group.nodes.find((n) => n.serving && n.ready)
  const standalone = rights.deployment === 'standalone'
  const inventory = useLoad(
    () => (serving && rights.effective && !standalone ? api.inventory(serving.label) : Promise.resolve(undefined)),
    [api, serving?.label, rights.effective, standalone, read])
  const d = page.data?.document
  const late = behind(group.nodes, group.profile.target)
  return (
    <section aria-labelledby="bootstrap-h" className="flex flex-col gap-4">
      <div>
        <h2 id="bootstrap-h" className="text-[22px] font-semibold leading-[30px]">Bootstrap</h2>
        <p className="text-sm text-ink-muted">What a node of the group starts with: the document the operator hands it at its entry, then the cluster's profile. The document's parts change by the install's values, the profile by <span className="mono">ACL CLUSTER</span> statements.</p>
      </div>
      {page.error !== undefined && <Problem error={page.error} />}
      {d && (
        <div className="grid grid-cols-[repeat(auto-fit,minmax(320px,1fr))] gap-6">
          {documentParts(d).map((part) => (
            <div key={part.title} role="group" aria-label={part.title} className="panel flex flex-col gap-3 p-8 text-sm">
              <div>
                <div className="font-semibold">{part.title}</div>
                {part.note && <div className="text-ink-muted">{part.note}</div>}
              </div>
              <dl className="flex flex-col gap-3">
                {part.rows.map(([label, value]) => (
                  <div key={label}><dt className="eyebrow">{label}</dt><dd className="mono break-all">{value || '-'}</dd></div>
                ))}
              </dl>
            </div>
          ))}
        </div>
      )}
      {d && page.data && (
        <details className="text-sm">
          <summary className="cursor-pointer">{documentTitle(page.data)}</summary>
          <pre className="mt-2 overflow-x-auto rounded-md bg-surface-soft p-6 font-mono text-[13px] leading-[22px]"><code>{JSON.stringify(d, null, 2)}</code></pre>
          <p className="mt-2 text-ink-muted">Each node gets its own label in place of <span className="mono">&lt;pod&gt;</span>. Names and addresses only: the document never holds a credential.</p>
        </details>
      )}
      <div className="flex flex-wrap items-end justify-between gap-4">
        <h3 className="text-lg font-semibold">The profile its nodes apply</h3>
        {rights.infrastructure && !standalone && (
          <button type="button" className="btn-outline" onClick={() => setChanging(true)}>Change for {group.resourceGroup}</button>
        )}
      </div>
      {changing && <ProfileDialog group={group.resourceGroup}
        onDone={(changed) => { setChanging(false); if (changed) { onChanged(); inventory.reload() } }} />}
      <div>
        <p className="text-sm text-ink-muted">
          Version {group.profile.target}, applied by {group.profile.applied} of {group.replicas}{late.length > 0 ? `; behind: ${late.join(', ')}` : ''}.
          {serving && rights.effective && !standalone && <> The items below are as <span className="mono">{serving.label}</span> computes them: the group's own over the cluster's.</>}
        </p>
      </div>
      {standalone ? (
        <p role="note" className={note}>A standalone node keeps no cluster profile: its sources and settings are not read against one.</p>
      ) : !rights.effective ? (
        <p role="note" className={note}>Your rights do not read the profile's items (the cluster bundle reads them).</p>
      ) : !serving ? (
        <p role="note" className={note}>No node of the group serves now: the items are read on one that does.</p>
      ) : inventory.error !== undefined ? (
        <Problem error={inventory.error} />
      ) : inventory.data?.effective.error ? (
        <p role="note" className={note}>{sentence(inventory.data.effective.error)}</p>
      ) : inventory.data ? (
        <div className="overflow-x-auto rounded-md border border-line">
          <table className="w-full border-collapse text-sm">
            <thead><tr>{['Scope', 'Kind', 'Name', 'Class', 'Version'].map((c) => <th key={c} className="th">{c}</th>)}</tr></thead>
            <tbody>
              {inventory.data.effective.rows.map((row, i) => (
                <tr key={i}>
                  {['scope', 'kind', 'name', 'class', 'version'].map((c) => (
                    <td key={c} className={`td ${c === 'name' ? 'mono break-all' : 'whitespace-nowrap'}`}>{row[c] === null || row[c] === undefined ? '-' : String(row[c])}</td>
                  ))}
                </tr>
              ))}
              {inventory.data.effective.rows.length === 0 && <tr><td className="td text-ink-muted" colSpan={5}>None.</td></tr>}
            </tbody>
          </table>
        </div>
      ) : null}
    </section>
  )
}
