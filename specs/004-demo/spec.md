# Spec 004: the platform in one cluster - the demo, end to end (kind in CI, minikube locally, AKS by hand)

- **Status**: accepted (the owner, 2026-10-18: minikube locally, manual runs on AKS as tresor-server's and kista's, and
  nothing of Azure kept between runs)
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
| the images | `kind load` | `minikube image load` | an Azure Container Registry of the run's, attached to the cluster; built there by `az acr build` |
| the architecture | the runner's | the machine's (arm64 on a Mac) | amd64 |
| the IdP | Keycloak in the cluster | Keycloak in the cluster | Entra ID: managed identities, app registrations (below) |
| the ServiceAccount issuer | read from the cluster's discovery | read from the cluster's discovery | the AKS OIDC issuer |
| `up`, `check`, `down` | one run does all three | one run does `up` and `check`; `down` deletes the namespaces | one run does all three, and `down` runs whatever happened before it; `up` / `check` / `down` alone are for debugging, the run's state in `~/.cache/hugr-platform-aks-live` (mode 0700) between them |

| namespace | what | how it is made |
| --- | --- | --- |
| `idp` | Keycloak 26.6, TLS by the run's CA | a Deployment and a realm import (below) |
| `tresor` | tresor-server, its Kubernetes store | tresor-server's Helm chart, its image built from a checkout |
| `kista` | kista: a tenant, a channel for the node's duckdb, the extensions the profile names | kista's Helm chart, its image built from a checkout; seeded by hugr-node's `scripts/publish_to_kista.sh` |
| `db` | PostgreSQL: the policy catalog and a source database | a Deployment, TLS by the run's CA |
| `hugr` | the operator, `HugrPlatform`, the groups `a` and `b` | `config/default` with a kind overlay (below), the image built here |

Every https server has a certificate of a CA made for the run. The platform's `caBundle` holds the system roots plus
that CA: `HUGR_CA_FILE` replaces the node's roots (hugr-node spec 006).

### The images

- **The operator** is built from this checkout and loaded into the cluster (or its registry, on AKS).
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
- **One client per group** (`hugr-node-a`, `hugr-node-b`):
  - `clientAuthenticatorType: federated-jwt`;
  - its subject `system:serviceaccount:hugr:hugr-node-<g>`;
  - no secret.

  A node presents its projected token as the client assertion and gets a token for tresor. The token's audience is
  the realm's issuer: `HugrPlatform.spec.idp.federationAudience`.
- **The demo's principals**: confidential clients whose service accounts carry the roles `analyst_a`, `analyst_b` and
  `platform_admin` (the `cluster` and `policy` bundles: the one that writes the profile and the resource groups through
  a node). Their secrets are made for the run and never leave the script.

### The IdP on AKS: Entra ID, as the Managed Application will

**Nothing of Azure is kept between runs** (the owner, 2026-10-18): a run makes its resource group, its registrations
and its identities, and deletes them at its end, a failed check included. Each resource carries the tags
`purpose=hugr-platform-demo` and `expires=<the run's day + 1>`; `leftovers` lists what a run left (a killed shell, a
lost connection), so it can be deleted by hand.

`up` makes, and `down` removes:

- **A user-assigned managed identity per group**, with a federated credential: the AKS OIDC issuer, the subject
  `system:serviceaccount:hugr:hugr-node-<g>`, the audience `api://AzureADTokenExchange`. Its client id is the group's
  `identity.clientID`. A node's tresor login is its projected token exchanged for an Entra token: no secret.
- **An app registration for tresor** (`api://<its app id>`): the audience of the nodes' tokens.
- **App registrations for the demo's principals** (`analyst_a`, `analyst_b`, `platform_admin` as app roles), with
  client credentials of two days kept in the run's state (mode 0600), as kista's live run does.

The policy's issuer is the tenant's (`https://login.microsoftonline.com/<tenant>/v2.0`), its client the principals'.
The signed-in `az` account needs to create app registrations, and role assignments in the run's resource group.
`leftovers` lists whatever of a run remains. Nothing of a token or a secret is printed.

### The first deployment

As spec 001 says: the installation's Job runs the first SQL in its own process, unprefixed, on the hugr-node image.
It writes the policy catalog in PostgreSQL directly, as the database's operator:

- the issuer and its client (Keycloak's realm, the demo's audience, roles from `realm_access.roles`);
- the roles, and the bundles of `platform_admin`;
- a virtual catalog over the source, granted to both roles;
- the resource groups `a` and `b`, granted to `analyst_a` and `analyst_b`;
- the first profile: the source's postgres_scanner from kista.

tresor gets, from the script, the reserved secret `hugr_policy_catalog` (the catalog's login), kista's http secret and
the source's secret, each granted to the groups' identities. Then the script creates the `HugrNodeGroup`s.

### The checks

Each check is a line `ok: …` or the run fails, naming the check. Logs are shown redacted (`eyJ…` → `<token>`). The
checks talk to the nodes through a small Go checker (`test/kind/check`, arrow-go's Flight SQL client): it gets a
principal's token from Keycloak (client credentials) and runs SQL on a node behind a port-forward, as the principal.

1. **The entry.**
   - Both groups' pods become Ready.
   - Each node's log shows the orchestrator's document fetched, the login to tresor and the catalog attached by the
     secret's type.
   - Each node has a Lease with its summary.
   - Each group's status counts its nodes ready and reports `resourceGroupKnown: true`.
2. **The federation.** The node's login to tresor ran on its projected ServiceAccount token alone, and no secret of a
   node exists anywhere. On Keycloak its event log names the client `hugr-node-<g>` authenticated by `federated-jwt`;
   on AKS, Entra's sign-in of the group's managed identity is the federated credential's.
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
   - A scale-down removes the node with the fewest sessions.
9. **The optional admission policy.** With `config/policy` applied, a pod created by hand to run as `hugr-node-a` is
   refused, and the groups keep rolling.

### The overlay

`test/demo/overlay` is `config/default` without cert-manager. The script makes the operator's certificate (SAN
`hugr-platform.hugr.svc`) and puts it in the Secret `hugr-platform-tls`. It writes the run's CA into the webhook's
`caBundle`. Everything else is the shipped install.

### CI

A workflow in this repository runs the demo on kind on every push to main and on dispatch, not on every PR: the run
takes about 20 minutes. AKS is never run by CI: it costs, and it needs a tenant's rights.

- It uses the node assets of hugr-node's latest successful release dry run on main.
- Both repositories are public, so the job's token can download the artifact.
- A run that finds no such artifact fails and says so; it never skips silently.

## Security

- **No credential at rest beyond the run's.**
  - The demo's secrets (Keycloak's admin, the principals' client secrets, PostgreSQL's password) are made per run,
    held in the script's variables and in Kubernetes Secrets of the throwaway cluster, and never written to the
    repository or logged.
  - A node has none: its tresor login is its ServiceAccount token, and its catalog login and kista's token come from
    tresor at use.
- **What the demo proves about secrets.** The checks grep the operator's and the nodes' logs, and every resource the
  operator wrote, for the run's secrets and for tokens (`eyJ`). Any hit fails the run.

## Testing

The demo is itself the test: `test/demo/run.sh`, on minikube locally, on kind in CI, on AKS by hand. It needs docker,
kubectl, helm, go, gh, openssl, python3, and kind, minikube or az for its provider. Its checks are the list above. A
tool that is missing is a SKIP on a developer's machine and a failure in CI.

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
