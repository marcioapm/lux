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
	"github.com/aws/smithy-go"
)

// Template is a pool's EC2 settings.
type Template struct {
	Region         string            `json:"region"`
	LaunchTemplate string            `json:"launchTemplate"`
	InstanceType   string            `json:"instanceType"`
	Subnets        []string          `json:"subnets"`
	Tags           map[string]string `json:"tags"`
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

// Launch starts one instance and returns its id. tags are set on it
// (with the template's own); env is the runner's environment, passed as
// user data (KEY=value lines).
func (p *Provider) Launch(ctx context.Context, template json.RawMessage, tags, env map[string]string) (string, error) {
	t, err := parse(template)
	if err != nil {
		return "", err
	}
	if t.LaunchTemplate == "" {
		return "", errors.New("ec2: the pool template needs a launchTemplate")
	}
	c, err := p.client(ctx, t.Region)
	if err != nil {
		return "", err
	}
	var ud strings.Builder
	for k, v := range env {
		fmt.Fprintf(&ud, "%s=%s\n", k, v)
	}
	lt := &types.LaunchTemplateSpecification{Version: aws.String("$Default")}
	if strings.HasPrefix(t.LaunchTemplate, "lt-") {
		lt.LaunchTemplateId = aws.String(t.LaunchTemplate)
	} else {
		lt.LaunchTemplateName = aws.String(t.LaunchTemplate)
	}
	var instTags []types.Tag
	for k, v := range t.Tags {
		instTags = append(instTags, types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	for k, v := range tags {
		instTags = append(instTags, types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	in := &awsec2.RunInstancesInput{
		MinCount:       aws.Int32(1),
		MaxCount:       aws.Int32(1),
		LaunchTemplate: lt,
		UserData:       aws.String(base64.StdEncoding.EncodeToString([]byte(ud.String()))),
		TagSpecifications: []types.TagSpecification{
			{ResourceType: types.ResourceTypeInstance, Tags: instTags},
		},
	}
	if t.InstanceType != "" {
		in.InstanceType = types.InstanceType(t.InstanceType)
	}
	if len(t.Subnets) > 0 {
		i := p.next[t.LaunchTemplate] % len(t.Subnets)
		p.next[t.LaunchTemplate]++
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

// Terminate ends an instance. One that no longer exists is done.
func (p *Provider) Terminate(ctx context.Context, template json.RawMessage, id string) error {
	t, err := parse(template)
	if err != nil {
		return err
	}
	c, err := p.client(ctx, t.Region)
	if err != nil {
		return err
	}
	_, err = c.TerminateInstances(ctx, &awsec2.TerminateInstancesInput{InstanceIds: []string{id}})
	if isNotFound(err) {
		return nil
	}
	return err
}

// Instances lists the instances carrying all the given tags, with their
// state (pending, running, shutting-down, terminated, …). A filtered call:
// ids EC2 does not know do not fail it.
func (p *Provider) Instances(ctx context.Context, template json.RawMessage, tags map[string]string) (map[string]string, error) {
	t, err := parse(template)
	if err != nil {
		return nil, err
	}
	c, err := p.client(ctx, t.Region)
	if err != nil {
		return nil, err
	}
	var filters []types.Filter
	for k, v := range tags {
		filters = append(filters, types.Filter{Name: aws.String("tag:" + k), Values: []string{v}})
	}
	states, err := describe(ctx, c, filters)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(states))
	for id, st := range states {
		out[id] = string(st)
	}
	return out, nil
}

func describe(ctx context.Context, c *awsec2.Client, filters []types.Filter) (map[string]types.InstanceStateName, error) {
	out := map[string]types.InstanceStateName{}
	pages := awsec2.NewDescribeInstancesPaginator(c, &awsec2.DescribeInstancesInput{Filters: filters})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range page.Reservations {
			for _, i := range r.Instances {
				if i.InstanceId == nil || i.State == nil {
					continue
				}
				out[*i.InstanceId] = i.State.Name
			}
		}
	}
	return out, nil
}

func isNotFound(err error) bool {
	var ae smithy.APIError
	return errors.As(err, &ae) && ae.ErrorCode() == "InvalidInstanceID.NotFound"
}

func parse(raw json.RawMessage) (Template, error) {
	var t Template
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &t); err != nil {
			return t, fmt.Errorf("ec2 template: %w", err)
		}
	}
	return t, nil
}
