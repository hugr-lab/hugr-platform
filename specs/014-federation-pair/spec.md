# Spec 014: a platform as a source of another - the token kept, the pair in the demo

- **Status**: draft
- **Date**: 2026-11-26

## Summary

Platform A takes platform B as a source, the way it takes PostgreSQL. A runs `ACL CLUSTER ATTACH '<B's front>' AS b
(TYPE hugr, SECRET b)`, and the secret `b` lives in A's tresor. Nothing on either side is special to federation (the
owner, 2026-11-26):
- B sets up its consumer with ordinary SQL: an `ISSUER`, a `CLIENT`, a mapping, a role, its grants and a resource
  group.
- A attaches B as any source; only its authentication differs, as each source's does.

What this spec has to make true is the token. A's node must get the right token for B, and keep it fresh for as long as
the link lives. The demo proves it with two platforms in one cluster.

## Problem

- **The `hugr` type is built** (hugr-node spec 011, stage 1, in hugr-node v0.4.1). Its token chain has run only in
  hugr-node's own e2e, against a tresor-server and a Keycloak of the test's own, never on a platform:
  - tresor's `mint+exchange://<audience>` is an RFC 8693 exchange done for the caller, here A's node and its workload
    identity;
  - the type reads the secret at each connect, and a reconnect is the refresh.
- **Nothing proves the platform's wiring:**
  - the IdP has to let A's node identity be exchanged for B's audience;
  - B has to verify that token and map it to a role;
  - A's nodes have to trust B's front certificate;
  - the token has to outlive its own `exp` through reconnects;
  - an expired, revoked or ended session has to come back, or be refused, as spec 011 says.

## Design

### The pair in the demo

- **B is a second platform in its own namespace**, `hugr-b`, with one group of one node.
  - It uses the platform chart alone: spec 005's strict install, given tresor and kista.
  - It shares A's tresor-server, kista, PostgreSQL server and Keycloak realm. A and B run one operator version, since
    the CRDs are the cluster's.
  - Its cluster-scoped objects are already named after the namespace (`<ns>-hugr-platform`), and the operator watches
    only its own namespace.
- **What B needs from the shared services:**

  | service | what B gets |
  | --- | --- |
  | catalog | its own database and login (`hugr_b` / `hugr_b_catalog`) |
  | tresor | its own secret names (`hugr_b_policy_catalog`, `hugr_b_door`); its hugr-init subject among tresor's admins |
  | Keycloak | its nodes' clients (`system:serviceaccount:hugr-b:hugr-node-<g>`) and their own group (`/hugr-b-nodes`, so they never get A's catalog secret) |
  | kista | nothing: the same channel, read-only |
- **B's data** is a table of its own, `shared.orders`, in a database of its own in the demo's PostgreSQL. B's catalog
  `shared` holds it, with an `RLS` that A's consumer role cannot step over.

### The token

- **On the IdP (Keycloak in the demo)**:
  - B's audience is a client of its own, `hugr-b`, and never A's `hugr`: A's own tokens must not pass at B;
  - tresor's exchange client (`duckdb-secrets`) carries an audience mapper that includes `hugr-b`, as it does for
    `hugr` today (the `hugr_door` secret).
  - A's node token already names `duckdb-secrets`, which is what Keycloak's exchange requires of a subject token.
  - On Entra, the same is an app registration for B, and A's node identity is given an application permission on it.
- **On B, ordinary SQL in its seed:**

  ```sql
  CREATE CLIENT consumer_a ISSUER idp AUDIENCES ('hugr-b') AZP 'duckdb-secrets';
  MAP CLAIM 'hugr_node' FROM CLIENT consumer_a TO ROLE platform_a;
  GRANT CATALOG shared TO ROLE platform_a WITH (select) MAIN RLS '…';
  GRANT RESOURCE GROUP <B's group> TO ROLE platform_a;
  ```

  The claim depends on what an exchanged token carries. The demo reads one and writes down what it holds.
- **On A, ordinary SQL in its seed:**

  ```sql
  CREATE PERSISTENT SECRET b IN owner (TYPE hugr, SCOPE 'hugr:hugr-front.hugr-b.svc:<quack port>',
                                       TOKEN 'mint+exchange://hugr-b');
  CALL owner.grant_secret('b', '<the nodes' principal>', ['use']);
  ACL CLUSTER ATTACH 'hugr-front.hugr-b.svc:<quack port>' AS b (TYPE hugr, SECRET b);
  ADD TABLE b.shared.orders AS demo.orders_b;
  ```

  Then analyst_a reads `demo.orders_b`.
- **A trusts B's front.** B's front certificate comes from B's internal CA, which is a namespaced Issuer. A's chart
  value `caBundle` adds B's CA to A's `hugr-ca-bundle`.
- **The token is read as the node.** hugr-node reads the secret in a system transaction (spec 011). A's actor rule
  lets nodes act for sessions, so a read inside a person's session would otherwise exchange the person's token. The
  demo checks which subject B sees.

### Kept fresh

The token's life in the demo is short: the `hugr-b` client's access token lifespan is 2 min. The checks are:
1. **Read.** analyst_a reads B's rows through A, within B's RLS, and B's sessions list names `platform_a`.
2. **Past the token's exp.** With the link's `max_lifetime` set to 60 s, a read after 3 min still works. Its
   connection was retired and opened again with a fresh token, and B's audit shows a new session.
3. **B ends A's session** (`KILL SESSION`). The next read reconnects once and succeeds (spec 011's "connect again"
   answers).
4. **Revoked.** The client `consumer_a` is dropped on B, so the next connect fails with the attach's error. A's other
   sources keep serving. Recreated, the link serves again.
5. **No token anywhere**: not in either platform's logs, events or `platform.*` views, and none in the demo's output.

## Security

- **A holds no credential for B.** It holds a secret whose token tresor mints for A's node identity at the IdP.
  There is no client secret where the IdP federates the identity.
- **B decides everything A may read:** the client, the mapping, the role's grants, the resource group. A's operator
  only attaches, and a person of A never does.
- **B's audience is distinct from A's**, so a token of A's own is refused at B.

## Testing

- The demo's checks 18 (the pair, read and RLS) and 19 (kept fresh: past exp, B's kill, revocation, no token), on
  minikube; kind if the runner fits the second platform: B's node runs at 250m there.
- Whatever the run finds in hugr-node (the type), tresor or the platform is fixed in its own repository, each with its
  own test.

## Alternatives considered

- **One platform attaching its own front.** It is cheap, but one IdP audience and one CA miss what a second platform
  brings. The owner chose two platforms.
- **A console page or a CRD for consumers and links.** The owner chose none: B is a source like any other.

## Open

- What an exchanged Keycloak token carries for the mapping on B (`azp`, `sub`, roles) is read in the first run and
  written here.
- Whether kind's 4-core runner fits B. If it does not, check 18 runs on minikube only, and CI runs A alone.
