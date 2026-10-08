package runner

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/egress"
	"github.com/marcioapm/lux/internal/proto"
)

// overlapFixture is finishFixture's run1 epoch 1 exporting its snapshot
// (blocked) while luxd, having given up on it, assigns epoch 2 to the same
// host. Epoch 1's reports are held until nackHeld.
func newOverlapFixture(t *testing.T) *finishFixture {
	t.Helper()
	f := newFinishFixture(t)
	f.r.egress = &egress.Firewall{}
	f.mu.Lock()
	f.holdEpoch = 1
	f.mu.Unlock()
	f.exitedAsSupervised(context.Background())
	f.waitFile(t, f.bin+".exporting")
	return f
}

// sendEpoch1Report has epoch 1 send a report, which luxd holds unanswered.
func (f *finishFixture) sendEpoch1Report(t *testing.T) {
	t.Helper()
	f.p.event(context.Background(), "test.ping", nil)
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		n := len(f.held)
		f.mu.Unlock()
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("epoch 1 sent no report")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *finishFixture) podmanLog() string {
	b, _ := os.ReadFile(f.bin + ".log")
	return string(b)
}

// started: epoch 2 has run its first podman command (its network).
func (f *finishFixture) epoch2Started() bool {
	return strings.Contains(f.podmanLog(), "network create")
}

// The stale nack of an older epoch still exporting here (its report was in
// flight when the next epoch was assigned) kills only the container that
// epoch made: never the Run's name, which the next epoch's container holds.
func TestStaleEpochDoesNotKillTheNextEpochsContainer(t *testing.T) {
	f := newOverlapFixture(t)
	f.sendEpoch1Report(t)
	f.r.assign(context.Background(), proto.Assign{RunID: "run1", TenantID: "t1", Epoch: 2})
	// Epoch 2's container, under the Run's name.
	if err := os.WriteFile(f.bin+".ctr", []byte("ctr-2"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.nackHeld()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(f.bin + ".killed")
		for _, target := range strings.Fields(string(b)) {
			if target == containerName("run1") || target == "ctr-2" {
				t.Fatalf("the fenced epoch 1 killed %q; podman:\n%s", target, f.podmanLog())
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	b, _ := os.ReadFile(f.bin + ".killed")
	if string(b) != "ctr-1\n" {
		t.Errorf("killed %q, want epoch 1's own ctr-1", b)
	}
}

// The next epoch assigned while the previous one still exports its
// snapshot here touches nothing of the Run's (no network, volume,
// container or run state) until that one is done; then it starts, and the
// run state is its own, whatever the fenced epoch 1 did meanwhile.
func TestNextEpochWaitsForTheFinishingOne(t *testing.T) {
	f := newOverlapFixture(t)
	f.sendEpoch1Report(t)
	f.r.assign(context.Background(), proto.Assign{RunID: "run1", TenantID: "t1", Epoch: 2})
	f.nackHeld()
	time.Sleep(500 * time.Millisecond)
	if f.epoch2Started() {
		t.Fatalf("epoch 2 started while epoch 1 was exporting; podman:\n%s", f.podmanLog())
	}
	if st, err := readRunState(f.p.dir); err == nil && st.Epoch != 1 {
		t.Fatalf("run state while epoch 1 finishes is epoch %d's, want 1's", st.Epoch)
	}
	f.release()
	if !f.p.waitDone(10 * time.Second) {
		t.Fatal("epoch 1 never ended")
	}
	f.r.mu.Lock()
	p2 := f.r.placements["run1"]
	f.r.mu.Unlock()
	if p2 == nil || p2.epoch != 2 {
		t.Fatalf("the runner holds %+v, want epoch 2", p2)
	}
	if !p2.waitDone(10 * time.Second) {
		t.Fatal("epoch 2 never ended")
	}
	// Epoch 1's export came before anything epoch 2 ran.
	log := f.podmanLog()
	export, network := strings.Index(log, "volume export"), strings.Index(log, "network create")
	if network < 0 || network < export {
		t.Fatalf("epoch 2's network at %d, epoch 1's export at %d: want epoch 2 after; podman:\n%s", network, export, log)
	}
	st, err := readRunState(f.p.dir)
	if err != nil || st.Epoch != 2 || st.Stale {
		t.Fatalf("run state %+v (%v), want epoch 2's, not stale", st, err)
	}
}

// An earlier epoch that never ends here fails the next epoch's start once
// the bounded wait is over, with the reason; the next epoch never ran.
func TestNextEpochFailsWhenThePreviousNeverEnds(t *testing.T) {
	f := newOverlapFixture(t)
	f.r.handover = 300 * time.Millisecond
	f.r.assign(context.Background(), proto.Assign{RunID: "run1", TenantID: "t1", Epoch: 2})
	f.r.mu.Lock()
	p2 := f.r.placements["run1"]
	f.r.mu.Unlock()
	if !p2.waitDone(10 * time.Second) {
		t.Fatal("epoch 2 never ended")
	}
	if f.epoch2Started() {
		t.Errorf("epoch 2 ran beside an epoch 1 still exporting; podman:\n%s", f.podmanLog())
	}
	var got *proto.Status
	f.mu.Lock()
	for _, fr := range f.reports {
		if fr.Type == proto.MsgStatus && fr.Epoch == 2 {
			var st proto.Status
			if err := json.Unmarshal(fr.Data, &st); err == nil {
				got = &st
			}
		}
	}
	f.mu.Unlock()
	if got == nil || got.State != "failed" || !strings.Contains(got.Message, "epoch 1") {
		t.Fatalf("epoch 2's last status %+v, want failed naming epoch 1", got)
	}
	if st, err := readRunState(f.p.dir); err == nil && st.Epoch != 1 {
		t.Errorf("run state is epoch %d's, want epoch 1's left alone", st.Epoch)
	}
	if !f.p.isStale() {
		t.Error("epoch 1 was not fenced off")
	}
}

// A placement fenced off before its container was created (its create
// failed, or never came) kills nothing: not the Run's name, which another
// epoch's container may hold.
func TestKillWithoutAContainerKillsNothing(t *testing.T) {
	f := newFinishFixture(t)
	f.p.mu.Lock()
	f.p.state.Container = ""
	f.p.mu.Unlock()
	f.p.markStale()
	f.p.kill(context.Background())
	f.p.sendStop(context.Background(), "stop")
	time.Sleep(200 * time.Millisecond)
	log, _ := os.ReadFile(f.bin + ".log")
	for _, l := range strings.Split(string(log), "\n") {
		if strings.HasPrefix(l, "kill ") || strings.HasPrefix(l, "stop ") {
			t.Errorf("podman %q for a placement with no container", l)
		}
	}
}
