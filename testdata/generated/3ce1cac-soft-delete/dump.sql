SELECT string_agg(concat_ws(' ', id, code, symbol), ', ' ORDER BY id) FROM corpus_sd_currencies WHERE deleted_at IS NULL;
SELECT string_agg(concat_ws(' ', id, name, currency_id, price_cents), ', ' ORDER BY id) FROM corpus_sd_plans WHERE deleted_at IS NULL;
