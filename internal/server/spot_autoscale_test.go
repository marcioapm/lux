package server

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// scaleDownFixture is pool1 (ec2, min 0, no warm) with one ready host h,
// registered an hour ago, holding one running placement of Run "live";
// spot says whether that Run is a spot Run.
func scaleDownFixture(t *testing.T, spot bool) (*Server, poolRow, *planningProvider) {
	t.Helper()
	s, pl, p := planningFixture(t, 0)
	ctx := context.Background()
	s.cfg.ScaleDownAfter = time.Second
	observePlanningHost(t, s, "h", "ready", proto.Capacity{CPUs: 4, Memory: 40, Disk: 40, Runs: 4}, map[string]string{})
	execSQL(t, s, ctx, `UPDATE hosts SET registered_at = now() - interval '1 hour' WHERE id = 'h'`)
	sp := `{"resources":{"cpus":1,"memory":10,"disk":10},"placement":{"pool":"burst"}}`
	if spot {
		sp = `{"spot":true,"resumePolicy":"never","resources":{"cpus":1,"memory":10,"disk":10},"placement":{"pool":"burst"}}`
	}
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, pool_id, spec, state, current_epoch) VALUES ('live', 't1', 'pool1', $1, 'running', 1)`, sp)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources, created_at)
		VALUES ('p', 't1', 'live', 'h', 1, 'running', '{"cpus":1,"memory":10,"disk":10}', now() - interval '1 hour')`)
	p.instances = map[string]Instance{"h": {State: "running", Tags: s.poolTags(pl)}}
	return s, pl, p
}

// A host whose only live placement is spot is idle: scale-down drains it
// and reclaims the spot placement (stop reason spot-reclaim, not a drain
// stop that would resume it). A normal placement keeps its host.
func TestScaleDownTreatsSpotOnlyHostAsIdle(t *testing.T) {
	for _, spot := range []bool{true, false} {
		t.Run(map[bool]string{true: "spot", false: "normal"}[spot], func(t *testing.T) {
			s, pl, p := scaleDownFixture(t, spot)
			planningTick(t, s, pl, p, false)
			state := queryOne[string](t, s, `SELECT state FROM hosts WHERE id = 'h'`)
			stop := queryOne[string](t, s, `SELECT stop_reason FROM placements WHERE id = 'p'`)
			if !spot {
				if state != "ready" || stop != "" {
					t.Fatalf("normal: host %s, stop %q; want ready and untouched", state, stop)
				}
				return
			}
			if state != "draining" || stop != stopSpotReclaim {
				t.Fatalf("spot-only: host %s, stop %q; want draining and %s", state, stop, stopSpotReclaim)
			}
			var typ string
			systemScan(t, s, `SELECT type FROM host_messages WHERE run_id = 'live' ORDER BY id DESC LIMIT 1`, nil, &typ)
			if typ != proto.MsgStop {
				t.Fatalf("message %s, want %s", typ, proto.MsgStop)
			}
			// The runner reports the exit: the Run ends terminated with the
			// reclaim's reason, never resumed; then the host is released.
			code := 143
			f := proto.Frame{Type: proto.MsgStatus, ID: 2, RunID: "live", Epoch: 1,
				Data: proto.Marshal(proto.Status{State: "exited", ExitCode: &code, Reason: "stopped"})}
			if got := s.handleReport(context.Background(), "h", f); got.Type != proto.MsgAck {
				t.Fatalf("exit report: %s %s", got.Type, got.Data)
			}
			var runState, reason string
			systemScan(t, s, `SELECT state, state_reason FROM runs WHERE id = 'live'`, nil, &runState, &reason)
			if runState != StateTerminated || reason != spotReclaimedReason {
				t.Fatalf("spot Run %s %q", runState, reason)
			}
			planningTick(t, s, pl, p, false)
			if state := queryOne[string](t, s, `SELECT state FROM hosts WHERE id = 'h'`); state != "terminated" {
				t.Fatalf("host after the reclaim: %s", state)
			}
		})
	}
}

// warm_while_active: a pool whose only placements are spot is not active,
// and a spot placement's end does not restart its host's idle clock.
func TestSpotPlacementsDoNotKeepPoolActive(t *testing.T) {
	s, pl, _ := scaleDownFixture(t, true)
	ctx := context.Background()
	var st poolState
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error { return s.poolState(ctx, tx, pl, &st) }); err != nil {
		t.Fatal(err)
	}
	if st.active || st.idle != 1 {
		t.Fatalf("active %v idle %d; want inactive with one idle host", st.active, st.idle)
	}
	execSQL(t, s, ctx, `UPDATE runs SET spec = spec - 'spot' WHERE id = 'live'`)
	st = poolState{}
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error { return s.poolState(ctx, tx, pl, &st) }); err != nil {
		t.Fatal(err)
	}
	if !st.active || st.idle != 0 {
		t.Fatalf("normal: active %v idle %d", st.active, st.idle)
	}
}
