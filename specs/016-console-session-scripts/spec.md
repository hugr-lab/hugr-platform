# Spec 016: a session's scripts in the console

- **Status**: accepted (2026-11-27)
- **Date**: 2026-11-27

## Summary

duckdb-acl spec 181 (in hugr-node v0.4.2, the charts' node) lets the policy hold SQL that a node runs when a client's
session opens. The node runs it on that session's own connection, as itself, before the session's first statement. A
script sits at one of three levels, and every level that has one runs, from the general to the specific:
1. a resource group's;
2. a role's;
3. a role's grant of a resource group.

This spec lets the console write and drop those scripts where the levels already are: the resource groups page and a
role's page. A role's page also shows what a session of that role runs on a group's nodes, in the node's order.

## Problem

- **The console cannot write them.** A script takes `ALTER RESOURCE GROUP … SET SESSION SCRIPT '…'` (or `ALTER ROLE`)
  written by hand in SQL, by a holder of the `policy` bundle.
- **The order is not shown.** Which scripts a session runs depends on its roles and on the node's group. The node lists
  them in order in `platform.session_scripts(role, group)`, and the console does not call it.
- **The script text shows raw.** The resource groups and roles pages read their views whole (`SELECT *`), and since
  spec 181 those views carry `session_script`, which may be many lines long.

## Design

### Where

- **Policy → Resource groups**:
  - the resource groups table shows each group's script folded (its first line, with "(n lines)"), and its policy on
    error;
  - an action on the row ("Session script of g") opens the editor for the group's script;
  - the table "Roles in them" does the same for each binding: the grant's script of role r in group g.
- **Policy → Roles → a role**:
  - a "Session script" section shows the role's own script, with its action;
  - the table "Resource groups" shows the grant's script per group, with its action;
  - a section "What a session runs" calls `platform.session_scripts(role, group)` for a group picked from the groups a
    session of that role is served by: the groups the role is bound to, or the default group when it is bound to none.
    It lists `pos, level, group, role, on_error, sql` as the node answers them, a long script folded.

  The console always names the group. Without one, the node would take the group of the node that answers, and that
  is whichever node the console reached.

The editor and the actions are offered only with the console's `policy` right (the `policy` or `passthrough` bundle).
The order is offered only when `console_info` names `session_scripts`.

### The editor

It is one small dialog for all three levels, built the way the resource group editor is (spec 013):
1. it reads the script first, with a version (a digest of the script and its policy as read; "" = none);
2. the server composes the statement, and the dialog shows it;
3. the write runs only the statement shown, on one node that writes, and only while the script is as it was read;
   otherwise the answer is 409, read it again.

**Its fields:**
- **the SQL**: a textarea, the same as the boot script's (spec 015);
- **on error**: "Refuse the session" (`FAIL SESSION`, the node's default) or "Log and serve" (`CONTINUE`);
- **a Drop button**, when a script is set.

**The dialog says plainly:**
- the script runs as the node, with full rights, on the connection of every session at that level;
- it is stored as written, and nothing checks it;
- every holder of the `policy` bundle reads it;
- a credential belongs in tresor, named from the script, never in its text.

**The statements**, one per edit:

| level | set | drop |
| --- | --- | --- |
| a resource group's | `ALTER RESOURCE GROUP "g" SET SESSION SCRIPT '<sql>' ON ERROR <p>` | `ALTER RESOURCE GROUP "g" DROP SESSION SCRIPT` |
| a role's | `ALTER ROLE "r" SET SESSION SCRIPT '<sql>' ON ERROR <p>` | `ALTER ROLE "r" DROP SESSION SCRIPT` |
| a grant's | `ALTER RESOURCE GROUP "g" FOR ROLE "r" SET SESSION SCRIPT '<sql>' ON ERROR <p>` | `ALTER RESOURCE GROUP "g" FOR ROLE "r" DROP SESSION SCRIPT` |

- **`ON ERROR` is always written**, `FAIL SESSION` or `CONTINUE`. A SET writes both the script and its policy.
- **The SQL is one literal**, built by the boot scripts' `scriptLiteral` (spec 015):
  - the quotes doubled, nothing else changed;
  - not blank;
  - at most 64 KiB;
  - valid UTF-8;
  - no control character but a line break, a carriage return and a tab.

  The console never reads the text beyond these checks.
- **Names**: a group must be one `platform.resource_groups` lists, and a role one `platform.roles` lists. A grant's
  binding must be one `platform.role_resource_groups` lists. Each is written in the node's spelling, as a quoted
  identifier.
- **Refused before writing**: a binding that does not exist, and a drop of a script that is not set. The node refuses
  both too.
- **What survives an edit elsewhere**: a script stays through a re-created group (spec 013's `CREATE OR REPLACE`) and
  a re-granted role (duckdb-acl spec 181). A revoke or a drop deletes the scripts that hang on it. The resource group
  editor (spec 013) therefore says, before it revokes a role, when that binding has a script.

**The API** (the console's `policy` right; the node judges every write: the `policy` bundle alone, by the person's
token):
- `GET policy/session-script?group=&role=` (either one, or both for a grant) answers:
  - `{exists, sql, onError, version}`;
  - for a grant, whether the binding exists.
- `POST policy/session-script/preview` and `POST policy/session-script` each take `{form: {group, role, sql, onError,
  op}, version[, statements]}`. The op is `set` or `drop`, and the body is read up to 192 KiB (`decodeStrictUpTo`, as in
  spec 015).

**The trial** (the owner, 2026-11-27: "check it safely by running it - a session opened with the script not yet
saved"):
- A **Try** button runs `platform.try_session_script` (duckdb-acl spec 183, then a hugr-node release). The node runs
  what a session would run when it opens, with the form's script in place of the stored one:
  - the `acl_session_*` variables;
  - every layer in its order, each as a session runs it.

  It runs as the node, on a connection made for the trial and dropped after, and nothing is stored. It answers each
  script that ran (ok, the error's class and message, its line and column in that script), then whether the session
  would be served.
- **The session tried** depends on the script's level:
  - a role's script: the role, on a group the person picks from the role's groups (or the default group);
  - a group's script: that group, with a role the person picks from the roles bound to it, or none;
  - a grant's script: its role and its group.
- **The trial is a click, never implicit.** A preview runs nothing. It is a run as the node: what a script does beyond
  its own connection it does once, as each session would. That covers a write to a source, `SET GLOBAL`, `ATTACH`,
  `INSTALL` / `LOAD`, `COPY … TO` and an HTTP call. The button says so.
- **The write is not blocked by a trial.** The person decides. When the last trial of the text as it stands refused
  the session, the dialog says so beside Write.
- The dialog shows the rows under the field. The edited script's error names its line and column, and an earlier
  layer's error names the layer it came from.
- **The API**: `POST policy/session-script/try` takes `{form, session: {role, group}}`. Its names are checked against
  the node's lists, as the read functions' are. It has the slow reads' deadline (45 s), because a script may wait on a
  remote source. When the console's request ends, the node's trial is interrupted.

The field is the boot scripts' plain textarea (the owner, 2026-11-27: minimal for now). The SQL editor comes later,
together with the SQL page: see Follow-ups.

**The answer** is the node's `(ok, message)`. A session already open keeps what it ran, so the dialog says the change
applies to sessions opened after it.

### The order

`policyFunctions` gains `session_scripts` with the arguments `role` and `group`:
- `role` comes from the roles list (as `effective_rights` takes it);
- `group` comes from a new list, `SELECT "group" AS name FROM platform.resource_groups`. It is compared exactly, as
  the node keeps a group's name.

## Security

- **A session script is the node's power on a principal's connection** (spec 181). The console gives no one more than
  they hold. The write is the node's to judge, under the person's token, and only the `policy` bundle passes. The
  console's `policy` right only decides what it offers.
- **No credential**: the form has no field for one, and the dialog says where one goes.
- **The text is never logged.** The console's log names the level and the owner of a script it failed to write (the
  group, the role), never the SQL. The SQL travels only in the person's own request and the node's answer.
- **Who reads the text**: only the `policy` bundle. A reader with only a point grant on the view gets NULL from the
  node, and the console shows no script.
- **No text from the browser becomes SQL** but the script, which is a literal. Names are the node's, quoted.

## Testing

- **Go**:
  - `TestSessionScriptStatements`:
    - each cell of the table;
    - the literal: a quote, a line break, a tab;
    - the refusals: a control character, past 64 KiB, blank, an unknown op, no level, a drop of none.
  - `TestSessionScriptEditor`:
    - the read and its version;
    - preview and write;
    - a binding that does not exist;
    - the trial: the form's script and the session sent as names the node listed, the node's rows answered, an
      unlisted role or group refused;
    - an unshown or stale edit refused with nothing written;
    - the cluster bundle refused (403).
  - `TestPolicyCall`: `session_scripts` with a group that is not listed is refused.
- **vitest**:
  - the dialog's fields, each level's statement shown and sent;
  - the folded script in the resource groups table;
  - the order section with its group picker;
  - the resource group editor's warning before it revokes a binding that has a script.
- **Demo check 21** (group a, `analyst_a` on a node of a). A script sets what the session's policy reads, variables;
  the session reads them back with `getvariable` (granted to `analyst_a` by the demo's seed). The zone a client renders
  in is the client's own (the owner, 2026-11-28), never a script's example.
  1. **Tried first.** A trial of a text that does not parse (`SET VARIABLE y =` on its second line) answers the
     parser's error at line 2 and says the session would be refused. Nothing is written. A trial of the role's script,
     over two lines (`SET VARIABLE demo_region = 'north';` and `SET VARIABLE demo_layer = 'role'`), runs.
  2. The console writes that script. The next session of `analyst_a` reads `role`, `north`. The client sets its own
     zone on that session (Flight's `SetSessionOptions`, duckdb-acl spec 068) and renders
     `2026-01-01 00:00:00+00` as `2026-01-01 09:00:00+09`: a platform node's locked configuration leaves it that
     (hugr-node spec 048).
  3. The console sets the grant's script (a, `analyst_a`) to `SET VARIABLE demo_layer = 'grant'`. It runs after the
     role's, so the next session reads `grant`, `north`. `session_scripts(analyst_a, a)` lists the role's script, then
     the grant's.
  4. The console sets the grant's script to read a table that does not exist, with `FAIL SESSION`. The next session of
     `analyst_a` is refused, and the refusal does not carry the script's text. With `CONTINUE`, the session is served
     and reads `role`, `north` again: the role's script stands.
  5. The console drops both scripts. A session reads neither variable, and `session_scripts` lists nothing.

  The scripts stay on the role and the grant, not on group a: the admin is in every group, and the console reads as the
  admin on a's nodes too (spec 013's note).

## Alternatives considered

- **The scripts inside the resource group editor (spec 013).** That editor writes a group whole and versions it whole.
  A script there would tie a limit's edit to a script's text. One dialog per script keeps both small.
- **The order without a group.** It is simpler, but the answer would depend on which node the console reached.
- **A check by parse, then `PREPARE`** (the first draft of the trial). The owner rejected it. A statement bound alone
  misjudges what an earlier one makes, and binding misses what fails only when it runs. A run of the session is the
  check.

## Follow-ups

- The same on a group of nodes' page, beside "Limits and roles", if the owner wants it there too.
- **A node check mode for the boot profile** (the owner, 2026-11-27; a spec of its own, hugr-platform 017). It tries a
  boot script before it is saved, as a session script is tried here:
  1. the console makes a resource group and a group of nodes with a random name, both temporary;
  2. it copies the group's profile items into it (`IN GROUP "<trial>"`), plus the unsaved change;
  3. it brings up one node of it;
  4. it shows how the node booted: the boot in order, the failing items, whether the node serves;
  5. it deletes all of it.
- **The console's SQL editor**, together with the SQL page's console to the node (duckdb-wasm, spec 007). Deferred by
  the owner, 2026-11-27, to a spec of its own. A first sketch:
  - one component for every SQL field of the console: session and boot scripts, the SQL page, and in one-line mode the
    grant editor's RLS and masks;
  - the surface is CodeMirror 6 (MIT, modular, small enough for a dialog), not Monaco (VS Code's editor: an IDE's
    weight for a field);
  - the colours come from DuckDB's own tokenizer, as in the DuckDB CLI. The CLI uses the PEG tokenizer
    (`HighlightTokenizerBehavior`), which also marks an unterminated string or comment and an unmatched bracket as
    errors while the text is typed. The duckdb-wasm the console ships has only the older `tokenize(text)`, with no
    error token, so the PEG tokenizer is to be exposed in the console's own wasm build. No SQL grammar of the editor's
    own. The wasm loads at the field's first use, and until then the text is plain;
  - the errors are the trial's (`try_session_script`), marked at their line and column;
  - completion later, from DuckDB's `autocomplete` extension (`sql_auto_complete`), either built for the console's
    wasm as quack is, or asked of the node. Which one decides what it knows: keywords and functions only, or the
    catalog as the person reads it.
