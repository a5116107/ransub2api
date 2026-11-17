-- Keep local monitoring capacity separate from supplier health and quality.
-- Rollback retains these nullable columns and history. Old detection workers
-- must be drained; old readers do not implement the neutral deferral contract.
ALTER TABLE pelican_group_test_results
    ADD COLUMN IF NOT EXISTS budget_deferral JSONB;
ALTER TABLE channel_monitor_v2_candy_results
    ADD COLUMN IF NOT EXISTS budget_deferral JSONB;
ALTER TABLE channel_monitor_v3_probe_results
    ADD COLUMN IF NOT EXISTS budget_deferral JSONB;

ALTER TABLE channel_monitor_v2_candy_results
    DROP CONSTRAINT IF EXISTS channel_monitor_v2_candy_results_verdict_check;
ALTER TABLE channel_monitor_v2_candy_results
    ADD CONSTRAINT channel_monitor_v2_candy_results_verdict_check
    CHECK (verdict IN ('running','correct','incorrect','error','deferred'));
ALTER TABLE channel_monitor_v3_probe_results
    DROP CONSTRAINT IF EXISTS channel_monitor_v3_probe_results_outcome_check;
ALTER TABLE channel_monitor_v3_probe_results
    ADD CONSTRAINT channel_monitor_v3_probe_results_outcome_check
    CHECK (outcome IN ('running','success','failure','deferred'));
