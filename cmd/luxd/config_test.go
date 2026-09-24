package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/spec"
)

// The file sets what it names over the defaults; the environment overrides
// the file; unknown keys and bad values are refused, naming them.
func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "luxd.toml")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`
listen = "0.0.0.0:7070"
lease = "45s"
[database]
url = "postgres://file"
[s3]
bucket = "from-file"
[defaults]
memory = "4Gi"
[history]
raw = "24h"
[console]
auth = "cloudflare-access"
[console.cloudflare_access]
team = "acme"
aud = "aud-from-file"
`)
	t.Setenv("LUX_S3_BUCKET", "from-env")
	t.Setenv("LUX_DEFAULT_CPUS", "1.5")
	c, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case c.Listen != "0.0.0.0:7070", c.Lease.Duration != 45*time.Second, c.Database.URL != "postgres://file":
		t.Errorf("file: %+v", c)
	case c.S3.Bucket != "from-env" || c.Defaults.CPUs != 1.5:
		t.Errorf("env over file: bucket %q cpus %v", c.S3.Bucket, c.Defaults.CPUs)
	case c.Defaults.Memory.Bytes != 4<<30 || c.Defaults.Pids != spec.BuiltinDefaults.Pids:
		t.Errorf("defaults: %+v", c.Defaults)
	case c.History.Raw.Duration != 24*time.Hour || c.History.Hours.Duration == 0:
		t.Errorf("history: %+v", c.History)
	case c.Console.Auth != "cloudflare-access" || c.Console.CloudflareAccess.Team != "acme":
		t.Errorf("console: %+v", c.Console)
	case c.S3.Region != "us-east-1" || c.Tick.Duration != time.Second:
		t.Errorf("defaults kept: region %q tick %v", c.S3.Region, c.Tick)
	}

	for _, bad := range []struct{ file, env, want string }{
		{"lisen = \"x\"", "", "lisen"},
		{"lease = \"soon\"", "", "lease"},
		{"", "LUX_LEASE=soon", "LUX_LEASE"},
		{"[defaults]\ncpus = -1", "", "defaults.cpus"},
		{"[console]\nauth = \"cloudflare-access\"", "", "console.cloudflare_access.team"},
		{"[console]\nauth = \"magic\"", "", "want key or cloudflare-access"},
	} {
		os.Unsetenv("LUX_LEASE")
		os.Unsetenv("LUX_DEFAULT_CPUS")
		write(bad.file)
		if bad.env != "" {
			k, v, _ := strings.Cut(bad.env, "=")
			t.Setenv(k, v)
		}
		if _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%q %q: error %v, want it to name %q", bad.file, bad.env, err, bad.want)
		}
	}
	// A file named but missing is an error; the default path missing is not.
	if _, err := loadConfig(filepath.Join(dir, "none.toml")); err == nil {
		t.Error("a missing named file was not an error")
	}
}
