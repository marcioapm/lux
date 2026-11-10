package cli

import (
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/marcioapm/lux/internal/spec"
)

// packFiles reads each file's content or source into its data, against
// dir (the spec file's directory; "" for the working directory), and adds
// the --copy LOCAL:REMOTE arguments. A source directory becomes a file
// per regular file under it, at path plus its relative name, each with
// its own mode unless one is given. Nothing is followed: a link, a
// device, a socket or a fifo is refused, whether named or found in a
// directory. The total is checked here too, to fail before uploading.
func packFiles(files []spec.File, dir string, copies []string) ([]spec.File, error) {
	for _, c := range copies {
		local, remote, ok := strings.Cut(c, ":")
		if !ok || local == "" || remote == "" {
			return nil, fmt.Errorf("--copy %q: need LOCAL:REMOTE, e.g. ./skills:~/.claude/skills", c)
		}
		// Relative to where lux runs, not to the spec file.
		abs, err := filepath.Abs(local)
		if err != nil {
			return nil, err
		}
		files = append(files, spec.File{Path: remote, Source: abs})
	}
	var out []spec.File
	var total int64
	for i, f := range files {
		at := fmt.Sprintf("files[%d]", i)
		if f.Source != "" && (f.Content != "" || f.Data != "") || f.Content != "" && f.Data != "" {
			return nil, fmt.Errorf("%s (%s): give one of data, content or source", at, f.Path)
		}
		switch {
		case f.Source != "":
			src := f.Source
			if !filepath.IsAbs(src) && dir != "" {
				src = filepath.Join(dir, src)
			}
			packed, err := packSource(f, src)
			if err != nil {
				return nil, fmt.Errorf("%s (%s): %w", at, f.Path, err)
			}
			for _, p := range packed {
				total += int64(base64.StdEncoding.DecodedLen(len(p.Data)))
			}
			out = append(out, packed...)
		case f.Content != "":
			f.Data = base64.StdEncoding.EncodeToString([]byte(f.Content))
			f.Content = ""
			total += int64(base64.StdEncoding.DecodedLen(len(f.Data)))
			out = append(out, f)
		default:
			out = append(out, f)
		}
		if total > spec.MaxFilesBytes {
			return nil, fmt.Errorf("files: more than %s in total (at %s); lux takes at most that", spec.BytesText(spec.MaxFilesBytes), f.Path)
		}
	}
	if len(out) > spec.MaxFiles {
		return nil, fmt.Errorf("files: %d files, at most %d", len(out), spec.MaxFiles)
	}
	return out, nil
}

// packSource reads src, a regular file or a directory, into files at f's
// path. Each file found takes f's mode, or its own permission bits.
func packSource(f spec.File, src string) ([]spec.File, error) {
	fi, err := os.Lstat(src)
	if err != nil {
		return nil, err
	}
	if fi.Mode().IsRegular() {
		g, err := readLocal(f, src, fi)
		return []spec.File{g}, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s: %s; lux copies regular files and directories, and follows no links", src, kindOf(fi))
	}
	var out []spec.File
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s: %s; lux copies regular files and directories, and follows no links", p, kindOf(info))
		}
		rel, _ := filepath.Rel(src, p)
		g := f
		g.Path = path.Join(f.Path, filepath.ToSlash(rel))
		g, err = readLocal(g, p, info)
		out = append(out, g)
		return err
	})
	if err == nil && len(out) == 0 {
		return nil, fmt.Errorf("%s: an empty directory (lux copies files)", src)
	}
	return out, err
}

// readLocal reads p into f's data; f's mode, or else p's permission bits.
// p is opened without following a link put there since it was looked at.
func readLocal(f spec.File, p string, fi fs.FileInfo) (spec.File, error) {
	if fi.Size() > spec.MaxFilesBytes {
		return f, fmt.Errorf("%s: %s, more than lux takes in total (%s)", p, spec.BytesText(fi.Size()), spec.BytesText(spec.MaxFilesBytes))
	}
	fh, err := openNoFollow(p)
	if err != nil {
		return f, err
	}
	defer fh.Close()
	b, err := io.ReadAll(io.LimitReader(fh, spec.MaxFilesBytes+1))
	if err != nil {
		return f, err
	}
	if f.Mode == 0 {
		f.Mode = spec.FileMode(fi.Mode().Perm())
	}
	f.Data = base64.StdEncoding.EncodeToString(b)
	f.Source = ""
	return f, nil
}

// openNoFollow opens p for reading, refusing a link at p (O_NOFOLLOW) and
// never blocking on a fifo swapped in (O_NONBLOCK), then checks that what
// it opened is a regular file.
func openNoFollow(p string) (*os.File, error) {
	fh, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if fi, err := fh.Stat(); err != nil || !fi.Mode().IsRegular() {
		fh.Close()
		if err == nil {
			err = fmt.Errorf("%s: %s", p, kindOf(fi))
		}
		return nil, err
	}
	return fh, nil
}

func kindOf(fi fs.FileInfo) string {
	m := fi.Mode()
	switch {
	case m&fs.ModeSymlink != 0:
		return "a symbolic link"
	case m&fs.ModeDevice != 0:
		return "a device"
	case m&fs.ModeNamedPipe != 0:
		return "a fifo"
	case m&fs.ModeSocket != 0:
		return "a socket"
	}
	return "not a regular file"
}

// specDir is the directory a spec file's relative sources are read from:
// its own; the working directory for stdin.
func specDir(file string) string {
	if file == "" || file == "-" {
		return ""
	}
	return filepath.Dir(file)
}
