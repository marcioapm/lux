package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

func fileData(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// skillFiles: two small files under the workload user's home, as a skills
// folder ships.
func skillFiles(version string) []spec.File {
	return []spec.File{
		{Path: "~/.claude/skills/review/SKILL.md", Data: fileData("# review " + version)},
		{Path: "~/.claude/skills/review/run.sh", Mode: 0o755, Data: fileData("#!/bin/sh\necho " + version)},
	}
}

func submitWithFiles(s *Server, files []spec.File) (*submitRunOutput, error) {
	sp := spec.RunSpec{Image: spec.Image{Ref: "alpine"}, Workload: spec.Workload{Command: []string{"true"}}, Files: files}
	return s.submitRun(tenantCtx("t1"), &submitRunInput{Body: sp})
}

// filesHost is a ready host of the default pool whose runner has (or
// lacks) the files capability.
func filesHost(t *testing.T, s *Server, id string, files bool) {
	t.Helper()
	readyHost(t, s, id, "default", "t1", false)
	caps := `{}`
	if files {
		caps = `{` + proto.CapFiles + `}`
	}
	execSQL(t, s, context.Background(), `UPDATE hosts SET capabilities = $2::text[] WHERE id = $1`, id, caps)
}

func runAssign(t *testing.T, s *Server, id string, epoch int) (int64, proto.Assign, []byte) {
	t.Helper()
	var msgID int64
	var payload []byte
	systemScan(t, s, `SELECT id, payload FROM host_messages WHERE run_id = $1 AND type = $2 AND epoch = $3`, []any{id, proto.MsgAssign, epoch}, &msgID, &payload)
	var a proto.Assign
	if err := json.Unmarshal(payload, &a); err != nil {
		t.Fatal(err)
	}
	return msgID, a, payload
}

func invalidSpecProblems(t *testing.T, err error) string {
	t.Helper()
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusUnprocessableEntity || he.Code != "invalid_spec" {
		t.Fatalf("want 422 invalid_spec, got %v", err)
	}
	return he.Message
}

// A bad file is refused with every problem at once; size and sha256 are
// luxd's to set.
func TestSubmitFilesRefused(t *testing.T) {
	s := promptServer(t)
	_, err := submitWithFiles(s, []spec.File{{Path: "../etc/passwd"}, {Path: "/.lux/run/x"}, {Path: "/a", Mode: 0o4755}})
	msg := invalidSpecProblems(t, err)
	for _, want := range []string{"files[0]: path", "files[1]: path", "files[2]: mode"} {
		if !strings.Contains(msg, want) {
			t.Errorf("%q lacks %q", msg, want)
		}
	}
	_, err = submitWithFiles(s, []spec.File{{Path: "/a", Data: fileData("x"), SHA256: "forged"}})
	if msg := invalidSpecProblems(t, err); !strings.Contains(msg, "files[0]: size and sha256 are set by luxd") {
		t.Fatal(msg)
	}
	big := fileData(strings.Repeat("a", spec.MaxFilesBytes))
	_, err = submitWithFiles(s, []spec.File{{Path: "/a", Data: big}, {Path: "/b", Data: fileData("one more")}})
	if msg := invalidSpecProblems(t, err); !strings.Contains(msg, "in total, at most 4.0 MiB") {
		t.Fatal(msg)
	}
}

// Files' bytes are kept beside the spec: the stored spec and Run views
// carry each one's path, mode, size and sha256; every placement's Assign
// carries the bytes, outside its spec, and its ack drops them from the
// queued message; a terminated Run keeps none.
func TestFilesBesideSpec(t *testing.T) {
	s := promptServer(t)
	key := ids.Secret("luxk")
	execSQL(t, s, context.Background(), `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ('k1', 't1', 'k', $1, ARRAY['run', 'read'])`, ids.Hash(key))
	files := skillFiles("v1")
	out, err := submitWithFiles(s, files)
	if err != nil {
		t.Fatal(err)
	}
	id := out.Body.ID
	if got := out.Body.Spec.Files; len(got) != 2 || got[0].Data != "" || got[0].Size != 11 || got[0].SHA256 == "" || got[1].Mode != 0o755 {
		t.Fatalf("answer's files %+v", got)
	}
	stored := queryOne[string](t, s, `SELECT spec::text FROM runs WHERE id = $1`, id)
	if strings.Contains(stored, files[0].Data) || !strings.Contains(stored, `"sha256"`) {
		t.Fatalf("stored spec: %s", stored)
	}
	if got := queryOne[string](t, s, `SELECT files->1->>'data' FROM runs WHERE id = $1`, id); got != files[1].Data {
		t.Fatalf("runs.files data %q", got)
	}
	var one Run
	if code := getJSON(t, s, key, "/v1/runs/"+id, &one); code != http.StatusOK || len(one.Spec.Files) != 2 || one.Spec.Files[0].Data != "" {
		t.Fatalf("get: %d %+v", code, one.Spec.Files)
	}

	filesHost(t, s, "h1", true)
	schedule(t, s)
	msgID, a, _ := runAssign(t, s, id, 1)
	if len(a.Files) != 2 || a.Files[0].Data != files[0].Data || a.Files[1].Data != files[1].Data || a.Files[0].Path != "~/.claude/skills/review/SKILL.md" {
		t.Fatalf("assign files %+v", a.Files)
	}
	if a.Spec.Files[0].Data != "" {
		t.Fatal("the Assign's spec carries the bytes")
	}
	if err := s.ackMessage(context.Background(), "h1", msgID); err != nil {
		t.Fatal(err)
	}
	_, a, payload := runAssign(t, s, id, 1)
	if strings.Contains(string(payload), files[0].Data) || a.RunID != id || len(a.Spec.Files) != 2 {
		t.Fatalf("acked assign payload %s", payload)
	}

	// The next placement (a resume) is sent them again.
	applyRunStatus(t, s, id, 1, proto.Status{State: "running"})
	a = failResumeAssign(t, s, id)
	if len(a.Files) != 2 || a.Files[1].Data != files[1].Data {
		t.Fatalf("epoch-2 assign files %+v", a.Files)
	}

	// A failed Run keeps them (a resume writes them again); once it is
	// terminated, they go.
	code := 1
	applyRunStatus(t, s, id, 2, proto.Status{State: "exited", ExitCode: &code})
	if n := queryOne[int](t, s, `SELECT count(*) FROM runs WHERE id = $1 AND files IS NOT NULL AND state = 'failed'`, id); n != 1 {
		t.Fatal("files dropped by a failed Run")
	}
	if _, err := s.terminateRun(tenantCtx("t1"), &RunPath{ID: id}); err != nil {
		t.Fatal(err)
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM runs WHERE id = $1 AND files IS NULL AND state = 'terminated'`, id); n != 1 {
		t.Fatal("files kept by a terminated Run")
	}
}

// A Run with files waits for a runner that writes them; one without
// files goes anywhere.
func TestFilesNeedARunnerWithFiles(t *testing.T) {
	s := promptServer(t)
	filesHost(t, s, "old", false)
	out, err := submitWithFiles(s, skillFiles("v1"))
	if err != nil {
		t.Fatal(err)
	}
	schedule(t, s)
	if host, state, reason := placedOn(t, s, out.Body.ID); host != "" || state != StateSubmitted || !strings.Contains(reason, "without files") {
		t.Fatalf("placed on %q, %s (%s)", host, state, reason)
	}
	plain, err := submitWithFiles(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	schedule(t, s)
	if host, _, _ := placedOn(t, s, plain.Body.ID); host != "old" {
		t.Fatalf("a Run without files placed on %q", host)
	}
	filesHost(t, s, "new", true)
	schedule(t, s)
	if host, _, _ := placedOn(t, s, out.Body.ID); host != "new" {
		t.Fatalf("placed on %q", host)
	}
}

// A resume's files replace the Run's, from that placement on; [] removes
// them; a bad one is refused and changes nothing.
func TestResumeReplacesFiles(t *testing.T) {
	s := promptServer(t)
	filesHost(t, s, "h1", true)
	out, err := submitWithFiles(s, skillFiles("v1"))
	if err != nil {
		t.Fatal(err)
	}
	id := out.Body.ID
	schedule(t, s)
	code := 1
	applyRunStatus(t, s, id, 1, proto.Status{State: "exited", ExitCode: &code})

	bad := []spec.File{{Path: "/home/agent/../../etc/shadow", Data: fileData("x")}}
	if _, err := s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: id}, Body: &resumeRequest{Files: &bad}}); err == nil {
		t.Fatal("a resume with a bad file was accepted")
	} else if msg := invalidSpecProblems(t, err); !strings.Contains(msg, "must not contain ..") {
		t.Fatal(msg)
	}
	// A dry run checks them and changes nothing.
	v2 := skillFiles("v2")
	v2 = append(v2, spec.File{Path: "/etc/tool.conf", Data: fileData("x")})
	if _, err := s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: id}, Body: &resumeRequest{Files: &v2, ValidateOnly: true}}); err != nil {
		t.Fatal(err)
	}
	if got := queryOne[string](t, s, `SELECT files->0->>'data' FROM runs WHERE id = $1`, id); got != fileData("# review v1") {
		t.Fatal("a dry run changed the files")
	}

	if _, err := s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: id}, Body: &resumeRequest{Files: &v2}}); err != nil {
		t.Fatal(err)
	}
	// Resuming already: the same files are fine, others are not.
	same := skillFiles("v2")
	same = append(same, spec.File{Path: "/etc/tool.conf", Data: fileData("x")})
	if _, err := s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: id}, Body: &resumeRequest{Files: &same}}); err != nil {
		t.Fatalf("the same files again: %v", err)
	}
	other := skillFiles("v3")
	var he *HTTPError
	if _, err := s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: id}, Body: &resumeRequest{Files: &other}}); !errors.As(err, &he) || he.Code != "not_resumable" {
		t.Fatalf("other files while resuming: %v", err)
	}
	schedule(t, s)
	_, a, _ := runAssign(t, s, id, 2)
	if len(a.Files) != 3 || a.Files[0].Data != fileData("# review v2") || a.Files[2].Path != "/etc/tool.conf" || len(a.Spec.Files) != 3 || a.Spec.Files[2].Data != "" {
		t.Fatalf("epoch-2 assign files %+v spec %+v", a.Files, a.Spec.Files)
	}
	var ev map[string]any
	systemScan(t, s, `SELECT data->'files' FROM run_events WHERE run_id = $1 AND type = 'resume.requested' ORDER BY id DESC LIMIT 1`, []any{id}, &ev)
	if ev["count"] != float64(3) {
		t.Fatalf("resume.requested files %v", ev)
	}

	// [] removes them: the next Assign has none, and any host will do.
	applyRunStatus(t, s, id, 2, proto.Status{State: "exited", ExitCode: &code})
	none := []spec.File{}
	if _, err := s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: id}, Body: &resumeRequest{Files: &none}}); err != nil {
		t.Fatal(err)
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM runs WHERE id = $1 AND files IS NULL AND NOT spec ? 'files'`, id); n != 1 {
		t.Fatal("files kept after a resume removed them")
	}
}

// Over HTTP: mode is an octal string, a number is refused; the answer
// names each file's mode in four digits and carries no bytes.
func TestSubmitFilesOverHTTP(t *testing.T) {
	s, key := inputFixture(t)
	mustPut(t, s, "t1", Pool{Name: "default", Provider: "static", IsDefault: mark(true)})
	body := func(mode string) []byte {
		return []byte(`{"image":{"ref":"alpine"},"workload":{"command":["true"]},"files":[{"path":"~/a","mode":` + mode + `,"data":"` + fileData("hi") + `"}]}`)
	}
	code, out := postJSON(t, s, key, "/v1/runs", body(`"0600"`))
	if code != http.StatusCreated && code != http.StatusAccepted && code != http.StatusOK {
		t.Fatalf("submit: %d %v", code, out)
	}
	spec, _ := out["spec"].(map[string]any)
	files, _ := spec["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("answer %v", out)
	}
	f := files[0].(map[string]any)
	if f["mode"] != "0600" || f["data"] != nil || f["size"] != float64(2) {
		t.Fatalf("answer's file %v", f)
	}
	if code, out := postJSON(t, s, key, "/v1/runs", body(`384`)); code < 400 {
		t.Fatalf("a numeric mode: %d %v", code, out)
	}
	if code, out := postJSON(t, s, key, "/v1/runs", body(`"rw-r--r--"`)); code < 400 {
		t.Fatalf("a symbolic mode: %d %v", code, out)
	}
}
