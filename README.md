# hugr-platform

The orchestrator of the hugr platform: a Kubernetes operator. It deploys the platform's node groups, is the place a
node starts from and reports to, and decides nothing for the nodes.

- **The node groups.** `HugrNodeGroup` is a pool of nodes, edited with standard tooling (the console, `kubectl`, the
  Azure Managed Application, GitOps) and checked against the installation's plan. The operator builds and scales its
  pods.
- **The entry.** A node presents its ServiceAccount token and is answered with its bootstrap document: pointers,
  never a secret.
- **The heartbeat.** Each node reports its state, and a lost node is an expired Lease. A group's status says who has
  applied the profile, who needs a restart and who is gone.
- **A client of the nodes.** With an identity of its own, through SQL like any principal, reading only (provided for;
  it comes with the console).

What the orchestrator does not hold:

- the cluster profile, the policy and the resource groups: these live in the policy catalog, changed with SQL
  (`ACL CLUSTER …`, `CREATE RESOURCE GROUP …`) on any node. A group's three faces - its pool here, its resource
  group and its profile there - are joined by its name;
- secrets: these live in tresor-server;
- extensions: these live in a repository speaking duckdb's protocol (kista, for one).

When the orchestrator is down, running nodes keep running; new and restarted nodes wait for it.

The node is [hugr-node](https://github.com/hugr-lab/hugr-node), and the image is `ghcr.io/hugr-lab/hugr-node`. The
platform's design and status are in the phase 1 document, and the work here goes spec by spec (`specs/`).

> **Status:** spec 001, the operator, is built: the resources, the group's objects, the plan's checks, the entry
> and the heartbeat, with unit tests and envtest. The front (002) is built and the demo runs through it; the console
> (003) follows.

## Install

The chart `charts/hugr-platform` (spec 005) installs one platform per namespace: the operator, its rights and webhook,
the platform's internal CA (an Issuer of the namespace: only its pods are issued from it), the `HugrPlatform` and its
first node groups, each door's certificate issued into its pod by cert-manager's csi-driver. The operator writes the CA
bundle (`hugr-ca-bundle`: its image's public roots, the internal CA, the organisation's CAs). The cluster needs
cert-manager and its csi-driver first.

```sh
helm upgrade --install hugr oci://ghcr.io/hugr-lab/charts/hugr-platform --version <X.Y.Z> -n hugr \
  --create-namespace -f my-values.yaml --wait
# or from a checkout: helm upgrade --install hugr charts/hugr-platform ...
```

A release (a tag `vX.Y.Z`, spec 005) publishes the image `ghcr.io/hugr-lab/hugr-platform:vX.Y.Z` (amd64, arm64) and
the chart `X.Y.Z`, whose default image is that tag. It publishes them only after the demo has passed through the chart
on the tag's commit. Both are signed keylessly by the release workflow:

```sh
cosign verify ghcr.io/hugr-lab/hugr-platform:vX.Y.Z \
  --certificate-identity https://github.com/hugr-lab/hugr-platform/.github/workflows/release.yml@refs/tags/vX.Y.Z \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

`--wait` matters: the first groups are made after the operator - whose webhook judges a group - is ready. The values
hold no credential (`charts/hugr-platform/values.yaml` documents them; `ci/example-values.yaml` is a whole one). The
CRDs are in the chart's `crds/`: Helm installs them once and never upgrades them, so a chart whose CRDs changed is
applied first (`kubectl apply --server-side -f charts/hugr-platform/crds`).

Without Helm, `config/` is the same operator as kustomize (bring an `Issuer` named `hugr-platform` and a `caBundle`
ConfigMap holding the system roots plus its CA: a node's `HUGR_CA_FILE` replaces its roots):

```sh
kubectl create namespace hugr
kubectl apply -k config/default            # the CRDs, the operator, its rights, the webhook, its certificate
kubectl apply -f config/samples/platform.yaml   # a HugrPlatform and a HugrNodeGroup, edited for the cluster
```

`hugrInit` in the values runs the install Job `hugr-init` (spec 005) ahead of the first groups. It makes the policy
catalog's database and its login on PostgreSQL, with a generated password that goes to tresor only. It writes that
login into tresor as `hugr_policy_catalog`, grants it to the groups' identities, and deletes the database
administrator's one-time Secret (`hugr-init-db-admin`: `username`, `password`). tresor-server (0.7.0 or later)
trusts the cluster's ServiceAccount issuer for it and names it an administrator:

```yaml
# tresor-server's values
clusterIssuer: {enabled: true}   # an issuer of the API server; a public one (AKS, EKS, GKE) needs none of it
config:
  issuers:
    - issuer: https://kubernetes.default.svc.cluster.local   # kubectl get --raw /.well-known/openid-configuration
      audience: duckdb-secrets                               # = hugrInit.tresor.audience
      ca_file: /var/run/tresor/cluster/ca.crt
      discovery_token_file: /var/run/tresor/cluster/token
      service: {claim: kubernetes.io, client_claim: sub}
  policy:
    admins: ["subject:https://kubernetes.default.svc.cluster.local|system:serviceaccount:<namespace>:hugr-init"]
```

tresor-server's ServiceAccount reads the issuer's discovery by the ClusterRole `system:service-account-issuer-discovery`,
which Kubernetes binds to every ServiceAccount by default (a cluster that removed that binding needs one for it). A
run again (`helm upgrade`) only grants: the database and the secret stay as they are.

A platform installed from nothing has an empty policy catalog. `spec.firstPolicy` names what the first node writes
into it (spec 005, hugr-node spec 015): the IdP, the applications whose tokens count (the console's always), the
roles-claim value of the administrators and the default group. Its resource groups are the groups'. A node writes it
only into a catalog nobody administers yet, so a later edit of the field changes nothing.

A group's doors need a certificate for `hugr-node-<group>.hugr.svc`, named by exactly one of:

- `tls.secretRef`: a TLS Secret named `hugr-node-…`;
- `tls.csi`: cert-manager's csi-driver issues each pod its own certificate from the issuer named there. The key is
  made on the pod's node and never written to a Secret (spec 005). It needs the csi-driver in the cluster.

The namespace is the platform's alone: whoever may create pods in it can run one as a
group's ServiceAccount. `kubectl apply -k config/policy` (Kubernetes 1.30+) lets a group's pods, ReplicaSets and
Deployment come only from the operator and the cluster's own controllers.

The front (spec 002) is the clients' one address: `spec.front` in the HugrPlatform makes the operator run it - the
operator's own image, a Service with the Flight SQL (32010) and quack (9494) doors, of `serviceType` `ClusterIP` or
`LoadBalancer`. It needs the public certificate's Secret (`hugr-front` by default; a cert-manager Certificate can
write it) and verifies the nodes by `caBundle`. It keeps no table of sessions: a connection stays on the node that
seated it by the label the node wrote into its cookie or connection id.

### The platform with its services: the chart `hugr`

`charts/hugr` (spec 005) installs the platform together with tresor-server, kista and its key store, each in a
namespace of its own (`hugr-tresor`, `hugr-kista`, `hugr-openbao`; made by the release and kept when it goes, with
their volumes). It pins their charts (tresor-server 0.7.2, kista 0.1.2) and wires them from one set of facts under
`global`:

- **TLS**: each service gets a CA of its own, made once in its namespace; cert-manager issues its serving certificate
  there. The CAs' public certificates go into the platform's bundle, so the nodes, the front and `hugr-init` trust
  both services.
- **The database**: one PostgreSQL 18 or later (`global.database`). A Job of the release (`hugr-init --phase
  databases`) makes tresor's and kista's databases and logins before they start, each password in a Secret of that
  service's namespace. Then `hugr-init` makes the catalog's, as above. The administrator's Secret (`hugr-init-db-admin`
  in the release's namespace) is made once by the installer.
- **The keys**: two Transit keys - kista signs its channel with `hugr-channel` (RSA), tresor wraps its data keys with
  `hugr-tresor` - and a Kubernetes-auth role for each service's ServiceAccount (`hugr-kista` in `hugr-kista`, `tresor`
  in `hugr-tresor`), bound to the audience `vault`. Where they live is `global.keys.mode`:
  - `internal` (the default): OpenBao of the release in `hugr-openbao`. Two replicas in HA, its storage a fourth
    database on the same PostgreSQL server, opened by a static key made once at install (`openbao-seal`; the cluster's
    etcd is the root of trust). `hugr-init --phase openbao` initialises it once - the recovery key in
    `openbao-recovery`, the root token revoked - and on every release logs in as its own role and makes the keys,
    policies and roles. The break-glass is a loopback-only listener in the pod (`bao operator generate-root
    -address=http://127.0.0.1:8210` with the recovery key).
  - `external`: the organisation's HashiCorp Vault or OpenBao (`global.keys.vault.address`, its CA in `caCert`),
    where the organisation made the keys and roles above.
- **The IdP and the cluster's issuer**: `global.idp` (its private CA, if any, in `caCert`) and `global.clusterIssuer`,
  whose URL `kubectl get --raw /.well-known/openid-configuration` gives.
- **kista's bootstrap**: the tenant `hugr`, the signed channel `platform` for the node's DuckDB (`global.duckdbVersion`:
  a release `vX.Y.Z`, or a commit's dev version) and its key.

```sh
helm dependency build charts/hugr
kubectl -n hugr create secret generic hugr-init-db-admin --from-literal=username=… --from-file=password=…
helm upgrade --install hugr charts/hugr -n hugr --create-namespace -f my-values.yaml \
  --set-file global.database.caCert=db-ca.pem --wait
```

`charts/hugr/ci/example-values.yaml` is a whole set of values. The values hold no credential. A node restarts until
kista holds the extensions its catalog needs (postgres_scanner for a PostgreSQL catalog), mirrored from an upstream
(`kista.config.bootstrap.upstreams`) or published into the channel (`kista admin release add`). The demo
(`test/demo`) installs through this chart, its PostgreSQL standing in for the organisation's; `HUGR_DEMO_KEYS=external`
puts its own OpenBao in the organisation vault's place.

Business Source License 1.1 (see `LICENSE`): production use is allowed, except offering it to third parties as a
hosted or managed service. Each version changes to Apache-2.0 four years after its release.
