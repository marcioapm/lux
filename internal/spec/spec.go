// Package spec is the RunSpec: the immutable description of what a Run
// executes. It is validated and normalized on submission; the normalized form,
// minus secret values, is what luxd stores and what runners receive.
package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type RunSpec struct {
	Name      string            `json:"name,omitempty" yaml:"name,omitempty"`
	Labels    map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
	Image     Image             `json:"image" yaml:"image"`
	Workload  Workload          `json:"workload" yaml:"workload"`
	Init      *Init             `json:"init,omitempty" yaml:"init,omitempty"`
	Env       map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	Secrets   []Secret          `json:"secrets,omitempty" yaml:"secrets,omitempty"`
	Git       *Git              `json:"git,omitempty" yaml:"git,omitempty"`
	Volumes   []Volume          `json:"volumes,omitempty" yaml:"volumes,omitempty"`
	Resources Resources         `json:"resources" yaml:"resources"`
	Timeout   Duration          `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	Placement Placement         `json:"placement" yaml:"placement"`
	Network   Network           `json:"network" yaml:"network"`
	Sandbox   Sandbox           `json:"sandbox" yaml:"sandbox"`
	Artifacts Artifacts         `json:"artifacts" yaml:"artifacts"`
}

// Image is exactly one of a pinned reference or a build.
type Image struct {
	Ref   string `json:"ref,omitempty" yaml:"ref,omitempty"`
	Build *Build `json:"build,omitempty" yaml:"build,omitempty"`
}

type Build struct {
	Containerfile string `json:"containerfile" yaml:"containerfile"`
	// Context is an optional artifact id whose contents are the build context.
	Context string            `json:"context,omitempty" yaml:"context,omitempty"`
	Args    map[string]string `json:"args,omitempty" yaml:"args,omitempty"`
}

type Workload struct {
	// generic | acp | claude-code | codex | opencode
	Adapter string   `json:"adapter" yaml:"adapter"`
	Command []string `json:"command,omitempty" yaml:"command,omitempty"`
	Prompt  string   `json:"prompt,omitempty" yaml:"prompt,omitempty"`
	Workdir string   `json:"workdir,omitempty" yaml:"workdir,omitempty"`
	// User the workload runs as; defaults to the image's user.
	User   string  `json:"user,omitempty" yaml:"user,omitempty"`
	TTY    bool    `json:"tty,omitempty" yaml:"tty,omitempty"`
	Resume *Resume `json:"resume,omitempty" yaml:"resume,omitempty"`
	// Grace is how long a graceful stop waits before SIGKILL.
	Grace Duration `json:"grace,omitempty" yaml:"grace,omitempty"`
}

type Resume struct {
	Command []string `json:"command,omitempty" yaml:"command,omitempty"`
}

type Init struct {
	Script string `json:"script" yaml:"script"`
}

type Secret struct {
	Name  string `json:"name" yaml:"name"`
	Value string `json:"value,omitempty" yaml:"value,omitempty"`
	As    string `json:"as,omitempty" yaml:"as,omitempty"` // env | file
	Path  string `json:"path,omitempty" yaml:"path,omitempty"`
	// Git marks a secret used only by the runner (a git credential): it is
	// never placed in the container.
	RunnerOnly bool `json:"runnerOnly,omitempty" yaml:"-"`
}

type Git struct {
	Repositories []Repository `json:"repositories" yaml:"repositories"`
	Push         *Push        `json:"push,omitempty" yaml:"push,omitempty"`
}

type Repository struct {
	Name       string `json:"name" yaml:"name"`
	URL        string `json:"url" yaml:"url"`
	Ref        string `json:"ref,omitempty" yaml:"ref,omitempty"`
	Credential string `json:"credential,omitempty" yaml:"credential,omitempty"`
	// Path inside the container; defaults to <workspace volume>/repos/<name>.
	Path string `json:"path,omitempty" yaml:"path,omitempty"`
}

type Push struct {
	Branch string `json:"branch" yaml:"branch"`
}

type Volume struct {
	Name string `json:"name" yaml:"name"`
	Path string `json:"path" yaml:"path"`
	Kind string `json:"kind" yaml:"kind"` // state | ephemeral
}

type Resources struct {
	CPUs   float64 `json:"cpus,omitempty" yaml:"cpus,omitempty"`
	Memory Bytes   `json:"memory,omitempty" yaml:"memory,omitempty"`
	Disk   Bytes   `json:"disk,omitempty" yaml:"disk,omitempty"`
	Pids   int64   `json:"pids,omitempty" yaml:"pids,omitempty"`
}

type Placement struct {
	Pool     string            `json:"pool,omitempty" yaml:"pool,omitempty"`
	Requires map[string]string `json:"requires,omitempty" yaml:"requires,omitempty"`
	Prefers  map[string]string `json:"prefers,omitempty" yaml:"prefers,omitempty"`
}

type Network struct {
	Egress []EgressRule `json:"egress,omitempty" yaml:"egress,omitempty"`
	Ports  []Port       `json:"ports,omitempty" yaml:"ports,omitempty"`
	// Unrestricted switches egress filtering off. Default deny otherwise.
	Unrestricted bool `json:"unrestricted,omitempty" yaml:"unrestricted,omitempty"`
}

type EgressRule struct {
	Host string `json:"host,omitempty" yaml:"host,omitempty"`
	CIDR string `json:"cidr,omitempty" yaml:"cidr,omitempty"`
}

type Port struct {
	Port int    `json:"port" yaml:"port"`
	Name string `json:"name" yaml:"name"`
}

type Sandbox struct {
	NestedContainers bool `json:"nestedContainers,omitempty" yaml:"nestedContainers,omitempty"`
	ReadOnlyRoot     bool `json:"readOnlyRoot,omitempty" yaml:"readOnlyRoot,omitempty"`
}

type Artifacts struct {
	Paths []string `json:"paths,omitempty" yaml:"paths,omitempty"`
}

// Adapters lux knows. Each is a way to start, resume, steer and stop a
// workload; `generic` is a plain process.
var Adapters = map[string]AdapterInfo{
	"generic": {},
	"acp":     {},
	"claude-code": {
		DefaultCommand: []string{"claude"},
		StatePaths:     []string{"$HOME/.claude"},
	},
	"codex": {
		DefaultCommand: []string{"codex"},
		StatePaths:     []string{"$HOME/.codex"},
	},
	"opencode": {
		DefaultCommand: []string{"opencode", "acp"},
		StatePaths:     []string{"$HOME/.local/share/opencode"},
	},
}

type AdapterInfo struct {
	DefaultCommand []string
	// Paths the agent keeps its session in. A spec whose state volumes do not
	// cover them is rejected: it would break on the first resume.
	StatePaths []string
}

// Duration marshals as a Go duration string ("4h", "90s").
type Duration struct{ time.Duration }

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		var n float64
		if err2 := json.Unmarshal(b, &n); err2 != nil {
			return err
		}
		d.Duration = time.Duration(n * float64(time.Second))
		return nil
	}
	return d.parse(s)
}
func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	return d.parse(s)
}
func (d *Duration) parse(s string) error {
	if s == "" {
		d.Duration = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q", s)
	}
	d.Duration = v
	return nil
}

// Bytes accepts "8Gi", "512Mi", "1G", or a plain number of bytes.
type Bytes int64

func (b Bytes) MarshalJSON() ([]byte, error) { return json.Marshal(int64(b)) }
func (b *Bytes) UnmarshalJSON(data []byte) error {
	var n int64
	if err := json.Unmarshal(data, &n); err == nil {
		*b = Bytes(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	return b.parse(s)
}
func (b *Bytes) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	return b.parse(s)
}

var bytesRe = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([KMGT]i?)?B?$`)

func (b *Bytes) parse(s string) error {
	m := bytesRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return fmt.Errorf("invalid size %q", s)
	}
	f, _ := strconv.ParseFloat(m[1], 64)
	mult := map[string]float64{
		"": 1, "K": 1e3, "M": 1e6, "G": 1e9, "T": 1e12,
		"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40,
	}[m[2]]
	*b = Bytes(f * mult)
	return nil
}

var (
	nameRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,62}$`)
	envRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	volumeRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
)

// Defaults applied by Normalize.
const (
	DefaultCPUs    = 2
	DefaultMemory  = Bytes(4 << 30)
	DefaultPids    = 1024
	DefaultTimeout = 24 * time.Hour
	DefaultGrace   = 30 * time.Second
)

// Normalize validates the spec and fills defaults. It returns every problem
// at once so a caller can fix a spec in one round trip.
func (s *RunSpec) Normalize() error {
	var errs []string
	fail := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	if (s.Image.Ref == "") == (s.Image.Build == nil) {
		fail("image: exactly one of ref or build is required")
	}
	if b := s.Image.Build; b != nil {
		if strings.TrimSpace(b.Containerfile) == "" {
			fail("image.build.containerfile is required")
		}
		if b.Context != "" {
			fail("image.build.context is not supported yet: COPY and ADD have no build context")
		}
	}

	w := &s.Workload
	if w.Adapter == "" {
		w.Adapter = "generic"
	}
	info, ok := Adapters[w.Adapter]
	if !ok {
		fail("workload.adapter: unknown adapter %q", w.Adapter)
	}
	if len(w.Command) == 0 {
		w.Command = info.DefaultCommand
	}
	if len(w.Command) == 0 {
		fail("workload.command is required for adapter %q", w.Adapter)
	}
	if w.Workdir != "" && !path.IsAbs(w.Workdir) {
		fail("workload.workdir must be absolute")
	}
	if w.TTY && w.Adapter != "generic" {
		fail("workload.tty is only for generic workloads: agent adapters own their stdio")
	}
	if w.Grace.Duration == 0 {
		w.Grace.Duration = DefaultGrace
	}

	for k := range s.Env {
		if !envRe.MatchString(k) {
			fail("env: invalid name %q", k)
		}
		if strings.HasPrefix(k, "LUX_") {
			fail("env: %q: the LUX_ prefix is reserved", k)
		}
	}

	seen := map[string]bool{}
	for i := range s.Secrets {
		sec := &s.Secrets[i]
		if !nameRe.MatchString(sec.Name) {
			fail("secrets[%d]: invalid name %q", i, sec.Name)
		}
		if seen[sec.Name] {
			fail("secrets: duplicate %q", sec.Name)
		}
		seen[sec.Name] = true
		if sec.As == "" {
			sec.As = "env"
		}
		switch sec.As {
		case "env":
			if !envRe.MatchString(sec.Name) {
				fail("secrets[%d]: %q is not a valid environment variable name", i, sec.Name)
			}
		case "file":
			if !path.IsAbs(sec.Path) {
				fail("secrets[%d]: file secrets need an absolute path", i)
			}
		default:
			fail("secrets[%d].as must be env or file", i)
		}
	}

	vols := map[string]bool{}
	for i := range s.Volumes {
		v := &s.Volumes[i]
		if !volumeRe.MatchString(v.Name) {
			fail("volumes[%d]: invalid name %q (lowercase, digits, - and _)", i, v.Name)
		}
		if vols[v.Name] {
			fail("volumes: duplicate %q", v.Name)
		}
		vols[v.Name] = true
		v.Path = path.Clean(v.Path)
		if !path.IsAbs(v.Path) || v.Path == "/" {
			fail("volumes[%d]: path must be absolute and not /", i)
		}
		if strings.HasPrefix(v.Path, "/.lux") {
			fail("volumes[%d]: /.lux is reserved", i)
		}
		if v.Kind == "" {
			v.Kind = "state"
		}
		if v.Kind != "state" && v.Kind != "ephemeral" {
			fail("volumes[%d].kind must be state or ephemeral", i)
		}
	}

	if s.Git != nil {
		names := map[string]bool{}
		for i := range s.Git.Repositories {
			r := &s.Git.Repositories[i]
			if !volumeRe.MatchString(r.Name) || names[r.Name] {
				fail("git.repositories[%d]: invalid or duplicate name %q", i, r.Name)
			}
			names[r.Name] = true
			if r.URL == "" {
				fail("git.repositories[%d].url is required", i)
			} else if u, err := url.Parse(r.URL); err == nil && u.User != nil {
				// It would end up in the checkout's config, the snapshot and
				// logs: credentials go through `credential`, never the URL.
				fail("git.repositories[%d].url must not carry credentials: use credential: <secret name>", i)
			}
			if r.Ref == "" {
				r.Ref = "HEAD"
			}
			if r.Path == "" {
				r.Path = "/workspace/repos/" + r.Name
			}
			r.Path = path.Clean(r.Path)
			if v := s.volumeFor(r.Path); v == nil || v.Kind != "state" {
				fail("git.repositories[%d]: %s is not on a state volume, so the checkout would not survive a stop", i, r.Path)
			}
			if r.Credential != "" && !seen[r.Credential] {
				fail("git.repositories[%d].credential: no secret named %q", i, r.Credential)
			}
		}
		if s.Git.Push != nil && s.Git.Push.Branch == "" {
			fail("git.push.branch is required")
		}
	}
	// Git credentials are the runner's, never the container's.
	for i := range s.Secrets {
		if s.Git != nil {
			for _, r := range s.Git.Repositories {
				if r.Credential == s.Secrets[i].Name {
					s.Secrets[i].RunnerOnly = true
				}
			}
		}
	}

	for _, p := range info.StatePaths {
		home := "/home/agent"
		if w.User == "root" {
			home = "/root"
		}
		want := strings.ReplaceAll(p, "$HOME", home)
		if s.stateVolumeFor(want) == nil {
			fail("volumes: adapter %q keeps its session in %s, which must be on a state volume", w.Adapter, want)
		}
	}

	r := &s.Resources
	if r.CPUs == 0 {
		r.CPUs = DefaultCPUs
	}
	if r.Memory == 0 {
		r.Memory = DefaultMemory
	}
	if r.Pids == 0 {
		r.Pids = DefaultPids
	}
	if r.CPUs < 0 || r.Memory < 0 || r.Pids < 0 {
		fail("resources must not be negative")
	}
	if s.Timeout.Duration == 0 {
		s.Timeout.Duration = DefaultTimeout
	}
	if s.Placement.Pool == "" {
		s.Placement.Pool = "default"
	}

	for i, e := range s.Network.Egress {
		if (e.Host == "") == (e.CIDR == "") {
			fail("network.egress[%d]: exactly one of host or cidr", i)
		}
		if strings.Contains(e.Host, "*") {
			fail("network.egress[%d]: wildcards cannot be resolved; list concrete hostnames", i)
		}
	}
	ports := map[string]bool{}
	for i, p := range s.Network.Ports {
		if p.Port < 1 || p.Port > 65535 || p.Name == "" || ports[p.Name] {
			fail("network.ports[%d]: need a port 1-65535 and a unique name", i)
		}
		ports[p.Name] = true
	}
	for i, p := range s.Artifacts.Paths {
		if !path.IsAbs(p) {
			fail("artifacts.paths[%d]: must be absolute", i)
		}
	}

	if len(errs) > 0 {
		return &ValidationError{Problems: errs}
	}
	return nil
}

// volumeFor is the volume a path is on: the most specific one, as mounts
// nest (a volume at /workspace/repos hides /workspace's files there).
func (s *RunSpec) volumeFor(p string) *Volume {
	var best *Volume
	for i := range s.Volumes {
		v := &s.Volumes[i]
		if (p == v.Path || strings.HasPrefix(p, v.Path+"/")) && (best == nil || len(v.Path) > len(best.Path)) {
			best = v
		}
	}
	return best
}

func (s *RunSpec) stateVolumeFor(p string) *Volume {
	if v := s.volumeFor(p); v != nil && v.Kind == "state" {
		return v
	}
	return nil
}

// ValidationError lists every problem found in a spec.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string { return "invalid spec: " + strings.Join(e.Problems, "; ") }

// SecretRef is what is stored about a secret: never its value.
type SecretRef struct {
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
}

// Fingerprint identifies a value without revealing it. Salted with the name
// so equal values under different names do not look alike.
func Fingerprint(name, value string) string {
	h := sha256.Sum256([]byte("lux-secret\x00" + name + "\x00" + value))
	return hex.EncodeToString(h[:8])
}

// SplitSecrets returns the spec without secret values, the stored refs, and
// the values by name.
func (s RunSpec) SplitSecrets() (RunSpec, []SecretRef, map[string]string) {
	refs := make([]SecretRef, 0, len(s.Secrets))
	values := map[string]string{}
	stripped := make([]Secret, len(s.Secrets))
	for i, sec := range s.Secrets {
		refs = append(refs, SecretRef{Name: sec.Name, Fingerprint: Fingerprint(sec.Name, sec.Value)})
		values[sec.Name] = sec.Value
		sec.Value = ""
		stripped[i] = sec
	}
	s.Secrets = stripped
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return s, refs, values
}
