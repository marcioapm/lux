CREATE INDEX placements_recent_ended ON placements (ended_at) WHERE ended_at IS NOT NULL;
CREATE INDEX placements_recent_created ON placements (created_at) WHERE ended_at IS NULL;
