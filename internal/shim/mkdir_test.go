package shim

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
)

// The shim creates missing parents for the workload: the workload's own
// under its home or a volume, root's elsewhere, and never touches a
// directory that already existed. Needs root, as the shim always is:
//
//	docker run --rm -v $PWD:/src -w /src golang go test ./internal/shim -run MkdirForWorkload
func TestMkdirForWorkload(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root to chown")
	}
	root := t.TempDir()
	home := filepath.Join(root, "home", "agent")
	vol := filepath.Join(root, "workspace")
	for _, d := range []string{home, vol, filepath.Join(home, "existing")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s := &Shim{
		user: &userInfo{uid: 1000, gid: 1000, home: home},
		cfg:  proto.ShimConfig{VolumePaths: []string{vol}},
	}
	for _, d := range []string{
		filepath.Join(home, ".config", "tool"),
		filepath.Join(home, "existing", "sub"),
		filepath.Join(vol, "a", "b"),
		filepath.Join(root, "etc", "tool"),
	} {
		if err := s.mkdirForWorkload(d); err != nil {
			t.Fatal(err)
		}
	}
	uid := func(p string) uint32 {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Sys().(*syscall.Stat_t).Uid
	}
	for p, want := range map[string]uint32{
		filepath.Join(home, ".config"):         1000,
		filepath.Join(home, ".config", "tool"): 1000,
		filepath.Join(home, "existing"):        0, // existed: untouched
		filepath.Join(home, "existing", "sub"): 1000,
		filepath.Join(vol, "a", "b"):           1000,
		filepath.Join(root, "etc"):             0, // outside home and volumes
		filepath.Join(root, "etc", "tool"):     0,
		home:                                   0,
	} {
		if got := uid(p); got != want {
			t.Errorf("%s: uid %d, want %d", p, got, want)
		}
	}
}
