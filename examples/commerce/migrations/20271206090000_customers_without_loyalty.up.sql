-- The loyalty programme ends: customers no longer have a tier.
ALTER TABLE public.customers DROP COLUMN loyalty_tier_id;
