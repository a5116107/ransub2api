-- Private V1 automatic scheduling state. No API credentials or probe history.
-- Rollback: drain all new workers first; retaining this table is compatible
-- with the old app. Dropping only this table preserves monitors and history.
CREATE TABLE IF NOT EXISTS channel_monitor_probe_claims (
    monitor_id BIGINT PRIMARY KEY REFERENCES channel_monitors(id) ON DELETE CASCADE,
    configuration_key TEXT NOT NULL,
    lease_id UUID,
    lease_expires_at TIMESTAMPTZ,
    next_due_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT channel_monitor_probe_lease_pair CHECK (
        (lease_id IS NULL) = (lease_expires_at IS NULL)
    )
);
