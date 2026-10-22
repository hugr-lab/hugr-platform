# Spec 006: the console manages the cluster - its groups, and a group's place in the policy

- **Status**: accepted (2026-10-22)
- **Date**: 2026-10-22

## Summary

The console (spec 003) shows the fleet and scales a group. The owner's direction (2026-10-20): it is the place the
cluster is operated from. So it also creates a group, changes it, and deletes it. Adding a group to the policy is a
second step, taken by hand from the console: the group's resource group, and the roles it serves. Every write is
judged where it lands: a Kubernetes write by the operator's webhook and the person's bundle, a policy write by the
node, under the person's token.

## Problem

- **A group is made only by `kubectl` or the chart's install hook today.** The console scales and restarts one, and
  that is all.
- **A new group serves nobody until the policy knows its resource group.** Its nodes report `group_known: false` and
  drain themselves (duckdb-acl spec 096). The fix is a management statement on a node, which nobody does from the
  console.
- **A group needs more than its object.** Before its nodes can work they need:
  - an identity in the IdP, which is their login to tresor;
  - in tresor, the catalog secret's grant, and a place among the actors that act for sessions;
  - room in the plan;
  - a doors' certificate.

  The console has to say what it does and what is the installation's.

## Design

### The operations (the operator's console API, the `cluster` bundle as today's scale)

Built: `internal/console/group_api.go` (create, one field per PATCH, delete), `policy.go` (the policy step),
`web/console/src/screens/GroupActions.tsx`.

| method, path | does |
| --- | --- |
| `POST groups` `{name, size, replicas, identity: {clientID}, doors: {quack}, drainSeconds, resourceGroup}` | creates the `HugrNodeGroup` (SSA, the console's field owner, `platform.hugr.io/changed-by`) |
| `PATCH groups/{g}` `{replicas}` / `{size}` / `{doors}` / `{drainSeconds}` / `{restart: true}` | changes it (today: `replicas` and `restart` only): a new size or doors roll the group pod by pod, each pod drained (the Deployment's own roll) |
| `DELETE groups/{g}` | deletes the group: every pod drains at its SIGTERM up to `drainSeconds`, then goes. The policy's resource group stays |

- **The webhook judges** a console write as it judges a `kubectl` one: the plan, the sizes, the name. Its refusal is
  shown with its reason.
- **The certificate** is the chart's groups' own: the csi-driver from the internal issuer (`groups.yaml`), its issuer
  and duration handed to the console by the chart (`--console-group-issuer`, `--console-group-certificate-duration`).
  Nothing is asked; without an issuer the console makes no group.
- **Each field has its own server-side apply owner** (`hugr-console`, `-size`, `-doors`, `-drain`, `-restart`), so an
  apply of one field never drops another. A group is made by a create (`hugr-console-create`), not an apply, so a later
  apply of one field takes that field over and leaves the rest.
- **The identity** is given, never made. The person types the client id of an identity that already exists, the IdP
  administrator's or the recipe's. The form shows the subject that identity must trust,
  `system:serviceaccount:<ns>:hugr-node-<group>`, for the person who federates it. The console holds no right in the
  IdP.

### A group in the policy (the second step, by hand)

On a group whose nodes report the resource group unknown, the group's page offers **Add to the policy**. It runs, on a
serving node and under the person's token:
- `CREATE RESOURCE GROUP <rg> (<limits>) [DEFAULT]`;
- `GRANT RESOURCE GROUP <rg> TO ROLE <r>` for each role chosen.

These are the same management statements the DBeaver client sends. The node judges them: an unrestricted `manage`, the
`policy` bundle. The form lists the policy's roles (`platform.roles`) and its resource groups.

- **No name from the browser becomes SQL as typed.** The console reads the roles on the node first and binds only roles
  in that list. Every name is written as a double-quoted identifier.
- **The resource group is created with no limits.** Its limits and the default flag of an existing group are set with
  the policy (spec 007).
- **The statements go to a serving node first.** The group's own nodes drain themselves, so they are asked last, and
  a placement refusal sends the call to one of the groups it names.
- **A repeat finishes a cut call.** A known resource group is never created again; binding a role again replaces the
  binding.

**Remove from the policy** (`DROP RESOURCE GROUP`) is a separate action, never part of a delete: the nodes of a group
whose resource group is gone drain themselves.

### What tresor needs for a new group

A node logs in to tresor by its group's identity. Two things must be there for it to work:
1. **The catalog's secret is granted by role** (`hugrInit.tresor.grants`, `role:...`). An identity that carries that
   role in the IdP is covered, with nothing to change in tresor.
2. **Acting for sessions** (duckdb-acl's `ACT_FOR_SESSIONS`) needs the node among tresor's `policy.actors`. That list
   is tresor-server's static configuration (`ActorVerbs` matches `client:<its client id>`; the demo writes one entry per
   group into tresor's values). A group made from the console, with an identity the list does not name, logs in but
   cannot act for its sessions until tresor's configuration is changed and tresor restarted. With tresor-server's actor by
   role (decision 1) the rule names the nodes' role once, and the group's identity carrying that role is enough.

## Security

- **Kubernetes writes** need the `cluster` bundle, checked by the operator on every call. The person is named in
  `changed-by` and in the operator's log, never with a token.
- **Policy writes** are the node's to judge, under the person's own token. The console's call is a fixed statement
  with bound names; a name from the browser is checked against the policy's name rule before it is bound.
- **Nothing in the console reaches the IdP or tresor's configuration.** Both stay the installation's: no credential of
  either is held.

## Testing

- The console API's unit tests (`internal/console/group_api_test.go`):
  - create, change and delete, each refused below `cluster`, nothing written on a refusal;
  - the webhook's reason shown, the operator's own RBAC refusal only logged;
  - a new group's certificate is the configured one, never the browser's;
  - one field owner per field (`fleet_test.go`).
- The policy step: the statements as sent, names quoted as identifiers (`odd "role"`), a role the node does not list
  refused before anything is written, a draining node asked last, a node's refusal shown and no other node asked.
- The web console: the change list and the name rule (`lib.test.ts`), the button's conditions (`fleet.test.tsx`), and
  in the browser e2e an admin makes a group, adds it to the policy on the stand's node (a real `CREATE RESOURCE
  GROUP`), removes it and deletes it.
- The demo's check 13 (spec 004): a group made from the console, added to the policy, a principal served on it,
  removed, deleted, its node drained.

## Decisions (owner, 2026-10-22)

1. **tresor's actors by role.** tresor-server is asked for an actor named by a role (relayed 2026-10-22, its product
   gap: `config.go` refuses an actor that is not `client:<id>`). Then every group whose identity carries the nodes' role
   acts for sessions, and a new group needs nothing in tresor. Until that release the installation lists the group's
   client id in tresor's actors, and the console says so on the group's page.
2. **The identity is given, not made.** The console asks for an existing identity's client id and shows the subject it
   must trust. Making one needs a right in the IdP, which is the recipe's or the orchestrator's.
3. **A delete drains and keeps the policy's resource group.** Removing it from the policy is its own action.
4. **Rights.** Create, change and delete take the `cluster` bundle, as `Scale` does today. Adding to and removing from
   the policy take `policy`, judged by the node.

## Follow-ups

- Scaling bounds and autoscale (after the release).
- The policy as the DBeaver client edits it, and SQL under `ACL NATIVE` (spec 007).
- The bootstrap in the console: seen first, edited later.
