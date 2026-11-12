package passwd

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const testPasswd = `root:x:0:0:root:/root:/bin/sh
runner:x:1000:1000::/home/runner:/bin/sh
run:x:1001:1001::/home/run:/bin/sh
solo:x:1002:1002::/home/solo:/bin/sh
`

const testGroup = `root:x:0:
adm:x:4:syslog,runner
runner:x:1000:runner
run:x:1001:
docker:x:999:runner
builders:x:3000:alice,runner,bob
`

func TestGroups(t *testing.T) {
	for _, c := range []struct {
		name    string
		user    string
		primary int
		group   string
		want    []int
	}{
		{"no memberships", "solo", 1002, testGroup, nil},
		{"one membership", "x", 7, "a:x:5:x\n", []int{5}},
		{"many, in file order; never the primary", "runner", 1000, testGroup, []int{4, 999, 3000}},
		{"a name that prefixes a member is not that member", "run", 1001, testGroup, nil},
		{"a member that prefixes the name is not it", "runnerx", 5, testGroup, nil},
		{"no name: none", "", 1000, testGroup, nil},
		{"empty file", "runner", 1000, "", nil},
		{"missing file (nil)", "runner", 1000, "", nil},
		{"a gid listed twice is kept once", "u", 1, "a:x:5:u\nb:x:5:u\n", []int{5}},
		{"malformed lines are skipped", "u", 1, "# a:x:6:u\nshort:x:7\nbadgid:x:notanumber:u\nneg:x:-1:u\n\n   \nok:x:8:u\n", []int{8}},
		{"crlf and padding around a line", "u", 1, "  a:x:5:u  \r\nb:x:6:v,u\r\n", []int{5, 6}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var g []byte
			if c.group != "" {
				g = []byte(c.group)
			}
			if got := Groups(c.user, c.primary, g); !slices.Equal(got, c.want) {
				t.Fatalf("Groups(%q, %d) = %v, want %v", c.user, c.primary, got, c.want)
			}
		})
	}
}

func TestResolveMember(t *testing.T) {
	pw := testPasswd + "zero:x:0:0::/:/bin/sh\n"
	for _, c := range []struct {
		spec   string
		member string
	}{
		{"runner", "runner"},
		{"1000", "runner"},  // a uid with an entry: its name
		{"01000", "runner"}, // matched as a number, as runc
		{"+1000", "runner"}, // likewise
		{"runner:", "runner"},
		{"1000:", "runner"},
		{"1001", "run"},
		{"4242", ""},       // a uid with no entry: the primary only
		{"runner:999", ""}, // an explicit group: that group only, as podman
		{"1000:1000", ""},  // likewise
		{"01000:1000", ""}, // likewise
		{"", ""},           // root
		{"root", ""},       // root
		{"0", ""},          // root
		{"00", ""},         // root, numerically
		{"zero", ""},       // an entry with uid 0
		{"-1000", ""},      // no such uid
	} {
		_, member, err := Resolve(c.spec, []byte(pw))
		if err != nil || member != c.member {
			t.Errorf("Resolve(%q) member = %q, %v; want %q", c.spec, member, err, c.member)
		}
	}
	if _, _, err := Resolve("nobody", []byte(pw)); err == nil {
		t.Error("Resolve(nobody) did not fail")
	}
	// Each uid-0 guard on its own: the member's entry has uid 0 but the
	// user's does not, and the user's uid is 0 but the member's is not.
	for spec, pw := range map[string]string{
		"00":  "a:x:0:0::/:/bin/sh\n00:x:5:5::/:/bin/sh\n",
		"abc": "q:x:abc:0::/:/bin/sh\nabc:x:5:5::/:/bin/sh\n",
	} {
		if _, member, err := Resolve(spec, []byte(pw)); err != nil || member != "" {
			t.Errorf("Resolve(%q) member = %q, %v; want none", spec, member, err)
		}
	}
}

// The name a numeric uid resolves to is what is matched in /etc/group.
func TestGroupsByUID(t *testing.T) {
	for _, spec := range []string{"1000", "01000", "+1000", "runner:"} {
		_, member, _ := Resolve(spec, []byte(testPasswd))
		if got := Groups(member, 1000, []byte(testGroup)); !slices.Equal(got, []int{4, 999, 3000}) {
			t.Fatalf("%q: got %v", spec, got)
		}
	}
}

// lookupBefore is Lookup as it was before Resolve: who a workload runs as
// must not change.
func lookupBefore(spec string, passwd []byte) (User, error) {
	if spec == "" || spec == "root" || spec == "0" {
		return User{Name: "root", Home: "/root"}, nil
	}
	name, group, _ := strings.Cut(spec, ":")
	u := User{Name: name, Home: "/"}
	found := false
	for _, l := range strings.Split(string(passwd), "\n") {
		f := strings.Split(l, ":")
		if len(f) < 7 || (f[0] != name && f[2] != name) {
			continue
		}
		u.Name, u.Home = f[0], f[5]
		u.UID, _ = strconv.Atoi(f[2])
		u.GID, _ = strconv.Atoi(f[3])
		found = true
		break
	}
	if !found {
		n, err := strconv.Atoi(name)
		if err != nil {
			return User{}, fmt.Errorf("user %q is not in the image's /etc/passwd", name)
		}
		u.UID, u.GID = n, n
	}
	if group != "" {
		g, err := strconv.Atoi(group)
		if err != nil {
			return User{}, errors.New("group must be numeric")
		}
		u.GID = g
	}
	return u, nil
}

func TestLookupUnchanged(t *testing.T) {
	files := []string{"", testPasswd,
		// uid 1000 under two names, the second with another gid; a name
		// that is a uid; malformed and short lines.
		"a:x:1000:5::/a:/bin/sh\nb:x:1000:6::/b:/bin/sh\n1000:x:7:7::/n:/bin/sh\nshort:x:1\nbad:x:x:y::/:/bin/sh\n",
		"x:x:01000:9::/x:/bin/sh\nroot:x:0:0::/root:/bin/sh\n"}
	specs := []string{"", "root", "0", "00", "0:0", "0:5", "runner", "1000", "01000", "+1000", "-1",
		"runner:", "runner:999", "1000:1000", "1000:x", "a", "b", "7", "x", "bad", "short", "nobody", "4242", "4242:1", ":", ":5"}
	for _, f := range files {
		for _, spec := range specs {
			want, wantErr := lookupBefore(spec, []byte(f))
			got, err := Lookup(spec, []byte(f))
			u, _, rerr := Resolve(spec, []byte(f))
			if got != want || u != want || fmt.Sprint(err) != fmt.Sprint(wantErr) || fmt.Sprint(rerr) != fmt.Sprint(wantErr) {
				t.Errorf("%q in %q: Lookup %+v %v, Resolve %+v %v; before %+v %v", spec, f, got, err, u, rerr, want, wantErr)
			}
		}
	}
}
