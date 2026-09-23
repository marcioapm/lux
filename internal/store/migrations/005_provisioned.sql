-- 005_provisioned.sql — a provisioned host carries what it was launched
-- with, so it can be terminated whatever becomes of its pool: the pool's
-- template at launch (its region, above all).
ALTER TABLE hosts ADD COLUMN launch_template jsonb;
