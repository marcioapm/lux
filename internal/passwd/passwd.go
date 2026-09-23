// Package passwd resolves a container user ("name", "uid" or "uid:gid")
// against an image's /etc/passwd. Shared by the shim, which switches to
// the user, and the runner, which prepares files for it.
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
