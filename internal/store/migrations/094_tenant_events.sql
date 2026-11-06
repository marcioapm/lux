-- 094_tenant_events.sql — events of a tenant's people (member.*,
-- invitation.*, key.*, service_account.*) go on the tenant's feed with
-- the Runs' and servers': a row with neither run_id nor server_id, its
-- subject naming what it is about.
ALTER TABLE run_events ADD COLUMN subject text;
ALTER TABLE run_events DROP CONSTRAINT run_events_subject;
-- NOT VALID as before: older rows all have a run_id or a server_id.
ALTER TABLE run_events ADD CONSTRAINT run_events_subject
  CHECK (run_id IS NOT NULL OR server_id IS NOT NULL OR subject IS NOT NULL) NOT VALID;

-- A tenant event notifies 'tenant:<id>': only followers of every event
-- (the feed) wake.
CREATE OR REPLACE FUNCTION lux_event_notify() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM pg_notify('lux_events', coalesce(NEW.run_id, 'srv:' || NEW.server_id, 'tenant:' || NEW.tenant_id));
  RETURN NULL;
END $$;
