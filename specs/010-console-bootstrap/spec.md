# Spec 010: the bootstrap in the console - what a node starts with

- **Status**: draft
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
  contract):
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
  - the document as JSON, as the node receives it;
  - a refused document as the page's first line, with the reason.
- **The profile on the group's page**: the items that apply to its nodes, as a node of the group computes them
  (`platform.cluster_effective` on one of its serving nodes - the node's rule of a group's item over the cluster's,
  never a copy in the console), and the profile's version against the one each node applied (the load report's
  `config.target` / `config.applied`). With no serving node the section says so.

### Stage 2: changed (after stage 1)

- **The profile, from forms**: install / update / remove an extension, attach / detach a source, set / reset a
  setting - for the cluster, or `IN GROUP g` from a group's page. Each form writes one `CLUSTER` statement, run on a
  node under the person's token, once, on the node that answered (spec 006's way for the policy step): the node judges
  it (the `cluster` bundle) and answers `{version, class, applied_here, note}`.
  - The page shows the class: hot (applied there at once, the others on their tick), drain or restart (the
    orchestrator rolls the nodes).
  - The statement's text is built by the console from the form's fields as quoted identifiers and literals; nothing a
    person types reaches it as SQL. A source names a `SECRET` (never a credential: the node refuses one anyway).
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
- **vitest**: the document's parts as the page shows them; a refused document first.
- **Browser e2e**: the group's page shows the stand's document, and a person without `fleet` is refused.
- **Demo** (check 4c): the console's document for group a equals the one a node of a fetched at its entry (its log
  names the version and the group).
- Stage 2:
  - the statements built from forms, with their quoting;
  - the e2e: an extension installed and removed for a group, the answer's class shown.

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
