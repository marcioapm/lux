-- 008_tagged_hosts.sql — whether a provisioned host's instance carries the
-- deployment tags luxd lists by. Hosts launched before them are never in
-- those listings: their absence says nothing.
ALTER TABLE hosts ADD COLUMN tagged boolean NOT NULL DEFAULT false;
