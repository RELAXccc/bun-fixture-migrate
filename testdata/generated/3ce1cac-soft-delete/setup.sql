-- The database the change set beside it runs against: the schema, and old.yml,
-- whose rows with a deleted_at dbfixture seeds soft-deleted. The change set was
-- rendered by bun-fixture-migrate at commit 3ce1cac from old.yml and new.yml
-- with fixture-migrate.yml, and is kept unchanged.
DROP TABLE IF EXISTS corpus_sd_plans, corpus_sd_currencies, bun_migrations, bun_migration_locks CASCADE;
CREATE TABLE corpus_sd_currencies (
    id         bigint PRIMARY KEY,
    code       text NOT NULL UNIQUE,
    symbol     text NOT NULL,
    deleted_at timestamptz
);
CREATE TABLE corpus_sd_plans (
    id          bigserial PRIMARY KEY,
    name        text NOT NULL,
    currency_id bigint NOT NULL REFERENCES corpus_sd_currencies (id),
    price_cents bigint NOT NULL,
    deleted_at  timestamptz
);
CREATE UNIQUE INDEX corpus_sd_plans_name_live ON corpus_sd_plans (name) WHERE deleted_at IS NULL;
INSERT INTO corpus_sd_currencies (id, code, symbol, deleted_at) VALUES
    (1, 'EUR', '€', NULL), (2, 'USD', '$', NULL), (3, 'GBP', '£', '2026-03-01Z');
INSERT INTO corpus_sd_plans (id, name, currency_id, price_cents, deleted_at) VALUES
    (1, 'free', 1, 0, NULL), (2, 'team', 1, 2500, NULL), (3, 'legacy', 2, 900, '2026-02-01Z'),
    (4, 'pro', 2, 5000, '2026-01-01Z');
SELECT setval(pg_get_serial_sequence('corpus_sd_plans', 'id'), 4);
