package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// checkingProvider answers Check as EC2's dry run would: an error for a
// launch template it does not know (or for every template, with down),
// and counts the checks and keeps the last one's tags.
type checkingProvider struct {
	fakeLaunchProvider
	known    map[string]bool
	down     bool
	checks   int
	lastTags map[string]string
}

func (p *checkingProvider) Check(_ context.Context, template json.RawMessage, tags map[string]string) error {
	p.checks++
	p.lastTags = tags
	if p.down {
		return errors.New("ec2 RunInstances: dial tcp: connection refused")
	}
	var t struct {
		LaunchTemplate string `json:"launchTemplate"`
	}
	if err := json.Unmarshal(template, &t); err != nil {
		return err
	}
	if !p.known[t.LaunchTemplate] {
		return fmt.Errorf("InvalidLaunchTemplateName.NotFound: The specified launch template, with template name %s, does not exist.", t.LaunchTemplate)
	}
	return nil
}

func storedTemplate(t *testing.T, s *Server, ctx context.Context, name string) map[string]any {
	t.Helper()
	var tmpl map[string]any
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT template FROM pools WHERE tenant_id = 't1' AND name = $1`, name).Scan(&tmpl)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return tmpl
}

// An ec2 pool is set only if its template would launch: a launch template
// EC2 does not know is a 422 naming EC2's code, and the pool keeps what it
// had. An unchanged template is not checked again.
func TestPutPoolChecksTheTemplateLaunches(t *testing.T) {
	s := testServer(t)
	prov := &checkingProvider{known: map[string]bool{"lux-runner": true}}
	s.cfg.Providers = map[string]Provider{"ec2": prov}
	ctx := context.Background()
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ctx = context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})
	good := map[string]any{"region": "eu-north-1", "launchTemplate": "lux-runner"}
	if _, err := s.putPool(ctx, poolIn(Pool{Name: "arm64", Provider: "ec2", MaxHosts: 4, Template: good})); err != nil {
		t.Fatalf("a template that launches was refused: %v", err)
	}
	if got := storedTemplate(t, s, ctx, "arm64"); got["launchTemplate"] != "lux-runner" {
		t.Fatalf("stored %v", got)
	}

	gone := map[string]any{"region": "eu-north-1", "launchTemplate": "lux-runner-arm64"}
	for _, name := range []string{"arm64", "fresh"} {
		_, err := s.putPool(ctx, poolIn(Pool{Name: name, Provider: "ec2", MaxHosts: 10, Template: gone}))
		var he *HTTPError
		if !errors.As(err, &he) || he.Status != http.StatusUnprocessableEntity || he.Code != "invalid_pool" ||
			!strings.HasPrefix(he.Message, "template: ec2 cannot launch it: InvalidLaunchTemplateName.NotFound: ") {
			t.Fatalf("%s: err %v, want 422 invalid_pool naming InvalidLaunchTemplateName.NotFound", name, err)
		}
	}
	if got := storedTemplate(t, s, ctx, "arm64"); got["launchTemplate"] != "lux-runner" {
		t.Errorf("the refused set changed the pool: %v", got)
	}
	if got := storedTemplate(t, s, ctx, "fresh"); got != nil {
		t.Errorf("the refused set created a pool: %v", got)
	}

	// EC2 unreachable: the same template set again (other fields changed)
	// is not checked; a changed one is refused.
	prov.down = true
	checks := prov.checks
	if _, err := s.putPool(ctx, poolIn(Pool{Name: "arm64", Provider: "ec2", MaxHosts: 6, Template: map[string]any{"launchTemplate": "lux-runner", "region": "eu-north-1"}})); err != nil {
		t.Fatalf("an unchanged template was checked: %v", err)
	}
	if prov.checks != checks {
		t.Errorf("%d checks for an unchanged template", prov.checks-checks)
	}
	changed := map[string]any{"region": "eu-north-1", "launchTemplate": "lux-runner", "spot": true}
	if _, err := s.putPool(ctx, poolIn(Pool{Name: "arm64", Provider: "ec2", Template: changed})); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("a changed template with EC2 down: err %v, want refused", err)
	}

	// A static pool has no check.
	if _, err := s.putPool(ctx, poolIn(Pool{Name: "metal", Provider: "static"})); err != nil {
		t.Errorf("a static pool: %v", err)
	}
}

// The check carries a launch's tags: this deployment's id, the pool's name
// and the id it is stored under (the one a create inserts), and a host's
// Name and lux:host.
func TestPutPoolChecksWithTheLaunchTags(t *testing.T) {
	s := testServer(t)
	prov := &checkingProvider{known: map[string]bool{"lux-runner": true}}
	s.cfg.Providers = map[string]Provider{"ec2": prov}
	ctx := context.Background()
	var deployment string
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT value FROM settings WHERE name = 'deployment'`).Scan(&deployment)
	}); err != nil {
		t.Fatal(err)
	}
	ctx = context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})
	poolID := func() string {
		var id string
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT id FROM pools WHERE tenant_id = 't1' AND name = 'arm64'`).Scan(&id)
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	for i, tmpl := range []map[string]any{
		{"region": "eu-north-1", "launchTemplate": "lux-runner"},               // creates
		{"region": "eu-north-1", "launchTemplate": "lux-runner", "spot": true}, // updates
	} {
		if _, err := s.putPool(ctx, poolIn(Pool{Name: "arm64", Provider: "ec2", MaxHosts: 4, Template: tmpl})); err != nil {
			t.Fatal(err)
		}
		got := prov.lastTags
		want := LaunchTags(deployment, poolID(), "arm64", got[tagHost])
		if !maps.Equal(got, want) || !strings.HasPrefix(got[tagHost], "host_") {
			t.Errorf("set %d: check tags %v, want %v", i, got, want)
		}
	}
}
