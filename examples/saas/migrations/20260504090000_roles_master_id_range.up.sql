-- Master roles and a tenant's custom roles share roles.id. The fixture file
-- names the ids of the master roles, and a tenant's role takes the next value
-- of the sequence, so the next master role's id may already be a tenant's.
-- Master roles get the ids below 10000 from now on; tenants' roles, the ids
-- the sequence hands out above it. (known: F3)
SELECT setval(pg_get_serial_sequence('roles', 'id'), GREATEST(10000, (SELECT max(id) FROM roles)));
