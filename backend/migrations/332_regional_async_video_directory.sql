-- Metadata only: no credentials, quota grants or transferable lease. Provider
-- task IDs in terminal receipts stay local; they never enter wire snapshots.
-- Each receiver keeps its own revision floor, even when databases are shared.
-- Application rollback may retain these tables; never delete the actual task
-- or slot records to roll back a directory. Drain tasks before mixed versions.
CREATE TABLE IF NOT EXISTS regional_async_video_directory_revisions (
    authority TEXT PRIMARY KEY,
    revision BIGINT NOT NULL CHECK (revision > 0)
);

CREATE TABLE IF NOT EXISTS regional_async_video_directory_snapshots (
    consumer TEXT NOT NULL,
    authority TEXT NOT NULL,
    revision BIGINT NOT NULL CHECK (revision > 0),
    payload JSONB NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    PRIMARY KEY (consumer, authority),
    CHECK (consumer <> authority)
);

-- Local terminal receipts retain read ownership after generation slots and
-- active task records are released. They never carry a reusable slot lease.
CREATE TABLE IF NOT EXISTS regional_async_video_completed_tasks (
    authority TEXT NOT NULL,
    task_key TEXT NOT NULL,
    member TEXT NOT NULL,
    task_id TEXT NOT NULL,
    owner JSONB NOT NULL CHECK (jsonb_typeof(owner) = 'object'),
    expires_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (authority, task_key)
);
CREATE INDEX IF NOT EXISTS regional_async_video_completed_expiry_idx
    ON regional_async_video_completed_tasks (authority, expires_at);
