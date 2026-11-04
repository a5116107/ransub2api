-- Latest availability is independent of quality ownership and history retention.
CREATE TABLE IF NOT EXISTS account_availability_states (
    plan_id BIGINT NOT NULL REFERENCES scheduled_test_plans(id) ON DELETE CASCADE,
    model_id TEXT NOT NULL,
    plan_version TIMESTAMPTZ NOT NULL,
    route_fingerprint BYTEA NOT NULL CHECK (octet_length(route_fingerprint) = 32),
    round_lease TIMESTAMPTZ NOT NULL,
    state JSONB NOT NULL,
    PRIMARY KEY (plan_id, model_id)
);
