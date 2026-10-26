package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"

	"github.com/marcioapm/lux/internal/proto"
)

const testRegistryHost = "123456789012.dkr.ecr.eu-west-1.amazonaws.com"

// fakeRegistryAWS mints a distinct token per call: <owner>-pw-<n>, with
// the owner read from the session tag, so a test can tell a reused token
// from a fresh one and one owner's from another's.
type fakeRegistryAWS struct {
	mu       sync.Mutex
	now      func() time.Time
	assumed  []sts.AssumeRoleInput
	n        int
	fail     map[string]error // by owner: AssumeRole fails
	tokenTTL time.Duration    // 0: 12h
}

func (f *fakeRegistryAWS) AssumeRole(_ context.Context, in *sts.AssumeRoleInput) (*ststypes.Credentials, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.assumed = append(f.assumed, *in)
	owner := ""
	for _, tag := range in.Tags {
		if aws.ToString(tag.Key) == "lux-owner" {
			owner = aws.ToString(tag.Value)
		}
	}
	if err := f.fail[owner]; err != nil {
		return nil, err
	}
	exp := f.now().Add(time.Duration(aws.ToInt32(in.DurationSeconds)) * time.Second)
	return &ststypes.Credentials{AccessKeyId: aws.String("ASIA" + owner), SecretAccessKey: aws.String("sk"), SessionToken: aws.String(owner), Expiration: &exp}, nil
}

func (f *fakeRegistryAWS) AuthorizationToken(_ context.Context, c *ststypes.Credentials) (*ecrtypes.AuthorizationData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	ttl := f.tokenTTL
	if ttl == 0 {
		ttl = 12 * time.Hour
	}
	exp := f.now().Add(ttl)
	tok := base64.StdEncoding.EncodeToString([]byte("AWS:" + aws.ToString(c.SessionToken) + "-pw-" + string(rune('0'+f.n))))
	return &ecrtypes.AuthorizationData{AuthorizationToken: &tok, ExpiresAt: &exp}, nil
}

func (f *fakeRegistryAWS) calls() []sts.AssumeRoleInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.assumed)
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

func testMinter(grants ...RegistryGrant) (*registryMinter, *fakeRegistryAWS, *testClock) {
	clock := &testClock{t: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}
	fake := &fakeRegistryAWS{now: clock.now}
	m := newRegistryMinter(RegistryConfig{Host: testRegistryHost, Region: "eu-west-1",
		PullRoleARN: "arn:aws:iam::123456789012:role/lux-registry-pull", Grants: grants, AWS: fake}, clock.now)
	return m, fake, clock
}

func TestRegistryGrants(t *testing.T) {
	m, _, _ := testMinter(
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
	none, fake, _ := testMinter(RegistryGrant{Owner: "team", Tenants: []string{"t1"}})
	if creds, errs := none.credentials(context.Background(), "t2"); creds != nil || errs != nil || len(fake.calls()) != 0 {
		t.Fatalf("ungranted tenant: %v %v, %d AssumeRole calls", creds, errs, len(fake.calls()))
	}
	// Off: no minter, nothing granted.
	if off := newRegistryMinter(RegistryConfig{}, time.Now); off != nil || off.ownersFor("t1") != nil {
		t.Fatal("an empty [registry] is not off")
	}
}

// One AssumeRole per owner, tagged and named for it; its token is reused
// while 30 minutes or more remain, and minted again after.
func TestRegistryMintPerOwnerAndCache(t *testing.T) {
	m, fake, clock := testMinter(RegistryGrant{Owner: "acme", Tenants: []string{"*"}}, RegistryGrant{Owner: "team.b", Tenants: []string{"t1"}})
	ctx := context.Background()
	creds, errs := m.credentials(ctx, "t1")
	if errs != nil {
		t.Fatal(errs)
	}
	want := []proto.RegistryCredential{
		{Registry: testRegistryHost + "/acme", User: "AWS", Password: "acme-pw-1"},
		{Registry: testRegistryHost + "/team.b", User: "AWS", Password: "team.b-pw-2"},
	}
	if !slices.Equal(creds, want) {
		t.Fatalf("credentials %+v, want %+v", creds, want)
	}
	calls := fake.calls()
	if len(calls) != 2 {
		t.Fatalf("%d AssumeRole calls, want 2", len(calls))
	}
	for i, owner := range []string{"acme", "team.b"} {
		c := calls[i]
		if aws.ToString(c.RoleArn) != "arn:aws:iam::123456789012:role/lux-registry-pull" ||
			aws.ToString(c.RoleSessionName) != "luxd-registry-"+owner || aws.ToInt32(c.DurationSeconds) != 3600 ||
			len(c.Tags) != 1 || aws.ToString(c.Tags[0].Key) != "lux-owner" || aws.ToString(c.Tags[0].Value) != owner {
			t.Errorf("AssumeRole for %s: role %s session %s duration %d tags %+v", owner, aws.ToString(c.RoleArn),
				aws.ToString(c.RoleSessionName), aws.ToInt32(c.DurationSeconds), c.Tags)
		}
	}

	// The session lasts an hour, so the token is taken to: reused until
	// fewer than 30 minutes remain.
	clock.advance(29 * time.Minute)
	if again, _ := m.credentials(ctx, "t2"); len(again) != 1 || again[0] != want[0] || len(fake.calls()) != 2 {
		t.Fatalf("at 29m: %+v, %d calls; want the cached token", again, len(fake.calls()))
	}
	clock.advance(2 * time.Minute)
	again, _ := m.credentials(ctx, "t2")
	if len(again) != 1 || again[0].Password != "acme-pw-3" || len(fake.calls()) != 3 {
		t.Fatalf("at 31m: %+v, %d calls; want a fresh token for acme only", again, len(fake.calls()))
	}
}

// A failed mint leaves that owner out, keeps the others, is retried only
// after a pause, and never surfaces a token value in its error.
func TestRegistryMintFailure(t *testing.T) {
	m, fake, clock := testMinter(RegistryGrant{Owner: "acme", Tenants: []string{"*"}}, RegistryGrant{Owner: "broken", Tenants: []string{"*"}})
	fake.fail = map[string]error{"broken": errors.New("AccessDenied: not authorized to perform sts:TagSession")}
	ctx := context.Background()
	creds, errs := m.credentials(ctx, "t1")
	if len(creds) != 1 || creds[0].Registry != testRegistryHost+"/acme" || len(errs) != 1 || errs["broken"] == nil {
		t.Fatalf("%+v %v", creds, errs)
	}
	if _, errs = m.credentials(ctx, "t1"); !errors.Is(errs["broken"], errRegistryBackoff) || len(fake.calls()) != 2 {
		t.Fatalf("retried at once: %v, %d calls", errs, len(fake.calls()))
	}
	clock.advance(registryRetry)
	delete(fake.fail, "broken")
	if creds, errs = m.credentials(ctx, "t1"); len(creds) != 2 || errs != nil {
		t.Fatalf("after the pause: %+v %v", creds, errs)
	}
}

// registryFixture: a static pool with h1, which advertises
// CapRegistryMinted, a minter granting acme to every tenant and team to t1
// only, and s's log in the returned buffer.
func registryFixture(t *testing.T) (*Server, context.Context, *fakeRegistryAWS, *syncBuffer) {
	t.Helper()
	s := testServer(t)
	logs := &syncBuffer{}
	s.log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m, fake, _ := testMinter(RegistryGrant{Owner: "acme", Tenants: []string{"*"}}, RegistryGrant{Owner: "team", Tenants: []string{"t1"}})
	s.registry = m
	namedPools(t, s, "default")
	execSQL(t, s, context.Background(), `UPDATE pools SET shared = true WHERE id = 'default'`)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1'), ('t2', 't2')`)
	readyHost(t, s, "h1", "default", "", false)
	execSQL(t, s, ctx, `UPDATE hosts SET capabilities = ARRAY[$1], capacity = '{"runs":10,"cpus":64,"memory":274877906944,"disk":1099511627776}' WHERE id = 'h1'`, proto.CapRegistryMinted)
	return s, ctx, fake, logs
}

// deliveredAssigns is every assignment pendingMessages hands h1 now (as a
// reconnect replays them), by run id.
func deliveredAssigns(t *testing.T, s *Server, ctx context.Context) map[string]proto.Assign {
	t.Helper()
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
	return out
}

// The minted credentials reach the runner in the delivered assignment and
// nowhere else: not host_messages, events, any other row, nor the log. A
// tenant gets its own grants only; a runner without the capability gets
// none.
func TestRegistryCredentialsAttachedAtSendTime(t *testing.T) {
	s, ctx, fake, logs := registryFixture(t)
	r1 := submitAs(t, s, "t1", "default", "")
	schedule(t, s)
	r2 := submitAs(t, s, "t2", "default", "")
	schedule(t, s)
	got := deliveredAssigns(t, s, ctx)
	if len(got) != 2 {
		h1, st1, why1 := placedOn(t, s, r1)
		h2, st2, why2 := placedOn(t, s, r2)
		t.Fatalf("delivered %d assignments: %s %s %s / %s %s %s", len(got), h1, st1, why1, h2, st2, why2)
	}
	regs := func(a proto.Assign) (out []string) {
		for _, c := range a.RegistryCredentials {
			if c.User != "AWS" || c.Password == "" {
				t.Fatalf("credential %+v", c)
			}
			out = append(out, c.Registry)
		}
		return out
	}
	if g := regs(got[r1]); !slices.Equal(g, []string{testRegistryHost + "/acme", testRegistryHost + "/team"}) {
		t.Fatalf("t1's Run got %v", g)
	}
	if g := regs(got[r2]); !slices.Equal(g, []string{testRegistryHost + "/acme"}) {
		t.Fatalf("t2's Run got %v", g)
	}
	var passwords []string
	for _, a := range got {
		for _, c := range a.RegistryCredentials {
			passwords = append(passwords, c.Password)
		}
	}
	for _, pw := range passwords {
		notStored(t, s, ctx, logs, pw)
	}
	var stored int
	systemScan(t, s, `SELECT count(*) FROM host_messages WHERE type = 'assign' AND payload ? 'registryCredentials'`, nil, &stored)
	if stored != 0 {
		t.Fatalf("%d stored assignments carry registryCredentials", stored)
	}
	calls := len(fake.calls())

	// A runner that re-registered without the capability: none attached,
	// and nothing minted for it.
	execSQL(t, s, ctx, `UPDATE hosts SET capabilities = '{}' WHERE id = 'h1'`)
	for id, a := range deliveredAssigns(t, s, ctx) {
		if a.RegistryCredentials != nil {
			t.Fatalf("%s: credentials sent to a runner without %s: %+v", id, proto.CapRegistryMinted, a.RegistryCredentials)
		}
	}
	if len(fake.calls()) != calls {
		t.Fatal("minted for a runner that cannot take the credentials")
	}
}

// A failed mint does not hold the assignment: it is delivered without the
// owner's entry, and the warning names the owner, never a token.
func TestRegistryMintFailureStillAssigns(t *testing.T) {
	s, ctx, fake, logs := registryFixture(t)
	fake.fail = map[string]error{"team": errors.New("AccessDenied")}
	r1 := submitAs(t, s, "t1", "default", "")
	schedule(t, s)
	a, ok := deliveredAssigns(t, s, ctx)[r1]
	if !ok {
		t.Fatal("the assignment was not delivered")
	}
	if len(a.RegistryCredentials) != 1 || a.RegistryCredentials[0].Registry != testRegistryHost+"/acme" {
		t.Fatalf("delivered %+v, want acme's only", a.RegistryCredentials)
	}
	log := logs.String()
	if !strings.Contains(log, "registry pull credentials not minted") || !strings.Contains(log, "owner=team") || !strings.Contains(log, "AccessDenied") {
		t.Fatalf("no warning naming the owner:\n%s", log)
	}
	notStored(t, s, ctx, logs, a.RegistryCredentials[0].Password)

	// Every owner failing: the assignment goes exactly as without minting.
	fake.fail["acme"] = errors.New("AccessDenied")
	s.registry.owners = map[string]*ownerToken{}
	if a := deliveredAssigns(t, s, ctx)[r1]; a.RunID != r1 || a.RegistryCredentials != nil {
		t.Fatalf("delivered %+v", a)
	}
}
