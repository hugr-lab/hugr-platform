// Package upgrade orders a new node image across a platform's groups (spec 008): the preconditions, a canary that
// migrates the catalog when the image's build needs it, then the groups one at a time. Pure: what the administrator
// set, where the last rollout stands and what the groups run in; the next status out. The controller reads the world
// and writes the result; nothing here rolls anything back.
package upgrade

import (
	"fmt"
	"slices"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
)

// The rollout's phases.
const (
	PhasePreconditions = "Preconditions"
	PhaseCanary        = "Canary"
	PhaseGroups        = "Groups"
	PhaseComplete      = "Complete"
	PhaseBlocked       = "Blocked"
	PhasePaused        = "Paused"
)

// Target is what the administrator set: the image, what its release says of it, and the rollout's switches.
type Target struct {
	Image                string
	DuckDBVersion        string
	CatalogSchema        int64
	CatalogMinReader     int64
	Paused               bool
	AllowCatalogDowntime bool
}

// TargetOf a platform.
func TargetOf(platform *v1alpha1.HugrPlatform) Target {
	return Target{Image: platform.Spec.Node.Image, DuckDBVersion: platform.Spec.Node.DuckDBVersion,
		CatalogSchema: platform.Spec.Node.CatalogSchema, CatalogMinReader: platform.Spec.Node.CatalogMinReader,
		Paused: platform.Spec.Upgrade.Paused, AllowCatalogDowntime: platform.Spec.Upgrade.AllowCatalogDowntime}
}

// Group is one group as the rollout sees it.
type Group struct {
	Name string
	// Default: the group the platform's first policy names its default; it rolls first
	Default bool
	// Image is the one its Deployment's pods are made from ("": no Deployment yet)
	Image string
	// Replicas the group asks for
	Replicas int32
	// Rolled: the Deployment runs only Image - its generation observed, every replica updated and available
	Rolled bool
	// Failed is the Deployment's own word that its rollout stopped (its progress deadline passed)
	Failed string
	// Nodes are the group's live nodes (a Lease not expired), with the image their pod runs
	Nodes []Node
}

// Node is a live node: its pod's image and what its heartbeat says it is.
type Node struct {
	Name          string
	Image         string
	DuckDBVersion string
	// the catalog as the node's build judges it (hugr-node spec 017); 0: not reported
	CatalogBuild     int64
	CatalogSchema    int64
	CatalogMinReader int64
}

// Check answers what the repository lacks for the target ("" when nothing), or an error when it could not be read
// now - the rollout then waits and asks again. kista's content is kista's: the rollout only reads it (spec 008 Q5).
type Check func() (string, error)

// Decide the rollout's next status from the last one. `image` is the platform's status.image: the one every group runs
// when no rollout is under way ("" on a fresh install, or under an operator from before spec 008).
func Decide(now time.Time, target Target, image string, last *v1alpha1.UpgradeStatus, groups []Group,
	check Check) (string, *v1alpha1.UpgradeStatus) {
	order := Order(groups)
	if image == "" {
		from := runningImage(order)
		if from == "" || from == target.Image {
			return target.Image, last // a fresh install, or one already there: nothing to roll
		}
		image = from // an operator from before spec 008 on a fleet behind its spec: rolled like any other change
	}
	if last != nil && last.To == target.Image && last.Phase == PhaseComplete {
		return target.Image, last
	}
	if target.Image == image && (last == nil || last.Phase == PhaseComplete) {
		return image, last
	}
	up := last.DeepCopy()
	if up == nil || up.To != target.Image {
		// a new target - a release, or the old image set back mid-way - starts over: every group is rolled to it in
		// order, and one already there is done at once
		up = &v1alpha1.UpgradeStatus{From: image, To: target.Image, Phase: PhasePreconditions,
			StartedAt: metav1.NewTime(now)}
	}
	if target.Paused {
		if up.Phase != PhasePaused {
			if up.Phase != PhaseBlocked {
				up.Resume = up.Phase
			}
			up.Phase, up.Reason = PhasePaused, "spec.upgrade.paused"
		}
		return image, up
	}
	if up.Phase == PhaseBlocked {
		return image, up // stopped until the administrator pauses and resumes it, or sets another image
	}
	if up.Phase == PhasePaused {
		up.Phase, up.Resume, up.Reason = up.Resume, "", ""
	}
	for {
		switch up.Phase {
		case PhasePreconditions:
			reason, wait := preconditions(target, up, order, check)
			if reason != "" {
				return image, block(up, reason)
			}
			if wait != "" {
				up.Reason = wait
				return image, up
			}
			up.Reason = ""
			next := nextGroup(order, up)
			if next == "" {
				up.Phase, up.Group = PhaseComplete, ""
				return target.Image, up
			}
			up.Phase, up.Group = PhaseCanary, next
		case PhaseCanary, PhaseGroups:
			done, reason := rolled(target, up, find(order, up.Group))
			if reason != "" {
				return image, block(up, reason)
			}
			if !done {
				return image, up
			}
			if up.Group != "" && !slices.Contains(up.Moved, up.Group) {
				up.Moved = append(up.Moved, up.Group)
			}
			up.Migrate = false // the canary migrated; no other node does
			next := nextGroup(order, up)
			if next == "" {
				up.Phase, up.Group = PhaseComplete, ""
				return target.Image, up
			}
			up.Phase, up.Group = PhaseGroups, next
		default:
			return image, up
		}
	}
}

func block(up *v1alpha1.UpgradeStatus, reason string) *v1alpha1.UpgradeStatus {
	up.Resume, up.Phase, up.Reason = up.Phase, PhaseBlocked, reason
	return up
}

// preconditions: the repository holds the target's extensions, and the catalog may go where the target's build needs
// it. Sets whether the canary migrates.
func preconditions(target Target, up *v1alpha1.UpgradeStatus, order []Group, check Check) (string, string) {
	if check != nil {
		missing, err := check()
		if err != nil {
			return "", "waiting for the repository: " + err.Error()
		}
		if missing != "" {
			return missing, ""
		}
	}
	return catalog(target, up, order), ""
}

// catalog: whether the catalog may go where the target's build needs it, and whether the canary migrates it.
func catalog(target Target, up *v1alpha1.UpgradeStatus, order []Group) string {
	up.Migrate = false
	if target.CatalogSchema == 0 {
		return ""
	}
	// the catalog's stamp and the oldest build still serving it, from the nodes that do not run the target
	var schema, minReader, oldest int64
	for _, g := range order {
		for _, n := range g.Nodes {
			if n.Image == up.To {
				continue
			}
			if n.CatalogSchema > schema {
				schema, minReader = n.CatalogSchema, n.CatalogMinReader
			}
			if n.CatalogBuild > 0 && (oldest == 0 || n.CatalogBuild < oldest) {
				oldest = n.CatalogBuild
			}
		}
	}
	if schema == 0 {
		return "" // nobody reports the catalog: a new build that finds it older stops at its step 5 (no migration)
	}
	if minReader > 0 && target.CatalogSchema < minReader {
		return fmt.Sprintf("the catalog is schema %d, readable from build %d on, and the target's build is %d: "+
			"a node of it would refuse the catalog (it was migrated past this build's window)", schema, minReader,
			target.CatalogSchema)
	}
	if target.CatalogSchema <= schema {
		return "" // the same schema, or an older build inside the catalog's window (it serves, never writes)
	}
	if target.CatalogMinReader > 0 && oldest > 0 && target.CatalogMinReader > oldest && !target.AllowCatalogDowntime {
		return fmt.Sprintf("the target's build migrates the catalog to schema %d, readable from build %d on, and "+
			"the nodes serving now are build %d: they would stop serving until they move - set "+
			"spec.upgrade.allowCatalogDowntime to take that outage", target.CatalogSchema, target.CatalogMinReader,
			oldest)
	}
	up.Migrate = true
	return ""
}

// rolled says whether the group runs the target whole: its Deployment rolled to it, and each of its live nodes on the
// target image reporting the target's DuckDB release and catalog build. A reason stops the rollout.
func rolled(target Target, up *v1alpha1.UpgradeStatus, g *Group) (bool, string) {
	if g == nil {
		return true, "" // the group went away mid-rollout: nothing of it to move
	}
	if g.Image != up.To {
		return false, "" // its reconciler has not taken the target yet
	}
	if g.Failed != "" {
		return false, fmt.Sprintf("group %s: %s", g.Name, g.Failed)
	}
	if !g.Rolled {
		return false, ""
	}
	reported := int32(0)
	for _, n := range g.Nodes {
		if n.Image != up.To {
			continue
		}
		if target.DuckDBVersion != "" && n.DuckDBVersion != "" && n.DuckDBVersion != target.DuckDBVersion {
			return false, fmt.Sprintf("node %s of group %s runs DuckDB %s, and the release says %s", n.Name, g.Name,
				n.DuckDBVersion, target.DuckDBVersion)
		}
		if target.CatalogSchema > 0 && n.CatalogBuild > 0 && n.CatalogBuild != target.CatalogSchema {
			return false, fmt.Sprintf("node %s of group %s reads catalog schema %d, and the release says %d", n.Name,
				g.Name, n.CatalogBuild, target.CatalogSchema)
		}
		reported++
	}
	// every node of the new image has spoken (a heartbeat comes once a node serves)
	return reported >= g.Replicas, ""
}

// Order of the rollout: the default group first (the one most principals reach), then by name.
func Order(groups []Group) []Group {
	out := slices.Clone(groups)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Default != out[j].Default {
			return out[i].Default
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func find(order []Group, name string) *Group {
	for i := range order {
		if order[i].Name == name {
			return &order[i]
		}
	}
	return nil
}

// nextGroup to roll: the first in order not moved yet. The canary is the first that runs nodes - a group of none
// cannot migrate the catalog nor prove the image - so groups of none before it are moved at once.
func nextGroup(order []Group, up *v1alpha1.UpgradeStatus) string {
	for _, g := range order {
		if slices.Contains(up.Moved, g.Name) {
			continue
		}
		if up.Phase == PhasePreconditions && g.Replicas == 0 {
			up.Moved = append(up.Moved, g.Name)
			continue
		}
		return g.Name
	}
	return ""
}

// runningImage is the image most of the groups' Deployments run (ties by the rollout's order): where an operator
// from before spec 008 finds its fleet.
func runningImage(order []Group) string {
	counts := map[string]int{}
	best := ""
	for _, g := range order {
		if g.Image == "" {
			continue
		}
		counts[g.Image]++
		if best == "" || counts[g.Image] > counts[best] {
			best = g.Image
		}
	}
	return best
}

// ImageFor a group's Deployment: the target once the rollout has reached the group, otherwise what the group runs.
// `current` is its Deployment's image ("": none yet).
func ImageFor(platform *v1alpha1.HugrPlatform, group string, current string) string {
	target := platform.Spec.Node.Image
	up := platform.Status.Upgrade
	if up != nil && up.To == target && up.Phase != PhaseComplete {
		if slices.Contains(up.Moved, group) || (group == up.Group && up.Phase != PhasePreconditions) {
			return up.To
		}
		if current != "" {
			return current
		}
		if up.From != "" {
			return up.From // a group made mid-rollout starts where the fleet is, and is rolled with it
		}
		return target
	}
	if platform.Status.Image == target || current == "" {
		// rolled, or a new group: the target, or the fleet's image while the rollout to a new target has not begun
		if current == "" && platform.Status.Image != "" && platform.Status.Image != target {
			return platform.Status.Image
		}
		return target
	}
	return current // the rollout to a new target has not begun: nothing moves before it decides
}
