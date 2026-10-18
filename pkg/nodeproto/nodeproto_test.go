package nodeproto

import (
	"errors"
	"strings"
	"testing"
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

const report = `{"node_id": "hugr-node-etl-a", "image": "v0.1.0", "duckdb": "f8f3f4d", "unknown": [1, 2],
 "config": {"target": 12, "applied": 11, "needs_restart": true,
            "pending": [{"kind": "setting", "name": "memory_limit", "class": "restart", "error": ""}]},
 "drift": {"same": 4, "cut": 1},
 "load": {"draining": false, "group_known": true, "sessions": {"live": 7, "max": 1000},
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
	if s.Sessions != 7 || s.SeatsLeft != 12 || !s.AdmitSession || s.Draining || s.GroupKnown == nil || !*s.GroupKnown {
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
		back.SeatsLeft != s.SeatsLeft || back.GroupKnown == nil || *back.GroupKnown != *s.GroupKnown {
		t.Errorf("the summary does not survive its annotations: %+v -> %+v", s, back)
	}
	none := ParseSummary(nil)
	if none.AdmitSession || none.GroupKnown != nil || none.SeatsLeft != -1 {
		t.Errorf("a node that never reported admits somebody: %+v", none)
	}
	noQuack := SummaryOf(Report{}, Load{})
	if noQuack.SeatsLeft != -1 {
		t.Errorf("a node without a quack door has seats: %d", noQuack.SeatsLeft)
	}
}
