package upgrade

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
)

var now = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

const oldImage, newImage = "hugr-node:v0.2.0", "hugr-node:v0.3.0"

// a group of n nodes, all on the image, at build 22 on a catalog of schema 22 (readable from 21)
func group(name string, n int32, image string, def bool) Group {
	g := Group{Name: name, Default: def, Image: image, Replicas: n, Rolled: true}
	for i := int32(0); i < n; i++ {
		g.Nodes = append(g.Nodes, Node{Name: name + string(rune('a'+i)), Image: image, DuckDBVersion: "v2.0.0",
			CatalogBuild: 22, CatalogSchema: 22, CatalogMinReader: 21})
	}
	return g
}

// rolled to the image at build b on a catalog of the given stamp
func onto(g Group, image string, build, schema, minReader int64) Group {
	g.Image, g.Rolled, g.Failed = image, true, ""
	for i := range g.Nodes {
		g.Nodes[i].Image, g.Nodes[i].DuckDBVersion = image, "v2.0.1"
		g.Nodes[i].CatalogBuild, g.Nodes[i].CatalogSchema, g.Nodes[i].CatalogMinReader = build, schema, minReader
	}
	return g
}

func target() Target {
	return Target{Image: newImage, DuckDBVersion: "v2.0.1", CatalogSchema: 23, CatalogMinReader: 22}
}

func none() (string, error) { return "", nil }

func TestFreshInstallRollsNothing(t *testing.T) {
	image, up := Decide(now, target(), "", nil, []Group{{Name: "a", Replicas: 2}}, none)
	if image != newImage || up != nil {
		t.Errorf("a fresh install: image %q, rollout %+v", image, up)
	}
	image, up = Decide(now, target(), "", nil, []Group{group("a", 2, newImage, true)}, none)
	if image != newImage || up != nil {
		t.Errorf("a fleet already on the target: image %q, rollout %+v", image, up)
	}
}

func TestRolloutCanaryThenGroupsInOrder(t *testing.T) {
	groups := []Group{group("b", 1, oldImage, false), group("a", 2, oldImage, true), group("c", 0, oldImage, false)}
	image, up := Decide(now, target(), oldImage, nil, groups, none)
	// the default group a is the canary; the catalog (22) is older than the target's build (23), whose min reader
	// (22) the serving build (22) is: a step inside the window
	if image != oldImage || up.Phase != PhaseCanary || up.Group != "a" || !up.Migrate || up.From != oldImage ||
		up.To != newImage || len(up.Moved) != 0 {
		t.Fatalf("the rollout's start: image %q, %+v", image, up)
	}
	// the canary's Deployment is not rolled yet: it waits
	groups[1].Image = newImage
	groups[1].Rolled = false
	if _, up = Decide(now, target(), image, up, groups, none); up.Phase != PhaseCanary || up.Group != "a" {
		t.Fatalf("a canary not rolled moved on: %+v", up)
	}
	// rolled, the catalog migrated by it: the next group, and no other node migrates
	groups[1] = onto(groups[1], newImage, 23, 23, 22)
	if _, up = Decide(now, target(), image, up, groups, none); up.Phase != PhaseGroups || up.Group != "b" || up.Migrate ||
		!slices.Equal(up.Moved, []string{"a"}) {
		t.Fatalf("after the canary: %+v", up)
	}
	groups[0] = onto(groups[0], newImage, 23, 23, 22)
	// c runs no node: moved as soon as its Deployment takes the image
	if _, up = Decide(now, target(), image, up, groups, none); up.Phase != PhaseGroups || up.Group != "c" {
		t.Fatalf("after b: %+v", up)
	}
	groups[2].Image = newImage
	image, up = Decide(now, target(), image, up, groups, none)
	if image != newImage || up.Phase != PhaseComplete || up.Group != "" || !slices.Equal(up.Moved, []string{"a", "b", "c"}) {
		t.Fatalf("every group rolled: image %q, %+v", image, up)
	}
	// complete stays complete
	if image2, up2 := Decide(now, target(), image, up, groups, none); image2 != newImage || up2.Phase != PhaseComplete {
		t.Errorf("a complete rollout moved again: %q %+v", image2, up2)
	}
}

func TestNodesNotReportedYetHold(t *testing.T) {
	groups := []Group{group("a", 2, oldImage, true)}
	image, up := Decide(now, target(), oldImage, nil, groups, none)
	groups[0] = onto(groups[0], newImage, 23, 23, 22)
	groups[0].Nodes = groups[0].Nodes[:1] // one of two has not sent a heartbeat yet
	if _, up = Decide(now, target(), image, up, groups, none); up.Phase != PhaseCanary {
		t.Errorf("a group with a node not reported moved on: %+v", up)
	}
}

func TestRepositoryMissingBlocks(t *testing.T) {
	missing := func() (string, error) { return "the channel serves no postgres_scanner for v2.0.1 on linux_arm64", nil }
	image, up := Decide(now, target(), oldImage, nil, []Group{group("a", 1, oldImage, true)}, missing)
	if image != oldImage || up.Phase != PhaseBlocked || up.Resume != PhasePreconditions ||
		!strings.Contains(up.Reason, "postgres_scanner") || up.Group != "" {
		t.Fatalf("a missing extension: %q %+v", image, up)
	}
	// blocked stays blocked when the extension appears: the administrator decides
	if _, up = Decide(now, target(), image, up, []Group{group("a", 1, oldImage, true)}, none); up.Phase != PhaseBlocked {
		t.Fatalf("a blocked rollout went on by itself: %+v", up)
	}
	// paused, then resumed: the step is tried again
	paused := target()
	paused.Paused = true
	if _, up = Decide(now, paused, image, up, []Group{group("a", 1, oldImage, true)}, none); up.Phase != PhasePaused ||
		up.Resume != PhasePreconditions {
		t.Fatalf("paused: %+v", up)
	}
	if _, up = Decide(now, target(), image, up, []Group{group("a", 1, oldImage, true)}, none); up.Phase != PhaseCanary ||
		up.Reason != "" {
		t.Fatalf("resumed: %+v", up)
	}
}

func TestStrictCatalogStepNeedsTheWord(t *testing.T) {
	strict := target()
	strict.CatalogMinReader = 23 // the serving build (22) could not read what the target writes
	_, up := Decide(now, strict, oldImage, nil, []Group{group("a", 1, oldImage, true)}, none)
	if up.Phase != PhaseBlocked || !strings.Contains(up.Reason, "allowCatalogDowntime") {
		t.Fatalf("a strict step without the word: %+v", up)
	}
	strict.AllowCatalogDowntime = true
	if _, up = Decide(now, strict, oldImage, nil, []Group{group("a", 1, oldImage, true)}, none); up.Phase != PhaseCanary ||
		!up.Migrate {
		t.Fatalf("a strict step with the word: %+v", up)
	}
}

func TestRollbackPastTheWindowBlocks(t *testing.T) {
	// the fleet on v0.3.0 migrated the catalog to 23 (readable from 22); setting v0.1.0 (build 21) back is refused
	back := Target{Image: "hugr-node:v0.1.0", CatalogSchema: 21, CatalogMinReader: 21}
	g := onto(group("a", 1, oldImage, true), newImage, 23, 23, 22)
	_, up := Decide(now, back, newImage, nil, []Group{g}, none)
	if up.Phase != PhaseBlocked || !strings.Contains(up.Reason, "past this build's window") {
		t.Fatalf("a rollback past the catalog's window: %+v", up)
	}
	// v0.2.0 (build 22) is inside it: it serves read-only, never migrates
	inside := Target{Image: oldImage, CatalogSchema: 22, CatalogMinReader: 21}
	if _, up = Decide(now, inside, newImage, nil, []Group{g}, none); up.Phase != PhaseCanary || up.Migrate {
		t.Fatalf("a rollback inside the window: %+v", up)
	}
}

func TestFailuresBlock(t *testing.T) {
	groups := []Group{group("a", 1, oldImage, true)}
	image, up := Decide(now, target(), oldImage, nil, groups, none)
	groups[0].Image, groups[0].Rolled, groups[0].Failed = newImage, false, "ReplicaSet has timed out progressing"
	if _, blocked := Decide(now, target(), image, up, groups, none); blocked.Phase != PhaseBlocked ||
		blocked.Resume != PhaseCanary || !strings.Contains(blocked.Reason, "timed out") {
		t.Errorf("a canary past its deadline: %+v", blocked)
	}
	// a node of another DuckDB than the release says
	groups[0] = onto(group("a", 1, oldImage, true), newImage, 23, 23, 22)
	groups[0].Nodes[0].DuckDBVersion = "v2.0.0"
	if _, blocked := Decide(now, target(), image, up, groups, none); blocked.Phase != PhaseBlocked ||
		!strings.Contains(blocked.Reason, "DuckDB v2.0.0") {
		t.Errorf("a node of another DuckDB: %+v", blocked)
	}
	// a node whose build reads another catalog schema than the release says
	groups[0] = onto(group("a", 1, oldImage, true), newImage, 24, 24, 22)
	if _, blocked := Decide(now, target(), image, up, groups, none); blocked.Phase != PhaseBlocked ||
		!strings.Contains(blocked.Reason, "schema 24") {
		t.Errorf("a node of another catalog build: %+v", blocked)
	}
}

func TestANewTargetStartsOver(t *testing.T) {
	groups := []Group{group("a", 1, oldImage, true), group("b", 1, oldImage, false)}
	image, up := Decide(now, target(), oldImage, nil, groups, none)
	groups[0] = onto(groups[0], newImage, 23, 23, 22)
	_, up = Decide(now, target(), image, up, groups, none) // a moved, b rolling
	// the administrator sets the old image back mid-way: a rollout to it, from the fleet's image
	back := Target{Image: oldImage, CatalogSchema: 22, CatalogMinReader: 21}
	_, up2 := Decide(now, back, image, up, groups, none)
	if up2.To != oldImage || up2.From != oldImage || up2.Phase != PhaseCanary || up2.Group != "a" || up2.Migrate {
		t.Errorf("the old image set back: %+v", up2)
	}
}

func TestPausedHolds(t *testing.T) {
	paused := target()
	paused.Paused = true
	image, up := Decide(now, paused, oldImage, nil, []Group{group("a", 1, oldImage, true)}, none)
	if image != oldImage || up.Phase != PhasePaused || up.Resume != PhasePreconditions {
		t.Errorf("a rollout paused before it began: %q %+v", image, up)
	}
}

func platform(image string, status v1alpha1.HugrPlatformStatus) *v1alpha1.HugrPlatform {
	p := &v1alpha1.HugrPlatform{Status: status}
	p.Spec.Node.Image = image
	return p
}

func TestImageFor(t *testing.T) {
	cases := []struct {
		name     string
		platform *v1alpha1.HugrPlatform
		group    string
		current  string
		want     string
	}{
		{"a fresh install", platform(newImage, v1alpha1.HugrPlatformStatus{}), "a", "", newImage},
		{"steady", platform(newImage, v1alpha1.HugrPlatformStatus{Image: newImage}), "a", newImage, newImage},
		{"a hand edit put back", platform(newImage, v1alpha1.HugrPlatformStatus{Image: newImage}), "a", "x", newImage},
		{"a new target, not begun", platform(newImage, v1alpha1.HugrPlatformStatus{Image: oldImage}), "a", oldImage, oldImage},
		{"a new group before the rollout", platform(newImage, v1alpha1.HugrPlatformStatus{Image: oldImage}), "n", "", oldImage},
		{"the canary", platform(newImage, v1alpha1.HugrPlatformStatus{Image: oldImage, Upgrade: &v1alpha1.UpgradeStatus{
			From: oldImage, To: newImage, Phase: PhaseCanary, Group: "a"}}), "a", oldImage, newImage},
		{"not reached yet", platform(newImage, v1alpha1.HugrPlatformStatus{Image: oldImage, Upgrade: &v1alpha1.UpgradeStatus{
			From: oldImage, To: newImage, Phase: PhaseCanary, Group: "a"}}), "b", oldImage, oldImage},
		{"moved", platform(newImage, v1alpha1.HugrPlatformStatus{Image: oldImage, Upgrade: &v1alpha1.UpgradeStatus{
			From: oldImage, To: newImage, Phase: PhaseGroups, Group: "b", Moved: []string{"a"}}}), "a", newImage, newImage},
		{"blocked keeps the canary on the target", platform(newImage, v1alpha1.HugrPlatformStatus{Image: oldImage,
			Upgrade: &v1alpha1.UpgradeStatus{From: oldImage, To: newImage, Phase: PhaseBlocked, Group: "a"}}), "a",
			newImage, newImage},
		{"blocked at the preconditions moves nobody", platform(newImage, v1alpha1.HugrPlatformStatus{Image: oldImage,
			Upgrade: &v1alpha1.UpgradeStatus{From: oldImage, To: newImage, Phase: PhaseBlocked}}), "a", oldImage, oldImage},
		{"a new group mid-rollout", platform(newImage, v1alpha1.HugrPlatformStatus{Image: oldImage, Upgrade: &v1alpha1.UpgradeStatus{
			From: oldImage, To: newImage, Phase: PhaseGroups, Group: "b"}}), "n", "", oldImage},
		{"complete", platform(newImage, v1alpha1.HugrPlatformStatus{Image: newImage, Upgrade: &v1alpha1.UpgradeStatus{
			From: oldImage, To: newImage, Phase: PhaseComplete}}), "a", oldImage, newImage},
	}
	for _, c := range cases {
		if got := ImageFor(c.platform, c.group, c.current); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// a group that runs no node before the first that does is moved at once: the canary is the first group with nodes
func TestEmptyGroupsBeforeTheCanary(t *testing.T) {
	groups := []Group{group("a", 0, oldImage, true), group("b", 1, oldImage, false)}
	_, up := Decide(now, target(), oldImage, nil, groups, none)
	if up.Phase != PhaseCanary || up.Group != "b" || !slices.Equal(up.Moved, []string{"a"}) {
		t.Errorf("an empty default group: %+v", up)
	}
	if _, up = Decide(now, target(), oldImage, nil, []Group{group("a", 0, oldImage, true)}, none); up.Phase != PhaseComplete {
		t.Errorf("no group runs a node: %+v", up)
	}
}

// a repository that cannot be read now is waited for, never a block
func TestRepositoryUnreadableWaits(t *testing.T) {
	down := func() (string, error) { return "", errors.New("dial tcp: connection refused") }
	_, up := Decide(now, target(), oldImage, nil, []Group{group("a", 1, oldImage, true)}, down)
	if up.Phase != PhasePreconditions || !strings.Contains(up.Reason, "waiting for the repository") {
		t.Fatalf("an unreadable repository: %+v", up)
	}
	if _, up = Decide(now, target(), oldImage, up, []Group{group("a", 1, oldImage, true)}, none); up.Phase != PhaseCanary ||
		up.Reason != "" {
		t.Fatalf("the repository back: %+v", up)
	}
}
