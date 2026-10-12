package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// listedPool is one pool of GET /v1/pools as JSON, by name.
func listedPool(t *testing.T, s *Server, key, name string) map[string]any {
	t.Helper()
	w := apiCall(t, s, key, http.MethodGet, "/v1/pools", nil)
	var body struct {
		Pools []map[string]any `json:"pools"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatalf("GET /v1/pools: %d %s", w.Code, w.Body)
	}
	for _, p := range body.Pools {
		if p["name"] == name {
			return p
		}
	}
	t.Fatalf("no pool %s in %s", name, w.Body)
	return nil
}

// GET /v1/pools says each pool's platforms and whether it offers nested
// containers, from the facts a resume into it is checked against: its
// hosts' arch and nested registration, an ec2 template's instance types in
// the catalog and its nestedContainers. Unknown: platforms absent,
// nestedContainers null.
func TestPoolsListPlatformsAndCapabilities(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	_, _ = movedRun(t, s)
	addCatalog(t, s)
	execSQL(t, s, ctx, `UPDATE hosts SET arch = 'arm64', labels = '{"nested":"true"}' WHERE id = 'hb'`)
	poolID(t, s, "empty", "t1")
	ec2Pool(t, s, "graviton", `{"region":"us-east-1","instanceType":"m7g.4xlarge"}`)
	ec2Pool(t, s, "nest", `{"region":"us-east-1","nestedContainers":true}`)
	ec2Pool(t, s, "odd", `{"region":"us-east-1","instanceType":"x9.huge","fallbackInstanceTypes":["m7i.4xlarge"]}`)
	key := apiKey(t, s, new("t1"), "read")
	for _, c := range []struct {
		pool      string
		platforms any
		nested    any
	}{
		{"big", []any{"linux/arm64"}, true},       // its host: arm64, --nested
		{"default", []any{"linux/amd64"}, false},  // hosts ha (no arch: unknown) and hc (amd64), neither nested
		{"empty", nil, nil},                       // static, no host: nothing known
		{"graviton", []any{"linux/arm64"}, false}, // the catalog's m7g; template without nestedContainers
		{"nest", nil, true},                       // no instanceType: platform unknown; the template opts in
		{"odd", []any{"linux/amd64"}, false},      // the catalog knows one of its two types
	} {
		p := listedPool(t, s, key, c.pool)
		caps, _ := p["capabilities"].(map[string]any)
		if got := p["platforms"]; jsonText(got) != jsonText(c.platforms) {
			t.Errorf("%s: platforms %v, want %v", c.pool, got, c.platforms)
		}
		if got, present := caps["nestedContainers"]; !present || got != c.nested {
			t.Errorf("%s: capabilities %v, want nestedContainers %v", c.pool, caps, c.nested)
		}
	}
}

func jsonText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
