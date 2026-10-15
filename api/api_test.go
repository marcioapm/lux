package api_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/marcioapm/lux/api"
)

// A client binary that imports api must not take in luxd's dependencies.
func TestDependencyClosureHasNoServerDependencies(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/marcioapm/lux/api").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	deps := strings.Fields(string(out))
	if len(deps) < 10 {
		t.Fatalf("go list -deps listed %d packages: %q", len(deps), out)
	}
	forbidden := []string{
		"github.com/jackc/pgx",
		"github.com/aws/aws-sdk-go-v2",
		"github.com/aws/smithy-go",
		"github.com/danielgtaylor/huma",
		"github.com/coreos/go-oidc",
		"github.com/marcioapm/lux/internal/server",
		"github.com/marcioapm/lux/internal/store",
	}
	for _, d := range deps {
		for _, f := range forbidden {
			if d == f || strings.HasPrefix(d, f+"/") {
				t.Errorf("api depends on %s", d)
			}
		}
	}
}

// The aliases are lux's own types: a value of one is a value of the other.
func TestTypesAreLuxs(t *testing.T) {
	sp := api.RunSpec{Image: api.Image{Ref: "alpine"}}
	sp.Workload.TTY = true
	open := api.StreamOpen{Kind: "attach", Restore: true, Rows: 24, Cols: 80}
	if !open.Restore || sp.Image.Ref != "alpine" {
		t.Fatal("fields not set")
	}
	c := api.FromConfig(api.Config{URL: "https://lux.example.com/", APIKey: "k", Tenant: "acme"})
	if c.Base != "https://lux.example.com" || c.Key != "k" || c.Tenant != "acme" {
		t.Fatalf("client %+v", c)
	}
}
