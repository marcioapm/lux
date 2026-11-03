package cli

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/server"
)

type tarEntry struct {
	hdr  tar.Header
	body string
}

func zstdTar(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		h := e.hdr
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// snapFake is a luxd answering a Run's snapshots and volume URLs, and an
// "S3" serving blobs; it records the Authorization S3 got.
type snapFake struct {
	t         *testing.T
	snapshots []server.Snapshot
	blobs     map[string][]byte // blob id → bytes S3 serves
	mu        sync.Mutex
	s3Auth    []string
	urls      []string // volume URL requests to luxd
	lux, s3   *httptest.Server
}

func newSnapFake(t *testing.T, snaps []server.Snapshot, blobs map[string][]byte) *snapFake {
	f := &snapFake{t: t, snapshots: snaps, blobs: blobs}
	f.s3 = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.s3Auth = append(f.s3Auth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		b, ok := f.blobs[strings.TrimPrefix(r.URL.Path, "/b/")]
		if !ok || r.URL.Query().Get("sig") != "ok" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write(b)
	}))
	f.lux = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/runs/run_1/snapshots" {
			_ = json.NewEncoder(w).Encode(map[string]any{"snapshots": f.snapshots})
			return
		}
		f.mu.Lock()
		f.urls = append(f.urls, r.URL.RequestURI())
		f.mu.Unlock()
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/runs/run_1/snapshots/"), "/")
		if len(parts) != 3 || parts[1] != "volumes" || r.URL.Query().Get("redirect") != "false" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"not found"}}`))
			return
		}
		for _, s := range f.snapshots {
			for _, v := range s.Manifest.Volumes {
				if s.ID == parts[0] && v.Name == parts[2] {
					_ = json.NewEncoder(w).Encode(server.SnapshotVolumeURL{URL: f.s3.URL + "/b/" + v.BlobID + "?sig=ok",
						ExpiresAt: time.Now().Add(15 * time.Minute), Size: v.Size, SHA256: v.SHA256, Name: v.Name, SnapshotID: s.ID, BlobID: v.BlobID})
					return
				}
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"no such volume"}}`))
	}))
	t.Cleanup(f.lux.Close)
	t.Cleanup(f.s3.Close)
	return f
}

// run runs lux with args in dir and returns stdout, stderr and the error.
func (f *snapFake) run(dir string, args ...string) (string, string, error) {
	f.t.Helper()
	wd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		f.t.Fatal(err)
	}
	defer os.Chdir(wd)
	var out, errOut strings.Builder
	a := &app{stdin: strings.NewReader(""), stdout: &out, stderr: &errOut}
	root := a.root()
	root.SetArgs(append([]string{"--url", f.lux.URL, "--api-key", "secret-key"}, args...))
	root.SetErr(io.Discard)
	err := root.Execute()
	return out.String(), errOut.String(), err
}

func snapWith(id string, epoch int, available, uploaded bool, vols ...proto.VolumeSnapshot) server.Snapshot {
	return server.Snapshot{ID: id, Epoch: epoch, Available: available, Uploaded: uploaded, CreatedAt: time.Now(),
		Manifest: proto.Manifest{SnapshotID: id, Volumes: vols}}
}

func vol(name, blobID string, b []byte) proto.VolumeSnapshot {
	return proto.VolumeSnapshot{Name: name, Path: "/" + name, BlobID: blobID, Size: int64(len(b)), SHA256: sum(b)}
}

func listFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(dir, func(p string, _ os.FileInfo, _ error) error {
		if p != dir {
			r, _ := filepath.Rel(dir, p)
			out = append(out, r)
		}
		return nil
	})
	return out
}

// Raw download: the newest available and uploaded snapshot by default
// (named on stderr), every volume, ./<snap>-<volume>.tar.zst; the API key
// never goes to S3; nothing is overwritten.
func TestSnapshotsDownloadRaw(t *testing.T) {
	isolateConfig(t, "")
	work, home, old := []byte("work-bytes"), []byte("home-bytes"), []byte("old")
	f := newSnapFake(t, []server.Snapshot{
		snapWith("snap1", 1, true, true, vol("work", "b1", old)),
		snapWith("snap2", 2, true, true, vol("work", "b2w", work), vol("home", "b2h", home)),
		snapWith("snap3", 3, true, false, vol("work", "b3", old)),
		snapWith("snap4", 4, false, true, vol("work", "b4", old)),
	}, map[string][]byte{"b1": old, "b2w": work, "b2h": home, "b3": old, "b4": old})
	dir := t.TempDir()
	stdout, stderr, err := f.run(dir, "snapshots", "download", "run_1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "snap2") {
		t.Fatalf("stderr %q: want the snapshot picked (snap2)", stderr)
	}
	for name, want := range map[string][]byte{"snap2-work.tar.zst": work, "snap2-home.tar.zst": home} {
		if b, err := os.ReadFile(filepath.Join(dir, name)); err != nil || !bytes.Equal(b, want) {
			t.Fatalf("%s: %q %v", name, b, err)
		}
		if !strings.Contains(stdout, name) {
			t.Fatalf("stdout %q: no %s", stdout, name)
		}
	}
	if len(listFiles(t, dir)) != 2 {
		t.Fatalf("files %v", listFiles(t, dir))
	}
	for _, a := range f.s3Auth {
		if a != "" {
			t.Fatalf("S3 got Authorization %q", a)
		}
	}
	if len(f.s3Auth) != 2 {
		t.Fatalf("S3 requests %d", len(f.s3Auth))
	}
	// Again: refused, the files unchanged.
	if _, _, err := f.run(dir, "snapshots", "download", "run_1"); err == nil || !strings.Contains(err.Error(), "not overwriting") {
		t.Fatalf("second download: %v, want refused", err)
	}

	// One volume, --out a file; -o json says what was written.
	out := filepath.Join(t.TempDir(), "w.tzst")
	stdout, _, err = f.run(dir, "snapshots", "download", "run_1", "--snapshot", "snap1", "--volume", "work", "--out", out, "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var written []snapshotFile
	if err := json.Unmarshal([]byte(stdout), &written); err != nil || len(written) != 1 || written[0].Path != out ||
		written[0].Snapshot != "snap1" || written[0].SHA256 != sum(old) || written[0].Size != 3 {
		t.Fatalf("json %q %v", stdout, err)
	}
	if b, _ := os.ReadFile(out); !bytes.Equal(b, old) {
		t.Fatalf("%s: %q", out, b)
	}
	// Several volumes, --out a directory (created).
	outDir := filepath.Join(t.TempDir(), "d")
	if _, _, err := f.run(dir, "snapshots", "download", "run_1", "--snapshot", "snap2", "--out", outDir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(outDir, "home.tar.zst")); !bytes.Equal(b, home) {
		t.Fatalf("home.tar.zst: %q", b)
	}
	// An unknown volume or snapshot.
	if _, _, err := f.run(dir, "snapshots", "download", "run_1", "--volume", "nope"); err == nil {
		t.Fatal("unknown volume accepted")
	}
	if _, _, err := f.run(dir, "snapshots", "download", "run_1", "--snapshot", "snap9"); err == nil {
		t.Fatal("unknown snapshot accepted")
	}
}

// No snapshot available and uploaded: a clear error, no request for a URL.
func TestSnapshotsDownloadNone(t *testing.T) {
	isolateConfig(t, "")
	f := newSnapFake(t, []server.Snapshot{snapWith("snap1", 1, false, true, vol("work", "b1", []byte("x")))}, nil)
	_, _, err := f.run(t.TempDir(), "snapshots", "download", "run_1")
	if err == nil || !strings.Contains(err.Error(), "no snapshot that is available and uploaded") {
		t.Fatalf("err %v", err)
	}
	if len(f.urls) != 0 {
		t.Fatalf("asked for %v", f.urls)
	}
}

// What S3 serves must match the manifest's size and sha256: a mismatch is
// an error naming both, and leaves no file (raw) or directory (extract).
func TestSnapshotsDownloadMismatch(t *testing.T) {
	isolateConfig(t, "")
	good := zstdTar(t, tarEntry{hdr: tar.Header{Name: "a.txt"}, body: "a"})
	bad := append([]byte{}, good...)
	bad[len(bad)-1] ^= 0xff
	f := newSnapFake(t, []server.Snapshot{snapWith("snap1", 1, true, true, vol("work", "b1", good))}, map[string][]byte{"b1": bad})
	dir := t.TempDir()
	_, _, err := f.run(dir, "snapshots", "download", "run_1")
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") || !strings.Contains(err.Error(), sum(good)) || !strings.Contains(err.Error(), sum(bad)) {
		t.Fatalf("err %v", err)
	}
	ext := filepath.Join(dir, "x")
	if _, _, err := f.run(dir, "snapshots", "download", "run_1", "--extract", ext); err == nil {
		t.Fatal("extract of a mismatch accepted")
	}
	if files := listFiles(t, dir); len(files) != 1 || files[0] != "x" {
		t.Fatalf("left behind %v", files)
	}
	f.blobs["b1"] = good[:len(good)-1]
	if _, _, err := f.run(dir, "snapshots", "download", "run_1"); err == nil || !strings.Contains(err.Error(), "size mismatch") {
		t.Fatalf("short: %v", err)
	}
	if files := listFiles(t, dir); len(files) != 1 {
		t.Fatalf("left behind %v", files)
	}
}

// --extract unpacks a real zstd tar into DIR/<volume>/: files, dirs,
// modes (setuid dropped), symlinks; devices and fifos are skipped; a
// non-empty DIR/<volume> is refused.
func TestSnapshotsDownloadExtract(t *testing.T) {
	isolateConfig(t, "")
	tz := zstdTar(t,
		tarEntry{hdr: tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755}},
		tarEntry{hdr: tar.Header{Name: "src/", Typeflag: tar.TypeDir, Mode: 0o755}},
		tarEntry{hdr: tar.Header{Name: "src/main.go", Mode: 0o644}, body: "package main\n"},
		tarEntry{hdr: tar.Header{Name: "priv/", Typeflag: tar.TypeDir, Mode: 0o700}},
		tarEntry{hdr: tar.Header{Name: "bin/tool", Mode: 0o4755}, body: "#!/bin/sh\n"},
		tarEntry{hdr: tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "src/main.go"}},
		tarEntry{hdr: tar.Header{Name: "hard", Typeflag: tar.TypeLink, Linkname: "src/main.go"}},
		tarEntry{hdr: tar.Header{Name: "fifo", Typeflag: tar.TypeFifo}},
		tarEntry{hdr: tar.Header{Name: "dev", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3}},
	)
	f := newSnapFake(t, []server.Snapshot{snapWith("snap1", 1, true, true, vol("workspace", "b1", tz))}, map[string][]byte{"b1": tz})
	dir := t.TempDir()
	ext := filepath.Join(dir, "out")
	if _, _, err := f.run(dir, "snapshots", "download", "run_1", "--extract", ext); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(ext, "workspace")
	if b, err := os.ReadFile(filepath.Join(root, "src/main.go")); err != nil || string(b) != "package main\n" {
		t.Fatalf("main.go %q %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "hard")); err != nil || string(b) != "package main\n" {
		t.Fatalf("hard %q %v", b, err)
	}
	if l, err := os.Readlink(filepath.Join(root, "link")); err != nil || l != "src/main.go" {
		t.Fatalf("link %q %v", l, err)
	}
	fi, err := os.Stat(filepath.Join(root, "bin/tool"))
	if err != nil || fi.Mode()&os.ModeSetuid != 0 || fi.Mode().Perm() != 0o755 {
		t.Fatalf("tool mode %v %v", fi.Mode(), err)
	}
	if fi, err := os.Stat(filepath.Join(root, "priv")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("priv mode %v %v", fi.Mode(), err)
	}
	for _, skipped := range []string{"fifo", "dev"} {
		if _, err := os.Lstat(filepath.Join(root, skipped)); err == nil {
			t.Fatalf("%s extracted", skipped)
		}
	}
	// No temp file left next to DIR.
	if files, _ := os.ReadDir(dir); len(files) != 1 {
		t.Fatalf("%s holds %v", dir, files)
	}
	if _, _, err := f.run(dir, "snapshots", "download", "run_1", "--extract", ext); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("again: %v, want refused", err)
	}
}

// A hostile archive is refused, writing nothing outside the volume's
// directory (and leaving no directory behind).
func TestSnapshotsDownloadExtractHostile(t *testing.T) {
	isolateConfig(t, "")
	wantErr := map[string]string{
		"hard link outside":         "outside the volume",
		"hard link through symlink": "through a symlink",
		"hard link to a symlink":    "not a regular file",
	}
	for name, entries := range map[string][]tarEntry{
		"absolute": {{hdr: tar.Header{Name: "/escape"}, body: "x"}},
		"dotdot":   {{hdr: tar.Header{Name: "a/../../escape"}, body: "x"}},
		"through symlink": {
			{hdr: tar.Header{Name: "l", Typeflag: tar.TypeSymlink, Linkname: ".."}},
			{hdr: tar.Header{Name: "l/escape"}, body: "x"},
		},
		"through nested symlink": {
			{hdr: tar.Header{Name: "d/", Typeflag: tar.TypeDir}},
			{hdr: tar.Header{Name: "d/l", Typeflag: tar.TypeSymlink, Linkname: "../.."}},
			{hdr: tar.Header{Name: "d/l/escape"}, body: "x"},
		},
		"dir over symlink": {
			{hdr: tar.Header{Name: "l", Typeflag: tar.TypeSymlink, Linkname: ".."}},
			{hdr: tar.Header{Name: "l/", Typeflag: tar.TypeDir}},
		},
		"hard link outside": {{hdr: tar.Header{Name: "h", Typeflag: tar.TypeLink, Linkname: "../../victim"}}},
		"hard link through symlink": {
			{hdr: tar.Header{Name: "l", Typeflag: tar.TypeSymlink, Linkname: ".."}},
			{hdr: tar.Header{Name: "h", Typeflag: tar.TypeLink, Linkname: "l/victim"}},
		},
		"hard link to a symlink": {
			{hdr: tar.Header{Name: "s", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}},
			{hdr: tar.Header{Name: "h", Typeflag: tar.TypeLink, Linkname: "s"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			tz := zstdTar(t, entries...)
			f := newSnapFake(t, []server.Snapshot{snapWith("snap1", 1, true, true, vol("v", "b1", tz))}, map[string][]byte{"b1": tz})
			dir := t.TempDir()
			ext := filepath.Join(dir, "out")
			// Victims next to DIR's parent and in it, for the hard links to reach.
			victims := []string{filepath.Join(dir, "victim"), filepath.Join(ext, "victim")}
			for _, v := range victims {
				if err := os.MkdirAll(filepath.Dir(v), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(v, []byte("victim"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			_, _, err := f.run(dir, "snapshots", "download", "run_1", "--extract", ext)
			if err == nil {
				t.Fatal("accepted")
			}
			if want := wantErr[name]; want != "" && !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q, want %q", err, want)
			}
			if _, err := os.Lstat(filepath.Join(dir, "escape")); err == nil {
				t.Fatal("wrote outside the volume")
			}
			for _, v := range victims {
				if n := linkCount(t, v); n != 1 {
					t.Fatalf("%s has %d links", v, n)
				}
			}
			if _, err := os.Lstat(filepath.Join(ext, "v")); err == nil {
				t.Fatalf("left %v", listFiles(t, ext))
			}
		})
	}
	// A symlink pointing outside is kept as a link, never followed.
	tz := zstdTar(t,
		tarEntry{hdr: tar.Header{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "../../target"}},
		tarEntry{hdr: tar.Header{Name: "l"}, body: "replaced"},
	)
	f := newSnapFake(t, []server.Snapshot{snapWith("snap1", 1, true, true, vol("v", "b1", tz))}, map[string][]byte{"b1": tz})
	dir := t.TempDir()
	if _, _, err := f.run(dir, "snapshots", "download", "run_1", "--extract", filepath.Join(dir, "out")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "target")); err == nil {
		t.Fatal("wrote through the symlink")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "out/v/l")); string(b) != "replaced" {
		t.Fatalf("l: %q", b)
	}
}

// linkCount is the number of hard links to file p.
func linkCount(t *testing.T, p string) uint64 {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return uint64(fi.Sys().(*syscall.Stat_t).Nlink)
}

// lux snapshots <run> still lists.
func TestSnapshotsListStill(t *testing.T) {
	isolateConfig(t, "")
	f := newSnapFake(t, []server.Snapshot{snapWith("snap1", 1, true, true, vol("work", "b1", []byte("x")))}, nil)
	stdout, _, err := f.run(t.TempDir(), "snapshots", "run_1")
	if err != nil || !strings.Contains(stdout, "snap1") || !strings.Contains(stdout, "s3") {
		t.Fatalf("%q %v", stdout, err)
	}
}
