package quackwire

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The corpus is real traffic: duckdb's quack client and a node of duckdb-acl labelled node-a (testdata/capture).
func corpus(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".bin"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestHeadersOfTheCorpus(t *testing.T) {
	for _, c := range []struct {
		name    string
		typ     Type
		labeled bool
	}{
		{"request_connect", Connect, false},
		{"request_prepare", Prepare, true},
		{"request_fetch", Fetch, true},
		{"request_ack", Acknowledge, true},
		{"request_cancel", Cancel, true},
		{"request_heartbeat", Heartbeat, true},
		{"request_disconnect", Disconnect, true},
		{"answer_connect_answer", ConnectAnswer, true},
		{"answer_error", Error, false},
	} {
		body := corpus(t, c.name)
		h, err := PeekHeader(bufio.NewReader(bytes.NewReader(body)))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if h.Type != c.typ {
			t.Errorf("%s: type %d, want %d", c.name, h.Type, c.typ)
		}
		if got := strings.HasPrefix(h.ConnectionID, "node-a."); got != c.labeled {
			t.Errorf("%s: a connection id %v, labelled %v", c.name, h.ConnectionID != "", got)
		}
	}
}

func TestPeekLeavesTheBody(t *testing.T) {
	body := corpus(t, "request_prepare")
	r := bufio.NewReader(bytes.NewReader(body))
	if _, err := PeekHeader(r); err != nil {
		t.Fatal(err)
	}
	var rest bytes.Buffer
	if _, err := rest.ReadFrom(r); err != nil || !bytes.Equal(rest.Bytes(), body) {
		t.Errorf("the body is not forwarded whole after the peek (%d of %d bytes)", rest.Len(), len(body))
	}
}

func TestErrorText(t *testing.T) {
	if text, ok := ErrorText(corpus(t, "answer_stale_error")); !ok ||
		text != "acl: this connection is not known on this node - connect again" {
		t.Errorf("the node's answer to a connection it does not know: %q %v", text, ok)
	}
	if text, ok := ErrorText(corpus(t, "answer_error")); !ok || !strings.HasPrefix(text, "Authentication") {
		t.Errorf("a refused connect: %q %v", text, ok)
	}
	for _, other := range []string{"answer_connect_answer", "answer_success", "answer_fetch_answer", "request_connect"} {
		if _, ok := ErrorText(corpus(t, other)); ok {
			t.Errorf("%s read as an error answer", other)
		}
	}
}

func TestTruncatedOrHostile(t *testing.T) {
	prepare := corpus(t, "request_prepare")
	for n := 0; n < 3+2+1+len("node-a.")+32; n++ {
		if h, err := ParseHeader(prepare[:n]); err == nil && h.ConnectionID != "" {
			t.Errorf("a body cut at %d bytes gave a connection id %q", n, h.ConnectionID)
		}
	}
	for name, body := range map[string][]byte{
		"empty":             {},
		"another field":     {0x02, 0x00, 0x03},
		"a type past a u8":  {0x01, 0x00, 0x80, 0x02, 0xff, 0xff},
		"a varint too long": append([]byte{0x01, 0x00}, bytes.Repeat([]byte{0xff}, 11)...),
		// a connection id past MaxConnectionID: refused, never read
		"a long id": append([]byte{0x01, 0x00, 0x03, 0x02, 0x00, 0x81, 0x02}, bytes.Repeat([]byte{'a'}, 257)...),
		// a length that runs past the body
		"a short id": {0x01, 0x00, 0x03, 0x02, 0x00, 0x20, 'a', 'b'},
	} {
		if _, err := ParseHeader(body); err == nil {
			t.Errorf("%s: read as a header", name)
		}
	}
	// a header without a connection id that goes on with a field of another revision is still a header
	if h, err := ParseHeader([]byte{0x01, 0x00, 0x03, 0x07, 0x00, 0x01, 0xff, 0xff}); err != nil || h.Type != Prepare ||
		h.ConnectionID != "" {
		t.Errorf("a header with an unknown field after the type: %+v %v", h, err)
	}
}
