// Package passwd resolves a container user ("name", "uid" or "uid:gid")
// against an image's /etc/passwd, and its supplementary groups against the
// image's /etc/group. Lookup is shared by the shim, which switches to the
// user, and the runner, which prepares files for it; the runner alone
// reads /etc/group (Resolve, Groups).
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
	u, _, err := Resolve(spec, passwd)
	return u, err
}

// Resolve is Lookup, from the same scan of passwd, plus member: the name
// whose /etc/group memberships the user gets, as runc's GetExecUser gives
// them. member is the name of the first entry whose name is spec's user
// or, for a numeric user, whose uid equals it as a number ("1000",
// "01000", "+1000"), as runc matches it; it is
// empty for root, for a user with no entry, for one whose entry has uid 0,
// and when spec has a non-empty group ("runner:4002" gets that group only;
// "runner:" gets memberships).
//
// The user's uid and primary gid are Lookup's, which matches an entry's
// uid as text: "01000" finds no entry there, so it runs as 1000:1000 even
// if uid 1000's entry has another primary gid, while its memberships are
// that entry's.
func Resolve(spec string, passwd []byte) (u User, member string, err error) {
	if spec == "" || spec == "root" || spec == "0" {
		return User{Name: "root", Home: "/root"}, "", nil
	}
	name, group, _ := strings.Cut(spec, ":")
	u = User{Name: name, Home: "/"}
	num, numErr := strconv.Atoi(name)
	found, wantMember := false, group == ""
	for _, l := range strings.Split(string(passwd), "\n") {
		if found && !wantMember {
			break
		}
		f := strings.Split(l, ":")
		if len(f) < 7 {
			continue
		}
		if !found && (f[0] == name || f[2] == name) {
			u.Name, u.Home = f[0], f[5]
			u.UID, _ = strconv.Atoi(f[2])
			u.GID, _ = strconv.Atoi(f[3])
			found = true
		}
		if wantMember && f[0] != "" {
			uid, err := strconv.Atoi(f[2])
			if f[0] == name || (numErr == nil && err == nil && uid == num) {
				if err == nil && uid != 0 {
					member = f[0]
				}
				wantMember = false
			}
		}
	}
	if !found {
		n, err := strconv.Atoi(name)
		if err != nil {
			return User{}, "", fmt.Errorf("user %q is not in the image's /etc/passwd", name)
		}
		u.UID, u.GID = n, n
	}
	if group != "" {
		g, err := strconv.Atoi(group)
		if err != nil {
			return User{}, "", errors.New("group must be numeric")
		}
		u.GID = g
	}
	if u.UID == 0 {
		member = "" // root keeps its primary group only
	}
	return u, member, nil
}

// Groups are the supplementary gids of user name with primary gid: in file
// order, each gid of /etc/group (its contents) whose member list names the
// user, each once and never the primary. As runc's GetAdditionalGroups, a
// member must equal name exactly; lines that are empty, comments, or
// without four fields and a numeric gid are skipped. No name, or an empty
// or missing file, gives none.
func Groups(name string, primary int, group []byte) []int {
	var gids []int
	if name == "" {
		return nil
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
