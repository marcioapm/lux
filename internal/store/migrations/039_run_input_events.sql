-- 039_run_input_events.sql — luxd records each input event once per
-- request id and type, whatever the runner re-reports after a restart or
-- reconnect. The key it deduplicates on is a row here, inserted with the
-- event (ON CONFLICT DO NOTHING): one primary-key probe, not a search of
-- the Run's event history. (An expression index on run_events'
-- data->>'requestId' cannot serve that search: under row-level security
-- Postgres does not push the non-leakproof ->> into an index condition.)

CREATE TABLE run_input_events (
  tenant_id  text NOT NULL REFERENCES tenants(id),
  run_id     text NOT NULL REFERENCES runs(id),
  type       text NOT NULL,
  request_id text NOT NULL,
  PRIMARY KEY (run_id, type, request_id)
);

ALTER TABLE run_input_events ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_rows ON run_input_events USING (tenant_id = lux_tenant() OR lux_system())
  WITH CHECK (tenant_id = lux_tenant() OR lux_system());

-- The input events already recorded.
INSERT INTO run_input_events (tenant_id, run_id, type, request_id)
SELECT DISTINCT tenant_id, run_id, type, data->>'requestId' FROM run_events
WHERE type IN ('input.delivered', 'input.consumed', 'input.failed') AND data ? 'requestId';
