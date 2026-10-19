package route

import (
	"slices"
	"strings"
	"testing"
)

func TestLabels(t *testing.T) {
	for label, ok := range map[string]bool{
		"hugr-node-etl-7d9f8-abcde": true, "a": true, "0": true, strings.Repeat("a", 63): true,
		"": false, strings.Repeat("a", 64): false, "-a": false, "a-": false, "A": false, "a.b": false, "a_b": false,
		"a b": false, "ä": false,
	} {
		if ValidLabel(label) != ok {
			t.Errorf("ValidLabel(%q) = %v", label, !ok)
		}
	}
	for _, c := range []struct {
		want    string
		headers []string
	}{
		{"node-a", []string{"acl_node=node-a"}},
		{"node-b", []string{"acl_flight_session_id=x; acl_node=node-b; acl_session_state=y"}},
		{"node-c", []string{"other=1", " acl_node = node-c "}},  // HTTP/2 splits the cookies into several headers
		{"", []string{"acl_nodes=node-a", "x=acl_node=node-a"}}, // never by a substring
		{"", []string{"acl_node=Node-A"}},                       // not a label: no label at all
	} {
		if got := CookieLabel(c.headers); got != c.want {
			t.Errorf("CookieLabel(%q) = %q, want %q", c.headers, got, c.want)
		}
	}
	for id, want := range map[string]string{
		"node-a.81F1D3A45BF2C561EF0A8F814E546AF0": "node-a", "0212F7B6B30CE4A17B2D9CA90FFC1333": "",
		".x": "", "NODE.x": "", "": "",
	} {
		if got := ConnectionLabel(id); got != want {
			t.Errorf("ConnectionLabel(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestGroups(t *testing.T) {
	// duckdb-acl's ResourceGroupsHeader: each name percent-encoded but RFC 3986's unreserved characters
	if got := Groups("h,k"); !slices.Equal(got, []string{"h", "k"}) {
		t.Errorf("Groups(h,k) = %q", got)
	}
	if got := Groups("a%2Cb,c%20d,x%25y,%C3%A4"); !slices.Equal(got, []string{"a,b", "c d", "x%y", "ä"}) {
		t.Errorf("encoded names: %q", got)
	}
	if got := Groups("ok,%zz,,"); !slices.Equal(got, []string{"ok"}) {
		t.Errorf("a part that does not decode is dropped: %q", got)
	}
}

func TestRefusals(t *testing.T) {
	wrong := `acl: this node serves resource group "etl" - your roles are served by "interactive"`
	if r, g := FlightRefusal(true, wrong, []string{"interactive"}); r != WrongGroup || !slices.Equal(g, []string{"interactive"}) {
		t.Errorf("Flight's wrong group: %v %q", r, g)
	}
	if r, _ := FlightRefusal(true, "acl: node is draining - not accepting new sessions", nil); r != Busy {
		t.Errorf("Flight's drain: %v", r)
	}
	for _, other := range []struct {
		unavailable bool
		message     string
	}{
		{false, wrong}, // another code than UNAVAILABLE is never the node's placement
		{true, "acl: authentication failed"},
		{true, "connection error"},
	} {
		if r, _ := FlightRefusal(other.unavailable, other.message, nil); r != None {
			t.Errorf("FlightRefusal(%v, %q) = %v", other.unavailable, other.message, r)
		}
	}
	if r, g := QuackRefusal("etl,b%2Cc", ""); r != WrongGroup || !slices.Equal(g, []string{"etl", "b,c"}) {
		t.Errorf("quack's wrong group: %v %q", r, g)
	}
	if r, _ := QuackRefusal("", "acl: node at capacity - 16 of 16 quack clients seated (...); try another node"); r != Busy {
		t.Errorf("quack at capacity: %v", r)
	}
	if r, _ := QuackRefusal("", "Authentication failed"); r != None {
		t.Errorf("quack's refused token: %v", r)
	}
}

func TestSeatBudget(t *testing.T) {
	s := NewSeat([]string{"general"})
	if !s.Next("n1", WrongGroup, []string{"etl"}) || !slices.Equal(s.Groups, []string{"etl"}) {
		t.Fatal("the first wrong group is seated again in the group it names")
	}
	if !s.Next("n2", Busy, nil) || !s.Next("n3", Unreachable, nil) {
		t.Fatal("two busy nodes of the group are passed over")
	}
	if s.Next("n4", Busy, nil) {
		t.Error("a third busy node: the answer is the client's")
	}
	if !s.Next("n5", WrongGroup, []string{"other"}) || s.Next("n6", WrongGroup, []string{"third"}) {
		t.Error("two re-seats by group, then the node's refusal goes to the client")
	}
	for _, l := range []string{"n1", "n2", "n3", "n4", "n5", "n6"} {
		if !s.Tried[l] {
			t.Errorf("%s is not remembered as tried", l)
		}
	}
	if NewSeat(nil).Next("n", WrongGroup, nil) {
		t.Error("a wrong group that names no group is the client's")
	}
	if NewSeat(nil).Next("n", None, nil) {
		t.Error("an answer that is not a refusal is the client's")
	}
}
