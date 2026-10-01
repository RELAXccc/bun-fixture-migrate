-- The table comes back empty: a rollback does not bring the tiers back.
CREATE TABLE public.loyalty_tiers (
    id               serial PRIMARY KEY,
    code             text NOT NULL UNIQUE,
    name             jsonb NOT NULL,
    min_points       integer NOT NULL,
    discount_percent numeric(4,1) NOT NULL
);
