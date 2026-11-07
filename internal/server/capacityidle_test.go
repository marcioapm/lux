package server

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// Trace the real app-role connection, not a replacement planner or SQL oracle.
func traceIdlePlanning(t *testing.T, s *Server) *feedRecorder {
	t.Helper()
	cfg := s.db.Pool.Config()
	rec := &feedRecorder{}
	cfg.ConnConfig.Tracer = rec
	db, err := store.OpenConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	orig := s.db
	s.db = db
	t.Cleanup(func() { s.db = orig; db.Close() })
	return rec
}

func TestCapacityPlanIdleFastpath(t *testing.T) {
	for _, private := range []bool{true, false} {
		t.Run(map[bool]string{true: "private", false: "platform"}[private], func(t *testing.T) {
			s, pl, _ := planningFixture(t, 0)
			ctx := context.Background()
			observePlanningHost(t, s, "ready", "ready", proto.Capacity{CPUs: 4}, map[string]string{})
			observePlanningHost(t, s, "starting", "provisioning", proto.Capacity{}, map[string]string{})
			if !private {
				pl.TenantID = nil
				execSQL(t, s, ctx, `UPDATE pools SET tenant_id = NULL WHERE id = 'pool1'`)
				execSQL(t, s, ctx, `UPDATE hosts SET tenant_id = NULL WHERE pool_id = 'pool1'`)
			}
			// A first pass must retract the previous provisioner's blockers too.
			execSQL(t, s, ctx, `INSERT INTO host_events (host_id, tenant_id, type, data)
				SELECT id, tenant_id, $1, '{"pool":"burst","stage":"ready","decision":"blocked"}'
				FROM hosts WHERE pool_id = 'pool1'`, evCapacityDecision)
			rec := traceIdlePlanning(t, s)
			var original capacityPlan
			for _, needExpectation := range []bool{false, true} {
				// Both paths start with the same unswept persisted decisions.
				s.forgetDecisions()
				rec.got = nil
				var plan capacityPlan
				if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
					var err error
					plan, err = s.planCapacity(ctx, tx, pl, map[string]bool{"ready": true}, needExpectation)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				want := map[string]hostDecision{
					"ready":    {Pool: "burst", Stage: "ready", Decision: "idle"},
					"starting": {Pool: "burst", Stage: "starting", Decision: "idle"},
				}
				if !reflect.DeepEqual(plan.hostDecisions, want) || plan.reserved == nil || len(plan.reserved) != 0 {
					t.Fatalf("idle decisions/reservations: %+v", plan)
				}
				expectations, sweeps := 0, 0
				for _, q := range rec.got {
					if strings.Contains(q.sql, "SELECT capacity, labels, registered_at FROM hosts") {
						expectations++
					}
					if strings.Contains(q.sql, "SELECT h.id FROM hosts h") && strings.Contains(q.sql, "host_events") {
						sweeps++
					} else if !needExpectation && !strings.Contains(q.sql, "FROM runs WHERE pool_id") && !strings.Contains(q.sql, "LEFT JOIN pools") {
						t.Errorf("unexpected idle query (history, future hosts and snapshots must be skipped): %s", q.sql)
					}
				}
				if sweeps != 1 || expectations != map[bool]int{false: 0, true: 1}[needExpectation] {
					t.Fatalf("expectation queries=%d, idle sweeps=%d", expectations, sweeps)
				}
				if !needExpectation {
					if plan.Expected != nil || plan.Unknown != "" {
						t.Fatalf("unread history must not claim unknown capacity: %+v", plan)
					}
					original = plan
				} else {
					if plan.Expected == nil || plan.Expected.Observations != 2 || plan.Unknown != "" {
						t.Fatalf("full idle evidence: %+v", plan)
					}
					plan.Expected = nil // The sole intentional difference on a truly idle pass.
					if !reflect.DeepEqual(plan, original) {
						t.Fatalf("full and fast idle plans differ: full=%+v fast=%+v", plan, original)
					}
				}
			}
		})
	}
}

// A false flag represents a stale no-demand poolState. Actual transactional
// waiting Runs must still reserve starting capacity and plan new hosts.
func TestCapacityPlanIdleSnapshotRace(t *testing.T) {
	s, pl, _ := planningFixture(t, 2)
	observePlanningHost(t, s, "history", "terminated", proto.Capacity{CPUs: 1, Memory: 10, Disk: 10, Runs: 1}, map[string]string{})
	execSQL(t, s, context.Background(), `INSERT INTO hosts (id,name,tenant_id,pool_id,state,provider_id,provision_requested_at,launch_template)
		VALUES ('start','start','t1','pool1','provisioning','i-start',now(),'{"version":1}')`)
	rec := traceIdlePlanning(t, s)
	if err := s.db.Tx(context.Background(), store.System(), func(tx pgx.Tx) error {
		plan, err := s.planCapacity(context.Background(), tx, pl, nil, false)
		if err == nil && (plan.Expected == nil || plan.Starting != 1 || plan.Planned != 1 || plan.NewHosts != 1 || plan.reservedStarting != 1) {
			t.Fatalf("actual demand bypassed full planning: %+v", plan)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(rec.got) < 2 || !strings.Contains(rec.got[0].sql, "FROM runs WHERE pool_id") || !strings.Contains(rec.got[1].sql, "SELECT capacity, labels, registered_at FROM hosts") {
		t.Fatalf("waiting Runs must precede history: %+v", rec.got)
	}
}

func TestCapacityReconcileIdleLaunchEvidence(t *testing.T) {
	for _, c := range []struct {
		name          string
		warm, min     int
		starts, calls int
		history       bool
	}{
		{"idle", 0, 0, 0, 0, false},
		{"warm", 1, 0, 0, 1, true},
		{"minimum", 0, 1, 0, 1, true},
		{"warm unknown", 1, 0, 0, 1, false},
		{"minimum unknown", 0, 1, 0, 1, false},
		{"warm covered by start", 1, 0, 1, 0, true},
		{"minimum covered by start", 0, 1, 1, 0, true},
		{"warm partly covered", 2, 0, 1, 1, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, pl, p := planningFixture(t, 0)
			pl.Warm, pl.Min = c.warm, c.min
			if c.history {
				observePlanningHost(t, s, "history", "terminated", proto.Capacity{CPUs: 4}, map[string]string{})
			}
			if c.starts > 0 {
				// An old template's start still covers physical warm demand.
				execSQL(t, s, context.Background(), `INSERT INTO hosts (id,name,tenant_id,pool_id,state,provider_id,provision_requested_at,launch_template)
					VALUES ('start','start','t1','pool1','provisioning','i-start',now(),'{"version":0}')`)
			}
			rec := traceIdlePlanning(t, s)
			planningTick(t, s, pl, p, false)
			expectations := 0
			for _, q := range rec.got {
				if strings.Contains(q.sql, "SELECT capacity, labels, registered_at FROM hosts") {
					expectations++
				}
			}
			if p.calls != c.calls || expectations != c.calls {
				t.Fatalf("launches=%d expectation queries=%d, want %d", p.calls, expectations, c.calls)
			}
			up := events(t, s, evScaleUp)
			if len(up) != c.calls || len(events(t, s, evScaleBlocked)) != 0 {
				t.Fatalf("idle scale events: %+v", up)
			}
			if c.calls > 0 {
				d := up[0].Data
				reason := "warm"
				if c.min > 0 {
					reason = "minimum"
				}
				if d["reason"] != reason || d["waiting"] != 0.0 || d["planned"] != 0.0 {
					t.Fatalf("idle launch summary: %+v", d)
				}
				if c.history {
					e, ok := d["expected"].(map[string]any)
					if !ok || e["observations"] != 1.0 || d["unknown"] != "" {
						t.Fatalf("missing expected evidence: %+v", d)
					}
				} else if d["expected"] != nil || d["unknown"] != "no registered host observations for current template" {
					t.Fatalf("missing unknown evidence: %+v", d)
				}
			}
		})
	}
}

func TestCapacityReconcileIdleBlockedEvidence(t *testing.T) {
	for _, cause := range []string{causeMax, causeQuota, causeBackoff, "retired"} {
		t.Run(cause, func(t *testing.T) {
			s, pl, p := planningFixture(t, 0)
			pl.Min = 2
			observePlanningHost(t, s, "ready", "ready", proto.Capacity{CPUs: 4}, map[string]string{})
			switch cause {
			case causeMax:
				pl.Max = 1
			case causeQuota:
				execSQL(t, s, context.Background(), `UPDATE tenants SET max_hosts = 1 WHERE id = 't1'`)
			case causeBackoff:
				s.launchFailed(pl, errors.New("launch refused"))
			case "retired":
				pl.Retired = true
			}
			rec := traceIdlePlanning(t, s)
			planningTick(t, s, pl, p, false)
			if p.calls != 0 || len(events(t, s, evScaleUp)) != 0 {
				t.Fatalf("blocked or retired pool launched %d", p.calls)
			}
			blocked := events(t, s, evScaleBlocked)
			if cause == "retired" {
				if len(blocked) != 0 {
					t.Fatalf("retired pool recorded blocked evidence: %+v", blocked)
				}
			} else if len(blocked) != 1 || blocked[0].Data["cause"] != cause || blocked[0].Data["expected"] == nil || blocked[0].Data["unknown"] != "" || blocked[0].Data["wanted"] != 1.0 {
				t.Fatalf("missing idle shortfall evidence: %+v", blocked)
			}
			expectations := 0
			for _, q := range rec.got {
				if strings.Contains(q.sql, "SELECT capacity, labels, registered_at FROM hosts") {
					expectations++
				}
			}
			if expectations != 1 {
				t.Fatalf("shortfall expectation queries=%d, want 1", expectations)
			}
		})
	}
}

type idleQueryFailure struct {
	pgx.Tx
	match string
	err   error
}

func (tx idleQueryFailure) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if strings.Contains(sql, tx.match) {
		return nil, tx.err
	}
	return tx.Tx.Query(ctx, sql, args...)
}

func TestCapacityPlanIdleErrors(t *testing.T) {
	for _, c := range []struct {
		name, query string
		demand      int
	}{
		{"waiting runs", "FROM runs WHERE pool_id", 0},
		{"idle decisions", "SELECT h.id FROM hosts h", 0},
		{"demand expectation", "SELECT capacity, labels, registered_at FROM hosts", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, pl, _ := planningFixture(t, c.demand)
			want := errors.New(c.name)
			err := s.db.Tx(context.Background(), store.System(), func(tx pgx.Tx) error {
				_, err := s.planCapacity(context.Background(), idleQueryFailure{tx, c.query, want}, pl, nil, false)
				return err
			})
			if !errors.Is(err, want) {
				t.Fatalf("got %v, want %v", err, want)
			}
		})
	}
}
