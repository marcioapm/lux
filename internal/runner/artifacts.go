package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
)

// Artifacts are files a Run produces, collected on every exit:
//
//   - files matching the spec's artifacts.paths (globs, ** for any depth),
//     which must be on the Run's volumes;
//   - whatever the workload put in $LUX_ARTIFACTS (the runtime volume's
//     artifacts directory), published under /.lux/artifacts/<name>.
//
// The runner reads them from the host side of the volumes, never following
// a symlink (the workload controls those files, and a link could point
// anywhere on the host). Each becomes a blob, uploaded like the snapshot.

// Limits: an artifact set is for results, not a second snapshot.
const (
	maxArtifacts     = 1000
	maxArtifactBytes = 1 << 30 // per file
)

func (p *placement) collectArtifacts(ctx context.Context) ([]proto.Artifact, error) {
	var out []proto.Artifact
	var errs []error
	add := func(hostFile, name string) {
		if len(out) >= maxArtifacts {
			errs = append(errs, fmt.Errorf("more than %d artifacts: %s and later ones skipped", maxArtifacts, name))
			return
		}
		a, err := p.artifactBlob(hostFile, name)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			return
		}
		out = append(out, a)
	}
	var paths []string
	if p.assign != nil {
		paths = p.assign.Spec.Artifacts.Paths
	}
	for _, pattern := range paths {
		root := p.volumeRoot(globBase(pattern)) // the volume's path
		if root == "" {
			errs = append(errs, fmt.Errorf("%s is not on a volume", pattern))
			continue
		}
		// Walk from the volume's root: nothing in the path to the pattern
		// (a symlinked directory the workload made) can lead the walk out.
		hostRoot, err := p.hostPath(root)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		err = walkFiles(hostRoot, func(hostFile string) {
			rel, _ := filepath.Rel(hostRoot, hostFile)
			name := path.Join(root, filepath.ToSlash(rel))
			if globMatch(pattern, name) {
				add(hostFile, name)
			}
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	// Published on demand, in $LUX_ARTIFACTS: collected once, then cleared
	// (the runtime volume outlives the placement).
	if rt, err := p.r.mountpoint(ctx, runtimeVolume(p.runID)); err == nil {
		dir := filepath.Join(rt, "artifacts")
		_ = walkFiles(dir, func(hostFile string) {
			rel, _ := filepath.Rel(dir, hostFile)
			add(hostFile, path.Join("/.lux/artifacts", filepath.ToSlash(rel)))
		})
		if entries, err := os.ReadDir(dir); err == nil {
			for _, e := range entries {
				_ = os.RemoveAll(filepath.Join(dir, e.Name()))
			}
		}
	}
	return out, errors.Join(errs...)
}

// artifactBlob stores one file as a blob (zstd, like every blob).
func (p *placement) artifactBlob(hostFile, name string) (proto.Artifact, error) {
	f, err := openNoFollow(hostFile)
	if err != nil {
		return proto.Artifact{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return proto.Artifact{}, err
	}
	if !fi.Mode().IsRegular() {
		return proto.Artifact{}, errors.New("not a regular file")
	}
	if fi.Size() > maxArtifactBytes {
		return proto.Artifact{}, fmt.Errorf("%d bytes: over the %d-byte limit", fi.Size(), maxArtifactBytes)
	}
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	ctype := mime.TypeByExtension(path.Ext(name))
	if ctype == "" {
		ctype = http.DetectContentType(head[:n])
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return proto.Artifact{}, err
	}
	blobID := ids.New(ids.Blob)
	size, sum, err := p.r.writeBlob(blobID, func(w io.Writer) error {
		_, err := io.Copy(w, io.LimitReader(f, maxArtifactBytes))
		return err
	})
	if err != nil {
		return proto.Artifact{}, err
	}
	return proto.Artifact{BlobInfo: proto.BlobInfo{BlobID: blobID, Size: size, SHA256: sum}, Path: name, ContentType: ctype}, nil
}

// walkFiles calls fn for each regular file under root, never following
// symlinks (links, to files or directories, are skipped).
func walkFiles(root string, fn func(string)) error {
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root && errors.Is(err, fs.ErrNotExist) {
				return filepath.SkipAll
			}
			return nil // an unreadable corner; the rest still counts
		}
		if d.Type().IsRegular() {
			fn(p)
		}
		return nil
	})
	return err
}

// openNoFollow opens a file, refusing a symlink in its last component
// (swapped in after the walk).
func openNoFollow(p string) (*os.File, error) {
	return os.OpenFile(p, os.O_RDONLY|oNoFollow, 0)
}

// globBase is the directory part of a pattern before its first wildcard.
func globBase(pattern string) string {
	i := strings.IndexAny(pattern, "*?[")
	if i < 0 {
		return pattern
	}
	return path.Dir(pattern[:i] + "x")
}

// globMatch matches a slash path against a pattern where * matches within
// a path segment and ** any number of segments.
func globMatch(pattern, name string) bool {
	ps, ns := strings.Split(strings.Trim(pattern, "/"), "/"), strings.Split(strings.Trim(name, "/"), "/")
	var match func(ps, ns []string) bool
	match = func(ps, ns []string) bool {
		for len(ps) > 0 {
			if ps[0] == "**" {
				for i := 0; i <= len(ns); i++ {
					if match(ps[1:], ns[i:]) {
						return true
					}
				}
				return false
			}
			if len(ns) == 0 {
				return false
			}
			if ok, _ := path.Match(ps[0], ns[0]); !ok {
				return false
			}
			ps, ns = ps[1:], ns[1:]
		}
		return len(ns) == 0
	}
	return match(ps, ns)
}
