package server

import (
	"cmp"
	"net/http"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
)

// A terminated placement's exit snapshot lists no volume (the runner
// exports none), whatever the Run's resumePolicy: luxd records it with its
// output as the Run's snapshot, without refusing it, the Run ends
// terminated, and a resume is refused.
func TestTerminatedRunZeroVolumeSnapshotIsRecorded(t *testing.T) {
	for _, policy := range []string{"", "auto", "manual", "restart"} {
		t.Run(cmp.Or(policy, "unset"), func(t *testing.T) {
			s, _ := policyFixture(t, policy)
			if _, err := s.terminateRun(tenantCtx("t1"), &RunPath{ID: "r1"}); err != nil {
				t.Fatal(err)
			}
			sd := proto.SnapshotDone{Manifest: proto.Manifest{SnapshotID: "snapR1", RunID: "r1", Epoch: 1, Volumes: []proto.VolumeSnapshot{}},
				Output: &proto.BlobInfo{BlobID: "b-r1-out", Size: 7, SHA256: "r1-out"}, OutputSeq: 3}
			if got := reportSnapshot(t, s, "h1", "r1", 1, sd); got.Type != proto.MsgAck || ackRefused(t, got) {
				t.Fatalf("snapshot report: %s %s", got.Type, got.Data)
			}
			reportR1Exit(t, s, 143, "stopped")

			var state, reason, snapshot, output, blobs string
			var bytes int64
			var snapRefused bool
			systemScan(t, s, `SELECT r.state, r.state_reason, coalesce(r.snapshot_id, ''), coalesce(p.output_blob_id, ''),
					coalesce(p.snapshot_bytes, -1), p.snapshot_refused,
					(SELECT string_agg(kind || ':' || id, ',' ORDER BY id) FROM blobs WHERE run_id = r.id)
				FROM runs r JOIN placements p ON p.run_id = r.id AND p.epoch = 1 WHERE r.id = 'r1'`, nil,
				&state, &reason, &snapshot, &output, &bytes, &snapRefused, &blobs)
			if state != StateTerminated || reason != "terminated" {
				t.Errorf("state %q reason %q, want terminated, terminated", state, reason)
			}
			if snapshot != "snapR1" || output != "b-r1-out" || bytes != 0 || snapRefused || blobs != "output:b-r1-out" {
				t.Errorf("recorded snapshot %q output %q bytes %d refused %v blobs %q", snapshot, output, bytes, snapRefused, blobs)
			}
			_, err := s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: "r1"}})
			refused(t, err, http.StatusConflict, "not_resumable", "resume of a terminated Run")
		})
	}
}
