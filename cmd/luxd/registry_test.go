package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/server"
)

const goodRegistry = `
[registry]
host = "123456789012.dkr.ecr.eu-west-1.amazonaws.com"
region = "eu-west-1"
pull_role_arn = "arn:aws:iam::123456789012:role/lux-registry-pull"
[[registry.grants]]
owner = "acme"
[[registry.grants]]
owner = "team.b"
tenants = ["ten_1", "ten_2"]
`

func registryEnvClear(t *testing.T) {
	t.Helper()
	for _, k := range []string{"LUX_REGISTRY_HOST", "LUX_REGISTRY_REGION", "LUX_REGISTRY_PULL_ROLE_ARN", "LUX_REGISTRY_GRANTS"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

// [registry]: absent is off; a good section reaches the server with
// tenants defaulting to every tenant; the environment overrides the file.
func TestRegistryConfig(t *testing.T) {
	registryEnvClear(t)
	path := filepath.Join(t.TempDir(), "luxd.toml")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("")
	c, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.registryConfig(); got.Host != "" || got.Grants != nil {
		t.Fatalf("absent section: %+v, want off", got)
	}

	write(goodRegistry)
	if c, err = loadConfig(path); err != nil {
		t.Fatal(err)
	}
	want := server.RegistryConfig{Host: "123456789012.dkr.ecr.eu-west-1.amazonaws.com", Region: "eu-west-1",
		PullRoleARN: "arn:aws:iam::123456789012:role/lux-registry-pull",
		Grants:      []server.RegistryGrant{{Owner: "acme", Tenants: []string{"*"}}, {Owner: "team.b", Tenants: []string{"ten_1", "ten_2"}}}}
	if got := c.registryConfig(); !reflect.DeepEqual(got, want) {
		t.Fatalf("file:\n%+v\nwant\n%+v", got, want)
	}

	t.Setenv("LUX_REGISTRY_HOST", "210987654321.dkr.ecr.us-east-2.amazonaws.com")
	t.Setenv("LUX_REGISTRY_REGION", "us-east-2")
	t.Setenv("LUX_REGISTRY_PULL_ROLE_ARN", "arn:aws:iam::210987654321:role/other")
	t.Setenv("LUX_REGISTRY_GRANTS", `[{"owner":"envowner","tenants":["ten_9"]}]`)
	if c, err = loadConfig(path); err != nil {
		t.Fatal(err)
	}
	want = server.RegistryConfig{Host: "210987654321.dkr.ecr.us-east-2.amazonaws.com", Region: "us-east-2",
		PullRoleARN: "arn:aws:iam::210987654321:role/other", Grants: []server.RegistryGrant{{Owner: "envowner", Tenants: []string{"ten_9"}}}}
	if got := c.registryConfig(); !reflect.DeepEqual(got, want) {
		t.Fatalf("env over file:\n%+v\nwant\n%+v", got, want)
	}

	// The environment alone turns it on.
	write("")
	if c, err = loadConfig(path); err != nil {
		t.Fatal(err)
	}
	if got := c.registryConfig(); !reflect.DeepEqual(got, want) {
		t.Fatalf("env only:\n%+v\nwant\n%+v", got, want)
	}
}

// Each invalid field is refused at startup, naming it.
func TestRegistryConfigInvalid(t *testing.T) {
	registryEnvClear(t)
	path := filepath.Join(t.TempDir(), "luxd.toml")
	sub := func(old, new string) string {
		if !strings.Contains(goodRegistry, old) {
			t.Fatalf("fixture lacks %q", old)
		}
		return strings.Replace(goodRegistry, old, new, 1)
	}
	for name, bad := range map[string]struct{ file, env, want string }{
		"host shape":         {sub(`host = "123456789012.dkr.ecr.eu-west-1.amazonaws.com"`, `host = "ghcr.io"`), "", "registry.host"},
		"host with path":     {sub(`eu-west-1.amazonaws.com"`, `eu-west-1.amazonaws.com/acme"`), "", "registry.host"},
		"short account":      {sub(`host = "123456789012.`, `host = "12345.`), "", "registry.host"},
		"region missing":     {sub(`region = "eu-west-1"`, ``), "", "registry.region"},
		"region mismatch":    {sub(`region = "eu-west-1"`, `region = "us-east-1"`), "", "registry.region"},
		"arn shape":          {sub(`arn:aws:iam::123456789012:role/lux-registry-pull`, `lux-registry-pull`), "", "registry.pull_role_arn"},
		"arn not a role":     {sub(`:role/lux-registry-pull`, `:user/lux`), "", "registry.pull_role_arn"},
		"owner uppercase":    {sub(`owner = "acme"`, `owner = "Acme"`), "", "registry.grants[0].owner"},
		"owner slash":        {sub(`owner = "acme"`, `owner = "acme/x"`), "", "registry.grants[0].owner"},
		"owner empty":        {sub(`owner = "acme"`, `owner = ""`), "", "registry.grants[0].owner"},
		"owner too long":     {sub(`owner = "acme"`, `owner = "`+strings.Repeat("a", 51)+`"`), "", "registry.grants[0].owner"},
		"duplicate owner":    {sub(`owner = "team.b"`, `owner = "acme"`), "", `registry.grants[1].owner "acme": duplicate`},
		"empty tenants":      {sub(`tenants = ["ten_1", "ten_2"]`, `tenants = []`), "", "registry.grants[1].tenants"},
		"blank tenant":       {sub(`tenants = ["ten_1", "ten_2"]`, `tenants = [" "]`), "", "registry.grants[1].tenants"},
		"no grants":          {"[registry]\nhost = \"123456789012.dkr.ecr.eu-west-1.amazonaws.com\"\nregion = \"eu-west-1\"\npull_role_arn = \"arn:aws:iam::123456789012:role/r\"\n", "", "registry.grants"},
		"no host":            {"[registry]\nregion = \"eu-west-1\"\n", "", "registry: host"},
		"unknown key":        {sub(`region = "eu-west-1"`, "region = \"eu-west-1\"\nrole = \"x\""), "", "registry.role"},
		"env host":           {goodRegistry, "LUX_REGISTRY_HOST=quay.io", "registry.host"},
		"env grants json":    {goodRegistry, "LUX_REGISTRY_GRANTS=acme", "LUX_REGISTRY_GRANTS"},
		"env grants unknown": {goodRegistry, `LUX_REGISTRY_GRANTS=[{"owner":"a","team":"x"}]`, "LUX_REGISTRY_GRANTS"},
		"env grants owner":   {goodRegistry, `LUX_REGISTRY_GRANTS=[{"owner":"A B"}]`, "registry.grants[0].owner"},
	} {
		t.Run(name, func(t *testing.T) {
			registryEnvClear(t)
			if err := os.WriteFile(path, []byte(bad.file), 0o600); err != nil {
				t.Fatal(err)
			}
			if bad.env != "" {
				k, v, _ := strings.Cut(bad.env, "=")
				t.Setenv(k, v)
			}
			if _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), bad.want) {
				t.Fatalf("error %v, want it to name %q", err, bad.want)
			}
		})
	}
}
