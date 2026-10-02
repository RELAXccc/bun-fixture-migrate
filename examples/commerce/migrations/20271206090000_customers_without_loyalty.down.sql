ALTER TABLE public.customers ADD COLUMN loyalty_tier_id integer REFERENCES public.loyalty_tiers (id);
