package quackproxy

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
	"github.com/hugr-lab/hugr-platform/internal/front/quackwire"
	"github.com/hugr-lab/hugr-platform/pkg/nodeproto"
)

func corpus(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "quackwire", "testdata", name+".bin"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// errorAnswer is an ERROR answer as a node writes one (header: type 100, its end; body: field 1, the message)
func errorAnswer(text string) []byte {
	b := []byte{0x01, 0x00, 0x64, 0xff, 0xff, 0x01, 0x00}
	n := len(text)
	for n >= 0x80 {
		b = append(b, byte(n)|0x80)
		n >>= 7
	}
	b = append(b, byte(n))
	return append(append(b, text...), 0xff, 0xff)
}

type got struct {
	body   []byte
	header http.Header
}

type fakeNode struct {
	label  string
	srv    *httptest.Server
	answer func(w http.ResponseWriter, body []byte)
	mu     sync.Mutex
	got    []got
}

func (n *fakeNode) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.got)
}

func startNode(t *testing.T, label string, answer func(w http.ResponseWriter, body []byte)) *fakeNode {
	n := &fakeNode{label: label, answer: answer}
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		n.mu.Lock()
		n.got = append(n.got, got{body: body, header: r.Header.Clone()})
		n.mu.Unlock()
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"issuer": "` + label + `"}`))
			return
		}
		n.answer(w, body)
	}))
	t.Cleanup(n.srv.Close)
	return n
}

func ok(label string) func(http.ResponseWriter, []byte) {
	return func(w http.ResponseWriter, _ []byte) {
		w.Header().Add("Set-Cookie", "acl_node="+label+"; HttpOnly; Path=/; SameSite=Strict")
		_, _ = w.Write([]byte("answer of " + label))
	}
}

func wrongGroup(groups string) func(http.ResponseWriter, []byte) {
	return func(w http.ResponseWriter, _ []byte) {
		w.Header().Set("x-acl-resource-groups", groups)
		_, _ = w.Write(errorAnswer(`Authentication callback failed: acl: this node serves resource group "x"`))
	}
}

func full(w http.ResponseWriter, _ []byte) {
	_, _ = w.Write(errorAnswer("acl: node at capacity - 16 of 16 quack clients seated (...); try another node"))
}

func member(n *fakeNode, group string, dflt, admit bool) fleet.Node {
	u, _ := url.Parse(n.srv.URL)
	host, port, _ := net.SplitHostPort(u.Host)
	p, _ := strconv.Atoi(port)
	return fleet.Node{Label: n.label, Service: "hugr-node-x", Address: host, FlightPort: 1, QuackPort: int32(p),
		Ready: true, Serving: true, Lease: &fleet.Lease{Until: time.Now().Add(time.Hour), Summary: nodeproto.Summary{
			Group: group, GroupDefault: dflt, AdmitSession: admit, SeatsLeft: 10}}}
}

func start(t *testing.T, nodes ...fleet.Node) (*fleet.Fleet, string) {
	f := fleet.New()
	f.Set(fleet.NewView(nodes))
	p := &Proxy{Fleet: f, Namespace: "hugr", NoNodeWait: 200 * time.Millisecond, Insecure: true}
	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)
	return f, srv.URL
}

// a client that adds no header of its own (Go's would ask for gzip), as a quack client
var client = &http.Client{Transport: &http.Transport{DisableCompression: true}}

func post(t *testing.T, front string, body []byte, headers ...string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, front+"/quack", bytes.NewReader(body))
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Add(headers[i], headers[i+1])
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(resp.Body)
	return resp, string(answer)
}

func TestConnectSeatedThenRouted(t *testing.T) {
	a := startNode(t, "node-a", ok("node-a"))
	b := startNode(t, "node-b", ok("node-b"))
	f, front := start(t, member(a, "general", true, true), member(b, "etl", false, true))
	connect := corpus(t, "request_connect")
	resp, answer := post(t, front, connect)
	if answer != "answer of node-a" || !strings.HasPrefix(resp.Header.Get("Set-Cookie"), "acl_node=node-a") {
		t.Fatalf("a CONNECT is seated on the default group's node: %q %v", answer, resp.Header)
	}
	if !bytes.Equal(a.got[0].body, connect) {
		t.Error("the CONNECT reached the node changed")
	}
	// the corpus' PREPARE names node-a in its own header: routed there, even with node-a admitting nobody new
	f.Set(fleet.NewView([]fleet.Node{member(a, "general", true, false), member(b, "etl", false, true)}))
	prepare := corpus(t, "request_prepare")
	if _, answer := post(t, front, prepare); answer != "answer of node-a" {
		t.Errorf("a PREPARE by the label in its body: %q", answer)
	}
	if !bytes.Equal(a.got[1].body, prepare) {
		t.Error("the PREPARE reached the node changed")
	}
	for k := range a.got[1].header {
		if strings.HasPrefix(k, "X-Forwarded") || k == "Accept-Encoding" {
			t.Errorf("the front added %s", k)
		}
	}
	// the header beats the body, the cookie beats the header
	if _, answer := post(t, front, prepare, ConnectionIDHeader, "node-b.ABC"); answer != "answer of node-b" {
		t.Errorf("X-Quack-Connection-Id: %q", answer)
	}
	if _, answer := post(t, front, prepare, "Cookie", "acl_node=node-a", ConnectionIDHeader, "node-b.ABC"); answer != "answer of node-a" {
		t.Errorf("the cookie: %q", answer)
	}
}

func TestConnectReseats(t *testing.T) {
	a := startNode(t, "node-a", wrongGroup("etl"))
	b := startNode(t, "node-b", full)
	c := startNode(t, "node-c", ok("node-c"))
	_, front := start(t, member(a, "general", true, true), member(b, "etl", false, true), member(c, "etl", false, true))
	for i := 0; i < 5; i++ {
		_, answer := post(t, front, corpus(t, "request_connect"))
		if answer != "answer of node-c" {
			t.Fatalf("seated in the group the node named, past the full node: %q", answer)
		}
	}
	// a refusal past the budget is the client's, as the node answered it
	d := startNode(t, "node-d", wrongGroup("g2"))
	e := startNode(t, "node-e", wrongGroup("g3"))
	g := startNode(t, "node-g", wrongGroup("g4"))
	_, front2 := start(t, member(d, "g1", true, true), member(e, "g2", false, true), member(g, "g3", false, true))
	resp, answer := post(t, front2, corpus(t, "request_connect"))
	if text, ok := quackwire.ErrorText([]byte(answer)); !ok || !strings.Contains(text, "acl: this node serves") ||
		resp.Header.Get("x-acl-resource-groups") != "g4" {
		t.Errorf("the third refusal goes to the client: %q %v", text, resp.Header)
	}
}

func TestUnknownLabelAndNoNode(t *testing.T) {
	stale := startNode(t, "node-a", func(w http.ResponseWriter, _ []byte) {
		_, _ = w.Write(errorAnswer("acl: this connection is not known on this node - connect again"))
	})
	_, front := start(t, member(stale, "general", true, true))
	// node-z is no node the front sees: any node of the default group answers that it does not know it
	_, answer := post(t, front, corpus(t, "request_prepare"), ConnectionIDHeader, "node-z.ABC")
	if text, _ := quackwire.ErrorText([]byte(answer)); !strings.Contains(text, "connect again") {
		t.Errorf("an unknown label: %q", answer)
	}
	if resp, answer := post(t, front, []byte("GET / HTTP/1.1")); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("not a quack message: %d %q", resp.StatusCode, answer)
	}
	busy := startNode(t, "node-b", ok("node-b"))
	_, front2 := start(t, member(busy, "general", true, false))
	started := time.Now()
	resp, answer := post(t, front2, corpus(t, "request_connect"))
	if resp.StatusCode != http.StatusServiceUnavailable ||
		!strings.Contains(answer, "hugr: no node of group general can take a new session - retry") {
		t.Errorf("no placeable node: %d %q", resp.StatusCode, answer)
	}
	if time.Since(started) < 200*time.Millisecond || busy.count() != 0 {
		t.Error("the CONNECT did not wait, or went to a node that admits nobody")
	}
}

func TestUnreachableAndDiscovery(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	dead := fleet.Node{Label: "node-dead", Service: "hugr-node-x", Address: "127.0.0.1", QuackPort: int32(port),
		Ready: true, Serving: true, Lease: &fleet.Lease{Until: time.Now().Add(time.Hour),
			Summary: nodeproto.Summary{Group: "general", GroupDefault: true, AdmitSession: true, SeatsLeft: 99}}}
	b := startNode(t, "node-b", ok("node-b"))
	_, front := start(t, dead, member(b, "general", true, true))
	for i := 0; i < 5; i++ {
		if _, answer := post(t, front, corpus(t, "request_connect")); answer != "answer of node-b" {
			t.Fatalf("a node that is gone: %q", answer)
		}
	}
	resp, err := http.Get(front + "/.well-known/quack-auth")
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(doc) != `{"issuer": "node-b"}` {
		t.Errorf("the discovery document from a node of the default group: %q", doc)
	}
}

func TestEveryNodeSilent(t *testing.T) {
	var nodes []fleet.Node
	for _, label := range []string{"node-x", "node-y"} {
		l, _ := net.Listen("tcp", "127.0.0.1:0")
		port := l.Addr().(*net.TCPAddr).Port
		_ = l.Close()
		nodes = append(nodes, fleet.Node{Label: label, Service: "hugr-node-x", Address: "127.0.0.1", QuackPort: int32(port),
			Ready: true, Serving: true, Lease: &fleet.Lease{Until: time.Now().Add(time.Hour),
				Summary: nodeproto.Summary{AdmitSession: true, SeatsLeft: 1}}})
	}
	_, front := start(t, nodes...)
	resp, answer := post(t, front, corpus(t, "request_connect"))
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(answer, "hugr: the node did not answer") {
		t.Errorf("nodes that do not answer are not 'no node': %d %q", resp.StatusCode, answer)
	}
}
