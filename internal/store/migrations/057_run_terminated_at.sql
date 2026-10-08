-- 057_run_terminated_at.sql — storage follows resumability: a succeeded
-- Run is resumable, so it rests (and expires) like a stopped, lost or
-- failed one, and only a terminated Run's blobs go after retention.
--
-- terminated_at: when the Run became terminated, retention's clock
-- (reapRetention). finished_at stays when its last placement ended, so a
-- Run terminated long after it finished keeps its tenant's whole retention
-- from the terminate. Existing terminated Runs: their latest state event
-- saying terminated, else finished_at, else updated_at.
ALTER TABLE runs ADD COLUMN terminated_at timestamptz;
UPDATE runs r SET terminated_at = coalesce(
	(SELECT e.created_at FROM run_events e WHERE e.run_id = r.id AND e.type = 'state' AND e.data->>'state' = 'terminated'
	 ORDER BY e.id DESC LIMIT 1),
	r.finished_at, r.updated_at)
WHERE r.state = 'terminated';

-- reapExpiry's scan: each tenant's resting Runs, oldest first; succeeded
-- now rests too. Built under the migration's transaction, as 053 built it.
DROP INDEX runs_resting;
CREATE INDEX runs_resting ON runs (tenant_id, state_changed_at) WHERE state IN ('stopped', 'lost', 'failed', 'succeeded');
