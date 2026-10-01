SELECT count(*) || ' ' || md5(string_agg(concat_ws(' ', id, locale, key, value, note), ', ' ORDER BY key)) FROM corpus_translations;
