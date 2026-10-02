package server

import (
	"context"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/spec"
)

// secretsSpec has a secret of every kind: TOKEN (env), GIT_TOKEN (a git
// credential), REG (registry auth) and HDR (a service's header).
func secretsSpec() spec.RunSpec {
	return spec.RunSpec{
		Image: spec.Image{Ref: "alpine", RegistryAuth: []spec.RegistryAuth{{Registry: "ghcr.io", Secret: "REG"}}},
		Workload: spec.Workload{Adapter: "generic", Command: []string{"true"},
			Services: []spec.Service{{Name: "api", URL: "https://api.example.com/", Headers: []spec.MCPHeader{{Name: "Authorization", Secret: "HDR"}}}}},
		Volumes:   []spec.Volume{{Name: "workspace", Path: "/workspace", Kind: "state"}},
		Git:       &spec.Git{Repositories: []spec.Repository{{Name: "app", URL: "https://git.example.com/app.git", Credential: "GIT_TOKEN"}}},
		Secrets:   []spec.Secret{{Name: "TOKEN", Value: "token-1"}, {Name: "GIT_TOKEN", Value: "git-1"}, {Name: "REG", Value: "reg-1"}, {Name: "HDR", Value: "hdr-1"}},
		Network:   spec.Network{Unrestricted: true},
		Placement: spec.Placement{Pool: "default"},
	}
}

// baseValues supplies every secret secretsSpec declares.
func baseValues() []spec.Secret {
	return []spec.Secret{{Name: "TOKEN", Value: "token-2"}, {Name: "GIT_TOKEN", Value: "git-2"}, {Name: "REG", Value: "reg-2"}, {Name: "HDR", Value: "hdr-2"}}
}

// stoppedWithSecrets submits secretsSpec as tenant t1 and stops it as
// stoppedRun does: a snapshot on host ha, uploaded.
func stoppedWithSecrets(t *testing.T, s *Server) string {
	t.Helper()
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1') ON CONFLICT DO NOTHING`)
	namedPools(t, s, "default")
	out, err := s.submitRun(tenantCtx("t1"), &submitRunInput{Body: secretsSpec()})
	if err != nil {
		t.Fatal(err)
	}
	id := out.Body.ID
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool_id, state, capacity, last_heartbeat)
		VALUES ('ha', 'ha', 'default', 'ready', '{"cpus":4,"memory":8589934592}', now()) ON CONFLICT DO NOTHING`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources)
		VALUES ('p-'||$1, 't1', $1, 'ha', 1, 'exited', '{}')`, id)
	execSQL(t, s, ctx, `INSERT INTO snapshots (id, tenant_id, run_id, placement_id, epoch, manifest, host_id, uploaded)
		VALUES ('snap-'||$1, 't1', $1, 'p-'||$1, 1, '{"volumes":[]}', 'ha', true)`, id)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopped', current_epoch = 1, snapshot_id = 'snap-'||id, pool_id = 'default' WHERE id = $1`, id)
	return id
}

func resumeSecrets(ctx context.Context, s *Server, id string, req resumeRequest) (*resumeOutput, error) {
	return s.resumeRun(ctx, &resumeRunInput{RunPath: RunPath{ID: id}, Body: &req})
}

func with(secs []spec.Secret, more ...spec.Secret) []spec.Secret {
	return append(slices.Clone(secs), more...)
}

// storedSecrets is the Run's stored spec.secrets and runs.secrets.
func storedSecrets(t *testing.T, s *Server, id string) ([]spec.Secret, []spec.SecretRef) {
	t.Helper()
	var secs []spec.Secret
	var refs []spec.SecretRef
	systemScan(t, s, `SELECT coalesce(spec->'secrets', '[]'), secrets FROM runs WHERE id = $1`, []any{id}, &secs, &refs)
	return secs, refs
}

func secretNames(secs []spec.Secret) []string {
	var n []string
	for _, s := range secs {
		n = append(n, s.Name)
	}
	slices.Sort(n)
	return n
}

func refNames(refs []spec.SecretRef) []string {
	var n []string
	for _, r := range refs {
		n = append(n, r.Name)
	}
	return n
}

// A value for a name the Run lacks declares it: env by default, or file
// at its path, stored without the value, its fingerprint in runs.secrets,
// its value held for the placement. The next resume needs it.
func TestResumeDeclaresSecrets(t *testing.T) {
	s := testServer(t)
	id := stoppedWithSecrets(t, s)
	out, err := resumeSecrets(tenantCtx("t1"), s, id, resumeRequest{Secrets: with(baseValues(),
		spec.Secret{Name: "EXTRA", Value: "extra-1"},
		spec.Secret{Name: "CONF", Value: "conf-1", As: "file", Path: "/home/agent/.conf"})})
	if err != nil {
		t.Fatal(err)
	}
	if out.Body.State != StateResuming {
		t.Fatalf("state %s", out.Body.State)
	}
	secs, refs := storedSecrets(t, s, id)
	byName := map[string]spec.Secret{}
	for _, sec := range secs {
		if sec.Value != "" {
			t.Errorf("stored a value for %s", sec.Name)
		}
		byName[sec.Name] = sec
	}
	if e := byName["EXTRA"]; e.As != "env" || e.RunnerOnly {
		t.Errorf("EXTRA stored as %+v, want env", e)
	}
	if c := byName["CONF"]; c.As != "file" || c.Path != "/home/agent/.conf" || c.RunnerOnly {
		t.Errorf("CONF stored as %+v, want file at its path", c)
	}
	if got, want := refNames(refs), []string{"CONF", "EXTRA", "GIT_TOKEN", "HDR", "REG", "TOKEN"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("runs.secrets %v, want %v", got, want)
	}
	for _, r := range refs {
		if r.Name == "EXTRA" && r.Fingerprint != spec.Fingerprint("EXTRA", "extra-1") {
			t.Errorf("EXTRA's fingerprint %s", r.Fingerprint)
		}
	}
	if vals, ok := s.secrets.get(id); !ok || vals["EXTRA"] != "extra-1" || vals["CONF"] != "conf-1" {
		t.Errorf("held values %v", vals)
	}
	if ev := resumeEvent(t, s, id); !reflect.DeepEqual(ev["addedSecrets"], []any{"EXTRA", "CONF"}) {
		t.Errorf("resume.requested addedSecrets %v", ev["addedSecrets"])
	}

	execSQL(t, s, context.Background(), `UPDATE runs SET state = 'stopped' WHERE id = $1`, id)
	_, err = resumeSecrets(tenantCtx("t1"), s, id, resumeRequest{Secrets: baseValues()})
	he := refused(t, err, http.StatusUnprocessableEntity, "secrets_required", "resume without the declared secrets")
	if !reflect.DeepEqual(he.Details, []string{"CONF", "EXTRA"}) {
		t.Errorf("missing %v, want CONF and EXTRA", he.Details)
	}
}

// A declaration submit would refuse is a 422 invalid_spec naming it, and
// nothing changes: not the spec, not runs.secrets, not the state.
func TestResumeRefusesInvalidDeclarations(t *testing.T) {
	for _, c := range []struct {
		name    string
		secrets []spec.Secret
		problem string
	}{
		{"invalid name", []spec.Secret{{Name: "1BAD", Value: "v-12345"}}, `invalid name "1BAD"`},
		{"reserved prefix", []spec.Secret{{Name: "lux-x", Value: "v-12345", As: "file", Path: "/x"}}, "prefix is reserved"},
		{"not an env name", []spec.Secret{{Name: "a.b", Value: "v-12345"}}, "not a valid environment variable name"},
		{"file without a path", []spec.Secret{{Name: "F", Value: "v-12345", As: "file"}}, "absolute path"},
		{"file with a relative path", []spec.Secret{{Name: "F", Value: "v-12345", As: "file", Path: "rel"}}, "absolute path"},
		{"unknown as", []spec.Secret{{Name: "F", Value: "v-12345", As: "disk"}}, "as must be env, file or none"},
		{"twice", []spec.Secret{{Name: "NEW", Value: "v-12345"}, {Name: "NEW", Value: "v-67890"}}, `duplicate "NEW"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			id := stoppedWithSecrets(t, s)
			beforeSecs, beforeRefs := storedSecrets(t, s, id)
			_, err := resumeSecrets(tenantCtx("t1"), s, id, resumeRequest{Secrets: with(baseValues(), c.secrets...)})
			he := refused(t, err, http.StatusUnprocessableEntity, "invalid_spec", "resume")
			if !strings.Contains(strings.Join(he.Details, "\n"), c.problem) {
				t.Errorf("details %q, want %q", he.Details, c.problem)
			}
			secs, refs := storedSecrets(t, s, id)
			if !reflect.DeepEqual(secs, beforeSecs) || !reflect.DeepEqual(refs, beforeRefs) || storedState(t, s, id) != StateStopped {
				t.Errorf("a refused resume changed the Run: %+v %+v", secs, refs)
			}
		})
	}
}

// A credential of a repository the same resume adds stays addRepositories'
// (runner-only, as none), not an env secret.
func TestResumeAddedRepositoryCredentialIsNotDeclaredAsEnv(t *testing.T) {
	s := testServer(t)
	id := stoppedWithSecrets(t, s)
	_, err := resumeSecrets(tenantCtx("t1"), s, id, resumeRequest{
		Secrets: with(baseValues(), spec.Secret{Name: "NEW_GIT", Value: "new-git-1"}),
		Git:     &resumeGit{Repositories: []spec.Repository{{Name: "two", URL: "https://git.example.com/two.git", Credential: "NEW_GIT"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	secs, refs := storedSecrets(t, s, id)
	i := slices.IndexFunc(secs, func(sec spec.Secret) bool { return sec.Name == "NEW_GIT" })
	if i < 0 || secs[i].As != "none" || !secs[i].RunnerOnly {
		t.Fatalf("NEW_GIT stored as %+v, want runner-only", secs)
	}
	if n := refNames(refs); !slices.Contains(n, "NEW_GIT") {
		t.Fatalf("runs.secrets %v", n)
	}
	if ev := resumeEvent(t, s, id); ev["addedSecrets"] != nil {
		t.Errorf("addedSecrets %v: a credential is the repository's", ev["addedSecrets"])
	}
}
