-- Existing resources have unknown route provenance. Never backfill from a mutable account.
-- Rollback: retain this additive column when rolling back the application binary.
ALTER TABLE gemini_cached_contents
    ADD COLUMN IF NOT EXISTS route_fingerprint VARCHAR(64) NOT NULL DEFAULT '';
