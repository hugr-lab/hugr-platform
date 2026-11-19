# Spec 012: the console reads a node's load report without a session

- **Status**: implemented
- **Date**: 2026-11-19
- **Follows**: spec 003 (the console's node page), spec 011 (an idle node stops), duckdb-acl specs 079 / 097 / 175
  (the load report, its `observe` gate, `idle_since`), hugr-node spec 043 (the node serves the report)

## Summary

The node page reads the node's load report through the Flight Handshake `node-load` with the person's token. That
opens no session on the node. Looking at a node no longer moves its `idle_since`, so the autoscaler (spec 011) still
sees it idle.

## Problem

The page read `SELECT * FROM platform.node_load` over Flight SQL. The pool keeps no session (spec 003), so each call
runs in a per-call session, and that session's end counts as a session's end (duckdb-acl spec 175). With someone's
node page open, the node's `idle_since` kept moving, and an auto group would never stop that node.

## Design

- **`Pool.Load(node, token)`**: one Handshake call carrying the person's bearer, no cookie, and the payload
  `node-load`. Its answer is the report (`acl_node_load()`'s JSON).
  - `FlatLoad` turns the JSON into the row `platform.node_load` answers, with the same column names (`sessions_live`,
    `admit_new_session`, `memory_working`, `sessions_idle_since`, ...).
  - A count becomes an int64.
  - `sessions_idle_since` becomes a time printed as Arrow prints a TIMESTAMP (UTC). Unix seconds would be the other
    reading.
  - A map becomes an object.
  - Only the view's columns are kept. The page reads one shape either way.
- **The statement where the Handshake does not answer.**
  - `Unimplemented`: the node does not serve the report. That is a bootstrap before hugr-node spec 043, or a
    standalone node.
  - `PermissionDenied`: the person reads the view by a point grant alone, without `observe`.
  - Either way the pool runs the statement as before; for those readers the node still sees a session's end.
  - Any other refusal is the node's answer, given to the person as its message. A token that does not verify is one.
- **The page.** `GET nodes/{label}` reads `Load` when the Caller has it. A test's Caller may not, and then the page
  runs the statement. A report the node does not give stays "the node answered no load report".
- The Handshake also shows the doors' certificate, as a statement does.
- **The engine's memory comes with the report.** The node page read `duckdb_memory()`, the memory limit and the spill
  with three statements on every open, and each one moved `idle_since` as well. The engine card now reads the
  report's `memory_*` (duckdb-acl spec 175) instead: working memory against the limit, the buffer pool (cache
  included), the spill against its limit, and the critical level.
  - The breakdown by use (`duckdb_memory()`'s tags) is read only on the person's "By use", and the page says it is a
    statement.
  - A report without memory figures (a node before spec 175) says so.

## Security

Nothing new is reachable. The node judges the bearer as it judges a statement's: verified offline, then `observe`
(a bundle, passthrough, or an unrestricted manage). The console keeps no token. The report holds no principal and no
statement text.

## Testing

- `internal/console/nodes_test.go` runs a fake door with a Handshake and checks:
  - the report comes from one Handshake, with no cookie and no session, flattened to the view's columns;
  - an endpoint that is off, and a point-grant reader, fall back to the statement;
  - a refused token is the node's answer, with no statement after it;
  - a node with no door is unreachable.
  - The test fails when `Load` runs the statement instead.
- `FlatLoad`: an answer that is no report is refused; an older node's report without `memory` reads `nil` there.
- `internal/console/server_test.go`: the node page reads `Load`, runs no statement, and passes on its refusals.
- `web/console/src/screens/node/overview.test.tsx`: the engine card comes from the report, and `nodes/{label}/memory`
  is read only after "By use". It fails against the page as it was.
- `test/console/e2e/run.sh`: against a real duckdb-acl node, serving the report as hugr-node spec 043 makes it, the
  operator's node page shows the engine's working memory, then the buffer pool by use.
- duckdb-acl's `test/e2e/flight/auth.sh` (its spec 175): against the real door, a Handshake read leaves `idle_since`
  where it was and a statement moves it.
- hugr-node's bootstrap e2e: a started node answers the Handshake, but only to a bearer.

## Alternatives considered

- **`GET /.well-known/acl-node`** on the quack listener. A node may serve no quack door, and the pool speaks Flight
  only.
- **The node leaving the reader's own session out of `idle_since`.** It cannot tell the console's call from a
  client's short session.
- **Polling less often.** Any read at all moves `idle_since`.

## Follow-ups

- The node page shows the idle time (`sessions_idle_since`) once the autoscaler acts on it (spec 011).
- Any other page that reads from a node (sessions, events, inventory, the policy) still runs statements. Each one is a
  look into the node, made when the person asks for it.
