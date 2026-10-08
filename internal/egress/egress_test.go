package egress

import (
	"context"
	"fmt"
	"maps"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/spec"
	"golang.org/x/net/dns/dnsmessage"
)

// fakeNet stands in for the system resolver and nft: every name resolves
// to one address, and "add element" lines go into per-set contents, which
// is what the Run's traffic would be checked against.
type fakeNet struct {
	mu   sync.Mutex
	sets map[string]map[string]bool
	addr map[string]netip.Addr
	nx   map[string]bool // names that do not resolve
	fail bool            // nft fails
}

var addElement = regexp.MustCompile(`^add element inet lux (\S+) \{ (.*) \}$`)

func (n *fakeNet) nft(script string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.fail {
		return fmt.Errorf("nft failed")
	}
	for line := range strings.SplitSeq(strings.TrimSpace(script), "\n") {
		if m := addElement.FindStringSubmatch(line); m != nil {
			if n.sets[m[1]] == nil {
				n.sets[m[1]] = map[string]bool{}
			}
			for a := range strings.SplitSeq(m[2], ", ") {
				n.sets[m[1]][a] = true
			}
		}
	}
	return nil
}

func (n *fakeNet) lookup(_ context.Context, host string) ([]netip.Addr, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.nx[host] {
		return nil, fmt.Errorf("lookup %s: no such host", host)
	}
	a, ok := n.addr[host]
	if !ok {
		a = netip.AddrFrom4([4]byte{203, 0, 113, byte(len(n.addr) + 1)})
		n.addr[host] = a
	}
	return []netip.Addr{a}, nil
}

func (n *fakeNet) inSet(iface string, a netip.Addr) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.sets[setName(iface)][a.String()]
}

// fixture is a Firewall with Runs installed as Apply does, without the
// stub's sockets or the chain's nft script.
func fixture(t *testing.T) (*Firewall, *fakeNet, func(iface string, unrestricted bool, rules ...spec.EgressRule) *[]Lookup) {
	return fixtureBlocking(t, nil)
}

// fixtureBlocking is fixture with extra hard-blocked prefixes, as New's
// control plane.
func fixtureBlocking(t *testing.T, extra []netip.Prefix) (*Firewall, *fakeNet, func(iface string, unrestricted bool, rules ...spec.EgressRule) *[]Lookup) {
	n := &fakeNet{sets: map[string]map[string]bool{}, addr: map[string]netip.Addr{}, nx: map[string]bool{}}
	f := newFirewall(extra, n.lookup, n.nft)
	add := func(iface string, unrestricted bool, rules ...spec.EgressRule) *[]Lookup {
		var mu sync.Mutex
		var got []Lookup
		r, err := newRun(unrestricted, rules, func(l Lookup) {
			mu.Lock()
			got = append(got, l)
			mu.Unlock()
		})
		if err != nil {
			t.Fatal(err)
		}
		f.install(context.Background(), iface, r)
		return &got
	}
	return f, n, add
}

// ask sends an A query through the stub's handler: the RCode and answers.
func ask(t *testing.T, f *Firewall, iface, name string) (dnsmessage.RCode, []netip.Addr) {
	t.Helper()
	return askType(t, f, iface, name, dnsmessage.TypeA)
}

func askType(t *testing.T, f *Firewall, iface, name string, typ dnsmessage.Type) (dnsmessage.RCode, []netip.Addr) {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 7, RecursionDesired: true})
	_ = b.StartQuestions()
	_ = b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name + "."), Type: typ, Class: dnsmessage.ClassINET})
	q, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	var m dnsmessage.Message
	if err := m.Unpack(answer(q, f, iface)); err != nil {
		t.Fatal(err)
	}
	var out []netip.Addr
	for _, a := range m.Answers {
		out = append(out, netip.AddrFrom4(a.Body.(*dnsmessage.AResource).A))
	}
	return m.RCode, out
}

func TestWildcardAdmitsMatchingNames(t *testing.T) {
	f, n, add := fixture(t)
	events := add("lux1", false, spec.EgressRule{Host: "*.wild.example.com"}, spec.EgressRule{Host: "exact.example.com"})
	for _, name := range []string{"a.wild.example.com", "B.c.Wild.example.com"} {
		rc, addrs := ask(t, f, "lux1", name)
		if rc != dnsmessage.RCodeSuccess || len(addrs) != 1 {
			t.Fatalf("%s: %v %v", name, rc, addrs)
		}
		if !n.inSet("lux1", addrs[0]) {
			t.Fatalf("%s: answered %v, which the Run's set does not hold", name, addrs[0])
		}
	}
	for _, name := range []string{"wild.example.com", "x.other.example.com", "evilwild.example.com"} {
		if rc, addrs := ask(t, f, "lux1", name); rc != dnsmessage.RCodeRefused || addrs != nil {
			t.Fatalf("%s: %v %v", name, rc, addrs)
		}
	}
	allowed := map[string]bool{}
	for _, l := range *events {
		allowed[l.Name] = l.Allowed
	}
	want := map[string]bool{"a.wild.example.com": true, "b.c.wild.example.com": true, "wild.example.com": false, "x.other.example.com": false, "evilwild.example.com": false}
	if fmt.Sprint(allowed) != fmt.Sprint(want) {
		t.Fatalf("events %v, want %v", allowed, want)
	}
}

// A name already resolved for another Run is answered from what is known,
// so admission must put those addresses in this Run's set too.
func TestWildcardAdmissionAddsKnownAddresses(t *testing.T) {
	f, n, add := fixture(t)
	add("lux1", false, spec.EgressRule{Host: "a.wild.example.com"})
	add("lux2", false, spec.EgressRule{Host: "*.wild.example.com"})
	_, addrs := ask(t, f, "lux2", "a.wild.example.com")
	if len(addrs) != 1 || !n.inSet("lux2", addrs[0]) {
		t.Fatalf("answered %v; lux2's set %v", addrs, n.sets[setName("lux2")])
	}
}

func TestWildcardAdmissionFailsClosedWhenNftFails(t *testing.T) {
	f, n, add := fixture(t)
	add("lux1", false, spec.EgressRule{Host: "a.wild.example.com"})
	add("lux2", false, spec.EgressRule{Host: "*.wild.example.com"})
	n.fail = true
	if rc, addrs := ask(t, f, "lux2", "a.wild.example.com"); rc == dnsmessage.RCodeSuccess && len(addrs) > 0 {
		t.Fatalf("answered %v with no set update", addrs)
	}
	n.fail = false
	if _, addrs := ask(t, f, "lux2", "a.wild.example.com"); len(addrs) != 1 || !n.inSet("lux2", addrs[0]) {
		t.Fatalf("retry: %v", addrs)
	}
}

func TestWildcardCap(t *testing.T) {
	f, _, add := fixture(t)
	events := add("lux1", false, spec.EgressRule{Host: "*.wild.example.com"}, spec.EgressRule{Host: "exact.example.com"})
	for i := range maxWildcardNames {
		if rc, _ := ask(t, f, "lux1", fmt.Sprintf("n%d.wild.example.com", i)); rc != dnsmessage.RCodeSuccess {
			t.Fatalf("name %d: %v", i, rc)
		}
	}
	if rc, _ := ask(t, f, "lux1", "over.wild.example.com"); rc != dnsmessage.RCodeRefused {
		t.Fatalf("over the cap: %v", rc)
	}
	// Admitted names and exact rules still answer.
	for _, name := range []string{"n0.wild.example.com", "exact.example.com"} {
		if rc, addrs := ask(t, f, "lux1", name); rc != dnsmessage.RCodeSuccess || len(addrs) != 1 {
			t.Fatalf("%s: %v %v", name, rc, addrs)
		}
	}
	allowed := map[string]bool{}
	for _, l := range *events {
		allowed[l.Name] = l.Allowed
	}
	if a, ok := allowed["over.wild.example.com"]; !ok || a {
		t.Fatalf("over-cap event: reported %v, allowed %v", ok, a)
	}
	// The cap is per Run.
	add("lux2", false, spec.EgressRule{Host: "*.wild.example.com"})
	if rc, _ := ask(t, f, "lux2", "over.wild.example.com"); rc != dnsmessage.RCodeSuccess {
		t.Fatalf("another Run: %v", rc)
	}
}

// Exact rules do not use up the wildcard cap.
func TestExactRulesDoNotCountTowardTheCap(t *testing.T) {
	f, _, add := fixture(t)
	rules := []spec.EgressRule{{Host: "*.wild.example.com"}}
	for i := range maxWildcardNames {
		rules = append(rules, spec.EgressRule{Host: fmt.Sprintf("e%d.example.com", i)})
	}
	add("lux1", false, rules...)
	if rc, _ := ask(t, f, "lux1", "a.wild.example.com"); rc != dnsmessage.RCodeSuccess {
		t.Fatalf("got %v", rc)
	}
}

// The refresh loop resolves every Run's hosts, which admitted names join.
func TestAdmittedNamesAreRefreshed(t *testing.T) {
	f, n, add := fixture(t)
	add("lux1", false, spec.EgressRule{Host: "*.wild.example.com"})
	ask(t, f, "lux1", "a.wild.example.com")
	moved := netip.MustParseAddr("198.51.100.9")
	n.mu.Lock()
	n.addr["a.wild.example.com"] = moved
	n.mu.Unlock()
	f.refresh(context.Background())
	if !n.inSet("lux1", moved) {
		t.Fatal("the new address was not added")
	}
}

// A refresh resolves names concurrently, at most refreshWorkers at once.
func TestRefreshIsBoundedConcurrent(t *testing.T) {
	var mu sync.Mutex
	inFlight, peak, over := 0, 0, 0
	gate := make(chan struct{})
	var open sync.Once
	lookup := func(ctx context.Context, _ string) ([]netip.Addr, error) {
		mu.Lock()
		inFlight++
		peak = max(peak, inFlight)
		if inFlight > refreshWorkers {
			over = max(over, inFlight)
		}
		if inFlight == refreshWorkers {
			// Hold the gate a little longer, so that an unbounded refresh
			// starts a lookup beyond the bound before any returns.
			open.Do(func() { time.AfterFunc(20*time.Millisecond, func() { close(gate) }) })
		}
		mu.Unlock()
		// A serial refresh never reaches refreshWorkers: the timeout lets
		// it finish and fail rather than hang.
		select {
		case <-gate:
		case <-time.After(100 * time.Millisecond):
		}
		mu.Lock()
		inFlight--
		mu.Unlock()
		return nil, fmt.Errorf("no such host")
	}
	f := newFirewall(nil, lookup, func(string) error { return nil })
	r, err := newRun(false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 * refreshWorkers {
		r.hosts[fmt.Sprintf("h%d.example.com", i)] = true
	}
	f.runs["lux1"] = r
	f.refresh(context.Background())
	if over > 0 {
		t.Fatalf("%d lookups at once, want at most %d", over, refreshWorkers)
	}
	if peak <= 1 {
		t.Fatalf("%d lookups at once, want 2..%d", peak, refreshWorkers)
	}
}

// Removing a Run drops the resolved addresses of the names no other Run
// allows, and keeps those another Run still does.
func TestRemovePrunesResolvedNames(t *testing.T) {
	f, _, add := fixture(t)
	add("lux1", false, spec.EgressRule{Host: "shared.example.com"}, spec.EgressRule{Host: "one.example.com"})
	add("lux2", false, spec.EgressRule{Host: "shared.example.com"}, spec.EgressRule{Host: "two.example.com"})
	resolved := func() []string {
		f.mu.Lock()
		defer f.mu.Unlock()
		return slices.Sorted(maps.Keys(f.resolved))
	}
	f.Remove("lux1")
	if got := resolved(); !slices.Equal(got, []string{"shared.example.com", "two.example.com"}) {
		t.Fatalf("after removing lux1: %v", got)
	}
	f.Remove("lux2")
	if got := resolved(); len(got) != 0 {
		t.Fatalf("after removing both: %v", got)
	}
}

// A lookup in flight when its Run is removed stores nothing for a name no
// remaining Run allows.
func TestRemoveDuringRefreshStoresNothing(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	lookup := func(ctx context.Context, _ string) ([]netip.Addr, error) {
		close(started)
		<-release
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}
	f := newFirewall(nil, lookup, func(string) error { return nil })
	r, err := newRun(false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.hosts["gone.example.com"] = true
	f.runs["lux1"] = r
	done := make(chan struct{})
	go func() {
		f.refresh(context.Background())
		close(done)
	}()
	<-started
	f.Remove("lux1")
	close(release)
	<-done
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.resolved) != 0 {
		t.Fatalf("resolved %v", f.resolved)
	}
}

func TestUnrestrictedRunAdmitsNothing(t *testing.T) {
	f, _, add := fixture(t)
	add("lux1", true, spec.EgressRule{Host: "*.wild.example.com"})
	if rc, addrs := ask(t, f, "lux1", "a.wild.example.com"); rc != dnsmessage.RCodeRefused || addrs != nil {
		t.Fatalf("%v %v", rc, addrs)
	}
}

// A non-A query for a name a wildcard matches is answered as an allowed
// name's (NOERROR, no answers) but neither admits nor reports it: only an
// A query spends a cap slot.
func TestNonAQueryDoesNotAdmit(t *testing.T) {
	f, _, add := fixture(t)
	events := add("lux1", false, spec.EgressRule{Host: "*.wild.example.com"})
	rc, addrs := askType(t, f, "lux1", "a.wild.example.com", dnsmessage.TypeAAAA)
	if rc != dnsmessage.RCodeSuccess || addrs != nil {
		t.Fatalf("AAAA: %v %v", rc, addrs)
	}
	f.mu.Lock()
	admitted := f.runs["lux1"].admitted
	f.mu.Unlock()
	if admitted != 0 || len(*events) != 0 {
		t.Fatalf("admitted %d, events %v", admitted, *events)
	}
	if rc, addrs := ask(t, f, "lux1", "a.wild.example.com"); rc != dnsmessage.RCodeSuccess || len(addrs) != 1 {
		t.Fatalf("A after AAAA: %v %v", rc, addrs)
	}
	if rc, _ := askType(t, f, "lux1", "x.other.example.com", dnsmessage.TypeAAAA); rc != dnsmessage.RCodeRefused {
		t.Fatalf("AAAA of a name no rule matches: %v", rc)
	}
}

// A name under a wildcard that resolves into a hard-blocked range (the
// metadata service, or the control plane passed to New) is not opened.
func TestWildcardNameCannotOpenABlockedAddress(t *testing.T) {
	controlPlane := netip.MustParsePrefix("10.9.0.0/16")
	for name, addr := range map[string]netip.Addr{
		"meta.wild.example.com": netip.MustParseAddr("169.254.169.254"),
		"cp.wild.example.com":   netip.MustParseAddr("10.9.0.5"),
	} {
		f, n, add := fixtureBlocking(t, []netip.Prefix{controlPlane})
		n.addr[name] = addr
		add("lux1", false, spec.EgressRule{Host: "*.wild.example.com"})
		if rc, addrs := ask(t, f, "lux1", name); rc != dnsmessage.RCodeNameError || addrs != nil {
			t.Errorf("%s: %v %v", name, rc, addrs)
		}
		if n.inSet("lux1", addr) {
			t.Errorf("%s: blocked address %v in the Run's set", name, addr)
		}
	}
}

// The stub's normalisation feeds the matcher a name it rejects: an
// underscore label is not a hostname.
func TestWildcardRefusesInvalidLabels(t *testing.T) {
	f, _, add := fixture(t)
	add("lux1", false, spec.EgressRule{Host: "*.wild.example.com"})
	if rc, addrs := ask(t, f, "lux1", "_x.wild.example.com"); rc != dnsmessage.RCodeRefused || addrs != nil {
		t.Fatalf("%v %v", rc, addrs)
	}
}

// Many concurrent first lookups of distinct names admit exactly the cap.
func TestConcurrentAdmissionHoldsTheCap(t *testing.T) {
	f, _, add := fixture(t)
	add("lux1", false, spec.EgressRule{Host: "*.wild.example.com"})
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for i := range 600 {
		wg.Go(func() {
			if ok, _ := f.answerFor("lux1", fmt.Sprintf("n%d.wild.example.com", i), dnsmessage.TypeA); ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if allowed != maxWildcardNames {
		t.Fatalf("%d names admitted, want %d", allowed, maxWildcardNames)
	}
}

// Two Runs looking up the same new name at once each answer only
// addresses in their own set.
func TestConcurrentSameNameAcrossRuns(t *testing.T) {
	for iter := range 200 {
		f, n, add := fixture(t)
		add("lux1", false, spec.EgressRule{Host: "*.wild.example.com"})
		add("lux2", false, spec.EgressRule{Host: "*.wild.example.com"})
		var wg sync.WaitGroup
		var mu sync.Mutex
		res := map[string][]netip.Addr{}
		for _, iface := range []string{"lux1", "lux2", "lux1", "lux2"} {
			wg.Go(func() {
				_, a := f.answerFor(iface, "a.wild.example.com", dnsmessage.TypeA)
				mu.Lock()
				res[iface] = append(res[iface], a...)
				mu.Unlock()
			})
		}
		wg.Wait()
		for iface, as := range res {
			for _, a := range as {
				if !n.inSet(iface, a) {
					t.Fatalf("iter %d: %s answered %v, not in its set", iter, iface, a)
				}
			}
		}
	}
}

// A slot is spent on admission, whether or not the name resolves.
func TestUnresolvableNameHoldsACapSlot(t *testing.T) {
	f, n, add := fixture(t)
	n.nx["nx.wild.example.com"] = true
	add("lux1", false, spec.EgressRule{Host: "*.wild.example.com"})
	if rc, _ := ask(t, f, "lux1", "nx.wild.example.com"); rc != dnsmessage.RCodeNameError {
		t.Fatalf("nx: %v", rc)
	}
	for i := range maxWildcardNames - 1 {
		if rc, _ := ask(t, f, "lux1", fmt.Sprintf("n%d.wild.example.com", i)); rc != dnsmessage.RCodeSuccess {
			t.Fatalf("name %d: %v", i, rc)
		}
	}
	if rc, _ := ask(t, f, "lux1", "over.wild.example.com"); rc != dnsmessage.RCodeRefused {
		t.Fatalf("over the cap: %v", rc)
	}
}

func TestNewRun(t *testing.T) {
	r, err := newRun(false, []spec.EgressRule{{CIDR: "10.0.0.0/33"}}, nil)
	if err == nil {
		t.Fatalf("bad cidr: got %+v", r)
	}
	r, err = newRun(false, []spec.EgressRule{
		{Host: "*.x.example.com"},
		{Host: "Exact.Example.com."},
		{CIDR: "10.1.2.3/16"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hosts := slices.Sorted(maps.Keys(r.hosts)); !slices.Equal(hosts, []string{"exact.example.com"}) {
		t.Errorf("hosts %v", hosts)
	}
	if !slices.Equal(r.wildcards, []string{".x.example.com"}) {
		t.Errorf("wildcards %v", r.wildcards)
	}
	if !slices.Equal(r.cidrs, []netip.Prefix{netip.MustParsePrefix("10.1.0.0/16")}) {
		t.Errorf("cidrs %v", r.cidrs)
	}
}
