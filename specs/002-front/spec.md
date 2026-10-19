# Spec 002: the front - one address for the clients, every call on its node

- **Status**: accepted (the owner, 2026-10-19: «спека одобрена»; Q1-Q6 as recommended)
- **Date**: 2026-10-19

## Summary

The front is the address clients reach: a stateless proxy for the nodes' two doors, Flight SQL (gRPC) and quack (HTTP).
It terminates TLS, puts a client's new connection on a node that may take it, and from then on sends every call of
that connection to the same node by the node's label, which the node itself wrote into the connection's identifiers
(duckdb-acl spec 134). It keeps no table of sessions: any replica routes any call, and replicas never talk to each
other. It reads the fleet from Kubernetes - the groups' EndpointSlices and the nodes' Leases (spec 001) - and decides
nothing about a principal: where a principal belongs is the node's answer (duckdb-acl spec 096), which the front
follows by seating the connection again on a node of the group the node named.

## Problem

- **Clients have no single address.** The demo (spec 004) reaches nodes through port-forwards, one node at a time.
- **A node is not a stable address.** Pods come and go (rolling updates, scale-down, a lost node), and a connection's
  state - a Flight session, a quack connection - lives on one node.
- **The group a principal belongs to is known only to the node.** A client does not know it, and the platform must
  not guess it (design/082 F2: a gateway that falls back to a default group breaks the isolation of ETL from
  interactive work).
- **The doors' certificate is the cluster's** (spec 001 decision 3), not one a client outside the cluster trusts.

## Design

### Where it runs

- **A binary of this repository** (`cmd/front`), in the same image as the operator, started by its own Deployment.
- **The operator builds the front's objects** from `HugrPlatform.spec.front`:
  - a ServiceAccount `hugr-front`, with a Role that may `list` / `watch` EndpointSlices and Leases in the platform's
    namespace and nothing else;
  - a Deployment `hugr-front`: `replicas` (2 by default), a readiness probe (below), `terminationGracePeriodSeconds`
    of the drain below + 5 s, the public certificate's Secret and the cluster CA bundle mounted;
  - a Service `hugr-front`: the Flight and quack ports, of `serviceType` (`ClusterIP` by default; `LoadBalancer` where
    the clients come from outside - Azure's L4 balancer, an NLB);
  - a PodDisruptionBudget with `minAvailable: 1`;
  - the NetworkPolicy of spec 001 already admits the front's pods to the doors' ports.
- **`spec.front`**: `replicas`, `serviceType`, `certificate` (a Secret with the public certificate and key; or
  cert-manager's `issuerRef`), `flightPort` (32010, the nodes' own), `quackPort` (9494), `noNodeWait` (5 s).

### The fleet, as the front sees it

- **Nodes**: the EndpointSlices of the groups' headless Services (`hugr-node-<group>`, spec 001). An endpoint is a pod:
  its name (the node's label), its address, `ready`, `serving`. The group is the Service's.
- **Whether a node takes new sessions**: its Lease (spec 001), named like the pod. It is current while
  `renewTime + leaseDurationSeconds` is ahead, and its annotations say `admit-new-session`, `draining`, `group-known`,
  `sessions`, `seats-left`.
- **A node is placeable** when its endpoint is `ready`, its Lease is current and says `admit-new-session = true`. A
  node with no Lease yet (a new pod before its first heartbeat) or an expired one (design/082 O2) is not placeable; its
  existing connections are still routed to it while its endpoint is `serving`.
- **The default group** (where an ungrouped principal is served, duckdb-acl spec 096's `DEFAULT`) is in the Lease: a
  duckdb-acl addendum adds `group_default` to the load report (`acl_node_load()`), hugr-node reports it, and the
  operator writes it as `platform.hugr.io/group-default`. It is a hint for the first seat only: a wrong one costs one
  re-seat, never a wrong placement - the node decides.
- **Readiness of the front** (design/082 F3): ready once both informers (EndpointSlices, Leases) have synced, and only
  then. A replica that loses its watch for longer than 3 × the heartbeat interval turns not ready.

### Seating a new connection

- **Which group**: the default group's nodes; if the Leases name none, every group's.
- **Which node**: of the placeable nodes of that group, two at random, then the one with more seats left, then fewer
  sessions (spec 001's summary, refreshed with each heartbeat). Two random choices keep the replicas, which never
  coordinate, from all piling on the same node.
- **The node's refusals at seating**:
  - `wrong_resource_group` with the groups that serve the principal (the addendum's `x-acl-resource-groups`: a Flight
    trailer, a header on quack's CONNECT answer) - seat again on a placeable node of the first of those groups that has
    one. At most two re-seats, then the node's refusal goes to the client as it was.
  - `at_capacity`, `draining` - another placeable node of the same group, at most twice.
  - Any other answer is the client's.
- **No placeable node in the group** (design/082 F2): wait up to `noNodeWait` for one (a node coming up, a drain
  ending), then refuse with UNAVAILABLE (Flight) / 503 (quack) `hugr: no node of group <g> can take a new session -
  retry`. Never another group's node.
- **Sticky from the first answer on**: the node's answer to the call the front seated carries the label (Flight's
  `acl_node` cookie, quack's connection id `<label>.<random>`), and every later call of the connection carries it back.

### Routing a call that carries a label

- **The label** comes from, first match wins:
  - Flight: the `acl_node` cookie in the call's `cookie` metadata (duckdb-acl spec 134);
  - quack: the `acl_node` cookie or the `X-Quack-Connection-Id` header (a client with duckdb/duckdb-quack#297, e.g.
    hugr-node's `hugr` type); else the connection id in the message's header, the first bytes of the body (a stock quack
    client: the header is `type`, `connection_id`, `client_query_id` in DuckDB's binary serialization - the front reads
    that much and forwards the body untouched).
- **The label names a node the front sees** (`serving`): the call goes there, placeable or not - a draining node keeps
  its connections until they end (duckdb-acl spec 066).
- **The label names no node** (the pod is gone, or the label is foreign):
  - Flight: the call is seated as a new connection. The node that takes it does not know the cookie and seats a session
    from the call's token and the session's state cookie (duckdb-acl spec 135): the session moves, or refuses once
    naming what could not move.
  - quack: the call goes to a placeable node of the default group, which answers `acl: this connection is not known on
    this node - connect again` (spec 135); the client connects again, and the front seats the CONNECT. The front writes
    no quack message of its own: the protocol's encoding stays the nodes'.
- **The front never rewrites** a cookie, a header or a body. It adds nothing a client could have sent itself.

### The two protocols

- **Flight SQL**: an HTTP/2 gRPC proxy with a raw codec (no message decoding). A call is forwarded whole on one
  upstream stream; streams in both directions are pumped, and a cancel or an end on either side ends the other.
  - A re-seat replays the call's first request message, which the front keeps for that purpose - only for a call whose
    client sent one message before the node answered (`Handshake`, `GetFlightInfo`, `DoGet`, `DoAction`, `GetSchema`).
    A refusal of a call that streamed more (`DoPut`, `DoExchange`) goes to the client as it was; drivers seat a session
    with a single-message call first.
  - Upstream connections to the nodes are HTTP/2 and pooled per node.
- **quack**: an HTTP/1.1 reverse proxy for `POST /quack` (each request routed on its own) and `GET
  /.well-known/quack-auth` (any placeable node of the default group: the discovery document is the policy's, the same on
  every node). `/.well-known/acl-node` and `/metrics` are not exposed: the console and the operator reach the nodes
  directly (spec 001).
- **TLS**: the front terminates the client's TLS with the public certificate. To a node it is a TLS client again: the
  pod's address, the group's name `hugr-node-<group>.<ns>.svc` as the server name, verified against the cluster CA
  bundle (spec 001 decision 3). A redirect from a node is never followed.

### Stopping, scaling

- **SIGTERM**: not ready at once, then stop accepting; HTTP/2 GOAWAY to every client connection, so gRPC clients move to
  another replica between calls; streams in flight get up to 30 s to end, then they are cut. quack opens a request per
  call and needs nothing more.
- **Scaling**: the front's work is bytes in transit (a `DoGet`, a `FETCH`), so it scales on network, by replicas - an
  HPA on CPU as a first proxy; nothing to share between replicas.

### What it reports

- `/metrics` (Prometheus) and `/healthz` on an internal port, never through the public Service:
  - `hugr_front_calls{protocol, outcome}` - routed, seated, re-seated, no_node, upstream_error;
  - `hugr_front_reseats{reason}` - wrong_resource_group, at_capacity, draining;
  - `hugr_front_streams_open{protocol}`, `hugr_front_bytes{protocol, direction}`;
  - the fleet as seen: `hugr_front_nodes{group, state}` (placeable, serving only, unknown).
- Logs: a call's protocol, method, node and outcome - never a token, a cookie's value, a connection id or a body.

### duckdb-acl, before the front ships

One addendum to spec 096 (the front needs both; neither changes a decision of the node):

- **`x-acl-resource-groups`**: on a `wrong_resource_group` refusal, the groups that serve the principal, as a Flight
  trailer and as a header on quack's CONNECT answer. It says what the refusal's text already says.
- **`group_default`** in the load report: whether this node's group is the policy's default. hugr-node carries it in the
  heartbeat (its `load`), and the operator writes the Lease annotation.

## Security

- **The front authenticates nobody and decides nothing.** A token passes through untouched and is verified by the node
  (duckdb-acl spec 095). The route is a hint (design/082 F4): a forged label reaches a node that does not know the
  connection and answers as for any stranger - a Flight session is seated only by a token that verifies, a quack call
  is refused.
- **Placement is the node's**: a principal never lands outside its group, because the node of the wrong group refuses
  it; the front only follows the refusal.
- **No credential at rest or in a log**: the front holds the public certificate's key (a Secret it mounts) and nothing
  else; it never logs a header's value, a cookie, a connection id (its random part is the connection's secret, spec
  134) or a body.
- **Kubernetes rights**: read-only on EndpointSlices and Leases of the platform's namespace; no Secrets other than its
  own certificate, no pods, no exec.
- **Inside the cluster** the front is a TLS client that verifies each node against the group's name, so a pod that is
  not a node of the group cannot answer for it; the NetworkPolicy keeps the doors to the front and the operator.

## Testing

- **Unit** (pure packages, no Kubernetes):
  - the label from each place it may come from, quack's message header from its binary serialization (a corpus of
    real messages: CONNECT, PREPARE, FETCH, ACK, CANCEL, HEARTBEAT, DISCONNECT), a truncated or hostile body refused;
  - placeability from an endpoint and a Lease (current, expired, missing, draining, not admitting);
  - the choice: two random, seats, sessions, the default group, every group when none is known;
  - re-seats: the groups' order, the limits, which Flight calls replay;
  - no placeable node: the wait, then the refusal, never another group.
- **envtest**: the informers on a real API server - an EndpointSlice and a Lease appear, change, expire; readiness only
  after sync.
- **The demo (spec 004) through the front**: the checks reach the nodes through it instead of port-forwards, and add:
  1. a principal of the second group is seated there by a re-seat, and stays;
  2. a Flight session survives a front replica's deletion (its next call reaches the same node through another
     replica);
  3. a node deleted under a Flight session: the next call is seated elsewhere and the session moves (duckdb-acl spec
     135), a session with a transaction refuses once;
  4. a quack client (duckdb's own, and hugr-node's `hugr` type) keeps its node, and reconnects when the node goes;
  5. a draining node takes no new connection, and keeps its own;
  6. a group with no placeable node: the client is refused after the wait, never seated in another group;
  7. no token, cookie or connection id in the front's logs.

## Alternatives considered

- **Envoy or another stock proxy** (design/017 §3.3): it hashes by a header or a cookie, but neither reads quack's label
  from the body nor seats by the nodes' Leases without a filter of our own.
- **A table of sessions in the front** or a shared store: the nodes already carry the route in their identifiers, and
  a store would be state to replicate (design/015).
- **Polling every node's `/.well-known/acl-node`** for the placement (design/017's first idea): the Leases already carry
  the summary, refreshed with each heartbeat, and the report needs an `observe` bearer the front should not hold.
- **Routing per statement**: breaks the seats' accounting and quack, and loses the node's caches (design/017).

## Decisions (owner, 2026-10-19): Q1-Q6 as recommended below

**Q1. One image or two?** *Recommendation:* one image, two binaries (`operator`, `front`): one release, one scan, and
the front is small.

**Q2. The first seat without a default group in the Leases.** *Recommendation:* any group's placeable node; the node
re-seats the principal (one extra hop on a first call only).

**Q3. A call that streamed before the refusal** (`DoPut`, `DoExchange` as a first call). *Recommendation:* no replay;
the node's refusal goes to the client. The JDBC/ADBC drivers open with `Handshake` or `GetFlightInfo`.

**Q4. quack's label for a stock client**: read the message header from the body, or require #297's header?
*Recommendation:* both - the cookie and the header first, the body's header for a stock client (the
`duckdb-quack#297` client is not released yet).

**Q5. `noNodeWait`.** *Recommendation:* 5 s by default - longer than a pod's restart is pointless, shorter loses a
drain's handover.

**Q6. OIDC termination at the front** (design/016 §5, tools without OIDC). *Recommendation:* not in 002; its own spec
when a tool needs it.

## Follow-ups

- duckdb-acl: the spec 096 addendum above; hugr-node: `group_default` in the heartbeat; this repository: the Lease
  annotation.
- After the release (design/082): F5 (routers in a chain: a group's host name, a client's tag), F6 (an affinity key for
  BI that multiplexes users), Flight's `Location` for very large results (design/017 §3.3).
- Packaging: the front's Service behind the cloud's L4 balancer (Helm, the Azure Managed Application).
