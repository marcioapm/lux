package shim

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/spec"
)

func TestServiceProxy(t *testing.T) {
	var got *http.Request
	var gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		if r.URL.Path == "/base/stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			for i := range 3 {
				io.WriteString(w, "data: "+string(rune('a'+i))+"\n\n")
				w.(http.Flusher).Flush()
				time.Sleep(50 * time.Millisecond)
			}
			return
		}
		io.WriteString(w, "ok")
	}))
	defer up.Close()

	svc := spec.Service{Name: "tools", URL: up.URL + "/base", Headers: []spec.MCPHeader{{Name: "Authorization", Secret: "TOK"}}}
	h, err := newServiceProxy(svc, map[string]string{"TOK": "Bearer s3cret"}, NewRedactor(map[string]string{"TOK": "Bearer s3cret"}))
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(h)
	defer proxy.Close()

	// The workload's own Authorization is replaced; path and query joined.
	req, _ := http.NewRequest("POST", proxy.URL+"/items?x=1", strings.NewReader(`{"a":1}`))
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got.Header.Get("Authorization") != "Bearer s3cret" || len(got.Header.Values("Authorization")) != 1 {
		t.Fatalf("auth %q", got.Header.Values("Authorization"))
	}
	if got.URL.Path != "/base/items" || got.URL.RawQuery != "x=1" || got.Method != "POST" || gotBody != `{"a":1}` {
		t.Fatalf("forwarded %s %s?%s %q", got.Method, got.URL.Path, got.URL.RawQuery, gotBody)
	}

	// Streamed: each event arrives before the next is sent.
	resp, err = http.Get(proxy.URL + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(resp.Body)
	start, n := time.Now(), 0
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") {
			n++
			if n == 1 && time.Since(start) > 80*time.Millisecond {
				t.Fatal("the first event was held back")
			}
		}
	}
	resp.Body.Close()
	if n != 3 {
		t.Fatalf("%d events", n)
	}
}

func TestServiceProxyUnreachable(t *testing.T) {
	svc := spec.Service{Name: "gone", URL: "http://127.0.0.1:1", Headers: []spec.MCPHeader{{Name: "Authorization", Secret: "TOK"}}}
	h, _ := newServiceProxy(svc, map[string]string{"TOK": "Bearer s3cret"}, NewRedactor(map[string]string{"TOK": "Bearer s3cret"}))
	proxy := httptest.NewServer(h)
	defer proxy.Close()
	resp, err := http.Get(proxy.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(b), "service gone unreachable") || strings.Contains(string(b), "s3cret") {
		t.Fatalf("%d %q", resp.StatusCode, b)
	}
}

func TestServiceEnv(t *testing.T) {
	k, v := ServiceEnv("my-tools")
	if k != "LUX_SERVICE_MY_TOOLS" || v != "unix:/.lux/services/my-tools.sock" {
		t.Fatalf("%s=%s", k, v)
	}
}
