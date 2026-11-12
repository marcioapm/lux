package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/marcioapm/lux/internal/passwd"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// The shim is given the image's USER as written alongside the uid:gid it
// switches to, so the workload gets that user's /etc/group memberships; a
// spec's user is passed as given and needs nothing alongside.
func TestShimConfigMemberUser(t *testing.T) {
	for _, c := range []struct {
		name, specUser, imageUser string
		user                      passwd.User
		wantUser, wantMember      string
	}{
		{"image user by name", "", "runner", passwd.User{UID: 1000, GID: 1000}, "1000:1000", "runner"},
		{"image user by uid", "", "1000", passwd.User{UID: 1000, GID: 1000}, "1000:1000", "1000"},
		{"image user with a group", "", "runner:4002", passwd.User{UID: 1000, GID: 4002}, "1000:4002", "runner:4002"},
		{"image root", "", "", passwd.User{Name: "root"}, "0:0", ""},
		{"spec user", "runner", "other", passwd.User{UID: 1000, GID: 1000}, "runner", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			rt := t.TempDir()
			r := &Runner{mem: mustScale(t, 30*gib+gib/2, gib/2, 32*gib)}
			r.mounts.Store(runtimeVolume("run_1"), rt)
			p := &placement{r: r, runID: "run_1", epoch: 1, assign: &proto.Assign{}, state: &runState{}, user: c.user, imageUser: c.imageUser}
			sp := spec.RunSpec{Workload: spec.Workload{User: c.specUser}, Resources: spec.Resources{Memory: spec.Bytes(gib)}}
			if err := p.writeShimConfig(context.Background(), sp, nil); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(filepath.Join(rt, "config.json"))
			if err != nil {
				t.Fatal(err)
			}
			var cfg proto.ShimConfig
			if err := json.Unmarshal(b, &cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.User != c.wantUser || cfg.MemberUser != c.wantMember {
				t.Fatalf("user %q member %q, want %q %q", cfg.User, cfg.MemberUser, c.wantUser, c.wantMember)
			}
		})
	}
}
