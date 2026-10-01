SELECT string_agg(concat_ws(' ', id, code, symbol), ', ' ORDER BY id) FROM corpus_currencies;
SELECT string_agg(concat_ws(' ', id, name, currency_id, price_cents, note, settings, tags), ', ' ORDER BY id) FROM corpus_plans;
SELECT string_agg(concat_ws(' ', p.name, f.code, f.quota), ', ' ORDER BY p.name, f.code) FROM corpus_features f JOIN corpus_plans p ON p.id = f.plan_id;
