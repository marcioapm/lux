-- 093_people_backfill.sql — every tenant key becomes a service account's,
-- and Runs get owners (docs/operators.md, "Upgrading to people").
--
-- A tenant key inserted without a person or a service account (keys made
-- before this migration, `luxd admin create-key`, older tooling) gets a
-- service account of its own: named after the key (lowercase DNS-label
-- style, deduplicated per tenant), its role from the key's scopes (admin →
-- admin, run → member, else viewer), no owner. The key's hash and its
-- effective scopes stay what they were.

CREATE FUNCTION lux_sa_name(tenant text, raw text) RETURNS text
LANGUAGE plpgsql AS $$
DECLARE
  base text := left(trim(both '-' from regexp_replace(lower(coalesce(raw, '')), '[^a-z0-9]+', '-', 'g')), 56);
  candidate text;
  n int := 1;
BEGIN
  base := trim(both '-' from base);
  IF base = '' THEN base := 'key'; END IF;
  candidate := base;
  WHILE EXISTS (SELECT 1 FROM service_accounts WHERE tenant_id = tenant AND name = candidate) LOOP
    n := n + 1;
    candidate := base || '-' || n;
  END LOOP;
  RETURN candidate;
END $$;

CREATE FUNCTION lux_scopes_role(scopes text[]) RETURNS text
LANGUAGE sql IMMUTABLE AS $$
  SELECT CASE WHEN 'admin' = ANY (scopes) THEN 'admin' WHEN 'run' = ANY (scopes) THEN 'member' ELSE 'viewer' END $$;

CREATE FUNCTION lux_key_service_account() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE sa text;
BEGIN
  IF NEW.tenant_id IS NULL OR NEW.user_id IS NOT NULL OR NEW.service_account_id IS NOT NULL THEN
    RETURN NEW;
  END IF;
  -- One name at a time per tenant: two keys of one name get two accounts.
  PERFORM pg_advisory_xact_lock(hashtext('lux_sa_name:' || NEW.tenant_id));
  sa := 'sa_' || substr(md5(random()::text || clock_timestamp()::text), 1, 16);
  INSERT INTO service_accounts (id, tenant_id, name, description, role, created_at)
    VALUES (sa, NEW.tenant_id, lux_sa_name(NEW.tenant_id, NEW.name), 'From API key ' || NEW.name, lux_scopes_role(NEW.scopes), NEW.created_at);
  NEW.service_account_id := sa;
  RETURN NEW;
END $$;

CREATE TRIGGER api_keys_service_account BEFORE INSERT ON api_keys
  FOR EACH ROW EXECUTE FUNCTION lux_key_service_account();

-- The keys already there, oldest first, so the oldest of a name keeps it.
DO $$
DECLARE k record; sa text;
BEGIN
  FOR k IN SELECT id, tenant_id, name, scopes, created_at FROM api_keys
      WHERE tenant_id IS NOT NULL AND service_account_id IS NULL AND user_id IS NULL ORDER BY created_at, id LOOP
    sa := 'sa_' || substr(md5(k.id || random()::text), 1, 16);
    INSERT INTO service_accounts (id, tenant_id, name, description, role, created_at)
      VALUES (sa, k.tenant_id, lux_sa_name(k.tenant_id, k.name), 'From API key ' || k.name, lux_scopes_role(k.scopes), k.created_at);
    UPDATE api_keys SET service_account_id = sa WHERE id = k.id;
  END LOOP;
END $$;

-- People who submitted Runs, by email (the first spelling seen wins).
INSERT INTO users (id, email, name, created_at)
SELECT DISTINCT ON (lower(submitted_by_email)) 'usr_' || substr(md5(lower(submitted_by_email)), 1, 16), submitted_by_email, '', min(created_at) OVER (PARTITION BY lower(submitted_by_email))
  FROM runs WHERE submitted_by_email IS NOT NULL AND submitted_by_email <> ''
  ORDER BY lower(submitted_by_email), created_at
ON CONFLICT DO NOTHING;

-- Run owners: the person, else the service account of the key, else none.
UPDATE runs r SET owner_user_id = u.id
  FROM users u
 WHERE r.submitted_by_email IS NOT NULL AND lower(u.email) = lower(r.submitted_by_email);
UPDATE runs r SET owner_sa_id = k.service_account_id
  FROM api_keys k
 WHERE r.owner_user_id IS NULL AND r.submitted_by_key = k.id AND k.service_account_id IS NOT NULL
   AND k.tenant_id = r.tenant_id;

-- Default-tenant memberships need luxd's configuration: luxd runs that step
-- once at its next start (lux_upgrades 'people-default-tenant').
