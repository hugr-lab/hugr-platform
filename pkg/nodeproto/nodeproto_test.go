package nodeproto

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func validDocument() Document {
	return Document{
		Version: 2, NodeGroup: "etl", NodeLabel: "hugr-node-etl-7f9c-x2k8p",
		Tresor:     Tresor{Host: "tresor.example:443", Identity: Identity{Issuer: "https://login.example/t/v2.0", ClientID: "etl-app"}},
		Repository: Repository{Name: "hugr", Prefix: "https://kista.example/t/prod"},
		Catalog:    Catalog{Secret: "hugr_policy_catalog", Schema: "acl"},
		Heartbeat:  Heartbeat{IntervalSeconds: 15},
	}
}

func TestDocumentValidate(t *testing.T) {
	if err := validDocument().Validate(); err != nil {
		t.Fatalf("a complete document is refused: %v", err)
	}
	cases := map[string]func(*Document){
		"another version":         func(d *Document) { d.Version = 1 },
		"no group":                func(d *Document) { d.NodeGroup = "" },
		"a label with a dot":      func(d *Document) { d.NodeLabel = "hugr.node" },
		"an upper-case label":     func(d *Document) { d.NodeLabel = "Node-1" },
		"a label past 63":         func(d *Document) { d.NodeLabel = strings.Repeat("a", 64) },
		"no client id":            func(d *Document) { d.Tresor.Identity.ClientID = "" },
		"a repository over http":  func(d *Document) { d.Repository.Prefix = "http://kista.example" },
		"a repository's userinfo": func(d *Document) { d.Repository.Prefix = "https://u:p@kista.example" },
		"no catalog secret":       func(d *Document) { d.Catalog.Secret = "" },
		"a heartbeat of 0":        func(d *Document) { d.Heartbeat.IntervalSeconds = 0 },
		"a heartbeat past 3600":   func(d *Document) { d.Heartbeat.IntervalSeconds = 3601 },
	}
	for name, change := range cases {
		d := validDocument()
		change(&d)
		if err := d.Validate(); err == nil {
			t.Errorf("%s: served although the node would refuse it", name)
		}
	}
}

func validFirstPolicy() *FirstPolicy {
	return &FirstPolicy{
		Issuer: FirstPolicyIssuer{Name: "corp", URL: "https://login.example/t/v2.0", Audiences: []string{"api://hugr"},
			RolesClaim: "roles"},
		Clients:        []FirstPolicyClient{{Name: "console", ClientID: "console-app"}, {Name: "dbeaver", ClientID: "dbeaver-app"}},
		Administrators: "hugr-admins",
		ResourceGroups: []FirstPolicyGroup{{Name: "bi"}, {Name: "etl", Default: true}},
	}
}

// TestFirstPolicyValidate: the orchestrator never serves a first policy the node would refuse (hugr-node spec 015)
func TestFirstPolicyValidate(t *testing.T) {
	d := validDocument()
	d.FirstPolicy = validFirstPolicy()
	if err := d.Validate(); err != nil {
		t.Fatalf("a complete first policy is refused: %v", err)
	}
	cases := map[string]func(*FirstPolicy){
		"an issuer name with a dot":    func(p *FirstPolicy) { p.Issuer.Name = "corp.x" },
		"an issuer over http":          func(p *FirstPolicy) { p.Issuer.URL = "http://login.example" },
		"an issuer's userinfo":         func(p *FirstPolicy) { p.Issuer.URL = "https://u:p@login.example" },
		"no roles claim":               func(p *FirstPolicy) { p.Issuer.RolesClaim = "" },
		"no audience":                  func(p *FirstPolicy) { p.Issuer.Audiences = nil },
		"an empty audience":            func(p *FirstPolicy) { p.Issuer.Audiences = append(p.Issuer.Audiences, "") },
		"no client":                    func(p *FirstPolicy) { p.Clients = nil },
		"a client without its id":      func(p *FirstPolicy) { p.Clients[1].ClientID = "" },
		"a client named twice":         func(p *FirstPolicy) { p.Clients[1].Name = "console" },
		"an application twice":         func(p *FirstPolicy) { p.Clients[1].ClientID = "console-app" },
		"a client name with a quote":   func(p *FirstPolicy) { p.Clients[1].Name = `db"x` },
		"no administrators":            func(p *FirstPolicy) { p.Administrators = "" },
		"two defaults":                 func(p *FirstPolicy) { p.ResourceGroups[0].Default = true },
		"a group twice":                func(p *FirstPolicy) { p.ResourceGroups[0].Name = "etl" },
		"a group name with a space":    func(p *FirstPolicy) { p.ResourceGroups[0].Name = "b i" },
		"not the node's own group":     func(p *FirstPolicy) { p.ResourceGroups = p.ResourceGroups[:1] },
		"the node's group by its case": func(p *FirstPolicy) { p.ResourceGroups[1].Name = "ETL" },
	}
	for name, change := range cases {
		d := validDocument()
		d.FirstPolicy = validFirstPolicy()
		change(d.FirstPolicy)
		if err := d.Validate(); err == nil {
			t.Errorf("%s: served although the node would refuse it", name)
		}
	}
}

const report = `{"node_id": "hugr-node-etl-a", "image": "v0.1.0", "duckdb": "f8f3f4d", "unknown": [1, 2],
 "config": {"target": 12, "applied": 11, "needs_restart": true,
            "pending": [{"kind": "setting", "name": "memory_limit", "class": "restart", "error": ""}]},
 "drift": {"same": 4, "cut": 1},
 "load": {"draining": false, "group": "etl", "group_known": true, "group_default": true, "sessions": {"live": 7, "max": 1000},
          "quack": {"doors": [{"uri": "quack:0.0.0.0:9494", "seats_left": 9}, {"seats_left": 3}]},
          "admit": {"new_session": true}}}`

func TestReadReport(t *testing.T) {
	r, load, err := ReadReport(strings.NewReader(report), "hugr-node-etl-a")
	if err != nil {
		t.Fatalf("the node's own report is refused: %v", err)
	}
	if r.Config.Target != 12 || r.Config.Applied != 11 || !r.Config.NeedsRestart || len(r.Config.Pending) != 1 {
		t.Errorf("the config is read wrong: %+v", r.Config)
	}
	s := SummaryOf(r, load)
	if s.Sessions != 7 || s.SeatsLeft != 12 || !s.AdmitSession || s.Draining || s.GroupKnown == nil || !*s.GroupKnown ||
		!s.GroupDefault || s.Group != "etl" {
		t.Errorf("the summary is wrong: %+v", s)
	}
	if _, _, err := ReadReport(strings.NewReader(report), "hugr-node-etl-b"); !errors.Is(err, ErrForeignNode) {
		t.Errorf("a report for another node: %v, want ErrForeignNode", err)
	}
	big := `{"node_id": "x", "image": "` + strings.Repeat("a", MaxReportBytes) + `"}`
	if _, _, err := ReadReport(strings.NewReader(big), "x"); !errors.Is(err, ErrTooLarge) {
		t.Errorf("a report past 256 KiB: %v, want ErrTooLarge", err)
	}
	if _, _, err := ReadReport(strings.NewReader("not json"), "x"); err == nil {
		t.Error("a body that is no JSON is accepted")
	}
}

func TestSummaryAnnotations(t *testing.T) {
	r, load, _ := ReadReport(strings.NewReader(report), "hugr-node-etl-a")
	s := SummaryOf(r, load)
	back := ParseSummary(s.Annotations())
	if back.Target != s.Target || back.Applied != s.Applied || back.NeedsRestart != s.NeedsRestart ||
		back.AdmitSession != s.AdmitSession || back.Draining != s.Draining || back.Sessions != s.Sessions ||
		back.SeatsLeft != s.SeatsLeft || back.GroupKnown == nil || *back.GroupKnown != *s.GroupKnown ||
		back.GroupDefault != s.GroupDefault || back.Group != s.Group {
		t.Errorf("the summary does not survive its annotations: %+v -> %+v", s, back)
	}
	none := ParseSummary(nil)
	if none.AdmitSession || none.GroupKnown != nil || none.GroupDefault || none.Group != "" || none.SeatsLeft != -1 {
		t.Errorf("a node that never reported admits somebody: %+v", none)
	}
	noQuack := SummaryOf(Report{}, Load{})
	if noQuack.SeatsLeft != -1 {
		t.Errorf("a node without a quack door has seats: %d", noQuack.SeatsLeft)
	}
}

// spec 008: what a node is (hugr-node spec 017's fields) survives the Lease, and an older node's report says none of it
func TestSummaryBuild(t *testing.T) {
	body := `{"node_id": "n", "image": "v0.3.0", "duckdb": "d54cbbe", "duckdb_version": "v2.0.0", "platform": "linux_arm64",
	  "catalog": {"type": "postgres", "extension": "postgres_scanner", "build": 23, "build_min_reader": 22, "schema": 22,
	  "min_reader": 21, "mode": "refused"}}`
	r, load, err := ReadReport(strings.NewReader(body), "n")
	if err != nil {
		t.Fatal(err)
	}
	s := SummaryOf(r, load)
	if s.Build != "v0.3.0" || s.DuckDBVersion != "v2.0.0" || s.Platform != "linux_arm64" ||
		s.CatalogExtension != "postgres_scanner" || s.CatalogBuild != 23 || s.CatalogBuildMinReader != 22 ||
		s.CatalogSchema != 22 || s.CatalogMinReader != 21 {
		t.Errorf("the build is read wrong: %+v", s)
	}
	if back := ParseSummary(s.Annotations()); back.Build != s.Build || back.DuckDBVersion != s.DuckDBVersion ||
		back.Platform != s.Platform || back.CatalogExtension != s.CatalogExtension || back.CatalogBuild != s.CatalogBuild ||
		back.CatalogBuildMinReader != s.CatalogBuildMinReader || back.CatalogSchema != s.CatalogSchema ||
		back.CatalogMinReader != s.CatalogMinReader {
		t.Errorf("the build does not survive its annotations: %+v -> %+v", s, back)
	}
	older := SummaryOf(Report{}, Load{}).Annotations()
	for _, key := range ReportedKeys {
		if _, ok := older[key]; ok {
			t.Errorf("a report that says nothing of %s writes it", key)
		}
	}
	long := SummaryOf(Report{Image: strings.Repeat("x", 4096)}, Load{})
	if len(long.Build) != maxReported {
		t.Errorf("a node's build of 4096 bytes is kept at %d", len(long.Build))
	}
	if cut := SummaryOf(Report{Image: strings.Repeat("a", maxReported-1) + "é"}, Load{}); !utf8.ValidString(cut.Build) {
		t.Errorf("a cut through a rune is kept as it is: %q", cut.Build)
	}
}

func TestPlaced(t *testing.T) {
	s := Summary{Group: "etl", AdmitSession: true, GroupDefault: true}
	if p := s.Placed("etl"); !p.AdmitSession || !p.GroupDefault || p.Group != "etl" {
		t.Errorf("a node serving its own group: %+v", p)
	}
	if p := s.Placed("interactive"); p.AdmitSession || p.GroupDefault || p.Group != "interactive" {
		t.Errorf("a node reporting another group than the operator gave it: %+v", p)
	}
}

func TestSummaryScalingLoad(t *testing.T) {
	// spec 011: the load the autoscaler reads, from duckdb-acl spec 175's report (and 079's seats, 080's streams)
	const loaded = `{"node_id": "n", "load": {"sessions": {"live": 0, "max": 1000, "idle_since": 1763546400},
	  "memory": {"used": 900, "working": 400, "limit": 1000, "temp_used": 50, "temp_limit": null, "critical": false},
	  "quack": {"seats": 16, "doors": [{"seated": 4, "seats_left": 12}, {"seated": 12, "seats_left": 4}]},
	  "streams": {"queued": 2}}}`
	r, load, err := ReadReport(strings.NewReader(loaded), "n")
	if err != nil {
		t.Fatal(err)
	}
	s := SummaryOf(r, load)
	if s.MemoryUsed != 0.4 || s.TempUsed != -1 || s.SeatsUsed != 0.5 || s.StreamsWaiting != 2 ||
		s.IdleSince != 1763546400 || s.SessionsMax != 1000 {
		t.Errorf("working memory against the limit, no spill limit no share, seats over both doors: %+v", s)
	}
	back := ParseSummary(s.Annotations())
	if back.MemoryUsed != 0.4 || back.TempUsed != -1 || back.SeatsUsed != 0.5 || back.StreamsWaiting != 2 ||
		back.IdleSince != 1763546400 || back.SessionsMax != 1000 {
		t.Errorf("the load does not survive the Lease: %+v", back)
	}
	if _, kept := s.Annotations()[AnnotationTempUsed]; kept {
		t.Error("a share with no limit is not written")
	}
	// a live node, and a node of an older pin: nothing to read
	old := SummaryOf(Report{}, Load{})
	if old.MemoryUsed != -1 || old.TempUsed != -1 || old.SeatsUsed != -1 || old.IdleSince != 0 {
		t.Errorf("an older node's report says nothing of its load: %+v", old)
	}
	if none := ParseSummary(nil); none.MemoryUsed != -1 || none.IdleSince != 0 || none.SeatsUsed != -1 {
		t.Errorf("a Lease without the keys: %+v", none)
	}
	for _, key := range []string{AnnotationMemoryUsed, AnnotationIdleSince, AnnotationSeatsUsed} {
		if !slices.Contains(ReportedKeys, key) {
			t.Errorf("%s is dropped from the Lease once a report no longer says it", key)
		}
	}
}

// spec 015: what fails on the node - its pending items of class error, never another class - survives the Lease,
// capped, each field cut to a node's own string; a node that reports none writes no annotation
func TestSummaryFailing(t *testing.T) {
	body := `{"node_id": "n", "config": {"target": 3, "applied": 3, "pending": [
	  {"kind": "script", "name": "20_after", "class": "error", "error": "script 20_after failed (Catalog)"},
	  {"kind": "source", "name": "lake", "class": "error", "error": "script 10_views failed (IO Error)"},
	  {"kind": "extension", "name": "spatial", "class": "restart"},
	  {"kind": "source", "name": "crm", "class": "hot", "error": "Connection Error"}]}}`
	r, load, err := ReadReport(strings.NewReader(body), "n")
	if err != nil {
		t.Fatal(err)
	}
	s := SummaryOf(r, load)
	want := []Failing{{Kind: "script", Name: "20_after", Reason: "script 20_after failed (Catalog)"},
		{Kind: "source", Name: "lake", Reason: "script 10_views failed (IO Error)"}}
	if !reflect.DeepEqual(s.Failing, want) {
		t.Errorf("failing %+v, want %+v", s.Failing, want)
	}
	if back := ParseSummary(s.Annotations()); !reflect.DeepEqual(back.Failing, want) {
		t.Errorf("the failing items do not survive the Lease: %+v", back.Failing)
	}
	var many Report
	for i := 0; i < 2*MaxFailing; i++ {
		many.Config.Pending = append(many.Config.Pending, Pending{Kind: "script", Name: strings.Repeat("n", 300),
			Class: "error", Error: strings.Repeat("e", 300)})
	}
	capped := SummaryOf(many, Load{})
	if len(capped.Failing) != MaxFailing || len(capped.Failing[0].Name) != maxReported ||
		len(capped.Failing[0].Reason) != maxReported {
		t.Errorf("a node's long list: %d items, a name of %d", len(capped.Failing), len(capped.Failing[0].Name))
	}
	if _, ok := SummaryOf(Report{}, Load{}).Annotations()[AnnotationFailing]; ok {
		t.Error("a node with nothing failing writes the annotation")
	}
	if got := ParseSummary(map[string]string{AnnotationFailing: "not json"}); got.Failing != nil {
		t.Errorf("an unreadable annotation reads as items: %+v", got.Failing)
	}
}
