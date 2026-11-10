-- Permanent submission/task identities for the existing financial ledgers.
-- Local execution and primary reconciliation retain separate copies; primary
-- requests and all reconciled regions compete in the same primary namespace.
-- No TTL or FK cascade: completed/canceled requests must never allow reuse.
-- Roll back only after draining video holds and acknowledging their outboxes;
-- retain this table across application rollback, including its unique indexes.
CREATE TABLE regional_video_financial_bindings (
    ledger TEXT NOT NULL CHECK (ledger IN ('primary','local')),
    request_id VARCHAR(63) NOT NULL,
    lease_id VARCHAR(63) NOT NULL,
    region_id VARCHAR(63) NOT NULL,
    member TEXT NOT NULL CHECK (member ~ '^video:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    origin JSONB NOT NULL CHECK (jsonb_typeof(origin) = 'object'),
    task_key TEXT,
    task_id TEXT,
    PRIMARY KEY (ledger,request_id),
    UNIQUE (ledger,member),
    UNIQUE (ledger,task_key),
    CHECK ((lease_id = '' AND region_id = '') OR (lease_id <> '' AND region_id <> '')),
    CHECK (ledger <> 'local' OR lease_id <> ''),
    CHECK ((task_key IS NULL AND task_id IS NULL) OR
        (task_key IS NOT NULL AND task_key ~ '^concurrency:upstream_key:async_video:task:v2:[0-9a-f]{64}$'
        AND task_id IS NOT NULL AND length(task_id) BETWEEN 1 AND 256))
);

CREATE FUNCTION reject_regional_video_binding_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'regional video financial identities are permanent';
    END IF;
    IF NEW.ledger IS DISTINCT FROM OLD.ledger OR NEW.request_id IS DISTINCT FROM OLD.request_id
        OR NEW.lease_id IS DISTINCT FROM OLD.lease_id OR NEW.region_id IS DISTINCT FROM OLD.region_id
        OR NEW.member IS DISTINCT FROM OLD.member OR NEW.origin IS DISTINCT FROM OLD.origin
        OR (OLD.task_key IS NOT NULL AND (NEW.task_key IS DISTINCT FROM OLD.task_key OR NEW.task_id IS DISTINCT FROM OLD.task_id)) THEN
        RAISE EXCEPTION 'regional video financial identities are immutable';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER regional_video_financial_bindings_immutable
    BEFORE UPDATE OR DELETE ON regional_video_financial_bindings
    FOR EACH ROW EXECUTE FUNCTION reject_regional_video_binding_mutation();
