# Spec 001: the minimal control plane - bootstrap, heartbeat, the fleet's view

- **Status**: draft
- **Date**: 2026-10-17

## Summary

Phase 1 of the platform has one node group behind one control plane. This spec builds the control plane as one Go
binary, `hugr-platform`, with three jobs:

- **The bootstrap document.** A node asks for it at start (hugr-node spec 006), and it is answered by the node's
  identity.
- **The heartbeat.** It records what each node reports (hugr-node spec 009 sends it).
- **The fleet's view.** It shows the operator the nodes and the cluster's convergence.

It decides nothing for a node, and it holds no profile, policy or secret. Its replicas are stateless, sharing one
PostgreSQL; SQLite serves for development.

## Problem

- **Nobody answers the bootstrap.** hugr-node's bootstrap (spec 006) fetches its document from `GET
  <HUGR_CONTROL_PLANE>/v1/bootstrap`, and nothing serves it: the e2e runs a 30-line python fake with a static token.
- **Nobody sees the fleet.** The operator cannot tell which nodes run, at which profile version, which need a
  restart, and which are gone.

## Design

### The binary and its configuration

- **The binary**: `hugr-platform serve -config <file>` and `hugr-platform migrate -config <file>`.
- **The configuration** is one YAML file. Nothing in it is a credential: the database login is a file path or an
  environment variable, which the deployment provides.

```yaml
listen:
  addr: ":8443"
  tls: {cert_file: /tls/tls.crt, key_file: /tls/tls.key}   # absent: plain http behind a TLS-terminating ingress
store:
  kind: postgres                        # or sqlite: {path: ./hugr-platform.db}
  dsn_file: /run/secrets/hugr-platform/dsn
issuer:
  url: https://kc.example/realms/hugr   # its JWKS by OIDC discovery
  audience: hugr-platform               # the audience every token must carry
operators:
  roles_claim: realm_access.roles       # a path into the token
  role: platform-admin
heartbeat:
  interval: 15s                         # lost = no heartbeat for 3 intervals
groups:
  analytics:
    identities:                         # who is a node of this group: claim = value, every listed claim must hold
      - {azp: hugr-node-analytics}
      - {sub: "system:serviceaccount:hugr:hugr-node"}
    bootstrap:                          # hugr-node spec 006's document (version 1), pointers only
      repository: {name: hugr, prefix: "https://ext.example/hugr/prod"}
      catalog: {type: postgres, extension: postgres_scanner, path: "", secret: policy_catalog, schema: acl}
```

The group's name is the document's `node_group`. Phase 1 has one group, and the configuration allows several.

### Authentication

- **Every call** carries `Authorization: Bearer <JWT>`. The token comes from the configured issuer and carries the
  configured audience. It is verified offline against the issuer's JWKS, which is cached and re-read when a token
  names an unknown `kid`. The usual checks apply: `exp`, `nbf`, a 60 s skew, RS256 and ES256 only.
- **A node** is a token that matches a group's `identities`. How the node obtains it is the node's business. In
  spec 006 it is an http secret in tresor scoped to the control plane's URL, which a dynamic secret mints per call.
- **An operator** is a token whose `roles_claim` holds `role`. A token that is neither is refused, with 403.

### The API

| call | who | answers |
| --- | --- | --- |
| `GET /v1/bootstrap` | a node | its group's document |
| `POST /v1/heartbeat` | a node | 204; idempotent, the last report wins |
| `GET /v1/nodes`, `GET /v1/nodes/{id}` | an operator | the nodes, with state `live` or `lost` |
| `GET /v1/cluster` | an operator | per group, as below |
| `GET /healthz`, `GET /readyz` | anyone | the process; ready means the database answers |

**`GET /v1/cluster`**, for each group:

- the target (the highest `config.target` live nodes report);
- the nodes that have applied it;
- those that need a restart;
- those that are lost.

The control plane never reads the policy catalog: the target is what the nodes themselves read there.

### The heartbeat (the contract hugr-node spec 009 sends)

```json
{"node_id": "hugr-node-7d9f-abcde", "image": "v0.1.0", "duckdb": "7304f7b115",
 "config": {"target": 12, "applied": 11, "needs_restart": true,
            "pending": [{"kind": "extension", "name": "mssql", "class": "restart", "error": ""}]},
 "drift": {"differs": 0, "node_only": 1, "profile_only": 0},
 "load": { /* acl_node_load() as it is */ }}
```

- **`node_id`** is the instance (the pod's name), while the token says which group it may speak for. A node reports
  only for its own group: a `node_id` already recorded under another group is refused, with 409.
- **Body limit**: 256 KiB. Unknown fields are kept, so the node may report more than this spec names.

### Data

- **`nodes`**: the node id, its group, the identity it reported with, the image and duckdb versions, the config
  target and applied version, `needs_restart`, the pending items, the drift, the load (JSON), and first_seen /
  last_seen. Each heartbeat is one upsert.
- **`node_events`**: written only when something changes — a node appears, its versions change, its target or
  applied version moves, `needs_restart` flips, it comes back after being lost. A steady fleet writes nothing but
  upserts: about 67 a second for 1000 nodes at 15 s.
- **Lost is computed when read**: `now - last_seen > 3 × interval`. No replica keeps a timer.
- **Migrations** are embedded and run by `hugr-platform migrate`, never at serve time. The same SQL runs on
  PostgreSQL and on SQLite.

## Security

- **No credential at rest.** The configuration names a DSN file. The database holds no token, and the documents
  hold pointers. Logs carry no token: a refusal logs the reason and the token's issuer and subject.
- **The bootstrap reveals a group's pointers only to that group's nodes.** An operator reads the fleet, not the
  documents.
- **Unauthenticated routes** are `healthz` and `readyz` only.

## Testing

- **Unit tests**: configuration parsing (a credential-looking key refused), claim matching, and the lost computation.
- **The store** on SQLite always, and on PostgreSQL in CI (a service container).
- **The API** against a fake issuer: an in-process JWKS with RSA and EC keys, covering each route for a node, an
  operator, an expired token, a foreign audience and a node of another group.
- **CI**: GitHub Actions, `go test ./...`, `go vet`, `staticcheck`.
- **The image**: a static binary in a distroless image, `ghcr.io/hugr-lab/hugr-platform`, released like
  hugr-node's.

## Open decisions (owner)

| # | question | proposal |
| --- | --- | --- |
| 1 | Where the groups (identities and bootstrap documents) come from in phase 1 | the configuration file; a change is a rollout. An API comes when a console needs it |
| 2 | The control plane's own database login | a file the deployment mounts (a Kubernetes Secret), like the nodes' static catalog secret in phase 1; through tresor later |
| 3 | TLS | both: in-process with cert files, or plain http behind an ingress that terminates TLS (the node insists on https either way) |

## Alternatives considered

- **Heartbeats as rows in the policy catalog.** Refused on 2026-10-15: the fleet would write into the policy's
  database.
- **A node registry before a node runs.** Not in phase 1: identity decides the group, and a node appears with its
  first heartbeat.

## Follow-ups

- hugr-node spec 009 (the tick and the heartbeat sender), and this repository's spec 002 (the demo in kind).
- Actions (a rollout across a group, a canary), an API for the groups, a console: phase 2.
