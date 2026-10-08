package runner

import (
	"context"
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

// fenceEpoch1 has epoch 1 send a report and luxd nack it stale.
func (f *finishFixture) fenceEpoch1(t *testing.T) {
	t.Helper()
	f.p.event(context.Background(), "test.ping", nil)
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		n := len(f.held)
		f.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("epoch 1 sent no report")
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.nackHeld()
}

// The stale nack of an older epoch still exporting here kills only the
// container that epoch made: never the Run's name, which the next epoch's
// container holds.
func TestStaleEpochDoesNotKillTheNextEpochsContainer(t *testing.T) {
	f := newOverlapFixture(t)
	f.r.assign(context.Background(), proto.Assign{RunID: "run1", TenantID: "t1", Epoch: 2})
	// Epoch 2's container, under the Run's name.
	if err := os.WriteFile(f.bin+".ctr", []byte("ctr-2"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.fenceEpoch1(t)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(f.bin + ".killed")
		for _, target := range strings.Fields(string(b)) {
			if target == containerName("run1") || target == "ctr-2" {
				log, _ := os.ReadFile(f.bin + ".log")
				t.Fatalf("the fenced epoch 1 killed %q; podman:\n%s", target, log)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
}
