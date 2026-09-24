package hostboot

import (
	"os"
	"os/exec"
	"testing"
)

// TestShellcheckScripts runs shellcheck over the fetch script and the
// rendered bootstrap and userData "script" renderings, so a syntax or
// quoting mistake is caught here rather than at boot, where nothing is
// watching. It skips only outside CI (a developer machine without
// shellcheck installed); inside CI (the GITHUB_ACTIONS / CI environment
// every runner sets) a missing shellcheck fails the test rather than
// silently skipping it, so the check is actually enforced there.
func TestShellcheckScripts(t *testing.T) {
	path, err := exec.LookPath("shellcheck")
	if err != nil {
		if os.Getenv("CI") != "" || os.Getenv("GITHUB_ACTIONS") != "" {
			t.Fatalf("shellcheck not installed in CI: %v", err)
		}
		t.Skip("shellcheck not installed")
	}
	scripts := map[string]string{
		"fetch-binaries.sh": FetchBinariesScript,
		"bootstrap.sh":      Bootstrap(),
		"script-userdata":   Script(Env{URL: "http://10.0.1.10:7070", HostToken: "luxh_test", HostName: "h", EC2IMDS: "http://169.254.169.254"}),
	}
	for name, content := range scripts {
		f, err := os.CreateTemp(t.TempDir(), "*.sh")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(content); err != nil {
			t.Fatal(err)
		}
		f.Close()
		out, err := exec.Command(path, f.Name()).CombinedOutput()
		if err != nil {
			t.Errorf("%s: shellcheck failed:\n%s", name, out)
		}
	}
}

// TestBashGuardStopsUnderSh checks, as a bare `sh`, only the guard line
// every bash-only script starts with (after its shebang): shellcheck's
// own POSIX (`sh`) dialect would otherwise reject the bash arrays and
// `set -o pipefail` further down, which is expected and not what this
// checks. What must hold under `sh` is that the guard itself is valid
// POSIX and refuses to proceed, which the fetch and bootstrap scripts'
// integration coverage (dash execution) exercises at runtime; this test
// pins the guard line's shellcheck-sh cleanliness so it never regresses
// into something that only happens to work under bash.
func TestBashGuardStopsUnderSh(t *testing.T) {
	path, err := exec.LookPath("shellcheck")
	if err != nil {
		if os.Getenv("CI") != "" || os.Getenv("GITHUB_ACTIONS") != "" {
			t.Fatalf("shellcheck not installed in CI: %v", err)
		}
		t.Skip("shellcheck not installed")
	}
	guard := `#!/bin/sh
[ -n "${BASH_VERSION:-}" ] || { echo "lux: run this script with bash, not sh (curl ... | sudo env ... bash)" >&2; exit 1; }
`
	f, err := os.CreateTemp(t.TempDir(), "*.sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(guard); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(path, "-s", "sh", f.Name()).CombinedOutput()
	if err != nil {
		t.Errorf("guard line is not valid POSIX sh:\n%s", out)
	}
}
