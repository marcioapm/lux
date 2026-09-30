package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// Each input's first answer and its later progress become one event each,
// however often the runner reports them (it re-reports what it tails after
// a restart or reconnect); a report from an older runner, with no phase,
// reads as it always did.
func TestInputPhasesRecordedOnce(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	sessionFixture(t, s, ctx)
	consumed := &proto.InputProgress{RequestID: "req-1", Phase: proto.InputConsumed}
	dropped := &proto.InputProgress{RequestID: "req-3", Phase: proto.InputFailed, Error: "the Run stopped before the agent read it"}
	reports := []proto.AdapterEvent{
		{InputAck: "req-1", InputPhase: proto.InputAccepted, InputText: "hi", InputLands: "next_step", InputReceipt: true},
		{InputAck: "req-1", InputPhase: proto.InputAccepted, InputText: "hi", InputLands: "next_step", InputReceipt: true},
		{InputProgress: consumed},
		{InputProgress: consumed},
		{InputAck: "req-2", InputPhase: proto.InputFailed, InputError: "refused"},
		{InputAck: "req-3", InputPhase: proto.InputAccepted, InputText: "x", InputLands: "next_step", InputReceipt: true},
		{InputProgress: dropped},
		{InputProgress: dropped},
		{InputAck: "old", InputText: "x"},
		{InputAck: "old-bad", InputError: "workload not reachable"},
	}
	for _, ev := range reports {
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return s.applyAdapterEvent(ctx, tx, "t1", "r1", 2, ev)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	type row struct {
		Type string
		Data map[string]any
	}
	var got []row
	err := s.db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT type, data FROM run_events WHERE run_id = 'r1' AND type LIKE 'input.%' ORDER BY id`)
		if err != nil {
			return err
		}
		got, err = pgx.CollectRows(rows, pgx.RowToStructByPos[row])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	want := `[{"Type":"input.delivered","Data":{"lands":"next_step","phase":"accepted","receipt":true,"requestId":"req-1","text":"hi"}},` +
		`{"Type":"input.consumed","Data":{"requestId":"req-1"}},` +
		`{"Type":"input.failed","Data":{"error":"refused","phase":"failed","requestId":"req-2"}},` +
		`{"Type":"input.delivered","Data":{"lands":"next_step","phase":"accepted","receipt":true,"requestId":"req-3","text":"x"}},` +
		`{"Type":"input.failed","Data":{"error":"the Run stopped before the agent read it","requestId":"req-3"}},` +
		`{"Type":"input.delivered","Data":{"phase":"accepted","requestId":"old","text":"x"}},` +
		`{"Type":"input.failed","Data":{"error":"workload not reachable","phase":"failed","requestId":"old-bad"}}]`
	if string(b) != want {
		t.Fatalf("events\n got %s\nwant %s", b, want)
	}
}

// A luxd that predates InputProgress (it decodes AdapterEvent with
// encoding/json, internal/server/runner.go:429, and knew only InputAck)
// sees a progress report as an empty event: no second delivery.
func TestInputProgressInvisibleToOldDecoders(t *testing.T) {
	b, _ := json.Marshal(proto.AdapterEvent{InputProgress: &proto.InputProgress{RequestID: "req-1", Phase: proto.InputConsumed}})
	var old struct {
		SessionID  string `json:"sessionId,omitempty"`
		Activity   string `json:"activity,omitempty"`
		InputAck   string `json:"inputAck,omitempty"`
		InputError string `json:"inputError,omitempty"`
	}
	if err := json.Unmarshal(b, &old); err != nil || old.InputAck != "" || old.SessionID != "" || old.Activity != "" {
		t.Fatalf("an old luxd reads %s as %+v (%v)", b, old, err)
	}
}
