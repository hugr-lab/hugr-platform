# Spec 003: the console - one place to run the platform

- **Status**: accepted (the owner, 2026-10-19: Q1-Q5 as recommended); the look is made with Claude Design on the Hugr Lab
  design system (hugr-lab.com). **Round 2** (the owner's asks after the first design) accepted the same day: Q6 and
  Q8-Q11 as recommended, Q7 the owner's way (the engine's memory from `duckdb_memory()`), and the owner's direction on
  where telemetry and lineage go (below)
- **Date**: 2026-10-19

## Summary

The console is the platform's one web interface. The operator serves it. It is a shell: it owns the sign-in, the
navigation and the theme, and it mounts tresor-server's and kista's consoles through their micro-frontend contracts.
Its own pages are the fleet: the groups and their nodes, what each node holds, and what a person may do to them -
scale or restart a group, drain or resume a node, end a session. Every node-side action carries the person's own
token, so the ACL judges it, as spec 001 decided. The pages a person sees are the ones their ACL rights admit (design/082
C3); the fleet's state comes from Kubernetes (C1). The policy is not edited here: that is SQL and the DBeaver plugin.

## Problem

- **No interface.** An administrator runs the platform with `kubectl`, the nodes' SQL and two separate consoles
  (tresor-server's, kista's), each with its own sign-in and address.
- **The fleet's state is in pieces**: a group's status in its `HugrNodeGroup`, a node's load in its Lease, its
  sessions behind its door. Nothing shows them together.
- **A person's actions on a node** (kill a session, drain) need a Flight SQL client and the person's token. A browser
  speaks neither gRPC nor Flight.
- **Who may do what** is the ACL's (bundles `observe`, `operate`, `cluster`, `policy` - duckdb-acl spec 117/118). A
  console that decided by itself would be a second judge (spec 001).

## Design

### Where it runs

- **The operator serves it**, on every replica (the leader only reconciles): a listener of its own (`:8444`), its own
  public certificate (the Secret `hugr-console`, the administrator's or a cert-manager Certificate's - as the front's,
  spec 002), and a Service `hugr-platform-console` in `config/manager`. Clients outside the cluster reach it through
  the installation's Ingress or load balancer (the packaging spec); the node API's port and certificate stay the
  nodes' alone.
- **The web build is embedded** in the operator's binary (`//go:embed`), built by a Node stage in the image's
  Dockerfile. A binary built without Node serves a placeholder page that says so (kista's rule, its spec 0015).
- **The stack is tresor-server's and kista's**: React 18, TypeScript, Vite, Tailwind 3, react-router-dom 7,
  lucide-react, oidc-client-ts; Manrope and JetBrains Mono self-hosted; no component library (native `<dialog>`s); a
  strict CSP (no inline script, no `data:` fonts); a licence gate (MIT, ISC, Apache-2.0, BSD, OFL). Source in
  `web/console/`.

### Sign-in and tokens

- **The browser signs the person in** at the organisation's IdP (`spec.idp.issuer`): authorization code with PKCE,
  oidc-client-ts, the console's own public client (`spec.console.clientID`). The console keeps tokens in memory only;
  a reload signs in again silently (the IdP's session).
- **A token per audience**, as tresor's and kista's contracts ask (`getToken(audience, {renew})`):
  - **the nodes'** (`spec.console.audiences.nodes`, the audience of the policy's client for the console, duckdb-acl
    spec 095): the console's own API and every node-side action;
  - **tresor-server's** and **kista's** for their mounts.
  - Entra: one token per resource, by the refresh token (`acquireTokenSilent` semantics: scope `<api>/.default`).
    Keycloak: audience mappers put the audiences in one token, or token exchange.
- **No second sign-in**, no token stored by the operator, none logged, none in a URL.

### Who may see and do what (C3)

- **The ACL decides, once per person, cached.** The operator asks a ready node `SELECT * FROM platform.console_info()`
  with the person's token: the answer's `bundles` and `my_views` / `my_functions` say what this person holds. Cached
  60 s by the token's hash (never the token); a refused token is the sign-in's problem (401).
- **A page shows when its reads are the person's**, an action when its call is:

  | page or action | needs |
  | --- | --- |
  | the fleet: groups, nodes, their Leases and status | `observe` (`platform.node_load` readable) |
  | a node's sessions | `observe` (`platform.sessions`) |
  | end a session, drain or resume a node | `operate` (`platform.kill_session`, `drain`, `resume`) |
  | scale a group, restart a group | `cluster` (the infrastructure's right, duckdb-acl spec 118) |
  | secrets | tresor-server's own `whoami` (`permissions.create`) |
  | extensions | kista's own `whoami` (`administrator`, or `holds_admin` on the tenant) |

- **The API checks the same table** on every call: hiding a button is a convenience, the check is the operator's (for
  the Kubernetes writes) and the node's (for its own actions, which it judges again).
- **No node answers**: the console cannot learn anyone's rights, so it shows nothing but that, and says the cluster's
  state is `kubectl`'s (fail closed).

### Pages (C1)

- **Fleet** - one row per group: its size, replicas wanted / ready, the resource group it serves and whether the
  policy knows it, the profile's target and applied version, a restart pending, draining. Actions: scale (within the
  plan: the webhook refuses past it, with its reason), restart (the group's `restartedAt`).
- **Group** - one row per node: label, ready, the Lease's age, sessions, seats left, admitting, the image's version.
- **Node** - its load report (`platform.node_load`, read with the person's token), its sessions (`platform.sessions`:
  principal, door, opened, idle, groups) with "end session"; drain / resume.
- **Secrets** - tresor-server's console, mounted. **Extensions** - kista's, mounted with the tenant the repository's
  prefix names.
- A page reads when it opens and on the person's refresh; nothing polls in the background (tresor's rule).

### The console's API (the operator)

Under `/api/v1/console/`, the person's nodes-audience token as the bearer on every call:

| method, path | does | right |
| --- | --- | --- |
| `GET me` | the person's rights (the table above), the platform's name, the mounts' bases and audiences | any signed-in |
| `GET groups` | the groups with their status and nodes (Kubernetes: the groups, their Leases) | observe |
| `PATCH groups/{g}` `{replicas}` / `{restart:true}` | writes the `HugrNodeGroup` as the operator (SSA, its field owner) | cluster |
| `GET nodes/{label}` | the node's load report, read on that node with the person's token | observe |
| `GET nodes/{label}/sessions` | `platform.sessions` on that node | observe |
| `POST nodes/{label}/sessions/{id}/end` | `KILL SESSION '<id>'` on that node | operate |
| `POST nodes/{label}/drain` / `resume` | `DRAIN NODE` / `RESUME NODE` on that node | operate |
| `POST sign-out` | `CloseSession` on every node holding one of this person's sessions | any signed-in |

- An answer carries no token, no session handle (`platform.sessions` shows the ops id, never the handle), no
  connection id, and a node's refusal only as its message.
- A Kubernetes write names the person in its audit: an annotation `platform.hugr.io/changed-by` (the person's `sub` at
  the IdP, never a token) on the group, and the operator's log line.

### Talking to the nodes (spec 001's pool, now built)

- **Flight SQL, directly to the node** a page names (the console's calls are about one node), over the group's TLS
  name and the platform's CA bundle (the front's dialing, spec 002).
- **One gRPC channel per node; one session cookie per node and person**: the first call opens the person's session,
  later ones reuse it (duckdb-acl spec 050). Each call carries the person's bearer, and `traceparent`.
- **Sign-out** ends the person's sessions on every node holding one; otherwise the node's idle timeout does.
- Statements are fixed texts; an id from the browser (a session's ops id) is bound as a constant only after it
  matches the ops id's form - never SQL from the browser.

### tresor-server and kista

- **Same origin, proxied**: `/tresor/…` to `spec.tresor.host`, `/kista/…` to the repository prefix's host - their
  contracts' recommended delivery (no CORS; the console's CSP allows itself). The proxy forwards the request as it
  came (the person's bearer for that service included), adds nothing, follows no redirect.
- The modules are imported from their own servers (`/tresor/ui/mfe/tresor.js`, `/kista/ui/mfe/kista.js`); their
  `contract` must be 1, else the entry says the component is too new or too old.
- `onUnauthorized` from either: the console renews the token once, then signs the person in again by a popup.

### Configuration

`HugrPlatform.spec.console`: `clientID` (the IdP's public client), `audiences` (`nodes`, `tresor`, `kista`),
`scopes`; the issuer is `spec.idp.issuer`. The console's public certificate is the operator's Deployment's
(`hugr-console`).

## Security

- **The ACL is the only judge** of node-side rights and of the fleet's pages (C3); the operator executes the
  Kubernetes writes a `cluster` right asked for, and nothing else of its own initiative.
- **No credential at rest**: tokens live in the browser's memory; the operator keeps a token's hash for the rights
  cache and the session cookies the nodes gave (in memory, per replica), never a token.
- **Nothing from the browser becomes SQL**: fixed statements, ids checked by form.
- **The proxy** to tresor and kista forwards the person's bearer to the service it is meant for (the audience is the
  service's), never another's.
- **CSP**: `default-src 'self'`; the IdP in `connect-src` (token endpoint) and `frame-src` (silent renew), nothing
  else.

## Testing

- **Unit (Go)**: the rights table against `console_info` answers; the API's checks (each route refused without its
  right); the pool's per-person cookies; the proxy (no redirect, nothing added); the ops id's form.
- **Web**: Vitest for the token per audience and the pages' gating; Playwright against the operator with a fake IdP
  and fake nodes.
- **The demo** (spec 004): the console's API through the person's token - Fleet, a node's sessions, end a session,
  drain and resume, scale a group - and tresor's and kista's mounts shown.

## Alternatives considered

- **A console of its own Deployment**: one more thing to run and secure; the operator already holds the fleet's state
  and the nodes' dialing (owner, 2026-10-17: the operator serves it).
- **The operator's identity for every call plus a trace** - two judges (spec 001).
- **The browser talking to the nodes** through a gRPC-web gateway: a second public door to every node.
- **A backend session (the operator holding the person's tokens)**: tresor's and kista's contracts expect
  `getToken` in the browser; a server-side token store is a credential at rest.

## Decisions (owner, 2026-10-19): Q1-Q5 as recommended below

**Q1. How the console is reached.** *Recommendation:* its own listener, Service and public certificate on the operator;
the installation's Ingress or load balancer in front (packaging). Alternative: through the front, as a third door.

**Q2. Rights when no node answers.** *Recommendation:* fail closed - the console shows that no node answers and points
to `kubectl`. Alternative: an IdP role (`platform-admin`) checked by the operator itself gives the fleet's read-only
pages without a node - a second judge, but the one view that helps most when every node is down.

**Q3. Sign-in.** *Recommendation:* the browser (PKCE, oidc-client-ts), a token per audience in memory. Alternative:
the operator as a backend holding the tokens (rejected above: tresor's and kista's `getToken` contract, a credential at
rest).

**Q4. The pages of v1.** *Recommendation:* Fleet, Group, Node (load, sessions, end, drain / resume), scale and restart,
tresor's and kista's mounts. Later: the audit's history (C2), a client's workload tag (C4), the drift view
(`platform.drift`), the profile's pending items.

**Q5. Who may scale and restart.** *Recommendation:* the `cluster` bundle (the infrastructure's right, duckdb-acl spec
118). Alternative: `operate`.

## Round 2: what a node is made of, how it is doing, what the cluster is set to

The owner's asks after the first design (2026-10-19): a node's metrics (memory first); its profile as it came up (the
sources attached and their state, the extensions, the logs); tracing a session; dynamic scaling, prepared for though
not built; the node's identity; the telemetry; the cluster's settings (OTel, lineage, ...).

### What is new where

| repository | what | spec |
| --- | --- | --- |
| duckdb-acl | nothing: the engine's memory and the node's inventory are duckdb's own functions, already in the policy's categories (below) | - |
| hugr-node | `platform.telemetry`: acl_otel's state on this node (signals on, the receiver's host, `acl_otel_healthy()`, queued, dropped) - hugr-node's, since acl-otel links no acl symbol and cannot register a view | a new spec there |
| hugr-platform (this spec) | the operator reads the pods' metrics (`metrics.k8s.io`), logs and events, the doors' certificate expiry; the API and the pages below; the scaling shape reserved | here |

### A node's metrics

- **The pod's** CPU and memory against the size's request and limit, from the Kubernetes metrics API
  (`metrics.k8s.io`: metrics-server, which AKS runs). The pod's restart count and the last termination's reason
  (`OOMKilled` is the one that matters) from its status. The operator reads them; `observe`.
- **The engine's**, which the pod's number does not show (the owner: the pod's metrics, and `duckdb_memory()`): the
  buffer pool by tag (`duckdb_memory()`: `memory_usage_bytes`, `temporary_storage_bytes`) against `memory_limit`, and
  the spill (`duckdb_temporary_files()`) against `max_temp_directory_size` - the decision 7 of spec 001 put the spill
  under the volume so an overflow fails a query instead of evicting the pod; the console shows how close it is. Read
  on the node with the person's token, as any principal reads them: `duckdb_memory` and `duckdb_temporary_files` are
  in the `meta` category, `current_setting` in `environment` (duckdb-acl spec 072). The panel shows when the person's
  roles hold them and says which category otherwise - the policy decides, the console adds no view.
- **Now, not a history**: the page reads when it opens. Series over time are the telemetry backend's (below).

### What a node came up with (its profile and bootstrap)

The node's page gains the tabs **Sources and extensions** and **Logs**:

- **Sources**: each attached database (`duckdb_databases()`, the `meta` category, as the memory above) - name, type
  (postgres, ducklake, mssql, ...), read-only - joined with the profile item it comes from (`platform.cluster_effective`) and drift's state (`platform.drift`:
  same, differs, node_only, profile_only, bootstrap). Never the ATTACH string: the profile keeps credentials out of it
  (spec 006), a `NATIVE` attach may not. Whether a source answers is not probed from the console; the listings' mark
  (duckdb-acl spec 136) and the audit say it.
- **Extensions** (`duckdb_extensions()`, `meta`): name, loaded, version, the repository it came from; with drift. **Settings**: drift's setting rows,
  the node's value against the profile's.
- **Logs**: the pod's log through the Kubernetes API (`pods/log`; the operator's RBAC gains `get`), the last 500 lines
  or 1 MiB, the current container or the previous one - a node whose bootstrap failed restarts, and the reason is in
  the previous log (spec 006). With the pod's events (scheduling, image pull, OOM). Read on open and on refresh, never
  streamed; a line is text, never interpreted. `cluster` (Q6).
- **A node that does not answer still has its page**: Kubernetes' part (pod, events, logs, metrics) is the operator's;
  the node's part says it does not answer. The person's rights come from another node.

### Tracing a session

- The sessions' table shows each session's audit level and profile level and where each comes from
  (`audit_level_source`, `profile_source`: the node, the policy's rule, an operator's override), and sets them:
  `SET SESSION '<id>' AUDIT LEVEL off|denied|decisions|all|DEFAULT`, `PROFILE SESSION '<id>' ON|SAMPLED|OFF`
  (duckdb-acl spec 118; `operate`). The override lives on that node's session record.
- A profile becomes spans where acl_otel's `acl_otel_profile_spans` is on; the page says so when the cluster's
  settings have it off.
- The policy-wide rules (acl_otel's level rules, by role or claim) are shown on the Cluster page, read only.

### Dynamic scaling, prepared for

The autoscaler is phase 2 (spec 001). The owner's model (2026-10-19, reserved now, to be refined with the
autoscaler): a group may have a min and a max number of nodes; under load the count grows toward the max, without it
it falls toward the min; a min of 0 means the last node stops when nothing loads the group, and a node starts when a
client comes. The console is built for a group that is either manual or autoscaled, so its pages do not change when
the autoscaler lands:

- The API's group carries `scaling: {mode: manual|auto, min, max, last: {at, from, to, reason}}`; until the autoscaler
  exists every group is `manual`.
- **Manual**: Scale sets the replicas, as now. **Auto**: the dialog edits min and max within the plan and never the
  replicas - the autoscaler owns `spec.replicas` (its server-side apply field), and the console never takes it back.
  Switching between the two is the same right (`cluster`).
- The fleet shows an autoscaled group's bounds, its last decision and the reason; a group at 0 (the idle stop of phase
  2) shows "stopped - starts on the first client" (the front holds that client, `noNodeWaitSeconds`).
- The plan's check counts an autoscaled group at its max.
- The CRD's fields and the signals are the autoscaler's spec; this spec reserves only the API's shape (Q8). One
  question that spec must answer: at 0 nodes there is no Lease and no load report, so the signal that wakes the group
  is the client the front holds - how the front tells the operator a group's client is waiting.

### The node's identity

On the group's page (it is the group's) and the node's:

- **who it is**: the ServiceAccount, the IdP client id (`spec.identity.clientID`), the subject the IdP sees
  (`system:serviceaccount:<ns>:<sa>`), the tresor host it logs in to;
- **what it answers as**: the doors' TLS name (`hugr-node-<group>.<ns>.svc`) and the certificate's expiry (the group's
  Secret: the operator reads `notAfter`, nothing else), the label, the pod's IP, the Kubernetes node and zone;
- **what it runs**: the image's digest, the duckdb version, the hugr-node build (the heartbeat's).
- Never a token, never the projected token's content.

### Telemetry and lineage: where they go, what the console shows

Where they go depends on the installation (the owner, 2026-10-19):

- **Telemetry**: the receiver the administrator chooses - on Azure most likely Application Insights, otherwise any
  backend; when the administrator chooses none, the platform may deploy one of its own (an OTel Collector with Grafana;
  later perhaps the platform's own DuckLake as the store); or none at all.
- **Lineage**: the same three cases - an OpenMetadata or another OpenLineage receiver the organisation has, one deployed
  with the platform, or none. Whether the platform deploys one, and which, is still to be thought through.
- Both are the integrations' spec (a follow-up), not this one. The console needs only what it links to.

What the console does:

- It stores no telemetry and queries no backend: that would be a second data path and a credential for it.
- It shows a node's **recent audit events** (`platform.audit_events`, the node's ring, `observe`) - the "recent" half
  of design/082's C2. The history is the receiver's.
- **A link out, nothing more** (the owner: a link is enough for now): the installation names the backend - its name
  ("Application Insights", "Grafana", "OpenMetadata") and URL templates with `{node}`, `{group}`, `{session}`,
  `{trace}`; for one deployed with the platform the link is known. The console renders "Open in <name>" on a node, a
  session, an event, and on the Cluster page's integrations. An https URL, checked, never a secret. No receiver: no
  link, and the Cluster page says the telemetry stays in the nodes' rings.

### The cluster's settings (read only)

A page **Cluster**:

- **The profile** (`platform.cluster_items`): per scope (the cluster, a group) the extensions, sources and settings,
  each with its version, class (hot, restart, drain) and dependencies; the profile's version.
- **The integrations**: telemetry (acl_otel's receiver host, protocol, the signals on; `platform.telemetry` per node:
  healthy, queued, dropped) and lineage (`platform.lineage_status` per node: namespace, level, sending, and why not).
- **Drift**: per node, counted (the heartbeat's `drift`), listed on the node's page.
- Rights: `cluster` for the profile and drift (CLUSTER views); `observe` for the integrations' state. The writes stay
  SQL - `ACL CLUSTER ...`, and the receivers a passthrough administrator's (spec 001, decision 4) (Q10).

### The API, added

| method, path | does | right |
| --- | --- | --- |
| `GET nodes/{label}/kubernetes` | the pod: status, restarts, last termination, metrics, events | observe |
| `GET nodes/{label}/logs?previous=&lines=` | the pod's log, bounded | cluster |
| `GET nodes/{label}/inventory` | sources, extensions, settings; drift's states with `cluster` | observe |
| `GET nodes/{label}/events` | the node's audit ring | observe |
| `POST nodes/{label}/sessions/{id}/tracing` `{audit, profile}` | the two statements above | operate |
| `GET cluster` | the profile, the integrations, drift counted | cluster / observe |
| `PATCH groups/{g}` `{scaling: {mode, min, max}}` | when the autoscaler exists | cluster |

### Decisions (owner, 2026-10-19): Q6 and Q8-Q11 as recommended below, Q7 otherwise

**Q6. Who reads a node's logs.** *Recommendation:* `cluster` - a log names hosts and sources, and it is the
infrastructure's view. Alternative: `observe`, beside the events.

**Q7. Where the engine's memory is measured.** *Decided otherwise by the owner:* the pod's metrics, and duckdb's own
`duckdb_memory()` read on the node - no new view or gauge. (The recommendation was a load-report field and gauges in
duckdb-acl.)

**Q8. Scaling in the console before the autoscaler.** *Recommendation:* reserve the API's shape now and show manual
groups; an autoscaled group's dialog edits min and max only. Alternative: leave scaling's shape to the autoscaler's
spec and change the page then.

**Q9. Telemetry.** *Recommendation:* the node's ring and links out, no queries to a backend. Alternative: the console
reads Prometheus/Tempo through the operator - a credential and a second data path.

**Q10. Editing the cluster's settings in the console.** *Recommendation:* read only in v1; later the same `ACL
CLUSTER ...` statements under the person's token, judged by the node. Alternative: edit in v1.

**Q11. Tracing a session.** *Recommendation:* in the sessions' table, per session on its node. Alternative: also a
"trace everything of this principal" action - that is the policy's rule (acl_otel's rules table), not a session's.

## Follow-ups

- The packaging spec: the console's Ingress / load balancer, the IdP registrations (the console's client, the API
  permissions on tresor's and kista's resources), the Managed Application's own.
- C2 and C4 (design/082), when the audit's storage is decided.
- Round 2's piece in another repository: hugr-node's `platform.telemetry` - a spec there.
- The integrations' spec: where telemetry and lineage go - the administrator's receiver (Application Insights on
  Azure; acl_otel speaks OTLP, so whether it goes straight in or through a collector is that spec's question), one
  deployed with the platform (an OTel Collector and Grafana; for lineage to be decided), or none; later perhaps the
  platform's own DuckLake as the telemetry store. It also gives the console its links.
- To check: what a session moved by duckdb-acl spec 135 keeps of an operator's tracing override (the override is on
  the old node's record).
- Languages: tresor's and kista's `locale`, once the console has more than English.
