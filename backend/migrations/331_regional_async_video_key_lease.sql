-- Durable async-video ownership for the regional SQL authority.
-- Records are retained until a terminal task release is confirmed; unknown
-- submissions are never reclaimed by this table alone. Before rollback,
-- drain all video tasks and retain this table until their slots are released.
CREATE TABLE IF NOT EXISTS regional_async_video_key_leases (
    member TEXT PRIMARY KEY CHECK (member ~ '^video:[0-9a-f-]{36}$'),
    task_id TEXT,
    task_key TEXT UNIQUE,
    state TEXT NOT NULL CHECK (state IN ('provisional','unknown','bound')),
    owner JSONB NOT NULL CHECK (jsonb_typeof(owner) = 'object'),
    lease JSONB NOT NULL CHECK (jsonb_typeof(lease) = 'object'),
    fenced BOOLEAN NOT NULL DEFAULT false,
    release_due_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CHECK ((state = 'bound' AND task_id IS NOT NULL AND task_key IS NOT NULL AND length(task_id) BETWEEN 1 AND 256)
        OR (state <> 'bound' AND task_id IS NULL AND task_key IS NULL)),
    CHECK ((fenced AND release_due_at IS NOT NULL) OR (NOT fenced AND release_due_at IS NULL))
);

CREATE INDEX IF NOT EXISTS regional_async_video_key_release_idx
    ON regional_async_video_key_leases (release_due_at, member)
    WHERE fenced AND release_due_at IS NOT NULL;
