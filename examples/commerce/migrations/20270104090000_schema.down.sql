DROP TABLE public.admin_users, public.order_lines, public.orders, public.customers, catalog.products,
    public.admin_role_permissions, public.admin_permissions, public.admin_roles, public.loyalty_tiers,
    public.sender_identities, public.menu_items, public.shipping_methods, public.shipping_zones,
    catalog.category_seo, catalog.categories, billing.payment_providers, billing.tax_rates,
    public.countries, billing.currencies;

--bun:split

DROP TYPE billing.provider_kind, billing.tax_class;

--bun:split

DROP SCHEMA catalog;

--bun:split

DROP SCHEMA billing;
