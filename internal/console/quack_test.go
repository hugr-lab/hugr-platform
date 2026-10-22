package console

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestQuackDoor(t *testing.T) {
	var got struct {
		path, auth, body string
		calls            int
	}
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.calls++
		got.path, got.auth = r.URL.Path, r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		got.body = string(b)
		_, _ = w.Write([]byte("quack answer"))
	}))
	defer front.Close()
	h := newHarness(t, observeInfo, node("hugr-node-etl-a", "etl"))
	h.server.Quack = QuackMount(func(context.Context) (*url.URL, error) { return url.Parse(front.URL) }, nil)
	h.http = h.server.Handler()

	// a person signed in to the console reaches the door; the console's token stays with the console
	code, _, raw := h.do(t, "POST", "/quack", opsToken, "a quack message")
	if code != 200 || raw != "quack answer" || got.path != "/quack" || got.auth != "" || got.body != "a quack message" {
		t.Fatalf("the door through the console: %d %q, front saw %+v", code, raw, got)
	}
	// nobody signed in: never forwarded
	if code, _, _ := h.do(t, "POST", "/quack", "", "x"); code != http.StatusUnauthorized || got.calls != 1 {
		t.Errorf("an anonymous request: %d, forwarded %d", code, got.calls)
	}
	if code, _, _ := h.do(t, "GET", "/.well-known/quack-auth", opsToken, ""); code != 200 || got.path != "/.well-known/quack-auth" {
		t.Errorf("the door's discovery: %d %s", code, got.path)
	}
	// a path the door has not is not the door's
	if code, _, _ := h.do(t, "POST", "/quack/../api", opsToken, "x"); code == 200 && got.path != "/quack" {
		t.Errorf("a path past the door: %d %s", code, got.path)
	}
	// no front: said, nothing forwarded
	calls := got.calls
	h.server.Quack = QuackMount(func(context.Context) (*url.URL, error) { return nil, io.EOF }, nil)
	if code, _, _ := h.do(t, "POST", "/quack", opsToken, "x"); code != http.StatusServiceUnavailable || got.calls != calls {
		t.Errorf("no front: %d", code)
	}
	h.server.Quack = nil
	if code, out, _ := h.do(t, "POST", "/quack", opsToken, "x"); code != 404 || !strings.Contains(out["message"].(string), "quack") {
		t.Errorf("no door: %d %v", code, out)
	}
}
