package runner

import (
	"testing"
	"time"
)

// The kill instant is fixed at the first notice's receipt: a retried notice,
// a reconnect or a runner restart (re-reading the stored value) never moves
// it, and it is exactly 15s after that receipt.
func TestReclaimKillAtIsFixedAtFirstReceipt(t *testing.T) {
	first := time.UnixMilli(1_800_000_000_000)
	at := reclaimKillAt(0, first)
	if want := first.Add(15 * time.Second).UnixMilli(); at != want {
		t.Fatalf("first deadline %d, want %d", at, want)
	}
	for _, later := range []time.Duration{time.Second, 14 * time.Second, time.Hour} {
		if got := reclaimKillAt(at, first.Add(later)); got != at {
			t.Fatalf("notice %v later moved the deadline to %d", later, got)
		}
	}
}
