-- 054_snapshot_cleanup.sql — the reaper's storage passes, index-backed.
--
-- reapSuperseded looks for Runs with an available snapshot that is not
-- their current one: after cleanup, about one available snapshot per Run
-- that still has one, so the scan stays near the number of such Runs.
CREATE INDEX snapshots_available ON snapshots (run_id) WHERE available;

-- reapRetention's per-Run test, "has a blob it may delete": only blobs in
-- S3 other than artifacts, which retention keeps.
CREATE INDEX blobs_retainable ON blobs (run_id) WHERE location = 's3' AND kind <> 'artifact';
