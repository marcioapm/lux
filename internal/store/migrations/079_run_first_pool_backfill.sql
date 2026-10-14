-- 079_run_first_pool_backfill.sql — runs.first_pool_id (078) for the Runs
-- that ended before it, and its index.
--
-- The rule is a frozen copy of runStartPool's lookup in
-- internal/server/history.go: the placement whose started_at is the Run's
-- first_started_at, else its earliest started one; its host's pool. Left
-- NULL where it cannot be told (no started placement, or its host has no
-- pool or is gone).
--
-- Only ended Runs (succeeded, failed, terminated: endedRunStates in
-- internal/server/lifecycle.go): no runner reports for them, so the row
-- locks this UPDATE holds until commit (about 31 s per million Runs on the
-- test machine) hold back only writes to an ended Run, such as a resume or a
-- late report. Live and resting Runs are not touched: their first_pool_id
-- stays NULL and the readers resolve it with the same lookup at read time.
-- The UPDATE takes ROW EXCLUSIVE on runs, which blocks no read, no insert
-- (a submit) and no write to another Run; pools only get the KEY SHARE row
-- locks of the foreign-key checks.
--
-- Then runs_first_pool_started, built as 041 built runs_pool_started: a
-- plain CREATE INDEX, whose SHARE lock on runs holds back writes to runs
-- (reads go on) for the build, about 0.3 s per 200,000 Runs, until this
-- transaction commits right after. It waits for writes in progress first,
-- up to lock_timeout.
SET LOCAL lock_timeout = '5s';

UPDATE runs r SET first_pool_id = x.pool_id
FROM (SELECT s.id, (SELECT h.pool_id FROM (SELECT pl.host_id FROM placements pl
			WHERE pl.run_id = s.id AND pl.started_at IS NOT NULL
			ORDER BY pl.started_at IS DISTINCT FROM s.first_started_at, pl.started_at, pl.epoch LIMIT 1) pl
		JOIN hosts h ON h.id = pl.host_id) AS pool_id
	FROM runs s WHERE s.first_started_at IS NOT NULL AND s.state IN ('succeeded', 'failed', 'terminated')) x
WHERE x.id = r.id AND x.pool_id IS NOT NULL;

-- A pool's Runs first started in a range.
CREATE INDEX runs_first_pool_started ON runs (first_pool_id, first_started_at) WHERE first_started_at IS NOT NULL;
