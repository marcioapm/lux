package ec2

import (
	"context"
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

// A check's first request is the request Launch sends for the same
// candidate, but for DryRun.
func TestCheckSendsLaunchsRequestWithDryRun(t *testing.T) {
	endpoint, forms := dryRunEC2(t, nil)
	p := New(endpoint, discard)
	if err := p.Check(context.Background(), json.RawMessage(checkTemplate)); err != nil {
		t.Fatal(err)
	}
	check := forms()[0]
	if _, err := New(endpoint, discard).Launch(context.Background(), json.RawMessage(checkTemplate),
		map[string]string{server.TagManaged: "true"}, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	launch := forms()[len(forms())-1]
	if check.Get("DryRun") != "true" || launch.Has("DryRun") {
		t.Fatalf("DryRun: check %q, launch %q", check.Get("DryRun"), launch.Get("DryRun"))
	}
	check.Del("DryRun")
	// The SDK's idempotency token, new per request.
	check.Del("ClientToken")
	launch.Del("ClientToken")
	if !maps.EqualFunc(check, launch, slices.Equal) {
		t.Errorf("check request\n%v\nlaunch request\n%v", check, launch)
	}
	for _, k := range []string{"LaunchTemplate.LaunchTemplateName", "InstanceType", "SubnetId", "InstanceMarketOptions.MarketType", "TagSpecification.1.Tag.2.Key"} {
		if launch.Get(k) == "" {
			t.Errorf("the launch request has no %s: %v", k, launch)
		}
	}
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
		if err := New(endpoint, discard).Check(context.Background(), json.RawMessage(c.template)); err != nil {
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

// DryRunOperation is a pass; any other answer fails the check at its
// first request, EC2's code first.
func TestCheckFailsOnEC2sRefusal(t *testing.T) {
	endpoint, forms := dryRunEC2(t, map[string]string{"lux-runner-arm64": "InvalidLaunchTemplateName.NotFoundException"})
	err := New(endpoint, discard).Check(context.Background(), json.RawMessage(checkTemplate))
	if err == nil || !strings.HasPrefix(err.Error(), "InvalidLaunchTemplateName.NotFoundException: The specified launch template") ||
		!strings.Contains(err.Error(), "m8g.2xlarge in subnet-a") {
		t.Fatalf("err %v, want EC2's code and message, naming the candidate", err)
	}
	if n := len(forms()); n != 1 {
		t.Errorf("%d requests after a refusal, want 1", n)
	}
	if err := New(endpoint, discard).Check(context.Background(), json.RawMessage(`{"launchTemplate": "lt-ok", "subnets": ["subnet-a"]}`)); err != nil {
		t.Errorf("DryRunOperation: %v, want nil", err)
	}
	if err := New(endpoint, discard).Check(context.Background(), json.RawMessage(`{"region": "eu-north-1"}`)); err == nil {
		t.Error("a template without launchTemplate passed")
	}
}
