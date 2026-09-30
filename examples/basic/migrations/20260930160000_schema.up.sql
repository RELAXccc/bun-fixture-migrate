-- The example's schema. In a bun project the schema has migrations of its own,
-- SQL or Go, in the same package as the fixture migrations and ordered with
-- them by name, so a fixture migration always runs after the schema it needs.
CREATE TABLE currencies (
    id     bigint PRIMARY KEY,
    code   text NOT NULL UNIQUE,
    symbol text NOT NULL
);

--bun:split

CREATE TABLE plans (
    id          bigserial PRIMARY KEY,
    name        text NOT NULL UNIQUE,
    currency_id bigint NOT NULL REFERENCES currencies (id),
    price_cents bigint NOT NULL DEFAULT 0,
    seats       bigint NOT NULL DEFAULT 1,
    settings    jsonb NOT NULL DEFAULT '{}',
    note        text
);

--bun:split

CREATE TABLE features (
    id      bigserial PRIMARY KEY,
    plan_id bigint NOT NULL REFERENCES plans (id),
    code    text NOT NULL,
    quota   bigint NOT NULL DEFAULT 0,
    UNIQUE (plan_id, code)
);
