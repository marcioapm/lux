package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// stageFixture is a Run (r1) scheduled on h1, its placement epoch 1
// assigned and accepted by its runner, as the stage tests start from.
func stageFixture(t *testing.T) (*Server, context.Context, string) {
	t.Helper()
	s := testServer(t)
	ctx := context.Background()
	key := ids.Secret("luxk")
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ('k1', 't1', 'ci', $1, ARRAY['run'])`, ids.Hash(key))
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state) VALUES ('h1', 'h1', 'ready')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ($1, 't1', '{"workload": {"workdir": "/work"}}', 'scheduled', 1)`, r1)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, accepted_at, lease_expires_at)
		VALUES ('p1', 't1', $1, 'h1', 1, 'assigned', now(), now() + interval '1 hour')`, r1)
	return s, ctx, key
}

// reportStatus sends a runner's status for r1's epoch 1 from h1, as the hub
// hands it to luxd, and fails on a nack.
func reportStatus(t *testing.T, s *Server, ctx context.Context, st proto.Status) {
	t.Helper()
	reply := s.handleReport(ctx, "h1", proto.Frame{Type: proto.MsgStatus, RunID: r1, Epoch: 1, Data: proto.Marshal(st)})
	if reply.Type != proto.MsgAck {
		t.Fatalf("report %+v: %s %s", st, reply.Type, reply.Data)
	}
}

func getRun(t *testing.T, s *Server, key string) Run {
	t.Helper()
	w := apiCall(t, s, key, http.MethodGet, "/v1/runs/"+r1, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get run: %d %s", w.Code, w.Body)
	}
	var run Run
	if err := json.Unmarshal(w.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	return run
}

// stageEvents are r1's run.stage events, oldest first.
func stageEvents(t *testing.T, s *Server, ctx context.Context) []map[string]any {
	t.Helper()
	var out []map[string]any
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT data FROM run_events WHERE run_id = $1 AND type = 'run.stage' ORDER BY id`, r1)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowTo[map[string]any])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func stateEvents(t *testing.T, s *Server, ctx context.Context) int {
	t.Helper()
	var n int
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM run_events WHERE run_id = $1 AND type = 'state'`, r1).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func ms(t time.Time) int64 { return t.UnixMilli() }

// A starting report with the marks so far records them while the Run
// starts, and the Run's stage follows: volumes once the image is ready,
// container once its repositories are.
func TestStartingReportRecordsPhaseMarks(t *testing.T) {
	s, ctx, key := stageFixture(t)
	t0 := time.Now().Add(-time.Minute).Truncate(time.Millisecond)

	reportStatus(t, s, ctx, proto.Status{State: "starting", Times: map[string]int64{"imageReady": ms(t0)}})
	run := getRun(t, s, key)
	if run.State != StateStarting || len(run.Placements) != 1 || run.Placements[0].State != "starting" {
		t.Fatalf("after the first starting report: run %s, placements %+v", run.State, run.Placements)
	}
	pl := run.Placements[0]
	if pl.ImageReadyAt == nil || !pl.ImageReadyAt.Equal(t0) || pl.VolumesRestoredAt != nil || pl.ReposReadyAt != nil {
		t.Fatalf("placement marks %+v, want only imageReady %s", pl, t0)
	}
	if run.Stage != StageVolumes || !run.StageSince.Equal(t0) {
		t.Fatalf("stage %s since %s, want volumes since %s", run.Stage, run.StageSince, t0)
	}

	t1, t2 := t0.Add(2*time.Second), t0.Add(3*time.Second)
	reportStatus(t, s, ctx, proto.Status{State: "starting", Times: map[string]int64{"imageReady": ms(t0), "volumesRestored": ms(t1), "reposReady": ms(t2)}})
	run = getRun(t, s, key)
	pl = run.Placements[0]
	if pl.VolumesRestoredAt == nil || !pl.VolumesRestoredAt.Equal(t1) || pl.ReposReadyAt == nil || !pl.ReposReadyAt.Equal(t2) || pl.ContainerStartedAt != nil {
		t.Fatalf("placement marks %+v, want volumesRestored %s and reposReady %s", pl, t1, t2)
	}
	if run.State != StateStarting || run.Stage != StageContainer || !run.StageSince.Equal(t2) {
		t.Fatalf("run %s, stage %s since %s; want starting, container since %s", run.State, run.Stage, run.StageSince, t2)
	}
	// The list carries the stage too.
	w := apiCall(t, s, key, http.MethodGet, "/v1/runs", nil)
	var list struct{ Runs []Run }
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Runs) != 1 || list.Runs[0].Stage != StageContainer {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
}

// A starting report that arrives after running (late, or redelivered)
// moves neither the Run nor its placement back, and its stage stays.
func TestLateStartingAfterRunningChangesNothing(t *testing.T) {
	s, ctx, key := stageFixture(t)
	t0 := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	marks := map[string]int64{"imageReady": ms(t0), "volumesRestored": ms(t0.Add(time.Second)),
		"reposReady": ms(t0.Add(2 * time.Second)), "containerStarted": ms(t0.Add(3 * time.Second))}
	reportStatus(t, s, ctx, proto.Status{State: "starting", Times: map[string]int64{"imageReady": ms(t0)}})
	reportStatus(t, s, ctx, proto.Status{State: "running", Times: marks})
	before := getRun(t, s, key)
	states, stages := stateEvents(t, s, ctx), len(stageEvents(t, s, ctx))
	if before.State != StateRunning || before.Stage != StageRunning {
		t.Fatalf("after running: %s, stage %s", before.State, before.Stage)
	}

	reportStatus(t, s, ctx, proto.Status{State: "starting", Times: map[string]int64{"imageReady": ms(t0)}})
	reportStatus(t, s, ctx, proto.Status{State: "starting", Times: marks})
	after := getRun(t, s, key)
	if after.State != StateRunning || after.Placements[0].State != "running" {
		t.Fatalf("after a late starting: run %s, placement %s; want both running", after.State, after.Placements[0].State)
	}
	if after.Stage != before.Stage || !after.StageSince.Equal(before.StageSince) {
		t.Fatalf("stage moved: %s since %s, was %s since %s", after.Stage, after.StageSince, before.Stage, before.StageSince)
	}
	if n := stateEvents(t, s, ctx); n != states {
		t.Fatalf("%d state events, was %d", n, states)
	}
	if n := len(stageEvents(t, s, ctx)); n != stages {
		t.Fatalf("%d run.stage events, was %d", n, stages)
	}
}

// run.stage is emitted once per change of stage, in order, with its since
// and epoch, and never for a report that changes nothing.
func TestRunStageEmittedOncePerChange(t *testing.T) {
	s, ctx, _ := stageFixture(t)
	t0 := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	steps := []map[string]int64{
		{"imageReady": ms(t0)},
		{"imageReady": ms(t0), "volumesRestored": ms(t0.Add(time.Second))},
		{"imageReady": ms(t0), "volumesRestored": ms(t0.Add(time.Second)), "reposReady": ms(t0.Add(2 * time.Second))},
	}
	for _, times := range steps {
		reportStatus(t, s, ctx, proto.Status{State: "starting", Times: times})
		reportStatus(t, s, ctx, proto.Status{State: "starting", Times: times}) // redelivered
	}
	reportStatus(t, s, ctx, proto.Status{State: "starting", Times: steps[0]}) // late
	running := map[string]int64{"containerStarted": ms(t0.Add(3 * time.Second))}
	reportStatus(t, s, ctx, proto.Status{State: "running", Times: running})
	reportStatus(t, s, ctx, proto.Status{State: "running", Times: running})

	got := stageEvents(t, s, ctx)
	want := []struct {
		stage string
		since time.Time
	}{
		{StageVolumes, t0}, {StageRepositories, t0.Add(time.Second)}, {StageContainer, t0.Add(2 * time.Second)}, {StageRunning, t0.Add(3 * time.Second)},
	}
	if len(got) != len(want) {
		t.Fatalf("run.stage events %v, want %d: one per change", got, len(want))
	}
	for i, w := range want {
		since, err := time.Parse(time.RFC3339Nano, got[i]["since"].(string))
		if err != nil || got[i]["stage"] != w.stage || !since.Equal(w.since) || got[i]["epoch"] != float64(1) {
			t.Errorf("event %d: %v, want %s since %s, epoch 1", i, got[i], w.stage, w.since)
		}
	}

	// A stop moves it to stopping, with the stop's reason, once; the
	// runner's stopping report changes nothing more.
	var host string
	for range 2 {
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			var err error
			host, err = s.requestStop(ctx, tx, "t1", r1, "migrate")
			return err
		}); err != nil || host != "h1" {
			t.Fatalf("stop: %q %v", host, err)
		}
	}
	reportStatus(t, s, ctx, proto.Status{State: "stopping"})
	got = stageEvents(t, s, ctx)
	if len(got) != len(want)+1 || got[len(got)-1]["stage"] != StageStopping || got[len(got)-1]["reason"] != "migrate" {
		t.Fatalf("after two stops: %v, want one more: stopping, reason migrate", got)
	}
	// A terminate while it stops replaces the reason: announced, same since.
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := s.requestStop(ctx, tx, "t1", r1, stopTerminate)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got = stageEvents(t, s, ctx)
	if len(got) != len(want)+2 || got[len(got)-1]["reason"] != stopTerminate || got[len(got)-1]["since"] != got[len(got)-2]["since"] {
		t.Fatalf("after a terminate: %v, want one more: stopping since the same time, reason terminate", got)
	}
}

// A placement assigned and not yet accepted is waiting; its runner's ack
// of the assignment moves the Run to image, since luxd recorded the ack.
func TestAcceptedAssignmentLeavesWaiting(t *testing.T) {
	s, ctx, key := stageFixture(t)
	execSQL(t, s, ctx, `UPDATE placements SET accepted_at = NULL, needed_since = now() - interval '5 seconds' WHERE run_id = $1`, r1)
	execSQL(t, s, ctx, `INSERT INTO host_messages (id, host_id, run_id, epoch, type) VALUES (7, 'h1', $1, 1, $2)`, r1, proto.MsgAssign)
	if run := getRun(t, s, key); run.Stage != StageWaiting {
		t.Fatalf("assigned, not accepted: stage %s", run.Stage)
	}
	for range 2 { // the second ack is a redelivery: nothing to announce
		if err := s.ackMessage(ctx, "h1", 7); err != nil {
			t.Fatal(err)
		}
	}
	run := getRun(t, s, key)
	if run.Stage != StageImage || run.Placements[0].AcceptedAt == nil || !run.StageSince.Equal(*run.Placements[0].AcceptedAt) {
		t.Fatalf("accepted: stage %s since %s, placement %+v; want image since acceptedAt", run.Stage, run.StageSince, run.Placements[0])
	}
	got := stageEvents(t, s, ctx)
	if len(got) != 1 || got[0]["stage"] != StageImage {
		t.Fatalf("run.stage events %v, want one: image", got)
	}
}
