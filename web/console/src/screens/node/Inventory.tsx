import { useParams } from 'react-router-dom'
import { sentence } from '../../lib/format'
import { Problem, Status, type Mark } from '../../components/ui'
import { useApp, useLoad } from '../../lib/context'
import type { Part } from '../../lib/types'
import { NodeFrame } from './Frame'

/** drift's state as a circle: same full, a difference half, the bootstrap's own a ring */
function stateMark(state: string): Mark {
  if (state === 'same') return 'ok'
  if (state === 'bootstrap') return 'off'
  return 'half'
}

const stateText: Record<string, string> = { same: 'same', differs: 'differs', node_only: 'this node only',
  profile_only: 'profile only', bootstrap: 'bootstrap' }

function Table({ id, title, lead, part, columns }: { id: string; title: string; lead?: string; part?: Part;
  columns: { key: string; label: string; mono?: boolean; wrap?: boolean; state?: boolean }[] }) {
  return (
    <section aria-labelledby={id} className="flex flex-col gap-4">
      <div>
        <h2 id={id} className="text-[22px] font-semibold leading-[30px]">{title}</h2>
        {lead && <p className="text-sm text-ink-muted">{lead}</p>}
      </div>
      {part?.error ? (
        <p role="note" className="rounded-md border border-line px-6 py-4 text-sm">{part.error}</p>
      ) : (
        <div className="overflow-x-auto rounded-md border border-line">
          <table className="w-full min-w-[880px] border-collapse">
            <thead><tr>{columns.map((c) => <th key={c.key} className="th">{c.label}</th>)}</tr></thead>
            <tbody>
              {(part?.rows ?? []).map((row, i) => (
                <tr key={i}>
                  {columns.map((c) => {
                    const v = row[c.key]
                    const text = v === null || v === undefined || v === '' ? '-' : typeof v === 'boolean' ? (v ? 'yes' : 'no')
                      : typeof v === 'object' ? JSON.stringify(v) : String(v)
                    return (
                      <td key={c.key} className={`td ${c.mono ? 'mono' : ''} ${c.wrap ? 'break-all' : 'whitespace-nowrap'}`}>
                        {c.state ? <Status mark={stateMark(text)}>{stateText[text] ?? text}</Status> : text}
                      </td>
                    )
                  })}
                </tr>
              ))}
              {part && part.rows.length === 0 && (
                <tr><td className="td text-ink-muted" colSpan={columns.length}>None.</td></tr>
              )}
            </tbody>
          </table>
        </div>
      )}
    </section>
  )
}

export function Inventory() {
  const { label = '' } = useParams()
  const { api } = useApp()
  const inv = useLoad(() => api.inventory(label), [api, label])
  const drift = inv.data?.drift
  const of = (kind: string): Part | undefined =>
    drift && { rows: drift.rows.filter((r) => r.kind === kind), error: drift.error }
  return (
    <NodeFrame label={label} alsoReload={inv.reload}>
      {() => (
        <>
          {inv.error !== undefined && <Problem error={inv.error} />}
          {drift?.error && (
            <p role="note" className="rounded-md border border-line px-6 py-4 text-sm">
              {sentence(drift.error)} The extensions are below.
            </p>
          )}
          {!drift?.error && <Table id="src-h" title="Sources"
            lead="The databases this node attached, against the profile. Never an ATTACH string: a source's credentials are in tresor."
            part={of('database')}
            columns={[{ key: 'name', label: 'Name', mono: true }, { key: 'scope', label: 'Scope' },
              { key: 'state', label: 'Against the profile', state: true }, { key: 'node_value', label: 'This node', mono: true },
              { key: 'profile_value', label: 'The profile', mono: true, wrap: true }]} />}
          <Table id="ext-h" title="Extensions" part={inv.data?.extensions}
            columns={[{ key: 'extension_name', label: 'Name', mono: true }, { key: 'extension_version', label: 'Version', mono: true },
              { key: 'loaded', label: 'Loaded' }, { key: 'install_mode', label: 'From' }, { key: 'installed_from', label: 'Repository', mono: true, wrap: true }]} />
          {!drift?.error && <><Table id="drift-ext-h" title="Extensions against the profile" part={of('extension')}
            columns={[{ key: 'name', label: 'Name', mono: true }, { key: 'scope', label: 'Scope' },
              { key: 'state', label: 'State', state: true }, { key: 'node_value', label: 'This node', mono: true, wrap: true },
              { key: 'profile_value', label: 'The profile', mono: true, wrap: true }]} />
          <Table id="set-h" title="Settings"
            lead="The settings the profile or the bootstrap sets, this node's value beside the profile's. A value never goes above the pod."
            part={of('setting')}
            columns={[{ key: 'name', label: 'Setting', mono: true }, { key: 'node_value', label: 'This node', mono: true },
              { key: 'profile_value', label: 'The profile', mono: true }, { key: 'state', label: 'State', state: true }]} />
          <Table id="eff-h" title="The profile's items for this node" part={inv.data?.effective}
            columns={[{ key: 'scope', label: 'Scope' }, { key: 'kind', label: 'Kind' }, { key: 'name', label: 'Name', mono: true },
              { key: 'class', label: 'Class' }, { key: 'version', label: 'Version' }]} /></>}
        </>
      )}
    </NodeFrame>
  )
}
