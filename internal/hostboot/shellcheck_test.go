package hostboot

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// shellcheck runs shellcheck with args over content. It skips when
// shellcheck is missing, except in CI, where that fails instead.
func shellcheck(t *testing.T, name, content string, args ...string) {
	t.Helper()
	path, err := exec.LookPath("shellcheck")
	if err != nil {
		if os.Getenv("CI") != "" || os.Getenv("GITHUB_ACTIONS") != "" {
			t.Fatalf("shellcheck not installed in CI: %v", err)
		}
		t.Skip("shellcheck not installed")
	}
	f := t.TempDir() + "/script.sh"
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(path, append(args, f)...).CombinedOutput(); err != nil {
		t.Errorf("%s: shellcheck failed:\n%s", name, out)
	}
}

func TestShellcheckScripts(t *testing.T) {
	shellcheck(t, "fetch-binaries.sh", FetchBinariesScript)
	shellcheck(t, "bootstrap.sh", Bootstrap())
	shellcheck(t, "script-userdata", Script(Env{URL: "http://10.0.1.10:7070", HostToken: "luxh_test", HostName: "h", EC2IMDS: "http://169.254.169.254"}))
}

// The BASH_VERSION guard on each bash-only script's second line must be
// valid POSIX sh, since that is the shell it has to stop. The rest of
// each script is bash and would fail this dialect by design.
func TestBashGuardIsPOSIX(t *testing.T) {
	for name, script := range map[string]string{"fetch-binaries.sh": FetchBinariesScript, "bootstrap.sh": Bootstrap()} {
		guard := strings.SplitN(script, "\n", 3)[1]
		if !strings.Contains(guard, "BASH_VERSION") {
			t.Fatalf("%s: second line is not the bash guard: %q", name, guard)
		}
		shellcheck(t, name+" guard", "#!/bin/sh\n"+guard+"\n", "-s", "sh")
	}
}
