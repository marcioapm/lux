package ec2

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/marcioapm/lux/internal/server"
)

// dryRunEC2 is a fake EC2 recording each RunInstances form. A dry run is
// answered as EC2 does (412 DryRunOperation) unless refuse names an error
// code for its launch template; a real one launches. DescribeInstanceTypes
// fails, so a launch's user data carries no memory and matches a check's.
func dryRunEC2(t *testing.T, refuse map[string]string) (string, func() []url.Values) {
	t.Helper()
	awsTestEnv(t)
	var mu sync.Mutex
	var forms []url.Values
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "text/xml")
		if r.PostForm.Get("Action") != "RunInstances" {
			ec2Error(w, http.StatusBadRequest, "UnauthorizedOperation", "not here (fake)")
			return
		}
		mu.Lock()
		forms = append(forms, r.PostForm)
		mu.Unlock()
		lt := r.PostForm.Get("LaunchTemplate.LaunchTemplateName") + r.PostForm.Get("LaunchTemplate.LaunchTemplateId")
		if code := refuse[lt]; code != "" {
			ec2Error(w, http.StatusBadRequest, code, "The specified launch template, with template name "+lt+", does not exist.")
			return
		}
		if r.PostForm.Get("DryRun") == "true" {
			ec2Error(w, http.StatusPreconditionFailed, "DryRunOperation", "Request would have succeeded, but DryRun flag is set.")
			return
		}
		fmt.Fprintf(w, `<RunInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><instancesSet><item><instanceId>i-1</instanceId>`+
			`<instanceType>%s</instanceType></item></instancesSet></RunInstancesResponse>`, r.PostForm.Get("InstanceType"))
	}))
	t.Cleanup(fake.Close)
	return fake.URL, func() []url.Values {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(forms)
	}
}

const checkTemplate = `{"region": "eu-north-1", "launchTemplate": "lux-runner-arm64", "userData": "env", "spot": true,
	"instanceType": "m8g.2xlarge", "fallbackInstanceTypes": ["m7g.2xlarge", "c8g.2xlarge"],
	"subnets": ["subnet-a", "subnet-b", "subnet-c"], "tags": {"team": "platform"}}`

var testTags = server.LaunchTags("0f3c9a", "pool_0123456789abcdef", "arm64", "host_0123456789abcdef")

// A check's first request is the request a production launch sends for
// the same candidate (server.LaunchTags, the provisioner's runner env), but
// for DryRun and what is the host's own: Name and lux:host carry another
// host id, and the user data has no LUX_URL, LUX_HOST_TOKEN or
// LUX_HOST_NAME. Every tag key is the same.
func TestCheckSendsLaunchsRequestWithDryRun(t *testing.T) {
	endpoint, forms := dryRunEC2(t, nil)
	p := New(endpoint, discard)
	const deployment, poolID, pool = "0f3c9a", "pool_0123456789abcdef", "arm64"
	if err := p.Check(context.Background(), json.RawMessage(checkTemplate), server.LaunchTags(deployment, poolID, pool, "host_checkcheckcheck01")); err != nil {
		t.Fatal(err)
	}
	check := forms()[0]
	launchHost := "host_launchlaunchlaun"
	env := map[string]string{"LUX_URL": "https://lux.example", "LUX_HOST_TOKEN": "luxh_secret", "LUX_HOST_NAME": pool + "-" + launchHost[len(launchHost)-8:]}
	if _, err := New(endpoint, discard).Launch(context.Background(), json.RawMessage(checkTemplate),
		server.LaunchTags(deployment, poolID, pool, launchHost), env); err != nil {
		t.Fatal(err)
	}
	launch := forms()[len(forms())-1]
	if check.Get("DryRun") != "true" || launch.Has("DryRun") {
		t.Fatalf("DryRun: check %q, launch %q", check.Get("DryRun"), launch.Get("DryRun"))
	}
	checkTags, launchTags := formTags(check), formTags(launch)
	if !slices.Equal(slices.Sorted(maps.Keys(checkTags)), slices.Sorted(maps.Keys(launchTags))) {
		t.Errorf("check tag keys %v, launch tag keys %v", slices.Sorted(maps.Keys(checkTags)), slices.Sorted(maps.Keys(launchTags)))
	}
	for _, k := range []string{"lux:managed", "lux:deployment", "lux:pool-id", "lux:pool", "team"} {
		if checkTags[k] == "" || checkTags[k] != launchTags[k] {
			t.Errorf("tag %s: check %q, launch %q", k, checkTags[k], launchTags[k])
		}
	}
	if checkTags["lux:host"] != "host_checkcheckcheck01" || checkTags["Name"] != "arm64-kcheck01" {
		t.Errorf("check's host tags: lux:host %q, Name %q", checkTags["lux:host"], checkTags["Name"])
	}
	checkEnv, launchEnv := runnerEnvOf(t, "env", userDataOf(t, check)), runnerEnvOf(t, "env", userDataOf(t, launch))
	for _, k := range []string{"LUX_URL", "LUX_HOST_TOKEN", "LUX_HOST_NAME"} {
		if launchEnv[k] == "" || checkEnv[k] != "" {
			t.Errorf("%s: check %q, launch %q", k, checkEnv[k], launchEnv[k])
		}
		delete(launchEnv, k)
	}
	if !maps.Equal(checkEnv, launchEnv) {
		t.Errorf("check runner env %v, launch's without the host's %v", checkEnv, launchEnv)
	}
	for _, f := range []url.Values{check, launch} {
		f.Del("DryRun")
		// The SDK's idempotency token, new per request.
		f.Del("ClientToken")
		f.Del("UserData")
		for k := range f {
			if strings.HasPrefix(k, "TagSpecification.") {
				f.Del(k)
			}
		}
	}
	if !maps.EqualFunc(check, launch, slices.Equal) {
		t.Errorf("check request\n%v\nlaunch request\n%v", check, launch)
	}
	for _, k := range []string{"LaunchTemplate.LaunchTemplateName", "InstanceType", "SubnetId", "InstanceMarketOptions.MarketType"} {
		if launch.Get(k) == "" {
			t.Errorf("the launch request has no %s: %v", k, launch)
		}
	}
}

// formTags is a RunInstances form's instance tags, key to value.
func formTags(f url.Values) map[string]string {
	out := map[string]string{}
	for i := 1; f.Has(fmt.Sprintf("TagSpecification.1.Tag.%d.Key", i)); i++ {
		out[f.Get(fmt.Sprintf("TagSpecification.1.Tag.%d.Key", i))] = f.Get(fmt.Sprintf("TagSpecification.1.Tag.%d.Value", i))
	}
	return out
}

func userDataOf(t *testing.T, f url.Values) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(f.Get("UserData"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// One request per subnet with instanceType, then one per fallback type in
// the first subnet; without subnets, one per type.
func TestCheckCallsPerSubnetAndFallback(t *testing.T) {
	for _, c := range []struct {
		template string
		want     []string
	}{
		{checkTemplate, []string{"m8g.2xlarge@subnet-a", "m8g.2xlarge@subnet-b", "m8g.2xlarge@subnet-c", "m7g.2xlarge@subnet-a", "c8g.2xlarge@subnet-a"}},
		{`{"launchTemplate": "lt-1", "instanceType": "m8g.2xlarge", "fallbackInstanceTypes": ["m7g.2xlarge"]}`, []string{"m8g.2xlarge@", "m7g.2xlarge@"}},
		{`{"launchTemplate": "lt-1"}`, []string{"@"}},
	} {
		endpoint, forms := dryRunEC2(t, nil)
		if err := New(endpoint, discard).Check(context.Background(), json.RawMessage(c.template), testTags); err != nil {
			t.Fatalf("%s: %v", c.template, err)
		}
		var got []string
		for _, f := range forms() {
			if f.Get("DryRun") != "true" {
				t.Errorf("%s: a request without DryRun: %v", c.template, f)
			}
			got = append(got, f.Get("InstanceType")+"@"+f.Get("SubnetId"))
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: requests %v, want %v", c.template, got, c.want)
		}
	}
}

// Checks from request handlers share the provider with the provisioner:
// cold-cache checks in distinct regions, alongside a Launch and an
// Instances, each building its region's client. Run under the race
// detector (go test -race ./internal/ec2/), which reports unsynchronised
// access to the client cache.
func TestConcurrentChecksShareTheClientCache(t *testing.T) {
	endpoint, _ := dryRunEC2(t, nil)
	p := New(endpoint, discard)
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := range 16 {
		wg.Go(func() {
			<-start
			errs <- p.Check(context.Background(), json.RawMessage(fmt.Sprintf(`{"region": "check-%d", "launchTemplate": "lt-1"}`, i)), testTags)
		})
	}
	wg.Go(func() {
		<-start
		for i := range 4 {
			_, err := p.Launch(context.Background(), json.RawMessage(fmt.Sprintf(`{"region": "launch-%d", "launchTemplate": "lt-1"}`, i)),
				map[string]string{}, map[string]string{})
			errs <- err
		}
	})
	wg.Go(func() {
		<-start
		for i := range 4 {
			// The fake answers only RunInstances: the listing fails, after
			// its client is built.
			_, _ = p.Instances(context.Background(), json.RawMessage(fmt.Sprintf(`{"region": "list-%d"}`, i)), map[string]string{"k": "v"})
		}
	})
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

// DryRunOperation is a pass; any other answer fails the check at its
// first request, EC2's code first.
func TestCheckFailsOnEC2sRefusal(t *testing.T) {
	endpoint, forms := dryRunEC2(t, map[string]string{"lux-runner-arm64": "InvalidLaunchTemplateName.NotFoundException"})
	err := New(endpoint, discard).Check(context.Background(), json.RawMessage(checkTemplate), testTags)
	if err == nil || !strings.HasPrefix(err.Error(), "InvalidLaunchTemplateName.NotFoundException: The specified launch template") ||
		!strings.Contains(err.Error(), "m8g.2xlarge in subnet-a") {
		t.Fatalf("err %v, want EC2's code and message, naming the candidate", err)
	}
	if n := len(forms()); n != 1 {
		t.Errorf("%d requests after a refusal, want 1", n)
	}
	if err := New(endpoint, discard).Check(context.Background(), json.RawMessage(`{"launchTemplate": "lt-ok", "subnets": ["subnet-a"]}`), testTags); err != nil {
		t.Errorf("DryRunOperation: %v, want nil", err)
	}
	if err := New(endpoint, discard).Check(context.Background(), json.RawMessage(`{"region": "eu-north-1"}`), testTags); err == nil {
		t.Error("a template without launchTemplate passed")
	}
}
