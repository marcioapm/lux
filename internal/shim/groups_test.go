package shim

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
)

const groupsPasswd = `root:x:0:0:root:/root:/bin/sh
runner:x:1000:1000::/home/runner:/bin/sh
run:x:1001:1001::/home/run:/bin/sh
`

const groupsGroup = `root:x:0:
adm:x:4:runner
runner:x:1000:runner
run:x:1001:
docker:x:999:runner
`

// A rootless container's map: container 0..65535 onto the host's.
const fullMap = "         0       1000      65536\n"

// withGroupFiles points resolveGroups at test copies of the image's and
// the kernel's files.
func withGroupFiles(t *testing.T, pw, gr, gidMap, setgroups string) {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if content != "" {
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return p
	}
	old := []string{etcPasswd, etcGroup, procGidMap, procSetgroups}
	etcPasswd, etcGroup = write("passwd", pw), write("group", gr)
	procGidMap, procSetgroups = write("gid_map", gidMap), write("setgroups", setgroups)
	t.Cleanup(func() { etcPasswd, etcGroup, procGidMap, procSetgroups = old[0], old[1], old[2], old[3] })
}

// groupsShim resolves cfg's user as the shim's start does, returning the
// shim and its output (events) once closed.
func groupsShim(t *testing.T, cfg proto.ShimConfig) (*Shim, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "out.jsonl")
	out, err := OpenOutput(path, NewRedactor(nil))
	if err != nil {
		t.Fatal(err)
	}
	u, err := lookupUser(cfg.User)
	if err != nil {
		t.Fatal(err)
	}
	s := &Shim{cfg: cfg, out: out}
	s.resolveGroups(u)
	s.user = u
	out.Close()
	b, _ := os.ReadFile(path)
	return s, string(b)
}

func credentialGroups(t *testing.T, s *Shim) []uint32 {
	t.Helper()
	cmd := s.command([]string{"/bin/true"}, nil)
	if cmd.SysProcAttr.Credential == nil {
		return nil
	}
	return cmd.SysProcAttr.Credential.Groups
}

func TestCredentialGroupsFromImage(t *testing.T) {
	withGroupFiles(t, groupsPasswd, groupsGroup, fullMap, "allow\n")
	for _, c := range []struct {
		name string
		cfg  proto.ShimConfig
		want []uint32
	}{
		{"a name", proto.ShimConfig{User: "runner"}, []uint32{1000, 4, 999}},
		{"a uid with an entry", proto.ShimConfig{User: "1000"}, []uint32{1000, 4, 999}},
		{"the image's USER behind the runner's uid:gid", proto.ShimConfig{User: "1000:1000", MemberUser: "runner"}, []uint32{1000, 4, 999}},
		{"an explicit group: that group only, as podman", proto.ShimConfig{User: "runner:999"}, []uint32{999}},
		{"an explicit uid:gid: that group only", proto.ShimConfig{User: "1000:1000"}, []uint32{1000}},
		{"the image's USER with a group", proto.ShimConfig{User: "1000:4", MemberUser: "runner:4"}, []uint32{4}},
		{"a uid with no entry: the primary only", proto.ShimConfig{User: "4242"}, []uint32{4242}},
		{"a user in no group", proto.ShimConfig{User: "run"}, []uint32{1001}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, events := groupsShim(t, c.cfg)
			if got := credentialGroups(t, s); !slices.Equal(got, c.want) {
				t.Fatalf("Credential.Groups = %v, want %v", got, c.want)
			}
			if strings.Contains(events, "lux.warning") {
				t.Fatalf("warned: %s", events)
			}
		})
	}
}

// Root runs as the shim does, with no credential change and no groups.
func TestRootGroupsUnchanged(t *testing.T) {
	withGroupFiles(t, groupsPasswd, "root:x:0:\nwheel:x:10:root\n", fullMap, "allow\n")
	for _, spec := range []string{"", "root", "0", "0:0"} {
		s, events := groupsShim(t, proto.ShimConfig{User: spec, MemberUser: spec})
		if cmd := s.command([]string{"/bin/true"}, nil); cmd.SysProcAttr.Credential != nil {
			t.Fatalf("%q: credential %+v", spec, cmd.SysProcAttr.Credential)
		}
		if strings.Contains(events, "lux.warning") {
			t.Fatalf("%q warned: %s", spec, events)
		}
	}
}

func TestUnmappedGroupsDropped(t *testing.T) {
	// Maps 0..998 and 1000 only, as a rootless container with a short
	// subgid range: adm (4) is mapped, docker (999) is not.
	withGroupFiles(t, groupsPasswd, groupsGroup, "0 1000 999\n1000 5000 1\n", "allow\n")
	s, events := groupsShim(t, proto.ShimConfig{User: "runner"})
	if got := credentialGroups(t, s); !slices.Equal(got, []uint32{1000, 4}) {
		t.Fatalf("Credential.Groups = %v", got)
	}
	if strings.Count(events, "lux.warning") != 1 || !strings.Contains(events, "[999]") || !strings.Contains(events, "not mapped") {
		t.Fatalf("events: %s", events)
	}
}

// The primary group is kept whatever the map says: it is what the shim
// always set.
func TestUnmappedPrimaryKept(t *testing.T) {
	withGroupFiles(t, groupsPasswd, groupsGroup, "0 1000 10\n", "allow\n")
	s, events := groupsShim(t, proto.ShimConfig{User: "runner"})
	if got := credentialGroups(t, s); !slices.Equal(got, []uint32{1000, 4}) {
		t.Fatalf("Credential.Groups = %v", got)
	}
	if strings.Count(events, "lux.warning") != 1 || !strings.Contains(events, "[999]") {
		t.Fatalf("events: %s", events)
	}
}

func TestSetgroupsDenied(t *testing.T) {
	withGroupFiles(t, groupsPasswd, groupsGroup, fullMap, "deny\n")
	s, events := groupsShim(t, proto.ShimConfig{User: "runner"})
	if got := credentialGroups(t, s); !slices.Equal(got, []uint32{1000}) {
		t.Fatalf("Credential.Groups = %v", got)
	}
	if strings.Count(events, "lux.warning") != 1 || !strings.Contains(events, "setgroups is denied") {
		t.Fatalf("events: %s", events)
	}
}

// A denied setgroups or an unreadable map with no memberships to give is
// today's start, with nothing to warn of.
func TestNoMembershipsNoWarning(t *testing.T) {
	withGroupFiles(t, groupsPasswd, groupsGroup, "", "deny\n")
	s, events := groupsShim(t, proto.ShimConfig{User: "run"})
	if got := credentialGroups(t, s); !slices.Equal(got, []uint32{1001}) {
		t.Fatalf("Credential.Groups = %v", got)
	}
	if strings.Contains(events, "lux.warning") {
		t.Fatalf("warned: %s", events)
	}
}

func TestMissingGroupFile(t *testing.T) {
	withGroupFiles(t, groupsPasswd, "", fullMap, "allow\n")
	s, events := groupsShim(t, proto.ShimConfig{User: "runner"})
	if got := credentialGroups(t, s); !slices.Equal(got, []uint32{1000}) {
		t.Fatalf("Credential.Groups = %v", got)
	}
	if strings.Contains(events, "lux.warning") {
		t.Fatalf("warned: %s", events)
	}
}

func TestUnreadableGidMap(t *testing.T) {
	withGroupFiles(t, groupsPasswd, groupsGroup, "", "allow\n")
	s, events := groupsShim(t, proto.ShimConfig{User: "runner"})
	if got := credentialGroups(t, s); !slices.Equal(got, []uint32{1000}) {
		t.Fatalf("Credential.Groups = %v", got)
	}
	if strings.Count(events, "lux.warning") != 1 {
		t.Fatalf("events: %s", events)
	}
}

func TestGroupsCappedAtNgroupsMax(t *testing.T) {
	var b strings.Builder
	for i := range ngroupsMax + 10 {
		fmt.Fprintf(&b, "g%d:x:%d:runner\n", i, 2000+i)
	}
	got, warn := workloadGroups("runner", 1000, []byte(groupsPasswd), []byte(b.String()), []byte("0 0 4294967295\n"), []byte("allow"))
	if len(got) != ngroupsMax || got[0] != 1000 {
		t.Fatalf("%d groups, first %d", len(got), got[0])
	}
	if !strings.Contains(warn, "NGROUPS_MAX") || !strings.Contains(warn, "11 supplementary") {
		t.Fatalf("warn %q", warn)
	}
}

func TestParseIDMap(t *testing.T) {
	m := parseIDMap([]byte("         0       1000          1\n         1     100000      65536\nbad line\n5 6 0\n"))
	for id, want := range map[int]bool{0: true, 1: true, 65536: true, 65537: false, -1: false, 1 << 20: false} {
		if m.contains(id) != want {
			t.Errorf("contains(%d) = %t", id, !want)
		}
	}
}

// The command really starts with the groups, the unmapped one dropped:
// as root in a user namespace whose gid map is short, as the shim is in a
// rootless container. Run it there with
//
//	go test -c -o /tmp/shim.test ./internal/shim && unshare --user --map-auto --map-root-user /tmp/shim.test -test.run StartsWithGroups -test.v
func TestStartsWithGroups(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root (in a user namespace) to setgroups")
	}
	gidMap, _ := os.ReadFile("/proc/self/gid_map")
	m := parseIDMap(gidMap)
	if !m.contains(5) || !m.contains(6) || m.contains(1<<31) {
		t.Skipf("needs gids 5 and 6 mapped and 2^31 not: %s", gidMap)
	}
	sg, _ := os.ReadFile("/proc/self/setgroups")
	if strings.TrimSpace(string(sg)) != "allow" {
		t.Skip("setgroups denied here")
	}
	withGroupFiles(t, "runner:x:5:5::/:/bin/sh\n", "a:x:6:runner\nhuge:x:2147483648:runner\n", string(gidMap), string(sg))
	s, events := groupsShim(t, proto.ShimConfig{User: "runner"})
	out, err := s.command([]string{"/bin/sh", "-c", "id -G"}, []string{"PATH=/usr/bin:/bin"}).Output()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "5 6" {
		t.Fatalf("id -G = %q", got)
	}
	if strings.Count(events, "lux.warning") != 1 || !strings.Contains(events, "2147483648") {
		t.Fatalf("events: %s", events)
	}
	// What the filter spares it: setgroups with the unmapped gid fails,
	// and the command with it.
	cmd := s.command([]string{"/bin/true"}, nil)
	cmd.SysProcAttr.Credential.Groups = []uint32{5, 6, 1 << 31}
	if err := cmd.Run(); err == nil {
		t.Fatal("an unmapped gid did not fail the start: the filter is untested here")
	}
}
