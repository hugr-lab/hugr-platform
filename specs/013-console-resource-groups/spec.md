# Spec 013: resource groups in the console

- **Status**: implemented
- **Date**: 2026-11-26

## Summary

The console edits a resource group whole: its limits, its comment, whether it is the policy's default group, and the
roles bound to it. It can also make and drop a resource group that no group of nodes serves. Until now it could only
create one with no limits and bind roles while adding a group of nodes to the policy (spec 006).

## Problem

- **The console is where the cluster is run** (the owner, 2026-11-26): its first job is what a SQL client does not do
  for the operator. A resource group is half of a group of nodes (which principals it serves, and with what limits),
  so it belongs with the groups.
- **Spec 006 stopped at creation.** "Add to the policy" made the resource group with no limits. Its limits, its default
  mark, its comment and any later binding were left to SQL ("set with the policy"), and a group that serves no nodes
  (limits for some roles only) could not be made here at all.

## Design

### The editor

The page Policy → Resource groups offers **New resource group** and opens a group from its row. A group of nodes' page
offers **Limits and roles** on its resource group once it is in the policy. Both open one editor with these parts:
- **the six limits** (`window_start`, `window_max`, `batch_bytes`, `max_result_rows`, `queue_priority`,
  `max_sessions`), whole numbers. Empty means the node's setting. Only `queue_priority` may be negative;
- **the comment**;
- **the default mark.** The editor says which group holds it now, since the node refuses a second;
- **the roles bound to the group**, from the policy's roles;
- **the groups of nodes that serve it**, read from Kubernetes and shown.

It works like the grant editor (spec 007 stage 2c):
1. it reads the group first, with its version (a digest of what it read);
2. the server composes the statements, and the editor shows them;
3. the write runs only the statements shown, one at a time on one node that writes (stage 2b's routing), and stops at
   the first refusal;
4. it runs only while the group is as it was read; otherwise the answer is 409, read it again.

### The statements

| edit | statements |
| --- | --- |
| a new group | `CREATE RESOURCE GROUP "g" [(limits)] [DEFAULT] [COMMENT '…']`, then `GRANT RESOURCE GROUP "g" TO ROLE "r"` per role |
| its limits or its comment changed | `CREATE OR REPLACE RESOURCE GROUP "g" [(limits)] [COMMENT '…']` |
| the mark | `ALTER RESOURCE GROUP "g" SET DEFAULT` / `DROP DEFAULT` |
| a role added / removed | `GRANT RESOURCE GROUP "g" TO ROLE "r"` / `REVOKE … FROM ROLE "r"` |
| dropped | `DROP RESOURCE GROUP "g"` |

- **Limits and comment are written whole.** `ALTER … SET (…)` changes only the limits it names and cannot clear one
  back to the node's setting. A re-create keeps the group's default mark and its bindings, and it writes the limits
  and the comment it names, clearing the rest. This was checked against duckdb-acl 6ee182d on 2026-11-26.
- **Names.** A role must be one the policy lists (`platform.roles`). A group is taken in the node's spelling. Every
  name is a quoted identifier. A new group's name is a plain name (letters, digits, `_`, `-`, at most 63), because a
  group of nodes may serve it later, and every node's bootstrap names its group's resource group (spec 006).
- **A drop is refused** while a group of nodes of the platform serves the group (409 `served`): its nodes would serve
  nobody and drain. It is also refused for the default group. "Remove from the policy" on the group's own page stays
  the way to take a served group out (spec 006).
- **A second default is refused before anything is written** (409 `default_held`, naming the current one): the node
  would refuse the mark after the limits were already written.
- **Limits travel as text** (whole numbers as strings): a BIGINT past 2^53 would round in the browser and be written
  back changed.
- **The comment is checked only where a statement writes it**, and compared as stored. A comment SQL wrote (two lines,
  an edge space) neither blocks a role's edit nor is rewritten by one; a limit's edit over it says so.
- **The API:**
  - `GET policy/resource-group?name=` answers the group with its version, the policy's roles, the current default
    group and the groups of nodes serving it;
  - `POST policy/resource-group/preview` and `POST policy/resource-group` each take `{form, version[, statements]}`.

  Each needs the policy bundle (the console's `policy` right), and the node judges every write: an unrestricted
  `manage`.

- **The person's own reads obey the limits.** The console reads as the person, and on a node of a group only that
  group's limits apply (duckdb-acl spec 096). A `max_result_rows` below a policy view's rows on a group the person is
  in therefore refuses the console's own reads on that group's nodes; the console then works through another group's
  node. The editor says so; the demo's check 16 met it (the admin is in every group).

Its review found the second default, the comment, the BIGINT limits and an unavailable platform answered as the
edit's refusal (now 503): all fixed as above.

## Security

- **Every write is the node's to judge**, under the person's token. The console adds no right.
- **No text from the browser becomes SQL** but the comment, which is a literal. Limits are numbers the server
  formats, and names are the node's, quoted.
- **The version check is best-effort**, as the grant editor's is: two editors that read the same version may both
  write, and the last one wins.

## Testing

- `TestResourceGroupStatements`: each row of the table above, plus the refusals: a negative limit, a role the policy
  lacks, no name, a comment with a line break, dropping the default group or a group that does not exist, an unknown op.
- `TestResourceGroupEditor`: the read with its version and serving groups; preview and write; an unshown, stale,
  existing or served edit refused with nothing written; a dotted new name; the cluster bundle refused (403).
- The web console's `resourcegroups.test.tsx`: the form's checks, a new group written as shown, an edit sending a
  cleared limit as none, and a drop offered only when no group of nodes serves the group.
- The demo's check 16 (spec 004) runs it on a cluster.

## Alternatives considered

- **`ALTER … SET (limits)` for a change.** It is smaller, but it cannot clear a limit, so a form showing an empty limit
  would write something other than what it shows.
- **Limits on the group of nodes' resource (CRD).** A resource group is the policy's (duckdb-acl spec 085/096), and
  it serves roles without nodes too. Two places to keep it would drift.

## Follow-ups

- duckdb-acl: `CREATE RESOURCE GROUP IF NOT EXISTS` (spec 006). The editor creates only after reading that no group of
  that name exists, but in that window a plain CREATE would replace one made meanwhile.
