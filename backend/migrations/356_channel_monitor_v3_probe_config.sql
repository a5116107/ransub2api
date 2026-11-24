-- Keep historical unknown scopes unknown. New claims record the model,
-- reasoning effort and interval digest in the original result row.
ALTER TABLE channel_monitor_v3_probe_results
    ADD COLUMN IF NOT EXISTS config_key VARCHAR(64)
        CHECK (config_key IS NULL OR config_key ~ '^[0-9a-f]{64}$');

CREATE INDEX IF NOT EXISTS idx_channel_monitor_v3_probe_config
    ON channel_monitor_v3_probe_results (group_id, config_key, slot_start DESC, id DESC)
    WHERE config_key IS NOT NULL;
