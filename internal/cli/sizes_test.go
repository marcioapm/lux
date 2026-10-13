package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/server"
)

// mShape is the shape of the fake luxd's pool "m", as GET /v1/pools lists it.
const mShape = `"shape":{"family":"m","arch":"arm64","cpus":8,"memory":34359738368,"disk":171798691840,"minCpus":0.5,"defaultCpus":2},
	"sizes":[{"name":"m-05-arm64","cpus":0.5,"memory":2147483648,"disk":10737418240,"fraction":0.0625},
		{"name":"m-1-arm64","cpus":1,"memory":4294967296,"disk":21474836480,"fraction":0.125},
		{"name":"m-2-arm64","cpus":2,"memory":8589934592,"disk":42949672960,"fraction":0.25,"default":true},
		{"name":"m-4-arm64","cpus":4,"memory":17179869184,"disk":85899345920,"fraction":0.5},
		{"name":"m-8-arm64","cpus":8,"memory":34359738368,"disk":171798691840,"fraction":1}]`

// --size sets resources.size and nothing else of resources.
func TestRunSizeFlag(t *testing.T) {
	var posted []map[string]any
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		posted = append(posted, body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"run_1","state":"submitted"}`))
	})
	out, err := runCLI(t, h, "run", "--image", "alpine", "--pool", "m", "--size", "m-2-arm64", "--", "true")
	if err != nil || out != "run_1\n" {
		t.Fatalf("run: %q %v", out, err)
	}
	if res := posted[0]["resources"]; !reflect.DeepEqual(res, map[string]any{"size": "m-2-arm64"}) {
		t.Errorf("POSTed resources %v, want only size", res)
	}
	posted = nil
	if _, err := runCLI(t, h, "run", "--image", "alpine", "--", "true"); err != nil {
		t.Fatal(err)
	}
	if res := posted[0]["resources"]; !reflect.DeepEqual(res, map[string]any{}) {
		t.Errorf("without --size: resources %v, want none", res)
	}
}

// --size sends resources.size, with --pool the move; the answer's size is
// printed on stderr.
func TestResumeSizeFlag(t *testing.T) {
	var posted []map[string]any
	h := poolsAndResume(t, http.StatusAccepted, `{"id":"run_1","state":"resuming","resize":{"requested":{"size":"m-4-arm64"},
		"applied":{"cpus":4,"memory":17179869184,"disk":85899345920},"size":"m-4-arm64"}}`, &posted)
	out, errOut, err := runCLIErr(t, h, "resume", "run_1", "--secret", "TOKEN=x", "--pool", "gpu", "--size", "m-4-arm64")
	if err != nil {
		t.Fatal(err)
	}
	if len(posted) != 1 || !reflect.DeepEqual(posted[0]["resources"], map[string]any{"size": "m-4-arm64"}) ||
		!reflect.DeepEqual(posted[0]["placement"], map[string]any{"poolId": "pool_gpu"}) {
		t.Errorf("POSTed %v", posted)
	}
	if out != "run_1 resuming\n" || !strings.Contains(errOut, "size: m-4-arm64\n") {
		t.Errorf("stdout %q stderr %q", out, errOut)
	}
}

// pools sizes lists a shaped pool's ladder, the default starred; -o json
// the sizes; an unshaped pool is an error naming it.
func TestPoolsSizes(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"pools":[{"id":"pool_m","name":"m","provider":"ec2",` + mShape + `},{"id":"pool_p","name":"plain","provider":"static"}]}`))
	})
	out, err := runCLI(t, h, "pools", "sizes", "m")
	if err != nil {
		t.Fatal(err)
	}
	want := "SIZE        DEFAULT  CPUS  MEMORY    DISK       FRACTION\n" +
		"m-05-arm64           0.5   2.0 GiB   10.0 GiB   0.0625\n" +
		"m-1-arm64            1     4.0 GiB   20.0 GiB   0.125\n" +
		"m-2-arm64   *        2     8.0 GiB   40.0 GiB   0.25\n" +
		"m-4-arm64            4     16.0 GiB  80.0 GiB   0.5\n" +
		"m-8-arm64            8     32.0 GiB  160.0 GiB  1\n"
	if out != want {
		t.Errorf("table:\n%s\nwant:\n%s", out, want)
	}
	out, err = runCLI(t, h, "-o", "json", "pools", "sizes", "pool_m")
	var sizes []server.PoolSize
	if err != nil || json.Unmarshal([]byte(out), &sizes) != nil || len(sizes) != 5 || sizes[2].Name != "m-2-arm64" || !sizes[2].Default {
		t.Errorf("json: %q %v", out, err)
	}
	if _, err := runCLI(t, h, "pools", "sizes", "plain"); err == nil || !strings.Contains(err.Error(), "pool plain has no shape") {
		t.Errorf("unshaped: %v", err)
	}
	if _, err := runCLI(t, h, "pools", "sizes", "nope"); err == nil || !strings.Contains(err.Error(), `no pool "nope"`) {
		t.Errorf("unknown: %v", err)
	}
}

// pools set --shape sends the parsed shape; a malformed one is refused
// before anything is sent; a set leaving out a pool's shape is a removal.
func TestPoolsSetShape(t *testing.T) {
	url, posted := poolsLuxd(t, false)
	code, _, stderr := lux(url, "pools", "set", "m", "--provider", "static",
		"--shape", "family=m,arch=arm64,cpus=8,memory=32Gi,disk=160Gi,min-cpus=0.5,default-cpus=2")
	if code != 0 || len(*posted) != 1 {
		t.Fatalf("exit %d (%s), posted %v", code, stderr, *posted)
	}
	want := map[string]any{"family": "m", "arch": "arm64", "cpus": 8.0, "memory": float64(32 << 30), "disk": float64(160 << 30), "minCpus": 0.5, "defaultCpus": 2.0}
	if got := (*posted)[0]["shape"]; !reflect.DeepEqual(got, want) {
		t.Errorf("shape %v, want %v", got, want)
	}
	if code, _, stderr = lux(url, "pools", "set", "m", "--provider", "static", "--shape", "family=m,arch=arm64"); code == 0 || !strings.Contains(stderr, "missing cpus") || len(*posted) != 1 {
		t.Errorf("malformed: exit %d, stderr %q, posted %d", code, stderr, len(*posted))
	}

	url, posted = poolsLuxd(t, false, `{"name":"m","tenant":"t1","provider":"static",`+mShape+`}`)
	code, stdout, stderr := lux(url, "pools", "set", "m", "--provider", "static")
	if code != 4 || len(*posted) != 0 || !strings.Contains(stderr, "would remove shape.arch, shape.cpus") || !strings.Contains(stdout, "shape.family m→-") {
		t.Errorf("removal: exit %d, posted %v, stdout %q, stderr %q", code, *posted, stdout, stderr)
	}
}

// get prints a shaped Run's size and what its submit asked for; none for
// an unshaped Run.
func TestGetPrintsSize(t *testing.T) {
	run := `{"id":"run_1","state":"queued","spec":{"workload":{"adapter":"generic"}},"createdAt":"2026-10-01T00:00:00Z"`
	for _, c := range []struct{ body, want string }{
		{run + `,"sizing":{"requested":{"cpus":3,"memory":10737418240},"resolved":{"size":"m-4-arm64","cpus":4,"memory":17179869184,"disk":85899345920}}}`,
			"size:      m-4-arm64 (4 cpus, 16.0 GiB memory, 80.0 GiB disk); asked 3 cpus, 10.0 GiB memory\n"},
		{run + `,"sizing":{"requested":{},"resolved":{"size":"m-2-arm64","cpus":2,"memory":8589934592,"disk":42949672960}}}`,
			"size:      m-2-arm64 (2 cpus, 8.0 GiB memory, 40.0 GiB disk); asked the pool's default\n"},
		{run + `}`, ""},
	} {
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(c.body))
		})
		out, err := runCLI(t, h, "get", "run_1")
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		for _, l := range strings.SplitAfter(out, "\n") {
			if strings.HasPrefix(l, "size:") {
				got = l
			}
		}
		if got != c.want {
			t.Errorf("size line %q, want %q", got, c.want)
		}
	}
}
