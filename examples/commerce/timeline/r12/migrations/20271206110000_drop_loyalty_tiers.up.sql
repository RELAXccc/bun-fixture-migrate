-- The tiers go with the programme, rows and all: this migration is what takes
-- them out of every database, which baseline -force recorded.
DROP TABLE public.loyalty_tiers;
