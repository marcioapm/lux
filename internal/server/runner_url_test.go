package server

import (
	"cmp"
	"context"
	"encoding/json"
	"testing"
)

// fakeLaunchProvider records the env launch passed it (the one thing
// this test cares about); Terminate and Instances are unused here.
type fakeLaunchProvider struct {
	env map[string]string
}

func (p *fakeLaunchProvider) Launch(ctx context.Context, template json.RawMessage, tags, env map[string]string) (string, error) {
	p.env = env
	return "i-fake", nil
}

func (p *fakeLaunchProvider) Terminate(ctx context.Context, template json.RawMessage, providerID string) error {
	return nil
}

func (p *fakeLaunchProvider) Instances(ctx context.Context, template json.RawMessage, tags map[string]string) (map[string]Instance, error) {
	return nil, nil
}

// launch puts RunnerURL into LUX_URL for the runner's env, defaulting to
// PublicURL when RunnerURL is unset (server.go's cfg.RunnerURL =
// cmp.Or(cfg.RunnerURL, cfg.PublicURL), applied in New).
func TestLaunchSetsLuxURLFromRunnerURL(t *testing.T) {
	cases := []struct {
		name                 string
		publicURL, runnerURL string
		wantLuxURL           string
	}{
		{"RunnerURL set: used over PublicURL", "https://public.example", "http://10.0.1.10:7070", "http://10.0.1.10:7070"},
		{"RunnerURL unset: falls back to PublicURL", "https://public.example", "", "https://public.example"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			s.cfg.PublicURL = c.publicURL
			s.cfg.RunnerURL = cmp.Or(c.runnerURL, c.publicURL)
			ctx := context.Background()
			execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider) VALUES ('pool1', 'burst', 'ec2')`)
			pl := poolRow{ID: "pool1", Name: "burst", Provider: "ec2"}
			prov := &fakeLaunchProvider{}
			if err := s.launch(ctx, prov, pl); err != nil {
				t.Fatal(err)
			}
			if prov.env["LUX_URL"] != c.wantLuxURL {
				t.Errorf("LUX_URL = %q, want %q", prov.env["LUX_URL"], c.wantLuxURL)
			}
		})
	}
}
