package shim

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An init script larger than Linux's 128 KiB cap on one argument
// (MAX_ARG_STRLEN) runs: the shim hands sh a file, not `sh -c SCRIPT`.
// A second start (a resume, a move) rewrites the file.
func TestInitScriptLargerThanAnArgument(t *testing.T) {
	s, _, dir := reclaimShim(t)
	old := initScriptDir
	initScriptDir = t.TempDir()
	t.Cleanup(func() { initScriptDir = old })

	for _, word := range []string{"first", "second"} {
		out := filepath.Join(dir, "init-ran")
		// A comment pads the script past 128 KiB; the command after it must run.
		s.cfg.Init = "#" + strings.Repeat("x", 200<<10) + "\necho " + word + " > " + out + "\n"
		// reclaimShim's reaper delivers init's status on initCh.
		code, err := s.runInit(s.env)
		if err != nil || code != 0 {
			t.Fatalf("%s start: init exited %d, err %v", word, code, err)
		}
		got, err := os.ReadFile(out)
		if err != nil || strings.TrimSpace(string(got)) != word {
			t.Fatalf("%s start: init wrote %q, err %v", word, got, err)
		}
	}
}
