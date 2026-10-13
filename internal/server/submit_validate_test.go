package server

import (
	"context"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
	"testing"
	"time"
)

func TestSubmitValidateOnlyReadOnlyAndSpotParity(t *testing.T) {
	s, _, _ := planningFixture(t, 0)
	ctx := context.Background()
	observePlanningHost(t, s, "h", "ready", proto.Capacity{CPUs: 4, Memory: 40, Disk: 40, Runs: 4}, map[string]string{})
	execSQL(t, s, ctx, `UPDATE hosts SET capabilities=$1 WHERE id='h'`, []string{proto.CapSpotReclaim})
	at := time.Now().Add(-61 * time.Second)
	c := capacityAge{}
	c.observe(s.id+"/0/4/40/40/4/pool1", at, freeVec{4, 40, 40, 4})
	execSQL(t, s, ctx, `INSERT INTO host_capacity_age(host_id,key,history) VALUES('h',$1,$2)`, c.Key, c.marshal())
	sp := baseSpec("burst", spec.Resources{CPUs: 1, Memory: 10, Disk: 10})
	sp.Spot = true
	before := queryOne[string](t, s, `SELECT history::text FROM host_capacity_age WHERE host_id='h'`)
	for range 2 {
		out, err := s.submitRun(tenantCtx("t1"), &submitRunInput{ValidateOnly: true, Body: sp})
		if err != nil {
			t.Fatal(err)
		}
		if !out.Body.Valid || !out.Body.WouldStartNow || out.Body.EligibleHost != "h" {
			t.Fatalf("answer=%+v", out.Body)
		}
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM runs`); n != 0 {
		t.Fatal("dry-run created run")
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM run_events`); n != 0 {
		t.Fatal("dry-run event")
	}
	if after := queryOne[string](t, s, `SELECT history::text FROM host_capacity_age WHERE host_id='h'`); after != before {
		t.Fatal("age mutation")
	}
	out, err := s.submitRun(tenantCtx("t1"), &submitRunInput{Body: sp})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.scheduleOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if state := queryOne[string](t, s, `SELECT state FROM runs WHERE id=$1`, out.Body.ID); state != StateScheduled {
		t.Fatalf("real admission=%s", state)
	}
}
