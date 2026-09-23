-- 004_pools.sql — a removed pool is retired, not deleted: the provisioner
-- still needs its provider and template to terminate its hosts.
ALTER TABLE pools ADD COLUMN retired boolean NOT NULL DEFAULT false;
