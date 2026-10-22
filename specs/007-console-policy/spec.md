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
| Grants of a catalog | `grants`, `schema_grants`, `object_grants`, `grant_columns` | the grant nodes |
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
  bounds a listing by `acl_metadata_max_rows`, and its refusal is shown as it says.

### The SQL page (stage 1): DuckDB in the browser, the node through quack

The page embeds duckdb-wasm, which attaches the platform's quack door as a client does and runs the person's statement
there. acl is not in the browser (its wasm build is out by the owner's decision of 2026-09-14: a tab is no boundary).
The node behind the door enforces the policy, as it does for any quack client.

- **The path:** duckdb-wasm runs the quack client from `extensions.duckdb.org/v2.0.0/wasm_eh/quack`, DuckDB-signed. A
  quack URI has no path, so the client posts to `https://<host>/quack`. The console serves `POST /quack` (and
  `GET /.well-known/quack-auth`) on its own origin and proxies them to the front's quack door, which seats the person on
  a node of their group (spec 002). The browser reaches nothing but the console: the nodes and the front stay
  in-cluster, their certificates the internal CA's, and the page needs no CORS.
- **The sign-in is the console's:** the person's token (the nodes' audience) goes into a quack secret in the page's
  DuckDB memory, and the door verifies it as any client's (duckdb-acl spec 041). Later: tresor's own flows, when they
  come to the browser.
- **Two modes:**
  - "As you": the statement as written, under the person's principal. It sees the virtual catalog and may use the
    management grammar the person's bundles allow.
  - "NATIVE": offered only with `passthrough`. The page writes `ACL NATIVE ` in front of the statement, the break-glass
    door, where nothing is refused (the owner, 2026-10-15). The page says so above the box.
- **The answer is capped** at 1000 rows and 4 MiB: the page stops reading the stream there, cancels the rest and says
  how much it cut.
- **The text is the person's.** The node judges it as any client's. The console's proxy sees only quack's frames,
  logs a request's size and the person's subject, never a statement.
- **What it needs:**
  - a duckdb-wasm built on DuckDB v2.0.0 (its main since 2026-10-21; its npm package is not yet on 2.0);
  - a group with its quack door open (`doors.quack`), since the front seats a quack client only on such a node;
  - the console's CSP allowing wasm (`'wasm-unsafe-eval'`) and a worker of its own origin.

The same embedding is what the second stage's grids read from.

### Stage 2: the editors, the data, view-as (after the release unless time allows)

- **Data shown as the DBeaver client shows it:** a plain grid of a result, nothing more.
- **View-as:** maybe, and maybe the DBeaver client's alone (the owner, 2026-10-22).
- **The DBeaver client's editors,** in the same order spec 008 built them:

    1. The grant editor: catalog / schema / object grants with the row filter, the columns tree (visible / masked /
       hidden) and "Check with the node" (`check_grant`), saved as `GRANT` / `ALTER GRANT` statements.
    2. Roles, resource groups, role mappings.
    3. Catalog objects (tables, views, schemas, functions, references) and repair from `check_catalog`.
    4. Issuers, clients, function categories and admin bundles.

As in the DBeaver client, an edit composes the ACL grammar, shows the statements before it runs them, runs them one at
a time on one node and stops at the first refusal, and re-reads the object first to catch a stale form.

## Security

- **Every read and write is the node's to judge**, under the person's own token. The console adds no right: it hides
  what `console_info()` says the person cannot read, and the node refuses the rest.
- **SQL from the browser exists only on the SQL page**, typed by the person and sent by the page's own DuckDB through
  the quack door, as any client's statement. Everywhere else names are matched against the node's own lists before they
  are written.
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
   embedding. The sign-in is the console's token for now, tresor's when its flows reach the browser.

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

- Stage 2: the editors, the data grid, maybe view-as.
- tresor's flows in the browser, for the SQL page's sign-in.
- A DDL / export tab, when duckdb-acl 120.4 lands (the DBeaver client waits for it too).
