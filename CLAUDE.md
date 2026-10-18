# hugr-platform - Development Guidelines

The hugr platform's orchestrator: a Kubernetes operator. It keeps the node groups the policy catalog names (in SQL),
answers a node's entry with its bootstrap document, records the heartbeat in Leases, and is a client of the nodes with
its own identity, reading only. The front
(the balancer the clients reach) and the web console come from the same repository. Read `README.md` first, then the
specs in `specs/`.

## Technology

- **Go** (the same stack as kista, hugr-lab/duckdb-extension-repository), one module.
- **A Kubernetes operator** on controller-runtime: the custom resources `HugrPlatform` and `HugrNodeGroup` hold the
  desired state, Leases and the resources' status hold the fleet's. **No database of its own.**
- **Shared logic in packages with no Kubernetes dependency** (the node protocol, a group's expansion), so a
  bare-metal service can import them when one is needed - not before.
- **Authentication**:
  - A node is a pod of a group: its projected ServiceAccount token, checked by TokenReview.
  - The operator itself is a workload identity federated into the organisation's IdP, a client of the nodes like
    any principal.
  - People sign in through the same IdP.

## Rules the owner set

- **One lightweight spec per feature**, `specs/NNN-slug/spec.md` from `specs/TEMPLATE.md`. The owner accepts a spec
  before its code is written. One branch per spec (`NNN-slug`), and push only when asked.
- **Every change lands with tests**, and three adversarial self-review passes come before a PR.
- **PR flow**: CI green, then squash-merge, then check the main runs.
- **Never a credential at rest**: nothing in the control plane's config, its database, its documents or its logs.
  - A bootstrap document holds pointers only, and a secret is named, never carried.
  - The service's own database login comes from the deployment (a mounted file or the environment), never from
    the repository.
- **The control plane decides nothing for a node.** It serves what the operator configured and records what nodes
  report. Profile, policy and secrets have their own homes (the policy catalog, tresor-server).
- **Products stay products.** kista and tresor-server are used through their protocols; their sessions get only
  product gaps the platform needs, never our deployment configuration.
- **Scale by statelessness**: any replica answers any request, state lives in Kubernetes, and "lost" is an expired
  Lease. Reconciling is the leader's.

## Commands

The commands come with spec 001's code.
