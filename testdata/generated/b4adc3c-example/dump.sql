SELECT string_agg(concat_ws(' ', id, code, symbol), ', ' ORDER BY id) FROM currencies;
SELECT string_agg(concat_ws(' ', id, name, currency_id, price_cents, seats, settings, note), ', ' ORDER BY id) FROM plans;
SELECT string_agg(concat_ws(' ', p.name, f.code, f.quota), ', ' ORDER BY p.name, f.code) FROM features f JOIN plans p ON p.id = f.plan_id;
