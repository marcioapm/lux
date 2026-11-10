package spec

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// File is a file lux writes into the container on every start of the Run,
// before init: its bytes travel with the Run, not in it (runs.files), and
// the stored spec keeps only its path, mode, size and sha256.
type File struct {
	Path string   `json:"path" yaml:"path" doc:"Where it goes in the container: absolute, or ~/ for the workload user's home. No .. and no NUL; not under /.lux, /proc, /sys or /dev. Missing directories are made, as the workload user."`
	Mode FileMode `json:"mode,omitzero" yaml:"mode,omitempty" doc:"Its permission bits, octal: \"0644\" (the default) or \"0755\". No setuid, setgid or sticky bit."`
	Data string   `json:"data,omitempty" yaml:"data,omitempty" doc:"Its bytes, standard base64; absent or empty: an empty file. Run views leave it out."`
	// Size and SHA256 are luxd's: set from Data at submit (and on a resume
	// that replaces the files), kept in the stored spec without it.
	Size   int64  `json:"size,omitempty" yaml:"-" doc:"Set by luxd: its size in bytes."`
	SHA256 string `json:"sha256,omitempty" yaml:"-" doc:"Set by luxd: the sha256 of its bytes, hex."`
	// Content and Source are the CLI's: it reads them into Data, so
	// neither reaches luxd.
	Content string `json:"-" yaml:"content,omitempty"`
	Source  string `json:"-" yaml:"source,omitempty"`
}

// File limits (docs/runspec.md, Files).
const (
	MaxFiles      = 1000
	MaxFilesBytes = 4 << 20
	maxFilePath   = 4096
)

// DefaultFileMode is a file's mode when its spec gives none.
const DefaultFileMode FileMode = 0o644

// FileMode is a file's permission bits, always read as octal: "0644",
// 0644, 644 and 0o644 are all rw-r--r--. YAML reads a bare 644 as a
// decimal number, so the scalar's text is taken, never its number; JSON
// takes a string only. It is written "0644". 0 is unset.
type FileMode uint32

func (m FileMode) String() string { return fmt.Sprintf("%04o", uint32(m)) }

func (m FileMode) MarshalJSON() ([]byte, error) { return json.Marshal(m.String()) }

func (m *FileMode) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("invalid mode %s: need an octal string such as \"0644\"", b)
	}
	return m.parse(s)
}

func (m *FileMode) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	return m.parse(s)
}

func (m *FileMode) parse(s string) error {
	n, err := strconv.ParseUint(strings.TrimPrefix(s, "0o"), 8, 32)
	if err != nil {
		return fmt.Errorf("invalid mode %q: need an octal number such as 0644", s)
	}
	*m = FileMode(n)
	return nil
}

// reservedFileRoots are where no file may go: lux's own directory, and
// the kernel's.
var reservedFileRoots = []string{ShimDir, "/proc", "/sys", "/dev"}

// ShimDir is lux's directory in every container (proto.ShimDir).
const ShimDir = "/.lux"

// ExpandFilePath is a file's path in the container, ~/ taken as home:
// clean, absolute; "" if p is neither absolute nor ~/.
func ExpandFilePath(p, home string) string {
	switch {
	case strings.HasPrefix(p, "~/"):
		return path.Join(home, p[2:])
	case path.IsAbs(p):
		return path.Clean(p)
	}
	return ""
}

// CheckFileTarget is what is wrong with a file's path once ~/ is
// expanded (p, absolute and clean), "" if nothing: the shim's check,
// since only it knows the home: not under a reserved root, nor at, in or
// around a file secret's path.
func CheckFileTarget(p string, secrets []Secret) string {
	for _, root := range reservedFileRoots {
		if Under(p, root) {
			return fmt.Sprintf("%s is under %s, which is reserved", p, root)
		}
	}
	for _, sec := range secrets {
		if sec.As == "file" && !sec.RunnerOnly && (Under(path.Clean(sec.Path), p) || Under(p, path.Clean(sec.Path))) {
			return fmt.Sprintf("%s meets secret %q's path %s", p, sec.Name, sec.Path)
		}
	}
	return ""
}

// checkFilePath is what is wrong with a file's path, "" if nothing: it
// must be absolute or ~/, name a file (not / or a directory), and keep
// out of the reserved roots.
func checkFilePath(p string) string {
	switch {
	case p == "":
		return "path is required"
	case len(p) > maxFilePath:
		return fmt.Sprintf("path is %d bytes, at most %d", len(p), maxFilePath)
	case !utf8.ValidString(p) || strings.ContainsRune(p, 0):
		return "path must be UTF-8 without NUL"
	case !strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "~/"):
		return fmt.Sprintf("path %q must be absolute, or start with ~/ (the workload user's home)", p)
	case slices.Contains(strings.Split(p, "/"), ".."):
		return fmt.Sprintf("path %q must not contain ..", p)
	case strings.HasSuffix(p, "/") || path.Base(p) == "." || p == "~/":
		return fmt.Sprintf("path %q must name a file, not a directory", p)
	}
	if strings.HasPrefix(p, "/") {
		for _, root := range reservedFileRoots {
			if Under(path.Clean(p), root) {
				return fmt.Sprintf("path %q is under %s, which is reserved", p, root)
			}
		}
	}
	return ""
}

// normalizeFiles checks the spec's files (fail reports each problem):
// their paths and modes, their count and total size, and that no two
// collide or meet a file secret's path. Each with data gets its size and
// sha256 from it; one without is a stored file, which must have them.
// Paths are cleaned and an unset mode set.
func (s *RunSpec) normalizeFiles(fail func(string, ...any)) {
	if len(s.Files) > MaxFiles {
		fail("files: %d files, at most %d", len(s.Files), MaxFiles)
	}
	var total int64
	for i := range s.Files {
		f := &s.Files[i]
		at := fmt.Sprintf("files[%d]", i)
		if msg := checkFilePath(f.Path); msg != "" {
			fail("%s: %s", at, msg)
		} else if strings.HasPrefix(f.Path, "~/") {
			f.Path = "~/" + strings.TrimPrefix(path.Clean("/"+f.Path[2:]), "/")
		} else {
			f.Path = path.Clean(f.Path)
		}
		if f.Mode == 0 {
			f.Mode = DefaultFileMode
		}
		if f.Mode > 0o777 {
			fail("%s: mode %s: only permission bits (at most 0777): no setuid, setgid or sticky bit", at, f.Mode)
		}
		switch {
		case f.Data != "":
			b, err := base64.StdEncoding.Strict().DecodeString(f.Data)
			if err != nil {
				fail("%s: data is not standard base64: %v", at, err)
				continue
			}
			sum := sha256.Sum256(b)
			f.Size, f.SHA256 = int64(len(b)), hex.EncodeToString(sum[:])
		case f.SHA256 == "":
			// No data and no sum: an empty file. (A stored file has its
			// sum and no data; luxd refuses a sum sent to it.)
			f.Size, f.SHA256 = 0, emptySHA256
		}
		total += f.Size
	}
	if total > MaxFilesBytes {
		fail("files: %s in total, at most %s", BytesText(total), BytesText(MaxFilesBytes))
	}
	for i, f := range s.Files {
		for j, g := range s.Files[:i] {
			switch {
			case f.Path == g.Path:
				fail("files[%d]: %s is also files[%d]", i, f.Path, j)
			case Under(f.Path, g.Path):
				fail("files[%d]: %s is under files[%d], %s, a file", i, f.Path, j, g.Path)
			case Under(g.Path, f.Path):
				fail("files[%d]: %s is a directory of files[%d], %s", i, f.Path, j, g.Path)
			}
		}
		for _, sec := range s.Secrets {
			if sec.As == "file" && (Under(path.Clean(sec.Path), f.Path) || Under(f.Path, path.Clean(sec.Path))) {
				fail("files[%d]: %s meets secret %q's path %s", i, f.Path, sec.Name, sec.Path)
			}
		}
	}
}

// FilesProblems normalizes the spec's files alone (as Normalize does) and
// returns what is wrong with them: a resume's, before they replace the
// Run's.
func (s *RunSpec) FilesProblems() []string {
	var errs []string
	s.normalizeFiles(func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) })
	return errs
}

// emptySHA256 is the sha256 of no bytes: an empty file's.
var emptySHA256 = func() string { s := sha256.Sum256(nil); return hex.EncodeToString(s[:]) }()

// ClientSetFileFields are the problems of files sent to luxd (a submit,
// a resume's): size and sha256 are luxd's to set.
func ClientSetFileFields(at string, files []File) []string {
	var errs []string
	for i, f := range files {
		if f.Size != 0 || f.SHA256 != "" {
			errs = append(errs, fmt.Sprintf("%s[%d]: size and sha256 are set by luxd, never sent", at, i))
		}
	}
	return errs
}

// Decoded is the file's bytes; nil if its data is not base64 (a checked
// file's always is).
func (f File) Decoded() []byte {
	b, _ := base64.StdEncoding.DecodeString(f.Data)
	return b
}

// SplitFiles is the spec as stored (each file's path, mode, size and
// sha256) and its files with their bytes (runs.files), nil for none.
func (s RunSpec) SplitFiles() (RunSpec, []File) {
	if len(s.Files) == 0 {
		return s, nil
	}
	full := s.Files
	stored := make([]File, len(full))
	for i, f := range full {
		stored[i] = File{Path: f.Path, Mode: f.Mode, Size: f.Size, SHA256: f.SHA256}
	}
	s.Files = stored
	return s, full
}

// FilesBytes is the files' total size.
func FilesBytes(files []File) int64 {
	var n int64
	for _, f := range files {
		n += f.Size
	}
	return n
}
