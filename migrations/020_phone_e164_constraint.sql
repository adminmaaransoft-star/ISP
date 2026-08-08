-- +goose Up
-- Session DB-020 | DoD Phase 2 Step 4 | E.164 phone validation, defense in depth
--
-- Both subscribers.mobile_number and franchises.mobile_number have always
-- carried an "E.164: +91XXXXXXXXXX"-style comment documenting the expected
-- format, but nothing ever enforced it at any layer — the DoD audit found
-- zero validation logic anywhere in the codebase. Application-level
-- validation now exists (pkg/validate.E164, checked in
-- internal/api's CreateSubscriber and both notification send paths), but a
-- DB-level CHECK is added here too, matching this schema's existing pattern
-- of enforcing invariants at the database as well as the application
-- (chk_gst_logic, the tickets category CHECK, etc.) — so no future code
-- path, including ones that insert directly rather than through the API,
-- can silently store a malformed number.
--
-- Existing seed/demo data (+919876543210 and similar) already conforms;
-- confirmed before writing this migration, not assumed.
ALTER TABLE subscribers
    ADD CONSTRAINT chk_subscribers_mobile_e164
    CHECK (mobile_number ~ '^\+[1-9][0-9]{1,14}$');

ALTER TABLE franchises
    ADD CONSTRAINT chk_franchises_mobile_e164
    CHECK (mobile_number ~ '^\+[1-9][0-9]{1,14}$');

-- +goose Down
ALTER TABLE subscribers DROP CONSTRAINT IF EXISTS chk_subscribers_mobile_e164;
ALTER TABLE franchises DROP CONSTRAINT IF EXISTS chk_franchises_mobile_e164;
