-- 092_people.sql — people, their roles per tenant, service accounts,
-- invitations, and who owns a Run (docs/security.md).
--
-- users: a person, identified by the email Cloudflare Access verified.
-- Emails compare case-insensitively (a unique index on lower(email), not
-- citext: no extension to install). A person belongs to no tenant, so the
-- table is the system's: handlers read it by ids taken from tenant rows.
CREATE TABLE users (
  id           text PRIMARY KEY,
  email        text NOT NULL,
  name         text NOT NULL DEFAULT '',
  picture      text NOT NULL DEFAULT '',
  operator     boolean NOT NULL DEFAULT false,
  created_at   timestamptz NOT NULL DEFAULT now(),
  last_seen_at timestamptz,
  disabled_at  timestamptz
);
CREATE UNIQUE INDEX users_email ON users (lower(email));

CREATE TABLE memberships (
  tenant_id     text NOT NULL REFERENCES tenants(id),
  user_id       text NOT NULL REFERENCES users(id),
  role          text NOT NULL CHECK (role IN ('viewer', 'member', 'admin', 'owner')),
  added_by      text,
  invitation_id text,
  created_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, user_id)
);
CREATE INDEX memberships_user ON memberships (user_id);

CREATE TABLE service_accounts (
  id            text PRIMARY KEY,
  tenant_id     text NOT NULL REFERENCES tenants(id),
  name          text NOT NULL,
  description   text NOT NULL DEFAULT '',
  role          text NOT NULL CHECK (role IN ('viewer', 'member', 'admin')),
  owner_user_id text REFERENCES users(id),
  created_by    text REFERENCES users(id),
  created_at    timestamptz NOT NULL DEFAULT now(),
  disabled_at   timestamptz,
  UNIQUE (tenant_id, name)
);

-- One live invitation (pending or expired: neither accepted nor revoked)
-- per tenant and email.
CREATE TABLE invitations (
  id               text PRIMARY KEY,
  tenant_id        text NOT NULL REFERENCES tenants(id),
  code             text NOT NULL UNIQUE,
  email            text NOT NULL,
  name             text NOT NULL DEFAULT '',
  role             text NOT NULL CHECK (role IN ('viewer', 'member', 'admin', 'owner')),
  invited_by       text REFERENCES users(id),
  created_at       timestamptz NOT NULL DEFAULT now(),
  expires_at       timestamptz NOT NULL,
  renewed_at       timestamptz,
  renewed_by       text REFERENCES users(id),
  accepted_at      timestamptz,
  accepted_user_id text REFERENCES users(id),
  revoked_at       timestamptz,
  revoked_by       text REFERENCES users(id)
);
CREATE UNIQUE INDEX invitations_live ON invitations (tenant_id, lower(email))
  WHERE accepted_at IS NULL AND revoked_at IS NULL;

-- A key is a person's (user_id), a service account's, or neither: an
-- operator's key owned by no person (a platform key). Effective scopes come
-- from the person's or the account's role at auth time; scopes stays for
-- operator keys and keys made before this migration.
ALTER TABLE api_keys
  ADD COLUMN user_id text REFERENCES users(id),
  ADD COLUMN service_account_id text REFERENCES service_accounts(id),
  ADD COLUMN read_only boolean NOT NULL DEFAULT false,
  ADD COLUMN expires_at timestamptz,
  ADD COLUMN created_by text REFERENCES users(id),
  ADD COLUMN prefix text NOT NULL DEFAULT '',
  ADD CONSTRAINT api_keys_one_principal CHECK (user_id IS NULL OR service_account_id IS NULL);
CREATE INDEX api_keys_user ON api_keys (user_id) WHERE user_id IS NOT NULL;
CREATE INDEX api_keys_service_account ON api_keys (service_account_id) WHERE service_account_id IS NOT NULL;

-- Nullable columns without defaults: catalog-only, no rewrite of runs.
ALTER TABLE runs
  ADD COLUMN owner_user_id text,
  ADD COLUMN owner_sa_id text,
  ADD CONSTRAINT runs_one_owner CHECK (owner_user_id IS NULL OR owner_sa_id IS NULL) NOT VALID;
CREATE INDEX runs_owner_user ON runs (tenant_id, owner_user_id) WHERE owner_user_id IS NOT NULL;
CREATE INDEX runs_owner_sa ON runs (tenant_id, owner_sa_id) WHERE owner_sa_id IS NOT NULL;

-- Upgrade steps luxd runs once, at startup, because they need its
-- configuration (the default tenant, the operators).
CREATE TABLE lux_upgrades (
  name    text PRIMARY KEY,
  done_at timestamptz NOT NULL DEFAULT now()
);

-- A tenant reads the people it can name: its members, the owners of its
-- Runs (a removed member's Runs stay theirs) and operators (who invite);
-- only the system writes them.
ALTER TABLE users ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_rows ON users USING (lux_system()) WITH CHECK (lux_system());
CREATE POLICY tenant_reads ON users FOR SELECT USING (
  operator
  OR EXISTS (SELECT 1 FROM memberships m WHERE m.user_id = users.id AND m.tenant_id = lux_tenant())
  OR EXISTS (SELECT 1 FROM runs r WHERE r.owner_user_id = users.id AND r.tenant_id = lux_tenant()));
ALTER TABLE lux_upgrades ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON lux_upgrades USING (lux_system()) WITH CHECK (lux_system());
DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['memberships', 'service_accounts', 'invitations'] LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('CREATE POLICY tenant_rows ON %I USING (tenant_id = lux_tenant() OR lux_system()) WITH CHECK (tenant_id = lux_tenant() OR lux_system())', t);
  END LOOP;
END $$;
