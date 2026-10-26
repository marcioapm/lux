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
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

const testRegistryHost = "123456789012.dkr.ecr.eu-west-1.amazonaws.com"

// fakeRegistryAWS mints a distinct token per call: <owner>-pw-<n>, with
// the owner read from the session tag, so a test can tell a reused token
// from a fresh one and one owner's from another's.
type fakeRegistryAWS struct {
	mu       sync.Mutex
	now      func() time.Time
	clock    *testClock    // the clock now reads, when the fixture made one
	timers   *manualTimers // the minter's hold timers, when the fixture made them
	assumed  []sts.AssumeRoleInput
	tokens   int
	fail     map[string]error // by owner: AssumeRole fails
	noCreds  map[string]bool  // by owner: AssumeRole answers no credentials
	tokenTTL time.Duration    // 0: 12h
	// token, by owner, replaces GetAuthorizationToken's answer.
	token map[string]func() (*ecrtypes.AuthorizationData, error)
	// gate, by owner: AssumeRole waits for it to close (or its context).
	gate map[string]chan struct{}
	// entered, if set, gets the owner of every AssumeRole as it starts;
	// contexts, if set, the context it was called with.
	entered  chan string
	contexts chan context.Context
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
	gate, entered, contexts := f.gate[owner], f.entered, f.contexts
	f.mu.Unlock()
	if contexts != nil {
		contexts <- ctx
	}
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
	if f.noCreds[owner] {
		return nil, nil
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

// manualTimers replaces a minter's hold timers: each scheduled function
// runs only when the test calls fire.
type manualTimers struct {
	mu      sync.Mutex
	pending []*manualTimer
}

type manualTimer struct {
	f       func()
	stopped bool
}

func useManualTimers(m *registryMinter) *manualTimers {
	mt := &manualTimers{}
	m.afterFunc = func(_ time.Duration, f func()) func() bool {
		mt.mu.Lock()
		defer mt.mu.Unlock()
		tm := &manualTimer{f: f}
		mt.pending = append(mt.pending, tm)
		return func() bool {
			mt.mu.Lock()
			defer mt.mu.Unlock()
			was := !tm.stopped
			tm.stopped = true
			return was
		}
	}
	return mt
}

// fire runs every timer not stopped, once; it returns how many ran.
func (mt *manualTimers) fire() int {
	mt.mu.Lock()
	var run []func()
	for _, tm := range mt.pending {
		if !tm.stopped {
			tm.stopped = true
			run = append(run, tm.f)
		}
	}
	mt.mu.Unlock()
	for _, f := range run {
		f()
	}
	return len(run)
}

// live is how many timers are neither fired nor stopped.
func (mt *manualTimers) live() int {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	n := 0
	for _, tm := range mt.pending {
		if !tm.stopped {
			n++
		}
	}
	return n
}

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

// minter is a minter on a fake clock with a fake AWS and manual hold
// timers, closed when the test ends.
func minter(t testing.TB, grants ...RegistryGrant) (*registryMinter, *fakeRegistryAWS, *testClock, minted) {
	t.Helper()
	clock := &testClock{t: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}
	m := newTestMinter(clock.now, grants...)
	useManualTimers(m)
	done := make(minted, 1)
	m.ready = done.signal
	closeMinter(t, m)
	return m, fakeOf(m), clock, done
}

// closeMinter closes m when the test ends; a close that does not return
// (a mint or timer outliving it) fails the test.
func closeMinter(t testing.TB, m *registryMinter) {
	t.Cleanup(func() {
		closed := make(chan struct{})
		go func() { m.close(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(30 * time.Second):
			t.Error("the minter's mints did not end on close")
		}
	})
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

// AssumeRole answering without credentials (nor an error) is a failure.
func TestRegistrySTSNoCredentials(t *testing.T) {
	requireMintFails(t, func(f *fakeRegistryAWS) {
		f.noCreds = map[string]bool{"acme": true}
	}, "sts:AssumeRole: no credentials", "")
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
// owner in backoff for the next caller: the context AWS got outlives the
// caller's, keeps its values, and has the mint's own deadline.
func TestRegistryCancelledCallerDoesNotArmBackoff(t *testing.T) {
	m, fake, _, done := minter(t, RegistryGrant{Owner: "acme", Tenants: []string{"*"}})
	fake.contexts = make(chan context.Context, 1)
	release := fake.block("acme")
	type key struct{}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), key{}, "caller"))
	before := time.Now()
	if _, pending, _ := m.lookup(ctx, "h1", 1, []string{"acme"}); !pending {
		t.Fatal("not minting")
	}
	var mintCtx context.Context
	select {
	case mintCtx = <-fake.contexts:
	case <-time.After(10 * time.Second):
		t.Fatal("AssumeRole was not called")
	}
	cancel()
	if err := mintCtx.Err(); err != nil {
		t.Fatalf("the caller's cancellation reached AWS: %v", err)
	}
	if mintCtx.Value(key{}) != "caller" {
		t.Fatal("AWS's context lost the caller's values")
	}
	if dl, ok := mintCtx.Deadline(); !ok || dl.Before(before.Add(registryMintTimeout)) || dl.After(time.Now().Add(registryMintTimeout)) {
		t.Fatalf("AWS's deadline %v (set %v), want the mint's own %s", dl, ok, registryMintTimeout)
	}
	close(release)
	creds, errs := settle(t, m, done, 2, "t1")
	if len(creds) != 1 || errs != nil {
		t.Fatalf("after a cancelled caller: %+v %v", creds, errs)
	}
	if len(fake.calls()) != 1 {
		t.Fatalf("%d AssumeRole calls, want the first mint's only", len(fake.calls()))
	}
}

// Closing the minter cancels a blocked mint, waits for it, stops the hold
// timers, and leaves lookups inert: nothing minted, and an assignment that
// needs a token is held rather than sent without it.
func TestRegistryCloseEndsMintsAndTimers(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}
	m := newTestMinter(clock.now, RegistryGrant{Owner: "acme", Tenants: []string{"*"}})
	timers := useManualTimers(m)
	var notified sync.WaitGroup
	m.ready = func(string) { notified.Done() }
	fake := fakeOf(m)
	fake.contexts = make(chan context.Context, 1)
	fake.block("acme") // never released: only close ends it
	if _, pending, _ := m.lookup(context.Background(), "h1", 1, []string{"acme"}); !pending {
		t.Fatal("not minting")
	}
	mintCtx := <-fake.contexts
	if timers.live() != 1 {
		t.Fatalf("%d hold timers, want 1", timers.live())
	}
	closed := make(chan struct{})
	go func() { m.close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("close did not return while a mint was blocked")
	}
	if mintCtx.Err() == nil {
		t.Fatal("close did not cancel the mint in flight")
	}
	if timers.live() != 0 {
		t.Fatalf("%d hold timers still live after close", timers.live())
	}
	// Firing a stopped timer runs nothing, so ready is never called.
	if n := timers.fire(); n != 0 {
		t.Fatalf("%d timers fired after close", n)
	}
	m.notify("h1") // no ready after close: a Done here would panic
	if creds, pending, errs := m.lookup(context.Background(), "h1", 2, []string{"acme"}); creds != nil || !pending || errs != nil || len(fake.calls()) != 1 {
		t.Fatalf("lookup after close: %v %v %v, %d AssumeRole calls", creds, pending, errs, len(fake.calls()))
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
	// A pass that then fails to send it does not start a new wait.
	clock.advance(time.Second)
	if _, pending, _ := m.lookup(context.Background(), "h1", 1, m.ownersFor("t1")); pending {
		t.Fatal("a second look after the bound waited again")
	}
	// Another assignment has its own bound.
	if _, pending, _ := m.lookup(context.Background(), "h1", 2, m.ownersFor("t1")); !pending {
		t.Fatal("a new assignment did not get its own wait")
	}
}

// When an assignment's hold ends with no mint finished, its host gets a
// pass: one per held assignment, from a timer scheduled when it was first
// held, and stopped once it is no longer held.
func TestRegistryHoldEndNotifiesItsHost(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}
	m := newTestMinter(clock.now, RegistryGrant{Owner: "acme", Tenants: []string{"*"}})
	timers := useManualTimers(m)
	closeMinter(t, m)
	var mu sync.Mutex
	var woken []string
	m.ready = func(h string) { mu.Lock(); woken = append(woken, h); mu.Unlock() }
	release := fakeOf(m).block("acme")
	for range 3 {
		if _, pending, _ := m.lookup(context.Background(), "h1", 1, []string{"acme"}); !pending {
			t.Fatal("not held")
		}
	}
	if _, pending, _ := m.lookup(context.Background(), "h2", 2, []string{"acme"}); !pending {
		t.Fatal("not held")
	}
	if timers.live() != 2 {
		t.Fatalf("%d hold timers, want one per held assignment", timers.live())
	}
	if n := timers.fire(); n != 2 {
		t.Fatalf("%d timers fired", n)
	}
	mu.Lock()
	got := slices.Sorted(slices.Values(woken))
	mu.Unlock()
	if !slices.Equal(got, []string{"h1", "h2"}) {
		t.Fatalf("woken %v, want h1 and h2 once each", got)
	}
	// An assignment no longer held stops its timer.
	if _, pending, _ := m.lookup(context.Background(), "h1", 3, []string{"acme"}); !pending {
		t.Fatal("not held")
	}
	close(release)
	waitUntil(t, func() bool {
		_, pending, _ := m.lookup(context.Background(), "h1", 3, []string{"acme"})
		return !pending
	}, "acme's token")
	if timers.live() != 0 {
		t.Fatalf("%d hold timers live after the token came", timers.live())
	}
}

// A hold whose assignment is never looked up again (acked elsewhere) is
// forgotten, once no mint it waited on can still run.
func TestRegistryForgetsAbandonedHolds(t *testing.T) {
	m, fake, clock, _ := minter(t, RegistryGrant{Owner: "acme", Tenants: []string{"*"}})
	defer close(fake.block("acme"))
	for id := range int64(5) {
		m.lookup(context.Background(), "h1", id+1, []string{"acme"})
	}
	clock.advance(registryHold + registryMintTimeout + time.Millisecond)
	m.lookup(context.Background(), "h1", 100, []string{"acme"})
	m.mu.Lock()
	_, kept := m.held[1]
	n := len(m.held)
	m.mu.Unlock()
	if kept || n != 1 {
		t.Fatalf("%d holds kept (the first: %v), want only the new one", n, kept)
	}
}

func TestRegistryOwners(t *testing.T) {
	h := testRegistryHost
	build := func(cf, cache string) spec.Image {
		return spec.Image{Build: &spec.Build{Containerfile: cf, Cache: cache}}
	}
	granted := []string{"acme", "team", "other"}
	for _, tc := range []struct {
		name     string
		image    spec.Image
		resolved string
		want     []string
	}{
		{"ref on the host", spec.Image{Ref: h + "/acme/app:1"}, "", []string{"acme"}},
		{"ref on the host, upper case", spec.Image{Ref: strings.ToUpper(h) + "/acme/app:1"}, "", []string{"acme"}},
		{"ref elsewhere", spec.Image{Ref: "ghcr.io/acme/app:1"}, "", nil},
		{"ref on a host with the same prefix", spec.Image{Ref: h + ".evil.example/acme/app:1"}, "", nil},
		{"ref of an ungranted owner", spec.Image{Ref: h + "/nobody/app:1"}, "", nil},
		{"ref with no owner segment", spec.Image{Ref: h + "/acme:1"}, "", nil},
		{"explicit auth for the host", spec.Image{Ref: h + "/acme/app:1", RegistryAuth: []spec.RegistryAuth{{Registry: h, Secret: "S"}}}, "", nil},
		{"explicit auth for another host", spec.Image{Ref: h + "/acme/app:1", RegistryAuth: []spec.RegistryAuth{{Registry: "ghcr.io", Secret: "S"}}}, "", []string{"acme"}},
		{"FROM on the host", build("FROM "+h+"/team/base:1 AS b\nRUN true", ""), "", []string{"team"}},
		{"FROM with a flag", build("FROM --platform=linux/arm64 "+h+"/acme/base:1", ""), "", []string{"acme"}},
		{"FROM elsewhere", build("FROM alpine\nRUN true", ""), "", nil},
		{"FROM a build arg", build("ARG BASE\nFROM ${BASE}", ""), "", nil},
		{"FROM a build arg on the host", build("ARG T=1\nFROM "+h+"/acme/base:$T", ""), "", nil},
		{"cache on the host", build("FROM alpine", h+"/other/cache"), "", []string{"other"}},
		{"cache elsewhere", build("FROM alpine", "ghcr.io/acme/cache"), "", nil},
		{"pinned FROM on the host", build("FROM alpine", ""), "FROM " + h + "/acme/base@sha256:ab", []string{"acme"}},
		{"COPY --from a stage", build("FROM alpine AS a\nFROM alpine\nCOPY --from=a /x /x", ""), "", nil},
		{"every place, in grant order", build("FROM "+h+"/other/a:1\nFROM "+h+"/acme/b:1", h+"/team/cache"), "", []string{"acme", "team", "other"}},
	} {
		if got := registryOwners(tc.image, tc.resolved, h, granted); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// registryFixture: a static pool with h1, which advertises
// CapRegistryMinted, a minter granting acme to every tenant and team to t1
// only, and s's log in the returned buffer. The minter's clock (fake.clock)
// stands still unless a test advances it, and its hold timers
// (fake.timers) fire only by hand, so no hold ends on its own. It is
// closed when the test ends.
func registryFixture(t *testing.T) (*Server, context.Context, *fakeRegistryAWS, *syncBuffer) {
	t.Helper()
	s := testServer(t)
	logs := &syncBuffer{}
	s.log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	clock := &testClock{t: time.Now()}
	m := newTestMinter(clock.now, RegistryGrant{Owner: "acme", Tenants: []string{"*"}}, RegistryGrant{Owner: "team", Tenants: []string{"t1"}})
	fakeOf(m).clock, fakeOf(m).timers = clock, useManualTimers(m)
	m.ready = s.hub.Notify
	s.registry = m
	closeMinter(t, m)
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

// ecrBuild is a build whose FROMs name each of owners' repositories.
func ecrBuild(owners ...string) spec.Image {
	var cf []string
	for _, o := range owners {
		cf = append(cf, "FROM "+testRegistryHost+"/"+o+"/base:1")
	}
	return spec.Image{Build: &spec.Build{Containerfile: strings.Join(append(cf, "RUN true"), "\n")}}
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
// tenant gets its own grants only, and of those only the owners its Run
// names; a runner without the capability gets none.
func TestRegistryCredentialsAttachedAtSendTime(t *testing.T) {
	s, ctx, _, logs := registryFixture(t)
	r1 := submitImage(t, s, "t1", ecrBuild("acme", "team"))
	r2 := submitImage(t, s, "t2", ecrBuild("acme", "team"))
	acmeOnly := submitImage(t, s, "t1", ecrImage("acme"))
	got := deliveredAssigns(t, s, ctx, r1, r2, acmeOnly)
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
	if g := registries(got[acmeOnly]); !slices.Equal(g, []string{testRegistryHost + "/acme"}) {
		t.Fatalf("t1's Run naming only acme got %v", g)
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
// registry, has its own registryAuth for this one, or names its base only
// through a build argument (the runner refuses to pin such a FROM, so it
// never pulls with a token).
func TestRegistryNotMintedForRunsThatCannotUseIt(t *testing.T) {
	s, ctx, fake, _ := registryFixture(t)
	elsewhere := submitImage(t, s, "t1", spec.Image{Ref: "ghcr.io/acme/app:1"})
	explicit := ecrImage("acme")
	explicit.RegistryAuth = []spec.RegistryAuth{{Registry: testRegistryHost, Secret: "ECR"}}
	own := submitImage(t, s, "t1", explicit, spec.Secret{Name: "ECR", Value: "AWS:caller-token"})
	variable := submitImage(t, s, "t1", spec.Image{Build: &spec.Build{Containerfile: "ARG BASE=" + testRegistryHost + "/acme/base:1\nFROM $BASE\nRUN true"}})
	got := deliveredAssigns(t, s, ctx, elsewhere, own, variable)
	for _, id := range []string{elsewhere, own, variable} {
		if got[id].RegistryCredentials != nil {
			t.Fatalf("%s: attached %+v", id, got[id].RegistryCredentials)
		}
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
			r1 := submitImage(t, s, "t1", ecrBuild("acme", "team"))
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
	r1 := submitImage(t, s, "t1", ecrBuild("acme", "team"))
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

// pass is one delivery pass for h1's current connection (undelivered
// messages only), bounded so a stuck pass fails the test.
func pass(t *testing.T, s *Server) []proto.Frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	frames, err := s.pendingMessages(ctx, "h1", true)
	if err != nil {
		t.Fatal(err)
	}
	return frames
}

// passPausedAfterPlan is pass, stopped between its registry plan and its
// UPDATE while during runs.
func passPausedAfterPlan(t *testing.T, s *Server, during func()) []proto.Frame {
	t.Helper()
	planned, resume := make(chan struct{}), make(chan struct{})
	hook := func() { close(planned); <-resume }
	s.afterRegistryPlan.Store(&hook)
	type result struct {
		frames []proto.Frame
		err    error
	}
	done := make(chan result, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() {
		frames, err := s.pendingMessages(ctx, "h1", true)
		done <- result{frames, err}
	}()
	select {
	case <-planned:
	case r := <-done:
		t.Fatalf("the pass ended before its UPDATE: %v", r.err)
	case <-ctx.Done():
		t.Fatal("the pass never finished its plan")
	}
	func() {
		defer close(resume)
		during()
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r.frames
	case <-ctx.Done():
		t.Fatal("the paused pass did not finish")
		return nil
	}
}

// keys are frames as type/run, in order.
func keys(frames []proto.Frame) []string {
	out := []string{}
	for _, f := range frames {
		out = append(out, f.Type+"/"+f.RunID)
	}
	return out
}

func requireKeys(t *testing.T, what string, frames []proto.Frame, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if got := keys(frames); !slices.Equal(got, want) {
		t.Fatalf("%s: %v, want %v", what, got, want)
	}
}

// registryAssign decodes the assignment of run in frames (its only one).
func registryAssign(t *testing.T, frames []proto.Frame, run string) proto.Assign {
	t.Helper()
	var out []proto.Assign
	for _, f := range frames {
		if f.Type == proto.MsgAssign && f.RunID == run {
			var a proto.Assign
			if err := json.Unmarshal(f.Data, &a); err != nil {
				t.Fatal(err)
			}
			out = append(out, a)
		}
	}
	if len(out) != 1 {
		t.Fatalf("%d assignments of %s, want 1: %v", len(out), run, keys(frames))
	}
	return out[0]
}

// warm waits until s's minter holds a ready token for each of owners.
func warm(t *testing.T, s *Server, owners ...string) {
	t.Helper()
	waitUntil(t, func() bool {
		creds, pending, _ := s.registry.lookup(context.Background(), "warm", -1, owners)
		return !pending && len(creds) == len(owners)
	}, "tokens for "+strings.Join(owners, ", "))
}

// queue writes a durable message of typ for run's epoch to h1.
func queue(t *testing.T, s *Server, run string, epoch int, typ string) {
	t.Helper()
	execSQL(t, s, context.Background(), `INSERT INTO host_messages (host_id, run_id, epoch, type) VALUES ('h1', $1, $2, $3)`, run, epoch, typ)
}

func assignEpoch(t *testing.T, s *Server, run string) int {
	t.Helper()
	return queryOne[int](t, s, `SELECT epoch FROM host_messages WHERE run_id = $1 AND type = 'assign' ORDER BY id DESC LIMIT 1`, run)
}

// An assignment committed after a pass planned, and a stop and input for
// its Run committed after it, all wait for the next pass, which sends them
// in order, the assignment with its tokens. A stop sent before its Run's
// assignment would be acked by a runner that ignores it (no placement yet).
// Other Runs' messages go out on the paused pass.
func TestRegistryAssignCommittedDuringAPassHoldsItsRun(t *testing.T) {
	s, _, _, _ := registryFixture(t)
	warm(t, s, "acme")
	var r1 string
	first := passPausedAfterPlan(t, s, func() {
		r1 = submitImage(t, s, "t2", ecrImage("acme"))
		epoch := assignEpoch(t, s, r1)
		queue(t, s, r1, epoch, proto.MsgStop)
		queue(t, s, r1, epoch, proto.MsgInput)
		queue(t, s, "run_other", 1, proto.MsgStop)
	})
	requireKeys(t, "the paused pass", first, proto.MsgStop+"/run_other")
	next := pass(t, s)
	requireKeys(t, "the next pass", next, proto.MsgAssign+"/"+r1, proto.MsgStop+"/"+r1, proto.MsgInput+"/"+r1)
	if g := registries(registryAssign(t, next, r1)); !slices.Equal(g, []string{testRegistryHost + "/acme"}) {
		t.Fatalf("assignment carried %v", g)
	}
	requireKeys(t, "a third pass", pass(t, s))
}

// A reconnect that makes an assignment undelivered again after a pass
// planned (so the plan never saw it) holds it and its Run's later
// messages for that pass; the next one sends them in order.
func TestRegistryRedeliveryDuringAPassHoldsItsRun(t *testing.T) {
	s, ctx, _, _ := registryFixture(t)
	warm(t, s, "acme")
	r1 := submitImage(t, s, "t2", ecrImage("acme"))
	requireKeys(t, "first delivery", pass(t, s), proto.MsgAssign+"/"+r1)
	epoch := assignEpoch(t, s, r1)
	first := passPausedAfterPlan(t, s, func() {
		execSQL(t, s, ctx, `UPDATE host_messages SET delivered_at = NULL WHERE host_id = 'h1' AND acked_at IS NULL`)
		queue(t, s, r1, epoch, proto.MsgStop)
		queue(t, s, "run_other", 1, proto.MsgStop)
	})
	requireKeys(t, "the paused pass", first, proto.MsgStop+"/run_other")
	next := pass(t, s)
	requireKeys(t, "the next pass", next, proto.MsgAssign+"/"+r1, proto.MsgStop+"/"+r1)
	if g := registries(registryAssign(t, next, r1)); !slices.Equal(g, []string{testRegistryHost + "/acme"}) {
		t.Fatalf("replayed assignment carried %v", g)
	}
	requireKeys(t, "a third pass", pass(t, s))
}

// A held assignment and a later message of its Run stay back, pass after
// pass, while another Run's stop goes out once. Once the token is minted
// the assignment goes, then its Run's message, each once on this
// connection; a reconnect replays both, same ids and epoch, in order.
func TestRegistryHeldRunKeepsItsOrder(t *testing.T) {
	s, ctx, fake, _ := registryFixture(t)
	fake.entered = make(chan string, 4)
	release := fake.block("acme")
	r1 := submitImage(t, s, "t2", ecrImage("acme"))
	epoch := assignEpoch(t, s, r1)
	queue(t, s, r1, epoch, proto.MsgInput)
	queue(t, s, "run_other", 1, proto.MsgStop)
	requireKeys(t, "while held", pass(t, s), proto.MsgStop+"/run_other")
	select {
	case <-fake.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("acme's mint did not start")
	}
	for i := range 3 {
		requireKeys(t, fmt.Sprintf("held, pass %d", i), pass(t, s))
	}
	close(release)
	warm(t, s, "acme")
	released := pass(t, s)
	requireKeys(t, "after the mint", released, proto.MsgAssign+"/"+r1, proto.MsgInput+"/"+r1)
	a := registryAssign(t, released, r1)
	if g := registries(a); !slices.Equal(g, []string{testRegistryHost + "/acme"}) || a.Epoch != epoch {
		t.Fatalf("assignment carried %v at epoch %d (want %d)", g, a.Epoch, epoch)
	}
	requireKeys(t, "once per connection", pass(t, s))

	execSQL(t, s, ctx, `UPDATE host_messages SET delivered_at = NULL WHERE host_id = 'h1' AND acked_at IS NULL`)
	replay := pass(t, s)
	requireKeys(t, "a reconnect's replay", replay, proto.MsgAssign+"/"+r1, proto.MsgInput+"/"+r1, proto.MsgStop+"/run_other")
	for i := range replay[:2] {
		if replay[i].ID != released[i].ID || replay[i].Epoch != released[i].Epoch {
			t.Fatalf("replayed %d/%d, first sent %d/%d", replay[i].ID, replay[i].Epoch, released[i].ID, released[i].Epoch)
		}
	}
	if g := registries(registryAssign(t, replay, r1)); !slices.Equal(g, []string{testRegistryHost + "/acme"}) {
		t.Fatalf("replayed assignment carried %v", g)
	}
}

// A Run requeued while its assignment is held for a mint: the old
// epoch's assignment is never sent; the new one waits on its own, and its
// later stop behind it; both go once the token is minted.
func TestRegistryRequeueWhileHeld(t *testing.T) {
	s, ctx, fake, _ := registryFixture(t)
	release := fake.block("acme")
	r1 := submitImage(t, s, "t2", ecrImage("acme"))
	requireKeys(t, "held", pass(t, s))
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var later laterEvents
		if err := s.requeueUnstartedPlacement(ctx, tx, "t2", r1, 1, "requeued by the test", &later, proto.Assign{}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE host_messages SET acked_at = now() WHERE run_id = $1 AND epoch = 1`, r1); err != nil {
			return err
		}
		return later.write()
	}); err != nil {
		t.Fatal(err)
	}
	schedule(t, s)
	epoch := assignEpoch(t, s, r1)
	if epoch != 2 {
		t.Fatalf("requeued at epoch %d, want 2", epoch)
	}
	queue(t, s, r1, epoch, proto.MsgStop)
	requireKeys(t, "the new epoch, held", pass(t, s))
	close(release)
	warm(t, s, "acme")
	next := pass(t, s)
	requireKeys(t, "after the mint", next, proto.MsgAssign+"/"+r1, proto.MsgStop+"/"+r1)
	if a := registryAssign(t, next, r1); a.Epoch != 2 || len(a.RegistryCredentials) != 1 {
		t.Fatalf("sent epoch %d with %v", a.Epoch, registries(a))
	}
	requireKeys(t, "once per connection", pass(t, s))
}

// The capability is checked again by the pass's UPDATE: an assignment
// planned with tokens for a host that dropped the capability since is
// not sent with them, nor one planned without for a host that gained it.
// A host that dropped it while an assignment was held gets it at once,
// without tokens.
func TestRegistryCapabilityChangeDuringAPass(t *testing.T) {
	t.Run("dropped between plan and update", func(t *testing.T) {
		s, ctx, _, _ := registryFixture(t)
		warm(t, s, "acme")
		r1 := submitImage(t, s, "t2", ecrImage("acme"))
		first := passPausedAfterPlan(t, s, func() {
			execSQL(t, s, ctx, `UPDATE hosts SET capabilities = '{}' WHERE id = 'h1'`)
			queue(t, s, r1, assignEpoch(t, s, r1), proto.MsgStop)
			queue(t, s, "run_other", 1, proto.MsgStop)
		})
		requireKeys(t, "the paused pass", first, proto.MsgStop+"/run_other")
		next := pass(t, s)
		requireKeys(t, "the next pass", next, proto.MsgAssign+"/"+r1, proto.MsgStop+"/"+r1)
		if a := registryAssign(t, next, r1); a.RegistryCredentials != nil {
			t.Fatalf("tokens sent to a host without %s: %v", proto.CapRegistryMinted, registries(a))
		}
	})
	t.Run("gained between plan and update", func(t *testing.T) {
		s, ctx, _, _ := registryFixture(t)
		warm(t, s, "acme")
		execSQL(t, s, ctx, `UPDATE hosts SET capabilities = '{}' WHERE id = 'h1'`)
		r1 := submitImage(t, s, "t2", ecrImage("acme"))
		first := passPausedAfterPlan(t, s, func() {
			execSQL(t, s, ctx, `UPDATE hosts SET capabilities = ARRAY[$1] WHERE id = 'h1'`, proto.CapRegistryMinted)
		})
		requireKeys(t, "the paused pass", first)
		next := pass(t, s)
		if g := registries(registryAssign(t, next, r1)); !slices.Equal(g, []string{testRegistryHost + "/acme"}) {
			t.Fatalf("assignment carried %v", g)
		}
	})
	t.Run("dropped while held", func(t *testing.T) {
		s, ctx, fake, _ := registryFixture(t)
		defer close(fake.block("acme"))
		r1 := submitImage(t, s, "t2", ecrImage("acme"))
		requireKeys(t, "held", pass(t, s))
		execSQL(t, s, ctx, `UPDATE hosts SET capabilities = '{}' WHERE id = 'h1'`)
		next := pass(t, s)
		requireKeys(t, "without the capability", next, proto.MsgAssign+"/"+r1)
		if a := registryAssign(t, next, r1); a.RegistryCredentials != nil {
			t.Fatalf("tokens sent to a host without %s", proto.CapRegistryMinted)
		}
		requireKeys(t, "once per connection", pass(t, s))
	})
}

// While a mint is blocked, the host's WebSocket still carries live frames
// and unrelated durable messages; the cold assignment goes once its hold
// ends, with the tokens that are ready, once. The hold's end is driven by
// the test (a fixed clock and its timer fired by hand), so no deadline
// depends on how fast the machine is; the timeouts are watchdogs only.
func TestRegistryMintDoesNotBlockDelivery(t *testing.T) {
	s, ctx, fake, _ := registryFixture(t)
	fake.entered = make(chan string, 4)
	release := fake.block("acme")
	defer close(release)
	raw := ids.Secret("luxh")
	execSQL(t, s, ctx, `INSERT INTO host_tokens (id, pool_id, token_hash) VALUES ('tok1', $1, $2)`, poolID(t, s, "default", ""), ids.Hash(raw))
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	wctx, cancel := context.WithTimeout(ctx, 60*time.Second) // a watchdog, not a bound under test
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
	frames := make(chan proto.Frame, 64)
	go func() {
		for {
			var f proto.Frame
			if err := readFrame(wctx, ws, &f); err != nil {
				return
			}
			frames <- f
		}
	}()
	// seen is every frame received, in order: none is discarded.
	var seen []proto.Frame
	collect := func(what string, done func() bool) {
		t.Helper()
		for !done() {
			select {
			case f := <-frames:
				seen = append(seen, f)
			case <-wctx.Done():
				t.Fatalf("watchdog: no %s; received %v", what, keys(seen))
			}
		}
	}
	has := func(typ, run string) func() bool {
		return func() bool {
			return slices.ContainsFunc(seen, func(f proto.Frame) bool { return f.Type == typ && f.RunID == run })
		}
	}
	count := func(typ, run string) int {
		n := 0
		for _, f := range seen {
			if f.Type == typ && f.RunID == run {
				n++
			}
		}
		return n
	}
	collect("welcome", has(proto.MsgWelcome, ""))

	r1 := submitImage(t, s, "t1", ecrBuild("acme", "team"))
	for range 2 {
		select {
		case <-fake.entered:
		case <-wctx.Done():
			t.Fatal("watchdog: the mints did not start")
		}
	}
	// acme's AssumeRole is blocked, and the logical clock stands still, so
	// the hold cannot end. A stop for another Run, and a live frame, go out.
	queue(t, s, "run_other", 1, proto.MsgStop)
	s.hub.Notify("h1")
	if err := s.hub.SendLive("h1", proto.Frame{Type: proto.MsgOutputCancel, Data: []byte(`{"subId":"x"}`)}); err != nil {
		t.Fatal(err)
	}
	collect("unrelated stop and live frame", func() bool {
		return has(proto.MsgStop, "run_other")() && has(proto.MsgOutputCancel, "")()
	})
	if n := count(proto.MsgAssign, r1); n != 0 {
		t.Fatalf("the held assignment went out before its hold ended: %v", keys(seen))
	}

	// team's token is ready; the hold ends: the assignment goes with it.
	warm(t, s, "team")
	fake.clock.advance(registryHold)
	if n := fake.timers.fire(); n != 1 {
		t.Fatalf("%d hold timers fired, want r1's", n)
	}
	collect("assignment", has(proto.MsgAssign, r1))
	as := registryAssign(t, seen, r1)
	if g := registries(as); !slices.Equal(g, []string{testRegistryHost + "/team"}) {
		t.Fatalf("assignment carried %v, want team's only (acme still minting)", g)
	}
	// Later passes on this connection do not send it again.
	queue(t, s, "run_last", 1, proto.MsgStop)
	s.hub.Notify("h1")
	collect("a later stop", has(proto.MsgStop, "run_last"))
	if n := count(proto.MsgAssign, r1); n != 1 {
		t.Fatalf("%d assignments of r1 on one connection: %v", n, keys(seen))
	}
}

// The hold timer is a real one in production: an assignment's host gets a
// pass when its hold ends, with no mint finished. Real time, generous
// bound.
func TestRegistryHoldTimerFires(t *testing.T) {
	m := newTestMinter(time.Now, RegistryGrant{Owner: "acme", Tenants: []string{"*"}})
	m.hold = 50 * time.Millisecond
	woken := make(chan string, 1)
	m.ready = func(h string) {
		select {
		case woken <- h:
		default:
		}
	}
	closeMinter(t, m)
	defer close(fakeOf(m).block("acme"))
	if _, pending, _ := m.lookup(context.Background(), "h1", 1, []string{"acme"}); !pending {
		t.Fatal("not held")
	}
	select {
	case h := <-woken:
		if h != "h1" {
			t.Fatalf("woke %s", h)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no pass when the hold ended")
	}
}

// Goroutine leak check: in a synctest bubble, with the production timers
// and clock, every mint (one blocked for good, one done) and hold timer
// started by lookups is gone once close returns. A goroutine left behind
// deadlocks the bubble, which fails the test.
func TestRegistryCloseLeavesNoGoroutines(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newTestMinter(time.Now, RegistryGrant{Owner: "stuck", Tenants: []string{"*"}}, RegistryGrant{Owner: "quick", Tenants: []string{"*"}})
		var woken atomic.Int32
		m.ready = func(string) { woken.Add(1) }
		fakeOf(m).block("stuck") // never released
		for id := range int64(3) {
			m.lookup(context.Background(), "h1", id, m.ownersFor("t1"))
		}
		synctest.Wait()
		if creds, _, _ := m.lookup(context.Background(), "h1", 9, []string{"quick"}); len(creds) != 1 {
			t.Fatalf("quick's token: %v", creds)
		}
		m.close()
		synctest.Wait()
		n := woken.Load()
		time.Sleep(2 * registryHold) // no hold timer fires after close
		synctest.Wait()
		if woken.Load() != n {
			t.Fatal("a hold timer fired after close")
		}
	})
}

// BenchmarkRegistryLookupHeld: lookups while n assignments are held on a
// blocked owner, on a fixed clock. populate: each op holds n new
// assignments; sweep: each op looks all n up again (a delivery pass);
// warm: a warm owner's lookup, in parallel, while n are held.
func BenchmarkRegistryLookupHeld(b *testing.B) {
	for _, n := range []int{100, 1_000, 10_000} {
		setup := func(b *testing.B) (*registryMinter, minted) {
			m, fake, _, done := minter(b, RegistryGrant{Owner: "cold", Tenants: []string{"*"}}, RegistryGrant{Owner: "warm", Tenants: []string{"*"}})
			release := fake.block("cold")
			b.Cleanup(func() { close(release) })
			return m, done
		}
		b.Run(fmt.Sprintf("populate/%d", n), func(b *testing.B) {
			m, _ := setup(b)
			base := int64(0)
			b.ReportAllocs()
			for b.Loop() {
				for i := range int64(n) {
					m.lookup(context.Background(), "h1", base+i, []string{"cold"})
				}
				base += int64(n)
			}
		})
		b.Run(fmt.Sprintf("sweep/%d", n), func(b *testing.B) {
			m, _ := setup(b)
			for i := range int64(n) {
				m.lookup(context.Background(), "h1", i, []string{"cold"})
			}
			b.ReportAllocs()
			for b.Loop() {
				for i := range int64(n) {
					m.lookup(context.Background(), "h1", i, []string{"cold"})
				}
			}
		})
		b.Run(fmt.Sprintf("warm/%d", n), func(b *testing.B) {
			m, done := setup(b)
			for i := range int64(n) {
				m.lookup(context.Background(), "h1", i, []string{"cold"})
			}
			for {
				if creds, _, _ := m.lookup(context.Background(), "h2", -1, []string{"warm"}); len(creds) == 1 {
					break
				}
				<-done
			}
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					m.lookup(context.Background(), "h2", -1, []string{"warm"})
				}
			})
		})
	}
}
