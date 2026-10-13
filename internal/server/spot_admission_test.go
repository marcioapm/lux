package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

func TestSpotAgedAdmissionAndNormalPriority(t *testing.T) {
	s, _, _ := planningFixture(t, 1)
	ctx := context.Background()
	at := time.Now()
	s.now = func() time.Time { return at }
	execSQL(t, s, ctx, `UPDATE runs SET state='submitted', spec=spec||'{"spot":true,"resumePolicy":"never"}'`)
	observePlanningHost(t, s, "h", "ready", proto.Capacity{CPUs: 1, Memory: 10, Disk: 10, Runs: 1}, map[string]string{})
	execSQL(t, s, ctx, `UPDATE hosts SET capabilities=$1 WHERE id='h'`, []string{proto.CapSpotReclaim})
	if err := s.scheduleOnce(ctx); err != nil {
		t.Fatal(err)
	}
	at = at.Add(61 * time.Second)
	for i := 0; i < 25; i++ {
		execSQL(t, s, ctx, `INSERT INTO runs(id,tenant_id,pool_id,spec,state,updated_at) VALUES($1,'t1','pool1','{"resources":{"cpus":2},"placement":{"pool":"burst"}}','submitted',now())`, "blocked"+string(rune('a'+i)))
	}
	execSQL(t, s, ctx, `INSERT INTO runs(id,tenant_id,pool_id,spec,state,updated_at) VALUES('normal','t1','pool1','{"resources":{"cpus":1,"memory":10,"disk":10},"placement":{"pool":"burst"}}','resuming',now()+interval '1 second')`)
	if err := s.scheduleOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM placements WHERE run_id='r0'`); n != 0 {
		t.Fatal("spot stole normal capacity")
	}
	if state := queryOne[string](t, s, `SELECT state FROM runs WHERE id='normal'`); state != StateScheduled {
		t.Fatal(state)
	}
}

func TestSpotReclaimPledgeConcurrencyAndRelease(t *testing.T) {
	s, _, _ := planningFixture(t, 0)
	ctx := context.Background()
	observePlanningHost(t, s, "h", "ready", proto.Capacity{CPUs: 2, Memory: 20, Disk: 20, Runs: 2}, map[string]string{})
	execSQL(t, s, ctx, `INSERT INTO runs(id,tenant_id,pool_id,spec,state,current_epoch) VALUES
 ('spot','t1','pool1','{"spot":true,"resumePolicy":"never","resources":{"cpus":2,"memory":20,"disk":20},"placement":{"pool":"burst"}}','running',1),
 ('n1','t1','pool1','{"resources":{"cpus":2,"memory":20,"disk":20},"placement":{"pool":"burst"}}','submitted',0),
 ('n2','t1','pool1','{"resources":{"cpus":2,"memory":20,"disk":20},"placement":{"pool":"burst"}}','submitted',0)`)
	execSQL(t, s, ctx, `INSERT INTO placements(id,tenant_id,run_id,host_id,epoch,state,resources) VALUES('p','t1','spot','h',1,'running','{"cpus":2,"memory":20,"disk":20}')`)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.scheduleOnce(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := queryOne[int](t, s, `SELECT count(*) FROM spot_pledges`); n != 1 {
		t.Fatalf("pledges=%d", n)
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM placements`); n != 1 {
		t.Fatal("started before release")
	}
	var requester string
	systemScan(t, s, `SELECT reclaim_for FROM placements WHERE id='p'`, nil, &requester)
	if reason := queryOne[string](t, s, `SELECT stop_reason FROM placements WHERE id='p'`); reason != stopSpotReclaim {
		t.Fatal(reason)
	}
	execSQL(t, s, ctx, `UPDATE placements SET state='exited' WHERE id='p'`)
	if err := s.scheduleOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if state := queryOne[string](t, s, `SELECT state FROM runs WHERE id=$1`, requester); state != StateScheduled {
		t.Fatalf("requester=%s", state)
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM spot_pledges`); n != 0 {
		t.Fatal("pledge leaked")
	}
}
