package passwd

import (
	"slices"
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
		{"no memberships", "solo", 1002, testGroup, []int{1002}},
		{"one membership", "x", 7, "a:x:5:x\n", []int{7, 5}},
		{"many, in file order; the primary listed again is kept once", "runner", 1000, testGroup, []int{1000, 4, 999, 3000}},
		{"a name that prefixes a member is not that member", "run", 1001, testGroup, []int{1001}},
		{"a member that prefixes the name is not it", "runnerx", 5, testGroup, []int{5}},
		{"no name: the primary only", "", 1000, testGroup, []int{1000}},
		{"empty file", "runner", 1000, "", []int{1000}},
		{"missing file (nil)", "runner", 1000, "", []int{1000}},
		{"a gid listed twice is kept once", "u", 1, "a:x:5:u\nb:x:5:u\n", []int{1, 5}},
		{"malformed lines are skipped", "u", 1, "# a:x:6:u\nshort:x:7\nbadgid:x:notanumber:u\nneg:x:-1:u\n\n   \nok:x:8:u\n", []int{1, 8}},
		{"crlf and padding around a line", "u", 1, "  a:x:5:u  \r\nb:x:6:v,u\r\n", []int{1, 5, 6}},
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

func TestMemberName(t *testing.T) {
	for _, c := range []struct {
		spec string
		name string
		ok   bool
	}{
		{"runner", "runner", true},
		{"1000", "runner", true}, // a uid with an entry: its name
		{"1001", "run", true},
		{"4242", "", false},       // a uid with no entry: the primary only
		{"nobody", "", false},     // not in passwd
		{"runner:999", "", false}, // an explicit group: that group only, as podman
		{"1000:1000", "", false},  // likewise
		{"", "", false},           // root
		{"root", "", false},       // root
		{"0", "", false},          // root
	} {
		name, ok := MemberName(c.spec, []byte(testPasswd))
		if name != c.name || ok != c.ok {
			t.Errorf("MemberName(%q) = %q, %v; want %q, %v", c.spec, name, ok, c.name, c.ok)
		}
	}
}

// The name a numeric uid resolves to is what is matched in /etc/group.
func TestGroupsByUID(t *testing.T) {
	name, ok := MemberName("1000", []byte(testPasswd))
	if !ok {
		t.Fatal("uid 1000 not found")
	}
	if got := Groups(name, 1000, []byte(testGroup)); !slices.Equal(got, []int{1000, 4, 999, 3000}) {
		t.Fatalf("got %v", got)
	}
}

func TestLookupUnchanged(t *testing.T) {
	u, err := Lookup("runner", []byte(testPasswd))
	if err != nil || u != (User{Name: "runner", UID: 1000, GID: 1000, Home: "/home/runner"}) {
		t.Fatalf("Lookup = %+v, %v", u, err)
	}
	u, err = Lookup("1000:999", []byte(testPasswd))
	if err != nil || u.UID != 1000 || u.GID != 999 {
		t.Fatalf("Lookup uid:gid = %+v, %v", u, err)
	}
}
