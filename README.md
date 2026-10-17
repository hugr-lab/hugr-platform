# hugr-platform

The control plane of the hugr platform. It is the place a platform node starts from and reports to. It decides
nothing for the nodes.

- **The bootstrap document.** A node asks for it at start and is answered by its identity. It holds pointers, never
  a secret: the extension repository's prefix, the policy catalog's type and ATTACH string, the name of the
  catalog's secret in tresor, and the node's group.
- **The heartbeat.** Each node reports its state: its versions, the profile version it targets and the one it has
  applied, the items still pending, whether it needs a restart, its drift and its load.
- **The fleet's view.** The operator reads the nodes and the cluster: who has reached the target, who needs a
  restart, who is lost.

What the control plane does not hold:

- the cluster profile, the policy and the groups: these live in the policy catalog, changed with `ACL CLUSTER …` on
  any node;
- secrets: these live in tresor-server;
- extensions: these live in a repository speaking duckdb's protocol (kista, for one).

When the control plane is down, running nodes keep running; new and restarted nodes wait for it.

The node is [hugr-node](https://github.com/hugr-lab/hugr-node), and the image is `ghcr.io/hugr-lab/hugr-node`. Phase 1
of the platform is one node group behind this service. Its design and status are in the phase 1 document, and the
work here goes spec by spec (`specs/`).

> **Status: starting** - spec 001 (the minimal control plane) is a draft.

Business Source License 1.1 (see `LICENSE`): production use is allowed, except offering it to third parties as a
hosted or managed service. Each version changes to Apache-2.0 four years after its release.
