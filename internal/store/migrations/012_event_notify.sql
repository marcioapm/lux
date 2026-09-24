-- 012_event_notify.sql — every Run event wakes whoever is following it
-- (the feed, the Run's output streams) in every luxd, instead of each
-- polling. The payload is the Run's id: listeners re-read under their own
-- scope. Postgres folds identical notifications within a transaction.
CREATE FUNCTION lux_event_notify() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM pg_notify('lux_events', NEW.run_id);
  RETURN NULL;
END $$;
CREATE TRIGGER run_events_notify AFTER INSERT ON run_events
  FOR EACH ROW EXECUTE FUNCTION lux_event_notify();
