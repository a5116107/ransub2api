-- Existing jobs have unknown creation provenance; do not infer it from accounts.
-- Rollback retains this additive column and every unresolved job/hold.
ALTER TABLE batch_image_jobs
    ADD COLUMN IF NOT EXISTS route_fingerprint VARCHAR(64) NOT NULL DEFAULT '';
