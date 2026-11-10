package spec

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func filesSpec(files ...File) RunSpec {
	return RunSpec{Image: Image{Ref: "alpine"}, Workload: Workload{Command: []string{"true"}}, Files: files}
}

func b64s(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// A file's path, mode and data, as Normalize takes or refuses them.
func TestFilesNormalize(t *testing.T) {
	for _, c := range []struct {
		name string
		f    File
		want string // a problem's text; "" accepts
	}{
		{"absolute", File{Path: "/etc/tool.conf", Data: b64s("x")}, ""},
		{"home", File{Path: "~/.claude/skills/a/SKILL.md", Data: b64s("x")}, ""},
		{"empty", File{Path: "/tmp/empty"}, ""},
		{"relative", File{Path: "a/b", Data: b64s("x")}, "must be absolute, or start with ~/"},
		{"tilde user", File{Path: "~root/x", Data: b64s("x")}, "must be absolute, or start with ~/"},
		{"dotdot", File{Path: "/home/agent/../../etc/passwd", Data: b64s("x")}, "must not contain .."},
		{"home dotdot", File{Path: "~/../x", Data: b64s("x")}, "must not contain .."},
		{"root", File{Path: "/", Data: b64s("x")}, "must name a file"},
		{"home itself", File{Path: "~/", Data: b64s("x")}, "must name a file"},
		{"trailing slash", File{Path: "/etc/x/", Data: b64s("x")}, "must name a file"},
		{"dot", File{Path: "/etc/.", Data: b64s("x")}, "must name a file"},
		{"nul", File{Path: "/etc/a\x00b", Data: b64s("x")}, "without NUL"},
		{"lux", File{Path: "/.lux/run/config.json", Data: b64s("x")}, "under /.lux, which is reserved"},
		{"proc", File{Path: "/proc/self/attr", Data: b64s("x")}, "reserved"},
		{"sys", File{Path: "/sys/fs/cgroup/x", Data: b64s("x")}, "reserved"},
		{"dev", File{Path: "/dev/sda", Data: b64s("x")}, "reserved"},
		{"no path", File{Data: b64s("x")}, "path is required"},
		{"setuid", File{Path: "/x", Mode: 0o4755, Data: b64s("x")}, "only permission bits"},
		{"bad base64", File{Path: "/x", Data: "not base64!"}, "not standard base64"},
		{"long", File{Path: "/" + strings.Repeat("a", 5000), Data: b64s("x")}, "at most 4096"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := filesSpec(c.f)
			err := s.Normalize(BuiltinDefaults)
			if c.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

// Normalize cleans paths, defaults the mode, and sets size and sha256 from
// the data; an empty file gets the sum of no bytes.
func TestFilesNormalizeFills(t *testing.T) {
	s := filesSpec(File{Path: "/etc//tool/./a.conf", Data: b64s("hello")}, File{Path: "~/x//y", Mode: 0o755}, File{Path: "~/.z"})
	if err := s.Normalize(BuiltinDefaults); err != nil {
		t.Fatal(err)
	}
	f := s.Files[0]
	if f.Path != "/etc/tool/a.conf" || f.Mode != 0o644 || f.Size != 5 ||
		f.SHA256 != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("file 0: %+v", f)
	}
	if g := s.Files[1]; g.Path != "~/x/y" || g.Mode != 0o755 || g.Size != 0 || g.SHA256 != emptySHA256 {
		t.Fatalf("file 1: %+v", g)
	}
	if g := s.Files[2]; g.Path != "~/.z" {
		t.Fatalf("file 2: %+v", g)
	}
}

func TestFilesLimitsAndCollisions(t *testing.T) {
	big := b64s(strings.Repeat("a", MaxFilesBytes/2+1))
	for _, c := range []struct {
		name  string
		files []File
		want  string
	}{
		{"too big in total", []File{{Path: "/a", Data: big}, {Path: "/b", Data: big}}, "in total, at most 4.0 MiB"},
		{"same path", []File{{Path: "/a/b"}, {Path: "/a//b"}}, "/a/b is also files[0]"},
		{"under a file", []File{{Path: "/a"}, {Path: "/a/b"}}, "is under files[0], /a, a file"},
		{"a directory of", []File{{Path: "~/a/b"}, {Path: "~/a"}}, "is a directory of files[0]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := filesSpec(c.files...)
			if err := s.Normalize(BuiltinDefaults); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
	many := make([]File, MaxFiles+1)
	for i := range many {
		many[i] = File{Path: "/f/" + strings.Repeat("x", 1+i%7) + "/" + string(rune('a'+i%26)) + strings.Repeat("y", i/26)}
	}
	s := filesSpec(many...)
	if err := s.Normalize(BuiltinDefaults); err == nil || !strings.Contains(err.Error(), "1001 files, at most 1000") {
		t.Fatal(err)
	}
	// Exactly at the size limit is fine.
	s = filesSpec(File{Path: "/a", Data: b64s(strings.Repeat("a", MaxFilesBytes))})
	if err := s.Normalize(BuiltinDefaults); err != nil {
		t.Fatal(err)
	}
}

// A file may not land on a file secret's path, nor around it.
func TestFilesMeetASecretsPath(t *testing.T) {
	s := filesSpec(File{Path: "/home/agent/.npmrc"})
	s.Secrets = []Secret{{Name: "npmrc", Value: "x", As: "file", Path: "/home/agent/.npmrc"}}
	if err := s.Normalize(BuiltinDefaults); err == nil || !strings.Contains(err.Error(), `meets secret "npmrc"'s path`) {
		t.Fatal(err)
	}
}

// Modes are octal however they are written; JSON takes a string.
func TestFileModeParsing(t *testing.T) {
	for _, y := range []string{"0755", "755", "0o755", `"0755"`, `"755"`} {
		var f File
		if err := yaml.Unmarshal([]byte("{path: /x, mode: "+y+"}"), &f); err != nil || f.Mode != 0o755 {
			t.Errorf("yaml %s: %v %v", y, f.Mode, err)
		}
	}
	var f File
	if err := json.Unmarshal([]byte(`{"path":"/x","mode":"0600"}`), &f); err != nil || f.Mode != 0o600 {
		t.Fatalf("json: %v %v", f.Mode, err)
	}
	if err := json.Unmarshal([]byte(`{"path":"/x","mode":420}`), &f); err == nil {
		t.Fatal("a JSON number read as a mode: 420 would be 0644, not 0420")
	}
	if err := yaml.Unmarshal([]byte("{path: /x, mode: 0999}"), &f); err == nil {
		t.Fatal("0999 accepted")
	}
	b, _ := json.Marshal(File{Path: "/x", Mode: 0o644})
	if !strings.Contains(string(b), `"mode":"0644"`) {
		t.Fatal(string(b))
	}
}

// The CLI's fields never reach luxd, and luxd's are never taken from a
// client.
func TestFileFieldsByWhoSetsThem(t *testing.T) {
	b, _ := json.Marshal(File{Path: "/x", Content: "c", Source: "s"})
	if strings.Contains(string(b), `"c"`) || strings.Contains(string(b), `"s"`) {
		t.Fatalf("CLI fields sent: %s", b)
	}
	var f File
	if err := yaml.Unmarshal([]byte("{path: /x, content: hi, source: ./a, size: 3, sha256: ab}"), &f); err != nil {
		t.Fatal(err)
	}
	if f.Content != "hi" || f.Source != "./a" || f.Size != 0 || f.SHA256 != "" {
		t.Fatalf("%+v", f)
	}
	if errs := ClientSetFileFields("files", []File{{Path: "/a"}, {Path: "/b", SHA256: "x"}, {Path: "/c", Size: 1}}); len(errs) != 2 ||
		!strings.HasPrefix(errs[0], "files[1]: size and sha256 are set by luxd") || !strings.HasPrefix(errs[1], "files[2]") {
		t.Fatal(errs)
	}
}

// SplitFiles keeps each file's metadata in the spec and its bytes apart;
// the stored spec normalizes again unchanged.
func TestSplitFiles(t *testing.T) {
	s := filesSpec(File{Path: "~/a", Data: b64s("abc"), Mode: 0o600})
	if err := s.Normalize(BuiltinDefaults); err != nil {
		t.Fatal(err)
	}
	stored, full := s.SplitFiles()
	if len(full) != 1 || full[0].Data != b64s("abc") || string(full[0].Decoded()) != "abc" {
		t.Fatalf("full %+v", full)
	}
	if f := stored.Files[0]; f.Data != "" || f.Size != 3 || f.SHA256 == "" || f.Mode != 0o600 || f.Path != "~/a" {
		t.Fatalf("stored %+v", f)
	}
	if s.Files[0].Data == "" {
		t.Fatal("SplitFiles changed the spec it was called on")
	}
	b, _ := json.Marshal(stored)
	var again RunSpec
	if err := json.Unmarshal(b, &again); err != nil {
		t.Fatal(err)
	}
	if err := again.Normalize(BuiltinDefaults); err != nil {
		t.Fatal(err)
	}
	if again.Files[0] != stored.Files[0] {
		t.Fatalf("renormalized %+v, stored %+v", again.Files[0], stored.Files[0])
	}
	if FilesBytes(full) != 3 {
		t.Fatal(FilesBytes(full))
	}
	if _, none := filesSpec().SplitFiles(); none != nil {
		t.Fatal(none)
	}
}

func TestExpandFilePath(t *testing.T) {
	for in, want := range map[string]string{
		"~/.claude/skills/a": "/home/agent/.claude/skills/a",
		"/etc/x":             "/etc/x",
		"rel":                "",
	} {
		if got := ExpandFilePath(in, "/home/agent"); got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
}
