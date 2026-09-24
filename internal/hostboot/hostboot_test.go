package hostboot

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEnvLinesOmitsEmptyFields(t *testing.T) {
	got := Env{URL: "http://10.0.1.10:7070", HostToken: "luxh_x"}.Lines()
	want := "LUX_URL=http://10.0.1.10:7070\nLUX_HOST_TOKEN=luxh_x\n"
	if got != want {
		t.Errorf("Lines() = %q, want %q", got, want)
	}
}

func TestValidUserData(t *testing.T) {
	for v, want := range map[string]bool{"": true, "ignition": true, "script": true, "env": true, "cloud-init": false, "IGNITION": false} {
		if got := ValidUserData(v); got != want {
			t.Errorf("ValidUserData(%q) = %v, want %v", v, got, want)
		}
	}
}

// Ignition renders valid JSON, spec 3.4.0, writing the env file with 0600
// and installing the unit enabled; zincati is masked so a runner host
// never auto-updates and reboots mid-Run.
func TestIgnitionShape(t *testing.T) {
	b, err := Ignition(Env{URL: "http://10.0.1.10:7070", HostToken: "luxh_x", HostName: "h", EC2IMDS: "http://169.254.169.254"})
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if v := cfg["ignition"].(map[string]any)["version"]; v != ignitionSpec {
		t.Errorf("version %v, want %v", v, ignitionSpec)
	}
	files := cfg["storage"].(map[string]any)["files"].([]any)
	var envFile map[string]any
	for _, f := range files {
		fm := f.(map[string]any)
		if fm["path"] == "/etc/lux/runner.env" {
			envFile = fm
		}
	}
	if envFile == nil {
		t.Fatal("no /etc/lux/runner.env file entry")
	}
	if envFile["mode"].(float64) != 384 { // 0600 decimal
		t.Errorf("runner.env mode %v, want 384 (0600)", envFile["mode"])
	}
	units := cfg["systemd"].(map[string]any)["units"].([]any)
	var runnerUnit, zincati map[string]any
	for _, u := range units {
		um := u.(map[string]any)
		switch um["name"] {
		case UnitName:
			runnerUnit = um
		case "zincati.service":
			zincati = um
		}
	}
	if runnerUnit == nil || runnerUnit["enabled"] != true {
		t.Errorf("lux-runner.service not enabled: %+v", runnerUnit)
	}
	if zincati == nil || zincati["mask"] != true {
		t.Errorf("zincati.service not masked: %+v", zincati)
	}
	if len(b) > 16<<10 {
		t.Errorf("rendered Ignition config is %d bytes, over the EC2 user-data limit", len(b))
	}
}

// Script exports the env ahead of the shared, unfilled script body: the
// two must differ only by that prefix.
func TestScriptWrapsBootstrap(t *testing.T) {
	env := Env{URL: "http://10.0.1.10:7070", HostToken: "luxh_x", HostName: "h"}
	got := Script(env)
	if !strings.HasPrefix(got, "#!/bin/bash\nexport LUX_URL='http://10.0.1.10:7070'\nexport LUX_HOST_TOKEN='luxh_x'\nexport LUX_HOST_NAME='h'\n") {
		t.Errorf("Script did not export the env first:\n%s", got[:200])
	}
	if !strings.HasSuffix(got, strings.TrimPrefix(scriptBody, "#!/bin/bash\n")) {
		t.Error("Script's body diverged from the shared scriptBody")
	}
}

// A value with a shell metacharacter is quoted, not interpolated raw: a
// host token or URL is never trusted to be shell-safe as given.
func TestScriptQuotesValues(t *testing.T) {
	env := Env{URL: "http://x", HostToken: "a'; rm -rf /; echo '"}
	got := Script(env)
	if !strings.Contains(got, `export LUX_HOST_TOKEN='a'\''; rm -rf /; echo '\'''`) {
		t.Errorf("token not safely quoted:\n%s", got[:300])
	}
}

func TestBootstrapTakesEnvFromEnvironment(t *testing.T) {
	b := Bootstrap()
	for _, want := range []string{`LUX_URL:?LUX_URL is required`, `LUX_HOST_TOKEN:?LUX_HOST_TOKEN is required`, `LUX_HOST_NAME:-$(hostname)`} {
		if !strings.Contains(b, want) {
			t.Errorf("bootstrap.sh missing %q", want)
		}
	}
}
