# Spec 008: upgrades - the platform, release by release

- **Status**: accepted 2026-10-21 (the owner's answers to Q1-Q6 below)
- **Date**: 2026-10-21

## Summary

A platform release is one version that pins every part: the operator (with the front and hugr-init), tresor-server,
kista (when the umbrella runs its own), and the node image with its DuckDB. An upgrade is one explicit act: `helm upgrade` to the
next release, or a catalog's update button doing the same. Helm moves the services. The operator then moves the nodes
in a safe order:
1. the repository holds the new DuckDB's extensions;
2. the catalog is migrated by a node of the new build;
3. the groups roll one after another, pod by pod, each drained.

The operator records where it stands, stops on the first failure, and never rolls anything back by itself.

## Problem

Today an upgrade is a plain Kubernetes rolling update, and nothing else (research of 2026-10-21):

- **Every group rolls at once.** A new `spec.node.image` reaches every group's Deployment in the same reconcile: no
  order, no canary, no status.
  - A pod whose bootstrap fails never becomes ready, so the roll stalls with the old pods serving. That is the only
    safety, and nobody is told.
- **The catalog is never migrated.** A node of a new build refuses an older catalog (duckdb-acl spec 094: "migrate it
  first"), and an old build cannot migrate forward. A release with a schema step therefore has no path but a manual
  duckdb of the new build.
- **The repository does not learn the new DuckDB.**
  - kista's `bootstrap` applies once; a changed section only logs a warning.
  - A node's catalog extension is installed with no version: whatever the channel serves for the node's DuckDB. A
    channel without the new DuckDB fails the new nodes at step 5.
- **The CRDs are never upgraded.** They sit in the chart's `crds/`, and the upgrade docs say "kubectl apply first".
  A catalog install has no such step.
- **Nothing knows which build runs where.** The heartbeat carries the node's build and DuckDB, and the operator drops
  both.
- **The services' own rules are not composed.** tresor and kista migrate at start, each with a window. tresor needs
  its key store up to start a pod. kista is `Recreate` on a ReadWriteOnce volume. tresor speaks `duckdb-secrets/1`,
  and its client demands an exact match.

## Design

### The release

- **One version, everything pinned.**
  - The umbrella chart `hugr` at version V pins the platform chart V (the operator image vV, which is also the front
    and hugr-init), the tresor-server and kista charts, the OpenBao image, the node image `hugr-node:X`, and that
    image's DuckDB version (`global.duckdbVersion`).
  - The strict install (the platform chart alone) pins the operator and the node image.
- **The release notes state what the upgrade does**: the catalog schema it needs, its window, and whether a step is
  strict (old nodes stop serving once it is applied), each service's migrations, and the DuckDB change.
- **One release at a time.** An upgrade from V-1 to V is the supported path. Skipping a release works only where every
  window allows it; the notes of each release say so.

### Who moves what

| part | moved by | how it rolls | what holds during the roll |
| --- | --- | --- | --- |
| CRDs | Helm, by a pre-upgrade hook (Q4) | they stay in `crds/` for the first install; before an upgrade a Job of the new operator image applies the CRDs it carries, server-side | only additive changes inside a version; a CRD that drops a served or stored version is refused, so a breaking change is a new API version with conversion |
| tresor-server | its own recipe (spec 005 step 3: given by its address); the umbrella's own (one replica, SQLite) by Helm | its own recipe's (a release of tresor-server's: rolling, 2 replicas, PDB, migrations at start under a lock) | an old replica keeps serving after a migration; the protocol is `duckdb-secrets/1` on both sides |
| kista | its own recipe (given by its address); the umbrella's own (one replica, SQLite) by Helm | `Recreate` on its volume: a short gap | nodes need kista only to start and for a profile item, so the operator rolls no node while kista is not ready |
| operator | Helm | rolling, 2 replicas, leader election | old and new replicas serve the node API side by side: the document stays v2, fields are only added |
| front | the operator, after its own upgrade (the operator's image) | rolling, PDB; `GracefulStop` within 30 s | a stream longer than that is cut; the client resumes on another front, the session following it (duckdb-acl spec 135) |
| kista's content | kista itself (Q5): it keeps an extension's builds for every DuckDB version its channel serves | the platform upgrades kista (its chart, its store) and checks the target DuckDB's builds are there before any node moves | old versions stay: old nodes keep installing theirs |
| the catalog schema | the operator, through a node of the new build (Q2) | `acl_migrate_catalog`, once | old nodes inside the window serve read-only, policy writes refused; a strict step needs the admin's word (Q3) |
| nodes | the operator | group by group, pod by pod, drained (Q1) | below |

### The nodes' rollout

- **The trigger** is a change of `HugrPlatform.spec.node.image`, the admin's act (the upgrade or an edit). The
  operator does not apply the new image to every group at once any more. It writes `status.upgrade`
  (`from`, `to`, `phase`, the group in progress, `startedAt`, the reason it stopped) and moves through these phases:
  1. **Preconditions.** kista is ready, and the channel serves the target DuckDB.
     - The operator HEADs the catalog's extension (its engine's scanner) under the channel for the target DuckDB and
       each node architecture in the cluster.
     - The target DuckDB is `spec.node.duckdbVersion`, a new field the chart fills from `global.duckdbVersion`. The
       first new node's heartbeat confirms it.
     - A missing extension: phase `Blocked`, the reason naming the channel, the DuckDB version and the extension,
       nothing rolled. Putting the builds into kista (a channel version, a mirror, a publish) is kista's own
       administration, never the operator's: the operator only checks.
  2. **The catalog.** The operator compares the catalog's stamp (from the old nodes' heartbeats: `acl_catalog_schema`)
     with the target build's (`spec.node.catalogSchema`, `catalogMinReader`, from the release).
     - Same version: skip.
     - Newer target: the canary's bootstrap document carries `catalog.migrate: true`. That node (the new build, which
       holds the steps) runs `acl_migrate_catalog` at step 5 and goes on. The others never migrate.
     - A strict step (the target's min reader above the old build's version, so old nodes would stop serving at their
       next freshness check) waits for `spec.upgrade.allowCatalogDowntime: true` (Q3).
  3. **The canary.** The first group's Deployment gets the new image. Its first new pod has to become ready (maxSurge 1,
     maxUnavailable 0, as today); an old one drains only then.
  4. **The groups, one after another.** The order is the default group first, then by name. Each group's roll waits for
     the previous one's Deployment to be complete and every new node's heartbeat to report the target build.
  5. **Done.** `status.upgrade.phase: Complete`, and `observedImage` per group.
- **A failure stops the rollout.** A pod not ready within the group's deadline, a node refusing the catalog, or a
  heartbeat with another build all set phase `Blocked`, with the reason. Groups already moved stay moved, and the rest
  keep their image.
- **Nothing rolls back by itself** (the owner's rule). The admin continues (`spec.upgrade.paused: false` after a fix)
  or sets the old image back, which is a rollout like any other.
- **Capacity.** One group rolls at a time, so one surge pod at a time: one spare node's worth of capacity, not one per
  group.
- **The heartbeat's build counts.** `image` (the hugr_node build) and `duckdb` become part of each node's summary;
  the group's status counts nodes per build, and the console shows it.

### Compatibility (the skew the rollout relies on)

- **Operator V serves nodes V and V-1.** The bootstrap document stays v2; a field is added, never changed. A node
  ignores what it does not know.
- **Front V routes nodes V and V-1.** It reads labels and trailers only.
- **tresor V serves nodes V-1.** The protocol string has not changed; when it does, tresor has to serve the previous
  one for a release (a product gap to relay then).
- **The catalog**: within the window, by acl's own rule. A release whose step is strict says so in its notes.
- **kista**: any node installs from any kista through DuckDB's own repository protocol.

### Rollback

- **The node image back**: a rollout like any other. It is possible while the catalog is still inside the old build's
  window, and the operator refuses it otherwise, naming the catalog's stamp.
- **The operator back**: `helm rollback`. The CRDs only grew, so the old operator reads them.
- **The services back** (tresor, kista): not supported once their migrations ran, by their own rules. A
  restore from backup is the way, as each product documents. The platform says so and does not try.

### The pieces of this spec

| where | what |
| --- | --- |
| hugr-platform | `status.upgrade`, the phased rollout in the group reconciler, `spec.node.duckdbVersion` / `catalogSchema` / `catalogMinReader`, `spec.upgrade.paused` / `allowCatalogDowntime`, the heartbeat's build in the summary, CRDs in templates, the console's upgrade view (with 006) |
| hugr-node | the document's `catalog.migrate`: step 5 migrates when told, and only then; the heartbeat adds the catalog's stamp (`acl_catalog_schema`) |
| the demo | an upgrade check: install, upgrade to a node image with another tag, then watch the groups roll one by one, nothing lost, the status complete. A catalog migration needs two acl builds, so it runs once a second release exists |

### Built so far

- **hugr-node spec 017** (hugr-node #18): the document's `catalog.migrate`, and the heartbeat's `duckdb_version`,
  `platform` and `catalog` (`type`, `extension`, `build`, `build_min_reader`, `schema`, `min_reader`, `mode`). Its
  bootstrap e2e stamps a catalog one schema version back: a node without the flag stops at step 5, and one with it
  migrates and serves.
- **What each node is, in the fleet** (this repository): the heartbeat's build, DuckDB release, platform, catalog
  extension and catalog stamps go into the node's Lease (`platform.hugr.io/build`, `duckdb-version`, `platform`,
  `catalog-*`). A key the latest report leaves out is removed, so a value never outlives the build that wrote it, and
  a node's string is kept to 128 bytes. A group's status gains `builds` (live nodes per build and DuckDB release)
  and `catalog` (the newest stamp a live node read).
- **The phased rollout** (this repository):
  - **The state.** `status.image` is the image every group runs when no rollout is under way. `status.upgrade`
    holds `from`, `to`, `phase` (Preconditions, Canary, Groups, Complete, Blocked, Paused), the `group` rolling, the
    groups `moved`, `migrate`, `resume` (the phase a blocked or paused rollout returns to), `reason` and
    `startedAt`.
  - **Who decides.** `internal/upgrade` is pure: `Decide` takes the target, the last status and what each group runs,
    and returns the next status. The `UpgradeReconciler` reads the world every 5 s and writes the two fields with a
    merge patch.
  - **Who applies.** The group reconciler builds each Deployment with `upgrade.ImageFor`: the target once the rollout
    has reached the group, and what the group runs until then. Between a new image and the rollout's first decision,
    nothing moves.
  - **The canary's flag.** The node API sets `catalog.migrate` only for a pod of the canary group made from the
    target image, while the phase is Canary and the rollout migrates.
  - **The release's facts** come from the charts: `spec.node.duckdbVersion` (the umbrella takes `global.duckdbVersion`)
    and `catalogSchema` / `catalogMinReader` (22 / 21 for hugr-node v0.2.0, the schema of its acl, 41ec046).
  - **The demo's check 12**: the same build under another name rolls group by group - the canary a first, b waiting on
    its image, then b - and the rollout ends Complete with the fleet serving. A catalog migration needs a second acl
    build: it is the bootstrap e2e's in hugr-node (spec 017) until a second release exists.
- **Decisions taken while building** (within the accepted design):
  - **A blocked rollout stays blocked.** It continues after the administrator pauses and resumes it
    (`spec.upgrade.paused` true, then false), or sets another image. A repository that does not answer is waited
    for and never blocks; one that answers without an extension blocks.
  - **The order and the canary.** The canary is the first group in the order (the first policy's default group,
    else the group whose nodes report the default, then by name) that runs nodes. Groups of no node before it are
    moved at once. A group is done when its Deployment rolled (every replica updated and available, no old one) and
    every node of the new image has reported.
  - **A wrong release fact blocks.** A new node that reports another DuckDB release, or another catalog build than
    the release says, blocks the rollout: a mistyped value in the chart is caught at the canary.
  - **A new target starts over.** A new image mid-rollout (the old one set back, or another release) starts a rollout
    to it from `status.image`, and every group is rolled to it in order.
- **The CRDs before an upgrade** (Q4 as replaced, the owner 2026-10-22). Helm never upgrades `crds/`, so on an
  existing install the API server would drop the new fields until the CRDs are applied, and moving them to
  `templates/` breaks the first install (the chart's own `HugrPlatform` would not map).
  - The CRDs stay in `crds/` for the first install.
  - Before an upgrade, a pre-upgrade hook Job of the new operator image, `hugr-init --phase crds`, server-side-applies
    the CRDs that image carries (`internal/crds`, which `make manifests` copies and a test keeps equal to the chart's).
    It takes the fields over from whoever wrote them (Helm's install, a `kubectl apply`).
  - A CRD that would drop a version the cluster serves or stores is refused before anything is applied, so the
    upgrade stops there.
  - Its ServiceAccount, ClusterRole (get and patch of the two CRDs, by `resourceNames`) and binding are hooks too:
    they exist only while the upgrade runs (`crdUpgrade.enabled`, on by default).
  - The demo's check 11 (an upgrade of the release to itself) runs the hook.

## Security

- **An upgrade is the admin's act, and so is a rollback.** The operator never chooses a version. It only orders what
  the admin set.
- **A migration runs as a node.** The canary migrates with the catalog's login it already holds (its tresor grant).
  No new credential exists and none is stored. The document flag is the operator's, served only to the canary pod,
  verified by TokenReview like every document.
- **A strict catalog step is a stated outage**, never a surprise: `allowCatalogDowntime` is a written decision in
  the resource's history.
- **The repository's content stays kista's.** The operator only reads it (a HEAD of a public path, as a node does);
  it holds no kista credential and writes nothing there.

## Testing

- **Unit**: the phase machine (preconditions, migration, canary, group order, block, pause, resume) against
  fake Deployments and heartbeats; the strict-step rule; the document flag only for the canary.
- **envtest**: an image change moves one group at a time; a group that never gets ready blocks the next one; a rollback
  is refused past the window.
- **The demo**: the upgrade check above, on kind in CI.
- **hugr-node**: step 5 with `catalog.migrate` on an older catalog migrates it; without it, it refuses as today.

## Alternatives considered

- **All groups at once (today).** It is fastest, but needs a spare node per group, and a bad image reaches every
  group before anyone knows.
- **A migration Job instead of the canary.** It needs an identity that may read the catalog's secret from tresor, and
  an image of the new build anyway. The canary already has both.
- **Nodes migrate by themselves on an older catalog.** Rejected by duckdb-acl spec 094 (design/017 §3.2a): the first
  new node would change the shared catalog with no decision.
- **A blue/green pool per upgrade.** It doubles capacity for the upgrade's length. Possible later for strict steps.

## The owner's decisions (2026-10-21)

- **Q1 - nodes**: one group at a time, a canary first. Within a group, pod by pod, each drained.
- **Q2, Q3 - the catalog**: the canary migrates it on the operator's flag. A step inside the window goes with the
  upgrade; a strict step waits for `spec.upgrade.allowCatalogDowntime`.
- **Q4 - CRDs**: first `templates/` with `helm.sh/resource-policy: keep` (proposed, not objected to). Building it showed
  that this breaks the first install: the chart's own `HugrPlatform` would not map. Replaced (the owner, 2026-10-22):
  the CRDs stay in `crds/`, and a pre-upgrade hook applies the new release's CRDs from the operator's image.
- **Q5 - kista**: "kista keeps the extensions' versions for each DuckDB version, so the task is to migrate kista itself,
  and to check the versions are there." The platform upgrades kista and checks; it adds nothing to a channel, and no
  product gap is asked of kista.
- **Q6 - the internal OpenBao** (umbrella step 2, spec 005; the umbrella dropped OpenBao on 2026-10-22, spec 005 step 3): PostgreSQL storage, OpenBao's own recommendation -
  stateless pods, HA on two replicas, a fourth database on the platform's server.

## Follow-ups

- The extensions of a release for a real install come from our public channel, mirrored by the platform's kista
  (spec 005). That channel is a prerequisite of every upgrade outside the demo.
- tresor's protocol N-1, when its version string changes.
- The front's grace for long streams, if 30 s proves short.
- Automatic upgrades (a channel of platform releases) - not before the explicit path is proven.
