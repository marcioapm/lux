-- 014_drain_causes.sql — state_reason is display text; every writer may
-- overwrite it, so it cannot also be the marker drain, undrain, the
-- reaper and the per-pool cap key off. drain_causes tracks each drain's
-- cause independently ("outdated", "manual", "scale-down", "preempt"): a
-- Hello's undrain removes only "outdated", and draining stays true while
-- any cause remains (an operator's drain or force-evict survives a
-- binaries-matching reconnect).
ALTER TABLE hosts ADD COLUMN drain_causes text[] NOT NULL DEFAULT '{}';
