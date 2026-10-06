package ec2

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/server"
)

func TestRenderUserData(t *testing.T) {
	env := map[string]string{"LUX_URL": "http://10.0.1.10:7070", "LUX_HOST_TOKEN": "luxh_x", "LUX_HOST_NAME": "h", "LUX_EC2_IMDS": "http://169.254.169.254"}

	ign, err := renderUserData("", env)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(ign, &m); err != nil {
		t.Fatalf("default format is not valid JSON (want Ignition): %v", err)
	}
	if m["ignition"].(map[string]any)["version"] == nil {
		t.Error("default format has no ignition.version")
	}

	ign2, err := renderUserData("ignition", env)
	if err != nil {
		t.Fatal(err)
	}
	if string(ign) != string(ign2) {
		t.Error(`"" and "ignition" rendered differently`)
	}

	script, err := renderUserData("script", env)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(script), "#!/bin/bash\n") || !strings.Contains(string(script), "LUX_URL='http://10.0.1.10:7070'") {
		t.Errorf("script format: %s", script[:200])
	}

	lines, err := renderUserData("env", env)
	if err != nil {
		t.Fatal(err)
	}
	if string(lines) != "LUX_URL=http://10.0.1.10:7070\nLUX_HOST_TOKEN=luxh_x\nLUX_HOST_NAME=h\nLUX_EC2_IMDS=http://169.254.169.254\n" {
		t.Errorf("env format: %q", lines)
	}

	if _, err := renderUserData("cloud-init-yaml", env); err == nil {
		t.Error("an unknown format was not rejected")
	}
}

// A template tag with a key lux also sets (lux:pool, as the Terraform
// module once generated) is sent once, lux's value: EC2 refuses a request
// naming a key twice ("Duplicate tag key"), which failed every launch.
func TestLaunchSendsEachTagKeyOnceLuxWins(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_CONFIG_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_PROFILE", "")
	var sent [][2]string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		for i := 1; ; i++ {
			k := r.PostForm.Get(fmt.Sprintf("TagSpecification.1.Tag.%d.Key", i))
			if k == "" {
				break
			}
			sent = append(sent, [2]string{k, r.PostForm.Get(fmt.Sprintf("TagSpecification.1.Tag.%d.Value", i))})
		}
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprint(w, `<RunInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><reservationId>r-1</reservationId>`+
			`<instancesSet><item><instanceId>i-0abc</instanceId><instanceType>m8g.2xlarge</instanceType>`+
			`<placement><availabilityZone>eu-north-1a</availabilityZone></placement>`+
			`<instanceState><code>0</code><name>pending</name></instanceState></item></instancesSet></RunInstancesResponse>`)
	}))
	defer fake.Close()
	template := `{"region": "eu-north-1", "launchTemplate": "lt-1", "userData": "env", "tags": {"lux:pool": "arm64", "team": "platform"}}`
	lux := map[string]string{"lux:managed": "true", "lux:pool": "ten_1/default"}
	if _, err := New(fake.URL, discard).Launch(context.Background(), json.RawMessage(template), lux, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	want := [][2]string{{"lux:managed", "true"}, {"lux:pool", "ten_1/default"}, {"team", "platform"}}
	if !slices.Equal(sent, want) {
		t.Errorf("tags sent %v, want %v", sent, want)
	}
}

// Launch returns what RunInstances reports: the instance type (not the
// template's) and the zone; the market follows the template's spot.
func TestLaunchReturnsInstanceFacts(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_CONFIG_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_PROFILE", "")
	var markets []string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		markets = append(markets, r.PostForm.Get("InstanceMarketOptions.MarketType"))
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprint(w, `<RunInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><reservationId>r-1</reservationId>`+
			`<instancesSet><item><instanceId>i-0abc</instanceId><instanceType>m7i.2xlarge</instanceType>`+
			`<placement><availabilityZone>eu-west-1b</availabilityZone></placement>`+
			`<instanceState><code>0</code><name>pending</name></instanceState></item></instancesSet></RunInstancesResponse>`)
	}))
	defer fake.Close()
	p := New(fake.URL, discard)
	for _, c := range []struct {
		template, market string
	}{
		{`{"region": "eu-west-1", "launchTemplate": "lt-1", "userData": "env"}`, server.MarketOnDemand},
		{`{"region": "eu-west-1", "launchTemplate": "lt-1", "userData": "env", "spot": true}`, server.MarketSpot},
	} {
		l, err := p.Launch(context.Background(), json.RawMessage(c.template), nil, map[string]string{})
		if err != nil {
			t.Fatal(err)
		}
		want := server.Launched{ProviderID: "i-0abc", InstanceType: "m7i.2xlarge", Zone: "eu-west-1b", Market: c.market}
		if l != want {
			t.Errorf("%s: got %+v, want %+v", c.template, l, want)
		}
	}
	if fmt.Sprint(markets) != "[ spot]" {
		t.Errorf("requested markets %q, want on-demand (none) then spot", markets)
	}
}

// The runner env a pool's instances boot with: LUX_NESTED=true only when the
// template opts in, and a caller's own LUX_NESTED never leaks through a
// template that does not.
func TestLaunchUserDataCarriesNestedOnlyForAnOptedInTemplate(t *testing.T) {
	url, _, userData := fakeEC2(t, 0)
	launch := func(template string, env map[string]string) string {
		t.Helper()
		if _, err := New(url, discard).Launch(context.Background(), json.RawMessage(template), nil, env); err != nil {
			t.Fatal(err)
		}
		return (*userData)[len(*userData)-1]
	}
	base := map[string]string{"LUX_URL": "http://10.0.1.10:7070", "LUX_HOST_TOKEN": "luxh_x", "LUX_HOST_NAME": "h", "LUX_RUNNER_MEMORY": "68719476736"}
	with := func(k, v string) map[string]string {
		m := maps.Clone(base)
		m[k] = v
		return m
	}
	for _, format := range []string{"env", "script", "ignition"} {
		for _, c := range []struct {
			name, nested string // nested: the template's nestedContainers, "" for absent
			caller       map[string]string
			want         string // LUX_NESTED in the runner env, "" for absent
		}{
			{"opted in", "true", base, "true"},
			{"opted in, caller says false", "true", with("LUX_NESTED", "false"), "true"},
			{"absent, caller says true", "", with("LUX_NESTED", "true"), ""},
			{"false, caller says true", "false", with("LUX_NESTED", "true"), ""},
		} {
			template := `{"launchTemplate":"lt-1","userData":"` + format + `"`
			if c.nested != "" {
				template += `,"nestedContainers":` + c.nested
			}
			got := runnerEnvOf(t, format, launch(template+"}", c.caller))
			if v, ok := got["LUX_NESTED"]; v != c.want || ok != (c.want != "") {
				t.Errorf("%s, %s: LUX_NESTED=%q (set %v), want %q", format, c.name, v, ok, c.want)
			}
			if got["LUX_RUNNER_MEMORY"] != "68719476736" || got["LUX_URL"] != base["LUX_URL"] {
				t.Errorf("%s, %s: runner env lost the memory or URL: %v", format, c.name, got)
			}
		}
	}
}

// attempt is one RunInstances request the capacity fake received.
type attempt struct{ instanceType, subnet, memory string }

// capacityEC2 is a fake EC2 whose RunInstances fails with fail[{type,
// subnet}] (an EC2 error code) and otherwise launches the requested type;
// "*" as the subnet fails the type everywhere. Capacity errors and
// InternalError come with a 500, as EC2 sends them, others with a 400.
// DescribeInstanceTypes answers memMiB[type] (an error when absent).
func capacityEC2(t *testing.T, fail map[[2]string]string, memMiB map[string]int) (url string, attempts *[]attempt, calls map[string]int) {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_CONFIG_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_RETRY_MODE", "")
	t.Setenv("AWS_MAX_ATTEMPTS", "")
	attempts, calls = &[]attempt{}, map[string]int{}
	var mu sync.Mutex
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		defer mu.Unlock()
		action := r.PostForm.Get("Action")
		calls[action]++
		w.Header().Set("Content-Type", "text/xml")
		reply := func(status int, code string) {
			w.WriteHeader(status)
			fmt.Fprintf(w, `<Response><Errors><Error><Code>%s</Code><Message>no %s (fake)</Message></Error></Errors><RequestID>1</RequestID></Response>`, code, code)
		}
		switch action {
		case "DescribeInstanceTypes":
			it := r.PostForm.Get("InstanceType.1")
			mem, ok := memMiB[it]
			if !ok {
				reply(http.StatusBadRequest, "InvalidInstanceType")
				return
			}
			fmt.Fprintf(w, `<DescribeInstanceTypesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><instanceTypeSet><item>`+
				`<instanceType>%s</instanceType><memoryInfo><sizeInMiB>%d</sizeInMiB></memoryInfo></item></instanceTypeSet></DescribeInstanceTypesResponse>`, it, mem)
		case "RunInstances":
			it, sn := r.PostForm.Get("InstanceType"), r.PostForm.Get("SubnetId")
			ud, _ := base64.StdEncoding.DecodeString(r.PostForm.Get("UserData"))
			*attempts = append(*attempts, attempt{it, sn, runnerEnvOf(t, "env", string(ud))["LUX_RUNNER_MEMORY"]})
			code := fail[[2]string{it, sn}]
			if code == "" {
				code = fail[[2]string{it, "*"}]
			}
			switch code {
			case "":
			case "InsufficientInstanceCapacity", "InsufficientCapacity", "Unsupported", "InternalError":
				reply(http.StatusInternalServerError, code)
				return
			default:
				reply(http.StatusBadRequest, code)
				return
			}
			fmt.Fprintf(w, `<RunInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><reservationId>r-1</reservationId>`+
				`<instancesSet><item><instanceId>i-0abc</instanceId><instanceType>%s</instanceType>`+
				`<placement><availabilityZone>%s-az</availabilityZone></placement>`+
				`<instanceState><code>0</code><name>pending</name></instanceState></item></instancesSet></RunInstancesResponse>`, it, sn)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(fake.Close)
	return fake.URL, attempts, calls
}

const fallbackTemplate = `{"region": "eu-north-1", "launchTemplate": "lt-1", "userData": "env",
	"instanceType": "m8g.2xlarge", "fallbackInstanceTypes": ["m7g.2xlarge"], "subnets": ["subnet-a", "subnet-b", "subnet-c"]}`

// With no capacity anywhere for the first type, Launch tries it in every
// subnet from the pool's next one round, then the fallback the same way,
// once each (the SDK does not retry a capacity error), and reports the
// instance type and zone of the one that launched.
func TestLaunchFallsBackTypeMajorFromTheRoundRobinSubnet(t *testing.T) {
	url, attempts, _ := capacityEC2(t, map[[2]string]string{
		{"m8g.2xlarge", "*"}:        "InsufficientInstanceCapacity",
		{"m7g.2xlarge", "subnet-b"}: "Unsupported",
		{"m7g.2xlarge", "subnet-c"}: "InsufficientCapacity",
	}, map[string]int{"m8g.2xlarge": 32768, "m7g.2xlarge": 32768})
	p := New(url, discard)
	p.next["lt-1"] = 1 // this launch starts at subnet-b
	l, err := p.Launch(context.Background(), json.RawMessage(fallbackTemplate), nil, map[string]string{"LUX_URL": "http://luxd"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range *attempts {
		got = append(got, a.instanceType+"@"+a.subnet)
	}
	want := []string{"m8g.2xlarge@subnet-b", "m8g.2xlarge@subnet-c", "m8g.2xlarge@subnet-a",
		"m7g.2xlarge@subnet-b", "m7g.2xlarge@subnet-c", "m7g.2xlarge@subnet-a"}
	if !slices.Equal(got, want) {
		t.Errorf("attempts %v, want %v", got, want)
	}
	if l.InstanceType != "m7g.2xlarge" || l.Zone != "subnet-a-az" || l.ProviderID != "i-0abc" {
		t.Errorf("launched %+v, want m7g.2xlarge in subnet-a-az", l)
	}
	if p.next["lt-1"] != 2 {
		t.Errorf("round robin at %d after one launch from 1, want 2", p.next["lt-1"])
	}
}

// Only capacity errors move to the next candidate: a quota, a permission or
// a bad parameter would fail every candidate the same way, so Launch
// returns it after its one request.
func TestLaunchDoesNotFallBackOnOtherErrors(t *testing.T) {
	for _, code := range []string{"InstanceLimitExceeded", "VcpuLimitExceeded", "MaxSpotInstanceCountExceeded",
		"UnauthorizedOperation", "InvalidParameterValue"} {
		t.Run(code, func(t *testing.T) {
			url, attempts, _ := capacityEC2(t, map[[2]string]string{{"m8g.2xlarge", "*"}: code}, nil)
			_, err := New(url, discard).Launch(context.Background(), json.RawMessage(fallbackTemplate), nil, map[string]string{})
			if err == nil || !strings.Contains(err.Error(), code) {
				t.Errorf("err %v, want %s", err, code)
			}
			if len(*attempts) != 1 {
				t.Errorf("%d RunInstances requests, want 1: %v", len(*attempts), *attempts)
			}
		})
	}
}

// A non-capacity error after capacity failures ends the launch there and
// says which candidate it came from.
func TestLaunchStopsAtANonCapacityErrorMidway(t *testing.T) {
	url, attempts, _ := capacityEC2(t, map[[2]string]string{
		{"m8g.2xlarge", "*"}:        "InsufficientInstanceCapacity",
		{"m7g.2xlarge", "subnet-a"}: "VcpuLimitExceeded",
	}, nil)
	_, err := New(url, discard).Launch(context.Background(), json.RawMessage(fallbackTemplate), nil, map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "VcpuLimitExceeded") || !strings.Contains(err.Error(), "m7g.2xlarge in subnet-a") {
		t.Errorf("err %v, want VcpuLimitExceeded naming m7g.2xlarge in subnet-a", err)
	}
	if len(*attempts) != 4 {
		t.Errorf("%d RunInstances requests, want 4: %v", len(*attempts), *attempts)
	}
}

// Every candidate without capacity: the error leads with the code (the
// provisioner keeps 200 characters), names each attempt, and ends with
// EC2's own error. One request per candidate: none is SDK-retried.
func TestLaunchWithNoCapacityAnywhere(t *testing.T) {
	url, attempts, _ := capacityEC2(t, map[[2]string]string{
		{"m8g.2xlarge", "*"}: "InsufficientInstanceCapacity",
		{"m7g.2xlarge", "*"}: "InsufficientInstanceCapacity",
	}, nil)
	_, err := New(url, discard).Launch(context.Background(), json.RawMessage(fallbackTemplate), nil, map[string]string{})
	if err == nil {
		t.Fatal("launched without capacity")
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "ec2 RunInstances: InsufficientInstanceCapacity for every candidate: m8g.2xlarge in subnet-a, ") {
		t.Errorf("err %q", msg)
	}
	for _, c := range []string{"m8g.2xlarge in subnet-c", "m7g.2xlarge in subnet-a", "m7g.2xlarge in subnet-c", "no InsufficientInstanceCapacity (fake)"} {
		if !strings.Contains(msg, c) {
			t.Errorf("err %q does not name %q", msg, c)
		}
	}
	if len(*attempts) != 6 {
		t.Errorf("%d RunInstances requests for 6 candidates: %v", len(*attempts), *attempts)
	}
}

// A template without subnets tries its types only, with no SubnetId; one
// without fallbacks makes one request, as before.
func TestLaunchWithoutSubnetsTriesEachType(t *testing.T) {
	url, attempts, _ := capacityEC2(t, map[[2]string]string{{"m8g.2xlarge", ""}: "InsufficientInstanceCapacity"}, nil)
	p := New(url, discard)
	template := `{"launchTemplate": "lt-1", "userData": "env", "instanceType": "m8g.2xlarge", "fallbackInstanceTypes": ["m7g.2xlarge", "c8g.2xlarge"]}`
	l, err := p.Launch(context.Background(), json.RawMessage(template), nil, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []attempt{{"m8g.2xlarge", "", ""}, {"m7g.2xlarge", "", ""}}; !slices.Equal(*attempts, want) {
		t.Errorf("attempts %v, want %v", *attempts, want)
	}
	if l.InstanceType != "m7g.2xlarge" {
		t.Errorf("launched %+v", l)
	}
	*attempts = nil
	if _, err := p.Launch(context.Background(), json.RawMessage(`{"launchTemplate": "lt-1", "userData": "env", "instanceType": "m8g.2xlarge"}`), nil, map[string]string{}); err == nil {
		t.Error("launched without capacity")
	}
	if len(*attempts) != 1 {
		t.Errorf("%d requests without fallbacks or subnets, want 1", len(*attempts))
	}
}

// Errors the SDK retries for any call (a 500 InternalError here) are still
// retried for RunInstances: only capacity errors are not.
func TestLaunchKeepsTheSDKRetryingOtherErrors(t *testing.T) {
	url, attempts, _ := capacityEC2(t, map[[2]string]string{{"m8g.2xlarge", "subnet-a"}: "InternalError"}, nil)
	_, err := New(url, discard).Launch(context.Background(), json.RawMessage(fallbackTemplate), nil, map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "InternalError") || !strings.Contains(err.Error(), "exceeded maximum number of attempts, 3") {
		t.Errorf("err %v, want InternalError after the SDK's 3 attempts", err)
	}
	if len(*attempts) != 3 {
		t.Errorf("%d RunInstances requests, want the SDK's 3 for one candidate", len(*attempts))
	}
}

// Each attempt's user data offers the memory of the type it asks for; a
// caller's LUX_RUNNER_MEMORY stays for every attempt.
func TestLaunchUserDataCarriesEachTypesMemory(t *testing.T) {
	fail := map[[2]string]string{{"m8g.2xlarge", "*"}: "InsufficientInstanceCapacity", {"m7g.2xlarge", "*"}: "InsufficientInstanceCapacity"}
	mem := map[string]int{"m8g.2xlarge": 32768, "m7g.2xlarge": 16384}
	gib := func(n int64) string { return strconv.FormatInt(n<<30, 10) }

	url, attempts, calls := capacityEC2(t, fail, mem)
	if _, err := New(url, discard).Launch(context.Background(), json.RawMessage(fallbackTemplate), nil, map[string]string{}); err == nil {
		t.Fatal("launched without capacity")
	}
	for _, a := range *attempts {
		if want := map[string]string{"m8g.2xlarge": gib(32), "m7g.2xlarge": gib(16)}[a.instanceType]; a.memory != want {
			t.Errorf("%s in %s offered %q, want %s", a.instanceType, a.subnet, a.memory, want)
		}
	}
	if calls["DescribeInstanceTypes"] != 2 {
		t.Errorf("%d DescribeInstanceTypes for two types", calls["DescribeInstanceTypes"])
	}

	url, attempts, _ = capacityEC2(t, fail, mem)
	if _, err := New(url, discard).Launch(context.Background(), json.RawMessage(fallbackTemplate), nil, map[string]string{"LUX_RUNNER_MEMORY": "1234"}); err == nil {
		t.Fatal("launched without capacity")
	}
	for _, a := range *attempts {
		if a.memory != "1234" {
			t.Errorf("%s in %s offered %q, want the caller's 1234", a.instanceType, a.subnet, a.memory)
		}
	}
}

// The round robin moves one subnet per Launch, however many attempts the
// launch took.
func TestLaunchAdvancesTheRoundRobinOncePerLaunch(t *testing.T) {
	url, attempts, _ := capacityEC2(t, map[[2]string]string{{"m8g.2xlarge", "subnet-a"}: "InsufficientInstanceCapacity"}, nil)
	p := New(url, discard)
	var first []string
	for range 4 {
		*attempts = nil
		if _, err := p.Launch(context.Background(), json.RawMessage(fallbackTemplate), nil, map[string]string{}); err != nil {
			t.Fatal(err)
		}
		first = append(first, (*attempts)[0].subnet)
	}
	if want := []string{"subnet-a", "subnet-b", "subnet-c", "subnet-a"}; !slices.Equal(first, want) {
		t.Errorf("launches started at %v, want %v", first, want)
	}
}

// runnerEnvOf is the runner env user data in format carries, key to value:
// env's lines, script's exports, or Ignition's decoded runner.env.
func runnerEnvOf(t *testing.T, format, ud string) map[string]string {
	t.Helper()
	text := ud
	if format == "ignition" {
		text = decodedUserData(t, ud)
	}
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		if format == "script" {
			var ok bool
			if line, ok = strings.CutPrefix(line, "export "); !ok {
				continue
			}
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || !strings.HasPrefix(k, "LUX_") {
			continue
		}
		if format == "script" {
			v = strings.TrimSuffix(strings.TrimPrefix(v, "'"), "'")
		}
		out[k] = v
	}
	return out
}

// decodedUserData is user data with Ignition's embedded runner.env decoded,
// so every format can be searched as text.
func decodedUserData(t *testing.T, ud string) string {
	t.Helper()
	var cfg struct {
		Storage struct {
			Files []struct {
				Path     string `json:"path"`
				Contents struct {
					Source string `json:"source"`
				} `json:"contents"`
			} `json:"files"`
		} `json:"storage"`
	}
	if json.Unmarshal([]byte(ud), &cfg) != nil {
		return ud
	}
	for _, f := range cfg.Storage.Files {
		if f.Path == "/etc/lux/runner.env" {
			data, ok := strings.CutPrefix(f.Contents.Source, "data:;base64,")
			if !ok {
				t.Fatalf("runner.env source is not a base64 data URL: %.40s", f.Contents.Source)
			}
			raw, err := base64.StdEncoding.DecodeString(data)
			if err != nil {
				t.Fatalf("runner.env source: %v", err)
			}
			return string(raw)
		}
	}
	t.Fatal("ignition config has no /etc/lux/runner.env")
	return ""
}

// fakeEC2 answers DescribeInstanceTypes (memMiB, an error when 0, no answer
// until the client gives up when negative) and RunInstances, counting calls
// and keeping each launch's user data.
func fakeEC2(t *testing.T, memMiB int) (url string, calls map[string]int, userData *[]string) {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_CONFIG_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_PROFILE", "")
	calls, userData = map[string]int{}, &[]string{}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		action := r.PostForm.Get("Action")
		calls[action]++
		w.Header().Set("Content-Type", "text/xml")
		switch action {
		case "DescribeInstanceTypes":
			if memMiB < 0 {
				<-r.Context().Done()
				return
			}
			if memMiB == 0 {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `<Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>no</Message></Error></Errors><RequestID>1</RequestID></Response>`)
				return
			}
			fmt.Fprintf(w, `<DescribeInstanceTypesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><instanceTypeSet><item>`+
				`<instanceType>%s</instanceType><memoryInfo><sizeInMiB>%d</sizeInMiB></memoryInfo></item></instanceTypeSet></DescribeInstanceTypesResponse>`,
				r.PostForm.Get("InstanceType.1"), memMiB)
		case "RunInstances":
			ud, _ := base64.StdEncoding.DecodeString(r.PostForm.Get("UserData"))
			*userData = append(*userData, string(ud))
			fmt.Fprint(w, `<RunInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><reservationId>r-1</reservationId>`+
				`<instancesSet><item><instanceId>i-0abc</instanceId><instanceType>c7a.8xlarge</instanceType>`+
				`<instanceState><code>0</code><name>pending</name></instanceState></item></instancesSet></RunInstancesResponse>`)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(fake.Close)
	return fake.URL, calls, userData
}

// A template naming its instance type launches a runner offering that
// type's gross memory; the type is looked up once for the process.
func TestLaunchOffersTheInstanceTypesMemory(t *testing.T) {
	url, calls, userData := fakeEC2(t, 65536)
	p := New(url, discard)
	template := json.RawMessage(`{"region": "eu-west-1", "launchTemplate": "lt-1", "userData": "env", "instanceType": "c7a.8xlarge"}`)
	for range 2 {
		if _, err := p.Launch(context.Background(), template, nil, map[string]string{"LUX_URL": "http://luxd"}); err != nil {
			t.Fatal(err)
		}
	}
	if calls["DescribeInstanceTypes"] != 1 || calls["RunInstances"] != 2 {
		t.Errorf("calls %v, want one DescribeInstanceTypes for two RunInstances", calls)
	}
	for _, ud := range *userData {
		if !slices.Contains(strings.Split(ud, "\n"), fmt.Sprintf("LUX_RUNNER_MEMORY=%d", int64(65536)<<20)) {
			t.Errorf("user data without the type's memory:\n%s", ud)
		}
	}
}

// A failed lookup never fails the launch: the runner offers its MemTotal.
func TestLaunchWithoutTheInstanceTypesMemory(t *testing.T) {
	url, calls, userData := fakeEC2(t, 0)
	template := json.RawMessage(`{"region": "eu-west-1", "launchTemplate": "lt-1", "userData": "env", "instanceType": "c7a.8xlarge"}`)
	if _, err := New(url, discard).Launch(context.Background(), template, nil, map[string]string{"LUX_URL": "http://luxd"}); err != nil {
		t.Fatalf("launch failed with the lookup: %v", err)
	}
	if calls["RunInstances"] != 1 || len(*userData) != 1 || strings.Contains((*userData)[0], "LUX_RUNNER_MEMORY") {
		t.Errorf("calls %v, user data %q: want one launch without LUX_RUNNER_MEMORY", calls, *userData)
	}
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// A lookup that never answers costs its own timeout, not the launch; the
// type is not asked again while its failure is recent.
func TestLaunchDoesNotWaitOnAHangingLookup(t *testing.T) {
	url, calls, userData := fakeEC2(t, -1)
	p := New(url, discard)
	p.memoryTimeout = 100 * time.Millisecond
	template := json.RawMessage(`{"region": "eu-west-1", "launchTemplate": "lt-1", "userData": "env", "instanceType": "c7a.8xlarge"}`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for range 2 {
		start := time.Now()
		if _, err := p.Launch(ctx, template, nil, map[string]string{"LUX_URL": "http://luxd"}); err != nil {
			t.Fatalf("launch failed with the lookup hanging: %v", err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("launch took %s with a %s lookup timeout", d, p.memoryTimeout)
		}
	}
	if calls["DescribeInstanceTypes"] != 1 || calls["RunInstances"] != 2 {
		t.Errorf("calls %v, want one DescribeInstanceTypes for two RunInstances", calls)
	}
	for _, ud := range *userData {
		if strings.Contains(ud, "LUX_RUNNER_MEMORY") {
			t.Errorf("user data with a memory nobody gave:\n%s", ud)
		}
	}
}
