// A group's own life from the console (spec 006): made, changed, deleted - the cluster right, judged by the
// operator's webhook - and its place in the policy, a second step taken by hand: the policy right, judged by the node.
import { useState } from 'react'
import { CircleAlert } from 'lucide-react'
import { Chip, Dialog, Problem, messageOf } from '../components/ui'
import { useApp, useLoad } from '../lib/context'
import type { Fleet, Group, GroupPatch } from '../lib/types'

const field = 'h-11 w-full rounded-sm border border-ink-muted bg-surface px-3 text-base text-ink'
const label = 'text-sm font-semibold'

/** a group's name as the webhook takes it: a DNS label its pods' names start with */
export const groupName = /^[a-z0-9]([a-z0-9-]{0,34}[a-z0-9])?$/

/** a resource group's name as the first policy takes it: every node's bootstrap document lists every group's */
export const resourceGroupName = /^[A-Za-z0-9_-]{1,63}$/

/** a stopping node's wait for its sessions, as the console takes it */
export const drainOK = (seconds: number) => Number.isInteger(seconds) && seconds >= 0 && seconds <= 86400

const hint = 'text-sm text-ink-muted'

function Failure({ text }: { text?: string }) {
  return text ? <p role="alert" className="rounded-md bg-surface-soft px-4 py-3 text-sm font-medium">{text}</p> : null
}

/** why a new group cannot be made as the form stands; nothing when it can */
export function newGroupProblem(f: { name: string; size: string; replicas: number; clientID: string; drain: number;
  resourceGroup: string }, room: number): string | undefined {
  if (!groupName.test(f.name)) return 'Name the group.'
  if (f.size === '') return 'The plan lists no size.'
  if (f.replicas > room) return `Past the plan: ${room} more ${room === 1 ? 'node fits' : 'nodes fit'}.`
  if (f.clientID.trim() === '') return "Give the identity's client id."
  if (!drainOK(f.drain)) return 'The drain time is 0 to 86400 seconds.'
  if (f.resourceGroup.trim() !== '' && !resourceGroupName.test(f.resourceGroup.trim()))
    return "A resource group's name is letters, digits, '_' and '-', at most 63."
  return undefined
}

/** the nodes the plan has left for a new group: its nodes less what every group keeps (an autoscaled one at its max) */
export const freeNodes = (fleet: Fleet) => fleet.plan.maxNodes -
  fleet.groups.reduce((n, g) => n + (g.scaling.mode === 'auto' ? (g.scaling.max ?? g.replicas) : g.replicas), 0)

export function NewGroupDialog({ fleet, onDone }: { fleet: Fleet; onDone: (made: boolean) => void }) {
  const { api } = useApp()
  const [name, setName] = useState('')
  const [size, setSize] = useState(fleet.plan.sizes[0] ?? '')
  const [replicas, setReplicas] = useState(1)
  const [clientID, setClientID] = useState('')
  const [quack, setQuack] = useState(false)
  const [drain, setDrain] = useState(300)
  const [resourceGroup, setResourceGroup] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  const nameOK = groupName.test(name)
  const room = freeNodes(fleet)
  const why = newGroupProblem({ name, size, replicas, clientID, drain, resourceGroup }, room)
  const make = async () => {
    setBusy(true)
    setError(undefined)
    try {
      await api.createGroup({ name, size, replicas, clientID: clientID.trim(), quack, drainSeconds: drain,
        ...(resourceGroup.trim() ? { resourceGroup: resourceGroup.trim() } : {}) })
      onDone(true)
    } catch (e) {
      setError(messageOf(e)) // the webhook's reason (past the plan, a size not allowed), or the console's
      setBusy(false)
    }
  }
  return (
    <Dialog open title="New group" onClose={() => onDone(false)}>
      <p className="text-ink-muted">
        Its nodes start once the group is made. They serve nobody until the policy has the group's resource group: add it from the group's page.
      </p>
      <div className="flex flex-col gap-2">
        <label htmlFor="ng-name" className={label}>Name</label>
        <input id="ng-name" className={field} value={name} onChange={(e) => setName(e.target.value.trim())}
          aria-invalid={name !== '' && !nameOK} aria-describedby="ng-name-hint" autoComplete="off" />
        <p id="ng-name-hint" className={hint}>Lower-case letters, digits and '-', starting and ending with a letter or a digit, at most 36.</p>
      </div>
      <div className="grid grid-cols-2 gap-4">
        <div className="flex flex-col gap-2">
          <label htmlFor="ng-size" className={label}>Size</label>
          <select id="ng-size" className={field} value={size} onChange={(e) => setSize(e.target.value)}>
            {fleet.plan.sizes.map((s) => <option key={s} value={s}>{s}</option>)}
          </select>
        </div>
        <div className="flex flex-col gap-2">
          <label htmlFor="ng-replicas" className={label}>Nodes</label>
          <input id="ng-replicas" type="number" min={0} max={room} className={field} value={replicas}
            aria-invalid={replicas > room} aria-describedby="ng-replicas-hint"
            onChange={(e) => setReplicas(Math.max(0, parseInt(e.target.value, 10) || 0))} />
          <p id="ng-replicas-hint" className={hint}>The plan has room for {Math.max(0, room)} more.</p>
        </div>
      </div>
      <div className="flex flex-col gap-2">
        <label htmlFor="ng-client" className={label}>Identity's client id</label>
        <input id="ng-client" className={`${field} mono`} value={clientID} onChange={(e) => setClientID(e.target.value)}
          aria-describedby="ng-client-hint" autoComplete="off" />
        <p id="ng-client-hint" className={hint}>
          An identity the identity provider already has, which trusts the subject <Chip>{fleet.newGroup.subjectPrefix}{name || '<name>'}</Chip>. The console makes none.
        </p>
      </div>
      <div className="grid grid-cols-2 gap-4">
        <div className="flex flex-col gap-2">
          <label htmlFor="ng-rg" className={label}>Resource group</label>
          <input id="ng-rg" className={`${field} mono`} value={resourceGroup} placeholder={name || 'the group\'s name'}
            aria-invalid={resourceGroup.trim() !== '' && !resourceGroupName.test(resourceGroup.trim())}
            aria-describedby="ng-rg-hint" onChange={(e) => setResourceGroup(e.target.value)} autoComplete="off" />
          <p id="ng-rg-hint" className={hint}>Empty: the group's name.</p>
        </div>
        <div className="flex flex-col gap-2">
          <label htmlFor="ng-drain" className={label}>Drain, seconds</label>
          <input id="ng-drain" type="number" min={0} max={86400} className={field} value={drain}
            aria-invalid={!drainOK(drain)} onChange={(e) => setDrain(Math.max(0, parseInt(e.target.value, 10) || 0))} />
        </div>
      </div>
      <label className="flex items-center gap-3 text-sm">
        <input type="checkbox" checked={quack} onChange={(e) => setQuack(e.target.checked)} className="h-5 w-5" />
        Open the quack door beside Flight
      </label>
      <Failure text={error} />
      {why && name !== '' && <p className={hint}>{why}</p>}
      <div className="flex flex-wrap justify-end gap-2">
        <button type="button" className="btn-outline" onClick={() => onDone(false)}>Cancel</button>
        <button type="button" className="btn-primary" disabled={why !== undefined || busy} onClick={make}>Make the group</button>
      </div>
    </Dialog>
  )
}

/** the changes a group's edit sends: one PATCH per field changed, in order */
export function changesOf(group: Group, size: string, quack: boolean, drain: number): GroupPatch[] {
  const out: GroupPatch[] = []
  if (size !== group.size) out.push({ size })
  if (quack !== group.quack) out.push({ quack })
  if (drain !== group.drainSeconds) out.push({ drainSeconds: drain })
  return out
}

export function EditGroupDialog({ fleet, group, onDone }: { fleet: Fleet; group: Group; onDone: (changed: boolean) => void }) {
  const { api } = useApp()
  const [size, setSize] = useState(group.size)
  const [quack, setQuack] = useState(group.quack)
  const [drain, setDrain] = useState(group.drainSeconds)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  const [made, setMade] = useState(0)
  const changes = changesOf(group, size, quack, drain)
  const save = async () => {
    setBusy(true)
    setError(undefined)
    let done = 0
    try {
      for (const change of changes) {
        await api.changeGroup(group.name, change)
        done++
        setMade((n) => n + 1)
      }
      onDone(true)
    } catch (e) {
      setError(done > 0 ? `${messageOf(e)} (the changes before it were made)` : messageOf(e))
      setBusy(false)
    }
  }
  // a change made (or under way) is the page's to show: closing reads the group again
  const close = () => onDone(made > 0 || busy)
  const sizes = fleet.plan.sizes.includes(group.size) ? fleet.plan.sizes : [group.size, ...fleet.plan.sizes]
  return (
    <Dialog open title={`Change ${group.name}`} onClose={close}>
      <p className="text-ink-muted">Each change replaces the group's nodes one at a time, each drained first - a new drain time too.</p>
      <div className="grid grid-cols-2 gap-4">
        <div className="flex flex-col gap-2">
          <label htmlFor="eg-size" className={label}>Size</label>
          <select id="eg-size" className={field} value={size} onChange={(e) => setSize(e.target.value)}>
            {sizes.map((s) => <option key={s} value={s}>{s}</option>)}
          </select>
        </div>
        <div className="flex flex-col gap-2">
          <label htmlFor="eg-drain" className={label}>Drain, seconds</label>
          <input id="eg-drain" type="number" min={0} max={86400} className={field} value={drain} aria-invalid={!drainOK(drain)}
            onChange={(e) => setDrain(Math.max(0, parseInt(e.target.value, 10) || 0))} />
          {!drainOK(drain) && <p className={hint}>0 to 86400 seconds.</p>}
        </div>
      </div>
      <label className="flex items-center gap-3 text-sm">
        <input type="checkbox" checked={quack} onChange={(e) => setQuack(e.target.checked)} className="h-5 w-5" />
        Open the quack door beside Flight
      </label>
      <Failure text={error} />
      <div className="flex flex-wrap justify-end gap-2">
        <button type="button" className="btn-outline" disabled={busy} onClick={close}>Cancel</button>
        <button type="button" className="btn-primary" disabled={changes.length === 0 || !drainOK(drain) || busy} onClick={save}>Save</button>
      </div>
    </Dialog>
  )
}

export function DeleteGroupDialog({ group, onDone }: { group: Group; onDone: (deleted: boolean) => void }) {
  const { api } = useApp()
  const [typed, setTyped] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  const remove = async () => {
    setBusy(true)
    setError(undefined)
    try {
      await api.deleteGroup(group.name)
      onDone(true)
    } catch (e) {
      setError(messageOf(e))
      setBusy(false)
    }
  }
  return (
    <Dialog open title={`Delete ${group.name}?`} onClose={() => onDone(false)}>
      <p className="text-ink-muted">
        Its nodes drain and stop. The policy keeps the resource group <Chip>{group.resourceGroup}</Chip>: remove it from the policy on its own if nothing else serves it. If it is the last group that serves you, the console finds no node to read your rights on; kubectl still can.
      </p>
      <div className="flex flex-col gap-2">
        <label htmlFor="dg-name" className={label}>Type the group's name</label>
        <input id="dg-name" className={`${field} mono`} value={typed} onChange={(e) => setTyped(e.target.value)} autoComplete="off" />
      </div>
      <Failure text={error} />
      <div className="flex flex-wrap justify-end gap-2">
        <button type="button" className="btn-outline" onClick={() => onDone(false)}>Cancel</button>
        <button type="button" className="btn-primary" disabled={typed !== group.name || busy} onClick={remove}>Delete</button>
      </div>
    </Dialog>
  )
}

/** the group's place in the policy: read on a node under the person's token, written there by hand */
export function PolicySection({ group, read, onChanged }: { group: Group; read?: number; onChanged: () => void }) {
  const { api } = useApp()
  // read again with the page (its Refresh, a change here): `read` is when the page read the fleet
  const policy = useLoad(() => api.groupPolicy(group.name), [api, group.name, read])
  const [adding, setAdding] = useState(false)
  const [removing, setRemoving] = useState(false)
  const p = policy.data
  const done = (changed: boolean) => {
    setAdding(false)
    setRemoving(false)
    if (changed) onChanged() // the page reads the fleet again, and this section with it
  }
  return (
    <section aria-labelledby="policy-h" className="flex flex-col gap-4">
      <div>
        <h2 id="policy-h" className="text-[22px] font-semibold leading-[30px]">Policy</h2>
        <p className="text-sm text-ink-muted">The group's nodes serve the principals the policy places in its resource group. A node writes what you do here, under your sign-in.</p>
      </div>
      {policy.error !== undefined && <Problem error={policy.error} />}
      {policy.loading && !p && <p className={hint}>Reading the policy on a node…</p>}
      {p && (
        <div className="panel flex flex-wrap items-start justify-between gap-6 p-8">
          <div className="flex flex-col gap-2">
            <div className="flex flex-wrap items-center gap-2">
              <Chip>{p.resourceGroup}</Chip>
              {p.inPolicy ? <span className="font-semibold">in the policy{p.default ? ', the default group' : ''}</span>
                : <span className="flex items-center gap-1.5 font-semibold"><CircleAlert size={16} aria-hidden="true" />not in the policy: the group's nodes drain themselves</span>}
            </div>
            {p.inPolicy && (
              <div className="text-sm text-ink-muted">
                {p.bound.length ? <>Roles: <span className="mono">{p.bound.join(', ')}</span></> : 'No role is bound to it.'}
              </div>
            )}
          </div>
          <div className="flex flex-wrap gap-2">
            {(!p.inPolicy || p.roles.some((r) => !p.bound.includes(r))) && (
              <button type="button" className="btn-outline" onClick={() => setAdding(true)}>{p.inPolicy ? 'Bind roles' : 'Add to the policy'}</button>
            )}
            {p.inPolicy && !p.default && <button type="button" className="btn-quiet" onClick={() => setRemoving(true)}>Remove from the policy</button>}
          </div>
        </div>
      )}
      {adding && p && <AddToPolicyDialog group={group.name} policy={p} onDone={done} />}
      {removing && p && <RemoveFromPolicyDialog group={group.name} resourceGroup={p.resourceGroup} onDone={done} />}
    </section>
  )
}

function AddToPolicyDialog({ group, policy, onDone }: { group: string; policy: { resourceGroup: string; inPolicy: boolean;
  roles: string[]; bound: string[] }; onDone: (changed: boolean) => void }) {
  const { api } = useApp()
  const [roles, setRoles] = useState<string[]>([])
  const [isDefault, setDefault] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  // a write that failed half way (the resource group made, a role's binding refused) is the page's to show
  const [failed, setFailed] = useState(false)
  const free = policy.roles.filter((r) => !policy.bound.includes(r))
  const toggle = (role: string) => setRoles((rs) => rs.includes(role) ? rs.filter((r) => r !== role) : [...rs, role])
  const add = async () => {
    setBusy(true)
    setError(undefined)
    try {
      await api.addToPolicy(group, roles, !policy.inPolicy && isDefault)
      onDone(true)
    } catch (e) {
      setError(messageOf(e)) // the node's refusal: the policy bundle, a name it does not take
      setFailed(true)
      setBusy(false)
    }
  }
  return (
    <Dialog open title={policy.inPolicy ? `Bind roles to ${policy.resourceGroup}` : `Add ${policy.resourceGroup} to the policy`}
      onClose={() => onDone(failed)}>
      <p className="text-ink-muted">
        {policy.inPolicy ? 'Principals of the roles you pick are placed in this resource group.'
          : 'The policy gets the resource group, with no limits, and the roles you pick are placed in it. Limits are set with the policy.'}
      </p>
      <fieldset className="flex max-h-64 flex-col gap-2 overflow-y-auto">
        <legend className="mb-2 text-sm font-semibold">Roles</legend>
        {free.map((role) => (
          <label key={role} className="flex items-center gap-3 text-sm">
            <input type="checkbox" checked={roles.includes(role)} onChange={() => toggle(role)} className="h-5 w-5" />
            <span className="mono">{role}</span>
          </label>
        ))}
        {free.length === 0 && <p className="text-sm text-ink-muted">Every role of the policy is bound to it already.</p>}
      </fieldset>
      {!policy.inPolicy && (
        <label className="flex items-center gap-3 text-sm">
          <input type="checkbox" checked={isDefault} onChange={(e) => setDefault(e.target.checked)} className="h-5 w-5" />
          The default group: principals bound to no group are its members
        </label>
      )}
      <Failure text={error} />
      <div className="flex flex-wrap justify-end gap-2">
        <button type="button" className="btn-outline" disabled={busy} onClick={() => onDone(failed)}>Cancel</button>
        <button type="button" className="btn-primary" disabled={busy || (policy.inPolicy && roles.length === 0)} onClick={add}>
          {policy.inPolicy ? 'Bind' : 'Add to the policy'}
        </button>
      </div>
    </Dialog>
  )
}

function RemoveFromPolicyDialog({ group, resourceGroup, onDone }: { group: string; resourceGroup: string;
  onDone: (changed: boolean) => void }) {
  const { api } = useApp()
  const [typed, setTyped] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  const remove = async () => {
    setBusy(true)
    setError(undefined)
    try {
      await api.removeFromPolicy(group, resourceGroup)
      onDone(true)
    } catch (e) {
      setError(messageOf(e))
      setBusy(false)
    }
  }
  return (
    <Dialog open title={`Remove ${resourceGroup} from the policy?`} onClose={() => onDone(false)}>
      <p className="text-ink-muted">
        Its role bindings go with it. Every node that serves it drains itself: the principals it served are placed in their other groups, or refused. The console refuses while another group serves it, or while it is the policy's default group.
      </p>
      <div className="flex flex-col gap-2">
        <label htmlFor="rp-name" className={label}>Type the resource group's name</label>
        <input id="rp-name" className={`${field} mono`} value={typed} onChange={(e) => setTyped(e.target.value)} autoComplete="off" />
      </div>
      <Failure text={error} />
      <div className="flex flex-wrap justify-end gap-2">
        <button type="button" className="btn-outline" onClick={() => onDone(false)}>Cancel</button>
        <button type="button" className="btn-primary" disabled={busy || typed !== resourceGroup} onClick={remove}>Remove</button>
      </div>
    </Dialog>
  )
}
