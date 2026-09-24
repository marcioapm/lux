-- 006_leases.sql — a named lease with an expiry: which luxd does a
-- singleton job (the provisioner), without holding a database connection
-- across its work.
CREATE TABLE leases (
  name       text PRIMARY KEY,
  holder     text NOT NULL,
  expires_at timestamptz NOT NULL
);
