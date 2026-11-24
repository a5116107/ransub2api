-- Durable ordering is separate from retained results and physical accounting.
-- Rollback may retain this private state; old workers must be drained first.
CREATE SEQUENCE IF NOT EXISTS monitoring_group_probe_turn_sequence;
CREATE TABLE IF NOT EXISTS monitoring_group_probe_turns (
    lane TEXT NOT NULL CHECK (lane IN ('v2', 'v3')),
    group_id BIGINT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    scope_hash TEXT NOT NULL CHECK (scope_hash ~ '^[0-9a-f]{64}$'),
    identity_hash TEXT NOT NULL CHECK (identity_hash ~ '^[0-9a-f]{64}$'),
    served BIGINT NOT NULL CHECK (served > 0),
    tried BIGINT NOT NULL DEFAULT 0 CHECK (tried >= 0),
    waiting_since TIMESTAMPTZ,
    claim_until TIMESTAMPTZ,
    PRIMARY KEY(lane, group_id, scope_hash)
);
