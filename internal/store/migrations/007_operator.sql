-- 007_operator.sql — operator keys: the platform's, not a tenant's. They
-- see every tenant; luxd resolves the owning tenant of what they act on and
-- acts within it, so every write still runs under that tenant's RLS.
ALTER TABLE api_keys ALTER COLUMN tenant_id DROP NOT NULL;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_operator_platform
  CHECK ((tenant_id IS NULL) = ('operator' = ANY (scopes)));
