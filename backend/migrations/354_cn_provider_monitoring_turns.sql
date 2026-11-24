-- CN automatic probe age survives process replacement. This is private
-- scheduling metadata, not account health, usage or a credential registry.
CREATE SEQUENCE IF NOT EXISTS cn_provider_monitoring_turn_sequence;

CREATE TABLE IF NOT EXISTS cn_provider_monitoring_turns (
    account_id BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    identity_hash TEXT NOT NULL CHECK (identity_hash ~ '^[0-9a-f]{64}$'),
    served BIGINT NOT NULL CHECK (served > 0),
    tried BIGINT NOT NULL DEFAULT 0 CHECK (tried >= 0)
);
