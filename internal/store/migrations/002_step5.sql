-- 002_step5.sql — stored bytes per tenant, for the storage quota: an
-- index-only sum over blobs not yet deleted.
CREATE INDEX blobs_tenant_stored ON blobs (tenant_id) INCLUDE (size) WHERE location <> 'deleted';
