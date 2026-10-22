# Spec 009: the SQL page enters the door through tresor

- **Status**: accepted
- **Date**: 2026-10-22
- **Author**: hugr lab (the owner, 2026-10-22: "через tresor" - the general scenario: DBeaver, the CLI and DuckDB-wasm
  take the door's credential from tresor, a page with no second login; accepted the same day, with tresor on every
  platform)

## Summary

The console's SQL page (spec 007) puts the console's own token into a quack secret of the page. Now the page hands
tresor its token for tresor's audience (`tresor_web_token`, tresor spec 043), attaches it, and the door's secret comes
from tresor: a `quack` secret whose token tresor mints for the caller by token exchange on every read. The page writes
no token into a secret. The same secret serves DBeaver or the CLI through the front.

A platform always has tresor (the owner, 2026-10-22: the nodes share their secrets through it). A single node
without tresor is not the platform: for it the open duckdb-acl is the simpler choice.

## Problem

- The page holds the credential itself: it writes the console's token into a `quack` secret before every run. That is
  the console's way, not one a client of the platform can follow.
- tresor (the client) now loads in DuckDB-wasm and takes a page's token with no login of its own (tresor specs 042,
  043; their wasm build loads into our duckdb-wasm, 2026-10-22). tresor-server mints a token for the caller in any
  parameter (`mint+exchange://<audience>`, its spec 023). Nothing of the platform uses either yet.
- tresor-server's discovery names one `api`, its `public_url`: the nodes reach it at its own address, so the
  console cannot make that address its own, and the page reaches only the console's origin (spec 007's CSP).

## Design

### The console: tresor's discovery under its origin

- The console already serves tresor's `/tresor/v1/` and `/tresor/admin/v1/` (spec 003's mount, tresor's own console).
- New: `GET /tresor/.well-known/duckdb-secrets` is the console's own answer. It reads tresor-server's discovery
  (the mount's upstream and roots) and answers it with `api` replaced by `https://<the request's host>/tresor`.
  Nothing else changes. The request's host is the one the page asked; a client asking with another host gets its
  own host back, nothing more.
  - tresor (the client) attaches `'<console host>/tresor'`: the discovery is
    `https://<console host>/tresor/.well-known/duckdb-secrets`, then every call goes to `api`, the console's
    `/tresor/v1/…`, which the mount forwards as today.
  - At ATTACH tresor also reads the IdP's `openid-configuration`: the console's CSP allows the IdP already.
- No CORS anywhere: tresor-server's spec 016 recommends this (same origin, by a proxy).

### The page

- duckdb-wasm's release (spec 007's workflow) also builds tresor for `wasm_eh` at a pin (`web/console/tresor.pin`,
  tresor main 40274ef or later; emscripten 6.0.9, `OVERRIDE_GIT_DESCRIBE=v2.0.0`), shipped as
  `dist/extensions/tresor.duckdb_extension.wasm` and loaded unsigned from the console's origin, like quack and httpfs.
- Before each run the page:
  1. hands tresor the person's token for that audience, renewed by the console's session as today:
     `FROM tresor_web_token(?, ?, issuer := ?)` with the service `tresor:<host>/tresor`, the token and the IdP's
     issuer as parameters - tresor-server lists every issuer it trusts (the cluster's too), so the page names its own;
  2. attaches it once: `ATTACH IF NOT EXISTS 'tresor:<host>/tresor' AS hugr_tresor (ISSUER '<issuer>')`;
  3. asks DuckDB which secret it would use for the door: `which_secret('quack:<host>:<port>', 'quack')`, and runs
     with tresor's.
- **One way in.** The page's own secret with the console's token (stage 1) is gone. When tresor serves no secret for
  the door (its scope does not name this address, or the person has no `use` grant), the page runs nothing and says
  what is missing: "tresor serves no secret for quack:<host>:<port> - hugr_door's scope (door.scopes) and a use
  grant (door.grants)". A tresor failure (the service down, the token refused) is shown as tresor's error.
- **Another person, another DuckDB.** tresor keeps a door's minted token for its next lookup, whoever asks. When the
  token handed over names another `iss|sub` (read, never verified) - a popup's sign-in as someone else - the page ends
  its DuckDB and opens a new one. The console's own tokens of their own audience (Entra) are dropped the same way.
- `console.audiences.tresor` becomes required (the CRD): the page needs a token for tresor. Every chart, example and
  the demo set it already.

### The door's secret in tresor (hugr-init)

- hugr-init (spec 005) writes it beside `hugr_policy_catalog`, once, and grants `use`:
  - `hugr_door`, type `quack`;
  - scope: the doors people reach, an install value (`door.scopes`, e.g. `quack:console.example.org:443` and the
    front's public `quack:hugr.example.org:9494`). The platform knows neither address: the administrator's ingress
    and load balancer make them;
  - params:
    - `TOKEN 'mint+exchange://<console.audiences.nodes>'`;
    - `EXTRA_HTTP_HEADERS MAP {'Authorization': 'Bearer ${exchange://<console.audiences.nodes>}'}`.
      The console's `/quack` admits a signed-in person only (spec 007). One exchange covers both: tresor-server
      caches the minted token per caller and audience.
  - grants: an install value (`door.grants`, `role:…` / `group:…`, the people who use the platform's doors). Never the
    nodes' (`tresor.grants`): under a delegation a node would mint a person's token. hugr-init refuses the overlap.
- `door.scopes` and `door.grants` are required, as hugr-init's `grants` are: the umbrella chart refuses an install
  without them. A scope is `quack:<host>:<port>`, the host in lower case (as a browser writes it), the port 1..65535;
  a loopback host is refused unless `door.allowLoopback` (a demo's stand: a client goes to a loopback door without TLS
  by default).
- The audience defaults to `console.audiences.nodes`. On Entra that is an `api://` URI, and On-Behalf-Of mints by the
  API's client id (a GUID): the chart refuses an `api://` audience, so an Entra install names `door.audience`.
- An existing `hugr_door` is left as it is, as `hugr_policy_catalog` is (nothing is rewritten by itself), and granted
  only while it is a `quack` secret within `door.scopes`, made for its reader (`dynamic`): another type, no scope, a
  door not named or a fixed value fails the run, granting nothing - as does a name taken by an entry the listing does
  not show (the write refused as taken). The grants are written again at every run: a revoke goes in the values.
- The database administrator's one-time Secret is deleted once the catalog is done, before this step: a failing
  doors' write leaves no credential behind.

### The policy and the IdP

- A minted token is the person's, asked by tresor-server's exchange client: its `azp` is that client's.
  - The node counts it only through a policy client with that `azp`: the install adds one to `firstPolicy.clients`
    (e.g. `{name: tresor, clientID: <the exchange client>}`).
  - hugr-node spec 015 maps the administrators' claim per client, so a person keeps their rights through tresor.
  - A catalog that already has its policy gets the client by hand (`CREATE CLIENT … AZP (…)` and the mapping), as
    any later change.
- tresor-server's issuer gets an `exchange` client.
  - Keycloak:
    - `client_auth: file`, the pod's projected ServiceAccount token, no secret;
    - standard token exchange enabled;
    - the nodes' audience available to it (an audience mapper).
  - Entra: `on_behalf_of`, the nodes' API delegated to tresor's application with admin consent (tresor-server's
    `token-exchange.md`).

## Security

- **The page writes no door credential.** The page's token for tresor's audience goes to tresor alone, as a prepared
  statement's parameter (tresor spec 043). The token for the nodes is minted by tresor-server for that same person
  and kept in the worker's memory. On Keycloak the console's sign-in token carries the nodes' audience too (one token
  for every audience): the page holds it as the console does, and writes it into no secret.
- **A minted token is the caller's own.**
  - tresor-server exchanges the bearer of the read. Under a delegation grant it is the grant's user's.
  - It refuses to mint for its own audience, and refuses a minted token that names it.
- **The secret is sent only to the doors in its scope.** DuckDB matches a scope as a prefix of the address it looks
  up (`quack:<host>:<port>`): `quack:console.example.org:443` also covers ports 4430-4439 and 44300-44399 of that
  host - the platform's own. Never another host's quack server.
- **The node judges as always.**
  - The minted token counts only through the policy's client for tresor.
  - An administrator reaches `platform_admin` only through that client's own mapping (duckdb-acl spec 095).
  - Another application's token gets nothing.
- **The discovery the console answers changes only `api`.** The issuers, the audience and the flows are
  tresor-server's own: the console cannot widen what tresor accepts.

## Testing

- **Go**:
  - the discovery: `api` rewritten to the request's host, every other field as the upstream's;
  - the upstream down gives 502.
- **hugr-init**:
  - `hugr_door` written with its scopes, params and grants;
  - its configuration refused without `door.scopes` or `door.grants`;
  - left alone when it exists.
- **vitest**: the page's statements (the hand-over, the ATTACH, `which_secret`), and the refusal when tresor serves
  no secret for the door.
- **Browser e2e** (`test/console/e2e/run.sh`) gains tresor-server v0.7.5 (built by `go install`, its state in memory)
  with a Keycloak exchange client (the stand's own: a client secret made for the run, in Keycloak's realm file and
  tresor-server's environment, never on a command line), a second issuer (as a platform's tresor trusts the
  cluster's: the page must name its own), the node's policy client for it, and `hugr_door` written as hugr-init
  writes it:
  - an admin's run uses tresor's secret, and its results match stage 1's (both modes);
  - a person with no `use` grant gets the page's refusal naming what is missing, and nothing runs.
- **vitest**: each run hands over the session's current token, named by its issuer; the ATTACH once; the door's
  secret looked up; another person told apart.
- **Demo** (spec 004, Keycloak):
  - the realm's exchange client `duckdb-secrets` (tresor's projected token, no secret) and the target `hugr` - an
    exchange's audience must be a client of the realm; tresor trusts the run's CA for the IdP's token endpoint by
    `SSL_CERT_DIR` (its `ca_file` covers discovery and keys only);
  - hugr-init writes `hugr_door` for the console as the checks and the stand reach it; `firstPolicy` has the client
    `tresor`;
  - check 4c: tresor's discovery under the console's `/tresor`; the admin reads `hugr_door` there (the checker's
    `-door`) - a token tresor minted, not the admin's own - and the console's `me` with that token answers the
    admin's bundles: the node took it through the policy's client for tresor;
  - the browser part is the e2e's.

## Alternatives considered

- **tresor-server's `public_url` as the console's `/tresor`**: one `api` for every client, so the nodes too would
  reach tresor through the console.
- **CORS** (tresor-server's `cors.allowed_origins`): the page would reach tresor-server's own address, which is
  in-cluster on most installs, and the console's CSP would open another origin.
- **The door's secret with the console's token, kept natively** (alone, or as a fallback): the page goes on holding
  a credential, a fallback would hide a missing grant or scope, and nothing of the general scenario is proven (the
  owner chose tresor; a platform always has it).
- **One broad scope (`quack:`)**: a client's every quack server would receive a token for the nodes.

## Follow-ups

- **The AKS demo (Entra)**: tresor's On-Behalf-Of for the nodes' API (its application delegated the API, admin consent),
  with the next live AKS run; the doors' audience is the API's client id there. Until then the demo checks only
  tresor's discovery there.

- A third mode of the page, statements in the page's own DuckDB (tresor's `whoami`, a person's secrets), only if
  asked.
- The tresor helper `attachTresor` (`@hugr-lab/tresor-web`) instead of the page's statements, once it is published.
