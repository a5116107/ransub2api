-- Optional local scheduling metadata; old applications may ignore this column.
-- Rollback keeps the column and recorded history instead of deleting evidence.
ALTER TABLE codex_harvest_flow_events
    ADD COLUMN IF NOT EXISTS budget_deferral JSONB;
