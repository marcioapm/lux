-- 013_exit_requested.sql — a static host drained for outdated binaries is
-- told to exit exactly once, once it has nothing left running or to
-- upload: the systemd unit's Restart=always brings it back with fresh
-- binaries (its ExecStartPre re-downloads them first).
ALTER TABLE hosts ADD COLUMN exit_requested_at timestamptz;
