-- Short-lived contention tickets at the original physical admission gate.
-- No credentials, prompts, observations or prepaid request/token reservations.
-- Rollback: drain upgraded callers, then drop only this table if desired.
CREATE TABLE IF NOT EXISTS monitoring_probe_budget_waiters (
    id UUID PRIMARY KEY,
    position BIGINT GENERATED ALWAYS AS IDENTITY UNIQUE,
    site_hash TEXT NOT NULL CHECK (length(site_hash) BETWEEN 1 AND 128),
    key_hashes TEXT[] NOT NULL CHECK (cardinality(key_hashes) BETWEEN 1 AND 16),
    recovery BOOLEAN NOT NULL,
    meter_tokens BOOLEAN NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_monitoring_probe_budget_waiter_expiry
    ON monitoring_probe_budget_waiters (expires_at);
