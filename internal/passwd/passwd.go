// Package passwd resolves a container user ("name", "uid" or "uid:gid")
// against an image's /etc/passwd, and its supplementary groups against the
// image's /etc/group. Shared by the shim, which switches to the user, and
// the runner, which prepares files for it.
package passwd

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type User struct {
	Name     string
	UID, GID int
	Home     string
}

// Lookup resolves spec against the contents of /etc/passwd. Empty, "root"
// or "0" is root.
func Lookup(spec string, passwd []byte) (User, error) {
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

// MemberName is the user name whose /etc/group memberships a process
// started as spec gets, as runc and podman give them: a name or a uid with
// an /etc/passwd entry, and no group in spec. ok is false for root, for a
// uid with no entry and for "user:group" (which gets that group only).
func MemberName(spec string, passwd []byte) (name string, ok bool) {
	if spec == "" || spec == "root" || spec == "0" || strings.Contains(spec, ":") {
		return "", false
	}
	for _, l := range strings.Split(string(passwd), "\n") {
		f := strings.Split(l, ":")
		if len(f) >= 7 && f[0] != "" && (f[0] == spec || f[2] == spec) {
			return f[0], true
		}
	}
	return "", false
}

// Groups is the process's group list for user name with primary gid: the
// primary first, then, in file order, every group of /etc/group (its
// contents) whose member list names the user, each gid once. As runc's
// GetAdditionalGroups, a member must equal name exactly; lines that are
// empty, comments, or without four fields and a numeric gid are skipped.
// An empty or missing file gives the primary only.
func Groups(name string, primary int, group []byte) []int {
	gids := []int{primary}
	if name == "" {
		return gids
	}
	seen := map[int]bool{primary: true}
	for _, l := range strings.Split(string(group), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		f := strings.Split(l, ":")
		if len(f) < 4 {
			continue
		}
		gid, err := strconv.ParseUint(f[2], 10, 32)
		if err != nil || seen[int(gid)] {
			continue
		}
		for _, m := range strings.Split(f[3], ",") {
			if m == name {
				seen[int(gid)] = true
				gids = append(gids, int(gid))
				break
			}
		}
	}
	return gids
}
