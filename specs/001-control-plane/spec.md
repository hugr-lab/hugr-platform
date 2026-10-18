# Spec 001: the operator - node groups, a node's entry, the fleet's state

- **Status**: accepted
- **Date**: 2026-10-18 (supersedes the draft of 2026-10-17, a service with its own database). Amended the same day:
  the pools are named in SQL.

## Summary

The platform's control part is a Kubernetes operator built on controller-runtime, with no database of its own. It
does four things:

- **It keeps each node group's pods** as the policy catalog says. How many nodes of which size a group has is a
  cluster profile item, written in SQL like the rest of the profile. The operator makes Kubernetes match it.
- **It answers a node's entry.** The node presents its ServiceAccount token and gets its bootstrap document.
- **It records the heartbeat.** A lost node is an expired Lease, and a group's convergence is in its status.
- **It is a client of the nodes with an identity of its own.** It reads, through SQL like any principal, the items
  that name the pools and the nodes' load. It writes nothing into the policy.

It decides nothing for a node, and it holds no profile, policy or secret. The front that routes clients to the
nodes (spec 002) and the web console (spec 003) come from the same repository and are specified separately.

Shared logic sits in Go packages with no Kubernetes dependency: the node protocol (the document, the heartbeat) and
the expansion of a pool into its pieces. A bare-metal service without Kubernetes comes only when one is needed; until
then hugr-node spec 006's static document covers it.

## Problem

- **Nobody deploys or scales the nodes.** A group of nodes is hand-written Kubernetes YAML.
- **Nobody answers the entry.** hugr-node fetches its bootstrap document from `GET <control plane>/v1/bootstrap`, and
  only a 30-line python fake in hugr-node's e2e serves it.
- **Nobody sees the fleet.** An operator cannot tell which nodes run, which profile version each has applied, which
  need a restart and which are gone.

## Design

### Who decides what

| what | where | who writes it |
| --- | --- | --- |
| a resource group, its limits, which roles land on it (duckdb-acl specs 085, 096) | the policy | the policy administrator: `CREATE RESOURCE GROUP`, `GRANT … TO ROLE` |
| how many nodes a group has, and of which size | the cluster profile, an item of kind `nodes` (hugr-node) | whoever holds the `cluster` bundle: `ACL CLUSTER SET NODES etl COUNT 3 SIZE 'large'` |
| what a size is in Kubernetes (requests, the VM pool, tolerations, the temp volume) and the plan's bounds | `HugrPlatform` | the installation (in Azure, the Managed Application's plan) |
| the pods, their Services, their status | `HugrNodeGroup` and the objects it owns | the operator, nobody else |

### The resources (`platform.hugr.io/v1alpha1`, namespaced)

**`HugrPlatform`** - one per namespace, the installation's: the pointers every node needs, the sizes, the plan's
bounds and the first group. No field holds a credential.

```yaml
apiVersion: platform.hugr.io/v1alpha1
kind: HugrPlatform
metadata: {name: hugr, namespace: hugr}
spec:
  node:
    image: ghcr.io/hugr-lab/hugr-node:v0.1.0
  idp:                                     # the organisation's IdP: users, nodes, the operator, tresor and kista trust it
    issuer: "https://login.microsoftonline.com/<tenant>/v2.0"
  tresor: {host: tresor.example:443, scope: "api://tresor/.default"}   # a product of its own, reached directly
  operator:
    identity: {clientID: "<the operator's workload identity>"}         # federated from its ServiceAccount
  repository: {name: hugr, prefix: "https://kista.example/<tenant>/<channel>"}
  catalog: {secret: hugr_policy_catalog, schema: acl}   # the defaults: the reserved secret's type is the catalog's
  caBundle: {configMapRef: {name: hugr-ca, key: ca.crt}} # optional: HUGR_CA_FILE
  sizes:                                   # what a size named in SQL means in this cluster
    large:
      resources: {requests: {cpu: "16", memory: 128Gi}, limits: {memory: 128Gi}}
      scheduling: {nodeSelector: {agentpool: e16ds}, tolerations: [...]}   # one pod per VM of the pool
      temp: {medium: local, size: 600Gi}   # the spill volume: the VM's local NVMe / temp disk (emptyDir)
  plan: {maxNodes: 12, sizes: [large]}     # what the operator will run, whatever SQL asks
  initialGroup: {name: general, nodes: 2, size: large}   # until the profile names it: somebody must read SQL
  groupIdentities:                         # the workload identity of each group's ServiceAccount (Azure: a managed
    general: {clientID: "<…>"}             # identity per group, made by the deployment)
    etl: {clientID: "<…>"}
status:
  conditions: [...]                        # Ready, PolicyAccess, and refusals: a size or a count past the plan
```

**`HugrNodeGroup`** - one pool, **written by the operator only**. It is created when the profile names a group, and
its `spec` mirrors the item. People read it with `kubectl`, and a hand edit is reconciled away.

```yaml
apiVersion: platform.hugr.io/v1alpha1
kind: HugrNodeGroup
metadata: {name: etl, namespace: hugr}
spec: {platform: hugr, nodes: 3, size: large, source: "profile v12"}   # or "initialGroup"
status:
  replicas: 3
  readyReplicas: 3
  profile: {target: 12, applied: 2, needsRestart: ["hugr-node-etl-7f9c-x2k8p"]}
  lost: []
  resourceGroupKnown: true                 # what the nodes report: does the policy have this group
  conditions: [...]
```

### Where a group's nodes come from

- **The item.** `ACL CLUSTER SET NODES <group> COUNT <n> SIZE '<size>'` is an item of the cluster profile.
  - It is new in hugr-node's cluster part, kind `nodes`, one per group, and it is the `cluster` bundle's.
  - At write, hugr-node refuses a group the policy has no resource group for, so a pool never runs for nobody.
  - `COUNT 0` keeps the group with no nodes. `ACL CLUSTER REMOVE NODES <group>` is the explicit end, and the operator
    then drains and removes the pool. Nothing goes by itself.
- **How the operator hears of a change.** The heartbeat already carries the profile version (`config.target`).
  - When a node reports a version the operator has not read, the operator reads `platform.cluster_items` through a
    ready node and reconciles. It does the same at start and every 5 minutes, in case a heartbeat was missed.
  - A change takes at most a heartbeat interval plus a reconcile. Provisioning a VM then takes minutes, so SQL
    records an intent and the status shows the fact.
- **The first group.** No node can read SQL before one runs, so `HugrPlatform.initialGroup` starts the first pool. As
  soon as the profile has a `nodes` item for that group, the item decides.
- **The plan's bounds.** A size the plan does not list, or a total past `maxNodes`, is not run. The platform's status
  says which item and why, and the pools already running are kept.

### A change of a pool: checked when written, applied after

- **Checked when written, in the write's own transaction.**
  - A profile write in hugr-node already applies a hot change before its commit, and a failure there rolls the write
    back (spec 093). For a `nodes` item that step is `POST /v1/pools/check`, with the node's own token, the one its
    heartbeat carries.
  - The operator answers whether the change fits: the size is known, and the plan's sizes and `maxNodes` hold, with
    this change counted across every group. It also refuses a change to a group being removed. It answers with what
    it will do (`+2 nodes`) or with a refusal and its reason.
  - **A refusal, or an operator that does not answer, fails the statement with the reason.** The write is rolled
    back, so the catalog never shows the item, the profile version does not move, and no node sees it.
  - The check judges feasibility, not rights. Whether the principal may change pools is the node's ACL (the
    `cluster` bundle) before the check is asked.
  - `ACL CLUSTER CHECK NODES etl COUNT 5 SIZE 'large'` asks the same question and writes nothing, like
    `platform.check_grant` (duckdb-acl spec 122).
- **The check reserves nothing.** It only says the change is allowed. The operator applies only what the catalog
  holds after the commit, so a check that passed before a commit that failed leaves no trace.
- **Applied after, and re-checked.** The operator judges every item again when it applies it.
  - Two writes may each pass alone and exceed the plan together.
  - An item may arrive through `ACL NATIVE`, which is break-glass and stays open.

  An item that does not fit is `refused`, with its reason; the pools already running are kept.
- **The state of a pool**, per group:
  - what is wanted: the count, the size, and the profile version that wrote it;
  - how many nodes are ready;
  - one of the states below, since when, with a message.

  | state | what it means |
  | --- | --- |
  | `converged` | the pool matches the item |
  | `scaling` | 3 of 5 ready |
  | `waiting_capacity` | pods Pending, with the scheduler's reason: a quota, a VM size not in the region |
  | `draining` | a scale-down's pod, with its sessions left |
  | `failed` | an image pull, a crash loop |
  | `refused` | the item did not pass the check when it was applied |

- **A write and its result are joined by the profile version.** `SET NODES` answers the version it wrote, and a
  converged pool reports the version it has applied.
- **Where the state is seen.**
  - In SQL, hugr-node's view `platform.pools` reads `GET /v1/pools` from the operator when queried. Nothing is stored
    in the policy.
  - The console (spec 003) shows the same.
  - In Kubernetes, the `HugrNodeGroup` status and its Events show it.
- **One trace.** The write's `traceparent` goes into the check and into the reconcile it causes.

### What the operator builds for a group

The leader reconciles each `HugrNodeGroup` into the following objects, owned by it and recreated when they differ:

- **A ServiceAccount** `hugr-node-<group>`. The group is the identity: a node is of the group whose ServiceAccount it
  runs as. It is annotated with the group's workload identity.
- **A Deployment** `hugr-node-<group>`, not a StatefulSet: scaling down removes the idlest pod, not the
  highest-numbered one. Its pods get:
  - the image, and the size's resources and scheduling;
  - an env with no credential in it:
    - `HUGR_ORCHESTRATOR`: the operator's in-cluster URL;
    - `HUGR_NODE_LABEL`: the pod's name, through the Downward API. It becomes duckdb-acl's `acl_node_label`
      (spec 134 there), the name the node's sessions carry for the front;
    - `HUGR_FLIGHT` / `HUGR_QUACK`;
    - `HUGR_DRAIN_DEADLINE`;
    - `HUGR_CA_FILE`;
    - the engine's settings that follow from the pod, set by the bootstrap before the lock:
      - `HUGR_MEMORY_LIMIT`: 80% of the pod's memory limit, DuckDB's own ratio of the machine;
      - `HUGR_THREADS`: the pod's CPU;
      - `HUGR_TEMP_DIRECTORY`: the spill volume's mount;
      - `HUGR_MAX_TEMP_SIZE`: 90% of that volume.

      An explicit profile item for the same setting wins.
  - **The spill volume** of the size, mounted at `HUGR_TEMP_DIRECTORY`.
    - When memory runs out, DuckDB spills to `temp_directory`, by default up to 90% of the free space.
    - In a pod that space is the node's ephemeral storage, and past its limit kubelet evicts the pod: every session
      on it dies at once.
    - With `max_temp_directory_size` under the volume's size, an overflow fails the one query instead.
  - two projected ServiceAccount tokens:
    - one with the audience `hugr-platform`, for the entry and the heartbeat;
    - one with the IdP's federation audience (`api://AzureADTokenExchange` in Entra). It is the client assertion with
      which the node gets the IdP's token for tresor (FLOW `federated`), as the group's workload identity.
  - the doors' TLS secret mounted;
  - `terminationGracePeriodSeconds = drainSeconds + 30` (`drainSeconds` 300 by default);
  - a readiness probe on the Flight port;
  - rolling updates with `maxSurge: 1, maxUnavailable: 0`, so that new nodes come up before old ones drain.
- **A headless Service** `hugr-node-<group>`. Its EndpointSlice is the front's node list (spec 002): pod name, address,
  and `ready` / `serving`.
- **A PodDisruptionBudget** with `maxUnavailable: 1`.
- **A NetworkPolicy**: the doors' ports accept the front and the operator only. Egress is open, because the nodes
  reach the sources, the repository, tresor and the IdP.
- **The pod-deletion cost.** The leader writes `controller.kubernetes.io/pod-deletion-cost` on each pod from its live
  sessions in the last heartbeat. A scale-down removes the pod with the fewest sessions, and that pod drains.

### A node's entry

1. The node starts with `HUGR_ORCHESTRATOR` and the projected token only.
2. `GET /v1/bootstrap` with `Authorization: Bearer <token>`.
3. The operator sends a **TokenReview** with the audience `hugr-platform`.
   - The answer names the ServiceAccount `system:serviceaccount:<ns>:hugr-node-<group>` and, since the token is
     bound, the pod's name and uid (`authentication.kubernetes.io/pod-name`, `pod-uid`).
   - The ServiceAccount must be one the operator owns, labelled with its group. The pod must exist, run as that
     account and belong to the group's Deployment.
   - Anything else is a 403, logged with the reason and never with the token.
4. **The document**, version 2. It holds pointers and names only. hugr-node takes it in its own spec: the
   orchestrator comes first, and spec 006's order stays for a deployment without one.

   ```json
   {"version": 2,
    "node_group": "etl", "node_label": "hugr-node-etl-7f9c-x2k8p",
    "tresor": {"host": "tresor.example:443",
               "identity": {"issuer": "https://login.microsoftonline.com/<tenant>/v2.0",
                            "client_id": "<the group's workload identity>", "scope": "api://tresor/.default"}},
    "repository": {"name": "hugr", "prefix": "https://kista.example/<tenant>/<channel>"},
    "catalog": {"secret": "hugr_policy_catalog", "schema": "acl"},
    "heartbeat": {"interval_seconds": 15}}
   ```

   The node's identity in tresor, and everything after it, is hugr-node spec 006's: tresor, then the repository, then
   the catalog by its secret, then the profile, then lock, then the doors.

### The heartbeat and the fleet's state

- **The call.** `POST /v1/heartbeat` every `interval_seconds`, with the same token, verified the same way. The body
  is the contract of the 2026-10-17 draft:
  - `node_id`;
  - `image`, `duckdb`;
  - `config`: `target`, `applied`, `needs_restart`, `pending`;
  - `drift`;
  - `load`: `acl_node_load()`.

  The limit is 256 KiB, and unknown fields are ignored.
- **The `node_id` is the token's pod name.** A body that names another pod is refused with 409. A node can only speak
  for itself.
- **The Lease.** The operator renews a Lease named after the pod (`coordination.k8s.io/v1`). Its holder is the pod
  and its duration is 3 × the interval. The pod owns it, so it goes with the pod. A lost node is an expired Lease;
  nothing keeps a timer.
- **The Lease's annotations** carry a small summary of the report: profile `target` / `applied`, `needs_restart`,
  draining, sessions and seats left. That is what the front and the console read (specs 002 and 003). The full report
  is not stored; the metrics travel through acl_otel when a receiver is set.
- **The group's status** is aggregated by the leader from its pods' Leases, at most every 5 s:
  - the target (the highest any live node reports) and how many have applied it;
  - who needs a restart;
  - who is lost;
  - whether the nodes know the resource group.
- **Load.** For 1000 nodes at 15 s that is about 67 Lease updates a second, the order of kubelet's own node Leases.

### The operator as a client of the nodes (owner, 2026-10-18)

The operator talks to the nodes the way any principal does: Flight SQL, its own bearer token, and the ACL deciding.

- **Its identity** is a workload identity federated into the organisation's IdP, the same mechanism as the nodes'.
  - The operator's ServiceAccount token is the client assertion; in Azure it is a user-assigned managed identity
    with a federated credential for that ServiceAccount.
  - The nodes already trust the IdP. The policy gets a CLIENT for the operator (its `azp`), whose own mapping gives
    the role `hugr_operator` (duckdb-acl spec 095: a privileged role is reached only through a client's own mapping).
  - Until that CLIENT exists, the operator's reads are refused and the platform's condition `PolicyAccess` says so.
    The first group still starts from `initialGroup`.
- **Its rights are reads only**: the `observe` bundle (the load report and the sessions) and a point grant on the
  view `platform.cluster_items` (duckdb-acl spec 117: a point grant on one view, written by `passthrough`). It holds
  neither `cluster` nor `policy`, and writes nothing into the policy.
- **The installation writes these, once, as a `passthrough` administrator:**
  1. the role `hugr_operator`;
  2. the CLIENT for the operator's identity, with its mapping;
  3. `GRANT ADMIN observe TO ROLE hugr_operator`;
  4. `GRANT VIEW platform.cluster_items TO ROLE hugr_operator`;
  5. the first group's resource group: `CREATE RESOURCE GROUP general … DEFAULT`;
  6. the integrations' receivers (decision 4).
- **Where it connects**: directly to one ready node, in the cluster, with the group's TLS name. The front is for
  clients.
- **Whose identity a call carries** is decided by who started it.
  - **The operator's own** carries the technical calls nobody asked for: reading the items, the nodes' load and
    sessions, checks. Each carries the reconcile run's trace, so the node's audit shows `hugr_operator` and joins the
    run.
  - **A person's** carries what a person does.
  - **No identity at all** is needed for what the console shows from the fleet's state (Leases, the groups' status):
    that is Kubernetes, read as the operator, with no node called.
- **A person's actions are the person's.** What an administrator does in the console (drain a node, kill a session)
  goes to the node with that administrator's token. The node judges it and the audit names that person, not the
  operator (spec 003).
  - **No session per action.** Flight SQL authenticates each call, and an acl session is sticky through its cookie
    (duckdb-acl spec 050). The operator connects to nodes directly, not through the front, so it keeps the stickiness
    itself.
    - The pool holds one gRPC channel per node, and one session cookie per node and principal. The principal is the
      operator itself, or a person signed in to the console.
    - The first call opens the session, with its connection, its open event and tresor's observer; every later call
      of that principal reuses it.
    - Each call still carries the bearer and is verified. Under the `connect` binding (spec 059 there) a past `exp`
      is forgiven on a live session, while the signature, the issuer, the audience and the same-principal check are
      not.
    - A person's sessions end with their console sign-out (`CloseSession` on each node holding one), or at the node's
      idle timeout. The operator's own last as long as the operator.
  - **One trace.** The console starts a span for the action, and the operator passes its `traceparent` and
    `x-correlation-id` as Flight headers (spec 069 there). The node's audit event then carries both the person and
    the trace, and acl_otel exports it, so the node's decision joins the console's span.
  - **Why not the operator's identity plus a trace.**
    - The node would judge the operator's rights, so the console would have to decide by itself which person may do
      what (observe, operate, cluster, policy). Otherwise everyone in the console would hold the operator's rights.
    - That makes two judges. With the person's token the ACL stays the only one, and a refusal comes from the node
      with its reason.

### Replicas and what stays outside

- **Replicas.** Any operator replica answers HTTP (`/v1/bootstrap`, `/v1/heartbeat`, `/v1/pools/check`, `/v1/pools`,
  `/healthz`, `/readyz`); leader election picks the one that reconciles.
- **Not in this spec:** packaging - the Azure Managed Application (it also makes the workload identities of the
  operator and of each group in the plan) and a Helm chart for private Kubernetes. A spec of their own.

## Security

- **No credential at rest.** Neither resource has a field for one, and the document carries names and URLs.
  - The doors' TLS key is a Kubernetes Secret, which cert-manager or the cluster's administrator provides. The
    operator only names it.
  - The operator logs a refusal with its reason, the ServiceAccount and the pod, never a token.
- **SQL decides how many nodes, the installation decides the most.** A principal with the `cluster` bundle can grow a
  pool, which costs money. The plan's bounds are the installation's, out of SQL's reach, and the operator never
  passes them.
- **Who may call.**
  - `/v1/bootstrap`, `/v1/heartbeat`, `/v1/pools/check` and `/v1/pools` answer a pod of a group, bound by
    TokenReview; a group's pointers go only to that group's pods. A node asks about pools only for a statement its ACL
    already admitted.
  - `/healthz` and `/readyz` are unauthenticated and answer nothing about the fleet.
  - The console's API is spec 003's.
- **The operator's rights.**
  - In its namespace: ServiceAccounts, Deployments, Services, PodDisruptionBudgets, NetworkPolicies, Leases, its
    resources and their status. It may read Pods and EndpointSlices and annotate Pods.
  - Cluster-wide: only `tokenreviews/create` (`system:auth-delegator`).
  - In the policy: reads only (`observe`, the items view).
  - Its egress goes to the API server, to the nodes' doors and to the IdP (its own token).

## Testing

- **Unit tests**:
  - a pool's expansion into its objects (golden YAML), including the derived settings: the memory, the threads, the
    temp volume and its 90%;
  - the document built from a platform and a group;
  - TokenReview answers: a foreign ServiceAccount, an unbound token, a pod of another group, a missing pod;
  - the 409 for a `node_id` not the token's pod;
  - status aggregation from Leases;
  - the profile's `nodes` items into pools:
    - the initial group until an item names it;
    - a size or a total past the plan refused, with what already runs kept;
    - `COUNT 0` and `REMOVE` drain;
    - a hand edit of a `HugrNodeGroup` reconciled away;
  - `/v1/pools/check`:
    - a size not in the plan, and a total past `maxNodes` counted across groups, refused with the reason;
    - a fitting change answered with what it will do;
  - the apply-time re-check: two changes that fit alone and not together, the second `refused`;
  - the pool states from pods: Pending with a reason, ready counts, a draining pod, a crash loop;
  - the applied version reported once converged.
- **envtest** (controller-runtime's API server and etcd, no kubelet):
  - reconcile creates, updates and recreates the objects;
  - a changed item changes the Deployment;
  - a Lease that stops being renewed makes a node lost;
  - the pod-deletion cost follows the reported sessions.
- **kind e2e**: the operator, hugr-node pods, tresor-server and a repository. It comes with hugr-node's spec (the
  document v2, the `nodes` item, the derived settings) and proves:
  - the entry;
  - that `ACL CLUSTER SET NODES` grows a pool, through the heartbeat;
  - that a refused check leaves the catalog unchanged;
  - that `platform.pools` shows `scaling` and then `converged` at the version written;
  - that the operator's token is admitted for its reads and refused for a write.
- **CI**: `go test ./...`, `go vet`, `staticcheck`, and the envtest binaries pinned. The image is distroless at
  `ghcr.io/hugr-lab/hugr-platform`, released like hugr-node's.

## Decisions (owner, 2026-10-18)

| # | question | decision |
| --- | --- | --- |
| 1 | The operator and the policy | a client of the nodes with its own identity from the start (the console needs it too), reading only: `observe` and the items view. No write into the policy, so no acl change |
| 2 | How a node logs in to tresor | a workload identity per group, federated into the organisation's IdP (Azure: a managed identity with a federated credential for the group's ServiceAccount). tresor-server and kista are products of their own, used by the organisation's people as well, so they trust the organisation's IdP, never the cluster |
| 3 | The doors' certificate inside the cluster | one certificate per group, its SAN `hugr-node-<group>.<ns>.svc`; the front and the operator dial a pod's address with that name as the TLS server name. cert-manager when the cluster has it, otherwise a Secret the administrator provides |
| 4 | The integrations' endpoints (where the telemetry and the OpenLineage events go) | a data path, so the installation (a `passthrough` administrator) sets the receivers; the `cluster` bundle's allowlist does not grow |
| 5 | Where a pool is decided | in SQL: a `nodes` item of the cluster profile names a group's count and size; the operator makes Kubernetes match, inside the plan's bounds. What a size is in Kubernetes is the installation's |
| 7 | Knowing a pool change took | checked when written, in the write's transaction, by the operator (a refusal or an unanswered check rolls the write back, so the catalog never shows it); applied after and re-checked; the pool's state and the applied profile version are read from the operator through `platform.pools` |
| 6 | The engine against the pod | memory, threads and the spill volume follow from the pod (env, at bootstrap): `max_temp_directory_size` under the volume, so an overflow fails a query instead of evicting the pod |

Still to verify:

- Outside Azure, the IdP's federation of Kubernetes ServiceAccount tokens (Keycloak's client authentication by a
  federated JWT). Without it, a node's or the operator's client secret would have to live somewhere, which the
  platform refuses.
- Whether DuckDB at our pin already reads the cgroup's memory and CPU. If it does, the env values only make explicit
  what it would choose anyway.

## Alternatives considered

- **A service with its own database** (the 2026-10-17 draft). Replaced: Kubernetes is the store, so there is nothing
  to migrate or back up, and Leases are what kubelet already uses for liveness.
- **The pool in the CRD, the resource group created from it.** Replaced the same day (decision 5).
  - The pool and its resource group would be two halves with one name in two places.
  - The administrator works in SQL for everything else of the platform: the policy, the profile, the platform
    catalog.
- **A StatefulSet per group.** Its scale-down removes the highest ordinal, whoever holds sessions there.
- **The node registering itself with the API server.** Refused: the node would need Kubernetes rights; with the
  heartbeat it needs none.
- **One core under two wrappers (operator and service) from the start.** Refused (owner, 2026-10-17): the operator is
  the product, and the shared part is a set of packages.

## Follow-ups

- **spec 002, the front**: routing by the label (duckdb-acl spec 134), the node list from EndpointSlices, placement
  by the Lease's summary.
- **spec 003, the console**: the shell with tresor-server's and kista's micro-frontends, and the fleet view.
- **hugr-node**:
  - the document v2 and the orchestrator-first entry;
  - `acl_node_label` from `HUGR_NODE_LABEL`;
  - the heartbeat sender;
  - the `nodes` item kind (`ACL CLUSTER SET | REMOVE | CHECK NODES`, refused for a group the policy lacks), whose
    pre-commit step is the operator's `/v1/pools/check`, so that a refusal rolls the write back;
  - the view `platform.pools`, read from the operator's `/v1/pools`;
  - the derived settings from env (`HUGR_MEMORY_LIMIT`, `HUGR_THREADS`, `HUGR_TEMP_DIRECTORY`,
    `HUGR_MAX_TEMP_SIZE`).
- **Packaging**: the Azure Managed Application (with the publisher's just-in-time access, never standing access) and
  a Helm chart.
- **Phase 2**: autoscaling from the nodes' load, and a group's idle stop.
