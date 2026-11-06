-- Never infer ownership or terminal evidence for existing provider jobs.
-- Retain these columns and unresolved members during application rollback.
ALTER TABLE batch_image_jobs
    ADD COLUMN IF NOT EXISTS upstream_key_lease JSONB,
    ADD COLUMN IF NOT EXISTS upstream_key_terminal_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS upstream_key_released_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_batch_image_jobs_pending_key_release
    ON batch_image_jobs (upstream_key_terminal_at, id)
    WHERE upstream_key_terminal_at IS NOT NULL AND upstream_key_released_at IS NULL;
