package shim

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// An OCI config, trimmed, as Podman hands a precreate hook.
func ociConfig(resources string) string {
	return `{"ociVersion":"1.2.1","process":{"args":["sh"]},"linux":{"resources":` + resources + `,"namespaces":[{"type":"cgroup"}]}}`
}

func runDefaults(t *testing.T, cfg string, args ...string) map[string]any {
	t.Helper()
	var out bytes.Buffer
	if code := OCIDefaults(args, strings.NewReader(cfg), &out); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var got map[string]any
	dec := json.NewDecoder(&out)
	dec.UseNumber()
	if err := dec.Decode(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

func resources(m map[string]any) map[string]any {
	r, _ := m["linux"].(map[string]any)["resources"].(map[string]any)
	return r
}

func TestOCIDefaultsFillsOnlyWhatIsUnset(t *testing.T) {
	const run = "1022312448"
	cases := []struct {
		name, res          string
		limit, swap, quota string // "" : absent
	}{
		{"no flags", `{"devices":[]}`, run, run, "200000"},
		{"no resources at all", `{}`, run, run, "200000"},
		{"-m wins", `{"memory":{"limit":536870912,"swap":1073741824}}`, "536870912", "1073741824", "200000"},
		{"-m 4g is passed on (the Run's cgroup still caps it)", `{"memory":{"limit":4294967296}}`, "4294967296", "", "200000"},
		{"--cpus wins", `{"cpu":{"quota":100000,"period":100000}}`, run, run, "100000"},
		{"--cgroup-conf memory.max wins", `{"unified":{"memory.max":"300000000"}}`, "", "", "200000"},
		{"--cgroup-conf cpu.max wins", `{"unified":{"cpu.max":"50000 100000"}}`, run, run, ""},
		{"--cpu-shares alone still gets the quota", `{"cpu":{"shares":512}}`, run, run, "200000"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := resources(runDefaults(t, ociConfig(c.res), run, "200000", "100000"))
			mem, _ := res["memory"].(map[string]any)
			cpu, _ := res["cpu"].(map[string]any)
			str := func(m map[string]any, k string) string {
				if v, ok := m[k]; ok {
					return v.(json.Number).String()
				}
				return ""
			}
			if got := str(mem, "limit"); got != c.limit {
				t.Errorf("memory.limit %q, want %q", got, c.limit)
			}
			if got := str(mem, "swap"); got != c.swap {
				t.Errorf("memory.swap %q, want %q", got, c.swap)
			}
			if got := str(cpu, "quota"); got != c.quota {
				t.Errorf("cpu.quota %q, want %q", got, c.quota)
			}
			if c.quota == "200000" && str(cpu, "period") != "100000" {
				t.Errorf("cpu.period %q", str(cpu, "period"))
			}
		})
	}
}

// Everything else in the config passes through, numbers as written.
func TestOCIDefaultsKeepsTheRest(t *testing.T) {
	in := `{"ociVersion":"1.2.1","process":{"user":{"uid":18446744073709551615}},"linux":{"resources":{"pids":{"limit":2048}},"uidMappings":[{"containerID":0,"hostID":1,"size":65536}]},"annotations":{"a":"b"}}`
	got := runDefaults(t, in, "0", "0", "0")
	b, _ := json.Marshal(got)
	if !strings.Contains(string(b), `"uid":18446744073709551615`) || !strings.Contains(string(b), `"pids":{"limit":2048}`) ||
		!strings.Contains(string(b), `"annotations":{"a":"b"}`) || strings.Contains(string(b), `"memory"`) || strings.Contains(string(b), `"cpu"`) {
		t.Fatalf("zero limits changed the config: %s", b)
	}
}

func TestOCIDefaultsRefusesBadArgs(t *testing.T) {
	for _, args := range [][]string{nil, {"1", "2"}, {"x", "1", "1"}, {"-1", "1", "1"}} {
		if code := OCIDefaults(args, strings.NewReader(ociConfig(`{}`)), &bytes.Buffer{}); code == 0 {
			t.Errorf("%q accepted", args)
		}
	}
	if code := OCIDefaults([]string{"1", "1", "1"}, strings.NewReader("not json"), &bytes.Buffer{}); code == 0 {
		t.Error("bad config accepted")
	}
}
