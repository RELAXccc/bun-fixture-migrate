-- The schema of the first release. Master data (currencies, countries, plans
-- and their prices, features, roles and permissions, translations, the
-- category tree) is loaded by dbfixture on a new database and changed by
-- generated fixture migrations afterwards. Everything else is the
-- application's own data, and points at master data.
CREATE TYPE plan_tier AS ENUM ('free', 'team', 'business');

--bun:split

CREATE TABLE currencies (
    id          serial PRIMARY KEY,
    code        char(3) NOT NULL UNIQUE,
    name        text NOT NULL,
    symbol      text NOT NULL,
    minor_units smallint NOT NULL
);

--bun:split

CREATE TABLE countries (
    id          serial PRIMARY KEY,
    code        char(2) NOT NULL UNIQUE,
    name        text NOT NULL,
    currency_id integer NOT NULL REFERENCES currencies (id),
    tax_rate    numeric(5,4) NOT NULL
);

--bun:split

-- Plans are named by code everywhere; the uuid is the database's.
CREATE TABLE plans (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    code       text NOT NULL UNIQUE,
    name       text NOT NULL,
    tier       plan_tier NOT NULL,
    active     boolean NOT NULL,
    sort_order integer NOT NULL,
    settings   jsonb NOT NULL,
    tags       text[] NOT NULL
);

--bun:split

-- Prices are effective-dated: a price change is a new row, so an invoice
-- keeps pointing at the price it was issued with.
CREATE TABLE plan_prices (
    id          bigserial PRIMARY KEY,
    plan_id     uuid NOT NULL REFERENCES plans (id),
    currency_id integer NOT NULL REFERENCES currencies (id),
    valid_from  date NOT NULL,
    amount      numeric(12,2) NOT NULL,
    UNIQUE (plan_id, currency_id, valid_from)
);

--bun:split

CREATE TABLE features (
    id   serial PRIMARY KEY,
    code text NOT NULL UNIQUE,
    name text NOT NULL,
    unit text
);

--bun:split

-- A grant of a feature to a plan; a NULL quota is unlimited.
CREATE TABLE plan_features (
    plan_id    uuid NOT NULL REFERENCES plans (id),
    feature_id integer NOT NULL REFERENCES features (id),
    quota      integer,
    PRIMARY KEY (plan_id, feature_id)
);

--bun:split

CREATE TABLE tenants (
    id         bigserial PRIMARY KEY,
    name       text NOT NULL UNIQUE,
    country_id integer NOT NULL REFERENCES countries (id),
    created_at timestamptz NOT NULL DEFAULT now()
);

--bun:split

-- Roles with no tenant are master data; a tenant's custom roles live in the
-- same table and draw their ids from the same sequence.
CREATE TABLE roles (
    id        serial PRIMARY KEY,
    tenant_id bigint REFERENCES tenants (id),
    code      text NOT NULL,
    name      text NOT NULL
);

--bun:split

CREATE UNIQUE INDEX roles_master_code ON roles (code) WHERE tenant_id IS NULL;

--bun:split

CREATE UNIQUE INDEX roles_tenant_code ON roles (tenant_id, code) WHERE tenant_id IS NOT NULL;

--bun:split

CREATE TABLE permissions (
    id          serial PRIMARY KEY,
    code        text NOT NULL UNIQUE,
    description text NOT NULL
);

--bun:split

CREATE TABLE role_permissions (
    role_id       integer NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
    permission_id integer NOT NULL REFERENCES permissions (id),
    PRIMARY KEY (role_id, permission_id)
);

--bun:split

CREATE TABLE translations (
    id      bigserial PRIMARY KEY,
    locale  text NOT NULL,
    key     text NOT NULL,
    value   text NOT NULL,
    context text,
    UNIQUE (locale, key)
);

--bun:split

CREATE TABLE categories (
    id        serial PRIMARY KEY,
    slug      text NOT NULL UNIQUE,
    parent_id integer REFERENCES categories (id),
    name      text NOT NULL,
    position  integer NOT NULL
);

--bun:split

CREATE TABLE users (
    id        bigserial PRIMARY KEY,
    tenant_id bigint NOT NULL REFERENCES tenants (id),
    email     text NOT NULL UNIQUE,
    role_id   integer NOT NULL REFERENCES roles (id)
);

--bun:split

CREATE TABLE subscriptions (
    id          bigserial PRIMARY KEY,
    tenant_id   bigint NOT NULL REFERENCES tenants (id),
    plan_id     uuid NOT NULL REFERENCES plans (id),
    currency_id integer NOT NULL REFERENCES currencies (id),
    seats       integer NOT NULL,
    started_at  timestamptz NOT NULL DEFAULT now()
);

--bun:split

CREATE TABLE invoices (
    id              bigserial PRIMARY KEY,
    subscription_id bigint NOT NULL REFERENCES subscriptions (id),
    plan_price_id   bigint NOT NULL REFERENCES plan_prices (id),
    amount          numeric(12,2) NOT NULL,
    issued_at       timestamptz NOT NULL DEFAULT now()
);
