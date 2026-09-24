-- 007_settings.sql — deployment-wide values. `deployment` names this lux
-- database in cloud tags (lux:deployment), so deployments sharing a cloud
-- account tell their instances apart.
CREATE TABLE settings (
  name  text PRIMARY KEY,
  value text NOT NULL
);
INSERT INTO settings (name, value) VALUES ('deployment', replace(gen_random_uuid()::text, '-', ''));
