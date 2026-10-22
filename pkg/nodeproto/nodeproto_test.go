package nodeproto

import (
	"errors"
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
