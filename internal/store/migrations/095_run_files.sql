-- 095_run_files.sql — the bytes of a Run's files (spec files[*].data), in
-- order, kept out of runs.spec as prompt_attachments are: every read of a
-- spec would otherwise carry them. The spec keeps each file's path, mode,
-- size and sha256. Every placement needs them (they are written on every
-- start); a resume may replace them. Cleared when the Run is terminated.
ALTER TABLE runs ADD COLUMN files jsonb;
