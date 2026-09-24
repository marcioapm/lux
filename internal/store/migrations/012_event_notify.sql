-- 012_event_notify.sql — every Run event wakes whoever is following
-- events (the feed, output streams) in every luxd, instead of each polling.
-- The payload carries nothing: listeners re-read under their own scope.
CREATE FUNCTION lux_event_notify() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM pg_notify('lux_events', '');
  RETURN NULL;
END $$;
CREATE TRIGGER run_events_notify AFTER INSERT ON run_events
  FOR EACH STATEMENT EXECUTE FUNCTION lux_event_notify();
