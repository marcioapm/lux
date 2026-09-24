package hostboot

import (
	"os"
	"os/exec"
	"testing"
)

// TestShellcheckScripts runs shellcheck (if installed) over the fetch
// script and the rendered bootstrap script, so a syntax or quoting mistake
// is caught here rather than at boot, where nothing is watching.
func TestShellcheckScripts(t *testing.T) {
	path, err := exec.LookPath("shellcheck")
	if err != nil {
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
