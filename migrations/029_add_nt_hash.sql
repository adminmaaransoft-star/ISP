-- +goose Up
-- Session DB-029 | FR-AAA-006 | DBD §6.2 subscribers.nt_hash | MDS §4.18
--
-- EAP-MSCHAPv2 verifies a challenge response, which requires recomputing it
-- server-side from the NT hash. bcrypt is one-way and cannot, which is the
-- whole reason FR-AAA-005/006 sat unimplemented.
--
-- Opt-in by design (decision 2026-08-14): NULL means this subscriber
-- authenticates by PAP against bcrypt only, exactly as before. A row only
-- gains an nt_hash when someone deliberately enrols it for EAP, so the
-- population exposed to an unsalted-MD4 credential is the population that
-- needed it rather than the whole base.
ALTER TABLE subscribers
    ADD COLUMN IF NOT EXISTS nt_hash BYTEA;

-- The NT hash is exactly MD4(UTF-16LE(password)) — 16 bytes, always. A
-- wrong-length value would fail deep inside the DES step of RFC 2759's
-- ChallengeResponse with no useful diagnostic, so it is rejected at the
-- boundary instead.
ALTER TABLE subscribers
    DROP CONSTRAINT IF EXISTS chk_subscribers_nt_hash_len;
ALTER TABLE subscribers
    ADD CONSTRAINT chk_subscribers_nt_hash_len
        CHECK (nt_hash IS NULL OR octet_length(nt_hash) = 16);

-- Which subscribers are EAP-enrolled is an operational question ("has the
-- hotspot rollout reached this segment yet"), and answering it should not
-- scan the whole table.
CREATE INDEX IF NOT EXISTS idx_subscribers_eap_enrolled ON subscribers(id)
    WHERE nt_hash IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_subscribers_eap_enrolled;
ALTER TABLE subscribers DROP CONSTRAINT IF EXISTS chk_subscribers_nt_hash_len;
ALTER TABLE subscribers DROP COLUMN IF EXISTS nt_hash;
