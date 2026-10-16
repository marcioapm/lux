package shim

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
)

// OCIDefaults is `lux-shim oci-defaults MEMORY CPU_QUOTA CPU_PERIOD`, the
// precreate OCI hook nested Runs register with Podman inside them
// (docs/podman.md, Limits of nested containers). It reads the proposed OCI
// runtime config on stdin and writes it back with the Run's memory limit
// and CPU quota where the container was given none: a container in its own
// cgroup namespace cannot see the Run's limits above it, and ClickHouse or
// the JVM would size themselves to the host. An explicit -m, --cpus or
// --cgroup-conf memory.max/cpu.max is kept as given. 0 leaves a resource as
// it is.
func OCIDefaults(args []string, in io.Reader, out io.Writer) int {
	if err := ociDefaults(args, in, out); err != nil {
		fmt.Fprintln(os.Stderr, "lux-shim oci-defaults:", err)
		return 1
	}
	return 0
}

func ociDefaults(args []string, in io.Reader, out io.Writer) error {
	if len(args) != 3 {
		return fmt.Errorf("want MEMORY CPU_QUOTA CPU_PERIOD, got %q", args)
	}
	var n [3]int64
	for i, a := range args {
		v, err := strconv.ParseInt(a, 10, 64)
		if err != nil || v < 0 {
			return fmt.Errorf("%q: want a non-negative integer", a)
		}
		n[i] = v
	}
	memory, quota, period := n[0], n[1], n[2]
	dec := json.NewDecoder(in)
	// Numbers stay as written: the config carries 64-bit values.
	dec.UseNumber()
	var cfg map[string]any
	if err := dec.Decode(&cfg); err != nil {
		return err
	}
	linux := child(cfg, "linux")
	res := child(linux, "resources")
	unified, _ := res["unified"].(map[string]any)
	mem := child(res, "memory")
	if _, set := mem["limit"]; !set && unified["memory.max"] == nil && memory > 0 {
		mem["limit"] = memory
		if _, set := mem["swap"]; !set && unified["memory.swap.max"] == nil {
			mem["swap"] = memory
		}
	}
	cpu := child(res, "cpu")
	if _, set := cpu["quota"]; !set && unified["cpu.max"] == nil && quota > 0 && period > 0 {
		cpu["quota"], cpu["period"] = quota, period
	}
	prune(res, "memory")
	prune(res, "cpu")
	prune(linux, "resources")
	return json.NewEncoder(out).Encode(cfg)
}

// child is m[key] as an object, made if absent.
func child(m map[string]any, key string) map[string]any {
	c, ok := m[key].(map[string]any)
	if !ok {
		c = map[string]any{}
		m[key] = c
	}
	return c
}

// prune drops m[key] if it is an empty object, as it was before child.
func prune(m map[string]any, key string) {
	if c, ok := m[key].(map[string]any); ok && len(c) == 0 {
		delete(m, key)
	}
}
