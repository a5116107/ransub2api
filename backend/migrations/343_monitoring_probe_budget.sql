-- Probe budgets count transport attempts independently of account health.
-- Only hashed sites/credentials and bounded accounting windows are retained.
CREATE TABLE IF NOT EXISTS monitoring_probe_budget_counters (
    dimension TEXT NOT NULL,
    scope_hash TEXT NOT NULL,
    bucket_start TIMESTAMPTZ NOT NULL,
    used BIGINT NOT NULL DEFAULT 0 CHECK (used >= 0),
    regular_used BIGINT NOT NULL DEFAULT 0 CHECK (regular_used >= 0),
    PRIMARY KEY (dimension, scope_hash, bucket_start),
    CHECK (dimension IN ('global_minute', 'global_day', 'site_minute', 'key_minute')),
    CHECK (regular_used <= used)
);
CREATE INDEX IF NOT EXISTS idx_monitoring_probe_budget_counter_expiry
    ON monitoring_probe_budget_counters (bucket_start);

CREATE TABLE IF NOT EXISTS monitoring_probe_budget_leases (
    id UUID PRIMARY KEY,
    site_hash TEXT NOT NULL,
    key_hashes TEXT[] NOT NULL,
    recovery BOOLEAN NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_monitoring_probe_budget_lease_expiry
    ON monitoring_probe_budget_leases (expires_at);
CREATE INDEX IF NOT EXISTS idx_monitoring_probe_budget_lease_site
    ON monitoring_probe_budget_leases (site_hash, expires_at);
CREATE INDEX IF NOT EXISTS idx_monitoring_probe_budget_lease_keys
    ON monitoring_probe_budget_leases USING GIN (key_hashes);
