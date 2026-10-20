import { CircleAlert } from 'lucide-react'
import { Link, useParams } from 'react-router-dom'
import { Bar, Chip, Crumbs, Header, Problem, Refresh, Stat, Status, age } from '../components/ui'
import { useApp, useLoad } from '../lib/context'
import { bytes, share } from '../lib/format'
import type { NodeInfo } from '../lib/types'
import { groupState, podReason, starting } from './Fleet'

export function nodeState(n: NodeInfo): { mark: 'ok' | 'half' | 'off'; text: string; why?: string } {
  // the pod first: a node a scale made is in the group before it serves - starting, or not starting and why
  if (n.pod?.state === 'failing') return { mark: 'off', text: 'Not starting', why: podReason(n.pod) }
  if (n.pod?.state === 'stopping') return { mark: 'half', text: 'Stopping', why: n.draining ? 'draining its sessions' : undefined }
  if (starting(n)) {
    const why = n.pod!.reason === 'NotReady' ? 'running, its doors not serving yet'
      : n.pod!.reason === 'Unschedulable' ? `waiting for room: ${n.pod!.message ?? ''}` : n.pod!.reason
    return { mark: 'half', text: 'Starting', why }
  }
  // it served, and its doors stopped answering (its readiness probe)
  if (!n.ready && n.leaseAge !== null) return { mark: 'off', text: 'Not ready', why: 'its doors stopped answering' }
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
          {group.problem && (
            <div role="alert" className="flex items-start gap-4 rounded-md border border-line px-6 py-4">
              <CircleAlert size={20} aria-hidden="true" className="mt-0.5 flex-none" />
              <div>
                <div className="font-semibold">The group's Deployment reports a problem</div>
                <div className="break-words text-sm text-ink-muted">{group.problem}</div>
              </div>
            </div>
          )}
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
                    <th className="th">Pod memory</th>
                    <th className="th">Seats left</th>
                    <th className="th">Admitting</th>
                  </tr>
                </thead>
                <tbody>
                  {group.nodes.map((n) => {
                    const state = nodeState(n)
                    // a node not serving yet stands out: starting softly, not starting as an alert
                    const tone = state.text === 'Not starting' ? 'border-l-4 border-l-[var(--ink)] bg-[var(--surface-soft)]'
                      : state.text === 'Starting' ? 'bg-[var(--surface-soft)]' : ''
                    return (
                      <tr key={n.label} className={tone}>
                        <td className="td">
                          {/* every node has its page - one not starting too: its pod, its events, its previous log */}
                          <Link className="link mono break-all" to={`/nodes/${encodeURIComponent(n.label)}`}>{n.label}</Link>
                          {n.needsRestart && <div className="text-ink-muted">restart pending</div>}
                        </td>
                        <td className="td">
                          <Status mark={state.mark}>{state.text}</Status>
                          {state.why && <div className="mt-1 max-w-[420px] break-words text-sm text-ink-muted">{state.why}</div>}
                          {n.pod && n.pod.restarts > 0 && <div className="text-sm text-ink-muted">restarted {n.pod.restarts} {n.pod.restarts === 1 ? 'time' : 'times'}</div>}
                        </td>
                        <td className="td text-ink-muted">{n.leaseAge === null ? 'none yet' : `${age(n.leaseAge)} ago`}</td>
                        <td className="td tabular-nums">{n.leaseAge === null ? '-' : n.sessions}</td>
                        <td className="td">
                          {n.usage ? (
                            <div className="flex flex-col gap-1">
                              <span className="tabular-nums">{bytes(n.usage.memoryBytes)}{group.memoryLimitBytes ? ` of ${bytes(group.memoryLimitBytes)}` : ''}</span>
                              {group.memoryLimitBytes > 0 && <Bar percent={share(n.usage.memoryBytes, group.memoryLimitBytes)} />}
                            </div>
                          ) : <span className="text-ink-muted">-</span>}
                        </td>
                        <td className="td tabular-nums">{n.seatsLeft < 0 ? '-' : n.seatsLeft}</td>
                        <td className="td">{n.leaseAge === null ? '-' : n.admitting ? 'yes' : 'no'}</td>
                      </tr>
                    )
                  })}
                  {group.nodes.length === 0 && (
                    <tr><td className="td text-ink-muted" colSpan={7}>{group.problem ? 'No node runs: see the problem above.' : 'No node runs: the group is stopped or starting.'}</td></tr>
                  )}
                  {group.nodes.length > 0 && group.nodes.filter((n) => n.pod?.state !== 'stopping').length < group.replicas && (
                    <tr className="bg-[var(--surface-soft)]">
                      <td className="td text-ink-muted" colSpan={7}>
                        {(() => {
                          const more = group.replicas - group.nodes.filter((n) => n.pod?.state !== 'stopping').length
                          return `${more} more ${more === 1 ? 'node is' : 'nodes are'} wanted and no pod is made yet${group.problem ? ': see above' : ''}.`
                        })()}
                      </td>
                    </tr>
                  )}
                </tbody>
              </table>
            </div>
            <p className="text-sm text-ink-muted">Pod memory is the pod's, by the metrics API, against its size's limit; the engine's own share is on each node's page. Seats left counts quack clients a node can still take.</p>
          </section>
          <section aria-labelledby="identity-h" className="flex flex-col gap-4">
            <div>
              <h2 id="identity-h" className="text-[22px] font-semibold leading-[30px]">Identity</h2>
              <p className="text-sm text-ink-muted">Every node of the group is the same workload: it logs in to tresor as it, and answers its clients under the group's name.</p>
            </div>
            <dl className="panel grid grid-cols-[repeat(auto-fit,minmax(280px,1fr))] gap-x-8 gap-y-6 p-8 text-sm">
              <div><dt className="eyebrow">ServiceAccount</dt><dd className="mono break-all">{group.identity.serviceAccount}</dd></div>
              <div><dt className="eyebrow">Subject at the identity provider</dt><dd className="mono break-all">{group.identity.subject}</dd></div>
              <div><dt className="eyebrow">Client id</dt><dd className="mono break-all">{group.identity.clientID || '-'}</dd></div>
              <div><dt className="eyebrow">Logs in to</dt><dd className="mono break-all">{group.identity.tresor || '-'}</dd></div>
              <div><dt className="eyebrow">Answers as</dt><dd className="mono break-all">{group.identity.tlsName}</dd></div>
            </dl>
          </section>
        </>
      )}
    </>
  )
}
