-- 003_git.sql — the last commit a Run pushed, per repository, for push
-- leases. Kept by luxd, not in the checkout: the workload controls its
-- checkout and must not be able to move the lease.
ALTER TABLE runs ADD COLUMN pushed jsonb NOT NULL DEFAULT '{}';
