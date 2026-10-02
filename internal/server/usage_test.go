package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// An exit status carrying usage above 2^31-1 (bytes) and fractional CPU
// seconds ends the placement, and every value is stored exactly.
func TestExitStatusRecordsLargeUsage(t *testing.T) {
	const gib = int64(1) << 30
	usage := proto.Usage{
		PeakMemoryBytes: 3 * gib,
		PeakDiskBytes:   5 * gib,
		PeakPids:        412,
		CPUSeconds:      1.5,
		NetRxBytes:      6 * gib,
		NetTxBytes:      7 * gib,
	}
	for _, c := range []struct {
		status, runState string
	}{
		{"exited", StateStopped},
		// A runner that could not finish the placement cleanly: still a
		// requested stop, so the Run is stopped, and its usage is kept.
		{"failed", StateStopped},
	} {
		t.Run(c.status, func(t *testing.T) {
			s := testServer(t)
			ctx := context.Background()
			key := serversFixture(t, s, ctx)
			if w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/stop", nil); w.Code/100 != 2 {
				t.Fatalf("stop: %d %s", w.Code, w.Body)
			}
			report := func(st proto.Status) error {
				return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
					return s.applyStatus(ctx, tx, "t1", r1, 1, st)
				})
			}
			if err := report(proto.Status{State: "stopping"}); err != nil {
				t.Fatal(err)
			}
			if pl, run := usageStates(t, s, ctx); pl != "stopping" || run != StateStopping {
				t.Fatalf("before the exit: placement %s, run %s", pl, run)
			}

			code := 137
			u := usage
			st := proto.Status{State: c.status, ExitCode: &code, Reason: "stopped", Usage: &u, OutputSeq: 3 * gib}
			if c.status == "failed" {
				st.ExitCode, st.Message = nil, "container lost"
			}
			if err := report(st); err != nil {
				t.Fatalf("exit status: %v", err)
			}
			if pl, run := usageStates(t, s, ctx); pl != "exited" || run != c.runState {
				t.Fatalf("after the exit: placement %s, run %s; want exited, %s", pl, run, c.runState)
			}
			var got proto.Usage
			var outputSeq int64
			err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT peak_memory_bytes, peak_disk_bytes, peak_pids, cpu_seconds, net_rx_bytes, net_tx_bytes, output_seq
					FROM placements WHERE run_id = $1 AND epoch = 1`, r1).Scan(
					&got.PeakMemoryBytes, &got.PeakDiskBytes, &got.PeakPids, &got.CPUSeconds, &got.NetRxBytes, &got.NetTxBytes, &outputSeq)
			})
			if err != nil {
				t.Fatal(err)
			}
			if got != usage {
				t.Fatalf("stored usage %+v, want %+v", got, usage)
			}
			if outputSeq != 3*gib {
				t.Fatalf("stored output_seq %d, want %d", outputSeq, 3*gib)
			}
		})
	}
}

// A snapshot's output sequence above 2^31-1 is stored exactly.
func TestSnapshotRecordsLargeOutputSeq(t *testing.T) {
	s, ctx := reportFixture(t)
	sd := snapshotB("snapB", 1)
	sd.OutputSeq = 3 << 30
	if f := reportSnapshot(t, s, "hb", "rb", 1, sd); f.Type != proto.MsgAck || ackRefused(t, f) {
		t.Fatalf("report: %s %s", f.Type, f.Data)
	}
	var got int64
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT output_seq FROM placements WHERE run_id = 'rb' AND epoch = 1`).Scan(&got)
	})
	if err != nil || got != sd.OutputSeq {
		t.Fatalf("stored output_seq %d (%v), want %d", got, err, sd.OutputSeq)
	}
}

func usageStates(t *testing.T, s *Server, ctx context.Context) (placement, run string) {
	t.Helper()
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT p.state, r.state FROM placements p JOIN runs r ON r.id = p.run_id
			WHERE p.run_id = $1 AND p.epoch = 1`, r1).Scan(&placement, &run)
	})
	if err != nil {
		t.Fatal(err)
	}
	return placement, run
}
