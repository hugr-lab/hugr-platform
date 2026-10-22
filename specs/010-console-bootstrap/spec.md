# Spec 010: the bootstrap in the console - what a node starts with

- **Status**: implemented (2026-10-22; decisions 1-3 as proposed)
- **Date**: 2026-10-22
- **Author**: hugr lab (the owner, 2026-10-20, spec 003's follow-up: the bootstrap seen in the console first, edited
  later)

## Summary

A node starts from two things: the bootstrap document the operator hands it at its entry (spec 001, hugr-node spec
010), and the cluster's profile it applies (hugr-node's `CLUSTER` statements). Today the console lists the profile on
the Cluster page and a node's drift on its page, but nowhere says what a group's nodes are handed, or why a node is
refused its document. Stage 1 shows both on the group's page. Stage 2 changes the profile from the console, by the
same `CLUSTER` statements, judged by a node.

## Problem

- **A node that does not start says little.** The operator refuses a document that would not pass the node's own
  check (503, "the platform's configuration is incomplete"), and the reason is only in the operator's log.
- **The document is invisible.** Its parts come from the platform's spec, the group's and the rollout. Nobody can see
  what a group's nodes are handed: tresor's host and the group's identity in it, the repository, the catalog's secret,
  the first policy.
- **The profile is changed only by SQL.** Changing it takes `ACL CLUSTER …` on a node with `ACL NATIVE`-level tooling;
  the console shows it read-only.

## Design

### Stage 1: seen (before the release)

- **`GET groups/{g}/bootstrap`** (the `fleet` right; names only - the document never holds a credential, by its own
  contract). The node API and the console build it by one function (`nodeapi.NodeDocument`), so they cannot drift:
  - `document`: what a node of the group is handed now, built by the operator's own functions - the node API's
    (`build.Document`, the first policy from the namespace's groups, the rollout's `migrate` for the canary), the pod's
    label shown as `<pod>`;
  - `served`: true, or the reason the node API refuses it now (`Document.Validate`'s problems, or the first policy's) -
    the same check, so the console says what a refused node sees;
  - `firstPolicy`: whether it is still in effect is the catalog's (a node writes it once, into a catalog nobody
    administers yet) - the page says so, never guesses.
- **The group's page** gains a Bootstrap section:
  - the document's parts as a person reads them: tresor (host; the identity's issuer, client id, scope), the
    repository (name, prefix), the catalog (secret, schema, a migration pending), the heartbeat, the first policy
    (issuer, audiences, roles claim, clients, the administrators' claim value, the resource groups and the default);
  - the document as JSON, as the node receives it (refused: as the operator would hand it);
  - a refused document as the group page's first alert, with the reason - read again with the page.
- **The profile on the group's page**: the items that apply to its nodes, as a node of the group computes them
  (`platform.cluster_effective` on one of its serving nodes - the node's rule of a group's item over the cluster's,
  never a copy in the console), the profile's version, how many nodes applied it and which are behind (each node's
  `config.target` / `config.applied` from its heartbeat). With no serving node, without the right, or on a standalone
  node (no profile) the section says which.

### Stage 2: changed (after stage 1)

- **The profile, from forms**: install / update / remove an extension, attach / detach a source, set / reset a
  setting - for the cluster, or `IN GROUP g` from a group's page. Each form writes one `CLUSTER` statement, run on a
  node under the person's token, once, on the node that answered (spec 006's way for the policy step): the node judges
  it (the `cluster` bundle) and answers `{version, class, applied_here, note}`.
  - The page shows the class: hot (applied there at once, the others on their tick), drain or restart (the
    orchestrator rolls the nodes).
  - The statement's text is built by the console (`POST cluster/profile`, the `cluster` bundle's - `Rights.Infrastructure`)
    from the form's fields; nothing a person types reaches it as SQL:
    - an extension, a repository, a type, a secret and a setting are plain names (`[A-Za-z_][A-Za-z0-9_]*`), written
      bare;
    - an alias and a dependency are quoted identifiers, a resource group (`IN GROUP`) a resource group's name, quoted -
      hugr-node reads a quoted name as one (`NameValue`);
    - a path, a version, a lineage name, a comment and a setting's value are string literals (the node keeps a
      setting's value as text whatever it is written as).
  - A source names a `SECRET` (never a credential: the node refuses one anyway). ATTACH's options beyond `READ_ONLY`
    stay the statement's own (SQL on a node).
  - The Cluster page offers the forms for the whole cluster, a group's page for its resource group - on a cluster node
    only (a standalone one keeps no profile).
- **The document's parts stay the install's.** The platform's spec is rendered by the chart from its values: a
  `helm upgrade` would put back whatever the console wrote. The group's parts (identity, size, doors) are spec 006's
  already.

## Security

- **Nothing secret is shown.** The document holds names and addresses only (`nodeproto.Document`: "never a
  credential"); the profile's items are hugr-node's, which refuses a credential in a path or an option.
- **Who sees what:**
  - the document: the `fleet` right, as the group's page;
  - the effective profile: a node's `platform.cluster_effective`, among the views the node lists for the person
    (`console_info`, `Rights.Effective`) - the `cluster` bundle's; a person without it sees the section's refusal.
- **Who changes the profile (stage 2)**: the node decides, by the person's token; the console only builds the
  statement.
- No new route reaches a node except as the person.

## Testing

- **Go**:
  - `groups/{g}/bootstrap` answers what the node API serves for a pod of the group: the same document, the pod as
    `<pod>`;
  - the canary's `migrate`;
  - a refused document with the reason;
  - the `fleet` right;
  - an unknown group gives 404.
- **vitest**: the document's parts as the page shows them; a refused document's title; the nodes behind the profile.
- **Browser e2e**: the group's page shows the stand's document, served (no alert), no first policy, its standalone
  node's word on the profile. A person without `fleet` is refused by the route (Go, `TestRightsGateEveryRoute`).
- **Demo** (check 4c): the console's document for group a is served (its nodes run on it) and names the platform's
  tresor, the group's own identity and the first policy's groups.
- Stage 2:
  - Go: each form's statement, with its quoting, and the refusals of what would be SQL (`TestProfileStatement`); the
    route writes once on a node that seats the person and answers the node's `{version, class, applied_here, note}`;
    the `cluster` bundle's;
  - vitest: the fields each change sends, what a form lacks, how the answer reads;
  - the e2e's node is a standalone one: no form offered there;
  - the demo (a cluster node): a source attached for group b through the console, listed, detached - each hot.

## Alternatives considered

- **Reading the document from a node** (what it fetched): it shows what a node got at its start, not what the next
  node will get. The operator's answer is the one that changes when the platform does.
- **Editing the platform's spec in the console**: the chart owns it; the console's write would be undone at the next
  upgrade, or would need the chart to give up those fields.
- **The effective profile computed in the console from the items**: a second copy of the node's override rule.

## Decisions for the owner

| # | question | proposal |
| --- | --- | --- |
| 1 | Who sees a group's document | the `fleet` right (names only); the effective profile stays the `cluster` bundle's, as the node gives it |
| 2 | The document's parts in the console | seen only; changed by the install's values (the chart owns the platform's spec) |
| 3 | Stage 2 | the profile's eight statements from forms, after stage 1 |
