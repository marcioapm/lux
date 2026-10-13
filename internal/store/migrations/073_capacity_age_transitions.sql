-- Every placement resource transition updates the bounded ledger in the
-- same transaction. The scheduler and lifecycle already hold cost-host;
-- the trigger also fences direct allocation writers with that lock.
CREATE FUNCTION lux_capacity_age_transition() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  hid text;
  hist jsonb;
  free jsonb;
  cap jsonb;
  cpu float8;
  mem bigint;
  disk bigint;
  slots bigint;
  last_free jsonb;
  merged jsonb;
BEGIN
  hid := CASE WHEN TG_OP = 'DELETE' THEN OLD.host_id ELSE NEW.host_id END;
  PERFORM pg_advisory_xact_lock(hashtextextended('cost-host:' || hid, 0));
  SELECT history INTO hist FROM host_capacity_age WHERE host_id = hid FOR UPDATE;
  IF hist IS NULL THEN RETURN NULL; END IF;
  SELECT capacity INTO cap FROM hosts WHERE id = hid;
  SELECT coalesce(sum((resources->>'cpus')::float8), 0),
         coalesce(sum((resources->>'memory')::bigint), 0),
         coalesce(sum((resources->>'disk')::bigint), 0), count(*)
    INTO cpu, mem, disk, slots FROM placements
    WHERE host_id = hid AND state IN ('assigned','starting','running','stopping');
  free := jsonb_build_object(
    'cpus', CASE WHEN coalesce((cap->>'cpus')::float8,0) > 0 THEN (cap->>'cpus')::float8-cpu ELSE 1152921504606846976 END,
    'memory', CASE WHEN coalesce((cap->>'memory')::bigint,0) > 0 THEN (cap->>'memory')::bigint-mem ELSE 1152921504606846976 END,
    'disk', CASE WHEN coalesce((cap->>'disk')::bigint,0) > 0 THEN (cap->>'disk')::bigint-disk ELSE 1152921504606846976 END,
    'runs', CASE WHEN coalesce((cap->>'runs')::bigint,0) > 0 THEN (cap->>'runs')::bigint-slots ELSE 1152921504606846976 END);
  last_free := hist->(jsonb_array_length(hist)-1)->'free';
  IF last_free = free THEN RETURN NULL; END IF;
  hist := hist || jsonb_build_array(jsonb_build_object('at', clock_timestamp(), 'free', free));
  -- Remove intervals wholly before the window, retaining its boundary entry.
  WHILE jsonb_array_length(hist) > 1 AND (hist->1->>'at')::timestamptz < clock_timestamp()-interval '60 seconds' LOOP
    hist := hist - 0;
  END LOOP;
  WHILE jsonb_array_length(hist) > 32 LOOP
    SELECT jsonb_object_agg(k, least((hist->0->'free'->>k)::numeric, (hist->1->'free'->>k)::numeric))
      INTO merged FROM unnest(ARRAY['cpus','memory','disk','runs']) k;
    hist := jsonb_set(hist, '{1}', jsonb_build_object('at', hist->0->'at', 'free', merged)) - 0;
  END LOOP;
  UPDATE host_capacity_age SET history = hist WHERE host_id = hid;
  RETURN NULL;
END $$;
CREATE TRIGGER capacity_age_transition AFTER INSERT OR DELETE OR UPDATE OF resources, state ON placements
  FOR EACH ROW EXECUTE FUNCTION lux_capacity_age_transition();

CREATE FUNCTION lux_capacity_age_invalidate() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.capacity IS DISTINCT FROM OLD.capacity OR NEW.capacity_epoch <> OLD.capacity_epoch
     OR NEW.pool_id IS DISTINCT FROM OLD.pool_id OR NEW.state <> OLD.state OR NEW.draining <> OLD.draining THEN
    DELETE FROM host_capacity_age WHERE host_id = NEW.id;
    DELETE FROM spot_pledges WHERE host_id = NEW.id;
  END IF;
  RETURN NULL;
END $$;
CREATE TRIGGER capacity_age_invalidate AFTER UPDATE OF capacity, capacity_epoch, pool_id, state, draining ON hosts
  FOR EACH ROW EXECUTE FUNCTION lux_capacity_age_invalidate();

CREATE FUNCTION lux_capacity_shape_invalidate() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.shape IS DISTINCT FROM OLD.shape THEN
    DELETE FROM host_capacity_age WHERE host_id IN (SELECT id FROM hosts WHERE pool_id = NEW.id);
  END IF;
  RETURN NULL;
END $$;
CREATE TRIGGER capacity_shape_invalidate AFTER UPDATE OF shape ON pools
  FOR EACH ROW EXECUTE FUNCTION lux_capacity_shape_invalidate();
