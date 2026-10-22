import { useState } from 'react'
import { CircleAlert, Minus, Plus, RotateCcw } from 'lucide-react'
import { NewGroupDialog } from './GroupActions'
import { Link } from 'react-router-dom'
import { Chip, Dialog, Glyph, Header, Problem, Refresh, Stat, Status, messageOf, type Mark } from '../components/ui'
import { useApp, useLoad } from '../lib/context'
import type { Fleet as FleetAnswer, Group, NodeInfo, PodState } from '../lib/types'

/** a group's state as one word and a circle (spec 003 round 2: shape, never colour alone) */
export function groupState(g: Group): { mark: Mark; text: string; note: string } {
  const condition = (type: string) => g.conditions.find((c) => c.type === type && c.status === 'True')
  if (condition('Refused')) return { mark: 'off', text: 'Refused', note: condition('Refused')!.message }
  if (condition('Draining')) return { mark: 'half', text: 'Draining', note: 'The policy has no such resource group' }
  if (g.replicas === 0) return { mark: 'off', text: 'Stopped', note: g.scaling.mode === 'auto' ? 'Starts on the first client' : '' }
  // what does not start weighs by what it costs: short of the count, the group is off; at it (a roll's extra pod),
  // only a new node waits
  const short = g.readyReplicas < g.replicas
  if (g.problem) return { mark: short ? 'off' : 'half', text: short ? 'Not starting' : 'A new node is not made', note: g.problem }
  const failing = g.nodes.find((n) => n.pod?.state === 'failing')
  if (failing)
    return { mark: short ? 'off' : 'half', text: short ? 'A node does not start' : 'A new node does not start',
      note: `${failing.label}: ${podReason(failing.pod!)}` }
  if (g.profile.needsRestart.length > 0)
    return { mark: 'half', text: 'Restart pending', note: 'A node applies an item only at a restart' }
  if (short) return { mark: 'half', text: 'Starting', note: `${g.readyReplicas} of ${nodes(g.replicas)} ready: the operator makes the rest` }
  const stopping = g.nodes.find((n) => n.pod?.state === 'stopping')
  if (stopping || g.readyReplicas > g.replicas)
    return { mark: 'half', text: 'Scaling down', note: stopping ? `${stopping.label} drains, then goes` : `${g.readyReplicas} ready, ${g.replicas} asked` }
  if (g.lost.length > 0) return { mark: 'half', text: 'Node lost', note: g.lost.join(', ') }
  return { mark: 'ok', text: 'Ready', note: '' }
}

/** "1 node", "3 nodes" */
export const nodes = (n: number) => `${n} ${n === 1 ? 'node' : 'nodes'}`

/** a pod's reason and Kubernetes' own words, as one line */
export function podReason(p: PodState): string {
  return [p.reason, p.message].filter(Boolean).join(' - ') || p.phase
}

/** a node not serving yet that never has: its pod made, no heartbeat of its own yet */
export const starting = (n: NodeInfo) => !n.ready && n.leaseAge === null && !!n.pod &&
  (n.pod.state === 'starting' || n.pod.state === 'running')

/** a glyph per node the group wants: serving, starting, then the ones not starting or not made yet */
export function wanted(g: Group): Mark[] {
  const coming = g.nodes.filter(starting).length
  return Array.from({ length: g.replicas }, (_, i) =>
    i < g.readyReplicas ? 'ok' : i < g.readyReplicas + coming ? 'half' : 'off')
}

/** what the group's count may grow to: the plan's nodes less what the others keep (an autoscaled one at its max) */
export function room(fleet: FleetAnswer, group: Group): number {
  const kept = fleet.groups
    .filter((g) => g.name !== group.name)
    .reduce((n, g) => n + (g.scaling.mode === 'auto' ? (g.scaling.max ?? g.replicas) : g.replicas), 0)
  return fleet.plan.maxNodes - kept
}

export function Fleet() {
  const { api, rights } = useApp()
  const fleet = useLoad(() => api.fleet(), [api])
  const [scaling, setScaling] = useState<Group | null>(null)
  const [restarting, setRestarting] = useState<Group | null>(null)
  const [making, setMaking] = useState(false)
  const data = fleet.data
  const running = data?.groups.reduce((n, g) => n + g.replicas, 0) ?? 0
  const ready = data?.groups.reduce((n, g) => n + g.readyReplicas, 0) ?? 0
  return (
    <>
      <Header title="Fleet"
        lead="The node groups of this platform and how their nodes are doing. Kubernetes holds this state; the page reads it when it opens."
        actions={
          <div className="flex flex-wrap items-center gap-4">
            <Refresh at={fleet.at} loading={fleet.loading} onClick={fleet.reload} />
            {rights.scale && data?.newGroup?.available && (
              <button type="button" className="btn-primary" onClick={() => setMaking(true)}>
                <Plus size={18} aria-hidden="true" />
                New group
              </button>
            )}
          </div>
        } />
      {fleet.error !== undefined && <Problem error={fleet.error} />}
      {data && (
        <>
          <section aria-label="Summary" className="panel flex flex-wrap gap-x-16 gap-y-4 px-8">
            <Stat label="Nodes ready" value={`${ready} of ${running}`} />
            <Stat label="Plan" value={`${running} of ${data.plan.maxNodes} nodes running`} />
            <Stat label="Sizes allowed" value={data.plan.sizes.join(', ') || '-'} />
          </section>
          <section aria-labelledby="groups-h" className="flex flex-col gap-4">
            <h2 id="groups-h" className="text-[22px] font-semibold leading-[30px]">Groups</h2>
            <div className="overflow-x-auto rounded-md border border-line">
              <table className="w-full min-w-[1040px] border-collapse">
                <thead>
                  <tr>
                    <th className="th">Group</th>
                    <th className="th">Nodes</th>
                    <th className="th">Scaling</th>
                    <th className="th">Resource group</th>
                    <th className="th">Profile</th>
                    <th className="th">Status</th>
                    {rights.scale && <th className="th text-right">Actions</th>}
                  </tr>
                </thead>
                <tbody>
                  {data.groups.map((g) => {
                    const state = groupState(g)
                    return (
                      <tr key={g.name}>
                        <td className="td">
                          <Link className="link text-base" to={`/groups/${encodeURIComponent(g.name)}`}>{g.name}</Link>
                          <div className="text-ink-muted">{g.size}</div>
                        </td>
                        <td className="td">
                          <span role="img" className="flex min-h-3 gap-1.5"
                            aria-label={`${g.readyReplicas} of ${g.replicas} ready${g.nodes.filter(starting).length ? `, ${g.nodes.filter(starting).length} starting` : ''}`}>
                            {wanted(g).map((mark, i) => <Glyph key={i} mark={mark} />)}
                          </span>
                          <div className="text-ink-muted">{g.replicas === 0 ? 'no nodes' : `${g.readyReplicas} of ${g.replicas} ready`}</div>
                        </td>
                        <td className="td">
                          {g.scaling.mode === 'auto' ? (
                            <span className="flex items-center gap-2 font-semibold">
                              {g.scaling.min} to {g.scaling.max} nodes <span className="pill border-brand-strong text-brand-strong">auto</span>
                            </span>
                          ) : (
                            <span className="font-semibold">{g.replicas} {g.replicas === 1 ? 'node' : 'nodes'}, manual</span>
                          )}
                        </td>
                        <td className="td">
                          <span className="flex flex-wrap items-center gap-2">
                            <Chip>{g.resourceGroup}</Chip>
                          </span>
                          {g.resourceGroupKnown === false && (
                            <span className="mt-1 flex items-center gap-1.5 font-medium">
                              <CircleAlert size={16} aria-hidden="true" />
                              not in the policy
                            </span>
                          )}
                        </td>
                        <td className="td">
                          <div className="font-semibold">version {g.profile.target}</div>
                          <div className="text-ink-muted">
                            {g.replicas === 0 ? 'no node to apply' : `${g.profile.applied} of ${g.replicas} applied`}
                          </div>
                        </td>
                        <td className="td">
                          <Status mark={state.mark}>{state.text}</Status>
                          {state.note && <div className="mt-1 text-ink-muted">{state.note}</div>}
                        </td>
                        {rights.scale && (
                          <td className="td">
                            <div className="flex flex-wrap justify-end gap-2">
                              <button type="button" className="btn-quiet" onClick={() => setScaling(g)}>Scale</button>
                              <button type="button" className="btn-quiet" onClick={() => setRestarting(g)} disabled={g.replicas === 0}>
                                <RotateCcw size={16} aria-hidden="true" />
                                Restart
                              </button>
                            </div>
                          </td>
                        )}
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>
            {!rights.scale && (
              <p className="text-sm text-ink-muted">
                Scaling and restarting a group take the cluster right, as making, changing and deleting one do. Your rights come from the policy; ask its administrator.
              </p>
            )}
          </section>
          {scaling && <ScaleDialog fleet={data} group={scaling} onDone={(changed) => { setScaling(null); if (changed) fleet.reload() }} />}
          {making && <NewGroupDialog fleet={data} onDone={(made) => { setMaking(false); if (made) fleet.reload() }} />}
          {restarting && <RestartDialog group={restarting} onDone={(changed) => { setRestarting(null); if (changed) fleet.reload() }} />}
        </>
      )}
    </>
  )
}

function ScaleDialog({ fleet, group, onDone }: { fleet: FleetAnswer; group: Group; onDone: (changed: boolean) => void }) {
  const { api } = useApp()
  const [replicas, setReplicas] = useState(group.replicas)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  const max = room(fleet, group)
  const over = replicas > max
  const save = async () => {
    setBusy(true)
    setError(undefined)
    try {
      await api.scale(group.name, replicas)
      onDone(true)
    } catch (e) {
      setError(messageOf(e)) // the webhook's reason, as the operator passes it
      setBusy(false)
    }
  }
  return (
    <Dialog open title={`Scale ${group.name}`} onClose={() => onDone(false)}>
      <p className="text-ink-muted">How many nodes this group runs. Kubernetes starts or stops them; a node that stops drains first.</p>
      {group.scaling.mode === 'auto' ? (
        <p className="text-sm">This group scales by itself between {group.scaling.min} and {group.scaling.max}; its bounds are set where it is defined.</p>
      ) : (
        <div className="flex flex-col gap-2">
          <label htmlFor="replicas" className="text-sm font-semibold">Nodes</label>
          <div className="flex items-center gap-2">
            <button type="button" className="btn-quiet h-11 w-11 px-0" aria-label="One node fewer"
              onClick={() => setReplicas((r) => Math.max(0, r - 1))}><Minus size={18} aria-hidden="true" /></button>
            <input id="replicas" type="number" min={0} value={replicas}
              onChange={(e) => setReplicas(Math.max(0, parseInt(e.target.value, 10) || 0))}
              className="h-11 w-20 rounded-sm border border-ink-muted bg-surface text-center text-base text-ink" />
            <button type="button" className="btn-quiet h-11 w-11 px-0" aria-label="One node more"
              onClick={() => setReplicas((r) => r + 1)}><Plus size={18} aria-hidden="true" /></button>
          </div>
          {over ? (
            <p role="alert" className="flex items-start gap-2 rounded-md bg-surface-soft px-4 py-3 text-sm font-medium">
              <CircleAlert size={18} aria-hidden="true" className="mt-0.5 flex-none" />
              Past the plan: this group can run at most {max} ({fleet.plan.maxNodes} nodes in all, {fleet.plan.maxNodes - max} kept by the other groups).
            </p>
          ) : (
            <p className="text-sm text-ink-muted">The plan allows up to {max} here.</p>
          )}
        </div>
      )}
      {error && <p role="alert" className="rounded-md bg-surface-soft px-4 py-3 text-sm font-medium">{error}</p>}
      <div className="flex flex-wrap justify-end gap-2">
        <button type="button" className="btn-outline" onClick={() => onDone(false)}>Cancel</button>
        {group.scaling.mode !== 'auto' && (
          <button type="button" className="btn-primary" disabled={over || busy || replicas === group.replicas} onClick={save}>
            Scale to {replicas}
          </button>
        )}
      </div>
    </Dialog>
  )
}

function RestartDialog({ group, onDone }: { group: Group; onDone: (changed: boolean) => void }) {
  const { api } = useApp()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  const restart = async () => {
    setBusy(true)
    try {
      await api.restart(group.name)
      onDone(true)
    } catch (e) {
      setError(messageOf(e))
      setBusy(false)
    }
  }
  return (
    <Dialog open title={`Restart ${group.name}?`} onClose={() => onDone(false)}>
      <p className="text-ink-muted">
        Its nodes are replaced one at a time. Each drains first: new clients go to the group's other nodes, open sessions keep working until the node stops.
      </p>
      {error && <p role="alert" className="rounded-md bg-surface-soft px-4 py-3 text-sm font-medium">{error}</p>}
      <div className="flex flex-wrap justify-end gap-2">
        <button type="button" className="btn-outline" onClick={() => onDone(false)}>Cancel</button>
        <button type="button" className="btn-primary" disabled={busy} onClick={restart}>Restart</button>
      </div>
    </Dialog>
  )
}
