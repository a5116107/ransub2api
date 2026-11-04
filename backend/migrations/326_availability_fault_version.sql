-- Availability must not reuse a quality ownership counter that can be cleared.
ALTER TABLE scheduled_test_plans
    ADD COLUMN IF NOT EXISTS availability_fault_version BIGINT NOT NULL DEFAULT 0
    CHECK (availability_fault_version >= 0);
