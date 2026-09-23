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
	for pattern, want := range map[string]string{
		"/workspace/out/**": "/workspace/out", "/workspace/*.log": "/workspace", "/w/report.xml": "/w/report.xml",
	} {
		if got := globBase(pattern); got != want {
			t.Errorf("globBase(%q) = %q, want %q", pattern, got, want)
		}
	}
}
