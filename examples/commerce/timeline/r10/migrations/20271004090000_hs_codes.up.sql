-- Harmonised System codes, which customs declarations name for every parcel
-- leaving the EU. Looked up by code, through an index of its own.
CREATE TABLE billing.hs_codes (
    id           serial PRIMARY KEY,
    code         char(6) NOT NULL,
    chapter      smallint NOT NULL,
    description  jsonb NOT NULL,
    duty_free_eu boolean NOT NULL
);

--bun:split

CREATE INDEX hs_codes_code ON billing.hs_codes (code);
