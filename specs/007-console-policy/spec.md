# Spec 007: the policy in the console - read it, and query a node from the browser

- **Status**: accepted (2026-10-22); the tresor sign-in waits on what tresor lacks in the browser
- **Date**: 2026-10-22

## Summary

The console shows the access-control policy the way the DBeaver client shows it (acl-clients specs 007, 008): catalogs
and their objects, the grants, roles and their effective rights, the identity providers, the function categories, the
resource groups, the sources and what exposes them. It also has a SQL page: DuckDB in the browser (duckdb-wasm)
attaches the platform through its quack door and runs a statement as the person or, with `passthrough`, under `ACL
NATIVE`. Before the release all of this is read-only, apart from the SQL page. The editors the DBeaver client has, and
view-as, come in a second stage.

## Problem

- **The console holds no policy page.** Spec 006 added one slice: a group's resource group and the roles bound to it.
  A person who operates the cluster from the console reaches for DBeaver to see who may read what.
- **The DBeaver client is a desktop install.** An operator on the console's machine, or one who does not use DBeaver,
  has no view of the policy at all.
- **The node already answers everything.** The `platform` catalog (duckdb-acl spec 117) has the views and read
  functions, and `console_info()` says which of them the person may read. The console only needs pages over them.

## Design

### Stage 1: read (before the release)

The pages follow the DBeaver client's tree. Each reads `platform` views and read functions on a node under the
person's own token, through `onNode` from spec 006 (it tries the next node on a refusal that seats nobody).

| page | reads | the DBeaver client's |
| --- | --- | --- |
| Catalogs, a catalog | `catalogs`, `relations`, `relation_columns`, `schemas`, `functions`, `function_columns`, `references`, `keys`; `check_catalog(c)` | the data tree and Check |
| Grants of a catalog | `grants`, `schema_grants`, `object_grants` (duckdb-acl spec 158 stage 3 dropped `grant_columns`) | the grant nodes: a catalog grant's caps, RLS and versions (it states no columns since spec 158); an object grant's caps, RLS, `COLUMNS` as the grant writes it (`*`, `a, b`, `* EXCLUDE (a, b)`), `MASK` items and versions |
| Roles, a role | `roles`, `role_claims`, `admins`, `platform_grants`, `role_resource_groups`; per catalog the role holds, `effective_rights(role, catalog)` | the role and its Effective rights tab |
| Identity | `issuers`, `clients`, `role_mappings` | Issuers, Clients, Mappings |
| Functions | `function_categories`, `function_category_members`, `function_grants` | Function categories |
| Resource groups | `resource_groups`, `role_resource_groups` | Resource groups |
| Sources | `platform.attached.<db>.<schema>` listed by `information_schema.schemata`; `exposures(source)` | Sources and Exposures |

- **What a page offers is `console_info()`'s.** A view not in `my_views` and a function not in `my_functions` are
  not offered, as the DBeaver client does (`offers()`). A catalog administrator sees the catalog pages for their own
  catalogs only: the node narrows the rows, and the console shows what it gets.
- **No name from the browser becomes SQL as typed** (the console's rule since spec 003):
  - a view is read whole and filtered in the console's server (by catalog, by role);
  - a read function's arguments are names the console read first from the node (a role in `roles`, a catalog in
    `catalogs`, a source in the `attached` listing), written as quoted literals.
- **A large catalog pages in the server.** A view is read once per request, and a page of rows is answered. The node
  bounds no `platform` view (`acl_metadata_max_rows` is its metadata surfaces'), so the statement carries a constant
  `LIMIT 100001`: past 100,000 rows the answer says it read the first ones.
- **A name compares as the node compares it:** a catalog or a source with duckdb-acl's ASCII fold, a role exactly. The
  node's spelling is written, never the browser's. A role or a catalog a grant names is listed with the created ones:
  a grant may name a role the policy never created (an IdP's role granted directly).
- **The slow reads** (`check_catalog`, `exposures` and the sources' listing) wait 45 s, the rest 15 s. A read that
  timed out is not sent to another node: the first may still be at it.

### The SQL page (stage 1): DuckDB in the browser, the node through quack

The page embeds duckdb-wasm, which attaches the platform's quack door as a client does and runs the person's statement
there. acl is not in the browser (its wasm build is out by the owner's decision of 2026-09-14: a tab is no boundary).
The node behind the door enforces the policy, as it does for any quack client.

- **The path:** duckdb-wasm attaches the platform with the quack client. A quack URI has no path, so the client posts
  to `https://<host>/quack`. The console serves `POST /quack` (and `GET /.well-known/quack-auth`) on its own origin and
  forwards them to the front's quack door, which seats the person on a node of their group (spec 002). The browser
  reaches nothing but the console: the nodes and the front stay in-cluster, their certificates the internal CA's, and
  the page needs no CORS. The console forwards only a request of a person signed in to it (the page's quack secret
  sends the console's token as a header too) and takes that header off; the door authenticates the quack session by the
  token in its CONNECT, as for any client.
- **The DuckDB the page runs** (built here, `.github/workflows/duckdb-wasm.yml`, a release per pin):
  - duckdb-wasm at a pinned commit of its main (DuckDB v2.0.0), its `wasm_eh` bindings able to LOAD extensions;
  - quack for `wasm_eh` built at a pinned commit by the same emscripten (6.0.9):
    - The official one could not connect from `wasm_eh`: its client started a `std::thread` per connection. We
      reported it as duckdb/duckdb-quack#328, and quack fixed it the same day (4f4b4c0), which is our pin.
    - A quack built by another emscripten (3.1.71, DuckDB's for its v2.0 extensions) fails to link against the bindings.
    - The `wasm_threads` build loads no extension of DuckDB's v2.0 repository.
  - httpfs, quack's HTTP in wasm, built with it;
  - the console names the release's package by its URL (`web/console/package.json`), the lockfile its integrity; a new
    pin pair is a new release, and the console moves to it by a change of that line, never by itself;
  - DuckDB is 34 MB, 8 gzipped: the console gzips a compressible asset once, at its first request that takes gzip,
    and the page loads it only when the SQL page opens (cached as immutable after that);
  - both shipped in the package and loaded unsigned from the console's own origin, with autoinstall off: the page
    reaches its own origin only. The CSP allows WebAssembly (`'wasm-unsafe-eval'`) and a worker of its own origin.
  - Without a heartbeat (no thread in `wasm_eh`), the node's lease ends a connection idle past its timeout. Each run
    opens a fresh one: `quack_query` connects at its bind and disconnects at its end.
- **The sign-in is the console's:** the person's token (the nodes' audience) goes into a quack secret in the page's
  DuckDB memory, and the door verifies it as any client's (duckdb-acl spec 041). Later: tresor's own flows, when they
  come to the browser.
- **Two modes:**
  - "As you": the statement as written, under the person's principal. It sees the virtual catalog and may use the
    management grammar the person's bundles allow.
  - "NATIVE": offered only with `passthrough`. The page writes `ACL NATIVE ` in front of the statement, the break-glass
    door, where nothing is refused (the owner, 2026-10-15). The page says so above the box.
- **The answer is DuckDB's text**: every value cast to VARCHAR by the page's DuckDB, so a DECIMAL keeps its scale, a
  date reads as a date, a list or a 64-bit integer as they are.
- **The answer is capped** at 1000 rows and 4 MiB of UTF-8: the page reads it batch by batch, stops there, and says
  how much it cut.
- **The text is the person's.** The node judges it as any client's; it is a bound parameter of the page's statement,
  never run in the page's DuckDB. The console logs no quack request - the door's audit records the sessions and the
  statements - only a forward that failed.
- **The door has no deadline in the console** (a PREPARE answers when its statement's first batch is ready); the
  quack client bounds its own wait. A run outliving the person's token past its grace is refused at its next request.
- **What it needs:**
  - a group with its quack door open (`doors.quack`), since the front seats a quack client only on such a node;
  - a front certificate that names `hugr-front.<namespace>.svc`, the name the console dials. The chart's internal CA
    issues it with that name. An organisation's certificate needs the name added, or every `/quack` fails, and the
    operator's log says why.
- **What it opens:** `/quack` puts the front's quack door (ClusterIP by default) within reach of anyone signed in to
  the console, with their own token and rights - as reaching the front directly would.

The same embedding is what the second stage's grids read from.

### Stage 2: the editors, the data, view-as (after the release unless time allows)

- **Data shown as the DBeaver client shows it:** a plain grid of a result, nothing more.
- **View-as:** the DBeaver client's alone (the owner, 2026-11-21).
- **Where an edit is composed** (the owner, 2026-11-21): in the console's server, from the form's fields, and run over
  Flight under the person's token, as the profile's forms are (spec 010). Names are checked against the node's own
  lists. An expression field (a row filter, a mask, a computed column) is the person's SQL, placed in its clause as
  written. The node judges it: one expression or a refusal (duckdb-acl spec 148), and Flight runs one statement. Not
  through the page's DuckDB: that would need a quack door and the 34 MB engine for every editor.
- **The order of the work** (each a PR; hugr-node v0.4.0 carries stage 2a's answers): 2b a group's
  sources, 2c the grant editor, 2d the data grid, 2e roles / resource groups / mappings, 2f catalog objects and their
  repair, 2g issuers / clients / function categories / admin bundles.
- **The DBeaver client's editors,** in the same order spec 008 built them:

    1. The grant editor: catalog / schema / object grants with the row filter, an object grant's `COLUMNS` test and
       `MASK` items (duckdb-acl spec 158) and "Check with the node" (`check_grant`), saved as `GRANT` / `ALTER GRANT`
       statements.
    2. Roles, resource groups, role mappings.
    3. Catalog objects (tables, views, schemas, functions, references) and repair from `check_catalog`.
    4. Issuers, clients, function categories and admin bundles.

As in the DBeaver client, an edit composes the ACL grammar, shows the statements before it runs them, runs them one at
a time on one node and stops at the first refusal, and re-reads the object first to catch a stale form.

#### Stage 2a: what a management statement answers (built 2026-11-20)

Every management statement answers one row, `ok` and `message` (duckdb-acl spec 179; hugr-node's CLUSTER answer keeps
its own fields after the pair, its `note` now the `message`). The console's writes read it (`AnswerOf`, nodes.go):

- `KILL SESSION` and a session's tracing levels: not ok is "this node holds no such session" (they read a lone `true`
  column, which no node answers any more - every end read as not found);
- `DRAIN NODE` / `RESUME NODE` answer `{ok, message}` and the node page shows it: drain's sessions still open, a resume
  that found the node serving as not done;
- a CLUSTER change answers its `ok` / `message` beside the version, class and `appliedHere` (they were read from a
  STRUCT column, which the answer now spreads into columns);
- an answer of any other shape is no answer - never read as done (`bad_answer`, 502).

It needs hugr-node v0.4.0 (spec 179 and duckdb-acl #319), which the charts pin since stage 2a. The editors' answers (a
GRANT's group-source hint, hugr-node spec 035) are shown the same way when the editors come. Tests: `TestAnswerOf`,
`TestDrainAndResumeAnswer`, the kill / tracing / profile tests on the new shape (each fails on the old reading), the web
console's `saidOf` and the node page's drain answer.

#### Stage 2b: a group's sources (hugr-node spec 035, built 2026-11-21)

The group's page has a Sources section, offered with `group_sources` among `console_info()`'s functions (the cluster
bundle's) on a cluster node:

- **The set** (`GET groups/{g}/sources`): the group's `sources` profile item read from `platform.cluster_items` - the
  aliases it lists, or all, with its lazy marks - or none (every source of the cluster and of the group). Beside it,
  the profile's ATTACH items the group's nodes may attach (the cluster's and the group's own), what a set may list.
- **The check**: `platform.group_sources('<g>')`, the group a plain name the platform's group carries, written as a
  literal. Per source: the roles of the group that reach it, whether the set lists it, and for one left out the repair
  the node writes. Another group's own source (one the profile does not attach for this group) is told apart: its
  repair is an `ATTACH … IN GROUP`. A slow read (45 s): it reads what exposes each source. The set is read first and
  on its own, so a check that fails (a resource group the policy lacks, a source whose exposures fail) still leaves
  the set to see and to change, with the check's error beside it. A request stays within 55 s, under the console
  server's write timeout.
- **A repair runs by hand** (`POST groups/{g}/sources/repair` with the source's alias and the repair as shown, the
  infrastructure right). The console reads the check again and runs that row's repair only while it is the text the
  person saw (else 409: read the check again), so the statement is the node's as it stands, never a text from the
  browser, and never one the person did not see. The dialog shows it before it runs, and the answer after.
- **Writes go to a node that writes** (every write of the console, the group-policy ones of spec 006 too). A node of a
  group whose set is a list refuses every write before it runs it (duckdb-acl spec 176), so the console tries a node
  of another group that seats the person; when there is none, the node's refusal is the answer. The set dialog says
  so when a list is chosen: if no other group serves the person, only `ACL NATIVE` changes the set back.
- **The set changes by a form**: every source (`CLUSTER RESET SOURCES IN GROUP g`), these sources (`SET SOURCES (…)`),
  or every source lazy (`SET SOURCES ALL LAZY`), with `LAZY [IDLE '<interval>']`. A source's own ATTACH takes the same
  marks in the profile's dialog. The console refuses what the grammar does (all without lazy, an idle time without
  lazy, a set of nothing, a cluster-wide set); the node judges the rest (an interval under a minute, a lazy source in a
  DEPENDS ON chain).
- **Nothing changes the set by itself** (the owner, 2026-11-20): a grant's answer names what it leaves out, and the
  section says what the roles read outside the set.
- Tests: `TestProfileStatement` (the new forms and refusals), `TestGroupSources`, `TestRepairGroupSource` (the
  browser names a source, never a statement; a repair that changed is refused; a listed source has nothing to
  repair), `TestGroupSourcesCheckFails`, `TestWriteAvoidsPartialNodes` (fails without the routing), the web console's
  `sources.test.tsx`.
- **Found by its review, fixed**: a list set locked the console's writes out on that group's nodes; a repair could run
  a text the person never saw; the check's 45 s plus the write's 15 s passed the server's 60 s; a failing check hid
  the set and its editor; the set offered another group's own sources and a source twice in another case; another
  group's source read as left out; the Repair buttons had one accessible name. Its re-review: a write cut by the
  deadline says it may have been written; a node that is down is said as down, not as the partial node's "run it
  elsewhere"; a repair whose text changed offers to read the check again.

#### Stage 2c: the grant editor (built 2026-11-21)

A catalog's page edits its grants, offered with `check_grant` among `console_info()`'s functions (who may grant may
check, duckdb-acl spec 122); the node judges every write. A row's role opens its grant, a button a new one.

- **Read first** (`GET policy/grant?level&role&catalog[&target]`): the grant as the node holds it now, from
  `platform.grants` / `schema_grants` (not an inherited row) / `object_grants`, and its version (a digest of it). The
  role, the catalog and the schema or object are taken from the node's lists (`roles` and the grants' roles,
  `catalogs`, `schemas`, `relations` + `functions`), in the node's spelling.
- **The statements** are the server's, from the form, shown before they run (`POST policy/grant/preview`):
  - a catalog grant: `GRANT CATALOG c TO ROLE r [WITH (…) | CAPS '{}'] [MAIN] [RLS '<filter>'] [VERSIONS …]`; one
    that exists changes property by property (`ALTER GRANT CATALOG … SET CAPS | MAIN | RLS`), keeping its object
    grants, and its versions are written with it;
  - a schema grant: `GRANT SCHEMA c.path TO ROLE r [caps] [INTO db.schema | VIRTUAL ONLY] [COMMENT '…']`;
  - an object grant, written whole (a GRANT replaces it): `GRANT OBJECT c.n TO ROLE r [caps] [RLS '<filter>'] [COLUMNS
    (a, b) | COLUMNS (* EXCLUDE (a, b))] [MASK ("col" AS <expression>, …)] [VERSIONS …]` (duckdb-acl spec 158);
  - a revoke: `REVOKE CATALOG | SCHEMA | OBJECT … FROM ROLE r`.

  Names are quoted identifiers; the row filter, the masks and the versions are literals (spec 180).
- **The write** (`POST policy/grant`) reads the grant again and composes the same statements only while its version is
  the form's (else 409: read it again; a new grant where one exists is the same), then runs them one at a time on one
  node that writes (stage 2b's routing), stopping at the first refusal. The answer is each statement's `(ok, message)` -
  a grant's group-source hint (hugr-node spec 035) among them - and the refused one with the node's reason.
- **Check with the node** (`POST policy/grant/check`): `platform.check_grant(role, catalog, object, rls, columns,
  caps :=, masks :=)`, every argument a literal - what the write would say, written nowhere.
- **What runs is what was shown**: the write carries the statements the preview showed, and runs only while the form
  composes exactly those (else 409); the form is locked while the node is asked.
- **The version check is best-effort**: read, compare, then write are separate calls, so two editors who both read
  the same version may both write it (the last wins). A compare-and-set needs the node's support (a follow-up).
- **A grant the node cannot read is only revoked**: an object grant whose `COLUMNS` or `MASK` the view answers NULL
  (the node could not read them) would show what they hide if written anew, so the editor offers its revoke alone. A
  catalog grant carrying an older build's column list keeps it: its versions are not changed here (written again,
  the list would go). An existing catalog grant's versions change by the grant written again
  (`GRANT CATALOG … VERSIONS …`, which keeps its object grants); its other properties by `ALTER GRANT`.
- **Nothing the person types is SQL of the statement** (duckdb-acl spec 180, the owner, 2026-11-21: duckdb's
  tokenizer, not a scanner of ours): a mask's items and a versions clause are written as literals (`MASK '<items>'`,
  `VERSIONS '<clause>'`), as the row filter is, and the node reads them with duckdb's parser. Until spec 180 they were
  placed in their clauses and kept there by a scanner of the console's own, which four review rounds found reading some
  texts otherwise than duckdb's tokenizer (`E'…'`, `$$…$$`, backtick names). It needs hugr-node v0.4.1 (duckdb-acl
  6ee182d), which the charts pin.
- **Names**: a role is one the policy names (created or granted, and mapped where the person reads the mappings - the
  policy bundle's view) - a role no list names yet is created first (stage 2e); a table under a live alias schema
  (`<alias>.<table>`) is an object, as the node takes it, the alias in the node's spelling. The
  physical schema of `INTO`, the column names of `COLUMNS` and a mask's column are quoted and the node checks them; a
  column whose name holds a comma is written from the SQL page.
- **Outcomes**: a statement that ran but answered no `(ok, message)` reads "ran", never done; one the node did not
  answer in time (or was not reached for) is "unknown" - it may have been written; a revoke of a catalog grant says
  that the role's object grants in the catalog go with it.
- The DBeaver client's object grant editor still writes the pre-158 column list: its fix is acl-clients' own.
- Tests: `TestGrantStatements` (each level, an existing catalog grant's ALTERs, the refusals), `TestGrantEditor` (the
  read with its version, preview, write and its hint, a stale form and a new grant over one refused, names the node
  does not list), `TestGrantRefusedHalfWay`, `TestGrantCheck` (a filter that would close the call stays a literal),
  the web console's `grants.test.tsx`.
- **Found by its review, fixed**: an unreadable grant widened by an edit; a preview the write was not bound to; an
  existing catalog grant's versions refused with advice that would drop its object grants; a table under a live alias
  and a mapped role not taken; a mask or a versions text that could leave its clause; a timeout shown as a refusal
  and a statement with no answer as not done; a revoke's button and its warning; a 409 with no way to read again. Its
  re-review: `E'…'` and `$$…$$` strings slipped past the clause check; the mappings read for every editor (refused
  below the policy bundle); an existing catalog grant's versions were not editable, and cleared ones wrote an empty
  clause; an alias's spelling was the person's; one 409 code for three cases. Its last pass: a backtick name and an
  `E'…'` after a digit slipped past the clause check.

## Security

- **Every read and write is the node's to judge**, under the person's own token. The console adds no right: it hides
  what `console_info()` says the person cannot read, and the node refuses the rest.
- **SQL from the browser exists only on the SQL page**, typed by the person and sent by the page's own DuckDB through
  the quack door, as any client's statement. An editor's expression fields (stage 2: a row filter, a mask, a versions
  clause) are SQL by nature: the server writes them as literals, and the node reads them with duckdb's parser under the
  person's token (duckdb-acl specs 148, 180). Everywhere else names are matched against the node's own
  lists before they are written, and a repair is the node's own text, read again where it runs.
- **NATIVE is offered to `passthrough` only**, the node's own rule for it; the page's offer is a convenience, the node
  refuses it to anyone else.
- **The browser is no boundary.** Nothing in the page enforces anything: a person who edits the page reaches the door
  as any quack client could, with their own token and their own rights.
- **The console's proxy adds nothing to a quack request.** It forwards the frames, logs no statement, and holds no
  session: the front and the node do what they do for any client.
- **The SQL page's answer is capped** in the page, so a large result does not fill the tab.

## Testing

- The console API's unit tests:
  - each page's reads, offered by `console_info()`;
  - the filters applied in the server;
  - a function's argument refused when the node's list lacks it;
  - the `/quack` proxy forwards to the front, adds nothing and is refused without a sign-in.
- The web console's unit tests: the SQL page's modes, NATIVE offered with `passthrough` only, the cap.
- The browser e2e against the stand's node (its quack door open):
  - an admin browses a catalog, a role's effective rights and an issuer;
  - an admin runs a query in both modes through the page's DuckDB;
  - `ops` is offered none of the policy pages.
- The demo: one check reads a catalog's grants and a role's effective rights through the console, and one runs a
  statement through the console's `/quack` with an analyst's token.

## Decisions (owner, 2026-10-22)

1. **Stage 1's scope:** the read pages and the SQL page. The editors, the data grid and maybe view-as go in stage 2.
2. **The SQL page:** both modes, NATIVE for `passthrough` only, 1000 rows / 4 MiB.
3. **View-as** is not in stage 1: maybe stage 2, maybe the DBeaver client's alone.
4. **The SQL page runs in the browser:** duckdb-wasm through the quack door (acl is not in wasm), which also tries the
   embedding. Stage 1's sign-in is the console's own token in a quack secret ("natively", the owner). tresor in the
   browser is a general scenario, relayed to tresor (2026-10-22): a user of any client (the DBeaver client, the CLI,
   duckdb-wasm embedded in any site that signs its users in by OIDC) reaches their tresor secrets; in an embedding,
   with no second sign-in, by an exchange of the site's token. What tresor lacks today is listed under Follow-ups.

5. **duckdb-wasm on v2.0.0** (in its main since 2026-10-21, not yet on npm): built in our CI from a pinned commit of
   their main and served by the console, until their npm package is published (proposed, the owner started the work
   on it).
6. **A quack door for the SQL page:** the page needs a group with `doors.quack` open, and says so when the person's
   groups have none. The demo opens it on group a.

## Alternatives considered

- **A SQL page in the console's server** (Flight from the operator, as the other reads). Smaller, but it embeds
  nothing; the owner prefers the browser's DuckDB, which tries the embedding the later pages build on.
- **The browser straight to the front with CORS.** It needs a certificate the browser trusts on the front and its
  address public; the console's own origin needs neither.
- **Reading views filtered on the node.** It needs browser values in SQL; the views are policy-sized, so filtering in
  the server costs little.

## Follow-ups

- Stage 2: the editors (2c, 2e-2g) and the data grid (2d).
- tresor in the browser (relayed to tresor 2026-10-22, verified in its code):
  - its wasm build still links OpenSSL and sockets (`tresor_sources.cmake`), and wasm is out of its distribution;
  - `FLOW 'token'` cannot renew or swap the token in an attached session;
  - its helper `attachTresor` (its spec 020) signs the user in again with its own client;
  - tresor-server's discovery names its in-cluster `public_url`, which a browser behind the console's mount cannot
    reach.

  Ours, when it comes: the mount of `/.well-known/duckdb-secrets` and the CSP.

  tresor's plan, the owner's approval of 2026-10-22:
  - Step 1: tresor builds for wasm without OpenSSL, sockets or threads, and is back in its CI and distribution
    (duckdb-ext-common v0.15.0).
  - Step 2: no second sign-in, and no exchange on the server.
    - The page hands tresor the access token for tresor's audience, as a parameter (`tresor_web_token(service, ?)`),
      and a new one at every renewal of its own.
    - tresor holds that token only, never the site's refresh token.
    - Its helper `attachTresor` takes `{token, onTokenChange}` and signs nobody in.
  - What the platform then gives:
    - tresor's audience in the console's token;
    - tresor-server's `public_url` = `https://<console>/tresor`;
    - the mount of `/tresor/.well-known/duckdb-secrets` and `/tresor/v1/`;
    - the CSP.
- A DDL / export tab, when duckdb-acl 120.4 lands (the DBeaver client waits for it too).
