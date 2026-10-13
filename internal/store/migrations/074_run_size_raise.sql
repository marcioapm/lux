-- 074_run_size_raise.sql — why a Run's size is larger than its resume asked (docs/design/sizes.md, phase 4).
--
-- runs.size_raised_from and size_raised_for: set when a resume in a shaped
-- pool asked for a size (size_raised_from: the one its request resolved
-- to) whose disk could not hold the Run's saved state, and luxd gave it the
-- smallest larger size that could (size_raised_for 'saved state'). Both
-- NULL otherwise, every Run before this migration. Rewritten by every
-- resume that sizes the Run, cleared with resolved_size.
--
-- Nullable columns without defaults: catalog-only, no table rewrite.
SET LOCAL lock_timeout = '5s';
ALTER TABLE runs
  ADD COLUMN size_raised_from text,
  ADD COLUMN size_raised_for  text;
