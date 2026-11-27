# Spec 015: a node's boot scripts in the console, and a failing script in the group's status

- **Status**: accepted (the owner, 2026-11-27)
- **Date**: 2026-11-27

## Summary

hugr-node spec 047 (stage 1, in hugr-node v0.4.2) lets the administrator add SQL to a node's boot. The scripts are
profile items: `CLUSTER ADD | ALTER | DROP SCRIPT`, in one of three places (before the sources, after one source, after
the sources), each with its policy on error. This spec is 047's stage 4, hugr-platform's part:
- the console writes scripts from the profile's form, as it writes the profile's other items;
- the group's page shows the boot in order, as a node of the group walks it;
- a script that fails on a node shows in the group's status and on the group's page.

## Problem

- **The form has no scripts.** The profile's form (spec 010 stage 2) writes extensions, sources and settings. A script
  takes `ACL CLUSTER ADD SCRIPT …` written by hand on a node.
- **The order is not shown.** Where a script runs depends on its place, its source and its name. hugr-node lists the
  boot in one place, `platform.bootstrap_order()`, and the console does not read it.
- **A failing script is not seen.** A node reports what fails now in its heartbeat: a source a script holds detached,
  an `AFTER SOURCES` script awaiting its retry. Those are pending items of class `error`, and the operator reads only
  `needs_restart` from the heartbeat. The group's status says nothing, and the node still serves.

## Design

### The form

Three more actions of `POST cluster/profile`, built into one statement as the others are:

```
CLUSTER ADD SCRIPT <name> <place> AS '<sql>' [ON ERROR <policy>] [IN GROUP "<g>"] [COMMENT '<text>']
CLUSTER ALTER SCRIPT <name> [<place>] [AS '<sql>'] [ON ERROR <policy>] [IN GROUP "<g>"]
CLUSTER DROP SCRIPT <name> [IN GROUP "<g>"]

<place>  := BEFORE SOURCES | AFTER SOURCE "<alias>" | AFTER SOURCES
<policy> := FAIL NODE | CONTINUE | DETACH SOURCE | RETRY
```

- **Fields**:
  - `script`: the name. Letters, digits and `_`, at most 63, and it may start with a digit (`10_vars`), as hugr-node
    takes it bare.
  - `place`: `before_sources`, `after_source` or `after_sources`.
  - `source`: the alias, for `after_source`.
  - `sql`: the text.
  - `onError`: `fail_node`, `continue`, `detach_source` or `retry`.
  - `comment`, `group`: as for the other actions.
- **The SQL is a literal**, never read by the console. It is the only field of the form that may span lines.
  - It is quoted by doubling its quotes.
  - At most 64 KiB.
  - No control character but a line break and a tab.
  - The node stores it and runs it as written (047: no lint, no acl check).
- **The policies are offered by the place** (047's table):

  | place | policies | default |
  | --- | --- | --- |
  | before the sources | fail node, continue | fail node |
  | after a source | detach source, retry, continue, fail node | detach source |
  | after the sources | fail node, retry, continue | fail node |

  The node judges it again and refuses another.
- **ALTER writes only what the form gives**: the node keeps the rest. **DROP** takes the name and the group.
- **The answer** is the node's, as for any change. A script is a restart item, so the dialog says the nodes take it at
  a restart, and restarting the group from the fleet rolls them (the operator does not roll a restart item by itself).
- **The request** of a script is up to 192 KiB of JSON (`decodeStrictUpTo`): 64 KiB of SQL, which JSON writes longer.
- **Rights**: the route's (`Rights.Infrastructure`, the `cluster` bundle). The node decides by the person's token.

### The boot in order

- The group's Bootstrap section reads `platform.bootstrap_order()` on a node of the group, beside the effective
  profile it reads there: the same request (`GET nodes/{label}/inventory`, its `order` part), asked only when the node's
  `console_info` names the function (the `cluster` bundle's; a node of an older build names none).
- It shows `(pos, step, sql)` as the node lists them. A script's text is shown in full, folded past a few lines. A lazy
  source's scripts are marked as the node marks them ("at its attach").
- The listing is the profile as it is now. A node runs what it held at its boot, so the section says so when the group
  lists nodes in `needsRestart`.
- The effective profile's table gets an edit and a drop action on a script's row, which open the form filled in.

### A failing script in the status

- **The heartbeat**: `nodeproto.Summary` gets `Failing []Item` from the report's pending items of class `error`
  (kind, name and the node's reason). It is one more annotation of the node's Lease, as the summary's other fields are: a JSON list, capped at 20 items.
- **The group's status**: `status.profile.failing` lists `{node, kind, name, reason}`.
  - The reason is the node's own text, which is the script's name and the error's class: `script 20_after failed
    (Catalog)`. The node never puts the script's text or the error's message there (047).
  - The list is sorted by node, then name, and capped at 20 entries, with a count of the rest.
- **A condition** `ProfileHeld` is true while any node lists one. It is not `Ready=false`: the node serves, by the
  policy the administrator chose.
- **The console**: the group's page shows the failing items above its nodes, and the fleet marks the group "Profile
  held" (before a restart pending). A failing item does not count toward `needsRestart`: a restart does not mend it.
- **It clears itself**: when the next heartbeat no longer lists the item (the retry passed, the source or the script
  changed, or the node restarted clean).

## Security

- **No credential**: a script names a secret in tresor if it needs one. The form takes no field for one. The node lints
  nothing in a script's text, so the dialog says so: a script is stored as written, and anyone with the cluster bundle
  reads it.
- **The text never reaches Kubernetes**: the status carries only the node's reason (name and class). Neither the
  console's logs nor the operator's carry a script's text.
- **Who writes and who reads**: only the `cluster` bundle, by the person's token on a node. The console builds the
  statement and decides nothing. A person without the right gets the route's 403.

## Testing

- **Go**:
  - `TestProfileStatement`: the three actions, the SQL's quoting (a quote, a line break, a tab), the refusals (a
    control character, past 64 KiB, a name that is not one, `after_source` with no source);
  - the heartbeat's `Failing` from pending items (only class `error`);
  - the fleet's `status.profile.failing`, its order and cap, and the `ProfileHeld` condition.
- **vitest**:
  - the form's fields per action, and the policies offered per place;
  - the bootstrap order's rows;
  - the failing items on the group's page.
- **Demo check 20** (group b):
  1. The console adds an `AFTER SOURCES` script with `ON ERROR RETRY` that fails (it reads a table no source has).
  2. The answer says restart, and the group lists its nodes in `needsRestart`.
  3. The bootstrap order lists the script with its SQL as written (over two lines), after every source and before the
     node serves.
  4. The console restarts group b. Its new node serves, and the group's status lists the script as failing, with
     `ProfileHeld` true.
  5. The console drops the script. The failing item clears at the node's next try (its backoff, at most 5 minutes),
     and `ProfileHeld` turns false.

## Alternatives considered

- **The error's message in the status**: a source's message may carry its connection string, and the node never sends
  it (047). The node's log has the class too. The person reads the error by running the script's text by hand.
- **A failing script as `Ready=false`**: the administrator chose the policy that lets the node serve. A condition of
  its own says it without stopping the front.

## Follow-ups

- Session scripts (duckdb-acl spec 181) in the console's policy pages: the policy bundle's, on a resource group and a
  role.
