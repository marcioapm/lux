-- 009_placement_hints.sql — where a Run's next placement may go: a host it
-- must go to (resume --to, migrate --to) and one it must not (migrate: away
-- from where it is). Both are consumed by the placement they steer.
ALTER TABLE runs ADD COLUMN place_on text REFERENCES hosts(id);
ALTER TABLE runs ADD COLUMN avoid_host text REFERENCES hosts(id);

-- Platform hosts (tenant_id NULL) run every tenant's Runs: a tenant may read
-- them (their names on its placements, hosts ls), never write them.
CREATE POLICY platform_read ON hosts FOR SELECT USING (tenant_id IS NULL);
