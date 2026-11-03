package cli

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/spf13/cobra"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/server"
	"github.com/marcioapm/lux/internal/spec"
)

// snapshotFile is one volume written by snapshots download.
type snapshotFile struct {
	Snapshot  string `json:"snapshot"`
	Volume    string `json:"volume"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	Extracted bool   `json:"extracted"`
}

func (a *app) snapshotsDownloadCmd() *cobra.Command {
	var snap, out, extract string
	var volumes []string
	cmd := &cobra.Command{
		Use:   "download <run>",
		Short: "Download (or unpack) the volumes of a Run's snapshot",
		Long: `Download the volumes of a Run's snapshot, in any state of the Run while the
snapshot is available (a terminated Run's too, for its tenant's retention).
Each volume is a zstd-compressed tar; its size and sha256 are checked against
the snapshot's manifest, and a mismatch leaves nothing behind.

--snapshot defaults to the newest snapshot that is available and uploaded.
--out PATH writes the raw .tar.zst: PATH is the file for one volume, else a
directory of <volume>.tar.zst. Without --out or --extract, each goes to
./<snapshot>-<volume>.tar.zst. --extract DIR unpacks each into DIR/<volume>/.
Nothing existing is overwritten. Needs a key with the run scope.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, run := ctxOf(cmd), args[0]
			if out != "" && extract != "" {
				return errors.New("--out and --extract are exclusive")
			}
			sn, err := a.pickSnapshot(ctx, run, snap)
			if err != nil {
				return err
			}
			vols, err := pickVolumes(sn, volumes)
			if err != nil {
				return err
			}
			// Every destination checked before any download starts.
			dsts := make([]string, len(vols))
			for i, v := range vols {
				switch {
				case extract != "":
					dsts[i] = filepath.Join(extract, v.Name)
					if err := emptyOrAbsent(dsts[i]); err != nil {
						return err
					}
				case out == "":
					dsts[i] = sn.ID + "-" + v.Name + ".tar.zst"
				case len(vols) == 1 && !isDir(out):
					dsts[i] = out
				case len(vols) == 1:
					dsts[i] = filepath.Join(out, sn.ID+"-"+v.Name+".tar.zst")
				default:
					dsts[i] = filepath.Join(out, v.Name+".tar.zst")
				}
				if extract == "" {
					if _, err := os.Lstat(dsts[i]); err == nil {
						return fmt.Errorf("%s exists: not overwriting it", dsts[i])
					}
				}
			}
			var written []snapshotFile
			for i, v := range vols {
				f := snapshotFile{Snapshot: sn.ID, Volume: v.Name, Path: dsts[i], Size: v.Size, SHA256: v.SHA256, Extracted: extract != ""}
				if err := a.downloadVolume(ctx, run, sn.ID, v, dsts[i], extract != ""); err != nil {
					return fmt.Errorf("volume %s: %w", v.Name, err)
				}
				written = append(written, f)
				if a.output != "json" {
					fmt.Fprintf(a.stdout, "%s\t%s\n", dsts[i], spec.BytesText(v.Size))
				}
			}
			if a.output == "json" {
				return a.json(written)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&snap, "snapshot", "", "the snapshot's id (default: the newest available and uploaded)")
	cmd.Flags().StringArrayVar(&volumes, "volume", nil, "only this volume (repeatable; default: every volume)")
	cmd.Flags().StringVar(&out, "out", "", "write the raw .tar.zst here: a file for one volume, a directory for several")
	cmd.Flags().StringVar(&extract, "extract", "", "unpack each volume into this directory's <volume>/")
	return cmd
}

// pickSnapshot is the Run's snapshot id, or its newest available and
// uploaded one (said on stderr).
func (a *app) pickSnapshot(ctx context.Context, run, id string) (server.Snapshot, error) {
	var resp struct {
		Snapshots []server.Snapshot `json:"snapshots"`
	}
	if err := a.c.Do(ctx, "GET", "/v1/runs/"+run+"/snapshots", nil, &resp); err != nil {
		return server.Snapshot{}, err
	}
	if id != "" {
		for _, s := range resp.Snapshots {
			if s.ID == id {
				return s, nil
			}
		}
		return server.Snapshot{}, fmt.Errorf("run %s has no snapshot %s (lux snapshots %s lists them)", run, id, run)
	}
	for _, s := range slices.Backward(resp.Snapshots) {
		if s.Available && s.Uploaded {
			fmt.Fprintf(a.stderr, "snapshot %s (epoch %d, %s)\n", s.ID, s.Epoch, ago(&s.CreatedAt))
			return s, nil
		}
	}
	return server.Snapshot{}, fmt.Errorf("run %s has no snapshot that is available and uploaded (lux snapshots %s)", run, run)
}

func pickVolumes(sn server.Snapshot, names []string) ([]proto.VolumeSnapshot, error) {
	if len(names) == 0 {
		if len(sn.Manifest.Volumes) == 0 {
			return nil, fmt.Errorf("snapshot %s has no volumes", sn.ID)
		}
		return sn.Manifest.Volumes, nil
	}
	var out []proto.VolumeSnapshot
	for _, n := range names {
		i := slices.IndexFunc(sn.Manifest.Volumes, func(v proto.VolumeSnapshot) bool { return v.Name == n })
		if i < 0 {
			return nil, fmt.Errorf("snapshot %s has no volume %s", sn.ID, n)
		}
		if !slices.ContainsFunc(out, func(v proto.VolumeSnapshot) bool { return v.Name == n }) {
			out = append(out, sn.Manifest.Volumes[i])
		}
	}
	return out, nil
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func emptyOrAbsent(dir string) error {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(ents) > 0 {
		return fmt.Errorf("%s is not empty: not extracting into it", dir)
	}
	return nil
}

// downloadVolume fetches one volume's blob into a temp file, checks it
// against the manifest, then moves it to dst or unpacks it there.
func (a *app) downloadVolume(ctx context.Context, run, snap string, v proto.VolumeSnapshot, dst string, extract bool) error {
	var u server.SnapshotVolumeURL
	path := "/v1/runs/" + run + "/snapshots/" + url.PathEscape(snap) + "/volumes/" + url.PathEscape(v.Name) + "?redirect=false"
	if err := a.c.Do(ctx, "GET", path, nil, &u); err != nil {
		return err
	}
	tmpDir := filepath.Dir(dst)
	if extract {
		tmpDir = filepath.Dir(filepath.Clean(filepath.Dir(dst)))
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(tmpDir, ".lux-snapshot-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := fetchPresigned(ctx, u.URL, f, v.Size, v.SHA256); err != nil {
		return err
	}
	if extract {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		return extractVolume(f, dst)
	}
	if err := f.Chmod(0o666 &^ umask()); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// A link fails if dst appeared meanwhile: never an overwrite.
	return os.Link(f.Name(), dst)
}

// fetchPresigned GETs a presigned URL into w (no lux credentials: the URL
// is S3's) and checks what came against size and sha256.
func fetchPresigned(ctx context.Context, rawURL string, w io.Writer, size int64, sum string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("storage answered %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(resp.Body, size+1))
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("size mismatch: expected %d bytes, got %d", size, n)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum {
		return fmt.Errorf("sha256 mismatch: expected %s, got %s", sum, got)
	}
	return nil
}

// extractVolume unpacks a zstd tar into dir (absent or empty), refusing
// what would land outside it: absolute names, .. components, and paths
// through a symlink. Devices and fifos are skipped, setuid/setgid bits
// dropped, owners ignored. On an error dir is removed.
func extractVolume(r io.Reader, dir string) (err error) {
	created := false
	if _, statErr := os.Lstat(dir); errors.Is(statErr, fs.ErrNotExist) {
		created = true
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if created {
				os.RemoveAll(dir)
			} else {
				clearDir(dir)
			}
		}
	}()
	zr, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	type dirMode struct {
		path string
		mode os.FileMode
	}
	var dirs []dirMode
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		rel, err := safeRel(hdr.Name)
		if err != nil {
			return err
		}
		if rel == "" {
			continue
		}
		dst := filepath.Join(dir, rel)
		if err := noSymlinkParents(dir, rel); err != nil {
			return err
		}
		mode := os.FileMode(hdr.Mode).Perm()
		switch hdr.Typeflag {
		case tar.TypeDir:
			fi, err := os.Lstat(dst)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				if err := os.Mkdir(dst, 0o755); err != nil {
					return err
				}
			case err != nil:
				return err
			case !fi.IsDir():
				return fmt.Errorf("%s: a directory over a file or symlink", hdr.Name)
			}
			dirs = append(dirs, dirMode{dst, mode})
		case tar.TypeReg:
			if err := replaceable(dst, hdr.Name); err != nil {
				return err
			}
			f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if err == nil {
				err = f.Chmod(mode)
			}
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
			_ = os.Chtimes(dst, hdr.ModTime, hdr.ModTime)
		case tar.TypeSymlink:
			if err := replaceable(dst, hdr.Name); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, dst); err != nil {
				return err
			}
		case tar.TypeLink:
			target, err := safeRel(hdr.Linkname)
			if err != nil || target == "" {
				return fmt.Errorf("%s: hard link to %q outside the volume", hdr.Name, hdr.Linkname)
			}
			if err := noSymlinkParents(dir, target); err != nil {
				return err
			}
			if err := replaceable(dst, hdr.Name); err != nil {
				return err
			}
			fi, err := os.Lstat(filepath.Join(dir, target))
			switch {
			case err == nil && fi.Mode()&os.ModeSymlink != 0:
				// A hard link to a symlink is that symlink again: link(2)
				// would follow it on macOS, out of the volume.
				l, err := os.Readlink(filepath.Join(dir, target))
				if err == nil {
					err = os.Symlink(l, dst)
				}
				if err != nil {
					return err
				}
			case err != nil || !fi.Mode().IsRegular():
				return fmt.Errorf("%s: hard link to %q, which is not a regular file in the volume", hdr.Name, hdr.Linkname)
			default:
				if err := os.Link(filepath.Join(dir, target), dst); err != nil {
					return err
				}
			}
		default:
			// Devices, fifos, and headers that are not files.
		}
	}
	// Deepest first, so a read-only directory is closed after its contents.
	for _, d := range slices.Backward(dirs) {
		if err := os.Chmod(d.path, d.mode); err != nil {
			return err
		}
	}
	return nil
}

// safeRel is a tar entry's name as a local path ("" for the root), or an
// error for an absolute name or one with a .. component.
func safeRel(name string) (string, error) {
	if strings.HasPrefix(name, "/") || filepath.IsAbs(name) {
		return "", fmt.Errorf("%s: absolute path in the archive", name)
	}
	for _, c := range strings.Split(name, "/") {
		if c == ".." {
			return "", fmt.Errorf("%s: .. in the archive", name)
		}
	}
	rel := filepath.Clean(filepath.FromSlash(name))
	if rel == "." {
		return "", nil
	}
	if !filepath.IsLocal(rel) {
		return "", fmt.Errorf("%s: path outside the volume", name)
	}
	return rel, nil
}

// noSymlinkParents refuses rel when a directory on its way under dir is a
// symlink (one the archive made: dir started empty).
func noSymlinkParents(dir, rel string) error {
	p := dir
	parts := strings.Split(rel, string(filepath.Separator))
	for _, c := range parts[:len(parts)-1] {
		p = filepath.Join(p, c)
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			if err := os.Mkdir(p, 0o755); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s: path through a symlink in the archive", filepath.ToSlash(rel))
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s: path through a file in the archive", filepath.ToSlash(rel))
		}
	}
	return nil
}

// replaceable removes a file or symlink an earlier entry wrote at dst (a
// later entry wins, as with tar); a directory is refused.
func replaceable(dst, name string) error {
	fi, err := os.Lstat(dst)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return fmt.Errorf("%s: a file over a directory", name)
	}
	return os.Remove(dst)
}

// clearDir empties dir, which existed (empty) before an extraction.
func clearDir(dir string) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}
