package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestMain lets tests run luxd itself: the test binary re-executed with
// luxdExecEnv set runs main instead of the tests.
func TestMain(m *testing.M) {
	if os.Getenv(luxdExecEnv) == "1" {
		os.Args = append([]string{"luxd"}, os.Args[1:]...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

const luxdExecEnv = "LUXD_TEST_EXEC"

type luxdResult struct {
	code           int
	stdout, stderr string
}

// runLuxd runs luxd with args and only the given environment (plus an
// AWS configuration pointed at nothing, so the host's own is never read).
func runLuxd(t *testing.T, env []string, args ...string) luxdResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], args...)
	home := t.TempDir()
	cmd.Env = append([]string{
		luxdExecEnv + "=1",
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
		"AWS_CONFIG_FILE=" + filepath.Join(home, "aws-config"),
		"AWS_SHARED_CREDENTIALS_FILE=" + filepath.Join(home, "aws-credentials"),
		"AWS_EC2_METADATA_DISABLED=true",
	}, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	code := 0
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("run luxd %q: %v", args, err)
	}
	return luxdResult{code, stdout.String(), stderr.String()}
}

// fakePostgres is a TCP listener standing where PostgreSQL would be: it
// counts connections and closes each at once.
type fakePostgres struct {
	ln    net.Listener
	conns atomic.Int32
	done  chan struct{}
}

func newFakePostgres(t *testing.T) *fakePostgres {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakePostgres{ln: ln, done: make(chan struct{})}
	go func() {
		defer close(f.done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.conns.Add(1)
			c.Close()
		}
	}()
	t.Cleanup(func() { ln.Close(); <-f.done })
	return f
}

func (f *fakePostgres) url() string {
	return fmt.Sprintf("postgres://lux_app:db-secret-%s@%s/lux?sslmode=disable&connect_timeout=2", "x7Q", f.ln.Addr())
}

// connections is the count once every accepted connection is counted.
func (f *fakePostgres) connections() int32 {
	f.ln.Close()
	<-f.done
	return f.conns.Load()
}

func writeConfig(t *testing.T, mode os.FileMode, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "luxd.toml")
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func validConfig(dbURL string) string {
	return fmt.Sprintf("listen = \"127.0.0.1:0\"\n[database]\nurl = %q\n[s3]\nbucket = \"lux-test\"\nendpoint = \"http://127.0.0.1:1\"\n", dbURL)
}

// badConfigs are configurations serve refuses before connecting, each
// with the exact message luxd prints. validate and serve must agree on
// every one of them.
func badConfigs(dbURL string) []struct {
	name, file string
	env        []string
	want       string
} {
	valid := validConfig(dbURL)
	return []struct {
		name, file string
		env        []string
		want       string
	}{
		{"database.url missing", "[s3]\nbucket = \"lux-test\"\n", nil,
			"database.url is required (database.url in the config file, or LUX_DATABASE_URL)"},
		{"s3.bucket missing", fmt.Sprintf("[database]\nurl = %q\n", dbURL), nil,
			"s3.bucket is required (s3.bucket in the config file, or LUX_S3_BUCKET)"},
		{"database.url malformed", "[database]\nurl = \"postgres://lux:pw-secret@[::1\"\n[s3]\nbucket = \"b\"\n", nil,
			"database.url (LUX_DATABASE_URL) is not a valid PostgreSQL connection string"},
		{"listen malformed", valid, []string{"LUX_LISTEN=7070"},
			`listen (LUX_LISTEN) "7070": want host:port`},
		{"unknown key", "lisen = \"x\"\n" + valid, nil,
			"%PATH%: unknown keys: lisen"},
		{"bad duration", valid + "[costs]\nevery = \"soon\"\n", nil,
			`%PATH%: line 8 column 9: costs.every: toml: time: invalid duration "soon"`},
		{"bad env value", valid, []string{"LUX_LEASE=soon"},
			`LUX_LEASE: "soon": time: invalid duration "soon"`},
		{"console auth", valid + "[console]\nauth = \"magic\"\n", nil,
			`configuration: console.auth (LUX_CONSOLE_AUTH) "magic": want key or cloudflare-access`},
		{"cloudflare access incomplete", valid, []string{"LUX_CONSOLE_AUTH=cloudflare-access", "LUX_CF_ACCESS_TEAM=acme", "LUX_CF_ACCESS_AUD=aud", "LUX_CF_ACCESS_DEFAULT_TENANT=t"},
			"configuration: console.cloudflare_access.operators (LUX_CF_ACCESS_OPERATORS) needs at least one email"},
		{"preview without public_url", valid, []string{"LUX_PREVIEW_DOMAIN=lux.example.com"},
			"configuration: preview.auth ticket needs public_url (LUX_PUBLIC_URL): previews send people there to sign in"},
		{"cost plugin", valid + "[[costs.plugin]]\nname = \"compute\"\nurl = \"https://ledger.example\"\n", nil,
			"configuration: costs.plugin[0].name must be nonempty, unique and not compute"},
		{"defaults", valid, []string{"LUX_DEFAULT_PIDS=-1"},
			"configuration: defaults.pids (LUX_DEFAULT_PIDS) must be a positive number"},
	}
}

func TestValidate(t *testing.T) {
	pg := newFakePostgres(t)
	valid := writeConfig(t, 0o600, validConfig(pg.url()))

	for _, cmd := range []string{"validate", "check-config"} {
		t.Run(cmd, func(t *testing.T) {
			r := runLuxd(t, nil, "--config", valid, cmd)
			if r.code != 0 || r.stdout != "ok: "+valid+"\n" || r.stderr != "" {
				t.Fatalf("valid file: %+v", r)
			}
			// --config after the command, as serve takes it.
			if r := runLuxd(t, nil, cmd, "--config", valid); r.code != 0 || r.stdout != "ok: "+valid+"\n" {
				t.Fatalf("--config after: %+v", r)
			}
			// The environment alone, when no file exists at the default path.
			if _, err := os.Stat(defaultConfigPath); errors.Is(err, os.ErrNotExist) {
				r := runLuxd(t, []string{"LUX_DATABASE_URL=" + pg.url(), "LUX_S3_BUCKET=b"}, cmd)
				if r.code != 0 || r.stdout != "ok: no file\n" {
					t.Fatalf("no file: %+v", r)
				}
			}

			for _, bad := range badConfigs(pg.url()) {
				t.Run(bad.name, func(t *testing.T) {
					path := writeConfig(t, 0o600, bad.file)
					r := runLuxd(t, bad.env, "--config", path, cmd)
					want := strings.ReplaceAll(bad.want, "%PATH%", path)
					if r.code != 1 || r.stderr != "luxd: "+want+"\n" || r.stdout != "" {
						t.Fatalf("got %+v\nwant exit 1 and %q", r, want)
					}
					if strings.Contains(r.stderr, "secret") {
						t.Fatalf("a secret was printed: %q", r.stderr)
					}
				})
			}

			t.Run("world-readable", func(t *testing.T) {
				path := writeConfig(t, 0o644, validConfig(pg.url())+"access_key = \"AKIA\"\nsecret_key = \"s3-secret-value\"\n")
				r := runLuxd(t, nil, "--config", path, cmd)
				if r.code != 0 || r.stdout != "ok: "+path+"\n" || !strings.Contains(r.stderr, "warning: others can read "+path) {
					t.Fatalf("got %+v", r)
				}
				if strings.Contains(r.stdout+r.stderr, "secret-") || strings.Contains(r.stdout+r.stderr, "AKIA") {
					t.Fatalf("a secret was printed: %+v", r)
				}
			})

			t.Run("extra argument", func(t *testing.T) {
				r := runLuxd(t, nil, "--config", valid, cmd, "now")
				if r.code != 2 || !strings.Contains(r.stderr, "usage: luxd") || r.stdout != "" {
					t.Fatalf("got %+v", r)
				}
			})
		})
	}
	if n := pg.connections(); n != 0 {
		t.Fatalf("validate connected to the database %d times", n)
	}
}

// serve refuses exactly the configurations validate refuses, with the
// same message, before it connects; a configuration validate accepts
// gets as far as the database.
func TestServeRefusesWhatValidateRefuses(t *testing.T) {
	pg := newFakePostgres(t)
	for _, bad := range badConfigs(pg.url()) {
		t.Run(bad.name, func(t *testing.T) {
			path := writeConfig(t, 0o600, bad.file)
			v := runLuxd(t, bad.env, "--config", path, "validate")
			s := runLuxd(t, bad.env, "--config", path, "serve")
			if v.code != 1 || s.code != 1 || v.stderr != s.stderr {
				t.Fatalf("validate %+v\nserve %+v", v, s)
			}
			for _, e := range bad.env {
				k, val, _ := strings.Cut(e, "=")
				t.Setenv(k, val)
			}
			_, _, _, err := loadServe(context.Background(), path)
			if err == nil || "luxd: "+err.Error()+"\n" != v.stderr {
				t.Fatalf("loadServe: %v, validate printed %q", err, v.stderr)
			}
		})
	}
	if n := pg.connections(); n != 0 {
		t.Fatalf("serve connected to the database %d times with a refused configuration", n)
	}

	// The control: the listener does see serve when the configuration passes.
	pg = newFakePostgres(t)
	path := writeConfig(t, 0o600, validConfig(pg.url()))
	if r := runLuxd(t, nil, "--config", path, "validate"); r.code != 0 {
		t.Fatalf("validate: %+v", r)
	}
	s := runLuxd(t, nil, "--config", path, "serve")
	if s.code != 1 || !strings.Contains(s.stderr, "connect to database") {
		t.Fatalf("serve: %+v", s)
	}
	if n := pg.connections(); n == 0 {
		t.Fatal("serve never reached the fake database: the no-connection assertions prove nothing")
	}
}
