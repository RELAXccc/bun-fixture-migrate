-- The database the change set beside it runs against: the schema, and old.yml.
-- The change set was rendered by bun-fixture-migrate at commit b87be44 from
-- old.yml and new.yml with fixture-migrate.yml, and is kept unchanged.
DROP TABLE IF EXISTS corpus_features, corpus_plans, corpus_currencies, bun_migrations, bun_migration_locks, corpus_audit CASCADE;
CREATE TABLE corpus_currencies (
    id     bigint PRIMARY KEY,
    code   text NOT NULL UNIQUE,
    symbol text NOT NULL
);
CREATE TABLE corpus_plans (
    id          bigserial PRIMARY KEY,
    name        text NOT NULL UNIQUE,
    currency_id bigint NOT NULL REFERENCES corpus_currencies (id),
    price_cents bigint NOT NULL DEFAULT 0,
    note        text,
    settings    jsonb NOT NULL DEFAULT '{}',
    tags        text[] NOT NULL DEFAULT '{}'
);
CREATE TABLE corpus_features (
    id      bigserial PRIMARY KEY,
    plan_id bigint NOT NULL REFERENCES corpus_plans (id) ON DELETE CASCADE,
    code    text NOT NULL,
    quota   bigint NOT NULL DEFAULT 0,
    UNIQUE (plan_id, code)
);
INSERT INTO corpus_currencies (id, code, symbol) VALUES (1, 'EUR', '€'), (2, 'USD', '$'), (3, 'GBP', '£');
INSERT INTO corpus_plans (id, name, currency_id, price_cents, note, settings, tags) VALUES
    (1, 'free', 1, 0, NULL, '{"sso": false}', '{basic}'),
    (2, 'team', 1, 2000, 'old "note"', '{"sso": false, "seats": 10}', '{team,popular}');
SELECT setval(pg_get_serial_sequence('corpus_plans', 'id'), 2);
INSERT INTO corpus_features (plan_id, code, quota) VALUES (1, 'api', 100), (2, 'api', 5000), (2, 'sso', 1);
