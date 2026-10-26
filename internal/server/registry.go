package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// RegistryConfig is [registry]: the operator's in-region ECR registry, from
// which luxd mints pull credentials for Runs whose tenant has a grant.
// Host "" is off.
type RegistryConfig struct {
	Host        string
	Region      string
	PullRoleARN string
	Grants      []RegistryGrant
	// AWS replaces the SDK (tests); nil is the default AWS chain.
	AWS RegistryAWS
}

// RegistryGrant lets Tenants (ids, or "*" for every tenant) pull Owner's
// repositories (<host>/<owner>/...).
type RegistryGrant struct {
	Owner   string
	Tenants []string
}

// RegistryAWS is the two calls minting takes: STS AssumeRole with luxd's
// own credentials, then ECR GetAuthorizationToken with the assumed ones.
type RegistryAWS interface {
	AssumeRole(ctx context.Context, in *sts.AssumeRoleInput) (*ststypes.Credentials, error)
	AuthorizationToken(ctx context.Context, creds *ststypes.Credentials) (*ecrtypes.AuthorizationData, error)
}

const (
	// registryOwnerTag is the session tag the pull role's policy scopes
	// repositories by (repository/${aws:PrincipalTag/lux-owner}/*).
	registryOwnerTag = "lux-owner"
	registrySession  = time.Hour
	// registryReuse: a cached token is handed out while it has at least
	// this long left, which covers a slow pull started just before expiry.
	registryReuse = 30 * time.Minute
	// registryRetry: after a failed mint, an owner is not tried again for
	// this long, so every delivery pass does not wait on a failing STS.
	registryRetry       = 30 * time.Second
	registryMintTimeout = 10 * time.Second
)

// registryMinter holds one pull token per owner, in memory only.
type registryMinter struct {
	cfg RegistryConfig
	now func() time.Time
	mu  sync.Mutex
	// owners: a lock per owner, so one owner's mint does not wait on
	// another's, and concurrent deliveries share one mint.
	owners map[string]*ownerToken
}

type ownerToken struct {
	mu         sync.Mutex
	user, pass string
	expires    time.Time
	failedAt   time.Time
}

func newRegistryMinter(cfg RegistryConfig, now func() time.Time) *registryMinter {
	if cfg.Host == "" {
		return nil
	}
	if cfg.AWS == nil {
		cfg.AWS = &awsRegistry{region: cfg.Region}
	}
	return &registryMinter{cfg: cfg, now: now, owners: map[string]*ownerToken{}}
}

// ownersFor are the owners tenantID may pull, in configuration order.
func (m *registryMinter) ownersFor(tenantID string) []string {
	if m == nil {
		return nil
	}
	var out []string
	for _, g := range m.cfg.Grants {
		if slices.Contains(g.Tenants, "*") || slices.Contains(g.Tenants, tenantID) {
			out = append(out, g.Owner)
		}
	}
	return out
}

// credentials are the auth.json entries for tenantID's granted owners. An
// owner whose token cannot be minted is left out (and its failure logged
// by the caller from errs): the pull then fails as it would without it.
func (m *registryMinter) credentials(ctx context.Context, tenantID string) (creds []proto.RegistryCredential, errs map[string]error) {
	for _, owner := range m.ownersFor(tenantID) {
		user, pass, err := m.token(ctx, owner)
		if err != nil {
			if errs == nil {
				errs = map[string]error{}
			}
			errs[owner] = err
			continue
		}
		creds = append(creds, proto.RegistryCredential{Registry: m.cfg.Host + "/" + owner, User: user, Password: pass})
	}
	return creds, errs
}

var errRegistryBackoff = errors.New("an earlier attempt failed; retrying shortly")

func (m *registryMinter) token(ctx context.Context, owner string) (user, pass string, err error) {
	m.mu.Lock()
	t := m.owners[owner]
	if t == nil {
		t = &ownerToken{}
		m.owners[owner] = t
	}
	m.mu.Unlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	now := m.now()
	if t.pass != "" && t.expires.Sub(now) >= registryReuse {
		return t.user, t.pass, nil
	}
	if now.Sub(t.failedAt) < registryRetry {
		return "", "", errRegistryBackoff
	}
	ctx, cancel := context.WithTimeout(ctx, registryMintTimeout)
	defer cancel()
	user, pass, expires, err := m.mint(ctx, owner)
	if err != nil {
		t.failedAt = now
		return "", "", err
	}
	t.user, t.pass, t.expires, t.failedAt = user, pass, expires, time.Time{}
	return user, pass, nil
}

func (m *registryMinter) mint(ctx context.Context, owner string) (user, pass string, expires time.Time, err error) {
	creds, err := m.cfg.AWS.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn:         aws.String(m.cfg.PullRoleARN),
		RoleSessionName: aws.String("luxd-registry-" + owner),
		DurationSeconds: aws.Int32(int32(registrySession / time.Second)),
		Tags:            []ststypes.Tag{{Key: aws.String(registryOwnerTag), Value: aws.String(owner)}},
	})
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("sts:AssumeRole: %w", err)
	}
	if creds == nil {
		return "", "", time.Time{}, errors.New("sts:AssumeRole: no credentials")
	}
	d, err := m.cfg.AWS.AuthorizationToken(ctx, creds)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("ecr:GetAuthorizationToken: %w", err)
	}
	if d == nil || d.AuthorizationToken == nil || d.ExpiresAt == nil {
		return "", "", time.Time{}, errors.New("ecr:GetAuthorizationToken: no token")
	}
	raw, err := base64.StdEncoding.DecodeString(*d.AuthorizationToken)
	if err != nil {
		return "", "", time.Time{}, errors.New("ecr:GetAuthorizationToken: token is not base64")
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok || user == "" || pass == "" {
		return "", "", time.Time{}, errors.New("ecr:GetAuthorizationToken: token is not user:password")
	}
	// A token is presumed to last no longer than the session that minted it.
	expires = *d.ExpiresAt
	if creds.Expiration != nil && creds.Expiration.Before(expires) {
		expires = *creds.Expiration
	}
	return user, pass, expires, nil
}

// attachRegistryCredentials adds to an assignment the pull credentials of
// its tenant's granted owners: at send time, never stored, and only for a
// runner with proto.CapRegistryMinted.
func (s *Server) attachRegistryCredentials(ctx context.Context, hostID string, capable bool, data json.RawMessage) json.RawMessage {
	if s.registry == nil || !capable {
		return data
	}
	var a proto.Assign
	if err := json.Unmarshal(data, &a); err != nil || len(s.registry.ownersFor(a.TenantID)) == 0 {
		return data
	}
	creds, errs := s.registry.credentials(ctx, a.TenantID)
	for owner, err := range errs {
		s.log.Warn("registry pull credentials not minted; the assignment goes without them",
			"host", hostID, "run", a.RunID, "epoch", a.Epoch, "tenant", a.TenantID, "owner", owner, "err", err)
	}
	if len(creds) == 0 {
		return data
	}
	a.RegistryCredentials = creds
	return proto.Marshal(a)
}

// awsRegistry is RegistryAWS with the SDK and luxd's default credential
// chain, loaded on first use.
type awsRegistry struct {
	region string
	once   sync.Once
	cfg    aws.Config
	err    error
}

func (r *awsRegistry) load(ctx context.Context) (aws.Config, error) {
	r.once.Do(func() { r.cfg, r.err = config.LoadDefaultConfig(ctx, config.WithRegion(r.region)) })
	return r.cfg, r.err
}

func (r *awsRegistry) AssumeRole(ctx context.Context, in *sts.AssumeRoleInput) (*ststypes.Credentials, error) {
	cfg, err := r.load(ctx)
	if err != nil {
		return nil, err
	}
	out, err := sts.NewFromConfig(cfg).AssumeRole(ctx, in)
	if err != nil {
		return nil, err
	}
	return out.Credentials, nil
}

func (r *awsRegistry) AuthorizationToken(ctx context.Context, c *ststypes.Credentials) (*ecrtypes.AuthorizationData, error) {
	cfg, err := r.load(ctx)
	if err != nil {
		return nil, err
	}
	out, err := ecr.NewFromConfig(cfg, func(o *ecr.Options) {
		o.Credentials = credentials.NewStaticCredentialsProvider(aws.ToString(c.AccessKeyId), aws.ToString(c.SecretAccessKey), aws.ToString(c.SessionToken))
	}).GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return nil, err
	}
	if len(out.AuthorizationData) == 0 {
		return nil, errors.New("no authorization data")
	}
	return &out.AuthorizationData[0], nil
}

// hostHas: whether hostID's latest registration lists capability.
func (s *Server) hostHas(ctx context.Context, hostID, capability string) (bool, error) {
	var has bool
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM hosts WHERE id = $1 AND $2 = ANY(capabilities))`, hostID, capability).Scan(&has)
	})
	return has, err
}
