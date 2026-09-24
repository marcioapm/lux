package spec

import (
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

const example = `
name: fix
image: { ref: alpine }
workload:
  adapter: claude-code
  prompt: hi
secrets:
  - { name: ANTHROPIC_API_KEY, value: sk-1 }
  - { name: GITHUB_TOKEN, value: ghp }
git:
  repositories:
    - { name: api, url: https://example.com/a.git, credential: GITHUB_TOKEN }
volumes:
  - { name: workspace, path: /workspace }
  - { name: home, path: /home/agent, kind: state }
resources: { cpus: 1.5, memory: 512Mi }
timeout: 90m
`

func TestNormalizeExample(t *testing.T) {
	var s RunSpec
	if err := yaml.Unmarshal([]byte(example), &s); err != nil {
		t.Fatal(err)
	}
	if err := s.Normalize(BuiltinDefaults); err != nil {
		t.Fatal(err)
	}
	if s.Resources.Memory != 512<<20 || s.Timeout.Duration != 90*time.Minute || s.Resources.CPUs != 1.5 {
		t.Fatalf("parsed %+v %v", s.Resources, s.Timeout)
	}
	if s.Git.Repositories[0].Path != "/workspace/repos/api" || s.Git.Repositories[0].Ref != "HEAD" {
		t.Fatalf("repo defaults: %+v", s.Git.Repositories[0])
	}
	if s.Volumes[0].Kind != "state" {
		t.Fatal("volume kind default")
	}
	for _, sec := range s.Secrets {
		if sec.Name == "GITHUB_TOKEN" && !sec.RunnerOnly {
			t.Fatal("git credential must be runner-only")
		}
	}
	stripped, refs, vals := s.SplitSecrets()
	for _, sec := range stripped.Secrets {
		if sec.Value != "" {
			t.Fatal("value kept")
		}
	}
	if len(refs) != 2 || vals["ANTHROPIC_API_KEY"] != "sk-1" {
		t.Fatal("split")
	}
}

func TestAdapterStatePathsMustBeOnStateVolume(t *testing.T) {
	s := RunSpec{Image: Image{Ref: "x"}, Workload: Workload{Adapter: "claude-code"}}
	err := s.Normalize(BuiltinDefaults)
	if err == nil || !strings.Contains(err.Error(), "/home/agent/.claude") {
		t.Fatalf("want state path error, got %v", err)
	}
}

func TestAllProblemsAtOnce(t *testing.T) {
	s := RunSpec{Workload: Workload{Adapter: "nope"}, Env: map[string]string{"LUX_X": "1"}}
	err := s.Normalize(BuiltinDefaults)
	ve, ok := err.(*ValidationError)
	if !ok || len(ve.Problems) < 3 {
		t.Fatalf("want several problems, got %v", err)
	}
}

func TestBytes(t *testing.T) {
	for in, want := range map[string]int64{"8Gi": 8 << 30, "1G": 1e9, "100": 100, "1.5Mi": 1.5 * (1 << 20)} {
		var b Bytes
		if err := b.parse(in); err != nil || int64(b) != want {
			t.Errorf("%s: %d %v", in, b, err)
		}
	}
}

func TestGitPathsMustBeOnAStateVolume(t *testing.T) {
	base := func(path string) RunSpec {
		return RunSpec{
			Image:    Image{Ref: "x"},
			Workload: Workload{Command: []string{"true"}},
			Volumes: []Volume{
				{Name: "workspace", Path: "/workspace", Kind: "state"},
				{Name: "cache", Path: "/workspace/cache", Kind: "ephemeral"},
			},
			Git: &Git{Repositories: []Repository{{Name: "api", URL: "https://x/a.git", Path: path}}},
		}
	}
	for path, ok := range map[string]bool{
		"/workspace/repos/api": true,
		"/workspace/cache/api": false, // the more specific, ephemeral volume
		"/opt/api":             false, // on no volume
	} {
		s := base(path)
		err := s.Normalize(BuiltinDefaults)
		if (err == nil) != ok {
			t.Errorf("%s: %v", path, err)
		}
	}
}

func TestGitURLWithCredentialsIsRefused(t *testing.T) {
	s := RunSpec{Image: Image{Ref: "x"}, Workload: Workload{Command: []string{"true"}},
		Volumes: []Volume{{Name: "workspace", Path: "/workspace"}},
		Git:     &Git{Repositories: []Repository{{Name: "a", URL: "https://user:tok@x/a.git"}}}}
	if err := s.Normalize(BuiltinDefaults); err == nil || !strings.Contains(err.Error(), "must not carry credentials") {
		t.Fatalf("got %v", err)
	}
}

func TestMCPServers(t *testing.T) {
	tok := []MCPHeader{{Name: "Authorization", Secret: "MCP_TOKEN"}}
	base := func(net Network, servers ...MCPServer) RunSpec {
		return RunSpec{Image: Image{Ref: "x"}, Workload: Workload{Command: []string{"true"}, MCPServers: servers},
			Secrets: []Secret{{Name: "MCP_TOKEN", Value: "Bearer t"}}, Network: net}
	}
	byHost := Network{Egress: []EgressRule{{Host: "mcp.example.com"}, {CIDR: "10.1.0.0/16"}}}
	for _, c := range []struct {
		name    string
		spec    RunSpec
		problem string // "" for valid
	}{
		{"host rule", base(byHost, MCPServer{Name: "a", URL: "https://mcp.example.com/mcp", Headers: tok}), ""},
		{"host rule, case and dot", base(byHost, MCPServer{Name: "a", URL: "https://MCP.example.com./mcp"}), ""},
		{"cidr for an address", base(byHost, MCPServer{Name: "a", URL: "http://10.1.2.3:8080/mcp"}), ""},
		{"unrestricted", base(Network{Unrestricted: true}, MCPServer{Name: "a", URL: "https://anywhere.example/mcp"}), ""},
		{"host not allowed", base(byHost, MCPServer{Name: "a", URL: "https://other.example.com/mcp"}), `does not allow MCP server "a" at other.example.com: add an egress rule`},
		{"address outside the cidr", base(byHost, MCPServer{Name: "a", URL: "http://10.2.0.1/mcp"}), "add an egress rule"},
		{"a name is not matched by a cidr", base(Network{Egress: []EgressRule{{CIDR: "0.0.0.0/0"}}}, MCPServer{Name: "a", URL: "https://x.example/mcp"}), "add an egress rule"},
		{"no rules", base(Network{}, MCPServer{Name: "a", URL: "https://mcp.example.com/mcp"}), "add an egress rule"},
		{"duplicate names", base(byHost, MCPServer{Name: "a", URL: "https://mcp.example.com/"}, MCPServer{Name: "a", URL: "https://mcp.example.com/"}), "duplicate name"},
		{"bad name", base(byHost, MCPServer{Name: "Bad Name", URL: "https://mcp.example.com/"}), "invalid or duplicate name"},
		{"not http", base(byHost, MCPServer{Name: "a", URL: "ftp://mcp.example.com/"}), "http or https"},
		{"no host", base(byHost, MCPServer{Name: "a", URL: "https:///mcp"}), "http or https"},
		{"relative", base(byHost, MCPServer{Name: "a", URL: "/mcp"}), "http or https"},
		{"userinfo", base(byHost, MCPServer{Name: "a", URL: "https://u:p@mcp.example.com/"}), "must not carry credentials"},
		{"unknown secret", base(byHost, MCPServer{Name: "a", URL: "https://mcp.example.com/", Headers: []MCPHeader{{Name: "X-Key", Secret: "NOPE"}}}), `no secret named "NOPE"`},
		{"bad header name", base(byHost, MCPServer{Name: "a", URL: "https://mcp.example.com/", Headers: []MCPHeader{{Name: "Bad Header:", Secret: "MCP_TOKEN"}}}), "invalid header name"},
	} {
		err := c.spec.Normalize(BuiltinDefaults)
		switch {
		case c.problem == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.problem != "" && (err == nil || !strings.Contains(err.Error(), c.problem)):
			t.Errorf("%s: want %q, got %v", c.name, c.problem, err)
		}
	}
}

// An MCP header's secret is not runner-only: it reaches the container
// through the header. So a git credential, which never does, is refused.
func TestMCPHeaderSecretsAndRunnerOnly(t *testing.T) {
	spec := func(secret string) RunSpec {
		return RunSpec{Image: Image{Ref: "x"}, Workload: Workload{Command: []string{"true"},
			MCPServers: []MCPServer{{Name: "a", URL: "https://m.example/", Headers: []MCPHeader{{Name: "Authorization", Secret: secret}}}}},
			Secrets: []Secret{{Name: "MCP", Value: "v"}, {Name: "GIT", Value: "g"}}, Network: Network{Unrestricted: true},
			Volumes: []Volume{{Name: "workspace", Path: "/workspace"}},
			Git:     &Git{Repositories: []Repository{{Name: "r", URL: "https://x/r.git", Credential: "GIT"}}}}
	}
	s := spec("MCP")
	if err := s.Normalize(BuiltinDefaults); err != nil {
		t.Fatal(err)
	}
	if s.Secrets[0].RunnerOnly || !s.Secrets[1].RunnerOnly {
		t.Fatalf("runner-only: %+v", s.Secrets)
	}
	// Used only as a header (or a git credential): not in any process's
	// environment unless the spec asks.
	if s.Secrets[0].As != "none" || s.Secrets[1].As != "none" {
		t.Fatalf("as: %+v", s.Secrets)
	}
	s = spec("MCP")
	s.Secrets[0].As = "env"
	if err := s.Normalize(BuiltinDefaults); err != nil || s.Secrets[0].As != "env" {
		t.Fatalf("an explicit as stands: %v %+v", err, s.Secrets)
	}
	s = spec("MCP")
	s.Workload.MCPServers[0].Headers = append(s.Workload.MCPServers[0].Headers, MCPHeader{Name: "authorization", Secret: "MCP"})
	if err := s.Normalize(BuiltinDefaults); err == nil || !strings.Contains(err.Error(), "duplicate header") {
		t.Fatalf("duplicate header: %v", err)
	}
	s = spec("MCP")
	s.Secrets = append(s.Secrets, Secret{Name: "lux-claude-mcp.json", Value: "x", As: "file", Path: "/x"})
	if err := s.Normalize(BuiltinDefaults); err == nil || !strings.Contains(err.Error(), "prefix is reserved") {
		t.Fatalf("reserved name: %v", err)
	}
	s = spec("GIT")
	if err := s.Normalize(BuiltinDefaults); err == nil || !strings.Contains(err.Error(), "is a git credential") {
		t.Fatalf("got %v", err)
	}
}
