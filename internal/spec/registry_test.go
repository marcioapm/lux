package spec

import (
	"strings"
	"testing"
)

func registrySpec() RunSpec {
	return RunSpec{Image: Image{Ref: "10.0.0.5:5000/team/app:1", RegistryAuth: []RegistryAuth{{Registry: "10.0.0.5:5000", Secret: "REG"}}},
		Workload: Workload{Command: []string{"true"}},
		Secrets:  []Secret{{Name: "REG", Value: "u:p"}, {Name: "OTHER", Value: "o"}}}
}

func TestRegistryAuth(t *testing.T) {
	s := registrySpec()
	if err := s.Normalize(BuiltinDefaults); err != nil {
		t.Fatal(err)
	}
	// The runner's alone: never in the container, nor any process's
	// environment.
	if !s.Secrets[0].RunnerOnly || s.Secrets[0].As != "none" || s.Secrets[1].RunnerOnly || s.Secrets[1].As != "env" {
		t.Fatalf("%+v", s.Secrets)
	}
	for _, reg := range []string{"ghcr.io", "123.dkr.ecr.eu-west-1.amazonaws.com", "10.0.0.5:5000", "registry"} {
		s := registrySpec()
		s.Image.RegistryAuth[0].Registry = reg
		if err := s.Normalize(BuiltinDefaults); err != nil {
			t.Errorf("%s: %v", reg, err)
		}
	}
	// Not the runner's own host (it pulls outside the Run's egress rules),
	// nor a spelling auth.json would not match.
	for _, reg := range []string{"", "https://ghcr.io", "ghcr.io/team", "ghcr.io:", "-x.io", "a b", "ghcr.io:5000:1",
		"localhost:5000", "127.0.0.1:2375", "169.254.169.254", "GHCR.io", "ghcr.io:99999", "ghcr.io:05000"} {
		s := registrySpec()
		s.Image.RegistryAuth[0].Registry = reg
		if err := s.Normalize(BuiltinDefaults); err == nil || !strings.Contains(err.Error(), "registryAuth[0].registry") {
			t.Errorf("%q: %v", reg, err)
		}
	}
	s = registrySpec()
	s.Image.RegistryAuth = append(s.Image.RegistryAuth, RegistryAuth{Registry: "10.0.0.5:5000", Secret: "OTHER"})
	if err := s.Normalize(BuiltinDefaults); err == nil || !strings.Contains(err.Error(), "duplicate registry") {
		t.Fatalf("duplicate: %v", err)
	}
	s = registrySpec()
	s.Image.RegistryAuth[0].Secret = "NOPE"
	if err := s.Normalize(BuiltinDefaults); err == nil || !strings.Contains(err.Error(), `no secret named "NOPE"`) {
		t.Fatalf("undeclared: %v", err)
	}
	// A header would put it in the container.
	s = registrySpec()
	s.Network.Unrestricted = true
	s.Workload.Services = []Service{{Name: "s", URL: "https://s.example/", Headers: []MCPHeader{{Name: "Authorization", Secret: "REG"}}}}
	if err := s.Normalize(BuiltinDefaults); err == nil || !strings.Contains(err.Error(), "is a registry credential") {
		t.Fatalf("header: %v", err)
	}
	// An explicit as: env does not take it into the container.
	s = registrySpec()
	s.Secrets[0].As = "env"
	if err := s.Normalize(BuiltinDefaults); err != nil || !s.Secrets[0].RunnerOnly {
		t.Fatalf("%v %+v", err, s.Secrets)
	}
}

func TestBuildCache(t *testing.T) {
	build := func(cache string) RunSpec {
		return RunSpec{Image: Image{Build: &Build{Containerfile: "FROM alpine", Cache: cache}},
			Workload: Workload{Command: []string{"true"}}}
	}
	for _, c := range []string{"registry.example.com/team/lux-cache", "10.0.0.5:5000/cache", "123.dkr.ecr.eu-west-1.amazonaws.com/lux/cache"} {
		s := build(c)
		if err := s.Normalize(BuiltinDefaults); err != nil {
			t.Errorf("%s: %v", c, err)
		}
	}
	for _, c := range []string{"lux-cache", "team/lux-cache", "registry.example.com/team/lux-cache:tag", "registry.example.com/team@sha256:ab",
		"https://registry.example.com/c", "registry.example.com/Team", "registry.example.com/",
		"localhost/c", "localhost/lux-build", "-a.b/c", "127.0.0.1:2375/x"} {
		s := build(c)
		if err := s.Normalize(BuiltinDefaults); err == nil || !strings.Contains(err.Error(), "image.build.cache") {
			t.Errorf("%q: %v", c, err)
		}
	}
}
