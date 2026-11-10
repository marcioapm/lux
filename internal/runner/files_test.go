package runner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/egress"
	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// A placement hands the Assign's files, bytes and all, to its shim config,
// on a first placement and a resume alike; state.json and the placement's
// own Assign keep none of them.
func TestFilesReachTheShimConfigOnly(t *testing.T) {
	secret := "skill-body-" + strings.Repeat("x", 32)
	files := []spec.File{{Path: "~/.claude/skills/a/SKILL.md", Mode: 0o644, Data: base64.StdEncoding.EncodeToString([]byte(secret))}}
	stored, _ := spec.RunSpec{Files: files}.SplitFiles()
	for _, resume := range []*proto.ResumeInfo{nil, {SessionID: "s1"}} {
		t.Run(map[bool]string{true: "first", false: "resume"}[resume == nil], func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "podman")
			if err := os.WriteFile(bin, []byte(podmanForAStart), 0o755); err != nil {
				t.Fatal(err)
			}
			for _, d := range []string{"rt", "podman.root", "runs"} {
				if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			r := &Runner{cfg: Config{DataDir: dir, Shim: "/bin/true"}, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
				pm: &podman.Podman{Bin: bin}, placements: map[string]*placement{}, egress: egress.Unloaded(),
				images: newImageUse(dir)}
			r.conn = newConn(r)
			r.conn.polling = true
			r.mounts.Store(runtimeVolume("run1"), filepath.Join(dir, "rt"))
			luxd := &fakeLuxd{r: r}
			ctx, cancel := context.WithCancel(context.Background())
			var wg sync.WaitGroup
			wg.Go(func() { luxd.poll(ctx) })
			sp := spec.RunSpec{Image: spec.Image{Ref: "img"}, Network: spec.Network{Unrestricted: true}, Files: stored.Files}
			p := newPlacement(r, proto.Assign{RunID: "run1", TenantID: "t1", Epoch: 1, Spec: sp, Resume: resume, Files: files}, nil)
			r.placements["run1"] = p
			go p.run(ctx)
			t.Cleanup(func() {
				cancel()
				wg.Wait()
				select {
				case <-p.done:
				case <-time.After(45 * time.Second):
					t.Error("the placement did not end")
				}
			})
			awaitStart(t, "the container started", func() bool { return calledFirst(bin, "start") },
				func() string { return strings.Join(podmanCalls(bin), "; ") })

			b, err := os.ReadFile(filepath.Join(dir, "rt", "config.json"))
			if err != nil {
				t.Fatal(err)
			}
			var cfg proto.ShimConfig
			if err := json.Unmarshal(b, &cfg); err != nil {
				t.Fatal(err)
			}
			if len(cfg.Files) != 1 || cfg.Files[0].Path != files[0].Path || cfg.Files[0].Data != files[0].Data || cfg.Files[0].Mode != 0o644 {
				t.Fatalf("shim config files %+v", cfg.Files)
			}
			if p.assign.Files != nil {
				t.Fatal("the placement kept the files' bytes in its Assign")
			}
			// Nothing else the runner writes holds the bytes.
			_ = filepath.WalkDir(filepath.Join(dir, "runs"), func(path string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					if b, _ := os.ReadFile(path); strings.Contains(string(b), files[0].Data) {
						t.Errorf("%s holds the files' bytes", path)
					}
				}
				return nil
			})
			if !slices.ContainsFunc(podmanCalls(bin), func(l string) bool { return strings.HasPrefix(l, "create ") }) {
				t.Fatal("no container made")
			}
		})
	}
}

// Hello offers files only when the --shim lists them: this tree's does,
// an older one (no capabilities subcommand, or one without files-v1) not.
func TestShimKnowsFiles(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "lux-shim")
	if out, err := exec.Command("go", "build", "-o", shim, "github.com/marcioapm/lux/cmd/lux-shim").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	if !shimKnowsFiles(shim) {
		t.Fatal("this tree's lux-shim: no files")
	}
	older := filepath.Join(dir, "older")
	script := "#!/bin/sh\n[ \"$1\" = capabilities ] && { echo '[\"" + proto.ShimCapReclaim + "\"]'; exit 0; }\nexit 125\n"
	if err := os.WriteFile(older, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if shimKnowsFiles(older) || shimKnowsFiles(filepath.Join(dir, "missing")) {
		t.Fatal("an older or missing lux-shim: files")
	}
	if caps := (&Runner{shimFiles: true}).capabilities(); !slices.Contains(caps, proto.CapFiles) {
		t.Fatalf("Hello with a files shim: %v", caps)
	}
	if caps := (&Runner{}).capabilities(); slices.Contains(caps, proto.CapFiles) {
		t.Fatalf("Hello with an older shim: %v", caps)
	}
}
