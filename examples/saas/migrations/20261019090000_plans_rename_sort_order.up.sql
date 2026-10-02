-- The fixture migrations of r02 and r06 insert plans with sort_order; they
-- run before this one wherever they run at all, and a new database skips
-- them (the seed guard), so they keep the name they were written with.
ALTER TABLE plans RENAME COLUMN sort_order TO position;
