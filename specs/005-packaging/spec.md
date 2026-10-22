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
| the operator | the CRDs in `crds/` (Helm installs them first and never upgrades or deletes them: as templates, a `HugrPlatform` in the same release would not map; before an upgrade a pre-upgrade hook of the new operator image applies them, spec 008), RBAC as spec 001 states it, the Deployment, its Service, the webhook with cert-manager's CA injection, the NetworkPolicy; the ValidatingAdmissionPolicies of `config/policy` optional | on |
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

### Stage A: the first deployment, by itself (the owner, 2026-10-20: "everything installs itself, as far as it can")

One mechanism everywhere: the chart's install Job `hugr-init`, under a ServiceAccount of its own. It is no group's, so
the admission policies leave it alone. Then the first node writes the first policy.

1. **The database.** `hugr-init` makes the policy catalog's database and its login:
   - on Azure SQL, Entra users for the groups' identities, with no password (stage B);
   - on PostgreSQL (stage A), a login with a generated password (32 random bytes), written straight into tresor and
     nowhere else, and the database owned by it.
2. **tresor.** It writes `hugr_policy_catalog` and grants `use` on it to the principals the values name (the groups'
   identities: a role or a group in tresor).
3. **kista** sets itself up from the chart's values: its `bootstrap` section makes the tenant, the channel and its
   upstreams once (kista spec 0024: DuckDB's core and community repositories, our public channel, the organisation's
   Enterest tenant by a credential of `upstreams.credentials`). `hugr-init` does nothing there. Subscribing to
   Enterest and granting the in-cluster kista there stays manual, as a matter between the organisation and Enterest.
4. **The nodes** start, and the first one writes the first policy. The administrator signs into the console. The
   first policy:
   - **what it is:** `HugrPlatform.spec.firstPolicy`, structured fields, never SQL:
     - the issuer: its name, its URL (default `idp.issuer`), the audiences besides the console's, and the roles claim;
     - the clients whose tokens count. The console's is always there (`console.clientID`, its tokens for
       `console.audiences.nodes`), so `firstPolicy` needs `console`; a tool's may join it;
     - the roles-claim value of the administrators;
     - the default group (optional). The resource groups themselves are the namespace's `HugrNodeGroup`s', which a
       marketplace's deployment form chooses (groups, node counts, sizes) as the chart's values;
   - **how it travels:** in each node's bootstrap document, as `first_policy`, which the operator builds per request
     from the platform and the namespace's groups. A platform whose first policy the node would refuse (a default group
     no group serves, say) gets no document: the node waits, and the operator logs why;
   - **where it is applied:** in the node's own process, before it serves, into a catalog nobody administers yet: one
     with no role mapping and no issuer but the first policy's own (an empty database gets its schema first). It writes
     the role `platform_admin` with the `cluster`, `policy` and `passthrough` bundles, a resource group per group bound
     to that role, the issuer, its clients, and last the administrators' mapping. A node whose group the policy lacks
     refuses everyone and drains (duckdb-acl spec 096), so the groups are part of it;
   - **never overwrites:** the mappings come last, so a write that stopped part-way is completed by the next start,
     and a catalog with a mapping is left alone. Nodes that start together race to the same policy;
   - **`passthrough` too** (the owner, 2026-10-20): handing out administration is `passthrough`'s alone in duckdb-acl
     (a catalog's `manage` for a mapped role, a mapping to an administering role, the issuer and the console's client),
     and no one else in a deployed platform holds it. It is also `ACL NATIVE`: the administrators' value is for few;
   - **the specs:** hugr-node spec 015 (the document's field, the write) and this one (the field, the document).

Where `hugr-init`'s rights come from:

- **Azure** (the Managed Application, stage B): its ServiceAccount is federated with a managed identity the Managed
  Application makes. That identity is the database server's Entra administrator and a tresor administrator.
- **IONOS, OTC, Hetzner, kind** (stage A):
  - **The database**, the form's choice: the provider's managed database, or one in the cluster (CloudNativePG as a
    dependency of the umbrella). Its administrator credential comes once, in a Secret the values name, which
    `hugr-init` deletes after use (the owner: acceptable where needed).
  - **tresor** trusts the cluster's own ServiceAccount issuer (tresor-server 0.7.0, its spec 026: `clusterIssuer`,
    the issuer's `ca_file` and `discovery_token_file`; a public issuer - AKS, EKS, GKE - needs neither), and lists
    `client:system:serviceaccount:<namespace>:hugr-init` among its administrators. `hugr-init` sends its projected
    token (audience: the one that issuer entry names, 600 s) as the bearer. An administrator is all-or-nothing; the
    owner wants no narrower role.
  - **The nodes** log in to tresor as today, by `federated`: the organisation's IdP trusts the cluster's issuer once.

**`hugr-init`, as built.** A third binary of the operator's image (`/hugr-init`), run as a Helm hook
(`post-install`, `post-upgrade`) ahead of the first groups:

1. tresor's discovery (`/.well-known/duckdb-secrets`), then whether `hugr_policy_catalog` exists (the descriptors an
   administrator sees);
2. when it does not: the login made (or its password set) and the database made, as the administrator; then the
   secret written with `If-None-Match: *` (TYPE postgres, the password redacted). A `412` is an install that already
   has it. The server gets the password's SCRAM-SHA-256 verifier, never the password (as psql's `\password`): a
   server that logs DDL logs nothing that logs in. The database stays the administrator's, and the login gets
   `CONNECT, CREATE, TEMPORARY` on it - a managed server's administrator may make a role but not give it a database;
3. a grant per principal (`PUT …/grants/<id>`, the id derived from the principal) - again at every run, so a principal
   added to the values is granted; one removed is not revoked (nothing is taken back by itself);
4. the administrator's Secret deleted (a Role allowing `delete` on that name only).

The server is PostgreSQL 18 or later (the owner, 2026-10-21): an older one is refused before anything is made. A
run after the secret exists touches neither the database nor the secret.

`--phase databases` (built 2026-10-21) is the umbrella's first run, a Job of its release (not a hook: the services
become ready only once it has run, and Helm waits for them before any post-install hook):
- **What it makes.** For each of the configuration's `services` (tresor, kista), the login with a generated password,
  the database, and the login's `USAGE, CREATE` on its `public` schema (the services keep their tables there; only
  the database's owner may create there by default). It writes the password into a Secret of the service's own
  namespace (`hugr-init`'s Role there may get and create Secrets).
- **What it leaves alone.** A service whose Secret exists, so a password never changes by itself. The administrator's
  Secret stays too, for the catalog phase that follows.
- **A run that failed between the login and its Secret** is made again by the next one. One Job runs at a time: a login whose
password was set by a run that failed before its write is set again by the next run.

**Product gaps** (relayed 2026-10-20, both closed the same day): kista v0.1.0 (spec 0024: `bootstrap.upstreams`,
`namespaceOverride`, its first release); tresor-server v0.7.0 (spec 026: `namespaceOverride`, the cluster's issuer).

### Stage A: the umbrella chart `hugr` (accepted by the owner, 2026-10-21)

The catalog's one chart (decision 1 of this spec). It lives here (`charts/hugr`) and ships with the operator, at the
same version.

- **Its dependencies:**
  - `hugr-platform`, this repository's chart, packaged together;
  - `tresor-server` 0.7.3 and `kista` 0.1.4, from `oci://ghcr.io/hugr-lab/charts`, both signed. Both render their
    `config` through `tpl` (`configTemplate: true`; their specs 028 and 0025), so the umbrella's values name its
    global facts there;
    - kista 0.1.4 (2026-10-22; image `sha256:68d1ddc6…` and chart `sha256:a0037d96…`, both checked here) adds an
      expected licence holder per tenant (its spec 0028) and licence managers (0029). Its migration 0029 makes the
      store readable from build 29 on, so an upgrade stops every older replica first: the umbrella's kista is one
      replica with `Recreate`, which does. It also refuses to start with a `role:`, `group:` or `client:` principal
      among `server_admins` when its server issuer names no matching claim; the umbrella and the demo set no such list;
  - optionally `openbao`, below.
- **The namespaces.** The release's namespace is the platform's. tresor, kista and an internal OpenBao each go into a
  namespace of their own (their charts' `namespaceOverride`): `<prefix>-tresor`, `<prefix>-kista`, `<prefix>-openbao`.
  - The umbrella makes these namespaces, annotated `helm.sh/resource-policy: keep`, so an uninstall leaves their PVCs.
  - Their names are values with defaults, because a subchart's values cannot be computed from the release. The
    services' addresses follow from them: `tresor.<ns>.svc`, `<release>-kista.<ns>.svc`.
- **TLS: each service has its own CA** (the owner, 2026-10-21):
  - In tresor's and kista's namespaces, a CA is made once at install and kept across upgrades (Helm's `lookup`).
    cert-manager issues and renews the service's serving certificate from a CA Issuer of that namespace.
  - The CA's public certificate goes into a ConfigMap of the platform's namespace, named in `caBundle.extra`. The
    operator's bundle then carries it to the nodes, the front and `hugr-init`.
  - No private key leaves its namespace, and the platform's own CA stays its namespace's.
- **Keys** (kista's channel key, tresor's KEK): `keys.mode`, asked by the form (the owner, 2026-10-21):
  - **`external`**: the organisation's Vault-compatible server, HashiCorp Vault or OpenBao. The form asks for its
    address, its CA, the Transit mount, and the Kubernetes-auth roles the organisation made for kista's and tresor's
    ServiceAccounts.
  - **`internal`**: OpenBao, installed in its own namespace by the umbrella.
    - **Why not HashiCorp Vault**: Vault is under the Business Source License 1.1 since 2023, and embedding it in an
      offering that third parties install is the grey zone of that licence. OpenBao is MPL 2.0, and its API is
      Vault's.
    - **Unseal**: OpenBao opens itself after a restart by its static-key auto-unseal. The key is a Secret of its
      namespace, made once at install. The root of trust is therefore the cluster's etcd encryption, and the docs
      say so (OpenBao's own docs ask for such a source of trust).
    - **The first setup**: an init Job initialises OpenBao once and keeps its recovery keys in a Secret of that
      namespace. With the root token it enables Transit, makes the two keys and the Kubernetes-auth roles for kista's
      and tresor's ServiceAccounts, then revokes the token.
    - **Built (step 2, 2026-10-21)**, after a live check on OpenBao 2.7.1 (PostgreSQL 18, static seal):
      - **Storage**: PostgreSQL (spec 008 Q6) - a fourth database and login, made by the databases Job like tresor's
        and kista's (the password a Secret of OpenBao's namespace). Two stateless replicas in HA: one active, a standby
        forwarding every request (`disable_standby_reads`: OpenBao 2.7's standbys otherwise serve reads themselves and
        are eventually consistent - a mount just enabled answered "no handler" through a standby).
      - **Unseal**: the static seal with a 32-byte key made once at install (`randBytes`, the Secret `openbao-seal`,
        kept).
      - **The Job** (`hugr-init --phase openbao`, a Job per revision): the first run initialises with one recovery
        share, keeps the key in `openbao-recovery`, configures with the root token and revokes it. Every later run logs
        in by Kubernetes auth as its own role `hugr-openbao-init`, whose policy names the paths it configures (the
        services' keys, policies and roles) and no key's material. It writes its own policy first, so a release that
        adds a service widens it before using it - which makes the role OpenBao's administrator in effect, as anyone
        who reads the namespace's Secrets (the seal and the recovery key) is: OpenBao's namespace is its boundary.
        Nothing is removed; a key of another type is refused, never changed.
      - **TLS**: the serving certificate from the umbrella's OpenBao CA, read again when cert-manager renews it
        (`tls_auto_reload`). tresor-server read its certificate once at start (`ServeTLS`), so a renewed one reached it
        only with a restart. The gap was relayed (the owner, 2026-10-21) and closed: tresor-server 0.7.3 (its spec 029)
        re-reads the pair every 30 s, keeps the old one while a new pair does not check, and the umbrella pins it. kista
        reloads its own.
      - **Break-glass**: OpenBao 2.5.3 turned the unauthenticated `sys/generate-root` off, and its successor
        `sys/generate-root-token` wants a token. The pod therefore has a second listener on its own loopback with the
        legacy endpoints on: `kubectl exec` and the recovery key make a root token when no other is left.
      - **The services**: tresor and kista reach `https://openbao.<ns>.svc:8200` by the umbrella's OpenBao CA (their
        `vault.crt`); the default is `keys.mode: internal`.
  - **`secret`**: kista's channel key and tresor's KEK as Secrets of their own namespaces, made once at install. It
    is the lightest of the three and is an explicit choice. tresor takes a KEK from a Secret today; kista takes one
    since 0.1.3 (its spec 0026, `kubesecret`).
    - **Deferred** (the owner, 2026-10-21). It saves only OpenBao's pods: both modes rest on etcd, since OpenBao's
      seal is a Secret too. It also changes the structure of tresor's and kista's configs (`keys` local and a
      `kubesecret` source, not vault), which their charts' string templates cannot switch. So it would need a values
      overlay, plus a check that an upgrade keeps the mode the install recorded.
  - **Azure** (stage B): Azure Key Vault for both, by workload identity. Both products have it already: kista's
    `azurekv` source (RSA-HSM outside dev, so a Premium vault) and tresor's `keys: {kind: azurekeyvault}`. The
    Marketplace form writes that structure itself.
- **The wiring** the umbrella writes into its dependencies' values. Each item is a value the form fills or a
  default:
  - tresor:
    - its issuers: the organisation's IdP, and the cluster's ServiceAccount issuer for `hugr-init` (whose URL the form
      gives: an API server's issuer is not readable at template time);
    - `hugr-init` among its administrators, by subject, and the groups' identities as its actors;
    - its state on the external PostgreSQL, and its KEK from the key store.
  - kista: its `bootstrap` - the tenant, the channel for the node's duckdb, the upstreams (core, community, our public
    channel, and an Enterest tenant when the form names one) - its store on the external PostgreSQL, and its channel
    key from the key store.
  - hugr-platform: tresor's host, kista's prefix, `hugrInit`, `firstPolicy`, and the groups from the form.
- **The databases: one PostgreSQL 18 or later, external or internal** (the owner, 2026-10-21). On it, the policy
  catalog, tresor's state and kista's store each get a database and a login of their own. `hugr-init` refuses an
  older server before it makes anything.
  - `database.mode: external`: the form names the server, the provider's managed one or the organisation's.
  - `database.mode: internal`, for the catalogs of the clouds other than Azure (OTC, Hetzner, IONOS, STACKIT): the
    umbrella runs PostgreSQL in the cluster, by CloudNativePG (Apache 2.0, the operator a dependency, the cluster in a
    namespace of its own). Its superuser Secret is `hugr-init`'s administrator credential.
  - `hugr-init` makes all three with the administrator's one-time Secret, which it then deletes.
  - The catalog's login goes to tresor only, as today.
  - tresor and kista cannot read their own login from tresor: each password is a generated value in a Secret of that
    service's namespace, mounted as the service's `password_file`. These two passwords are the only ones at rest, each in its own service's namespace. For PostgreSQL
    outside a cloud's IAM this is the only way for now (the owner, 2026-10-21). On Azure (stage B) both log in with
    Entra and hold no password.
  - **Rotating a database password is not the platform's** (the owner, 2026-10-21).
    - Where rotation is wanted, the answer is a managed database that takes tokens: Azure SQL or Azure Database for
      PostgreSQL with Entra, RDS with IAM, Cloud SQL with IAM. tresor (`state.auth: entra|aws|gcp`) and kista
      (`store.login.kind: entra|aws|gcp`) log in with their identities there, so nothing is stored and nothing
      rotates. tresor stores its state on SQL Server too (`state.kind: sqlserver`), and so does kista
      (`store.kind: sqlserver`).
    - Elsewhere the two passwords stay in their Secrets. Rotating them is the organisation's own setup, for example
      a Vault database engine's static role synced into the same Secret by its operator. Both services read
      `password_file` again for each new connection, and `hugr-init` never touches a Secret that exists, so such a
      rotation needs nothing of the platform.
    - OpenBao's database engine (tresor's leases, kista's static role, both supported since tresor 0.7.2 and kista
      0.1.3) is at most an option offered later.
  - Order: tresor's and kista's databases must exist before their pods are ready, while `hugr-init`'s write into
    tresor needs tresor up. So `hugr-init` runs twice: as a Job of the release (tresor's and kista's databases and
    their Secrets, by a Role in each of their namespaces), then as the platform's hook (the catalog's database, its
    secret, the grants, the administrator's Secret deleted).
- **Steps:**
  1. the chart with `keys.mode: external`, `database.mode: external` and the per-service CAs. The demo installs through
     it, its OpenBao and its PostgreSQL as the external ones.
  2. `keys.mode: internal` (OpenBao).
  3. `database.mode: internal` (CloudNativePG).
  4. `keys.mode: secret` - deferred (above).
  5. the release publishes the umbrella beside `hugr-platform`.

  **Step 1, built (2026-10-21).** The chart, its render tests (`charts/hugr/ci/render-test.sh`, run by `make chart`)
  and the demo through it. Building it found these:
  - tresor's Service port is 80 unless the values say 443, even with a TLS Secret. The umbrella sets 443.
  - An IdP with a private CA, such as a Keycloak of the organisation's, needs `global.idp.caCert`. It goes to tresor
    as that issuer's `ca_file`, and to the platform's bundle. The two private CAs reach the bundle through one
    ConfigMap, `hugr-private-cas`, with the keys `database.crt` and `idp.crt`.
  - Keycloak gives the groups in their own claim, so `global.idp.tresorGroupsClaim` was added.
  - kista's bootstrap names a version's kind. A node built on a DuckDB commit runs a dev version, so the kind is
    derived: `release` for `vX.Y.Z`, `dev` otherwise.
  - `tresor.config.policy.admins` is a list, and a list in the values replaces the whole default. The umbrella refuses
    a list that lost hugr-init's entry.
  - tresor reads the cluster's issuer discovery through `system:service-account-issuer-discovery`, which Kubernetes
    binds to every ServiceAccount by default. The umbrella makes no binding for it.
  - `global.idp.audience` and `rolesClaim` were dropped: the platform's own use of the IdP (the console, the first
    policy) is `hugr-platform.platform.spec`'s.
  - The databases Job mounted the administrator's Secret as required, but the catalog phase deletes it after the first
    install, so an upgrade's run never started. It is optional now (a render test holds it); a run that has to make a
    login without it fails naming the credential.

### Stage A: the operator's release

`.github/workflows/release.yml`, as hugr-node spec 008 and kista spec 0013 do it:

- a tag `v*` publishes `ghcr.io/hugr-lab/hugr-platform:<tag>` (amd64 and arm64) and the chart
  `oci://ghcr.io/hugr-lab/charts/hugr-platform:<version>`. Both are published only after the kind demo has passed
  through the chart on the tag's commit;
- a PR that touches `charts/` or the workflow dry-runs it: `helm lint --strict`, `helm template` over the example
  values, and the demo.

Built (2026-10-21):
- **Jobs.** `release.yml` has `chart` (lint, render, package at the tag's version, the appVersion checked) and
  `image` (both platforms, built without push, the amd64 one scanned by Trivy: a fixed HIGH or CRITICAL fails it).
  It also runs `demo.yml`, reusable now. `publish` runs on a tag only, after all three.
- **What is published.** The image gets an SBOM, provenance and a cosign keyless signature, verified at once; the
  chart is pushed to OCI and signed the same way.
- **The build.** The image's build stage cross-compiles (`--platform=$BUILDPLATFORM`, `GOOS`/`GOARCH` from the
  target), so arm64 builds without emulation.
- **What the demo tests** is the tag's commit through the chart, building its own operator image from it: the
  workflow's Dockerfile, not the pushed bytes.
- **Visibility**: the packages came out public - ghcr gives a package the public repository's visibility (v0.1.0,
  2026-10-21); nothing was asked of the owner.

### Stage A: the demo through the chart

- **Install**: one `helm upgrade --install` of the umbrella (`install_chart`), with the values the run writes
  (`chart_values`: no credential; the CAs given as files).
- **Certificates**: the run's own door certificates go. The demo installs cert-manager and its csi-driver at pinned
  versions, and gives the chart its CA as the platform's internal CA.
- **tresor and kista** (step 1 of the umbrella, 2026-10-21): the demo installs through `charts/hugr`, so they are the
  pinned releases, their charts and their published images. It no longer builds either from its main.
- **Keycloak, PostgreSQL and OpenBao** stay the demo's own. PostgreSQL 18 serves TLS by the run's CA
  (`sslmode=verify-full` everywhere). OpenBao, in a namespace of its own, stands in for the organisation's vault: it
  holds the two Transit keys and a Kubernetes-auth role for each service.
- **The first deployment installs itself** (built 2026-10-21):
  - the chart's `hugr-init` makes the catalog's database, login and secret;
  - tresor trusts the cluster's issuer for it;
  - the platform carries `firstPolicy`, and a first node writes it - check 1 asserts it;
  - the demo's own data (the source, the analysts, the catalog `demo`) is written after the platform, through the
    secret `hugr-init` made;
  - a run again installs the umbrella afresh: the groups and the release go, then the services' namespaces with
    their state, then the three databases and logins. The cluster, Keycloak, OpenBao, PostgreSQL and the images stay.
    OpenBao's dev mode keeps its keys in memory, and PostgreSQL its data in the pod, so a restarted cluster must not
    find a service's state sealed by a key that is gone;
  - kista's bootstrap makes the channel `hugr/platform`. The node's DuckDB is a commit's (a dev version, which no
    upstream mirrors), so the demo publishes the release's extensions into it with `kista admin`. Until it has, a
    node's catalog step fails, and the kubelet restarts the node.
- **Check 2** gets stronger: no door Secret is left in the namespace, and each node's certificate comes from its own
  pod.

### Stage B: Azure (its own PRs, detailed when stage A lands)

- **Each marketplace has its own recipe** (Helm or Bicep), and so does a private cloud's Kubernetes (the owner,
  2026-10-22). What they share:
  - **kista and tresor are external by default**: products of their own, used outside the platform too and possibly
    by several platforms, each installed by its own recipe (on Azure its own Managed Application). The platform's
    recipe takes their addresses. Only when none is given does it run its own, on SQLite, so nobody takes one for the
    other.
  - **The key store too**: the organisation's Vault or Key Vault; OpenBao installed with the platform only when
    asked.
  - **The databases**: Azure SQL Managed or Azure Database for PostgreSQL Flexible, made by the recipe or any the
    customer gives. Backups are then the customer's or Azure's.
  - The variants are not all written out now: each is added when a recipe needs it.
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
  - **The policy catalog on Azure SQL: the node, tresor, a token made on each read** (the owner, 2026-10-22; checked
    in the code, not yet live). Nothing is stored and no product changes:
    - tresor-server makes the token itself (its spec 023, a login of flow `rule` with an `azure` identity of
      `dynamic.identities`: its own managed identity or a user-assigned one, scoped to Azure SQL). The secret is
      `hugr_policy_catalog_token`: type `azure`, `PROVIDER access_token`, `ACCESS_TOKEN 'mint+rule://<login>#access_token'`.
      It is served with `expires_at` = the token's expiry less 10 minutes.
    - `hugr_policy_catalog` (type `mssql`) names the host, the database and `AZURE_SECRET 'hugr_policy_catalog_token'`.
      Both are granted to the groups' identities.
    - A node logs in to tresor by its own identity, as today. mssql's pool reads the azure secret again by name each
      time its token expires (the pool's factory has the `DatabaseInstance` only). tresor's storage answers a lookup
      without a context as the node itself, and fetches a dynamic secret again before it expires.
    - The catalog then sees one login, the identity tresor makes the token with; which nodes read the catalog is
      tresor's grants. A database user per node would need mssql to take a pod's workload identity itself
      (`AZURE_FEDERATED_TOKEN_FILE`; its `managed_identity` provider is "not yet implemented") - not needed for this.
    - A service principal (`PROVIDER service_principal`, its client secret in tresor) works too.
  - **The policy catalog on PostgreSQL** has one gap. An Entra token lives about an hour, and postgres_scanner takes
    its password once, so a new connection after that fails. Two ways close it: postgres_scanner re-reading a token as
    the password per connection (a patch of ours), or a dynamic secret in tresor that hands out a fresh token ("CREATE
    DYNAMIC SECRET", decision 5 of 2026-10-15). Until one lands, the catalog on PostgreSQL logs in by a password that
    only tresor holds: the deployment script makes it and writes it straight into tresor, and the server admits a
    password for that one login.
  - **The order** (the owner, 2026-10-20): SQL Server first, with no password anywhere. The PostgreSQL catalog's Entra
    login waits for upstream postgres_scanner to support a refreshed token. No patch of ours.
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
