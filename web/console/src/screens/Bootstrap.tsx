// A group's bootstrap (spec 010): what its nodes start with - the operator's document, and the profile's items that
// apply to them as one of its nodes computes them. Seen only: the document's parts are the install's values (the chart
// owns the platform's spec), the profile's are CLUSTER statements.
import { CircleAlert } from 'lucide-react'
import { Problem } from '../components/ui'
import { Rows } from '../components/ViewTable'
import { documentParts } from '../lib/bootstrap'
import { useApp, useLoad } from '../lib/context'
import type { Group } from '../lib/types'

export function BootstrapSection({ group }: { group: Group }) {
  const { api, rights } = useApp()
  const page = useLoad(() => api.groupBootstrap(group.name), [api, group.name])
  // the node's own rule of a group's item over the cluster's: read on one of the group's serving nodes, never redone here
  const serving = group.nodes.find((n) => n.serving && n.ready)
  const inventory = useLoad(() => (serving && rights.effective ? api.inventory(serving.label) : Promise.resolve(undefined)),
    [api, serving?.label, rights.effective])
  const d = page.data?.document
  return (
    <section aria-labelledby="bootstrap-h" className="flex flex-col gap-4">
      <div>
        <h2 id="bootstrap-h" className="text-[22px] font-semibold leading-[30px]">Bootstrap</h2>
        <p className="text-sm text-ink-muted">What a node of the group starts with: the document the operator hands it at its entry, then the cluster's profile. Its parts change by the install's values, the profile by <span className="mono">ACL CLUSTER</span> statements.</p>
      </div>
      {page.error !== undefined && <Problem error={page.error} />}
      {page.data && !page.data.served && (
        <div role="alert" className="flex items-start gap-4 rounded-md border border-line px-6 py-4">
          <CircleAlert size={20} aria-hidden="true" className="mt-0.5 flex-none" />
          <div>
            <div className="font-semibold">The operator refuses this document: a node of the group does not start</div>
            <div className="break-words text-sm text-ink-muted">{page.data.refused}</div>
          </div>
        </div>
      )}
      {d && (
        <div className="grid grid-cols-[repeat(auto-fit,minmax(320px,1fr))] gap-6">
          {documentParts(d).map((part) => (
            <dl key={part.title} aria-label={part.title} className="panel flex flex-col gap-3 p-6 text-sm">
              <div>
                <div className="font-semibold">{part.title}</div>
                {part.note && <div className="text-ink-muted">{part.note}</div>}
              </div>
              {part.rows.map(([label, value]) => (
                <div key={label}><dt className="eyebrow">{label}</dt><dd className="mono break-all">{value || '-'}</dd></div>
              ))}
            </dl>
          ))}
        </div>
      )}
      {d && (
        <details className="text-sm">
          <summary className="cursor-pointer">The document as a node receives it</summary>
          <pre className="mono mt-2 overflow-x-auto rounded-md border border-line p-4">{JSON.stringify(d, null, 2)}</pre>
          <p className="text-ink-muted">Each node gets its own label in place of <span className="mono">&lt;pod&gt;</span>. Names and addresses only: the document never holds a credential.</p>
        </details>
      )}
      <div>
        <h3 className="text-lg font-semibold">The profile its nodes apply</h3>
        <p className="text-sm text-ink-muted">Version {group.profile.target}, applied by {group.profile.applied} of {group.replicas}. The items below are as {serving ? <span className="mono">{serving.label}</span> : 'a serving node'} computes them: the group's own over the cluster's.</p>
      </div>
      {!rights.effective ? (
        <p className="text-sm text-ink-muted">The profile's items are the cluster bundle's to read.</p>
      ) : !serving ? (
        <p className="text-sm text-ink-muted">No node of the group serves now: the items are read on one that does.</p>
      ) : inventory.error !== undefined ? (
        <Problem error={inventory.error} />
      ) : inventory.data?.effective.error ? (
        <p className="text-sm text-ink-muted">{inventory.data.effective.error}</p>
      ) : inventory.data ? (
        <Rows page={{ columns: ['scope', 'kind', 'name', 'class', 'version'], rows: inventory.data.effective.rows,
          total: inventory.data.effective.rows.length }} />
      ) : null}
    </section>
  )
}
