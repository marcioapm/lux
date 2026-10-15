package ec2

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/marcioapm/lux/internal/server"
)

type subnetEntry struct {
	subnets map[string][]string
	all     []server.NetworkSubnet
	at      time.Time
}

// CacheNetworkFor sets the VPC subnet refresh interval; zero disables caching.
func (p *Provider) CacheNetworkFor(d time.Duration) {
	p.networkMu.Lock()
	defer p.networkMu.Unlock()
	p.subnetMaxAge = d
}

func (p *Provider) Network(ctx context.Context, template json.RawMessage, providerIDs []string) (map[string]server.HostNetwork, error) {
	t, err := parse(template)
	if err != nil {
		return nil, err
	}
	out := map[string]server.HostNetwork{}
	if len(providerIDs) == 0 {
		return out, nil
	}
	c, err := p.client(ctx, t.Region)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, id := range providerIDs {
		want[id] = true
	}
	pages := awsec2.NewDescribeInstancesPaginator(c, &awsec2.DescribeInstancesInput{
		Filters: []types.Filter{{Name: aws.String("instance-id"), Values: providerIDs}},
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("ec2 DescribeInstances network: %w", err)
		}
		for _, reservation := range page.Reservations {
			for _, inst := range reservation.Instances {
				id := aws.ToString(inst.InstanceId)
				if !want[id] || aws.ToString(inst.VpcId) == "" || aws.ToString(inst.SubnetId) == "" {
					continue
				}
				entry, err := p.vpcSubnets(ctx, c, t.Region, aws.ToString(inst.VpcId))
				if err != nil {
					return nil, err
				}
				cidrs, ok := entry.subnets[aws.ToString(inst.SubnetId)]
				if !ok || len(cidrs) == 0 {
					continue
				}
				n := server.HostNetwork{SubnetCIDRs: slices.Clone(cidrs), VPCSubnets: slices.Clone(entry.all)}
				if inst.Placement != nil {
					n.Zone = aws.ToString(inst.Placement.AvailabilityZone)
				}
				// Count distinct public addresses across all ENIs, including secondary IPs.
				public := map[string]bool{}
				if ip := aws.ToString(inst.PublicIpAddress); ip != "" {
					public[ip] = true
				}
				for _, eni := range inst.NetworkInterfaces {
					if eni.Association != nil {
						if ip := aws.ToString(eni.Association.PublicIp); ip != "" {
							public[ip] = true
						}
					}
					for _, addr := range eni.PrivateIpAddresses {
						if addr.Association != nil {
							if ip := aws.ToString(addr.Association.PublicIp); ip != "" {
								public[ip] = true
							}
						}
					}
				}
				n.PublicIPv4 = len(public)
				out[id] = n
			}
		}
	}
	return out, nil
}

func (p *Provider) vpcSubnets(ctx context.Context, c *awsec2.Client, region, vpc string) (subnetEntry, error) {
	p.networkMu.Lock()
	defer p.networkMu.Unlock()
	key := [2]string{region, vpc}
	if e, ok := p.subnetCache[key]; ok && p.now().Sub(e.at) < p.subnetMaxAge {
		return e, nil
	}
	e := subnetEntry{subnets: map[string][]string{}}
	pages := awsec2.NewDescribeSubnetsPaginator(c, &awsec2.DescribeSubnetsInput{
		Filters: []types.Filter{{Name: aws.String("vpc-id"), Values: []string{vpc}}},
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return subnetEntry{}, fmt.Errorf("ec2 DescribeSubnets: %w", err)
		}
		for _, sn := range page.Subnets {
			cidrs := []string{}
			if cidr := aws.ToString(sn.CidrBlock); cidr != "" {
				cidrs = append(cidrs, cidr)
			}
			for _, a := range sn.Ipv6CidrBlockAssociationSet {
				if a.Ipv6CidrBlockState != nil && a.Ipv6CidrBlockState.State == types.SubnetCidrBlockStateCodeAssociated {
					if cidr := aws.ToString(a.Ipv6CidrBlock); cidr != "" {
						cidrs = append(cidrs, cidr)
					}
				}
			}
			e.subnets[aws.ToString(sn.SubnetId)] = cidrs
			for _, cidr := range cidrs {
				e.all = append(e.all, server.NetworkSubnet{CIDR: cidr, Zone: aws.ToString(sn.AvailabilityZone)})
			}
		}
	}
	e.at = p.now()
	if p.subnetCache == nil {
		p.subnetCache = map[[2]string]subnetEntry{}
	}
	p.subnetCache[key] = e
	return e, nil
}

type ipRange struct {
	IPv4    string `json:"ip_prefix"`
	IPv6    string `json:"ipv6_prefix"`
	Region  string `json:"region"`
	Service string `json:"service"`
}
type ipRanges struct {
	Prefixes     []ipRange `json:"prefixes"`
	IPv6Prefixes []ipRange `json:"ipv6_prefixes"`
}

// Only same-region S3 and DynamoDB traffic is classified as free service traffic.
var freeRegionalServices = map[string]bool{"S3": true, "DYNAMODB": true}

func (p *Provider) Prefixes(ctx context.Context, template json.RawMessage) (server.ProviderPrefixes, error) {
	t, err := parse(template)
	if err != nil {
		return server.ProviderPrefixes{}, err
	}
	p.prefixMu.Lock()
	defer p.prefixMu.Unlock()
	if p.prefixChecked.IsZero() || p.now().Sub(p.prefixChecked) >= 24*time.Hour {
		err = p.fetchPrefixes(ctx)
		if err != nil && !p.prefixGood {
			return server.ProviderPrefixes{}, err
		}
	}
	if !p.prefixGood {
		return server.ProviderPrefixes{}, fmt.Errorf("AWS IP ranges: no validated catalog available")
	}
	out := server.ProviderPrefixes{Source: "ec2-ip-ranges", FetchedAt: p.prefixFetched, Provider: map[string][]string{}}
	for _, r := range append(slices.Clone(p.prefixCatalog.Prefixes), p.prefixCatalog.IPv6Prefixes...) {
		cidr := r.IPv4
		if cidr == "" {
			cidr = r.IPv6
		}
		switch {
		case r.Region == "GLOBAL":
			out.ProviderGlobal = append(out.ProviderGlobal, cidr)
		case r.Region != t.Region:
			out.Provider[r.Region] = append(out.Provider[r.Region], cidr)
		case freeRegionalServices[r.Service]:
			out.RegionService = append(out.RegionService, cidr)
		default:
			out.RegionPublic = append(out.RegionPublic, cidr)
		}
	}
	for r, list := range out.Provider {
		out.Provider[r] = uniquePrefixes(list)
	}
	out.ProviderGlobal = uniquePrefixes(out.ProviderGlobal)
	out.RegionService = uniquePrefixes(out.RegionService)
	// AWS repeats service prefixes under AMAZON; free classification wins.
	out.RegionPublic = slices.DeleteFunc(uniquePrefixes(out.RegionPublic), func(cidr string) bool {
		_, free := slices.BinarySearch(out.RegionService, cidr)
		return free
	})
	return out, nil
}

func uniquePrefixes(in []string) []string {
	slices.Sort(in)
	return slices.Compact(in)
}

func (p *Provider) fetchPrefixes(ctx context.Context) error {
	// Failed refreshes also wait 24h, retaining the last validated catalog.
	p.prefixChecked = p.now()
	url := os.Getenv("LUX_IPRANGES_URL")
	if url == "" {
		url = "https://ip-ranges.amazonaws.com/ip-ranges.json"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if p.prefixETag != "" {
		req.Header.Set("If-None-Match", p.prefixETag)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("AWS IP ranges: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotModified && p.prefixGood {
		return nil
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("AWS IP ranges: HTTP %d", res.StatusCode)
	}
	const maxBytes = 16 << 20
	data, err := io.ReadAll(io.LimitReader(res.Body, maxBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxBytes {
		return fmt.Errorf("AWS IP ranges exceeds %d bytes", maxBytes)
	}
	var catalog ipRanges
	if err := json.Unmarshal(data, &catalog); err != nil {
		return err
	}
	if len(catalog.Prefixes)+len(catalog.IPv6Prefixes) == 0 {
		return fmt.Errorf("AWS IP ranges: empty catalog")
	}
	for _, r := range append(slices.Clone(catalog.Prefixes), catalog.IPv6Prefixes...) {
		cidr := r.IPv4
		if cidr == "" {
			cidr = r.IPv6
		}
		if _, err := netip.ParsePrefix(cidr); err != nil || r.Region == "" || r.Service == "" {
			return fmt.Errorf("AWS IP ranges: invalid entry %q", cidr)
		}
	}
	p.prefixCatalog, p.prefixETag, p.prefixGood = catalog, res.Header.Get("ETag"), true
	p.prefixFetched = p.now()
	return nil
}
