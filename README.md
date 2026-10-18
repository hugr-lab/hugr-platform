# hugr-platform

The orchestrator of the hugr platform: a Kubernetes operator. It deploys the platform's node groups, is the place a
node starts from and reports to, and decides nothing for the nodes.

- **The node groups.** How many nodes of which size a group has is named in SQL, as a cluster profile item
  (`ACL CLUSTER SET NODES etl COUNT 3 SIZE 'large'`). The operator builds and scales the pool to match, inside the
  bounds of the installation's plan.
- **The entry.** A node presents its ServiceAccount token and is answered with its bootstrap document: pointers,
  never a secret.
- **The heartbeat.** Each node reports its state, and a lost node is an expired Lease. A group's status says who has
  applied the profile, who needs a restart and who is gone.
- **A client of the nodes.** With an identity of its own, through SQL like any principal, and reading only: the
  pools the profile names and the nodes' load.

What the orchestrator does not hold:

- the cluster profile, the policy and the resource groups: these live in the policy catalog, changed with SQL
  (`ACL CLUSTER …`, `CREATE RESOURCE GROUP …`) on any node - the operator only reads them, as a principal, through
  a node;
- secrets: these live in tresor-server;
- extensions: these live in a repository speaking duckdb's protocol (kista, for one).

When the orchestrator is down, running nodes keep running; new and restarted nodes wait for it.

The node is [hugr-node](https://github.com/hugr-lab/hugr-node), and the image is `ghcr.io/hugr-lab/hugr-node`. The
platform's design and status are in the phase 1 document, and the work here goes spec by spec (`specs/`).

> **Status: starting** - spec 001 (the operator) is a draft; the front (002) and the console (003) follow.

Business Source License 1.1 (see `LICENSE`): production use is allowed, except offering it to third parties as a
hosted or managed service. Each version changes to Apache-2.0 four years after its release.
