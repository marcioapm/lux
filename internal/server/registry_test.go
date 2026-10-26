package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
	"github.com/coder/websocket"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

const testRegistryHost = "123456789012.dkr.ecr.eu-west-1.amazonaws.com"

// fakeRegistryAWS mints a distinct token per call: <owner>-pw-<n>, with
// the owner read from the session tag, so a test can tell a reused token
// from a fresh one and one owner's from another's.
type fakeRegistryAWS struct {
	mu       sync.Mutex
	now      func() time.Time
	assumed  []sts.AssumeRoleInput
	tokens   int
	fail     map[string]error // by owner: AssumeRole fails
	tokenTTL time.Duration    // 0: 12h
	// token, by owner, replaces GetAuthorizationToken's answer.
	token map[string]func() (*ecrtypes.AuthorizationData, error)
	// gate, by owner: AssumeRole waits for it to close (or its context).
	gate map[string]chan struct{}
	// entered, if set, gets the owner of every AssumeRole as it starts.
	entered chan string
}

func (f *fakeRegistryAWS) AssumeRole(ctx context.Context, in *sts.AssumeRoleInput) (*ststypes.Credentials, error) {
	owner := ""
	for _, tag := range in.Tags {
		if aws.ToString(tag.Key) == "lux-owner" {
			owner = aws.ToString(tag.Value)
		}
	}
	f.mu.Lock()
	f.assumed = append(f.assumed, *in)
	gate, entered := f.gate[owner], f.entered
	f.mu.Unlock()
	if entered != nil {
		entered <- owner
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail[owner]; err != nil {
		return nil, err
	}
	exp := f.now().Add(time.Duration(aws.ToInt32(in.DurationSeconds)) * time.Second)
	return &ststypes.Credentials{AccessKeyId: aws.String("ASIA" + owner), SecretAccessKey: aws.String("sk"), SessionToken: aws.String(owner), Expiration: &exp}, nil
}

func (f *fakeRegistryAWS) AuthorizationToken(_ context.Context, c *ststypes.Credentials) (*ecrtypes.AuthorizationData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens++
	if tok := f.token[aws.ToString(c.SessionToken)]; tok != nil {
		return tok()
	}
	ttl := f.tokenTTL
	if ttl == 0 {
		ttl = 12 * time.Hour
	}
	exp := f.now().Add(ttl)
	tok := base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "AWS:%s-pw-%d", aws.ToString(c.SessionToken), f.tokens))
	return &ecrtypes.AuthorizationData{AuthorizationToken: &tok, ExpiresAt: &exp}, nil
}

func (f *fakeRegistryAWS) calls() []sts.AssumeRoleInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.assumed)
}

func (f *fakeRegistryAWS) tokenCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokens
}

func (f *fakeRegistryAWS) block(owner string) chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gate == nil {
		f.gate = map[string]chan struct{}{}
	}
	ch := make(chan struct{})
	f.gate[owner] = ch
	return ch
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestMinter(now func() time.Time, grants ...RegistryGrant) *registryMinter {
	fake := &fakeRegistryAWS{now: now}
	return newRegistryMinter(RegistryConfig{Host: testRegistryHost, Region: "eu-west-1",
		PullRoleARN: "arn:aws:iam::123456789012:role/lux-registry-pull", Grants: grants, AWS: fake}, now)
}

func fakeOf(m *registryMinter) *fakeRegistryAWS { return m.cfg.AWS.(*fakeRegistryAWS) }

// minted signals the end of each mint a lookup waits on (m.ready).
type minted chan struct{}

func (c minted) signal(string) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// settle looks up tenant's owners for message id until none is pending.
func settle(t *testing.T, m *registryMinter, done minted, id int64, tenant string) ([]proto.RegistryCredential, map[string]error) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		creds, pending, errs := m.lookup(context.Background(), "h1", id, m.ownersFor(tenant))
		if !pending {
			return creds, errs
		}
		select {
		case <-done:
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatal("still minting after 5s")
		}
	}
}

// minter is a minter on a fake clock with a fake AWS.
func minter(t *testing.T, grants ...RegistryGrant) (*registryMinter, *fakeRegistryAWS, *testClock, minted) {
	t.Helper()
	clock := &testClock{t: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}
	m := newTestMinter(clock.now, grants...)
	done := make(minted, 1)
	m.ready = done.signal
	return m, fakeOf(m), clock, done
}

func TestRegistryGrants(t *testing.T) {
	m, _, _, done := minter(t,
		RegistryGrant{Owner: "acme", Tenants: []string{"*"}},
		RegistryGrant{Owner: "team", Tenants: []string{"t1", "t3"}},
		RegistryGrant{Owner: "other", Tenants: []string{"t3"}})
	for tenant, want := range map[string][]string{
		"t1": {"acme", "team"},
		"t2": {"acme"},
		"t3": {"acme", "team", "other"},
	} {
		if got := m.ownersFor(tenant); !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", tenant, got, want)
		}
	}
	none, fake, _, _ := minter(t, RegistryGrant{Owner: "team", Tenants: []string{"t1"}})
	if creds, errs := settle(t, none, done, 1, "t2"); creds != nil || errs != nil || len(fake.calls()) != 0 {
		t.Fatalf("ungranted tenant: %v %v, %d AssumeRole calls", creds, errs, len(fake.calls()))
	}
	// Off: no minter, nothing granted.
	if off := newRegistryMinter(RegistryConfig{}, time.Now); off != nil || off.ownersFor("t1") != nil {
		t.Fatal("an empty [registry] is not off")
	}
}

// One AssumeRole per owner, tagged and named for it. Its token is handed
// out while 30 minutes or more remain; under 40, a new one is minted in the
// background while the old one is still handed out.
func TestRegistryMintPerOwnerAndCache(t *testing.T) {
	m, fake, clock, done := minter(t, RegistryGrant{Owner: "acme", Tenants: []string{"*"}}, RegistryGrant{Owner: "team.b", Tenants: []string{"t1"}})
	creds, errs := settle(t, m, done, 1, "t1")
	if errs != nil {
		t.Fatal(errs)
	}
	acme1 := proto.RegistryCredential{Registry: testRegistryHost + "/acme", User: "AWS", Password: "acme-pw-1"}
	team1 := proto.RegistryCredential{Registry: testRegistryHost + "/team.b", User: "AWS", Password: "team.b-pw-2"}
	slices.SortFunc(creds, func(a, b proto.RegistryCredential) int { return strings.Compare(a.Registry, b.Registry) })
	// Minted in parallel: either may take the first ECR call.
	if len(creds) == 2 && creds[0].Password == "acme-pw-2" {
		acme1.Password, team1.Password = "acme-pw-2", "team.b-pw-1"
	}
	if want := []proto.RegistryCredential{acme1, team1}; !slices.Equal(creds, want) {
		t.Fatalf("credentials %+v, want %+v", creds, want)
	}
	calls := fake.calls()
	if len(calls) != 2 {
		t.Fatalf("%d AssumeRole calls, want 2", len(calls))
	}
	slices.SortFunc(calls, func(a, b sts.AssumeRoleInput) int {
		return strings.Compare(aws.ToString(a.RoleSessionName), aws.ToString(b.RoleSessionName))
	})
	for i, owner := range []string{"acme", "team.b"} {
		c := calls[i]
		if aws.ToString(c.RoleArn) != "arn:aws:iam::123456789012:role/lux-registry-pull" ||
			aws.ToString(c.RoleSessionName) != "luxd-registry-"+owner || aws.ToInt32(c.DurationSeconds) != 3600 ||
			len(c.Tags) != 1 || aws.ToString(c.Tags[0].Key) != "lux-owner" || aws.ToString(c.Tags[0].Value) != owner {
			t.Errorf("AssumeRole for %s: role %s session %s duration %d tags %+v", owner, aws.ToString(c.RoleArn),
				aws.ToString(c.RoleSessionName), aws.ToInt32(c.DurationSeconds), c.Tags)
		}
	}

	// The session lasts an hour, so the token is taken to. At 15m: the
	// cached token, nothing minted.
	clock.advance(15 * time.Minute)
	if again, _, _ := m.lookup(context.Background(), "h1", 2, m.ownersFor("t2")); len(again) != 1 || again[0] != acme1 || len(fake.calls()) != 2 {
		t.Fatalf("at 15m: %+v, %d calls; want the cached token", again, len(fake.calls()))
	}
	// At 25m (35m left): still the cached token, at once, and acme's next
	// one minted behind it.
	clock.advance(10 * time.Minute)
	again, pending, _ := m.lookup(context.Background(), "h1", 3, m.ownersFor("t2"))
	if pending || len(again) != 1 || again[0] != acme1 {
		t.Fatalf("at 25m: %+v pending=%v; want the cached token without waiting", again, pending)
	}
	waitUntil(t, func() bool { return fake.tokenCalls() == 3 }, "a refresh ahead of the cutoff")
	waitUntil(t, func() bool {
		next, _, _ := m.lookup(context.Background(), "h1", 4, m.ownersFor("t2"))
		return len(next) == 1 && next[0].Password == "acme-pw-3"
	}, "the refreshed token")
	if len(fake.calls()) != 3 {
		t.Fatalf("%d AssumeRole calls, want 3 (acme refreshed once)", len(fake.calls()))
	}
}

// An ECR token that expires before the session bounds its reuse.
func TestRegistryECRExpiryBoundsReuse(t *testing.T) {
	m, fake, clock, done := minter(t, RegistryGrant{Owner: "acme", Tenants: []string{"*"}})
	fake.tokenTTL = 45 * time.Minute
	first, errs := settle(t, m, done, 1, "t1")
	if len(errs) != 0 || len(first) != 1 {
		t.Fatalf("first mint: %v %v", first, errs)
	}
	// 29m left by ECR's expiry (44m by the session's): below the cutoff.
	clock.advance(16 * time.Minute)
	next, errs := settle(t, m, done, 2, "t1")
	if len(errs) != 0 || len(next) != 1 || next[0].Password == first[0].Password {
		t.Fatalf("ECR-bound refresh: %v %v", next, errs)
	}
	if len(fake.calls()) != 2 {
		t.Fatalf("got %d AssumeRole calls, want 2", len(fake.calls()))
	}
}

// Concurrent deliveries needing one cold owner share one mint.
func TestRegistryConcurrentRequestsShareOneMint(t *testing.T) {
	m, fake, _, done := minter(t, RegistryGrant{Owner: "acme", Tenants: []string{"*"}})
	fake.entered = make(chan string, 16)
	release := fake.block("acme")
	const n = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			if _, pending, errs := m.lookup(context.Background(), fmt.Sprintf("h%d", i), int64(i+1), []string{"acme"}); !pending || errs != nil {
				t.Errorf("request %d: pending=%v errs=%v while acme mints", i, pending, errs)
			}
		})
	}
	close(start)
	wg.Wait()
	select {
	case <-fake.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no mint started")
	}
	close(release)
	var got []proto.RegistryCredential
	for i := range n {
		creds, errs := settle(t, m, done, int64(i+1), "t1")
		if len(creds) != 1 || errs != nil {
			t.Fatalf("request %d: %v %v", i, creds, errs)
		}
		got = append(got, creds[0])
	}
	for _, c := range got {
		if c != got[0] {
			t.Fatalf("different tokens for one owner: %+v", got)
		}
	}
	if len(fake.calls()) != 1 || fake.tokenCalls() != 1 {
		t.Fatalf("%d AssumeRole and %d GetAuthorizationToken calls, want 1 each", len(fake.calls()), fake.tokenCalls())
	}
}

// One owner's slow mint does not hold another owner's.
func TestRegistryOwnersMintIndependently(t *testing.T) {
	m, fake, _, done := minter(t, RegistryGrant{Owner: "acme", Tenants: []string{"ta"}}, RegistryGrant{Owner: "team", Tenants: []string{"tb"}})
	fake.entered = make(chan string, 4)
	release := fake.block("acme")
	defer close(release)
	if _, pending, _ := m.lookup(context.Background(), "h1", 1, m.ownersFor("ta")); !pending {
		t.Fatal("acme is not minting")
	}
	if owner := <-fake.entered; owner != "acme" {
		t.Fatalf("entered %s", owner)
	}
	// acme's AssumeRole is still blocked.
	creds, errs := settle(t, m, done, 2, "tb")
	if errs != nil || len(creds) != 1 || creds[0].Registry != testRegistryHost+"/team" || !strings.HasPrefix(creds[0].Password, "team-pw-") {
		t.Fatalf("tb while acme mints: %+v %v", creds, errs)
	}
}

// fails mints acme with fake configured by setup, and requires: no
// credential, an error naming the step without sentinel, backoff for
// registryRetry (no AWS call), then a fresh attempt.
func requireMintFails(t *testing.T, setup func(*fakeRegistryAWS), step, sentinel string) {
	t.Helper()
	m, fake, clock, done := minter(t, RegistryGrant{Owner: "acme", Tenants: []string{"*"}}, RegistryGrant{Owner: "good", Tenants: []string{"*"}})
	setup(fake)
	creds, errs := settle(t, m, done, 1, "t1")
	if len(creds) != 1 || creds[0].Registry != testRegistryHost+"/good" {
		t.Fatalf("credentials %+v, want good's only", creds)
	}
	err := errs["acme"]
	if err == nil || !strings.Contains(err.Error(), step) || (sentinel != "" && strings.Contains(err.Error(), sentinel)) {
		t.Fatalf("error %v: want %q, never %q", err, step, sentinel)
	}
	calls := len(fake.calls())
	if _, errs = settle(t, m, done, 2, "t1"); !errors.Is(errs["acme"], errRegistryBackoff) || len(fake.calls()) != calls {
		t.Fatalf("retried at once: %v, %d calls", errs, len(fake.calls()))
	}
	if sentinel != "" && strings.Contains(errs["acme"].Error(), sentinel) {
		t.Fatalf("backoff error quotes the token: %v", errs["acme"])
	}
	clock.advance(registryRetry)
	settle(t, m, done, 3, "t1")
	if len(fake.calls()) != calls+1 {
		t.Fatalf("%d AssumeRole calls after the pause, want %d", len(fake.calls()), calls+1)
	}
}

func TestRegistrySTSFailure(t *testing.T) {
	requireMintFails(t, func(f *fakeRegistryAWS) {
		f.fail = map[string]error{"acme": errors.New("AccessDenied: not authorized to perform sts:TagSession")}
	}, "sts:AssumeRole: AccessDenied", "")
}

func TestRegistryECRFailure(t *testing.T) {
	requireMintFails(t, func(f *fakeRegistryAWS) {
		f.token = map[string]func() (*ecrtypes.AuthorizationData, error){"acme": func() (*ecrtypes.AuthorizationData, error) {
			return nil, errors.New("AccessDeniedException: ecr:GetAuthorizationToken")
		}}
	}, "ecr:GetAuthorizationToken: AccessDeniedException", "")
}

// A response without a token or its expiry is no token.
func TestRegistryECREmptyToken(t *testing.T) {
	future := time.Now().Add(time.Hour)
	tok := base64.StdEncoding.EncodeToString([]byte("AWS:SENTINEL-empty"))
	for name, d := range map[string]*ecrtypes.AuthorizationData{
		"nil":        nil,
		"no token":   {ExpiresAt: &future},
		"no expiry":  {AuthorizationToken: &tok},
		"empty data": {},
	} {
		t.Run(name, func(t *testing.T) {
			requireMintFails(t, func(f *fakeRegistryAWS) {
				f.token = map[string]func() (*ecrtypes.AuthorizationData, error){"acme": func() (*ecrtypes.AuthorizationData, error) { return d, nil }}
			}, "ecr:GetAuthorizationToken: no token", "SENTINEL")
		})
	}
}

// A token that does not decode to AWS:<password> is refused without
// quoting it.
func TestRegistryECRMalformedToken(t *testing.T) {
	for name, raw := range map[string]string{
		"not base64":  "SENTINEL!!not*base64",
		"no colon":    base64.StdEncoding.EncodeToString([]byte("SENTINEL-no-colon")),
		"not AWS":     base64.StdEncoding.EncodeToString([]byte("root:SENTINEL-pw")),
		"no password": base64.StdEncoding.EncodeToString([]byte("AWS:")),
	} {
		step := "token is not AWS:password"
		if name == "not base64" {
			step = "token is not base64"
		}
		t.Run(name, func(t *testing.T) {
			exp := time.Now().Add(time.Hour)
			requireMintFails(t, func(f *fakeRegistryAWS) {
				f.token = map[string]func() (*ecrtypes.AuthorizationData, error){"acme": func() (*ecrtypes.AuthorizationData, error) {
					return &ecrtypes.AuthorizationData{AuthorizationToken: &raw, ExpiresAt: &exp}, nil
				}}
			}, step, "SENTINEL")
		})
	}
}

// A caller that goes away mid-mint neither aborts the mint nor puts the
// owner in backoff for the next caller.
func TestRegistryCancelledCallerDoesNotArmBackoff(t *testing.T) {
	m, fake, _, done := minter(t, RegistryGrant{Owner: "acme", Tenants: []string{"*"}})
	fake.entered = make(chan string, 1)
	release := fake.block("acme")
	ctx, cancel := context.WithCancel(context.Background())
	if _, pending, _ := m.lookup(ctx, "h1", 1, []string{"acme"}); !pending {
		t.Fatal("not minting")
	}
	<-fake.entered
	cancel()
	// The fake answers a cancelled context at once; give it the chance.
	time.Sleep(50 * time.Millisecond)
	close(release)
	creds, errs := settle(t, m, done, 2, "t1")
	if len(creds) != 1 || errs != nil {
		t.Fatalf("after a cancelled caller: %+v %v", creds, errs)
	}
	if len(fake.calls()) != 1 {
		t.Fatalf("%d AssumeRole calls, want the first mint's only", len(fake.calls()))
	}
}

// An assignment waits on its cold owners for one bound in total, from when
// it was first held, however many owners it has; then it goes without.
func TestRegistryHoldIsOneTotalBound(t *testing.T) {
	m, fake, clock, _ := minter(t, RegistryGrant{Owner: "acme", Tenants: []string{"*"}}, RegistryGrant{Owner: "team", Tenants: []string{"*"}})
	defer close(fake.block("acme"))
	defer close(fake.block("team"))
	if _, pending, _ := m.lookup(context.Background(), "h1", 1, m.ownersFor("t1")); !pending {
		t.Fatal("not held for the cold owners")
	}
	clock.advance(registryHold - time.Millisecond)
	if _, pending, _ := m.lookup(context.Background(), "h1", 1, m.ownersFor("t1")); !pending {
		t.Fatal("released before the bound")
	}
	clock.advance(time.Millisecond)
	creds, pending, errs := m.lookup(context.Background(), "h1", 1, m.ownersFor("t1"))
	if pending || creds != nil || !errors.Is(errs["acme"], errRegistrySlow) || !errors.Is(errs["team"], errRegistrySlow) {
		t.Fatalf("at the bound: %+v pending=%v %v", creds, pending, errs)
	}
	// Another assignment has its own bound.
	if _, pending, _ := m.lookup(context.Background(), "h1", 2, m.ownersFor("t1")); !pending {
		t.Fatal("a new assignment did not get its own wait")
	}
}

func TestRegistryNeeded(t *testing.T) {
	h := testRegistryHost
	build := func(cf, cache string) spec.Image {
		return spec.Image{Build: &spec.Build{Containerfile: cf, Cache: cache}}
	}
	for _, tc := range []struct {
		name     string
		image    spec.Image
		resolved string
		want     bool
	}{
		{"ref on the host", spec.Image{Ref: h + "/acme/app:1"}, "", true},
		{"ref elsewhere", spec.Image{Ref: "ghcr.io/acme/app:1"}, "", false},
		{"ref on a host with the same prefix", spec.Image{Ref: h + ".evil.example/acme/app:1"}, "", false},
		{"explicit auth for the host", spec.Image{Ref: h + "/acme/app:1", RegistryAuth: []spec.RegistryAuth{{Registry: h, Secret: "S"}}}, "", false},
		{"explicit auth for another host", spec.Image{Ref: h + "/acme/app:1", RegistryAuth: []spec.RegistryAuth{{Registry: "ghcr.io", Secret: "S"}}}, "", true},
		{"FROM on the host", build("FROM "+h+"/acme/base:1 AS b\nRUN true", ""), "", true},
		{"FROM with a flag", build("FROM --platform=linux/arm64 "+h+"/acme/base:1", ""), "", true},
		{"FROM elsewhere", build("FROM alpine\nRUN true", ""), "", false},
		{"FROM a build arg", build("ARG BASE\nFROM ${BASE}", ""), "", true},
		{"cache on the host", build("FROM alpine", h+"/acme/cache"), "", true},
		{"cache elsewhere", build("FROM alpine", "ghcr.io/acme/cache"), "", false},
		{"pinned FROM on the host", build("FROM alpine", ""), "FROM " + h + "/acme/base@sha256:ab", true},
		{"COPY --from a stage", build("FROM alpine AS a\nFROM alpine\nCOPY --from=a /x /x", ""), "", false},
	} {
		if got := registryNeeded(tc.image, tc.resolved, h); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// registryFixture: a static pool with h1, which advertises
// CapRegistryMinted, a minter on the real clock granting acme to every
// tenant and team to t1 only, and s's log in the returned buffer.
func registryFixture(t *testing.T) (*Server, context.Context, *fakeRegistryAWS, *syncBuffer) {
	t.Helper()
	s := testServer(t)
	logs := &syncBuffer{}
	s.log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m := newTestMinter(time.Now, RegistryGrant{Owner: "acme", Tenants: []string{"*"}}, RegistryGrant{Owner: "team", Tenants: []string{"t1"}})
	m.ready = s.hub.Notify
	s.registry = m
	namedPools(t, s, "default")
	execSQL(t, s, context.Background(), `UPDATE pools SET shared = true WHERE id = 'default'`)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1'), ('t2', 't2')`)
	readyHost(t, s, "h1", "default", "", false)
	execSQL(t, s, ctx, `UPDATE hosts SET capabilities = ARRAY[$1], capacity = '{"runs":10,"cpus":64,"memory":274877906944,"disk":1099511627776}' WHERE id = 'h1'`, proto.CapRegistryMinted)
	return s, ctx, fakeOf(m), logs
}

// submitImage submits a Run of tenant on the default pool with image (and
// secrets) and schedules it.
func submitImage(t *testing.T, s *Server, tenant string, image spec.Image, secrets ...spec.Secret) string {
	t.Helper()
	out, err := s.submitRun(tenantCtx(tenant), &submitRunInput{Body: spec.RunSpec{
		Image:     image,
		Secrets:   secrets,
		Workload:  spec.Workload{Adapter: "generic", Command: []string{"true"}},
		Placement: spec.Placement{Pool: "default"},
	}})
	if err != nil {
		t.Fatalf("%s submitting: %v", tenant, err)
	}
	schedule(t, s)
	return out.Body.ID
}

func ecrImage(owner string) spec.Image {
	return spec.Image{Ref: testRegistryHost + "/" + owner + "/app:1"}
}

// deliveredAssigns is every assignment pendingMessages hands h1 once
// those of runs are no longer held for a mint (as a reconnect replays
// them), by run id.
func deliveredAssigns(t *testing.T, s *Server, ctx context.Context, runs ...string) map[string]proto.Assign {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		execSQL(t, s, ctx, `UPDATE host_messages SET delivered_at = NULL WHERE host_id = 'h1'`)
		frames, err := s.pendingMessages(ctx, "h1", true)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]proto.Assign{}
		for _, f := range frames {
			if f.Type != proto.MsgAssign {
				continue
			}
			var a proto.Assign
			if err := json.Unmarshal(f.Data, &a); err != nil {
				t.Fatal(err)
			}
			out[a.RunID] = a
		}
		if !slices.ContainsFunc(runs, func(id string) bool { _, ok := out[id]; return !ok }) {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("assignments of %v not delivered after 5s: got %d", runs, len(out))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func registries(a proto.Assign) (out []string) {
	for _, c := range a.RegistryCredentials {
		out = append(out, c.Registry)
	}
	slices.Sort(out)
	return out
}

// The minted credentials reach the runner in the delivered assignment and
// nowhere else: not host_messages, events, any other row, nor the log. A
// tenant gets its own grants only; a runner without the capability gets
// none.
func TestRegistryCredentialsAttachedAtSendTime(t *testing.T) {
	s, ctx, _, logs := registryFixture(t)
	r1 := submitImage(t, s, "t1", ecrImage("acme"))
	r2 := submitImage(t, s, "t2", ecrImage("acme"))
	got := deliveredAssigns(t, s, ctx, r1, r2)
	for _, a := range got {
		for _, c := range a.RegistryCredentials {
			if c.User != "AWS" || c.Password == "" {
				t.Fatalf("credential %+v", c)
			}
		}
	}
	if g := registries(got[r1]); !slices.Equal(g, []string{testRegistryHost + "/acme", testRegistryHost + "/team"}) {
		t.Fatalf("t1's Run got %v", g)
	}
	if g := registries(got[r2]); !slices.Equal(g, []string{testRegistryHost + "/acme"}) {
		t.Fatalf("t2's Run got %v", g)
	}
	for _, a := range got {
		for _, c := range a.RegistryCredentials {
			notStored(t, s, ctx, logs, c.Password)
		}
	}
	var stored int
	systemScan(t, s, `SELECT count(*) FROM host_messages WHERE type = 'assign' AND payload ? 'registryCredentials'`, nil, &stored)
	if stored != 0 {
		t.Fatalf("%d stored assignments carry registryCredentials", stored)
	}

	// A runner that re-registered without the capability: none attached.
	execSQL(t, s, ctx, `UPDATE hosts SET capabilities = '{}' WHERE id = 'h1'`)
	for id, a := range deliveredAssigns(t, s, ctx, r1, r2) {
		if a.RegistryCredentials != nil {
			t.Fatalf("%s: credentials sent to a runner without %s: %+v", id, proto.CapRegistryMinted, a.RegistryCredentials)
		}
	}
}

// A runner without the capability, from a cold cache: nothing attached,
// and no AWS call at all.
func TestRegistryNothingMintedWithoutCapability(t *testing.T) {
	s, ctx, fake, _ := registryFixture(t)
	execSQL(t, s, ctx, `UPDATE hosts SET capabilities = '{}' WHERE id = 'h1'`)
	id := submitImage(t, s, "t1", ecrImage("acme"))
	a, ok := deliveredAssigns(t, s, ctx, id)[id]
	if !ok || len(a.RegistryCredentials) != 0 {
		t.Fatalf("unsupported runner assignment: %+v, present=%v", a, ok)
	}
	if len(fake.calls()) != 0 || fake.tokenCalls() != 0 {
		t.Fatalf("%d AssumeRole, %d GetAuthorizationToken calls for a runner without %s", len(fake.calls()), fake.tokenCalls(), proto.CapRegistryMinted)
	}
}

// Nothing is minted for a Run that cannot use it: its spec names another
// registry, or has its own registryAuth for this one.
func TestRegistryNotMintedForRunsThatCannotUseIt(t *testing.T) {
	s, ctx, fake, _ := registryFixture(t)
	elsewhere := submitImage(t, s, "t1", spec.Image{Ref: "ghcr.io/acme/app:1"})
	explicit := ecrImage("acme")
	explicit.RegistryAuth = []spec.RegistryAuth{{Registry: testRegistryHost, Secret: "ECR"}}
	own := submitImage(t, s, "t1", explicit, spec.Secret{Name: "ECR", Value: "AWS:caller-token"})
	got := deliveredAssigns(t, s, ctx, elsewhere, own)
	if got[elsewhere].RegistryCredentials != nil || got[own].RegistryCredentials != nil {
		t.Fatalf("attached: %+v / %+v", got[elsewhere].RegistryCredentials, got[own].RegistryCredentials)
	}
	if len(fake.calls()) != 0 {
		t.Fatalf("%d AssumeRole calls for Runs that cannot use the token", len(fake.calls()))
	}
	// A build whose FROM names the registry gets them.
	built := submitImage(t, s, "t2", spec.Image{Build: &spec.Build{Containerfile: "FROM " + testRegistryHost + "/acme/base:1\nRUN true"}})
	if g := registries(deliveredAssigns(t, s, ctx, built)[built]); !slices.Equal(g, []string{testRegistryHost + "/acme"}) {
		t.Fatalf("build from the registry got %v", g)
	}
}

// A failed mint does not hold the assignment: it is delivered without the
// owner's entry, and the warning names the owner, never a token.
func TestRegistryMintFailureStillAssigns(t *testing.T) {
	for name, setup := range map[string]func(*fakeRegistryAWS){
		"sts": func(f *fakeRegistryAWS) { f.fail = map[string]error{"team": errors.New("AccessDenied")} },
		"ecr": func(f *fakeRegistryAWS) {
			f.token = map[string]func() (*ecrtypes.AuthorizationData, error){"team": func() (*ecrtypes.AuthorizationData, error) {
				return nil, errors.New("AccessDenied")
			}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, ctx, fake, logs := registryFixture(t)
			setup(fake)
			r1 := submitImage(t, s, "t1", ecrImage("acme"))
			a := deliveredAssigns(t, s, ctx, r1)[r1]
			if g := registries(a); !slices.Equal(g, []string{testRegistryHost + "/acme"}) {
				t.Fatalf("delivered %v, want acme's only", g)
			}
			log := logs.String()
			if !strings.Contains(log, "registry pull credentials not minted") || !strings.Contains(log, "owner=team") || !strings.Contains(log, "AccessDenied") {
				t.Fatalf("no warning naming the owner:\n%s", log)
			}
			notStored(t, s, ctx, logs, a.RegistryCredentials[0].Password)
		})
	}
	// Every owner failing: the assignment goes exactly as without minting.
	s, ctx, fake, _ := registryFixture(t)
	fake.fail = map[string]error{"acme": errors.New("AccessDenied"), "team": errors.New("AccessDenied")}
	r1 := submitImage(t, s, "t1", ecrImage("acme"))
	if a := deliveredAssigns(t, s, ctx, r1)[r1]; a.RunID != r1 || a.RegistryCredentials != nil {
		t.Fatalf("delivered %+v", a)
	}
}

// A failed read while planning credentials leaves the batch undelivered:
// the next pass on the same connection sends all of it.
func TestRegistryPlanFailureLosesNothing(t *testing.T) {
	s, ctx, _, _ := registryFixture(t)
	r1 := submitImage(t, s, "t1", ecrImage("acme"))
	execSQL(t, s, ctx, `INSERT INTO host_messages (host_id, run_id, epoch, type) VALUES ('h1', 'run_other', 1, $1)`, proto.MsgStop)
	fail := true
	s.registryPlanFails = func() error {
		if fail {
			fail = false
			return errors.New("connection reset")
		}
		return nil
	}
	if _, err := s.pendingMessages(ctx, "h1", true); err == nil {
		t.Fatal("the plan's failure was not returned")
	}
	types := map[string]bool{}
	waitUntil(t, func() bool {
		frames, err := s.pendingMessages(ctx, "h1", true)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range frames {
			types[f.Type+"/"+f.RunID] = true
		}
		return types[proto.MsgAssign+"/"+r1] && types[proto.MsgStop+"/run_other"]
	}, "the batch after a failed plan")
}

// While a mint is blocked, the host's WebSocket still carries live frames
// and unrelated durable messages at once; the cold assignment waits for
// one bound, then goes with the tokens that are ready.
func TestRegistryMintDoesNotBlockDelivery(t *testing.T) {
	s, ctx, fake, _ := registryFixture(t)
	const hold = 600 * time.Millisecond
	s.registry.hold = hold
	release := fake.block("acme")
	defer close(release)
	raw := ids.Secret("luxh")
	execSQL(t, s, ctx, `INSERT INTO host_tokens (id, pool_id, token_hash) VALUES ('tok1', $1, $2)`, poolID(t, s, "default", ""), ids.Hash(raw))
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	wctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(wctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/runner/v1/ws",
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + raw}}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	hello := proto.Hello{Name: "h1", ProtocolVersion: proto.Version, Capabilities: []string{proto.CapRegistryMinted},
		Capacity: proto.Capacity{Runs: 10, CPUs: 64, Memory: 256 << 30, Disk: 1 << 40}}
	if err := writeFrame(wctx, ws, proto.Frame{Type: proto.MsgHello, Data: proto.Marshal(hello)}); err != nil {
		t.Fatal(err)
	}
	type arrival struct {
		f  proto.Frame
		at time.Time
	}
	frames := make(chan arrival, 64)
	go func() {
		for {
			var f proto.Frame
			if err := readFrame(wctx, ws, &f); err != nil {
				return
			}
			frames <- arrival{f, time.Now()}
		}
	}()
	next := func(what string, ok func(proto.Frame) bool, within time.Duration) arrival {
		t.Helper()
		deadline := time.After(within)
		for {
			select {
			case a := <-frames:
				if ok(a.f) {
					return a
				}
			case <-deadline:
				t.Fatalf("no %s within %s", what, within)
			}
		}
	}
	next("welcome", func(f proto.Frame) bool { return f.Type == proto.MsgWelcome }, 5*time.Second)

	queued := time.Now()
	r1 := submitImage(t, s, "t1", ecrImage("acme"))
	waitUntil(t, func() bool { return len(fake.calls()) > 0 }, "acme's mint")
	// acme's AssumeRole is blocked now. A stop for another Run, and a
	// live frame, go out at once.
	execSQL(t, s, ctx, `INSERT INTO host_messages (host_id, run_id, epoch, type) VALUES ('h1', 'run_other', 1, $1)`, proto.MsgStop)
	s.hub.Notify("h1")
	next("unrelated stop", func(f proto.Frame) bool { return f.Type == proto.MsgStop && f.RunID == "run_other" }, hold/2)
	if err := s.hub.SendLive("h1", proto.Frame{Type: proto.MsgOutputCancel, Data: []byte(`{"subId":"x"}`)}); err != nil {
		t.Fatal(err)
	}
	next("live frame", func(f proto.Frame) bool { return f.Type == proto.MsgOutputCancel }, hold/2)

	a := next("assignment", func(f proto.Frame) bool { return f.Type == proto.MsgAssign && f.RunID == r1 }, hold+3*time.Second)
	if waited := a.at.Sub(queued); waited < hold-100*time.Millisecond {
		t.Fatalf("assignment sent after %s, before acme's wait of %s ended", waited, hold)
	}
	var as proto.Assign
	if err := json.Unmarshal(a.f.Data, &as); err != nil {
		t.Fatal(err)
	}
	if g := registries(as); !slices.Equal(g, []string{testRegistryHost + "/team"}) {
		t.Fatalf("assignment carried %v, want team's only (acme still minting)", g)
	}
}
