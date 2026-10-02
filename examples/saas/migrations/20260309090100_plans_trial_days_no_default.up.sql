-- The default filled the plans that existed; from now on every plan says how
-- long its trial is. Kept, a default would make bun write DEFAULT, so 14, for
-- the free plan's trial of 0 days.
ALTER TABLE plans ALTER COLUMN trial_days DROP DEFAULT;
