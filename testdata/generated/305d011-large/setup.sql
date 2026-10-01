-- The database the change set beside it runs against. The change set was
-- rendered by bun-fixture-migrate at commit 305d011 from the result that
-- render_large.go.txt builds, a test run in the module root at that commit,
-- and is kept unchanged: 1001 changes, written in parts of a hundred.
DROP TABLE IF EXISTS corpus_translations, bun_migrations, bun_migration_locks CASCADE;
CREATE TABLE corpus_translations (
    id     bigserial PRIMARY KEY,
    locale text NOT NULL,
    key    text NOT NULL,
    value  text NOT NULL,
    note   text,
    UNIQUE (locale, key)
);
INSERT INTO corpus_translations (id, locale, key, value)
SELECT i, 'de', 'k' || lpad(i::text, 5, '0'), CASE WHEN i % 3 = 1 THEN 'alt' ELSE 'weg' END
FROM generate_series(0, 1000) i WHERE i % 3 <> 0;
SELECT setval(pg_get_serial_sequence('corpus_translations', 'id'), 1000);
