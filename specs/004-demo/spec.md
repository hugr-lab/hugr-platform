# Spec 004: the platform in one cluster - the demo, end to end (kind in CI, minikube locally, AKS by hand)

- **Status**: implemented for minikube and kind (the nine checks pass on both); the AKS provider is written and waits
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

`test/demo/run.sh <kind | minikube | aks> [up | check | down | leftovers]` builds or loads the images, installs
everything and runs the checks. **minikube is the main run** (the owner, 2026-10-18): every attempt runs there and costs
nothing; AKS is for what only Azure can prove (Entra, the managed identities), run by hand when it is needed. The
provider is the only difference between the clusters:

| | kind (CI) | minikube (a developer's machine) | aks (by hand) |
| --- | --- | --- | --- |
| the cluster | made and deleted by the run (`HUGR_DEMO_KEEP=1` keeps it) | the profile `hugr-demo` (`--memory`, `--cpus` from `HUGR_DEMO_MEMORY` / `_CPUS`, 12 GiB and 6 by default), made if missing, kept | a resource group of its own, AKS with its OIDC issuer and workload identity, made by `up`, deleted by `down` |
| the images | `kind load` | `minikube image load` (skipped when the cluster already has that image) | an Azure Container Registry of the run's, attached to the cluster; built here for amd64 and pushed (below) |
| the architecture | the runner's | the machine's (arm64 on a Mac) | amd64 |
| the IdP | Keycloak in the cluster | Keycloak in the cluster | Entra ID: managed identities, app registrations (below) |
| the ServiceAccount issuer | read from the cluster's discovery | read from the cluster's discovery | the AKS OIDC issuer |
| `up`, `check`, `down` | one run does all three | one run does `up` and `check`; `down` deletes the namespaces | one run does all three, and `down` runs whatever happened before it; `up` / `check` / `down` alone are for debugging, the run's state in `~/.cache/hugr-platform-demo/aks` (mode 0700, the cluster's kubeconfig in it) between them |

| namespace | what | how it is made |
| --- | --- | --- |
| `idp` | Keycloak 26.6, TLS by the run's CA | a Deployment and a realm import (below) |
| `tresor` | tresor-server, its Kubernetes store | tresor-server's Helm chart, its image built from a checkout |
| `kista` | kista: the tenant `hugr`, its signed channel `demo` for the node's duckdb, the node's extensions as its releases; OpenBao beside it | kista's Helm chart, its image built from a checkout; the releases added by `kista admin` from the node's assets. The channel's signing key is in OpenBao Transit (dev mode, kubernetes auth for kista's ServiceAccount): kista refuses a key from a mounted file by design, and its own kind test does the same |
| `db` | PostgreSQL: the policy catalog's database `hugr` (made anew on every `up`) and a source database `sales` | a Deployment, plain inside the cluster: both logins are tresor's secrets |
| `hugr` | the operator, `HugrPlatform`, the groups `a` and `b` | `test/demo/overlay` (below), the image built here |

Every https server has a certificate of a CA made for the run. The platform's `caBundle` holds the system roots plus
that CA: `HUGR_CA_FILE` replaces the node's roots (hugr-node spec 006).

### The images

- **The operator** is built from this checkout and loaded into the cluster (or its registry, on AKS).
- **On AKS** every image is built here for `linux/amd64` and pushed to the run's registry. The Dockerfiles cross-compile
  on the build's own platform (BuildKit's platform arguments), which a local `docker build --platform` provides. The
  push's token (`az acr login --expose-token`) goes into a docker config in the run's state, never the developer's
  keychain, and dies with the registry.
- **tresor-server and kista** are built from a checkout each (`TRESOR_SERVER_DIR`, `KISTA_DIR`; otherwise their main
  is cloned), as hugr-node's bootstrap e2e builds them.
- **hugr-node** is built from the assets of a hugr-node release dry run, so nothing new is published.
  - The release workflow's dry run builds the node and its extensions for both platforms and keeps them as an
    artifact (`dist-linux_<arch>`, spec 008 there).
  - The script downloads that artifact for the cluster's architecture: `gh run download` from hugr-node's latest
    successful run that has one (a dispatch on main makes one), or `HUGR_NODE_DIST`, a directory already at hand.
  - It builds the image from hugr-node's `docker/Dockerfile`, as the release does.
  - The same assets seed kista (postgres_scanner and the others), so the node's duckdb and its extensions match.
  - `NODE_IMAGE` uses a published image instead (a tag, once one carries spec 010).

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
- **The demo's principals**: confidential clients whose service accounts carry the roles `analyst_a`, `analyst_b` and
  `platform_admin`. `platform_admin` holds the `cluster` and `policy` bundles (it writes the profile and the resource
  groups through a node) and `passthrough`, only so that the checks read a node's own settings through `ACL NATIVE`.
  `demo-seeder` (role `demo_seeder`) is tresor's administrator for the run: the first deployment writes and grants the
  secrets as it. The principals' secrets are made for the run and never leave its state (below).

### The IdP on AKS: Entra ID, as the Managed Application will

**Nothing of Azure is kept between runs** (the owner, 2026-10-18): a run makes its resource group, its registrations
and its identities, and deletes them at its end, a failed check included. Each resource carries the tags
`purpose=hugr-platform-demo` and `expires=<the run's day + 1>`; `leftovers` lists what a run left (a killed shell, a
lost connection), so it can be deleted by hand.

`up` makes, and `down` removes:

- **A user-assigned managed identity per group**, with a federated credential: the AKS OIDC issuer, the subject
  `system:serviceaccount:hugr:hugr-node-<g>`, the audience `api://AzureADTokenExchange`. Its client id is the group's
  `identity.clientID`. A node's tresor login is its projected token exchanged for an Entra token: no secret.
- **An app registration for tresor** (`api://<its app id>`, v2 tokens): the audience of the nodes' and the seeder's
  tokens. Its app roles are `hugr_node`, held by every group's identity, and `demo_seeder`. tresor reads `roles`, and
  a service by `azp`. The nodes' secrets are granted to `role:hugr_node`; each identity is an actor
  (`client:<its client id>`).
- **An app registration for the nodes** (`hugr`, v2 tokens): the audience of the principals' tokens, its app roles
  `analyst_a`, `analyst_b`, `platform_admin`.
- **App registrations for the demo's principals** (the seeder, the two analysts, the admin), each holding its app role,
  with client credentials of two days kept in the run's state (mode 0600), as kista's live run does. `up` waits until
  their tokens carry the roles: an assignment reaches the tokens in a while.

The policy's issuer is the tenant's (`https://login.microsoftonline.com/<tenant>/v2.0`), its client the principals'.
`down` deletes the app registrations and purges them, and their service principals, from Entra's recycle bin.
The signed-in `az` account needs to create app registrations, and role assignments in the run's resource group.
`leftovers` lists whatever of a run remains. Nothing of a token or a secret is printed.

### The first deployment

As spec 001 says: the installation's Job (`demo-seed`) runs the first SQL in its own process, unprefixed, on the
hugr-node image. The SQL carries the run's passwords, so it is a Secret mounted into the Job, deleted when the Job
ends, whatever its outcome. In order:

1. postgres_scanner from kista (`INSTALL` / `LOAD … FROM hugr`, autoinstall and autoload off): a postgres secret's type
   is the scanner's.
2. tresor, as `demo-seeder` by client credentials: the reserved secret `hugr_policy_catalog` (the catalog's login) and
   the source's `sales`, granted to `group:hugr-nodes`. kista needs no secret: the node reads the channel without a
   token, and an extension's integrity is the channel's signature.
3. The policy catalog in PostgreSQL, written directly as the database's operator:
   - the issuer and its client (Keycloak's realm, the audience `hugr`, roles from `realm_access.roles`);
   - the roles, the bundles of `platform_admin`, its mapping from the client's own claim (duckdb-acl spec 095);
   - the first profile: `CLUSTER ATTACH … AS sales (TYPE postgres, SECRET sales)`;
   - a virtual catalog `demo` over `sales.public.orders`, granted to the three roles;
   - the resource groups `a` and `b`, granted to `analyst_a` and `analyst_b` and to `platform_admin`.

Then the script creates `HugrPlatform` and the `HugrNodeGroup`s. On an `up` again the catalog is new, so the script
sets each group's `restartedAt`, as an operator would.

### The checks

Each check is a line `ok: …` or the run fails, naming the check. Logs are shown redacted (`eyJ…` → `<token>`). The
checks talk to the nodes through a small Go checker (`test/demo/check`, arrow-go's Flight SQL client): it gets a
principal's token from Keycloak (client credentials, its secret read from a file) and runs SQL on a node behind a
port-forward, as the principal. The checks change the profile: they run once per `up` (`all` does both).

1. **The entry.**
   - Both groups' pods become Ready.
   - Each node's log shows the orchestrator's document fetched, the login to tresor and the catalog attached by the
     secret's type.
   - Each node has a Lease with its summary.
   - Each group's status counts its nodes ready and reports `resourceGroupKnown: true`.
2. **The federation.** No secret of a node exists anywhere: a group's pods mount their doors' certificate, the
   projected token and the CA bundle, and read no Secret into their environment. The node client has no secret
   (`federated-jwt`), so check 1's login to tresor ran on the projected ServiceAccount token alone. On AKS, Entra's
   sign-in of the group's managed identity is the federated credential's.
3. **The engine against the pod.**
   - The node's `memory_limit`, `threads` and `max_temp_directory_size` are the pod's 80% / CPU / 90%.
   - A plain duckdb run in the same container, with no `HUGR_POD_*`, shows what DuckDB chooses from the cgroup by
     itself. The answer is recorded in spec 001's "Still to verify", whatever it is.
4. **A principal on its group's node.**
   - `analyst_a` reads the virtual table through Flight on a node of group `a`.
   - On a node of group `b` it is refused with `wrong_resource_group`, which names `a`.
5. **A hot item.** `ACL CLUSTER ATTACH … IN GROUP a`, written by `platform_admin` through a node of `a`:
   - every node of `a` applies it on its tick and reports `applied` at the new version;
   - group `a`'s status shows the target applied by all its nodes;
   - group `b` does not take it.
6. **A restart item, rolled.** `ACL CLUSTER SET memory_limit … IN GROUP a` (under the pod's ceiling) makes group `a`'s
   status list its nodes in `needsRestart`. Then `kubectl patch` sets the group's `restartedAt`, and the pods roll one
   at a time: a new one up, the old one drained. Afterwards nobody needs a restart, and the new nodes run the new
   limit.
7. **A dropped resource group.**
   - `DROP RESOURCE GROUP b` drains group `b`'s nodes by themselves: their Leases say `draining`, and the group says
     `Draining`.
   - Recreating it resumes them.
8. **The plan.**
   - Scaling `a` past `maxNodes` is refused by the webhook through `kubectl`, with its reason.
   - Scaling within it adds a node that enters by itself.
   - A scale-down to the former size removes a node: it drains and goes. That the operator picks the one with the
     fewest sessions (the pods' deletion cost) is the operator's unit test.
9. **The optional admission policy.** With `config/policy` applied, a pod created by hand to run as `hugr-node-a` is
   refused, and the groups keep rolling.

### The overlay

`test/demo/overlay` is `config/default` without cert-manager, with one replica and the image loaded into the cluster
(`imagePullPolicy: Never`). The script makes the operator's certificate (SAN `hugr-platform.hugr.svc`) and puts it in
the Secret `hugr-platform-tls`. It writes the run's CA into the webhook's `caBundle`, and waits until the webhook
answers a server-side dry run. Everything else is the shipped install.

### CI

A workflow in this repository (`.github/workflows/demo.yml`) runs the demo on kind on every push to main and on
dispatch, not on every PR: the run takes about half an hour. A node requests 250m of CPU there (`HUGR_DEMO_NODE_CPU`;
the node rounds it up to one thread), so check 8's four nodes fit the 4-core runner. On a failure the job prints the
cluster's state, redacted. AKS is never run by CI: it costs, and it needs a tenant's rights.

- It uses the node assets of hugr-node's latest successful release dry run on main.
- Both repositories are public, so the job's token can download the artifact.
- A run that finds no such artifact fails and says so; it never skips silently.

## Security

- **No credential at rest beyond the run's.**
  - The demo's secrets (the principals' client secrets, PostgreSQL's password, tresor's KEK, OpenBao's root token)
    are made per run. They are kept in the run's state (`~/.cache/hugr-platform-demo/<provider>/secrets`, mode 0600)
    and in Kubernetes Secrets of the demo's cluster, and never written to the repository or logged. The run never
    switches the developer's kube context.
  - A node has none: its tresor login is its ServiceAccount token, and its catalog login and kista's token come from
    tresor at use.
- **What the demo proves about secrets.** The checks grep the operator's, the nodes' and the seed Job's logs, and the
  platform namespace's resources, for the run's secrets and for tokens (a JWT's shape: two base64 JSON parts before a
  dot). Any hit fails the run, and so does the seed's Secret outliving its Job.

## Testing

The demo is itself the test: `test/demo/run.sh`, on minikube locally, on kind in CI, on AKS by hand. It needs docker,
kubectl, helm, go, gh, openssl, python3, and kind, minikube or az for its provider. Its checks are the list above. A
tool that is missing is a SKIP on a developer's machine and a failure in CI.

## Findings

From the minikube run (2026-10-18), recorded in spec 001's "Still to verify":

- **Keycloak federates Kubernetes ServiceAccount tokens** (26.6): an identity provider of type `kubernetes` and a
  `federated-jwt` client named by the token's subject. A node logs in to tresor with no secret anywhere.
- **The cgroup.** Plain DuckDB at our pin, in the 2 GiB-limit, 1-CPU-request container, chooses `memory_limit` 1.5 GiB
  (80% of the cgroup's limit, the node's own value) and `threads` 12 (the host's cores). A CPU request sets no quota,
  so there is nothing for DuckDB to read; the node's `HUGR_POD_CPU` is what makes it 1.

## Decisions for the owner

| # | question | proposal |
| --- | --- | --- |
| 1 | Where the node image comes from | hugr-node's release dry-run assets (`dist-linux_<arch>`): nothing new is published, and the node and its extensions match. `NODE_IMAGE` for a published tag once one carries spec 010 |
| 2 | When CI runs it | kind, on pushes to main and dispatch; not every PR (about 20 minutes) |
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
- spec 003 (the console): the demo's restart button is the console's.
- Packaging (the Helm chart, the Azure Managed Application): the demo is its first user.
