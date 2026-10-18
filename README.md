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
> and the heartbeat, with unit tests and envtest. The kind demo, the front (002) and the console (003) follow.

## Install

The operator runs in the namespace `hugr` with cert-manager for its serving certificate (bring an `Issuer` named
`hugr-platform`). The platform's `caBundle` is the nodes' `HUGR_CA_FILE`, which replaces their system roots: put the
system roots in it, plus that Issuer's CA, so the nodes trust the operator and still reach the public repository and
IdP.

```sh
kubectl create namespace hugr
kubectl apply -k config/default            # the CRDs, the operator, its rights, the webhook, its certificate
kubectl apply -f config/samples/platform.yaml   # a HugrPlatform and a HugrNodeGroup, edited for the cluster
```

A group's pods need the doors' TLS Secret it names (`tls.secretRef`, named `hugr-node-…`, SAN
`hugr-node-<group>.hugr.svc`). The namespace is the platform's alone: whoever may create pods in it can run one as a
group's ServiceAccount. `kubectl apply -k config/policy` (Kubernetes 1.30+) lets a group's pods, ReplicaSets and
Deployment come only from the operator and the cluster's own controllers. Packaging proper - a Helm chart, the Azure Managed Application - is a spec of its own.

Business Source License 1.1 (see `LICENSE`): production use is allowed, except offering it to third parties as a
hosted or managed service. Each version changes to Apache-2.0 four years after its release.
