# Spec 011: a node group scales itself - from its load, down to zero

- **Status**: accepted (the owner, 2026-11-16); stage 1 built (the operator's decision, 2026-11-19), stage 2 (the
  front's demand) and stage 3 (the console's dialog) to come
- **Date**: 2026-11-16
- **Author**: Claude, from the owner's decisions of 2026-11-16 (in the release, not after it)
- **Follows**: spec 001 (the operator, the heartbeat, the Lease, the pod-deletion cost), spec 002 (the front's seating,
  `noNodeWait`), spec 003 (the console's reserved `scaling` API), duckdb-acl specs 066 (drain), 079 (seats), 096
  (a resource group is a set of nodes), 135 (a session moves between nodes)

## Summary

A node group runs between a minimum and a maximum number of nodes. Under load the operator adds nodes up to the
maximum; a node that has had no session for a while is drained and stopped, down to the minimum. The minimum may be 0:
a client of a group with no node is held by the front while the operator starts one, and is then seated on it.

## Problem

- A group runs a fixed `replicas`. A group used a few hours a day (an ETL window) costs its nodes all day.
- A group that is too small refuses at capacity (duckdb-acl spec 079) until someone scales it by hand.
- At 0 nodes nothing reports for the group - no Lease, no load - so nothing can tell the operator that a client is
  waiting (spec 003 left that question to this spec).

## Design

### The owner's model (2026-11-16)

- A group has `min` and `max` nodes. The **default group** (where an ungrouped principal is served, duckdb-acl spec 096)
  keeps `min >= 1`: it seats every new connection first (spec 002) and serves quack's discovery, so the entry never
  goes away. Every other group may go to 0.
- Example: an ETL client connects to its group, which has no node. The orchestrator starts one, and the client's
  session moves onto it. After every session of the group has ended (a timeout, a kill, the client closing it) the node
  stays a while, then drains and stops.

### The resource (`HugrNodeGroup`)

```yaml
spec:
  replicas: 3            # mode manual (today): the operator's number; mode auto: written by the autoscaler only
  scaling:
    mode: auto           # manual (the default) | auto
    min: 0               # 0..max; the default group's min is at least 1 (refused otherwise)
    max: 4               # <= the plan's maxNodes counted over every group at its max
    idle: 30m            # a node with no session for this long is drained and stopped (while above min)
    scaleUp:             # a new node when, on every placeable node, for `for` (a share in percent, 1..100):
      memoryUsed: 80     #   duckdb's working memory reaches this share of its memory_limit (the main signal), or
      tempUsed: 80       #   its spilled data reaches this share of max_temp_directory_size, or
      sessionsUsed: 90   #   the live sessions reach this share of the node's acl_max_sessions (duckdb-acl spec 044), or
      seatsUsed: 90      #   the quack seats used reach this share (duckdb-acl spec 079), or
      streamsWaiting: 1  #   this many streams wait for the stream budget (spec 080)
      for: 60s
    cooldown: 120s       # no second scaling decision for a group within this
status:
  scaling:
    pressureSince: ...   # every placeable node past a threshold since then (kept for the next leader)
    last: {at, from, to, reason}   # what the console shows (spec 003)
```

A duration is written as Kubernetes writes one (`60s`, `30m`); a share is a percent (a CRD has no plain float).
Unset is the default shown. There is no per-group wait (the owner): the front holds
a client of a group at 0 while a node is starting - the client's own timeout decides how long it waits.

CPU is no signal (the owner): duckdb takes every thread it is given, so a busy node always reads as saturated. Memory
is, in its two parts (the owner, 2026-11-18):

- **RAM**: the buffer manager's **working** memory against `memory_limit` - its memory in use less the cache it drops
  whenever it needs room (a persistent table's blocks it read, the file cache of remote reads; the owner, 2026-11-19:
  measured, a node that read a lot sits at its limit with nothing running, and would grow its group for good);
- **the spill**: what duckdb wrote to `temp_directory` (sorts, hash tables, joins too big for RAM) against
  `max_temp_directory_size`. A query that needs more fails there with RAM to spare - and past the volume's `sizeLimit`
  the kubelet evicts the pod with every session on it.

duckdb-acl's load report (its spec 175) gains `memory: {used, working, limit, temp_used, temp_limit, critical}` (the
buffer manager's `GetUsedMemory`, that less the `BASE_TABLE` / `EXTERNAL_FILE_CACHE` / `OBJECT_CACHE` tags, `GetMaxMemory`,
`GetUsedSwap` / `GetMaxSwap`; `temp_limit` null when no maximum is set - nothing to take a share of) beside the seats and
the streams, and the Lease's summary carries `memory-used` (the working memory's share) and `temp-used`. A new
node relieves new sessions only: a query already spilling stays where it runs.

**The spill's own volume** (the chart, hugr-node): a node's `temp_directory` is a volume of its own with a `sizeLimit`,
and the node sets `max_temp_directory_size` below it (90% by default), so duckdb refuses a query before the kubelet
evicts the pod. The two are one value in the chart, never set apart.

**At a critical level a node takes no new work** (the owner, 2026-11-18): past the working memory or `temp_used` at 95% (a
node setting, `acl_node_admit_memory`), the load report says `admit.new_session = false` and `admit.new_stream =
false` - the front seats a new client on another node of the group, and the queries already running keep what is
left. A group of one node then holds a new client until a second node is placeable (the growth signal is past its
threshold by then).

### Waking a group at 0: the front's demand

- The front seats a new connection on the default group; the node answers `wrong_resource_group` naming the group
  (`x-acl-resource-groups`, spec 002). The group has no placeable node.
- The front **creates or renews a Lease `hugr-demand-<group>`** in the platform's namespace (holder: the front's pod,
  duration 30 s, renewed every 10 s while it holds a client of the group). Leases are what the front already watches;
  no new port, no call to the operator. RBAC: the front may create / update Leases named `hugr-demand-*` only.
- The operator watches those Leases. A live demand Lease of an auto group below 1 node sets `replicas = max(1, min)`
  at once (no `for`, no cooldown: a client is waiting).
- The front holds the client (an HTTP / gRPC call kept open, nothing sent) while the group's node is starting, and
  seats it as soon as a node of the group is placeable (its Lease says `admit.new_session`). The client's own timeout
  ends the wait (quack's 30 s HTTP timeout); it reconnects, and the node is coming up meanwhile - the demand stays
  renewed while any of the front's replicas holds a client. A group the operator cannot start (past the plan, a pod
  that does not schedule) is answered as today (UNAVAILABLE / 503 `hugr: no node of group <g> can take a new session -
  retry`) once the operator marks it `Refused`.
- **The session moves**: the client's first answer comes from the group's node, so there is nothing to move on quack;
  on Flight the default group's node opened nothing (it refused the seat), and spec 002's re-seat carries the call.

### Growing under load

The operator's leader reads each auto group's Leases (spec 001's summary: memory used, sessions and their limit, seats left,
`admit`)
every 5 s, as it already aggregates the status. When every placeable node of the group passes a `scaleUp` threshold for `for`,
`replicas` grows by one, up to `max`, then waits `cooldown`. A node that is not yet placeable (starting) counts as
capacity on its way: no second step while one is starting.

### Shrinking: only an idle node stops

- duckdb-acl adds `sessions.idle_since` to `acl_node_load()`: the time the node's live sessions last fell to 0 (unix
  seconds), `null` while it has one. A reader's own cookie-less session is not counted live, but its end is a session's end: the
  console reads a node's report through the Flight Handshake `node-load` (no session; spec 012, the node serving it
  since hugr-node spec 043), not `platform.node_load` over SQL, or every view of a node would move its `idle_since`. hugr-node's heartbeat carries the load report as is, and the Lease's summary gains
  `idle-since`.
- An auto group above `min` with a node idle longer than `idle` loses one replica: the pod-deletion cost (spec 001)
  already makes the scale-down remove the pod with the fewest sessions; the costs become `-3` for a lost node, `-2` for
  an idle one, else by sessions as today. The pod drains (SIGTERM -> `acl_drain()`, spec 001) with no session to wait
  for, and stops.
- A busy node is never stopped to shrink a group: sessions are not killed. A group whose nodes all hold sessions stays
  at its size until they end.
- The last node of a group with `min: 0` stops the same way, once the group has had no session for `idle` and no
  demand Lease is live.
- **A race**: a client seated on a node the operator is removing meets its drain (duckdb-acl spec 066: `draining`) and
  is seated again by the front (spec 002), on another node or, at 0, after a new one starts.

### The console (spec 003)

The reserved `scaling: {mode, min, max, last}` API: an auto group's dialog edits `min`, `max`, `idle` and the
thresholds within the plan and never `replicas`; the fleet shows "stopped - starts on the first client" for a group at 0, and the
last decision with its reason. The plan's check counts an auto group at its `max`.

## Enforcement & security

- **The demand Lease starts nodes, nothing more**: a forged one (who may write Leases in the namespace may already do
  more) starts at most `max(1, min)` nodes of an existing auto group, within the plan; it never changes a policy, a
  placement or a route. The node still decides who it serves (duckdb-acl spec 096).
- **No session is killed by scaling**; a drain keeps established sessions (spec 066).
- **The plan bounds everything**: the sum of the groups' `max` within `maxNodes` is checked where the group is written.

### Built (stage 1, 2026-11-19)

- `pkg/autoscale.Decide` is the pure decision. Its inputs are the scaling with its defaults filled in, the replicas,
  the reported nodes, the demand (false until stage 2), the kept state and the clock. It answers the next replicas,
  a reason and the state to keep. In order:
  - the bounds: a min or max just written;
  - a client waiting at 0 nodes (stage 2);
  - nothing while a node is starting (asked for, not reported) or within the cooldown;
  - growth by one when every placeable node has been past a threshold for `for`;
  - shrinking by one when a node has been idle past `idle`.
- The default group's min of 1 is the autoscaler's floor, not the webhook's. The policy says which group is the
  default (the nodes report `group_default`), and the webhook cannot know it. A group whose nodes say so never goes
  below 1.
- The group's reconcile (the leader, every 5 s) runs the decision for an auto group that is not refused by the plan.
  - It writes `spec.replicas`, and the Deployment follows at the reconcile that write triggers.
  - It keeps `status.scaling`: `pressureSince`, so a new leader keeps the clock, and `last`.
  - The pod-deletion cost is -3 for a lost node and -2, in an auto group, for a node idle past `idle`.
- The heartbeat's summary, and so the Lease, carries:
  - `memory-used` (working / limit) and `temp-used`;
  - `seats-used`, `sessions-max` and `streams-waiting`;
  - `idle-since`.
  
  A share is left out when there is no limit, and a node of an older pin reports none of them, which reads as no
  pressure and never idle.
- The webhook counts an auto group at `max(max, replicas)`. A growth of its max, or a switch to auto, is a growth for
  the plan, and `min > max` is refused.
- The console answers the group's `scaling` (mode, min, max, `last`). A person's `replicas` on an auto group is
  refused with 409 `auto_scaled`; the dialog for min / max / idle is stage 3.

## Testing

- operator: the autoscaler's decision is pure (`pkg/autoscale`: from (spec, Leases, demand Leases, now) to the next
  replicas and the reason) - wake from 0 on demand, no step while one starts, growth on memory / spill / sessions / seats /
  streams for
  `for`,
  cooldown, shrink only an idle node above min, the last node of a min-0 group after `idle` with no demand, the
  default group's min >= 1 refused;
- front: the demand Lease written while a client of a 0-node group is held, renewed, released; the hold until the
  client's timeout or a `Refused` group; the seat once a node is placeable;
- duckdb-acl: the load report's `memory.temp_used` / `temp_limit` against a query that spills under a small
  `max_temp_directory_size`; `admit` false past the critical level, true again below it;
- e2e (kind / minikube demo): an ETL group with `min: 0` - a client connects, a node starts, the query runs; the
  client leaves, the node stops after `idle`; a second client during the stop is seated after a new node starts.

## Decisions (owner, 2026-11-16)

1. `idle` defaults to 30 minutes; no per-group wait - the client's own timeout decides.
2. Memory is the main signal to grow (duckdb takes every thread, so CPU always reads saturated), with the node's session
   limit, the seats and the stream queue beside it. Memory is RAM and the spill to `temp_directory` alike (2026-11-18),
   and a node past 95% of either takes no new session or stream.
4. An administrator's role belongs to a group whose nodes attach every source (hugr-node spec 035).
3. The front's demand is a Lease.

## Alternatives considered

- **The Kubernetes HPA / KEDA.** Neither scales to 0 from a client the front holds, and neither knows a session must
  never be killed; the decision needs our seats and idle times.
- **Kill idle sessions to shrink.** Rejected by the owner's model: a node stops after its sessions end.
