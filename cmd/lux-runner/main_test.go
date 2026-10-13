package main

import (
	"flag"
	"io"
	"testing"
)

// parse runs config over args with env as the environment, keeping the
// fields provisioning sets.
func parse(t *testing.T, env map[string]string, args ...string) (configView, error) {
	t.Helper()
	fs := flag.NewFlagSet("lux-runner", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfg, _, err := config(fs, args, func(k string) string { return env[k] })
	v := configView{CPUs: cfg.CPUs, Memory: cfg.Memory, Disk: cfg.Disk, HeadroomSet: cfg.DiskHeadroom != nil}
	if cfg.DiskHeadroom != nil {
		v.Headroom = *cfg.DiskHeadroom
	}
	return v, err
}

type configView struct {
	CPUs        float64
	Memory      int64
	Disk        int64
	HeadroomSet bool
	Headroom    int64
}

// A provisioned host is configured only through its environment
// (/etc/lux/runner.env): each variable reaches the Config, and a headroom
// of 0 is a headroom, which turns disk scaling on.
func TestConfigFromTheEnvironment(t *testing.T) {
	for _, c := range []struct {
		name string
		env  map[string]string
		args []string
		want configView
	}{
		{"none", nil, nil, configView{}},
		{"shaped host", map[string]string{"LUX_RUNNER_CPUS": "0.5", "LUX_RUNNER_MEMORY": "2147483648",
			"LUX_RUNNER_DISK": "21474836480", "LUX_RUNNER_DISK_HEADROOM": "10737418240"}, nil,
			configView{CPUs: 0.5, Memory: 2 << 30, Disk: 20 << 30, HeadroomSet: true, Headroom: 10 << 30}},
		{"zero headroom", map[string]string{"LUX_RUNNER_DISK": "100", "LUX_RUNNER_DISK_HEADROOM": "0"}, nil,
			configView{Disk: 100, HeadroomSet: true}},
		{"empty headroom is unset", map[string]string{"LUX_RUNNER_DISK": "100", "LUX_RUNNER_DISK_HEADROOM": ""}, nil,
			configView{Disk: 100}},
		{"flags win", map[string]string{"LUX_RUNNER_CPUS": "8", "LUX_RUNNER_DISK": "100", "LUX_RUNNER_DISK_HEADROOM": "7"},
			[]string{"--cpus", "16", "--disk", "200", "--disk-headroom", "0"},
			configView{CPUs: 16, Disk: 200, HeadroomSet: true}},
		{"zero headroom flag", nil, []string{"--disk", "100", "--disk-headroom", "0"}, configView{Disk: 100, HeadroomSet: true}},
	} {
		got, err := parse(t, c.env, c.args...)
		if err != nil || got != c.want {
			t.Errorf("%s: %+v (err %v), want %+v", c.name, got, err, c.want)
		}
	}
}

// Sizes are plain bytes; an unusable variable or a negative size stops
// the runner rather than leaving a host unshaped.
func TestConfigRefuses(t *testing.T) {
	for _, c := range []struct {
		name string
		env  map[string]string
		args []string
	}{
		{"suffixed disk", map[string]string{"LUX_RUNNER_DISK": "20Gi"}, nil},
		{"suffixed headroom", map[string]string{"LUX_RUNNER_DISK_HEADROOM": "10Gi"}, nil},
		{"cpus not a number", map[string]string{"LUX_RUNNER_CPUS": "eight"}, nil},
		{"cpus NaN", map[string]string{"LUX_RUNNER_CPUS": "NaN"}, nil},
		{"suffixed headroom flag", nil, []string{"--disk-headroom", "10Gi"}},
		{"negative headroom", map[string]string{"LUX_RUNNER_DISK_HEADROOM": "-1"}, nil},
		{"negative cpus", nil, []string{"--cpus", "-1"}},
	} {
		if got, err := parse(t, c.env, c.args...); err == nil {
			t.Errorf("%s: accepted, %+v", c.name, got)
		}
	}
}
