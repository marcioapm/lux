-- 056_run_terminated.sql — the irreversible end of a Run is terminated,
-- no longer cancelled; the request is terminate, no longer cancel.
--
-- Every statement rewrites rows of Runs that already ended (or are being
-- stopped for good), which luxd does not otherwise write, so the row locks
-- taken wait on nothing; run_events and host_events are scanned once each,
-- as 044 scans run_events. A luxd older than this migration reads and writes
-- cancel_requested and fails once it is renamed: every luxd sharing the
-- database moves to the new release together (docs/operations.md).

ALTER TABLE runs RENAME COLUMN cancel_requested TO terminate_requested;

UPDATE runs SET state = 'terminated' WHERE state = 'cancelled';
-- state_reason is display text written by luxd: the forms it wrote for a
-- cancel, and an expiry's "expired: …" (which needs no change).
UPDATE runs SET state_reason = CASE
		WHEN state_reason = 'cancelled' OR state_reason = 'cancel' AND state = 'terminated' THEN 'terminated'
		WHEN state_reason = 'cancel' THEN 'terminate'
		ELSE 'terminated' || substr(state_reason, length('cancelled') + 1) END
	WHERE state_reason IN ('cancel', 'cancelled') OR state_reason LIKE 'cancelled;%';

UPDATE placements SET stop_reason = 'terminate' WHERE stop_reason = 'cancel';

-- The cost queue's reasons: a row queued by a cancel, then the CHECK.
ALTER TABLE cost_pending DROP CONSTRAINT cost_pending_reason_check;
UPDATE cost_pending SET reason = 'state:terminated' WHERE reason = 'state:cancelled';
ALTER TABLE cost_pending ADD CONSTRAINT cost_pending_reason_check CHECK (reason IN ('tick', 'settle', 'retry',
	'state:stopping', 'state:stopped', 'state:lost', 'state:succeeded', 'state:failed', 'state:terminated', 'state:resuming'));

-- Events read back by clients and the console: the state events, the
-- request (cancel.requested), and what a Run's end did to its servers.
UPDATE run_events SET data = jsonb_set(data, '{state}', '"terminated"') WHERE type = 'state' AND data->>'state' = 'cancelled';
UPDATE run_events SET data = jsonb_set(data, '{reason}', to_jsonb(CASE
		WHEN data->>'reason' = 'cancel' AND data->>'state' = 'stopping' THEN 'terminate'
		WHEN data->>'reason' = 'cancel' THEN 'terminated'
		WHEN data->>'reason' = 'cancelled' THEN 'terminated'
		ELSE 'terminated' || substr(data->>'reason', length('cancelled') + 1) END))
	WHERE type = 'state' AND (data->>'reason' IN ('cancel', 'cancelled') OR data->>'reason' LIKE 'cancelled;%');
UPDATE run_events SET type = 'terminate.requested' WHERE type = 'cancel.requested';
UPDATE run_events SET data = jsonb_set(data, '{reason}', '"run terminated"')
	WHERE type IN ('server.deleted', 'server.detached') AND data->>'reason' = 'run cancelled';

UPDATE host_events SET data = data
		|| CASE WHEN data->>'outcome' = 'cancelled' THEN '{"outcome": "terminated"}'::jsonb ELSE '{}'::jsonb END
		|| CASE WHEN data->>'stopReason' = 'cancel' THEN '{"stopReason": "terminate"}'::jsonb ELSE '{}'::jsonb END
	WHERE type = 'host.placement_ended' AND (data->>'outcome' = 'cancelled' OR data->>'stopReason' = 'cancel');

-- Servers of an ended Run were detached with this reason as their stop.
UPDATE run_servers SET stop_reason = 'run terminated' WHERE stop_reason = 'run cancelled';
