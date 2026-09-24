-- 009_samples.sql — history: resource use and system state over time.
--
-- Rows at three resolutions: 0 is raw (one per heartbeat, or per system
-- tick), 60 and 3600 are rollups of the resolution below (averages of
-- levels, maxima of peaks, last of counters). The reaper rolls up and
-- expires them (docs/telemetry.md). Counters (cpu_seconds, net bytes) are
-- cumulative; rates are differences between rows.

CREATE TABLE host_samples (
  host_id     text NOT NULL REFERENCES hosts(id),
  res         int  NOT NULL,
  at          timestamptz NOT NULL,
  cpu_seconds float8,
  mem_bytes   bigint,
  disk_bytes  bigint,
  -- Live placements, and what they asked for.
  placements  int NOT NULL DEFAULT 0,
  alloc_cpus  float8 NOT NULL DEFAULT 0,
  alloc_mem   bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (host_id, res, at)
);

CREATE TABLE placement_samples (
  run_id      text NOT NULL,
  epoch       int  NOT NULL,
  tenant_id   text NOT NULL REFERENCES tenants(id),
  res         int  NOT NULL,
  at          timestamptz NOT NULL,
  cpu_seconds float8,
  mem_bytes   bigint,
  disk_bytes  bigint,
  pids        int,
  net_rx      bigint,
  net_tx      bigint,
  PRIMARY KEY (run_id, epoch, res, at)
);

-- Per tenant, so the system view can be narrowed; the whole system is the
-- row with tenant_id ''.
CREATE TABLE system_samples (
  tenant_id     text NOT NULL,
  res           int  NOT NULL,
  at            timestamptz NOT NULL,
  -- Starts and finishes counted up to here (see sampleSystem).
  window_end    timestamptz,
  runs          jsonb NOT NULL DEFAULT '{}', -- by state
  busy          int NOT NULL DEFAULT 0,
  idle          int NOT NULL DEFAULT 0,
  queued        int NOT NULL DEFAULT 0,
  started       int NOT NULL DEFAULT 0, -- first starts in the window
  finished      int NOT NULL DEFAULT 0,
  start_p50     float8,
  start_p95     float8,
  hosts         jsonb NOT NULL DEFAULT '{}', -- by state
  cap_cpus      float8 NOT NULL DEFAULT 0,
  cap_mem       bigint NOT NULL DEFAULT 0,
  alloc_cpus    float8 NOT NULL DEFAULT 0,
  alloc_mem     bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (tenant_id, res, at)
);

-- Rollups and retention walk each table by resolution and time.
CREATE INDEX host_samples_res_at ON host_samples (res, at);
CREATE INDEX placement_samples_res_at ON placement_samples (res, at);
CREATE INDEX system_samples_res_at ON system_samples (res, at);
-- The system sampler counts starts and finishes in a time window.
CREATE INDEX runs_first_started ON runs (first_started_at) WHERE first_started_at IS NOT NULL;
CREATE INDEX runs_finished ON runs (finished_at) WHERE finished_at IS NOT NULL;
-- Runs by host (lux ls --host): every placement, not only live ones.
CREATE INDEX placements_host_run ON placements (host_id, run_id);

-- Placement samples are a tenant's rows; host and system samples are the
-- platform's and read only through luxd, which scopes them.
ALTER TABLE placement_samples ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_rows ON placement_samples USING (tenant_id = lux_tenant() OR lux_system()) WITH CHECK (tenant_id = lux_tenant() OR lux_system());
ALTER TABLE host_samples ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON host_samples USING (lux_system()) WITH CHECK (lux_system());
ALTER TABLE system_samples ENABLE ROW LEVEL SECURITY;
CREATE POLICY system_only ON system_samples USING (lux_system()) WITH CHECK (lux_system());
