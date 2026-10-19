import { CircleAlert } from 'lucide-react'
import { Link, useParams } from 'react-router-dom'
import { Chip, Crumbs, Header, Problem, Refresh, Stat, Status, age } from '../components/ui'
import { useApp, useLoad } from '../lib/context'
import type { NodeInfo } from '../lib/types'
import { groupState } from './Fleet'

export function nodeState(n: NodeInfo): { mark: 'ok' | 'half' | 'off'; text: string } {
  if (!n.serving) return { mark: 'off', text: 'Gone' }
  if (!n.ready) return { mark: 'off', text: 'Not ready' }
  if (n.leaseAge !== null && !n.current) return { mark: 'half', text: 'Lease expired' }
  if (n.draining) return { mark: 'half', text: 'Draining' }
  return { mark: 'ok', text: 'Ready' }
}

export function Group() {
  const { name = '' } = useParams()
  const { api } = useApp()
  const fleet = useLoad(() => api.fleet(), [api])
  const group = fleet.data?.groups.find((g) => g.name === name)
  return (
    <>
      <Crumbs items={[{ label: 'Fleet', to: '/' }, { label: name }]} />
      <Header title={name} lead={group && `Size ${group.size}, ${group.replicas} ${group.replicas === 1 ? 'node' : 'nodes'} wanted. Each node's row comes from its Lease, which it renews every few seconds.`}
        actions={<Refresh at={fleet.at} loading={fleet.loading} onClick={fleet.reload} />} />
      {fleet.error !== undefined && <Problem error={fleet.error} />}
      {fleet.data && !group && <Problem error={new Error('no such group')} />}
      {group && (
        <>
          <section aria-label="The group" className="panel flex flex-wrap gap-x-16 gap-y-4 px-8">
            <Stat label="Nodes ready" value={`${group.readyReplicas} of ${group.replicas}`} />
            <Stat label="Resource group" value={<Chip>{group.resourceGroup}</Chip>}
              sub={group.resourceGroupKnown === false ? 'not in the policy' : group.resourceGroupKnown ? 'in the policy' : 'no node reported yet'} />
            <Stat label="Profile" value={`version ${group.profile.target}`} sub={`${group.profile.applied} of ${group.replicas} applied`} />
            <Stat label="Sessions" value={group.nodes.reduce((n, x) => n + x.sessions, 0)} />
          </section>
          {group.profile.needsRestart.length > 0 && (
            <div role="status" className="flex items-start gap-4 rounded-md border border-line px-6 py-4">
              <CircleAlert size={20} aria-hidden="true" className="mt-0.5 flex-none" />
              <div>
                <div className="font-semibold">{groupState(group).text}</div>
                <div className="text-sm text-ink-muted">
                  <span className="mono">{group.profile.needsRestart.join(', ')}</span> holds a profile item it can apply only at a restart. Restarting the group from the fleet replaces its nodes one at a time.
                </div>
              </div>
            </div>
          )}
          <section aria-labelledby="nodes-h" className="flex flex-col gap-4">
            <h2 id="nodes-h" className="text-[22px] font-semibold leading-[30px]">Nodes</h2>
            <div className="overflow-x-auto rounded-md border border-line">
              <table className="w-full min-w-[960px] border-collapse">
                <thead>
                  <tr>
                    <th className="th">Node</th>
                    <th className="th">State</th>
                    <th className="th">Lease</th>
                    <th className="th">Sessions</th>
                    <th className="th">Seats left</th>
                    <th className="th">Admitting</th>
                  </tr>
                </thead>
                <tbody>
                  {group.nodes.map((n) => {
                    const state = nodeState(n)
                    return (
                      <tr key={n.label}>
                        <td className="td">
                          {n.ready ? (
                            <Link className="link mono break-all" to={`/nodes/${encodeURIComponent(n.label)}`}>{n.label}</Link>
                          ) : (
                            <span className="mono break-all">{n.label}</span>
                          )}
                          {n.needsRestart && <div className="text-ink-muted">restart pending</div>}
                        </td>
                        <td className="td"><Status mark={state.mark}>{state.text}</Status></td>
                        <td className="td text-ink-muted">{n.leaseAge === null ? 'none yet' : `${age(n.leaseAge)} ago`}</td>
                        <td className="td tabular-nums">{n.leaseAge === null ? '-' : n.sessions}</td>
                        <td className="td tabular-nums">{n.seatsLeft < 0 ? '-' : n.seatsLeft}</td>
                        <td className="td">{n.leaseAge === null ? '-' : n.admitting ? 'yes' : 'no'}</td>
                      </tr>
                    )
                  })}
                  {group.nodes.length === 0 && (
                    <tr><td className="td text-ink-muted" colSpan={6}>No node runs: the group is stopped or starting.</td></tr>
                  )}
                </tbody>
              </table>
            </div>
            <p className="text-sm text-ink-muted">Seats left counts quack clients a node can still take; Flight sessions are bounded by sessions alone.</p>
          </section>
        </>
      )}
    </>
  )
}
