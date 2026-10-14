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
-- The column only, without a default: a catalog change, no rewrite and no
-- scan. ADD COLUMN takes an ACCESS EXCLUSIVE lock on runs and the foreign
-- key a SHARE ROW EXCLUSIVE lock on pools, both until this transaction
-- commits, which is at once. The ACCESS EXCLUSIVE request first waits up
-- to lock_timeout (5 s) behind transactions open on runs, and new reads
-- and writes of runs queue behind it meanwhile; on timeout the migration
-- fails and is run again. 079 fills it in for Runs that ended before it
-- and builds its index, in a transaction of its own.
SET LOCAL lock_timeout = '5s';
ALTER TABLE runs ADD COLUMN first_pool_id text REFERENCES pools(id);
