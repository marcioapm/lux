-- 078_run_first_pool.sql — the pool a Run first started in (docs/telemetry.md, pools over time).
--
-- runs.first_pool_id: the pool of the host of the placement the Run first
-- ran on, else the pool it was bound to (runs.pool_id) when that host had
-- none. Set once, with first_started_at, by the placement's running report
-- (lifecycle.go), and never rewritten: a later resume into another pool
-- leaves it. Pool history and GET /v1/pools/stats count a Run's first start
-- under it, without reading placements per Run. NULL for a Run never
-- started.
--
-- Started Runs from before this migration: the same rule as luxd's
-- runStartPool reads (the placement whose started_at is first_started_at,
-- else the earliest started one; its host's pool). Left NULL where it cannot
-- be told (no started placement, or its host has no pool or is gone); the
-- readers then fall back as before. One correlated UPDATE in this
-- migration's transaction, as 053 backfilled runs.state_changed_at: it
-- rewrites every started Run (about 31 s per million on the test machine)
-- and holds their row locks until it commits, so runner reports and API
-- writes to those Runs wait for it. The column is added without a default
-- (catalog-only) and its index built after the backfill, in the same
-- transaction, as 041 built runs_pool_started.
SET LOCAL lock_timeout = '5s';
ALTER TABLE runs ADD COLUMN first_pool_id text REFERENCES pools(id);

UPDATE runs r SET first_pool_id = x.pool_id
FROM (SELECT s.id, (SELECT h.pool_id FROM (SELECT pl.host_id FROM placements pl
			WHERE pl.run_id = s.id AND pl.started_at IS NOT NULL
			ORDER BY pl.started_at IS DISTINCT FROM s.first_started_at, pl.started_at, pl.epoch LIMIT 1) pl
		JOIN hosts h ON h.id = pl.host_id) AS pool_id
	FROM runs s WHERE s.first_started_at IS NOT NULL) x
WHERE x.id = r.id AND x.pool_id IS NOT NULL;

-- A pool's Runs first started in a range.
CREATE INDEX runs_first_pool_started ON runs (first_pool_id, first_started_at) WHERE first_started_at IS NOT NULL;
