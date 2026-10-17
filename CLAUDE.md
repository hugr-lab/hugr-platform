# hugr-platform - Development Guidelines

The hugr platform's control plane: the bootstrap document a node starts from, the heartbeat it reports, and the
fleet's view for the operator. Read `README.md` first, then the specs in `specs/`.

## Technology

- **Go** (the same stack as kista, hugr-lab/duckdb-extension-repository), one module and one binary.
- **Its own database**: PostgreSQL in a deployment, SQLite (CGO-free) for development and tests.
- **Authentication**: OIDC bearer tokens verified offline against the issuer's JWKS. Nodes present their workload
  identity, and operators a token carrying the platform's admin role.

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
- **Scale by statelessness**: any replica answers any request, state lives in the database, and "lost" is computed
  when read. Work for one replica takes a database lease.

## Commands

The commands come with spec 001's code.
