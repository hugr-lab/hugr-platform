# Spec 001: the operator - node groups, a node's entry, the fleet's state

- **Status**: accepted
- **Date**: 2026-10-18 (supersedes the draft of 2026-10-17: a service with its own database)

## Summary

The platform's control part is a Kubernetes operator. It is built on controller-runtime and has no database of its
own: the desired state lives in two custom resources, and the fleet's state lives in Leases and resource statuses.
It does four things:

- It deploys the nodes of each group.
- It answers a node's entry: the node presents its service-account token and gets its bootstrap document.
- It records the heartbeat, so that a lost node is an expired Lease and a group's convergence is in its status.
- It is a client of the nodes with an identity of its own: it creates a group's resource group, writes the
  platform's integrations into the cluster profile and reads the nodes' load, through SQL like any principal.

It decides nothing for a node, and it holds no profile, policy or secret. The front that routes clients to the
nodes (spec 002) and the web console (spec 003) come from the same repository and are specified separately.

Shared logic sits in Go packages with no Kubernetes dependency: the node protocol (the document, the heartbeat) and
the expansion of a group into its pieces. A bare-metal service without Kubernetes comes only when one is needed; until
then hugr-node spec 006's static document covers it.

## Problem

- **Nobody deploys or scales the nodes.** A group of nodes is hand-written Kubernetes YAML.
- **Nobody answers the entry.** hugr-node fetches its bootstrap document from `GET <control plane>/v1/bootstrap`, and
  only a 30-line python fake in hugr-node's e2e serves it.
- **Nobody sees the fleet.** An operator cannot tell which nodes run, which profile version each has applied, which
  need a restart and which are gone.

## Design

### The resources (`platform.hugr.io/v1alpha1`, namespaced)

**`HugrPlatform`** - one per namespace: the pointers every node of the platform needs. No field holds a credential.

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
status:
  conditions: [...]                        # Ready, and why not
```

**`HugrNodeGroup`** - one pool of nodes.

```yaml
apiVersion: platform.hugr.io/v1alpha1
kind: HugrNodeGroup
metadata: {name: etl, namespace: hugr}
spec:
  platform: hugr
  replicas: 3                              # phase 1: set by hand (or by the Azure plan); autoscale in phase 2
  resources: {requests: {cpu: "8", memory: 64Gi}, limits: {memory: 64Gi}}
  scheduling: {nodeSelector: {hugr.io/pool: etl}, tolerations: [...]}   # a VM pool per group, one pod per VM
  identity: {clientID: "<the group's workload identity>"}   # federated from the group's ServiceAccount (Azure: a
                                           # user-assigned managed identity per group, made by the deployment)
  resourceGroup: etl                       # the duckdb-acl resource group (spec 096); default: the group's name
  drainSeconds: 300                        # SIGTERM -> acl_drain; the pod's grace is drainSeconds + 30
  doors: {flight: true, quack: true}
  tls: {secretRef: {name: hugr-node-etl-tls}}   # the doors' certificate (cert-manager or the operator's admin)
status:
  replicas: 3
  readyReplicas: 3
  profile: {target: 12, applied: 2, needsRestart: ["hugr-node-etl-7f9c-x2k8p"]}
  lost: []
  resourceGroupKnown: true                 # what the nodes report: does the policy have this group
  conditions: [...]
```

### What the operator builds for a group

The leader reconciles each `HugrNodeGroup` into the following objects, owned by the group and recreated when they
differ:

- **A ServiceAccount** `hugr-node-<group>`. The group is the identity: a node is of the group whose ServiceAccount it
  runs as.
- **A Deployment** `hugr-node-<group>`, not a StatefulSet: scaling down removes the idlest pod, not the
  highest-numbered one. Its pods get:
  - the image;
  - an env with no credential in it:
    - `HUGR_ORCHESTRATOR`: the operator's in-cluster URL;
    - `HUGR_NODE_LABEL`: the pod's name, through the Downward API. It becomes duckdb-acl's `acl_node_label`
      (spec 134 there), the name the node's sessions carry for the front;
    - `HUGR_FLIGHT` / `HUGR_QUACK`;
    - `HUGR_DRAIN_DEADLINE`;
    - `HUGR_CA_FILE`;
  - two projected service-account tokens: one with the audience `hugr-platform` for the entry and the heartbeat, one
    with the IdP's federation audience (`api://AzureADTokenExchange` in Entra). The second is the client assertion
    with which the node gets the IdP's token for tresor (FLOW `federated`), as the group's workload identity;
  - the doors' TLS secret mounted;
  - `terminationGracePeriodSeconds = drainSeconds + 30`;
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
  - That CLIENT is part of the platform's initial policy, written at installation. Until it exists the operator's
    writes are refused, and the platform's condition `PolicyAccess` says so.
- **Its rights** are the `cluster` bundle (spec 118 there: `cluster` ⊇ `operate` ⊇ `observe`), plus the resource
  groups' definitions.
  - Today `CREATE / ALTER / DROP RESOURCE GROUP` needs an unrestricted `manage`, which is the whole policy.
  - A duckdb-acl spec moves the definitions (a group of nodes and its limits are infrastructure) to the `cluster`
    bundle.
  - `GRANT RESOURCE GROUP … TO ROLE`, which roles land on which nodes, stays the policy administrator's.
- **What it does with them**, on reconcile:
  - `CREATE RESOURCE GROUP <g> (…)` for a group whose resource group is missing, with the group's limits.
    - It never drops one by itself (nothing is rolled back automatically): a deleted `HugrNodeGroup` leaves its
      resource group, and the platform's status names it as orphaned.
    - A drop is an explicit command.
  - The integrations of `HugrPlatform` as `ACL CLUSTER SET` items, as far as the `cluster` bundle allows: the
    lineage naming (`acl_lineage_namespace`, `acl_lineage_level`) is on its allowlist (hugr-node's
    `ClusterBundleMaySet`).
    - Where the telemetry and the OpenLineage events go (the receiver's endpoint, its key's tresor name) is a data
      path. That allowlist leaves it to `passthrough` on purpose (the 118.2 review), so the installation sets it, not
      the operator (decision 4).
    - A key is always named by its tresor secret, never carried.
  - Reads of the nodes' load and sessions for the console (spec 003).
- **Where it connects**: directly to one ready node of the group, in the cluster, with the group's TLS name. The front
  is for clients.
- **Whose identity a call carries** is decided by who started it.
  - **The operator's own** carries the technical calls nobody asked for:
    - reconcile: the resource group, the profile's naming;
    - reads of the nodes' load and sessions;
    - checks.

    Each carries the reconcile run's trace, so the node's audit shows `hugr_operator` and joins the run.
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

- **Replicas.** Any operator replica answers HTTP (`/v1/bootstrap`, `/v1/heartbeat`, `/healthz`, `/readyz`); leader
  election picks the one that reconciles.
- **Not in this spec:** packaging - the Azure Managed Application (it also makes the workload identities of the
  operator and of each group in the plan) and a Helm chart for private Kubernetes. A spec of their own.

## Security

- **No credential at rest.** Neither resource has a field for one, and the document carries names and URLs.
  - The doors' TLS key is a Kubernetes Secret, which cert-manager or the cluster's administrator provides. The
    operator only names it.
  - The operator logs a refusal with its reason, the ServiceAccount and the pod, never a token.
- **Who may call.**
  - `/v1/bootstrap` and `/v1/heartbeat` answer a pod of a group, bound by TokenReview; a group's pointers go only to
    that group's pods.
  - `/healthz` and `/readyz` are unauthenticated and answer nothing about the fleet.
  - The console's API is spec 003's.
- **The operator's rights.**
  - In its namespace: ServiceAccounts, Deployments, Services, PodDisruptionBudgets, NetworkPolicies, Leases, its
    resources and their status. It may read Pods and EndpointSlices and annotate Pods.
  - Cluster-wide: only `tokenreviews/create` (`system:auth-delegator`).
  - Its egress goes to the API server, to the nodes' doors and to the IdP (its own token).

## Testing

- **Unit tests**:
  - the expansion of a group into its objects (golden YAML);
  - the document built from a platform and a group;
  - TokenReview answers: a foreign ServiceAccount, an unbound token, a pod of another group, a missing pod;
  - the 409 for a `node_id` not the token's pod;
  - status aggregation from Leases;
  - the SQL it sends as a client: a missing resource group created with the group's limits, an existing one left
    alone, a deleted group's never dropped, the integrations as `ACL CLUSTER SET` items naming tresor secrets.
- **envtest** (controller-runtime's API server and etcd, no kubelet):
  - reconcile creates, updates and recreates the objects;
  - scaling changes the Deployment;
  - a Lease that stops being renewed makes a node lost;
  - the pod-deletion cost follows the reported sessions.
- **kind e2e**: the operator, one group of two hugr-node pods, tresor-server and a repository. It comes with
  hugr-node's document-v2 spec, since the real node is what proves the entry - and that the operator's own token is
  admitted by the nodes' policy for its writes and refused for anything past its bundle.
- **CI**: `go test ./...`, `go vet`, `staticcheck`, and the envtest binaries pinned. The image is distroless at
  `ghcr.io/hugr-lab/hugr-platform`, released like hugr-node's.

## Decisions (owner, 2026-10-18)

| # | question | decision |
| --- | --- | --- |
| 1 | The operator writing into the policy catalog | it is a client of the nodes with its own identity, from the start: the console needs it too. Its role holds the `cluster` bundle and the resource groups' definitions (a duckdb-acl spec); grants stay the policy administrator's |
| 2 | How a node logs in to tresor | a workload identity per group, federated into the organisation's IdP (Azure: a managed identity with a federated credential for the group's ServiceAccount). tresor-server and kista are products of their own, used by the organisation's people as well, so they trust the organisation's IdP, never the cluster |
| 3 | The doors' certificate inside the cluster | one certificate per group, its SAN `hugr-node-<group>.<ns>.svc`; the front and the operator dial a pod's address with that name as the TLS server name. cert-manager when the cluster has it, otherwise a Secret the administrator provides |
| 4 | The integrations' endpoints (where the telemetry and the OpenLineage events go) | a data path, so the installation (a `passthrough` administrator) sets the receivers; the operator writes only the naming its `cluster` bundle allows. The allowlist does not grow |

Still to verify: outside Azure, the IdP's federation of Kubernetes ServiceAccount tokens (Keycloak's client
authentication by a federated JWT). Without it, a node's or the operator's client secret would have to live
somewhere, which the platform refuses.

## Alternatives considered

- **A service with its own database** (the 2026-10-17 draft). Replaced: Kubernetes is the store, so there is nothing
  to migrate or back up, and Leases are what kubelet already uses for liveness.
- **A StatefulSet per group.** Its scale-down removes the highest ordinal, whoever holds sessions there.
- **The node registering itself with the API server.** Refused: the node would need Kubernetes rights; with the
  heartbeat it needs none.
- **One core under two wrappers (operator and service) from the start.** Refused (owner, 2026-10-17): the operator is
  the product, and the shared part is a set of packages.

## Follow-ups

- spec 002, the front: routing by the label (duckdb-acl spec 134), the node list from EndpointSlices, placement by
  the Lease's summary.
- spec 003, the console: the shell with tresor-server's and kista's micro-frontends, and the fleet view.
- hugr-node: the document v2 and the orchestrator-first entry; setting `acl_node_label` from `HUGR_NODE_LABEL`; the
  heartbeat sender.
- duckdb-acl: the resource groups' definitions under the `cluster` bundle.
- Packaging: the Azure Managed Application (with the publisher's just-in-time access, never standing access) and a
  Helm chart.
- Phase 2: autoscaling from the nodes' load, and a group's idle stop.
