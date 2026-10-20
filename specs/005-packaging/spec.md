# Spec 005: packaging - the platform as one Helm chart, then the Azure Managed Application

- **Status**: accepted 2026-10-20 for stage A (stage B is detailed when A lands; hugr_node's path after duckdb `v2.0.0`
  is a proposal to confirm)
- **Date**: 2026-10-20

## Summary

Today the demo's script installs the platform: a kustomize overlay for the operator, the `HugrPlatform` and the groups
typed into heredocs, and certificates the run makes itself. An organisation needs one package. The work has two
stages:

- **Stage A** (this spec's first PRs):
  - the chart `hugr-platform`, which installs the operator, the platform, its first groups, its certificates, kista
    and tresor-server;
  - the operator's release: the image and the chart, both on a tag;
  - the demo installs through the chart, so every run on main tests the package an organisation installs.
- **Stage B**: Entra's registrations, the Azure Managed Application, and the first live AKS run.

It builds on two decisions of 2026-10-20 (the phase-1 doc):

- **Decision 6**: a door's key is generated in its pod by cert-manager's csi-driver and never reaches etcd.
- **Decision 7**: kista runs in the cluster and mirrors the channels. Nodes never reach a hosted repository.

## Problem

- **There is no package.** `config/default` is kustomize, and it needs an Issuer the installer brings. The
  `HugrPlatform` and its groups are written by hand. hugr-platform has no release workflow, so no operator image or
  chart has ever been published.
- **The demo proves the code, not the install.** Nothing tests what an organisation would run.
- **Decision 6**: a door's certificate is a Secret today (`tls.secretRef`), so its key sits in etcd.
- **The doors read their pair once.** Both read their certificate and key at serve time
  (`acl_flight_serve`: `options.tls_certificates`, and `acl_quack_serve` the same way). A renewed certificate
  reaches a node only at its next start.
- **Decision 7**: the nodes' repository is whatever the installer points `repository.prefix` at. The package must
  bring the in-cluster kista and its upstreams.

## Design

### Stage A: what the chart installs

`charts/hugr-platform`, for Helm 3.14+ and 4:

| part | how | default |
| --- | --- | --- |
| the operator | the CRDs in `crds/` (Helm installs them first and never upgrades or deletes them: as templates, a `HugrPlatform` in the same release would not map; a changed CRD is `kubectl apply`'d before an upgrade), RBAC as spec 001 states it, the Deployment, its Service, the webhook with cert-manager's CA injection, the NetworkPolicy; the ValidatingAdmissionPolicies of `config/policy` optional | on |
| `HugrPlatform` | rendered from `platform.*` values: node image, IdP, tresor, repository, CA bundle, front, console, sizes, plan | on |
| the first groups | `groups[]` values as post-install hooks: made once, after the operator - whose webhook judges a group - is ready (`--wait`). A hook is no part of the release: `helm upgrade` never resets a scale made in the console or with kubectl, and the groups stay when the release goes | none |
| certificates | cert-manager is required. The platform's internal CA and its Issuer (`hugr-internal`) are the namespace's own, so only a pod or a Certificate of this namespace is issued from them: a self-signed root the chart makes, or the organisation's CA as a Secret of the namespace. It issues the operator's serving and webhook certificate and the doors' (csi-driver, below). The operator writes the CA bundle (`hugr-ca-bundle`): its image's public roots, the internal CA (its own certificate's `ca.crt`), the organisation's CAs. The front's and the console's public certificates come from the organisation's issuer for their names, or from Secrets the installer brings | on |
| kista | a subchart: kista's own chart, `oci://ghcr.io/hugr-lab/charts/kista`. It is the nodes' only repository, and `repository.prefix` defaults to it. Its store (PostgreSQL) and its key source come from values. Its upstreams: DuckDB's core and community repositories, plus our channel or the organisation's hosted tenant, the latter as a private upstream by kista's ServiceAccount token (kista spec 0009, phase 3) | on |
| tresor-server | a subchart: `oci://ghcr.io/hugr-lab/charts/tresor-server` (v0.6.0 is out), its store from values; `tresor.enabled: false` + `platform.tresor.host` for one the organisation already runs | on |

The chart leaves out:

- the IdP;
- PostgreSQL (the catalog's, kista's and tresor's databases: the organisation's own, or the cloud's managed one);
- cert-manager and its csi-driver, which are prerequisites of the cluster;
- an OpenTelemetry collector. Telemetry stays an endpoint value (`platform.telemetry`) until the integrations spec.

**Values** hold no credential: a database login lives in tresor (decision 4 of 2026-10-15), and a key lives in its
key source. A password in values would also land in Helm's release Secret.

### Stage A: the doors' keys (decision 6)

- **The CRD**: `HugrNodeGroup.spec.tls` gains `csi: {issuerRef, duration}` beside `secretRef`, and the chart's groups
  use it. The operator mounts the csi-driver volume into the node's pod with these attributes:
  - `csi.cert-manager.io/issuer-name`, `issuer-kind` and `issuer-group`;
  - `dns-names`: `hugr-node-<group>.<ns>.svc` and `.cluster.local`;
  - `fs-group`: the node's group (10001). The pod's `fsGroup` reaches the files as well (measured on minikube with
    cert-manager v1.21.2 and csi-driver v0.16.0), but the attribute keeps the files readable whatever a driver's
    version defaults to;
  - `duration`.
- **The key** is generated on the pod's node, kept in tmpfs, and never written to a Secret. cert-manager renews the
  certificate in place.
- **Renewal**: the doors must pick up a renewed pair, and that is a duckdb-acl spec inside this stage. A door serving
  from files takes the new pair for new connections. The spec confirms feasibility:
  - Flight: gRPC's file-watching certificate provider, reached through Arrow Flight's `builder_hook`;
  - quack: a fresh SSL context.

  hugr-node passes the paths it already has (`HUGR_FLIGHT_CERT`, `HUGR_QUACK_CERT`).
- **The fallback** is visible, not silent. The console already shows each door's `notAfter` as its last handshake
  saw it. A certificate that a node still serves past renewal is flagged there.
- **`secretRef` stays** for an installation that brings its own. A group names exactly one of the two: the CRD's
  rule refuses both or neither at the API server, and the webhook checks it again with the Secret's name rule and a
  `duration` of at least 1h.
- **Who can get a door's certificate.** The csi-driver issues to any pod that mounts its volume, so anyone who may
  create pods in the namespace can ask the platform's issuer for a node's name. That is spec 001's boundary: such a
  person can already run a pod as a group's ServiceAccount. A group's editor picks only the issuer; the names are
  the operator's. cert-manager's approver-policy can narrow it further, as a follow-up.

### Stage A: the spill volume, a group's resource (the owner, 2026-10-20)

DuckDB spills to `temp_directory` when memory runs out, so the volume must be fast and big enough for the group's
work: an ETL group's large and on local SSD, a BI group's small. Today only the platform's size names it
(`sizes.<s>.temp: {medium: local, size}`), as an `emptyDir` with a `sizeLimit`, and that falls short in three ways:

- **Its speed is the node's.** An `emptyDir` sits on the kubelet's disk, which in AKS is by default the network OS
  disk. It is fast only on a pool with an ephemeral OS disk and `kubeletDiskType: Temporary`, on a SKU with a local
  disk (the `d` series, E16ds_v5: 600 GiB; NVMe on the L series). That is the pool's configuration: stage B.
- **Its size is not reserved.** A `sizeLimit` caps the volume, but the scheduler knows nothing of it, and a pod may land
  where it cannot fit, to be evicted mid-query.
- **There is no choice without a local disk**: a sovereign cluster, a pool of network disks only.

The design:

- **`HugrNodeGroup.spec.temp`** (`medium`, `size`, `storageClassName`) is the group's. The size's `temp` stays its
  default, so a group written today keeps working.
- **`medium: local`**: the `emptyDir`, plus an `ephemeral-storage` request of its size, so the scheduler places the
  pod only where the disk fits.
- **`medium: volume`**: a generic ephemeral volume, a PVC per pod from `storageClassName`, made with the pod and deleted
  with it. Azure Premium SSD v2 or Ultra take an IOPS and throughput of their own; a sovereign cluster uses local PVs
  (TopoLVM, OpenEBS LVM) or its cloud's fast SSD class.
- **The directory**: the operator mounts the volume and tells the node `HUGR_TEMP_DIRECTORY` (spec 001). DuckDB's
  `temp_directory` moves through the profile when needed (`ACL CLUSTER SET temp_directory … IN GROUP g`). The node keeps
  `max_temp_directory_size` under the volume, so an overflow fails one query rather than evicting the pod.
- **Stage B**: each of the plan's sizes names a SKU with a local disk, and a group's pool gets an ephemeral OS disk and
  `kubeletDiskType: Temporary`. The Managed Application checks that a group's `local` temp fits the SKU's local disk,
  and offers `volume` otherwise.

### Stage A: hugr_node on the node

- **Today it is never installed.** hugr_node is linked into the image's duckdb (decision 1 of 2026-10-15; hugr-node's
  `duckdb_extension_statically_link(hugr_node)`), so its version is the image's tag.
- **An update on a live cluster** is a new tag in `HugrPlatform.spec.node.image`. Kubernetes rolls each group one
  node at a time: the new pod is up before the old one drains (spec 001).
- **Any path needs a restart.** A loaded extension cannot be replaced in a running duckdb process, so a new hugr_node
  means new processes whichever way it arrives. The paths differ only in what changes: the whole image, or one
  extension's version.
- **It cannot be a profile item.** hugr_node is what reads the cluster profile, so its version can never be an item
  of that profile.
- **The in-cluster kista still carries it.** It mirrors our channel with the loadable hugr_node, which is already among
  hugr-node's release assets. So the path after duckdb `v2.0.0` is ready when decision 1 is revisited.
- **After `v2.0.0`** (proposal, to decide with decision 1):
  - the official duckdb image;
  - the entrypoint runs `INSTALL hugr_node FROM <repository> VERSION <v>`, then `LOAD`, then `PRAGMA hugr_node_run`;
  - the version and the repository come from the pod's env, which the operator takes from `HugrPlatform`. They cannot
    come from the bootstrap document, because hugr_node is what fetches that document;
  - a new version changes the pod template, so it rolls the groups exactly as a new image does.

  A loadable is built for one duckdb version, so a new duckdb still means a new hugr_node.

### Stage A: the first deployment

The order is one `helm install` plus one action of the installer in tresor:

1. **Prerequisites**: cert-manager with its csi-driver, the databases, and the IdP's registrations (Keycloak by hand;
   Entra in stage B).
2. **`helm install hugr`**: tresor, kista, the operator, the platform, the first groups. The nodes start, find no
   catalog login in tresor and exit. Kubernetes restarts them with backoff, and the console shows them `failing` with
   the reason.
3. **The installer writes the catalog's login into tresor** as `hugr_policy_catalog` and grants it to the nodes'
   principal, with tresor's console or CLI. It is a credential, so it is never written into the chart, its values or
   a Job.
4. **The first policy is the node's** (the owner, 2026-10-20). It replaces an install Job run as the first group's
   ServiceAccount, which the admission policies refuse: only a group's ReplicaSet may run as the group.
   - **What it is:** `HugrPlatform.spec.firstPolicy`, structured fields, never SQL: the issuer's URL, its audiences, the
     roles claim, and the claim value of the administrators.
   - **How it travels:** in each node's bootstrap document, from the operator.
   - **Where it is applied:** a node that attaches a catalog with no issuer writes it once, in one transaction, in its
     own process before it serves. It writes the issuer, its client, the role `platform_admin` with the `cluster` and
     `policy` bundles, and the mapping of the administrators' claim to that role.
   - **Never overwrites:** a catalog that has an issuer is left alone, and nodes starting together race to one write.
   - **The specs:** a small spec in hugr-node (the document's field, the write) and one in this repository (the field
     and the document).

   The nodes come up. The administrator signs into the console and does the rest there.

### Stage A: the operator's release

`.github/workflows/release.yml`, as hugr-node spec 008 and kista spec 0013 do it:

- a tag `v*` publishes `ghcr.io/hugr-lab/hugr-platform:<tag>` (amd64 and arm64) and the chart
  `oci://ghcr.io/hugr-lab/charts/hugr-platform:<version>`. Both are published only after the kind demo has passed
  through the chart on the tag's commit;
- a PR that touches `charts/` or the workflow dry-runs it: `helm lint --strict`, `helm template` over the example
  values, and the demo.

### Stage A: the demo through the chart

- **Install**: `install_operator` and `install_platform` become one `helm upgrade --install` with
  `test/demo/values/<provider>.yaml`.
- **Certificates**: the run's own door certificates go. The demo installs cert-manager and its csi-driver at pinned
  versions, and gives the chart its CA as the platform's internal CA.
- **tresor and kista**: subcharts packaged from their source at the dependency's version into the chart's `charts/`.
  The demo runs their main, as it does today.
- **Keycloak, PostgreSQL and OpenBao** stay the demo's own.
- **Check 2** gets stronger: no door Secret is left in the namespace, and each node's certificate comes from its own
  pod.

### Stage B: Azure (its own PRs, detailed when stage A lands)

- **Entra**:
  - app registrations for the nodes' API, the console's SPA, tresor and kista;
  - a user-assigned managed identity per group, with a federated credential for its ServiceAccount (what
    `provider-aks.sh` does by hand);
  - app roles for the platform's roles.
- **The Managed Application**:
  - `mainTemplate` (Bicep compiled to ARM): AKS with a node pool per group (a memory-optimized SKU from the plan),
    Azure Database for PostgreSQL, Key Vault for kista's signing key, and the chart;
  - `createUiDefinition`: the plan becomes the groups;
  - the publisher's access is just-in-time, never standing (spec 001).
- **The databases** (the owner, 2026-10-20): the Managed Application offers two managed engines, Azure SQL Database or
  Azure Database for PostgreSQL Flexible Server. One server holds three databases: `tresor`, `kista` and `hugr` (the
  policy catalog). The goal is that each of the three logs in by its managed identity, with no password, on either
  engine. The server is Entra-only.
  - **tresor and kista** log in by their managed identity's Entra token, with no password (tresor's `auth: entra`,
    kista's `store.login.kind: entra`). The deployment script creates their database users for the identities
    (`CREATE USER … FROM EXTERNAL PROVIDER`, `pgaadauth_create_principal`).
  - **The policy catalog on Azure SQL**: a node logs in by its group's Entra token through mssql and the linked `azure`
    extension (mssql reads a token per connection), so `hugr_policy_catalog` in tresor names the host and the
    database only. Still to verify: the group's workload identity reaching the `azure` extension's credential chain.
  - **The policy catalog on PostgreSQL** has one gap. An Entra token lives about an hour, and postgres_scanner takes
    its password once, so a new connection after that fails. Two ways close it: postgres_scanner re-reading a token as
    the password per connection (a patch of ours), or a dynamic secret in tresor that hands out a fresh token ("CREATE
    DYNAMIC SECRET", decision 5 of 2026-10-15). Until one lands, the catalog on PostgreSQL logs in by a password that
    only tresor holds: the deployment script makes it and writes it straight into tresor, and the server admits a
    password for that one login. Which comes first is the owner's open question.
  - **The first deployment is automatic.** The deployment script creates the server, the databases and the users,
    writes `hugr_policy_catalog` into tresor and grants it to the groups' identities. The first node writes the first
    policy (`firstPolicy`, decision 4 of stage A).
- **The images**: which registry the Marketplace requires for them is to check.
- **The first live AKS run**: `test/demo/run.sh aks`, through the chart.

## Security

- **No credential** in the chart, its values, a ConfigMap or the bootstrap document. The first policy is written by a
  node, as itself: its projected ServiceAccount token logs it in to tresor, and the fields name an issuer, never a
  secret.
- **A door's key** never reaches etcd. The operator's own webhook certificate stays a cert-manager Secret: the API
  server's CA injection reads it.
- **The operator's RBAC** is spec 001's. Spec 001's boundary holds: the namespace is the platform's alone. The
  chart's notes say so, because anyone who may create pods there can act as a group.
- **Repositories**: the nodes' repository is the in-cluster kista. Only kista reaches its upstreams. A node egress
  policy generated from the profile stays a follow-up (decided 2026-10-14).

## Testing

- **On every PR**: `helm lint --strict` and `helm template` over each example values file.
- **The kind demo through the chart**, on main's pushes and on dispatch:
  - every existing check passes;
  - check 2 finds no door Secret.
- **The release's dry run** on the PRs that touch it.
- **duckdb-acl's reload spec** has its own tests: an e2e that rewrites a door's files and connects again on the new
  certificate.

## Alternatives considered

- **Keep kustomize and wrap it in Helm later**: the Managed Application and most organisations install Helm. One
  package is less to keep.
- **Door certificates as cert-manager Certificates (Secrets)**: simpler, but the key sits in etcd (decision 6).
- **Groups owned by the chart**: a `helm upgrade` would undo every scale made in the console.
- **Keycloak and PostgreSQL bundled**: the IdP and the databases are the organisation's. The demo keeps its own.

## Decided (the owner, 2026-10-20)

1. **Two ways to install** (the owner, 2026-10-20; this replaces "tresor-server is a subchart of hugr-platform"):
   - **the strict one**: the chart `hugr-platform` is the platform alone, and tresor and kista are releases of their
     own, in their own namespaces. tresor holds credentials and kista reaches a signing key, so neither shares the
     platform's namespace, whose administrator would otherwise be theirs too. The installer orders the releases;
   - **the one for a catalog** (IONOS, OTC, Hetzner, the Azure Marketplace): an umbrella chart `hugr` with
     `hugr-platform`, `tresor-server` and `kista` as dependencies, and their values wired together (tresor's host,
     kista's prefix, their certificates from the platform's internal CA). A platform works only with both services,
     and a catalog takes one chart.
   - To keep each service in its own namespace inside the umbrella, their charts need a `namespaceOverride`. That is a
     product gap of tresor-server and kista, for their sessions. Until it lands, the umbrella installs them in the
     platform's namespace, a trade-off its documentation states. kista's first release, with its chart, is the other
     precondition.
2. **kista**: the demo packages it from source until kista's first release. That release is the product's decision.
3. **The first groups are created at install only**, as post-install hooks rather than a `lookup`. Made with the
   release's other resources, a group meets the operator's webhook before it serves (`failurePolicy: Fail`), and a
   `lookup` renders nothing under `helm template`. After that, the console and kubectl own them.
4. **duckdb-acl's certificate reload is part of stage A**, as a precondition of decision 6.
5. **The CA bundle is the operator's** (the owner, 2026-10-20; it replaces trust-manager, decided the same day). The
   operator writes `hugr-ca-bundle` from three sources: its image's public roots, its own serving certificate's
   `ca.crt` (the internal CA, so the operator needs no right to the CA's Secret), and the ConfigMaps
   `caBundle.extra` names. Only CERTIFICATE blocks enter, each once. It never writes over a ConfigMap of that name
   that is not its own. One prerequisite fewer; the public roots follow the operator's image.
6. **Several platforms in one cluster** are isolated by namespace: one platform per namespace (spec 001's boundary).
   The cluster-scoped objects (ClusterRole, ClusterRoleBinding, the webhook's configuration) are named after the
   namespace. The CRDs are the one thing they share, at one version: one release installs them (`crds.install`, on
   by default), and the others set it off.
7. **The internal CA is the namespace's own** (the owner, 2026-10-20). It replaces a ClusterIssuer in cert-manager's
   namespace, decided the same day: a ClusterIssuer issues to every namespace, so any team that may create a
   Certificate, or mount the csi-driver into its pods, would get a certificate for a node's or the operator's name
   from the CA the platform trusts. Found in review before the chart's first PR. A namespaced Issuer keeps spec 001's
   boundary: only the platform's namespace is issued from it.
8. **hugr_node** stays in the image until `v2.0.0`. Its path after that is the proposal in "hugr_node on the node",
   to be confirmed.

## Follow-ups

- Stage B.
- **The front and the console verify a node by the internal CA alone.** Today they read the platform's whole bundle
  (spec 002's `FrontCABundle`), public roots and `caBundle.extra` included. A public CA issues no certificate for a
  `.svc` name, but an organisation's CA added to `extra` could. The operator would write a second ConfigMap with the
  internal CA only, and the chart would point `spec.front.caBundle` at it.
- ADBC drivers (decision 9; the owner, 2026-10-20). For now: a driver image per group in `HugrNodeGroup`, mounted by an
  init container, plus `adbc_` in duckdb-acl's never set; its own spec. After the release: kista serves ADBC drivers on
  a par with extensions, as a product change of kista's.
- A node egress NetworkPolicy from the profile.
- Telemetry receivers: the integrations spec.
