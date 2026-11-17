-- Returned usage only. Zero token limits (in settings JSON) mean unlimited.
-- Retained completed lease IDs make settlement idempotent across processes.
ALTER TABLE monitoring_probe_budget_counters
    ADD COLUMN IF NOT EXISTS tokens_used BIGINT NOT NULL DEFAULT 0 CHECK (tokens_used >= 0),
    ADD COLUMN IF NOT EXISTS tokens_regular BIGINT NOT NULL DEFAULT 0 CHECK (tokens_regular >= 0),
    ADD COLUMN IF NOT EXISTS usage_reported BIGINT NOT NULL DEFAULT 0 CHECK (usage_reported >= 0),
    ADD COLUMN IF NOT EXISTS usage_unknown BIGINT NOT NULL DEFAULT 0 CHECK (usage_unknown >= 0);
ALTER TABLE monitoring_probe_budget_leases
    ADD COLUMN IF NOT EXISTS meter_tokens BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS released_at TIMESTAMPTZ;
