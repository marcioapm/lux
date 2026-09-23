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
	if err := s.Normalize(); err != nil {
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
	err := s.Normalize()
	if err == nil || !strings.Contains(err.Error(), "/home/agent/.claude") {
		t.Fatalf("want state path error, got %v", err)
	}
}

func TestAllProblemsAtOnce(t *testing.T) {
	s := RunSpec{Workload: Workload{Adapter: "nope"}, Env: map[string]string{"LUX_X": "1"}}
	err := s.Normalize()
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
