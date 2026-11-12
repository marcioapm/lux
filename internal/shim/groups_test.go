package shim

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

const groupsPasswd = `root:x:0:0:root:/root:/bin/sh
runner:x:1000:1000::/home/runner:/bin/sh
run:x:1001:1001::/home/run:/bin/sh
`

// A rootless container's map: container 0..65535 onto the host's.
const fullMap = "         0       1000      65536\n"

// withPasswd points lookupUser's /etc/passwd at a test copy.
func withPasswd(t *testing.T, pw string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "passwd")
	if err := os.WriteFile(p, []byte(pw), 0o644); err != nil {
		t.Fatal(err)
	}
	old := etcPasswd
	etcPasswd = p
	t.Cleanup(func() { etcPasswd = old })
}

// withKernelFiles points resolveGroups at test copies of the kernel's
// files; empty content is a missing file.
func withKernelFiles(t *testing.T, gidMap, setgroups string) {
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
	old := []string{procGidMap, procSetgroups}
	procGidMap, procSetgroups = write("gid_map", gidMap), write("setgroups", setgroups)
	t.Cleanup(func() { procGidMap, procSetgroups = old[0], old[1] })
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

// credential is what the workload's process is started as: nil for root.
func credential(s *Shim) *syscall.Credential {
	return s.command([]string{"/bin/true"}, nil).SysProcAttr.Credential
}

func cred(uid, gid uint32, groups ...uint32) *syscall.Credential {
	return &syscall.Credential{Uid: uid, Gid: gid, Groups: groups}
}

func sameCredential(a, b *syscall.Credential) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Uid == b.Uid && a.Gid == b.Gid && slices.Equal(a.Groups, b.Groups) && a.NoSetGroups == b.NoSetGroups
}

// The full credential: uid and primary gid as before, then the groups the
// runner supplied that the user namespace maps.
func TestCredentialFromConfig(t *testing.T) {
	withPasswd(t, groupsPasswd)
	withKernelFiles(t, fullMap, "allow\n")
	for _, c := range []struct {
		name string
		cfg  proto.ShimConfig
		want *syscall.Credential
	}{
		{"a name with groups", proto.ShimConfig{User: "runner", Groups: []int{4, 999}}, cred(1000, 1000, 1000, 4, 999)},
		{"the image's USER as uid:gid with groups", proto.ShimConfig{User: "1000:1000", Groups: []int{4, 999}}, cred(1000, 1000, 1000, 4, 999)},
		{"no groups supplied (an older runner)", proto.ShimConfig{User: "runner"}, cred(1000, 1000, 1000)},
		{"an explicit group", proto.ShimConfig{User: "runner:999"}, cred(1000, 999, 999)},
		{"an explicit uid:gid", proto.ShimConfig{User: "1000:1000"}, cred(1000, 1000, 1000)},
		{"a uid with no entry", proto.ShimConfig{User: "4242"}, cred(4242, 4242, 4242)},
		{"a user in no group", proto.ShimConfig{User: "run"}, cred(1001, 1001, 1001)},
		{"the primary and repeats are not added twice", proto.ShimConfig{User: "runner", Groups: []int{1000, 4, 4, -1}}, cred(1000, 1000, 1000, 4)},
		{"group 0 as a supplementary group", proto.ShimConfig{User: "runner", Groups: []int{0}}, cred(1000, 1000, 1000, 0)},
		{"root", proto.ShimConfig{User: ""}, nil},
		{"root by name", proto.ShimConfig{User: "root"}, nil},
		{"root, with groups supplied: ignored", proto.ShimConfig{User: "0:0", Groups: []int{4}}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, events := groupsShim(t, c.cfg)
			if got := credential(s); !sameCredential(got, c.want) {
				t.Fatalf("credential %+v, want %+v", got, c.want)
			}
			if strings.Contains(events, "lux.warning") {
				t.Fatalf("warned: %s", events)
			}
		})
	}
}

// With no groups supplied the shim reads neither /proc file: a start is
// what it was before. Both are FIFOs here, so a read would block.
func TestNoGroupsReadsNothing(t *testing.T) {
	withPasswd(t, groupsPasswd)
	dir := t.TempDir()
	old := []string{procGidMap, procSetgroups}
	procGidMap, procSetgroups = filepath.Join(dir, "gid_map"), filepath.Join(dir, "setgroups")
	t.Cleanup(func() { procGidMap, procSetgroups = old[0], old[1] })
	for _, p := range []string{procGidMap, procSetgroups} {
		if err := syscall.Mkfifo(p, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, user := range []string{"runner", "1000:1000", "runner:999", "4242", "root"} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			s, events := groupsShim(t, proto.ShimConfig{User: user})
			if c := credential(s); (c != nil && (len(c.Groups) != 1 || c.Groups[0] != c.Gid)) || strings.Contains(events, "lux.warning") {
				t.Errorf("%s: %+v %s", user, c, events)
			}
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: read a /proc file", user)
		}
	}
}

func TestUnmappedGroupsDropped(t *testing.T) {
	withPasswd(t, groupsPasswd)
	// Maps 0..998 and 1000 only, as a rootless container with a short
	// subgid range: adm (4) is mapped, docker (999) is not.
	withKernelFiles(t, "0 1000 999\n1000 5000 1\n", "allow\n")
	s, events := groupsShim(t, proto.ShimConfig{User: "runner", Groups: []int{4, 999}})
	if got := credential(s); !sameCredential(got, cred(1000, 1000, 1000, 4)) {
		t.Fatalf("credential %+v", got)
	}
	if strings.Count(events, "lux.warning") != 1 || !strings.Contains(events, "[999]") || !strings.Contains(events, "not mapped") {
		t.Fatalf("events: %s", events)
	}
}

// The primary group is kept whatever the map says: it is what the shim
// always set.
func TestUnmappedPrimaryKept(t *testing.T) {
	withPasswd(t, groupsPasswd)
	withKernelFiles(t, "0 1000 10\n", "allow\n")
	s, events := groupsShim(t, proto.ShimConfig{User: "runner", Groups: []int{4, 999}})
	if got := credential(s); !sameCredential(got, cred(1000, 1000, 1000, 4)) {
		t.Fatalf("credential %+v", got)
	}
	if strings.Count(events, "lux.warning") != 1 || !strings.Contains(events, "[999]") {
		t.Fatalf("events: %s", events)
	}
}

func TestSetgroupsDenied(t *testing.T) {
	withPasswd(t, groupsPasswd)
	withKernelFiles(t, fullMap, "deny\n")
	s, events := groupsShim(t, proto.ShimConfig{User: "runner", Groups: []int{4, 999}})
	if got := credential(s); !sameCredential(got, cred(1000, 1000, 1000)) {
		t.Fatalf("credential %+v", got)
	}
	if strings.Count(events, "lux.warning") != 1 || !strings.Contains(events, "setgroups is denied") {
		t.Fatalf("events: %s", events)
	}
}

// A denied setgroups with no groups to give is today's start, with
// nothing to warn of.
func TestNoGroupsNoWarning(t *testing.T) {
	withPasswd(t, groupsPasswd)
	withKernelFiles(t, "", "deny\n")
	s, events := groupsShim(t, proto.ShimConfig{User: "run"})
	if got := credential(s); !sameCredential(got, cred(1001, 1001, 1001)) {
		t.Fatalf("credential %+v", got)
	}
	if strings.Contains(events, "lux.warning") {
		t.Fatalf("warned: %s", events)
	}
}

func TestUnreadableGidMap(t *testing.T) {
	withPasswd(t, groupsPasswd)
	withKernelFiles(t, "", "allow\n")
	s, events := groupsShim(t, proto.ShimConfig{User: "runner", Groups: []int{4}})
	if got := credential(s); !sameCredential(got, cred(1000, 1000, 1000)) {
		t.Fatalf("credential %+v", got)
	}
	if strings.Count(events, "lux.warning") != 1 {
		t.Fatalf("events: %s", events)
	}
}

func TestGroupsCappedAtNgroupsMax(t *testing.T) {
	var extra []int
	for i := range ngroupsMax + 10 {
		extra = append(extra, 2000+i)
	}
	got, warn := workloadGroups("runner", 1000, extra, []byte("0 0 4294967295\n"), []byte("allow"))
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
	withPasswd(t, "runner:x:5:5::/:/bin/sh\n")
	withKernelFiles(t, string(gidMap), string(sg))
	s, events := groupsShim(t, proto.ShimConfig{User: "runner", Groups: []int{6, 1 << 31}})
	if got := credential(s); !sameCredential(got, cred(5, 5, 5, 6)) {
		t.Fatalf("credential %+v", got)
	}
	out, err := s.command([]string{"/bin/sh", "-c", "echo $(id -u) $(id -g) $(id -G)"}, []string{"PATH=/usr/bin:/bin"}).Output()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// uid, primary gid and groups, each as the kernel gives them.
	if f := strings.Fields(string(out)); !slices.Equal(f, []string{"5", "5", "5", "6"}) {
		t.Fatalf("id -u, -g, -G = %q, want 5, 5, 5 6", out)
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
