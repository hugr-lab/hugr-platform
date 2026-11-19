# Spec 011: a node group scales itself - from its load, down to zero

- **Status**: accepted (the owner, 2026-11-16); stage 1 built (the operator's decision, 2026-11-19), stage 2 built
  (the front's demand, 2026-11-19), stage 3 (the console's dialog) to come
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
    min: 0               # 0..max; the policy's default group keeps at least 1 whatever min says
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
  placement or a route. The node still decides who it serves (duckdb-acl spec 096). The front writes demand only for
  groups a node named to a verified token.
- **No session is killed by scaling**; a drain keeps established sessions (spec 066).
- **The plan bounds everything**: the sum of the groups' `max` within `maxNodes` is checked where the group is written.

### Built (stage 1, 2026-11-19)

- `pkg/autoscale.Decide` is the pure decision. Its input is one look at the group:
  - the replicas asked and the replicas the Deployment runs;
  - the group's pods (not going, not ended: an evicted pod is Failed until pod GC) and how many have not reported;
  - the reported nodes;
  - the demand (false until stage 2);
  - whether it is the policy's default group;
  - whether the plan admits a growth;
  - the kept state and the clock.

  It answers the next replicas, a reason and the state to keep. In order:
  - **The bounds.** A max just lowered is applied.
  - **The plan.** While the plan refuses the group, what it asked past what runs goes back, never below its floor,
    which it asks the way a manual group asks its replicas. A revert below the floor would be raised at the next
    look, admitted and refused again: a flap (the re-review). The group does not grow, is not raised to its min and
    is not woken, but it may still shrink. When the plan admits the group, a min just raised is applied. `Running` is
    the plan's own direct read.
  - **A waiting client.** A client waiting at 0 nodes wakes the group (stage 2).
  - **The cooldown.** Nothing happens within it. The change's time is written in the same patch as the replicas, as
    the annotation `platform.hugr.io/scaled-at`. A look that reads the new replicas therefore reads the cooldown too,
    even when the status patch after it is lost to a leader's end or the cache is behind. Otherwise two pods would go
    for one idle node (the reviews).
  - **The Deployment behind.** Nothing happens while the Deployment is not yet asked the replicas last written: its
    apply fails, or the look is the one that applies them (`Running` read before the apply). Its effect is not seen
    yet, and a second shrink would remove a busy node for the same idle one (the re-review). A surge pod, a pod that
    does not schedule or a quota changes nothing the Deployment is asked, so none of them freezes the group (a surge pod
    holds only the shrink, below).
  - **Growth.** One more node when every placeable node has been past a threshold for `for`. A pod not created yet,
    or not reported, holds the growth: it is a node on its way, or one that does not come up (a surge pod the
    cluster cannot schedule, a quota), and more of the same would help nothing. Within what the Deployment is asked it
    never holds the shrink, since a scale-down takes an unready pod first.
  - **Shrinking.** One fewer node when a node has been idle past `idle`. A shrink waits while there are more pods
    than the Deployment is asked: a rollout's surge pod, or a ReplicaSet behind. A Deployment scaled down
    mid-rollout splits the decrease between its ReplicaSets, and the deletion cost ranks pods only within one, so a
    busy old pod could go while the idle node stays (the re-review). A stuck rollout holds the shrink too; that is
    visible, and better than stopping busy nodes.
- **The floor.**
  - The policy's default group never goes below 1. It is the default when the platform's `firstPolicy.defaultGroup`
    names its resource group, or when a node's Lease says `group-default`. A single heartbeat during a catalog hiccup
    reads not-default, so the platform's word stands beside the Lease's. The webhook cannot judge this, because the
    policy is the node's.
  - Until stage 2 nothing wakes a group at 0, so every auto group keeps at least one node, whatever its min (the
    review). Stage 2 lifts that (`WakeOnDemand`).
- The group's reconcile (the leader, every 5 s) runs the decision for every auto group, refused or not.
  - It writes `spec.replicas` with an optimistic lock, so a person's change since the read is never written over,
    and the Deployment follows at the reconcile that write triggers.
  - A write that fails is logged and left to the next look. The rest of the status is written all the same.
  - It keeps `status.scaling`: `pressureSince`, so a new leader keeps the clock, and `last`. A manual group keeps no
    pressure clock.
  - The pod-deletion cost is -3 for a lost node and -2, in an auto group, for a node idle past `idle`.
- Known: the decision reads `sessions` / `idle-since` from a Lease up to one heartbeat old. A client seated on the
  idle node in that gap meets its drain, which keeps it for `drainSeconds`; the front seats it again after that. The
  console's statement calls (a node's sessions, the policy pages) are per-call sessions, so they go to a busy node
  first (spec 012).
- The heartbeat's summary, and so the Lease, carries:
  - `memory-used` (working / limit) and `temp-used`;
  - `seats-used`, `sessions-max` and `streams-waiting`;
  - `idle-since`.
  
  A share is left out when there is no limit, and a node of an older pin reports none of them, which reads as no
  pressure and never idle.
- The webhook counts an auto group at `max(max, replicas)`. A growth of its max, or a switch to auto, is a growth for
  the plan. It refuses `min > max` and a cooldown under 30 s: a step's effect, first the Deployment and then its
  ReplicaSet, is seen a few 5 s looks later.
### Built (stage 2, 2026-11-19): waking a group at 0

- **The demand Lease** (`pkg/nodeproto`, `DemandLeaseName`).
  - Its name is `hugr-demand-` and the first 16 hex of the resource group's SHA-256, because a resource group is a
    policy's name and not a Kubernetes one.
  - Its label `platform.hugr.io/demand`, not the nodes' managed-by, keeps it out of the front's fleet.
  - It carries the group in its annotation, and the operator's answer `platform.hugr.io/starting: <HugrNodeGroup>`.
- **The front** (`internal/front/demand`). When no node is placeable for a seat's *named* groups, both doors hold
  the client. Named groups are those a node's `wrong_resource_group` refusal named, which it does only for a token
  it verified, so an unauthenticated caller wakes nothing.
  - It writes the demand Lease at once: created, then merge-patched on its spec only, so the operator's answer stays.
    A new demand on a Lease that expired drops an answer left from before: the group may be manual by now, or gone,
    and a client would otherwise be held for nothing (the review).
  - It renews the Lease every 10 s while any of its clients waits for the group, and lets it expire after that
    (30 s).
  - It reads the operator's answer once per group every 2 s, however many clients wait. Its API client's limit is
    raised to 50 QPS / 100 burst, which the informers share.
  - It waits `noNodeWait` as before, then for as long as the operator answers "starting", up to `--demand-hold`
    (5 min) or the client's own end. On a cluster that adds a VM for the node (cluster-autoscaler: often 3-6 min) a
    wake can take longer than that: raise `--demand-hold` there. That bound is one deadline per connection, so a connection re-seated after a
    node came up and refused it again is not held anew.
  - When no answer comes within 15 s of the demand's first write (the operator looks every 5 s), the client hears
    "no node" as before. That is a manual group, a refused one, a full one, or no group of the platform. It now waits
    up to 15 s, not `noNodeWait`.
  - Its Role gains `create` and `patch` on Leases, because a name derived from a group cannot be narrowed by
    `resourceNames`. A ValidatingAdmissionPolicy keeps it to its demand Leases: a name `hugr-demand-*` with the
    demand label, never the operator's leader Lease nor a node's (the review). It is
    `config/policy/front-leases.yaml`, and the chart ships it wherever the cluster has the API (Kubernetes 1.30+),
    not as an option. At start the front checks the policy is in force: a dry-run of a node's Lease must be refused,
    and the log says so when it is not (an older cluster, a chart rendered without the API).
- **The operator.**
  - The group's reconcile reads the demand Lease of the group's resource group; a renewed one is the decision's
    demand.
  - A group at 0 is woken to `max(1, min)` at once (no `for`, no cooldown).
  - The last node of a min-0 group stops only with no live demand.
  - It answers "starting" only while a node is on its way for the demand, with the plan admitting the group. A node
    is on its way when none of the group's nodes admits (Ready, admitting, neither lost nor draining) and a pod is
    either not here yet, not reported, or not admitting within 5 minutes of its first heartbeat. The window counts
    from the node's Lease, not its pod, because a pod that waited for a VM and an image pull is new all the same. A node that reported may need its
    readiness and a heartbeat more before it admits (the re-review).
  - It never answers for a group whose nodes run and admit nobody (at capacity, past the critical level, at its
    max), so that client hears "no node" in 15 s and is not held 5 min (the review).
  - The front counts the answer's 15 s from the last time it saw "starting", too.
  - A group clears only its own answer, so two groups serving one resource group do not undo each other's.
  - Every look also drops a "starting" that is no longer anybody's (the re-review):
    - one naming this group on another resource group's demand, or while it is not auto or is going;
    - one naming a group that is gone, on any demand: a deleted group has no finalizer, so its own look never
      comes;
    - on its own resource group's demand, one naming a group that is not auto or serves another resource group.
  - The answer of a new demand is dropped by its first write the API takes, not only by its first try.
  - The policy's default group keeps its floor of 1. The resource group it served as the default is kept in
    `status.scaling.defaultGroup`, because one heartbeat during a catalog hiccup says not-default. It is dropped once a
    live node of another resource group reports itself the default (`ALTER RESOURCE GROUP … SET DEFAULT`), never on
    `firstPolicy`'s word, which is the bootstrap's only. A default group at 0
    would leave no node to name any group to wake. Known: a policy with no default group whose every auto group
    allows min 0 can reach 0 everywhere, and then nothing names a group to wake. The first policy (hugr-node spec
    015) writes a default group.
  - `WakeOnDemand` is on, so an auto group may go to its min of 0.
- **Tests.**
  - `internal/front/demand` (fake clientset, `-race`):
    - a placeable node writes no demand;
    - an unanswered demand ends the hold after the answer's wait;
    - an ungrouped seat writes none;
    - a client held while "starting" is seated once the node comes up, and the renew keeps the answer;
    - the client's end ends the hold;
    - holders share one renewer;
    - an old answer on an expired Lease is dropped;
    - a re-seat is not held past the connection's deadline.
  - The controller tests:
    - a renewed demand wakes an auto group at 0 and is answered, an expired one is not;
    - a min-0 group's last idle node stops;
    - a full group at its max is not answered;
    - another group's answer stays;
    - the default group keeps one node through a heartbeat that no longer says so.
  - The envtest: the front's Role reads the fleet and writes only Leases (create, patch).
  - The envtest, as the front's own account against a real API server: the policy refuses a patch of a node's Lease
    and of the leader Lease, a node Lease made by the front, and a demand Lease unmarked. The front's own demand code
    creates and renews its Lease.
  - The demo (kind): b at 0 waits until the demand the refused statement left has expired, is switched to auto with
    min 0, and stays at 0 with nobody waiting. analyst_b's statement through the front is then held, b's node starts
    for it (woken after the statement began), and the statement is answered there (the checker's `-timeout`).

- The console answers the group's `scaling` (mode, min, max, `last`). A person's `replicas` on an auto group is
  refused with 409 `auto_scaled`; the dialog for min / max / idle is stage 3.

## Testing

- operator: the autoscaler's decision is pure (`pkg/autoscale`: from (spec, Leases, demand Leases, now) to the next
  replicas and the reason) - wake from 0 on demand, no step while one starts, growth on memory / spill / sessions / seats /
  streams for
  `for`,
  cooldown, shrink only an idle node above min, the last node of a min-0 group after `idle` with no demand, the
  default group kept at 1;
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
