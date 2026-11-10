package cli

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
)

func unb64(t *testing.T, s any) string {
	t.Helper()
	str, _ := s.(string)
	b, err := base64.StdEncoding.DecodeString(str)
	if err != nil {
		t.Fatalf("data %q: %v", s, err)
	}
	return string(b)
}

// sentFiles is what was sent as files, by path.
func sentFiles(t *testing.T, body map[string]any) map[string]map[string]any {
	t.Helper()
	list, _ := body["files"].([]any)
	out := map[string]map[string]any{}
	for _, f := range list {
		m := f.(map[string]any)
		out[m["path"].(string)] = m
	}
	return out
}

// skillsTree is a spec next to a skills directory: a nested file, an
// executable one, and a dotfile.
func skillsTree(t *testing.T) (dir, specFile string) {
	t.Helper()
	dir = t.TempDir()
	for p, c := range map[string]string{
		"skills/review/SKILL.md":  "# review\n",
		"skills/review/run.sh":    "#!/bin/sh\n",
		"skills/.hidden/note.txt": "n",
		"config/settings.json":    "{}",
	} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(dir, "skills/review/run.sh"), 0o750); err != nil {
		t.Fatal(err)
	}
	specFile = filepath.Join(dir, "spec.yaml")
	y := `image: {ref: alpine}
workload: {command: ["true"]}
files:
  - {path: ~/.claude/skills, source: skills}
  - {path: ~/.claude/settings.json, source: ./config/settings.json, mode: 0600}
  - path: /etc/motd
    content: |
      hello
`
	if err := os.WriteFile(specFile, []byte(y), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, specFile
}

// lux run -f reads sources against the spec file's directory, wherever lux
// runs; a directory becomes a file per file, each keeping its mode.
func TestRunPacksFilesAgainstTheSpecFile(t *testing.T) {
	_, specFile := skillsTree(t)
	t.Chdir(t.TempDir()) // not the spec's directory
	body := submitted(t, "run", "-f", specFile)
	got := sentFiles(t, body)
	var paths []string
	for p := range got {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	want := []string{"/etc/motd", "~/.claude/settings.json", "~/.claude/skills/.hidden/note.txt", "~/.claude/skills/review/SKILL.md", "~/.claude/skills/review/run.sh"}
	if strings.Join(paths, " ") != strings.Join(want, " ") {
		t.Fatalf("paths %v, want %v", paths, want)
	}
	for p, w := range map[string]struct{ body, mode string }{
		"~/.claude/skills/review/SKILL.md": {"# review\n", "0644"},
		"~/.claude/skills/review/run.sh":   {"#!/bin/sh\n", "0750"},
		"~/.claude/settings.json":          {"{}", "0600"},
		"/etc/motd":                        {"hello\n", ""},
	} {
		f := got[p]
		if unb64(t, f["data"]) != w.body {
			t.Errorf("%s: data %q", p, unb64(t, f["data"]))
		}
		if mode, _ := f["mode"].(string); mode != w.mode {
			t.Errorf("%s: mode %q, want %q", p, mode, w.mode)
		}
		for _, k := range []string{"source", "content"} {
			if _, ok := f[k]; ok {
				t.Errorf("%s: %s sent", p, k)
			}
		}
	}
}

// --copy LOCAL:REMOTE adds to the spec's files, LOCAL against where lux
// runs; it works without a spec file.
func TestRunCopyFlag(t *testing.T) {
	dir, _ := skillsTree(t)
	t.Chdir(dir)
	body := submitted(t, "run", "--image", "alpine", "--copy", "skills/review:~/.claude/skills/review", "--copy", "config/settings.json:/etc/app.json", "--", "true")
	got := sentFiles(t, body)
	if len(got) != 3 || unb64(t, got["/etc/app.json"]["data"]) != "{}" || unb64(t, got["~/.claude/skills/review/run.sh"]["data"]) != "#!/bin/sh\n" {
		t.Fatalf("files %v", got)
	}
	for _, bad := range []string{"nocolon", ":/x", "./a:"} {
		if _, err := runCLI(t, http.NotFoundHandler(), "run", "--image", "alpine", "--copy", bad, "--", "true"); err == nil || !strings.Contains(err.Error(), "need LOCAL:REMOTE") {
			t.Errorf("--copy %q: %v", bad, err)
		}
	}
}

// Links are never followed, and devices, sockets and fifos never read:
// named as a source, or found in a source directory.
func TestRunRefusesWhatIsNotAFile(t *testing.T) {
	dir, specFile := skillsTree(t)
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("not yours"), 0o600); err != nil {
		t.Fatal(err)
	}
	nothing := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Errorf("sent %s %s", r.Method, r.URL) })
	check := func(what, want string, args ...string) {
		t.Helper()
		if _, err := runCLI(t, nothing, args...); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", what, want, err)
		}
	}
	link := filepath.Join(dir, "skills", "review", "leak")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	check("a link in a directory", "leak: a symbolic link", "run", "-f", specFile)
	check("a link in a directory, dry run", "leak: a symbolic link", "run", "-f", specFile, "--dry-run")
	check("a link named", "a symbolic link", "run", "--image", "alpine", "--copy", link+":/x", "--", "true")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	dirLink := filepath.Join(dir, "skills", "linked")
	if err := os.Symlink(t.TempDir(), dirLink); err != nil {
		t.Fatal(err)
	}
	check("a link to a directory", "linked: a symbolic link", "run", "-f", specFile)
	if err := os.Remove(dirLink); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "skills", "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	check("a fifo", "pipe: a fifo", "run", "-f", specFile)
	if err := os.Remove(fifo); err != nil {
		t.Fatal(err)
	}
	check("/dev/null", "a device", "run", "--image", "alpine", "--copy", "/dev/null:/x", "--", "true")
	check("a missing source", "no such file", "run", "--image", "alpine", "--copy", filepath.Join(dir, "absent")+":/x", "--", "true")
	empty := filepath.Join(dir, "empty")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	check("an empty directory", "an empty directory", "run", "--image", "alpine", "--copy", empty+":/x", "--", "true")
}

// Too much is refused before anything is sent; so is a file with two
// sources.
func TestRunRefusesTooMuchAndTwoSources(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big")
	if err := os.WriteFile(big, make([]byte, 3<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	nothing := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Errorf("sent %s %s", r.Method, r.URL) })
	if _, err := runCLI(t, nothing, "run", "--image", "alpine", "--copy", big+":/a", "--copy", big+":/b", "--", "true"); err == nil || !strings.Contains(err.Error(), "more than 4.0 MiB in total") {
		t.Fatalf("too much: %v", err)
	}
	specFile := filepath.Join(dir, "spec.yaml")
	if err := os.WriteFile(specFile, []byte("image: {ref: alpine}\nworkload: {command: [\"true\"]}\nfiles: [{path: /a, content: x, source: big}]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, nothing, "run", "-f", specFile); err == nil || !strings.Contains(err.Error(), "one of data, content or source") {
		t.Fatalf("two sources: %v", err)
	}
}

// --dry-run sends the files it read, so luxd checks them as a submit
// would.
func TestRunDryRunSendsTheFiles(t *testing.T) {
	_, specFile := skillsTree(t)
	var body map[string]any
	var query string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"valid":true,"effectivePool":"p","effectiveResources":{"cpus":1,"memory":2,"disk":3}}`))
	})
	if _, err := runCLI(t, h, "run", "-f", specFile, "--dry-run"); err != nil {
		t.Fatal(err)
	}
	if query != "validateOnly=true" || len(sentFiles(t, body)) != 5 {
		t.Fatalf("query %q files %v", query, body["files"])
	}
}

// lux resume --files-from replaces the Run's files with a spec file's,
// --copy adds to them, --no-files removes them; without any, no files
// key is sent.
func TestResumeFiles(t *testing.T) {
	_, specFile := skillsTree(t)
	resumed := func(args ...string) map[string]any {
		t.Helper()
		var body map[string]any
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == "GET" {
				_, _ = w.Write([]byte(`{"id":"run_1","state":"stopped","spec":{"image":{"ref":"alpine"},"workload":{"adapter":"generic"}}}`))
				return
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			_, _ = w.Write([]byte(`{"id":"run_1","state":"resuming"}`))
		})
		if _, err := runCLI(t, h, append([]string{"resume", "run_1"}, args...)...); err != nil {
			t.Fatal(err)
		}
		return body
	}
	if got := sentFiles(t, resumed("--files-from", specFile)); len(got) != 5 || unb64(t, got["/etc/motd"]["data"]) != "hello\n" {
		t.Fatalf("--files-from: %v", got)
	}
	extra := filepath.Join(t.TempDir(), "extra.md")
	if err := os.WriteFile(extra, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := sentFiles(t, resumed("--files-from", specFile, "--copy", extra+":~/extra.md")); len(got) != 6 || got["~/extra.md"] == nil {
		t.Fatalf("--files-from with --copy: %v", got)
	}
	if body := resumed("--no-files"); body["files"] == nil || len(body["files"].([]any)) != 0 {
		t.Fatalf("--no-files sent %v", body["files"])
	}
	if body := resumed(); body["files"] != nil {
		t.Fatalf("a plain resume sent files %v", body["files"])
	}
	if _, err := runCLI(t, http.NotFoundHandler(), "resume", "run_1", "--no-files", "--copy", extra+":/x"); err == nil || !strings.Contains(err.Error(), "--no-files cannot be combined") {
		t.Fatalf("--no-files with --copy: %v", err)
	}
}

// The open itself refuses a link and a fifo: one swapped in after the
// directory was walked is not followed, nor waited on.
func TestOpenNoFollow(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if f, err := openNoFollow(link); err == nil {
		f.Close()
		t.Fatal("opened through a link")
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if f, err := openNoFollow(fifo); err == nil || !strings.Contains(err.Error(), "a fifo") {
		if f != nil {
			f.Close()
		}
		t.Fatalf("a fifo: %v", err)
	}
	f, err := openNoFollow(target)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}
