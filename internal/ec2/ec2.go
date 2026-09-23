// Package ec2 provisions lux hosts as EC2 instances.
//
// A pool's template names what to launch:
//
//	{"region": "eu-west-1", "launchTemplate": "lt-0abc…" (id or name),
//	 "instanceType": "m7i.2xlarge", "subnets": ["subnet-…", …],
//	 "tags": {"team": "platform"}}
//
// The instance's AMI (from the launch template) has Podman and lux-runner
// installed. Its user data carries the runner's environment: luxd's URL, a
// host token for the pool, and the instance's name; a boot script (in the
// AMI) starts `lux-runner` with them. The runner registers with its
// instance id as provider id, which is how luxd matches it to the host row
// it made when it launched the instance.
//
// Credentials and region come from the standard AWS configuration of the
// luxd process (environment, instance role). LUX_EC2_ENDPOINT points the
// client elsewhere: the test suite's fake EC2.
package ec2

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// Template is a pool's EC2 settings.
type Template struct {
	Region         string            `json:"region"`
	LaunchTemplate string            `json:"launchTemplate"`
	InstanceType   string            `json:"instanceType"`
	Subnets        []string          `json:"subnets"`
	Tags           map[string]string `json:"tags"`
}

// Launch is one instance to start.
type Launch struct {
	Pool     string
	HostName string
	Template Template
	// UserData: the runner's environment (KEY=value lines).
	UserData map[string]string
}

// Instance is what DescribeInstances tells about one of ours.
type Instance struct {
	ID    string
	State string // pending | running | shutting-down | terminated | stopping | stopped
}

type Provider struct {
	endpoint string
	clients  map[string]*awsec2.Client
	// next subnet per pool: launches spread across the pool's subnets.
	next map[string]int
}

// New builds the provider. endpoint overrides the EC2 endpoint (tests).
func New(endpoint string) *Provider {
	return &Provider{endpoint: endpoint, clients: map[string]*awsec2.Client{}, next: map[string]int{}}
}

func (p *Provider) Name() string { return "ec2" }

func (p *Provider) client(ctx context.Context, region string) (*awsec2.Client, error) {
	if c := p.clients[region]; c != nil {
		return c, nil
	}
	var opts []func(*config.LoadOptions) error
	if region != "" {
		opts = append(opts, config.WithRegion(region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}
	c := awsec2.NewFromConfig(cfg, func(o *awsec2.Options) {
		if p.endpoint != "" {
			o.BaseEndpoint = aws.String(p.endpoint)
		}
	})
	p.clients[region] = c
	return c, nil
}

// Launch starts one instance and returns its id.
func (p *Provider) Launch(ctx context.Context, l Launch) (string, error) {
	t := l.Template
	if t.LaunchTemplate == "" {
		return "", errors.New("ec2: the pool template needs a launchTemplate")
	}
	c, err := p.client(ctx, t.Region)
	if err != nil {
		return "", err
	}
	var ud strings.Builder
	for k, v := range l.UserData {
		fmt.Fprintf(&ud, "%s=%s\n", k, v)
	}
	lt := &types.LaunchTemplateSpecification{Version: aws.String("$Default")}
	if strings.HasPrefix(t.LaunchTemplate, "lt-") {
		lt.LaunchTemplateId = aws.String(t.LaunchTemplate)
	} else {
		lt.LaunchTemplateName = aws.String(t.LaunchTemplate)
	}
	tags := []types.Tag{
		{Key: aws.String("Name"), Value: aws.String(l.HostName)},
		{Key: aws.String("lux:pool"), Value: aws.String(l.Pool)},
		{Key: aws.String("lux:managed"), Value: aws.String("true")},
	}
	for k, v := range t.Tags {
		tags = append(tags, types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	in := &awsec2.RunInstancesInput{
		MinCount:       aws.Int32(1),
		MaxCount:       aws.Int32(1),
		LaunchTemplate: lt,
		UserData:       aws.String(base64.StdEncoding.EncodeToString([]byte(ud.String()))),
		TagSpecifications: []types.TagSpecification{
			{ResourceType: types.ResourceTypeInstance, Tags: tags},
		},
	}
	if t.InstanceType != "" {
		in.InstanceType = types.InstanceType(t.InstanceType)
	}
	if len(t.Subnets) > 0 {
		i := p.next[l.Pool] % len(t.Subnets)
		p.next[l.Pool]++
		in.SubnetId = aws.String(t.Subnets[i])
	}
	out, err := c.RunInstances(ctx, in)
	if err != nil {
		return "", fmt.Errorf("ec2 RunInstances: %w", err)
	}
	if len(out.Instances) != 1 || out.Instances[0].InstanceId == nil {
		return "", errors.New("ec2 RunInstances: no instance in the reply")
	}
	return *out.Instances[0].InstanceId, nil
}

// Terminate ends an instance. An instance that no longer exists is done.
func (p *Provider) Terminate(ctx context.Context, region, id string) error {
	c, err := p.client(ctx, region)
	if err != nil {
		return err
	}
	_, err = c.TerminateInstances(ctx, &awsec2.TerminateInstancesInput{InstanceIds: []string{id}})
	if err != nil && strings.Contains(err.Error(), "InvalidInstanceID.NotFound") {
		return nil
	}
	return err
}

// Describe reports the state of the given instances. Ones EC2 no longer
// knows are absent from the result.
func (p *Provider) Describe(ctx context.Context, region string, ids []string) (map[string]Instance, error) {
	out := map[string]Instance{}
	if len(ids) == 0 {
		return out, nil
	}
	c, err := p.client(ctx, region)
	if err != nil {
		return nil, err
	}
	pages := awsec2.NewDescribeInstancesPaginator(c, &awsec2.DescribeInstancesInput{InstanceIds: ids})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			if strings.Contains(err.Error(), "InvalidInstanceID.NotFound") {
				return out, nil
			}
			return nil, err
		}
		for _, r := range page.Reservations {
			for _, i := range r.Instances {
				if i.InstanceId == nil {
					continue
				}
				st := ""
				if i.State != nil {
					st = string(i.State.Name)
				}
				out[*i.InstanceId] = Instance{ID: *i.InstanceId, State: st}
			}
		}
	}
	return out, nil
}

// Pool adapts the provider to a pool's JSON template, as luxd calls it.
type Pool struct{ *Provider }

func parse(raw json.RawMessage) (Template, error) {
	var t Template
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &t); err != nil {
			return t, fmt.Errorf("ec2 template: %w", err)
		}
	}
	return t, nil
}

func (p Pool) Launch(ctx context.Context, pool, hostName string, template json.RawMessage, env map[string]string) (string, error) {
	t, err := parse(template)
	if err != nil {
		return "", err
	}
	return p.Provider.Launch(ctx, Launch{Pool: pool, HostName: hostName, Template: t, UserData: env})
}

func (p Pool) Terminate(ctx context.Context, template json.RawMessage, id string) error {
	t, err := parse(template)
	if err != nil {
		return err
	}
	return p.Provider.Terminate(ctx, t.Region, id)
}

func (p Pool) Alive(ctx context.Context, template json.RawMessage, ids []string) (map[string]bool, error) {
	t, err := parse(template)
	if err != nil {
		return nil, err
	}
	insts, err := p.Provider.Describe(ctx, t.Region, ids)
	if err != nil {
		return nil, err
	}
	alive := map[string]bool{}
	for id, i := range insts {
		alive[id] = i.State != "terminated" && i.State != "shutting-down"
	}
	return alive, nil
}
