# Spec 001: the operator - node groups, a node's entry, the fleet's state

- **Status**: implemented, but for three parts that come with their consumers: the operator's client pool to the nodes
  (spec 003's console API is its first user), the kind e2e (the platform's demo, with hugr-node and the front), and
  the image's release workflow (with the first release; CI builds the image on every change)
- **Date**: 2026-10-18 (supersedes the draft of 2026-10-17, a service with its own database). Settled the same day,
  after weighing SQL as the master: the infrastructure is Kubernetes', and SQL keeps the policy and the profile.

## Summary

The platform's control part is a Kubernetes operator built on controller-runtime, with no database of its own. It does
three things:

- **It keeps each node group's pods.** A group is a custom resource that people edit with standard tooling: the
  console, `kubectl`, the Azure Managed Application, GitOps.
- **It answers a node's entry.** The node presents its ServiceAccount token and gets its bootstrap document.
- **It records the heartbeat.** A lost node is an expired Lease, and a group's convergence is in its status.

It decides nothing for a node, and it holds no profile, policy or secret. The front that routes clients to the
nodes (spec 002) and the web console (spec 003) come from the same repository and are specified separately.

Shared logic sits in Go packages with no Kubernetes dependency: the node protocol (the document, the heartbeat, the
Lease's summary, `pkg/nodeproto`) and the plan's judgement of groups (`pkg/plan`). A group's Kubernetes objects are
`internal/build`'s. A bare-metal service without Kubernetes comes only when one is needed; until
then hugr-node spec 006's static document covers it.

## Problem

- **Nobody deploys or scales the nodes.** A group of nodes is hand-written Kubernetes YAML.
- **Nobody answers the entry.** hugr-node fetches its bootstrap document from `GET <control plane>/v1/bootstrap`, and
  only a 30-line python fake in hugr-node's e2e serves it.
- **Nobody sees the fleet.** An operator cannot tell which nodes run, which profile version each has applied, which
  need a restart and which are gone.

## Design

### A group has three faces, joined by its name

| face | what it says | who writes it | judged where |
| --- | --- | --- | --- |
| `HugrNodeGroup` (Kubernetes) | how many nodes, of which size | the infrastructure's people: the console, `kubectl`, the Managed Application, GitOps | Kubernetes, with the operator's webhook |
| the resource group (duckdb-acl specs 085, 096) | whom the group's nodes serve (role → group) and the sessions' limits there | the policy administrator: `CREATE / ALTER / DROP RESOURCE GROUP`, `GRANT … TO ROLE` | acl, at every session's opening |
| the profile `IN GROUP g` (hugr-node specs 093, 096) | what a node of the group is: the engine's limits (memory, threads, temp), its extensions, its sources | whoever holds the `cluster` bundle: `ACL CLUSTER …` | the node, at its start and on its tick |

- **The name ties them.** A node belongs to the group its deployment names (`acl_node_group`, set from its
  `HugrNodeGroup`), and that name is the resource group's and the profile's scope.
- **The faces stay loosely coupled.** The operator writes nothing into the policy, and SQL does not drive
  Kubernetes.
- **A resource group is not CPU and memory.** It is placement plus the sessions' limits; the engine's limits are the
  profile's. The words of spec 085 stay as they are, and the documentation names the three faces explicitly.

### The resources (`platform.hugr.io/v1alpha1`, namespaced)

**`HugrPlatform`** - one per namespace, the installation's: the pointers every node needs, the sizes and the plan's
bounds. No field holds a credential.

```yaml
apiVersion: platform.hugr.io/v1alpha1
kind: HugrPlatform
metadata: {name: hugr, namespace: hugr}
spec:
  node:
    image: ghcr.io/hugr-lab/hugr-node:v0.1.0
  idp:                                     # the organisation's IdP: users, nodes, the operator, tresor and kista trust it
    issuer: "https://login.microsoftonline.com/<tenant>/v2.0"
    federationAudience: api://AzureADTokenExchange   # the default: the audience of a node's client assertion
  tresor: {host: tresor.example:443, scope: "api://tresor/.default"}   # a product of its own, reached directly
  operator:
    identity: {clientID: "<the operator's workload identity>"}         # federated from its ServiceAccount
  repository: {name: hugr, prefix: "https://kista.example/<tenant>/<channel>"}
  catalog: {secret: hugr_policy_catalog, schema: acl}   # the defaults: the reserved secret's type is the catalog's
  caBundle: {configMapRef: {name: hugr-ca, key: ca.crt}} # HUGR_CA_FILE: it REPLACES the node's system roots (hugr-node
                                           # spec 006), so it holds them plus the CA of the operator's certificate
  heartbeat: {intervalSeconds: 15}         # the default; a node is lost after three
  sizes:                                   # what a group's size means in this cluster
    large:
      resources: {requests: {cpu: "16", memory: 128Gi}, limits: {memory: 128Gi}}
      scheduling: {nodeSelector: {agentpool: e16ds}, tolerations: [...]}   # one pod per VM of the pool
      temp: {medium: local, size: 600Gi}   # the spill volume: the VM's local NVMe / temp disk (emptyDir)
  plan: {maxNodes: 12, sizes: [large]}     # what the operator will run (in Azure, the Managed Application's plan)
status:
  conditions: [...]                        # reserved: nothing writes them yet
```

**`HugrNodeGroup`** - one pool, edited by people.

```yaml
apiVersion: platform.hugr.io/v1alpha1
kind: HugrNodeGroup
metadata: {name: etl, namespace: hugr}
spec:
  platform: hugr
  replicas: 3
  size: large
  resourceGroup: etl                       # default: the group's name
  identity: {clientID: "<the group's workload identity>"}   # Azure: a managed identity per group, with a federated
                                           # credential for the group's ServiceAccount
  drainSeconds: 300                        # SIGTERM -> acl_drain; the pod's grace is drainSeconds + 30
  doors: {quack: true}                     # Flight is always open
  tls: {secretRef: {name: hugr-node-etl-tls}}
  restartedAt: ""                          # set to a new time to roll the group (see "Applying a change")
status:
  replicas: 3
  readyReplicas: 3
  profile: {target: 12, applied: 2, needsRestart: ["hugr-node-etl-7f9c-x2k8p"]}
  lost: []
  resourceGroupKnown: true                 # what the nodes report: does the policy have this group
  conditions: [...]                        # Ready, Refused (past the plan), Draining (its resource group is gone)
```

### Checking a change of a group

- **When the change is written.** The operator's validating webhook (`failurePolicy: Fail`) checks every create and
  update of a `HugrNodeGroup`:
  - the group's name is a DNS label of at most 36 characters, so that its pods' names (`hugr-node-<group>-<hash>-<id>`)
    are DNS labels too: a pod's name is the node's label (duckdb-acl spec 134), which the node refuses otherwise;
  - the size is one `HugrPlatform` lists;
  - the plan's sizes and `maxNodes` hold, summed over every group of the namespace with this change in it (one
    platform per namespace);
  - a change that does not grow a group (a scale-down, a restart, a label) passes even while the namespace is past
    the plan - it is how a group gets back within it - and so does a group being deleted;
  - the doors' certificate is a Secret named `hugr-node-…`: a group mounts no other Secret of the namespace (the
    operator's own serving key, a catalog's).

  A refusal reaches `kubectl apply`, the console or the Managed Application with its reason, and nothing is written.
  An operator that does not answer refuses the change too.
- **When it is reconciled, again.** Two changes may each pass alone and exceed the plan together. The controller
  re-checks every group, reading the Deployments from the API server: what runs counts as it runs (an orphaned
  Deployment included), and the growing groups, in the order of their creation, grow only while everything running
  plus their growth stays within `maxNodes` - so the reconcile never takes the namespace past the plan by itself. A
  scale-down frees its nodes once its Deployment runs fewer. A group past the plan keeps what already runs (its
  Deployment is left as it is, a restart included, but a scale-down is applied) and says `Refused` with the reason;
  the size's own checks (a memory limit, a CPU request, a spill volume) are judged here too, after a platform edit.
  Nothing that runs is taken away by itself.
- **The plan's bounds are the installation's**, in `HugrPlatform`, under Kubernetes' (and Azure's) access control.

### What the operator builds for a group

The leader reconciles each `HugrNodeGroup` into the following objects, owned by it and recreated when they differ:

- **A ServiceAccount** `hugr-node-<group>`. The group is the identity: a node is of the group whose ServiceAccount it
  runs as. It is annotated with the group's workload identity.
- **A Deployment** `hugr-node-<group>`, not a StatefulSet: scaling down removes the idlest pod, not the
  highest-numbered one. Its pods get:
  - the image, and the size's resources and scheduling;
  - an env with no credential in it (hugr-node spec 010 reads it):
    - `HUGR_ORCHESTRATOR`: the operator's in-cluster URL;
    - `HUGR_ORCHESTRATOR_TOKEN_FILE` and `HUGR_IDENTITY_ASSERTION_FILE`: the two projected tokens below;
    - `HUGR_FLIGHT` / `HUGR_QUACK`, and `HUGR_FLIGHT_CERT` / `HUGR_FLIGHT_KEY` (`HUGR_QUACK_CERT` / `_KEY`): the
      paths of the mounted doors' TLS secret;
    - `HUGR_DRAIN_DEADLINE`;
    - `HUGR_CA_FILE`;
    - the container's resources, through the Downward API with divisor 1: `HUGR_POD_MEMORY` (`limits.memory`, in
      bytes - the line past which the pod is OOM-killed) and `HUGR_POD_CPU` (`requests.cpu`, whole cores - what the
      pod is guaranteed; a size sets no CPU limit, which would only throttle);
    - `HUGR_TEMP_DIRECTORY` and `HUGR_TEMP_SIZE`: the spill volume's mount and its size in bytes.

    From these the node sets the engine's defaults before the lock:
    - `memory_limit` at 80% of the pod's memory, DuckDB's own ratio of the machine;
    - `threads` at the pod's CPU;
    - `max_temp_directory_size` at 90% of the volume.

    A profile item for the same setting (`IN GROUP g`) wins, but is cut to a ceiling and reported as drift. The
    ceilings are 90% of the pod's memory, its CPU, and 90% of the volume. The node's label (duckdb-acl spec 134
    `acl_node_label`, the name its sessions carry for the front) is the pod's name, and comes in the document.
  - **HOME** (`/var/lib/hugr-node`, where duckdb installs the extensions from the repository) on an emptyDir of
    2 GiB, and **the spill volume** of the size, mounted at `HUGR_TEMP_DIRECTORY` inside it.
    - When memory runs out, DuckDB spills to `temp_directory`, by default up to 90% of the free space.
    - In a pod that space is the node's ephemeral storage, and past its limit kubelet evicts the pod: every session
      on it dies at once.
    - With `max_temp_directory_size` under the volume's size, an overflow fails the one query instead.
  - two projected ServiceAccount tokens:
    - one with the audience `hugr-platform`, for the entry and the heartbeat;
    - one with the IdP's federation audience (`api://AzureADTokenExchange` in Entra). It is the client assertion with
      which the node gets the IdP's token for tresor (FLOW `federated`), as the group's workload identity.
  - the doors' TLS secret mounted;
  - `terminationGracePeriodSeconds = drainSeconds + 30`;
  - a readiness probe on the Flight port (the process serves, and nothing more: see "Where clients go");
  - the pod template's annotations `platform.hugr.io/restartedAt`, from the group's `restartedAt`, and
    `platform.hugr.io/heartbeat-seconds`, the interval its document carries and its Lease is judged by - a new
    interval rolls the pods, never a pod beating at one interval and judged at another;
  - rolling updates with `maxSurge: 1, maxUnavailable: 0`, so that new nodes come up before old ones drain.
- **A headless Service** `hugr-node-<group>`. Its EndpointSlice is the front's node list (spec 002): pod name, address,
  and `ready` / `serving`.
- **A PodDisruptionBudget** with `maxUnavailable: 1`.
- **A NetworkPolicy**: the doors' ports accept the front and the operator only. Egress is open, because the nodes
  reach the sources, the repository, tresor and the IdP.
- **The pod-deletion cost.** The leader writes `controller.kubernetes.io/pod-deletion-cost` on each pod from its live
  sessions in the last heartbeat, in powers of two (a pod is patched when its load changes in kind, not at every
  session), and -1 for a lost node. A scale-down removes a lost node first, then the pod with the fewest sessions, and
  that pod drains.

### Applying a change

| what changes | how | how it reaches the nodes |
| --- | --- | --- |
| a new source | `ACL CLUSTER ATTACH sales (TYPE postgres, SECRET pg_sales) IN GROUP etl` | **hot**: the group's nodes attach it live |
| …and its data opened | `ACL ADMIN ADD TABLE …`, `GRANT …` | the policy: **at once** (the nodes re-read it every `acl_version_check_interval`, 1 s) |
| an extension installed | `ACL CLUSTER INSTALL EXTENSION spatial FROM hugr IN GROUP g` | **hot** |
| an extension updated or removed | `ACL CLUSTER UPDATE / REMOVE EXTENSION …` | **restart** |
| the sessions' limits | `ALTER RESOURCE GROUP etl SET (max_result_rows 1000000, …)` | the policy: **at once**, for new sessions (spec 096: on a group's nodes the group's limits apply to everyone) |
| the engine's limits | `ACL CLUSTER SET memory_limit '100GB' IN GROUP etl` | **restart** (a node's configuration is locked after its bootstrap) |
| a source re-pointed, or detached while in use | `ACL CLUSTER ATTACH` of the same alias, `DETACH` | **drain** |
| the number or size of nodes | `HugrNodeGroup` | Kubernetes |
| the image | `HugrPlatform.spec.node.image` | Kubernetes: a rolling update, one node at a time |

- **Hot.**
  - The node that runs the statement applies it before its commit, and a failure rolls the write back (spec 093).
  - Every other node of the scope applies it on its convergence tick when it sees the new profile version, and then
    reports `config.applied`. The tick is hugr-node's, coming with the heartbeat. Until then the other nodes take a
    hot item at their next start only (spec 006 there).
- **Restart and drain.**
  - A node that cannot apply an item live reports `needs_restart` and the item in `pending`.
  - The group's status lists those nodes, and the console offers to restart the group.
  - Restarting is setting the group's `restartedAt`, the console's button or a `kubectl patch`. The operator puts it
    into the pod template, and Kubernetes rolls the pods one at a time: a new pod up, the old one drained by SIGTERM.
    Each new pod applies the profile at its start.
  - Nothing restarts by itself. The group's field is the handle (`kubectl rollout restart` on the Deployment rolls the
    pods too: its annotation is kubectl's field, which the operator's server-side apply leaves alone).
- **New nodes** always start with the current profile.
- **Where the outcome is seen.**
  - Per node: `platform.drift` (hugr-node), what matches the profile, what is missing, what is extra.
  - Per group: its status, the target profile version, how many nodes have applied it, who needs a restart.
  - A change through `ACL NATIVE` on one node is break-glass: it is not in the profile and shows as drift.

### Where clients go

- **The role decides the group, and the node decides, not the front.** That keeps the ACL the only judge.
  - The front (spec 002) seats a new connection on a node of the default group.
  - The node's `SessionOpen` computes the placement (duckdb-acl spec 096). For a principal of another group it
    refuses with `wrong_resource_group`, naming the groups that serve it.
  - The front takes that refusal, seats the connection on a node of a named group, and it is sticky from then on.
    The client sees none of it.
  - The cost is one extra hop, only on a first connection and only for a principal outside the default group. A
    client that knows its group may go to its host directly.
  - The refusal names the groups in text today. The front needs them machine-readable: a duckdb-acl addendum to
    spec 096 adds them as `x-acl-resource-groups` (a Flight trailer, a header on quack's CONNECT answer). It tells a
    client nothing the text does not already tell.
- **A node the front no longer seats on.**
  - The front seats new connections only on nodes whose Lease summary says `admit.new_session`. A draining node, or
    one whose group the policy lacks, is skipped.
  - Sticky sessions keep going to their node until they end.
  - Readiness is not used for this: a pod that is not ready is not serving either, and the front would cut its live
    sessions.
- **A resource group dropped.**
  - The group's nodes already refuse new sessions (spec 096: a group the policy lacks fails closed).
  - hugr-node makes it explicit: a node that finds its group gone calls `acl_drain()` itself, and its report says
    `draining`.
  - The group's status says `Draining`. Its pods cost until someone scales the group down or deletes it; nothing
    removes them by itself.

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

   The node refuses a document without (hugr-node spec 010) `node_group`, `node_label` (the pod's name, a DNS label),
   tresor's host, issuer and client id, the repository's https prefix or the catalog's secret. The rest have
   defaults: the repository's name `hugr`, the catalog's schema `acl`, no tresor scope, and a heartbeat every 15 s (a
   whole number of seconds in 1..3600).

   The node's identity in tresor, and everything after it, is hugr-node spec 006's: tresor, then the repository, then
   the catalog by its secret, then the profile, then lock, then the doors.

### The heartbeat and the fleet's state

- **The call.** `POST /v1/heartbeat` every `interval_seconds`, with the same token, verified the same way. The body
  is the contract of the 2026-10-17 draft:
  - `node_id`;
  - `image`, `duckdb` (the build's version, duckdb's source id);
  - `config`: `target`, `applied`, `needs_restart`, `pending` (kind, name, class - `restart`, `drain`, or `hot` with the
    error's class for one that failed);
  - `drift` (counts by state, `cut` for a setting held at the pod's ceiling);
  - `load`: `acl_node_load()`.

  The limit is 256 KiB, and unknown fields are ignored.
- **The `node_id` is the token's pod name.** A body that names another pod is refused with 409. A node can only speak
  for itself.
- **The Lease.** The operator renews a Lease named after the pod (`coordination.k8s.io/v1`). Its holder is the pod
  and its duration is 3 × the interval. The pod owns it, so it goes with the pod. A lost node is an expired Lease;
  nothing keeps a timer.
- **The Lease's annotations** carry a small summary of the report:
  - the profile's `target` and `applied`;
  - `needs_restart`;
  - `admit.new_session`;
  - draining;
  - sessions and seats left.

  The front and the console read it (specs 002 and 003). The full report is not stored; the metrics travel through
  acl_otel when a receiver is set.
- **The group's status** is aggregated by the leader from its pods' Leases every 5 s, and when one of the group's own
  objects changes - never per heartbeat:
  - the target (the highest any live node reports) and how many have applied it;
  - who needs a restart;
  - who is lost;
  - whether the nodes know the resource group.
- **Load.** For 1000 nodes at 15 s that is about 67 Lease updates a second (read from the operator's cache, written to
  the API server), the order of kubelet's own node Leases; a node's token is reviewed once a minute, not at each
  heartbeat (a positive review is kept a minute, by the token's hash).
- **The Lease is the pod's alone.** A Lease named like the pod that the operator did not write for it (the leader
  election's, anyone's) is never taken over: the heartbeat is answered 409.

### The first deployment, and the break-glass door

- **The first deployment.** The installation's Job runs the first SQL in its own process, unprefixed, writing straight
  into the policy catalog as the database's operator. No door and no token are involved: before the issuers exist,
  nobody could present one. The Job writes:
  - the issuers and the clients;
  - the roles and the administrators' bundles;
  - the operator's CLIENT;
  - the resource groups;
  - the first profile;
  - the integrations' receivers (decision 4).

  Then it creates the `HugrNodeGroup`s.
- **When nobody can sign in** (an issuer broken, the administrators locked out), the same door serves: SQL run in a
  pod's own process through `kubectl exec`, or a Job, under Kubernetes' access control and its audit log.

### The operator as a client of the nodes (owner, 2026-10-18)

The operator talks to the nodes the way any principal does: Flight SQL, its own bearer token, and the ACL deciding.
None of this spec's flows needs it, but it is provided for from the start: the console's API (spec 003) and the
technical calls to come use it.

- **Its identity** is a workload identity federated into the organisation's IdP, the same mechanism as the nodes'.
  - The operator's ServiceAccount token is the client assertion; in Azure it is a user-assigned managed identity
    with a federated credential for that ServiceAccount.
  - The nodes already trust the IdP. The policy gets a CLIENT for the operator (its `azp`), whose own mapping gives
    the role `hugr_operator` (duckdb-acl spec 095: a privileged role is reached only through a client's own mapping).
- **Its rights are reads only**: the `observe` bundle. It writes nothing into the policy.
- **Where it connects**: directly to one ready node, in the cluster, with the group's TLS name. The front is for
  clients.
- **Whose identity a call carries** is decided by who started it.
  - **The operator's own** carries the technical calls nobody asked for, each with its reconcile run's trace.
  - **A person's** carries what a person does in the console (drain a node, kill a session). The node judges it, and
    the audit names that person, not the operator.
  - **No identity at all** is needed for what the console shows from the fleet's state (Leases, the groups' status):
    that is Kubernetes, read as the operator.
- **No session per action.** Flight SQL authenticates each call, and an acl session is sticky through its cookie
  (duckdb-acl spec 050). The operator connects to nodes directly, not through the front, so it keeps the stickiness
  itself.
  - The pool holds one gRPC channel per node, and one session cookie per node and principal.
  - The first call opens the session; every later call of that principal reuses it.
  - Each call still carries the bearer and is verified. Under the `connect` binding (spec 059 there) a past `exp` is
    forgiven on a live session, while the signature, the issuer, the audience and the same-principal check are not.
  - A person's sessions end with their console sign-out (`CloseSession` on each node holding one), or at the node's
    idle timeout.
- **One trace.** The operator passes `traceparent` and `x-correlation-id` as Flight headers (spec 069 there), so the
  node's audit event joins the console's span or the reconcile run.
- **Why a person's token and not the operator's plus a trace.** The node would judge the operator's rights, and the
  console would have to decide by itself which person may do what. That makes two judges; with the person's token the
  ACL stays the only one.

### Replicas and what stays outside

- **Replicas.** Any operator replica answers HTTP (`/v1/bootstrap`, `/v1/heartbeat`, the webhook, `/healthz`,
  `/readyz`); leader election picks the one that reconciles.
- **Not in this spec:** packaging - the Azure Managed Application (it also makes the workload identities of the
  operator and of each group) and a Helm chart for private Kubernetes. A spec of their own.

## Security

- **No credential at rest.** No resource has a field for one, and the document carries names and URLs.
  - The doors' TLS key is a Kubernetes Secret, which cert-manager or the cluster's administrator provides. The
    operator only names it.
  - The operator logs a refusal with its reason, the ServiceAccount and the pod, never a token.
- **Who decides what.**
  - The infrastructure (how many nodes, how big) is Kubernetes' access control's, inside the installation's bounds.
  - Who may use which nodes is the policy's, judged by acl at every session.
  - What a node is, is the profile's (`cluster` bundle).
  - None of the three can widen another.
- **Who may call.**
  - `/v1/bootstrap` and `/v1/heartbeat` answer a pod of a group, bound by TokenReview; a group's pointers go only to
    that group's pods.
  - `/healthz` and `/readyz` are unauthenticated and answer nothing about the fleet; a replica is ready once its node
    API listens and its webhook serves.
  - The webhook is registered with the API server for the platform's namespace only.
  - The metrics are https behind TokenReview and SubjectAccessReview.
  - A NetworkPolicy lets only a group's pods reach the node API; a bearer that is no JWT, or past 16 KiB, costs no
    review, and at most 32 reviews run at once, each within 10 s.
- **The namespace is the boundary.** Kubernetes lets anyone who may create pods in a namespace run one as any of its
  ServiceAccounts, and the ownerReferences the entry checks are the pod's own, which its creator writes. So whoever
  may create pods (or exec into them, or request a ServiceAccount's token) in the platform's namespace can act as a
  group: its entry, its Lease, its federated login to tresor. The namespace is the platform's alone. The optional
  admission policies `config/policy` (ValidatingAdmissionPolicies, Kubernetes 1.30+) let a pod, a ReplicaSet or a
  Deployment that runs as a group's account come only from the chain that owns it: the operator writes the
  Deployment, the deployment controller its ReplicaSets, the ReplicaSet controller its pods.
- **The operator's rights.**
  - In its namespace (a RoleBinding): ServiceAccounts, Deployments, Services, PodDisruptionBudgets, NetworkPolicies,
    Leases (create, update - no delete: the garbage collector removes what its owners leave), its resources, their
    status and the groups' finalizers. It reads ReplicaSets and Pods and annotates Pods.
  - Cluster-wide: `tokenreviews/create` and `subjectaccessreviews/create` (`system:auth-delegator`: the entry and the
    metrics) and its webhook's registration.
  - In the policy: `observe` only.
  - Its egress needs the API server, the nodes' doors and the IdP (its own token).

## Testing

- **Unit tests**:
  - a group's expansion into its objects (golden YAML), including the engine's defaults and the spill volume;
  - the webhook: a size not in the plan, a total past `maxNodes` over every group, a fitting change, a change that
    does not grow passing while past the plan, a certificate that is another Secret;
  - the reconcile's re-check: two changes that fit alone and not together, the second `Refused` with what runs kept;
    an older group whose size comes back never growing into a newer group's nodes;
  - the document built from a platform and a group;
  - TokenReview answers: a foreign ServiceAccount, an unbound token, a pod of another group, a missing pod, a
    ReplicaSet of another uid; a bearer that is no token costing no review; the review cache;
  - the 409 for a `node_id` not the token's pod, and for a Lease that is not the pod's;
  - status aggregation from Leases: target, applied, needs restart, lost, the resource group unknown;
  - `restartedAt` and the heartbeat interval into the pod template; the deletion cost's buckets.
- **envtest** (controller-runtime's API server and etcd, no kubelet):
  - reconcile creates, updates and recreates the objects;
  - the webhook refuses through the API server;
  - a Lease that stops being renewed makes a node lost;
  - the pod-deletion cost follows the reported sessions;
  - a node's entry and heartbeats on real bound tokens (TokenRequest bound to the pod, TokenReview), a token of the
    federation audience refused, a deleted pod's token refused;
  - the optional admission policies refusing a pod and a ReplicaSet made by hand to run as a group's account.
- **kind e2e**: the operator, two groups of hugr-node pods, tresor-server and a repository. It comes with hugr-node's
  spec and proves:
  - the entry;
  - a hot `ATTACH` reaching every node of its group through the tick;
  - a `SET memory_limit … IN GROUP` reported as needing a restart and rolled by `restartedAt`;
  - a principal of the second group placed there by the front's re-seat (with spec 002);
  - a dropped resource group draining its nodes.
- **CI**: `go test ./...`, `go vet`, `staticcheck`, and the envtest binaries pinned. The image is distroless at
  `ghcr.io/hugr-lab/hugr-platform`, released like hugr-node's.

## Decisions (owner, 2026-10-18)

| # | question | decision |
| --- | --- | --- |
| 1 | The operator and the policy | a client of the nodes with its own identity, provided for from the start (the console's API needs it); reads only (`observe`), never a write into the policy |
| 2 | How a node logs in to tresor | a workload identity per group, federated into the organisation's IdP (Azure: a managed identity with a federated credential for the group's ServiceAccount). tresor-server and kista are products of their own, used by the organisation's people as well, so they trust the organisation's IdP, never the cluster |
| 3 | The doors' certificate inside the cluster | one certificate per group, its SAN `hugr-node-<group>.<ns>.svc`; the front and the operator dial a pod's address with that name as the TLS server name. cert-manager when the cluster has it, otherwise a Secret the administrator provides |
| 4 | The integrations' endpoints (where the telemetry and the OpenLineage events go) | a data path, so the installation (a `passthrough` administrator) sets the receivers; the `cluster` bundle's allowlist does not grow |
| 5 | Where the infrastructure is decided | in Kubernetes, with standard tooling: `HugrNodeGroup` (count, size), checked by a webhook against the installation's plan |
| 6 | What SQL keeps | the resource groups (placement and the sessions' limits, applied at once) and the profile (what a node is: the engine's limits, extensions, sources - hot, restart or drain). The three faces of a group are joined by its name and nothing else |
| 7 | The engine against the pod | the defaults follow from the pod (env, at bootstrap); a profile item wins but never above the pod; `max_temp_directory_size` under the spill volume, so an overflow fails a query instead of evicting the pod |
| 8 | A restart | by the group's `restartedAt` (the console's button), rolled one node at a time; nothing restarts by itself |
| 9 | Clients to their group | the node decides (spec 096) and the front re-seats on its machine-readable `wrong_resource_group`; a dropped resource group drains its nodes by themselves |

Verified by the demo (spec 004, minikube, 2026-10-18):

- Outside Azure, the IdP's federation of Kubernetes ServiceAccount tokens works: Keycloak 26.6's client authentication
  by a federated JWT, the client named by the token's subject. No node or operator secret lives anywhere.
- DuckDB at our pin reads the cgroup's memory but not the CPU: in a 2 GiB-limit, 1-CPU-request container it chooses
  `memory_limit` 1.5 GiB (80% of the limit, the node's own value) and `threads` 12 (the host's cores). A request sets
  no CPU quota, so the env's CPU is what bounds the threads; its memory only makes explicit what DuckDB would choose.

## Alternatives considered

- **A service with its own database** (the 2026-10-17 draft). Replaced: Kubernetes is the store, so there is nothing
  to migrate or back up, and Leases are what kubelet already uses for liveness.
- **SQL as the master, Snowflake-like**, weighed in full on 2026-10-18. The pools would be profile items, written into
  a pending log and checked by the operator, then applied by one `ACL CLUSTER APPLY`. A seed node would serve the
  first deployment and rescues. Set aside:
  - It needed a pending log, a plan and an apply, an executor of steps, a check at write, a pool item and a seed
    node, across hugr-node, acl's schema and the operator.
  - Each would carry its own failure modes. The operator would depend on a node to learn what it should run.
  - Nothing of it is needed for a first version. With Kubernetes as the truth, SQL control of the pools can come later
    as a facade: a statement asks the operator's API to change a `HugrNodeGroup`. The reverse move, out of a catalog
    that held the infrastructure, would be the hard one.
- **The operator writing the resource groups from the groups.** Not needed: the faces are joined by name, and a group
  whose resource group is missing says so and drains.
- **A StatefulSet per group.** Its scale-down removes the highest ordinal, whoever holds sessions there.
- **The node registering itself with the API server.** Refused: the node would need Kubernetes rights; with the
  heartbeat it needs none.
- **One core under two wrappers (operator and service) from the start.** Refused (owner, 2026-10-17): the operator is
  the product, and the shared part is a set of packages.

## Follow-ups

- **hugr-node**, spec 010 there:
  - the document v2 and the orchestrator-first entry;
  - `acl_node_label` from the document;
  - the heartbeat, with the convergence tick that applies hot items on every node and reports `config.applied`;
  - the engine's defaults from env, and a profile value cut to the pod;
  - the self-drain when the node's resource group is gone.
- **duckdb-acl**: an addendum to spec 096, the machine-readable `x-acl-resource-groups` on `wrong_resource_group`.
- **spec 002, the front**: routing by the label (duckdb-acl spec 134), the node list from EndpointSlices, seating by
  the Lease's summary, the re-seat on `wrong_resource_group`.
- **spec 003, the console**: the shell with tresor-server's and kista's micro-frontends, the fleet view, the groups'
  editing and restart, and a person's actions with their own token.
- **Packaging**: the Azure Managed Application (with the publisher's just-in-time access, never standing access) and
  a Helm chart.
- **Phase 2**: autoscaling from the nodes' load, a group's idle stop, and perhaps the SQL facade over the groups.
