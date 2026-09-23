package runner

import "testing"

func TestGlobMatch(t *testing.T) {
	for _, c := range []struct {
		pattern, name string
		want          bool
	}{
		{"/workspace/out/**", "/workspace/out/a.txt", true},
		{"/workspace/out/**", "/workspace/out/x/y/z.bin", true},
		{"/workspace/out/**", "/workspace/other/a.txt", false},
		{"/workspace/*.log", "/workspace/run.log", true},
		{"/workspace/*.log", "/workspace/sub/run.log", false},
		{"/workspace/**/*.xml", "/workspace/a/b/report.xml", true},
		{"/workspace/**/*.xml", "/workspace/report.xml", true},
		{"/workspace/report.xml", "/workspace/report.xml", true},
	} {
		if got := globMatch(splitPath(c.pattern), splitPath(c.name)); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestGlobPrefix(t *testing.T) {
	for _, c := range []struct {
		pattern, dir string
		want         bool
	}{
		{"/workspace/out/*.bin", "/workspace", true},
		{"/workspace/out/*.bin", "/workspace/out", true},
		{"/workspace/out/*.bin", "/workspace/node_modules", false},
		{"/workspace/out/*.bin", "/workspace/out/deeper", false},
		{"/workspace/**/*.xml", "/workspace/a/b/c", true},
		{"/workspace/*/reports/*", "/workspace/app", true},
		{"/workspace/*/reports/*", "/workspace/app/src", false},
	} {
		if got := globPrefix(splitPath(c.pattern), splitPath(c.dir)); got != c.want {
			t.Errorf("globPrefix(%q, %q) = %v, want %v", c.pattern, c.dir, got, c.want)
		}
	}
}
