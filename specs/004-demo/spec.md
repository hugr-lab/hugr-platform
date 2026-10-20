# Spec 004: the platform in one cluster - the demo, end to end (kind in CI, minikube locally, AKS by hand)

- **Status**: implemented for minikube and kind (the checks pass on both; 4c, the console, on minikube since
  2026-10-20); the AKS provider is written and waits
  for its first live run until more of the platform is in (the owner, 2026-10-18). Accepted 2026-10-18: minikube
  locally, manual runs on AKS as tresor-server's and kista's, and nothing of Azure kept between runs
- **Date**: 2026-10-18

## Summary

Step 6 of phase 1: the whole platform in one Kubernetes cluster, run by one script, proving what spec 001 and hugr-node
spec 010 promise against real Kubernetes. The same script runs on three clusters: kind in CI, minikube on a developer's
machine, and AKS by hand (`up | check | down`, as tresor-server's and kista's live runs). It runs:

- the operator;
- two groups of hugr-node pods;
- the organisation's IdP logging the nodes in by their ServiceAccount tokens: Keycloak on kind and minikube, Entra ID
  on AKS (a managed identity per group with a federated credential, as the Azure Managed Application will);
- tresor-server;
- kista;
- PostgreSQL for the policy catalog and a source.

The front (spec 002) is not in it yet. The checks reach the nodes through port-forwards, and the re-seat of a principal
to its group comes with spec 002.

It also closes the two items spec 001 left "to verify":

- Keycloak's federation of Kubernetes ServiceAccount tokens;
- whether DuckDB at our pin reads the cgroup's memory and CPU.

## Problem

- The operator is proven against envtest only: no kubelet, no pods that run, no real node at the other end of
  `/v1/bootstrap` and `/v1/heartbeat`.
- hugr-node spec 010 is proven against a 30-line python fake of the orchestrator.
- The node's login to tresor by a federated workload identity has run only with Entra in mind. Outside Azure (private
  and sovereign Kubernetes) it needs an IdP that takes a Kubernetes ServiceAccount token as a client assertion. Without
  one, a node's client secret would have to live somewhere, which the platform refuses.

## Design

### What runs where

`test/demo/run.sh <kind | minikube | aks> [up | check | down | diagnose | console | leftovers]` builds or loads the
images, installs everything and runs the checks. `console` (kind, minikube, after an up) serves the console to a
person's browser (spec 003): Keycloak and the console forwarded to this machine on 8443 and 8444, the realm's person
`operator` signing in there (the issuer's name, `keycloak.idp.svc`, resolved to 127.0.0.1 by a line of the person's
`/etc/hosts`), metrics-server enabled on a minikube profile (the profile keeps it: a later check runs with a metrics
API). A cluster's context lives in the run's own kubeconfig
(`~/.cache/hugr-platform-demo/<provider>/kubeconfig`): the developer's `~/.kube/config` and current context are never
touched, whatever happens to the run. `diagnose` prints the cluster's state through the run's redaction. **minikube is the main run** (the owner, 2026-10-18): every attempt runs there and costs
nothing; AKS is for what only Azure can prove (Entra, the managed identities), run by hand when it is needed. The
provider is the only difference between the clusters:

| | kind (CI) | minikube (a developer's machine) | aks (by hand) |
| --- | --- | --- | --- |
| the cluster | made and deleted by the run (`HUGR_DEMO_KEEP=1` keeps it); kindnet enforces NetworkPolicies | the profile `hugr-demo` (`--memory`, `--cpus` from `HUGR_DEMO_MEMORY` / `_CPUS`, 12 GiB and 6 by default) with calico, so NetworkPolicies are enforced; made if missing, kept | the run's resource group, AKS with its OIDC issuer and cilium, made by `up`, deleted by `down` |
| the images | `kind load` | `minikube image load` (skipped when the cluster already has that image) | an Azure Container Registry of the run's, attached to the cluster; built here for amd64 and pushed (below) |
| the architecture | the runner's | the machine's (arm64 on a Mac) | amd64 |
| the IdP | Keycloak in the cluster | Keycloak in the cluster | Entra ID: managed identities, app registrations (below) |
| the ServiceAccount issuer | read from the cluster's discovery | read from the cluster's discovery | the AKS OIDC issuer |
| `up`, `check`, `down` | one run does all three | one run does `up` and `check`; `down` deletes the namespaces and the cluster-wide objects, and waits | one run does all three, and `down` runs whatever happened before it; `up` / `check` / `down` alone are for debugging, the run's state in `~/.cache/hugr-platform-demo/aks` (mode 0700) between them |
| a node's CPU request | 250m (`HUGR_DEMO_NODE_CPU`) | 1 | 250m |

| namespace | what | how it is made |
| --- | --- | --- |
| `idp` | Keycloak 26.6, TLS by the run's CA | a Deployment and a realm import (below) |
| `tresor` | tresor-server, its Kubernetes store | tresor-server's Helm chart, its image built from a checkout |
| `kista` | kista: the tenant `hugr`, its signed channel `demo` for the node's duckdb, the node's extensions as its releases; OpenBao beside it | kista's Helm chart, its image built from a checkout; every asset added by `kista admin` (one it holds answers so). Its profile is `dev`: SQLite with the blobs beside it, where another profile needs a store and a blob domain configured; signing is as strict (`software_keys`, the key in OpenBao). The channel's signing key is in OpenBao Transit (kubernetes auth for kista's ServiceAccount): kista refuses a key from a mounted file by design, and its own kind test does the same. OpenBao runs in dev mode, in memory: a restarted one has a new key, so kista's state starts anew then |
| `db` | PostgreSQL: the policy catalog's database `hugr` (made anew on every `up`) and a source database `sales` | a Deployment, plain TCP inside the throwaway cluster. Two logins, neither the superuser: `hugr_catalog` owns the catalog's database, `sales_reader` may only read `sales.orders` - so the source's login, which every node may use, cannot touch the catalog |
| `hugr` | the operator, `HugrPlatform`, the groups `a` and `b` | `test/demo/overlay` (below), the image built here |

Every https server has a certificate of a CA made for the run (30 days, made again two days before its end; a
certificate is kept while it has a day left, and a service whose certificate changed is restarted). The platform's
`caBundle` holds the system roots, read from the node's image in the cluster, plus that CA: `HUGR_CA_FILE` replaces the
node's roots (hugr-node spec 006).

### The images

- **The operator** is built from this checkout and loaded into the cluster (or its registry, on AKS).
- **On AKS** every image is built here for `linux/amd64` and pushed to the run's registry. tresor-server and kista
  cross-compile (BuildKit's platform arguments); the operator's and the node's images build emulated on an arm64
  machine. The push's token (`az acr login --expose-token`) goes into a docker config in the run's state, written so
  that docker keeps it in that file and never in the developer's keychain, and dies with the registry.
- **tresor-server and kista** are built from a checkout each (`TRESOR_SERVER_DIR`, `KISTA_DIR`; otherwise their main
  is cloned), as hugr-node's bootstrap e2e builds them.
- **hugr-node** is built from the assets of a hugr-node release dry run, so nothing new is published.
  - The release workflow's dry run builds the node and its extensions for both platforms and keeps them as an
    artifact (`dist-linux_<arch>`, spec 008 there).
  - The script downloads that artifact for the cluster's architecture: `gh run download` from hugr-node's latest
    successful run that has one (a dispatch on main makes one), or `HUGR_NODE_DIST`, a directory already at hand.
  - It builds the image from hugr-node's `docker/Dockerfile`, as the release does.
  - The same assets seed kista (postgres_scanner and the others), so the node's duckdb and its extensions match.
  - `NODE_IMAGE` uses a published image instead of the dry run's (a tag, once one carries spec 010). kista is still
    seeded from the dry run's assets (or `HUGR_NODE_DIST`): they must be built for the image's duckdb.

### The IdP on kind and minikube: Keycloak, with no client secret for a node

The realm `hugr`:

- **An identity provider of type `kubernetes`** whose issuer is the cluster's ServiceAccount issuer. Keycloak reads
  its keys with its own ServiceAccount token (`system:service-account-issuer-discovery`), as tresor-server's kind
  test does.
- **One client per group**, `clientAuthenticatorType: federated-jwt`, no secret, **named by its subject**
  `system:serviceaccount:hugr:hugr-node-<g>` - the group's `identity.clientID` is that name. The node's federated login
  sends its `client_id`, and Keycloak authenticates a federated assertion only as the client its `sub` names, so the
  two must be one. A node presents its projected token as the client assertion and gets a token for tresor. The
  assertion's audience is the realm's issuer: `HugrPlatform.spec.idp.federationAudience`.
- **The nodes are one group of Keycloak's**, `hugr-nodes`: each node client's service-account user is a member, and
  the token carries `groups`. tresor grants only to `role:` and `group:` principals, so the nodes' secrets are granted
  to `group:hugr-nodes` (the owner, 2026-10-18); each node client is also a tresor actor (`client:<its subject>`,
  `use`).
- **The demo's principals**: confidential clients whose service accounts carry a role each.
  - `analyst_a`, `analyst_b`: readers of the virtual catalog, each served by its group.
  - `platform_admin`: the `cluster` and `policy` bundles and nothing more - it writes the profile and the resource
    groups through a node, so the checks' writes prove those bundles.
  - `node_breakglass`: `passthrough`, writing nothing - the checks read a node's own state through `ACL NATIVE` as it
    (its settings, the profile's version, its attached databases).
  - `demo_seeder`: tresor's administrator for the run; the first deployment writes and grants the secrets as it.
- **The console's sign-in** (spec 003): the realm's public client `hugr-console` (PKCE, the console stand's forwarded
  origin, the audiences `hugr` and `duckdb-secrets` in one token) and one person, `operator`: `platform_admin` (the
  console's every page) and `secrets_admin` - a tresor administrator's role of its own, so tresor's console is mounted
  for the person, never the seeder's role with the first deployment's grants. Its password is made for the run like
  the principals' secrets.

  Their secrets are made for the run and never leave its state (below). Keycloak's first admin is the run's too, from
  a Secret: without one, its welcome page would let any local process create it through the port-forward.

### The IdP on AKS: Entra ID, as the Managed Application will

**Nothing of Azure is kept between runs** (the owner, 2026-10-18): a run makes its resource group, its registrations
and its identities, and deletes them at its end, a failed check included. **A run is its id** (`$WORK/aks.env`: the
id, the subscription, the tenant): the group `hugr-platform-demo-<id>`, the registry `hugrdemo<id>`, the apps
`hugr-platform-demo-<id>-<name>`, each tagged `purpose=hugr-platform-demo`, `run=<id>`, `expires=<the run's day + 1>`.
Every resource call names the run's subscription. `up` refuses a group whose `run` tag is another's; `down` deletes only
the run's group and the apps whose ids it recorded at creation, then purges them and their service principals from
Entra's recycle bin, retrying until each is gone - never anything found by a name, so runs in one tenant cannot delete
each other. `leftovers` lists what any run left (a killed shell, a lost connection), by the tag and the name prefix.

`up` makes, and `down` removes:

- **A user-assigned managed identity per group**, with a federated credential: the AKS OIDC issuer, the subject
  `system:serviceaccount:hugr:hugr-node-<g>`, the audience `api://AzureADTokenExchange`. Its client id is the group's
  `identity.clientID`. A node's tresor login is its projected token exchanged for an Entra token: no secret, and no
  workload-identity webhook (the node presents its own projected token).
- **An app registration for tresor** (`api://<its app id>`, v2 tokens, an assignment required): the audience of the
  nodes' and the seeder's tokens. Its app roles are `hugr_node`, held by every group's identity, and `demo_seeder`.
  tresor reads `roles`, and a service by `azp`. The nodes' secrets are granted to `role:hugr_node`; each identity is an
  actor (`client:<its client id>`).
- **An app registration for the nodes** (`hugr`, v2 tokens, an assignment required): the audience of the principals'
  tokens, its app roles `analyst_a`, `analyst_b`, `platform_admin`, `node_breakglass`.
- **App registrations for the demo's principals**, each holding its app role, with client credentials of two days kept
  in the run's state (mode 0600), as kista's live run does. `up` waits until every principal's token carries its role:
  an assignment reaches the tokens in a while.

The policy's issuer is the tenant's (`https://login.microsoftonline.com/<tenant>/v2.0`), its client the principals'.
The signed-in `az` account needs to create app registrations, to assign app roles (Application Administrator or the
like) and role assignments in the run's group. The cluster is one `Standard_D4s_v6` by default, a node requests 250m.
Nothing of a token or a secret is printed.

The AKS provider is written; its first live run waits until more of the platform is in (the owner, 2026-10-18).

### The first deployment

As spec 001 says: the installation's Job (`demo-seed`) runs the first SQL in its own process, unprefixed, on the
hugr-node image. The SQL carries the run's passwords, so it is a Secret mounted into the Job, deleted when the Job
ends, whatever its outcome. `acl_allow_anonymous_admin` is set in the Job's own process for its `ACL ADMIN` statements
and off again at its end: it is no setting of the catalog nor the profile, and the Job serves no door. In order:

1. postgres_scanner from kista (`INSTALL` / `LOAD … FROM hugr`, autoinstall and autoload off): a postgres secret's type
   is the scanner's.
2. tresor, as `demo-seeder` by client credentials: the reserved secret `hugr_policy_catalog` (`hugr_catalog`'s login)
   and the source's `sales` (`sales_reader`'s), granted to the nodes' principal (`group:hugr-nodes`, on AKS
   `role:hugr_node`). kista needs no secret: the node reads the channel without a token, and an extension's integrity
   is the channel's signature.
3. The policy catalog in PostgreSQL, written directly as its owner, attached by a secret of the Job's process only (a
   failed ATTACH names its path, so a password is never in one):
   - the issuer and its client (Keycloak's realm, the audience `hugr`, roles from `realm_access.roles`);
   - the roles, the bundles of `platform_admin` and `node_breakglass`, their mappings from the client's own claims
     (duckdb-acl spec 095);
   - the first profile: `CLUSTER ATTACH … AS sales (TYPE postgres, SECRET sales)`;
   - a virtual catalog `demo` over `sales.public.orders`, granted to the three roles;
   - the resource groups `a` and `b`, granted to `analyst_a` and `analyst_b`, to `platform_admin` and to
     `node_breakglass` (an administrator is placed like anyone, duckdb-acl spec 096).

Then the script creates `HugrPlatform` and the `HugrNodeGroup`s. On an `up` again the catalog is new, so the script
sets each group's `restartedAt`, as an operator would.

### The checks

Each check is a line `ok: …` or the run fails, naming the check. Whatever a failure shows is redacted: every value of
the run's secrets and every token. Every node's log is kept from its pod's start while the run lasts, the pods that go
away included: the drain checks and the secrets scan read them. The checks talk to the nodes through a small Go checker
(`test/demo/check`, arrow-go's Flight SQL client): it gets a principal's token from the IdP (client credentials, its
secret read from a file) and runs SQL on a node behind a port-forward, as the principal. The checks change the profile:
they run once per `up` (`all` does both).

1. **The entry.**
   - Both groups' pods become Ready.
   - Each node's log shows the orchestrator's document fetched, the login to tresor and the catalog attached by the
     secret's type.
   - Each node has a Lease with its summary.
   - Each group's status counts its nodes ready and reports `resourceGroupKnown: true`.
2. **The federation: a node holds no credential of its own.** On the live pods, as admitted: a Secret is mounted only
   as the doors' certificate, none reaches the environment (`env`, `envFrom`), a projected volume carries tokens,
   ConfigMaps and the downward API only, no image pull secret; no node runs a development identity (a key). The
   platform's namespace holds the operator's, the console's and the doors' TLS Secrets and nothing else. With check 1's login to
   tresor, the node's ServiceAccount token is all it logged in with: its IdP client has no secret.
3. **The engine against the pod.** The node's `memory_limit`, `threads` and `max_temp_directory_size` are the pod's
   80% / CPU / 90%. A plain duckdb in the same container, with an empty environment, shows what DuckDB chooses by itself
   (recorded in spec 001). The threads and the spill are the node's doing; the memory is DuckDB's own choice as well
   (80% of the cgroup's limit), so that part of the check cannot tell the two apart.
4. **A principal on its group's node.**
   - `analyst_a` reads the virtual table through Flight on a node of group `a`.
   - On a node of group `b` it is refused (`UNAVAILABLE`), the message naming `a` (the audit's reason is
     `wrong_resource_group`; the machine-readable form a client acts on comes with spec 002).
4b. **The front** (spec 002): the platform's `front` with two replicas and the run's certificate; the checker reaches it
   instead of a node. Group `a` is made the default: `analyst_b`, seated on a node of `a`, is re-seated on `b` by
   `a`'s refusal (the front's `hugr_front_reseats{reason="wrong_resource_group"}`), and `analyst_a` reads as on its
   node; with `b`'s node drained, `analyst_b` is refused after the wait - `hugr: no node of group b can take a new
   session - retry` - and never seated on `a`. The checker keeps no cookie: its `DoGet` reaches the node by the label
   in its ticket. The front's logs are in check 12's scan.
4c. **The console** (spec 003): the operator serves it with the run's certificate (Secret `hugr-console`), the platform
   names it (`spec.console`); the checker calls its API through its Service with a principal's token (`-console`).
   Its config answers before a sign-in; `analyst_a` is refused the fleet, a node's sessions, a drain and a scale (403);
   `platform_admin` reads the fleet (every node admitting), a node's load report, pod, memory, extensions, log and
   audit ring, and the cluster's page; `analyst_a`'s own session (the checker's `-hold`, a driver's cookie) is traced,
   cleared and ended, ending it again a 404; `b`'s node is drained and resumed, every call with a new token of the
   admin's; a scale of `a` to 4 is the webhook's refusal (422), one of `b` within the plan is the console's field
   manager's, the admin named on the group; tresor's and kista's modules come through the mounts, and tresor's
   `whoami` answers the seeder there (the demo's kista signs nobody in: the platform names no kista audience, so the
   shell offers no Extensions page). The admin holds the `meta` and `environment` function categories (the engine's
   memory and extensions are duckdb's own functions).
5. **A hot item.** `ACL CLUSTER ATTACH … IN GROUP a`, written by `platform_admin` through a node of `a`:
   - every node of `a` applies it on its tick and reports `applied` at the new version;
   - group `a`'s status shows the target applied by all its nodes;
   - group `b`'s node reaches the same version and has no such database.
6. **A restart item, rolled.** `ACL CLUSTER SET memory_limit … IN GROUP a` (under the pod's ceiling) makes group `a`'s
   status list its nodes in `needsRestart`. Then `kubectl patch` sets the group's `restartedAt`; the Deployment rolls one
   at a time (`maxSurge 1`, `maxUnavailable 0`), and each old node's log shows it drained and stopped. Afterwards nobody
   needs a restart, and every new node runs the new limit.
7. **A dropped resource group.**
   - `DROP RESOURCE GROUP b` drains group `b`'s nodes by themselves: their Leases say `draining`, and the group says
     `Draining`.
   - Recreating it resumes them.
8. **The plan.**
   - Scaling `a` past `maxNodes` is refused by the webhook through `kubectl`, with its reason.
   - Scaling within it adds a node that enters by itself.
   - A scale-down to the former size removes a node, whose log shows it drained and stopped. That the operator picks the
     one with the fewest sessions (the pods' deletion cost) is the operator's unit test.
9. **The network.** Probe pods show the NetworkPolicies enforced: a pod of no group is refused at the operator's node
   API and a group's pod admitted; a node's door refuses a pod of no role and admits the front's.
10. **The optional admission policy.** With `config/policy` applied, a pod created by hand to run as `hugr-node-a` is
    refused, and the groups keep rolling.

Then the secrets scan (Security).

### The overlay

`test/demo/overlay` is `config/default` without cert-manager, with one replica and the image loaded into the cluster
(`imagePullPolicy: Never`). The script makes the operator's certificate (SAN `hugr-platform.hugr.svc`) and puts it in
the Secret `hugr-platform-tls`. It writes the run's CA into the webhook's `caBundle`, and waits until the webhook
answers a server-side dry run. Everything else is the shipped install.

### CI

A workflow in this repository (`.github/workflows/demo.yml`) runs the demo on kind on every push to main and on
dispatch, not on every PR: the run takes about half an hour. A node requests 250m of CPU there (`HUGR_DEMO_NODE_CPU`;
the kubelet rounds a request up to whole CPUs, so the node sees one), so check 8's four nodes fit the 4-core runner.
The run's generated values are masked in the job's log (`::add-mask::`); on a failure the job prints `diagnose`.
AKS is never run by CI: it costs, and it needs a tenant's rights.

- It uses the node assets of hugr-node's latest successful release dry run on main.
- The job's own token reads hugr-node's artifacts (both repositories are public; proven by run 39180509095,
  2026-10-19). A fine-grained token (Actions: read on hugr-node) in the secret `HUGR_NODE_ARTIFACTS_TOKEN` is used
  instead when set; gh's refusal is reported as such.
- The demo step has its own limit (55 minutes) under the job's (75): a hang fails the step and the diagnosis still
  runs. The checker and `kubectl exec` are bounded too (`timeout`, 120 s). The first main run hung in check 3: a
  backgrounded shell function kept a `$(...)` pipe open under bash 5 (macOS's bash 3.2 does not), so the background
  processes are kubectl itself.
- A run that finds no such artifact fails and says so; it never skips silently.
- tresor-server and kista are built from their main: the run breaks when theirs does.

## Security

- **No credential at rest beyond the run's.**
  - The demo's secrets (the principals' client secrets, the console person's password, the PostgreSQL logins, tresor's
    KEK, OpenBao's root token, Keycloak's first admin) are made per run. They are kept in the run's state
    (`~/.cache/hugr-platform-demo/<provider>/secrets`, mode 0600) and in Kubernetes Secrets of the demo's cluster, and
    never written to the repository or logged. A Secret is made from its file and a password goes to psql on stdin:
    no secret is on a command line or in an environment.
  - A node has none: its tresor login is its ServiceAccount token, and its catalog and source logins come from tresor
    at use.
  - The developer's kubeconfig, kube context and keychain are never touched (the run's own kubeconfig and docker
    config).
  - OpenBao's dev mode prints its own root token in its own log: the throwaway cluster's, blanked wherever the run
    shows a log, and the one log the scan leaves out.
- **What the demo proves about secrets.** The scan reads every node's log from its start (the gone ones included, and
  a restarted container's previous one), the operator's, the seed Job's and the stack's logs, the events, and the
  platform namespace's resources. It looks for every value of the run's secrets, for a JWT's shape (two base64 JSON
  parts before a dot) and for a private key. Any hit fails the run, and so does the seed's Secret outliving its Job.
  Check 2 inventories the namespace's Secrets.
- **The run's CA** is the nodes' only trust store besides the system roots and has no name constraints; its key stays
  in the run's state.

## Testing

The demo is itself the test: `test/demo/run.sh`, on minikube locally, on kind in CI, on AKS by hand. It needs docker,
kubectl, helm, go, gh, git, openssl, python3, and kind, minikube or az (with curl) for its provider. Its checks are the list above. A
tool that is missing is a SKIP on a developer's machine and a failure in CI.

## Findings

From the minikube run (2026-10-18), recorded in spec 001's "Still to verify":

- **Keycloak federates Kubernetes ServiceAccount tokens** (26.6): an identity provider of type `kubernetes` and a
  `federated-jwt` client named by the token's subject. A node logs in to tresor with no secret anywhere.
- **The cgroup.** Plain DuckDB at our pin, in the 2 GiB-limit, 1-CPU-request container, chooses `memory_limit` 1.5 GiB
  (80% of the cgroup's limit, the node's own value) and `threads` 12 (the host's cores). A CPU request sets no quota,
  so there is nothing for DuckDB to read; the node's `HUGR_POD_CPU` is what makes it 1. A CPU limit (a quota) was not
  tried.

## Decisions for the owner

| # | question | proposal |
| --- | --- | --- |
| 1 | Where the node image comes from | hugr-node's release dry-run assets (`dist-linux_<arch>`): nothing new is published, and the node and its extensions match. `NODE_IMAGE` for a published tag once one carries spec 010 |
| 2 | When CI runs it | kind, on pushes to main and dispatch; not every PR (about half an hour) |
| 3 | The spec's number | 004: 002 (the front) and 003 (the console) keep the numbers spec 001 gave them |
| 4 | Where it runs (the owner) | minikube on a developer's machine, kind in CI, and AKS by hand, as tresor-server's and kista's live runs |

## Alternatives considered

- **Publishing a hugr-node image of every main commit** (`:edge`). It is a new public artifact, and the dry run's
  assets give the same image without one.
- **cert-manager in the cluster for every certificate.** It is one more install and one more thing to wait for. The
  shipped install still uses it; the demo's own CA makes the run self-contained.
- **A dev identity (a private key per node) instead of Keycloak's federation.** It would prove nothing about the
  secretless login that outside-Azure installations depend on.

## Follow-ups

- spec 002 (the front): the re-seat of a principal of another group joins check 4. Clients go through the front, not
  port-forwards.
- spec 003 (the console): a group's restart through the console (check 6 sets `restartedAt` itself).
- Packaging (the Helm chart, the Azure Managed Application): the demo is its first user.
