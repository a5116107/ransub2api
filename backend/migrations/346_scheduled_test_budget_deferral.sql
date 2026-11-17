-- Local scheduling metadata is distinct from upstream failures and quality.
-- Rollback keeps this nullable column and its history; old readers ignore it.
ALTER TABLE scheduled_test_results
    ADD COLUMN IF NOT EXISTS budget_deferral JSONB;
