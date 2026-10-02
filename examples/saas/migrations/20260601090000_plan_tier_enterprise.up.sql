-- Not a .tx.up.sql: bun runs this file outside a transaction, so the new
-- value is committed before the fixture migration of this release inserts a
-- plan with it. A value added in a transaction cannot be used in it.
ALTER TYPE plan_tier ADD VALUE 'enterprise';
