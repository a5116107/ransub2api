-- Configuration-only revisions for the existing V1 runner. Observation writes
-- never advance the clock. Drain workers before rollback; the extra state may
-- be retained when reverting the application without affecting business data.
CREATE TABLE IF NOT EXISTS channel_monitor_schedule_clock (
    id SMALLINT PRIMARY KEY CHECK (id = 1),
    revision BIGINT NOT NULL DEFAULT 0 CHECK (revision >= 0)
);
INSERT INTO channel_monitor_schedule_clock (id) VALUES (1) ON CONFLICT DO NOTHING;

ALTER TABLE channel_monitor_probe_claims
    ADD COLUMN IF NOT EXISTS schedule_revision BIGINT NOT NULL DEFAULT 0;

-- Acquire the clock before configuration row locks, including multi-row
-- template updates. MarkChecked does not target these columns or take it.
CREATE OR REPLACE FUNCTION lock_channel_monitor_schedule_clock()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM revision FROM channel_monitor_schedule_clock WHERE id = 1 FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'channel monitor schedule clock missing';
    END IF;
    RETURN NULL;
END;
$$;

CREATE OR REPLACE FUNCTION advance_channel_monitor_schedule_revision()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    next_revision BIGINT;
    configuration_key TEXT;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        -- The two internal header records are observations. An ordinary
        -- configuration save, including identical values, is a new generation.
        IF NEW.extra_headers IS DISTINCT FROM OLD.extra_headers
           AND NEW.updated_at IS NOT DISTINCT FROM OLD.updated_at
           AND (to_jsonb(NEW) - 'extra_headers' - 'last_checked_at' - 'updated_at')
               = (to_jsonb(OLD) - 'extra_headers' - 'last_checked_at' - 'updated_at')
           AND (COALESCE(NEW.extra_headers, '{}'::jsonb)
                - 'sub2api:budget_credential_scope' - 'sub2api:duplicate_operation_id')
               = (COALESCE(OLD.extra_headers, '{}'::jsonb)
                - 'sub2api:budget_credential_scope' - 'sub2api:duplicate_operation_id') THEN
            RETURN NEW;
        END IF;
    END IF;

    UPDATE channel_monitor_schedule_clock SET revision = revision + 1 WHERE id = 1
        RETURNING revision INTO STRICT next_revision;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;

    configuration_key := md5(((to_jsonb(NEW) - 'last_checked_at' - 'updated_at') ||
        jsonb_build_object('extra_headers', COALESCE(NEW.extra_headers, '{}'::jsonb)
        - 'sub2api:budget_credential_scope' - 'sub2api:duplicate_operation_id'))::text);
    INSERT INTO channel_monitor_probe_claims AS c
        (monitor_id, configuration_key, next_due_at, schedule_revision)
        VALUES (NEW.id, configuration_key, clock_timestamp(), next_revision)
        ON CONFLICT (monitor_id) DO UPDATE SET
            configuration_key = EXCLUDED.configuration_key,
            lease_id = NULL, lease_expires_at = NULL,
            next_due_at = EXCLUDED.next_due_at,
            schedule_revision = EXCLUDED.schedule_revision;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS channel_monitor_schedule_lock ON channel_monitors;
CREATE TRIGGER channel_monitor_schedule_lock
    BEFORE INSERT OR DELETE OR UPDATE OF
        id, name, provider, api_mode, endpoint, api_key_encrypted, primary_model,
        extra_models, group_name, enabled, interval_seconds, jitter_seconds,
        created_by, created_at, check_mode, account_id, template_id,
        extra_headers, body_override_mode, body_override
    ON channel_monitors FOR EACH STATEMENT
    EXECUTE FUNCTION lock_channel_monitor_schedule_clock();

DROP TRIGGER IF EXISTS channel_monitor_schedule_revision ON channel_monitors;
CREATE TRIGGER channel_monitor_schedule_revision
    AFTER INSERT OR DELETE OR UPDATE OF
        id, name, provider, api_mode, endpoint, api_key_encrypted, primary_model,
        extra_models, group_name, enabled, interval_seconds, jitter_seconds,
        created_by, created_at, check_mode, account_id, template_id,
        extra_headers, body_override_mode, body_override
    ON channel_monitors FOR EACH ROW
    EXECUTE FUNCTION advance_channel_monitor_schedule_revision();

-- Seed pre-existing monitors without changing a live lease or its due time.
INSERT INTO channel_monitor_probe_claims
    (monitor_id, configuration_key, next_due_at, schedule_revision)
    SELECT m.id, md5(((to_jsonb(m) - 'last_checked_at' - 'updated_at') ||
        jsonb_build_object('extra_headers', COALESCE(m.extra_headers, '{}'::jsonb)
        - 'sub2api:budget_credential_scope' - 'sub2api:duplicate_operation_id'))::text),
        clock_timestamp(), c.revision
    FROM channel_monitors m CROSS JOIN channel_monitor_schedule_clock c
    WHERE c.id = 1 ON CONFLICT (monitor_id) DO NOTHING;
