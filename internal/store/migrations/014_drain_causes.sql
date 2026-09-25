-- 014_drain_causes.sql — state_reason is display text; every writer may
-- overwrite it, so it cannot also be the marker drain, undrain, the
-- reaper and the per-pool cap key off. drain_causes tracks each drain's
-- cause independently ("outdated", "manual", "scale-down", "preempt"): a
-- Hello's undrain removes only "outdated", and draining stays true while
-- any cause remains (an operator's drain or force-evict survives a
-- binaries-matching reconnect).
ALTER TABLE hosts ADD COLUMN drain_causes text[] NOT NULL DEFAULT '{}';

-- reapOutdatedStaticHosts' NOT EXISTS on blobs (and poolState's) filter
-- on host_id under location = 'host': a seq scan on blobs at 200k rows
-- (EXPLAIN ANALYZE on a seeded test DB), an index scan under it.
CREATE INDEX blobs_host_id_on_host ON blobs (host_id) WHERE location = 'host';
