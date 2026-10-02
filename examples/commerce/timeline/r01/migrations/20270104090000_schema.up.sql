-- The schema of the first release of the shop's back office. Three schemas:
-- billing (money: currencies, tax rates, payment providers), catalog (the
-- category tree and the products merchandisers keep in it) and public
-- (countries, shipping, the storefront menu, mail senders, the back office's
-- roles, and the shop's own data: customers, orders, admin users).
--
-- Master data is loaded by dbfixture into a new database and changed by
-- generated fixture migrations afterwards. Products, customers, orders and
-- admin users are the application's, and point at master data.
CREATE EXTENSION IF NOT EXISTS citext;

--bun:split

CREATE SCHEMA billing;

--bun:split

CREATE SCHEMA catalog;

--bun:split

CREATE TYPE billing.tax_class AS ENUM ('standard', 'reduced', 'zero');

--bun:split

CREATE TYPE billing.provider_kind AS ENUM ('card', 'wallet', 'bank_transfer', 'bnpl');

--bun:split

-- ISO 4217. The code is the primary key: every table that names a currency
-- names it by its code.
CREATE TABLE billing.currencies (
    code        char(3) PRIMARY KEY,
    name        text NOT NULL,
    symbol      text NOT NULL,
    minor_units smallint NOT NULL
);

--bun:split

-- ISO 3166-1. Display names are translated, a JSON object per locale.
CREATE TABLE public.countries (
    id            smallserial PRIMARY KEY,
    iso2          char(2) NOT NULL UNIQUE,
    iso3          char(3) NOT NULL UNIQUE,
    name          jsonb NOT NULL,
    currency_code char(3) NOT NULL REFERENCES billing.currencies (code),
    eu_member     boolean NOT NULL
);

--bun:split

-- VAT by country and class, effective-dated: a change of rate closes the
-- current row (valid_to) and opens the next one, so an order keeps the rate
-- it was taxed at. At most one rate per country and class is open.
CREATE TABLE billing.tax_rates (
    id         serial PRIMARY KEY,
    country_id smallint NOT NULL REFERENCES public.countries (id),
    tax_class  billing.tax_class NOT NULL,
    rate       numeric(6,4) NOT NULL,
    valid_from date NOT NULL,
    valid_to   date,
    CHECK (valid_to IS NULL OR valid_to > valid_from)
);

--bun:split

CREATE UNIQUE INDEX tax_rates_open ON billing.tax_rates (country_id, tax_class) WHERE valid_to IS NULL;

--bun:split

-- Payment providers. The fixture file brings the ones every shop has; ops add
-- local ones through the back office, numbered by the same sequence.
CREATE TABLE billing.payment_providers (
    id                   serial PRIMARY KEY,
    code                 text NOT NULL UNIQUE,
    kind                 billing.provider_kind NOT NULL,
    display_name         jsonb NOT NULL,
    supported_currencies char(3)[] NOT NULL,
    fee_percent          numeric(5,2) NOT NULL,
    active               boolean NOT NULL
);

--bun:split

-- The category tree. Siblings are ordered by position, unique among them. The
-- constraint is deferrable, so a transaction that reorders siblings can have
-- it checked once, at its end, rather than after every statement.
CREATE TABLE catalog.categories (
    id        serial PRIMARY KEY,
    slug      citext NOT NULL UNIQUE,
    parent_id integer REFERENCES catalog.categories (id),
    position  smallint NOT NULL,
    name      jsonb NOT NULL,
    tags      text[] NOT NULL,
    CONSTRAINT categories_sibling_position UNIQUE (parent_id, position) DEFERRABLE INITIALLY IMMEDIATE
);

--bun:split

-- A category's search-engine texts: one row per category, keyed by it.
CREATE TABLE catalog.category_seo (
    category_id      integer PRIMARY KEY REFERENCES catalog.categories (id) ON DELETE CASCADE,
    meta_title       jsonb NOT NULL,
    meta_description jsonb,
    noindex          boolean NOT NULL
);

--bun:split

-- Shipping zones are seeded once; the logistics team owns them afterwards.
CREATE TABLE public.shipping_zones (
    id                 serial PRIMARY KEY,
    code               text NOT NULL UNIQUE,
    name               text NOT NULL,
    countries          char(2)[] NOT NULL,
    free_shipping_over numeric(10,2)
);

--bun:split

-- Shipping methods. Merchandising sets the price in the back office once a
-- method exists. Checkout preselects the zone's default method.
CREATE TABLE public.shipping_methods (
    id            serial PRIMARY KEY,
    code          text NOT NULL UNIQUE,
    zone_id       integer NOT NULL REFERENCES public.shipping_zones (id),
    carrier       text NOT NULL,
    name          jsonb NOT NULL,
    base_price    numeric(10,2) NOT NULL,
    currency_code char(3) NOT NULL REFERENCES billing.currencies (code),
    min_days      smallint NOT NULL,
    max_days      smallint NOT NULL,
    is_default    boolean NOT NULL
);

--bun:split

CREATE UNIQUE INDEX shipping_methods_one_default ON public.shipping_methods (zone_id) WHERE is_default;

--bun:split

-- The storefront's header and footer menus. A position is unique per menu,
-- checked after every statement.
CREATE TABLE public.menu_items (
    id       serial PRIMARY KEY,
    menu     text NOT NULL,
    slug     text NOT NULL,
    position smallint NOT NULL,
    label    jsonb NOT NULL,
    url      text NOT NULL,
    UNIQUE (menu, slug),
    UNIQUE (menu, position)
);

--bun:split

-- The addresses transactional mail is sent from. An address is unique
-- whatever its case.
CREATE TABLE public.sender_identities (
    id           serial PRIMARY KEY,
    code         text NOT NULL UNIQUE,
    email        text NOT NULL,
    display_name text NOT NULL,
    reply_to     text
);

--bun:split

CREATE UNIQUE INDEX sender_identities_email ON public.sender_identities (lower(email));

--bun:split

-- The loyalty programme's tiers.
CREATE TABLE public.loyalty_tiers (
    id               serial PRIMARY KEY,
    code             text NOT NULL UNIQUE,
    name             jsonb NOT NULL,
    min_points       integer NOT NULL,
    discount_percent numeric(4,1) NOT NULL
);

--bun:split

-- The back office's roles. The built-in ones are master data; a merchant adds
-- custom roles of its own, and the identity numbers both.
CREATE TABLE public.admin_roles (
    id      integer GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
    code    text NOT NULL UNIQUE,
    name    text NOT NULL,
    builtin boolean NOT NULL
);

--bun:split

CREATE TABLE public.admin_permissions (
    id          serial PRIMARY KEY,
    code        text NOT NULL UNIQUE,
    description text NOT NULL
);

--bun:split

CREATE TABLE public.admin_role_permissions (
    role_id       integer NOT NULL REFERENCES public.admin_roles (id) ON DELETE CASCADE,
    permission_id integer NOT NULL REFERENCES public.admin_permissions (id),
    PRIMARY KEY (role_id, permission_id)
);

--bun:split

-- The application's own data from here on.
CREATE TABLE catalog.products (
    id            bigserial PRIMARY KEY,
    sku           citext NOT NULL UNIQUE,
    category_id   integer NOT NULL REFERENCES catalog.categories (id),
    tax_class     billing.tax_class NOT NULL,
    price         numeric(12,2) NOT NULL,
    currency_code char(3) NOT NULL REFERENCES billing.currencies (code)
);

--bun:split

CREATE TABLE public.customers (
    id              bigserial PRIMARY KEY,
    email           citext NOT NULL UNIQUE,
    country_id      smallint NOT NULL REFERENCES public.countries (id),
    loyalty_tier_id integer REFERENCES public.loyalty_tiers (id)
);

--bun:split

CREATE TABLE public.orders (
    id                  bigserial PRIMARY KEY,
    customer_id         bigint NOT NULL REFERENCES public.customers (id),
    currency_code       char(3) NOT NULL REFERENCES billing.currencies (code),
    shipping_method_id  integer NOT NULL REFERENCES public.shipping_methods (id),
    payment_provider_id integer NOT NULL REFERENCES billing.payment_providers (id),
    placed_at           timestamptz NOT NULL DEFAULT now()
);

--bun:split

CREATE TABLE public.order_lines (
    id          bigserial PRIMARY KEY,
    order_id    bigint NOT NULL REFERENCES public.orders (id),
    product_id  bigint NOT NULL REFERENCES catalog.products (id),
    quantity    integer NOT NULL,
    unit_price  numeric(12,2) NOT NULL,
    tax_rate_id integer NOT NULL REFERENCES billing.tax_rates (id)
);

--bun:split

CREATE TABLE public.admin_users (
    id      bigserial PRIMARY KEY,
    email   citext NOT NULL UNIQUE,
    role_id integer NOT NULL REFERENCES public.admin_roles (id)
);
