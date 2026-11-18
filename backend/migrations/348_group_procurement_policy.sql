-- Administrator-owned multiplier range. Legacy groups remain unconfigured;
-- no sales price, account binding, budget counter or history is rewritten.
-- Rollback retains this column. Old binaries do not enforce this policy:
-- drain and pause affected products before running an older application.
ALTER TABLE groups ADD COLUMN IF NOT EXISTS procurement_policy JSONB;
