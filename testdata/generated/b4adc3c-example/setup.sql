-- The database the change set beside it runs against: the example project's
-- schema, and its fixture file as it was before the migration. The change set
-- is examples/basic/migrations/20260930165255_fixture_plan_prices.go as of
-- commit b4adc3c, copied unchanged.
DROP TABLE IF EXISTS features, plans, currencies, bun_migrations, bun_migration_locks CASCADE;
CREATE TABLE currencies (
    id     bigint PRIMARY KEY,
    code   text NOT NULL UNIQUE,
    symbol text NOT NULL
);
CREATE TABLE plans (
    id          bigserial PRIMARY KEY,
    name        text NOT NULL UNIQUE,
    currency_id bigint NOT NULL REFERENCES currencies (id),
    price_cents bigint NOT NULL DEFAULT 0,
    seats       bigint NOT NULL DEFAULT 1,
    settings    jsonb NOT NULL DEFAULT '{}',
    note        text
);
CREATE TABLE features (
    id      bigserial PRIMARY KEY,
    plan_id bigint NOT NULL REFERENCES plans (id),
    code    text NOT NULL,
    quota   bigint NOT NULL DEFAULT 0,
    UNIQUE (plan_id, code)
);
INSERT INTO currencies (id, code, symbol) VALUES (1, 'EUR', '€'), (2, 'USD', '$');
INSERT INTO plans (id, name, currency_id, price_cents, seats, settings, note) VALUES
    (1, 'free', 1, 0, 1, '{"trial_days": 0}', NULL),
    (2, 'team', 1, 2000, 10, '{"sso": false, "trial_days": 14}', NULL);
SELECT setval(pg_get_serial_sequence('plans', 'id'), 2);
INSERT INTO features (plan_id, code, quota) VALUES (1, 'api', 100), (2, 'api', 5000);
