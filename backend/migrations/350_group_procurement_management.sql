-- Review is opt-in. Legacy products remain NULL/manual and no memberships,
-- protections, schedules or prices are changed. Application rollback retains
-- this authorization and the original membership ownership metadata.
ALTER TABLE groups ADD COLUMN IF NOT EXISTS procurement_management JSONB;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
        WHERE conrelid = 'groups'::regclass AND conname = 'groups_procurement_management_contract') THEN
        ALTER TABLE groups ADD CONSTRAINT groups_procurement_management_contract
        CHECK (procurement_management IS NULL OR ((
            jsonb_typeof(procurement_management) = 'object'
            AND procurement_management - ARRAY['version','revision','mode']::text[] = '{}'::jsonb
            AND jsonb_typeof(procurement_management->'version') = 'number'
            AND procurement_management->>'version' = '1'
            AND procurement_management->>'mode' IN ('manual','review')
            AND jsonb_typeof(procurement_management->'revision') = 'number'
            AND CASE WHEN procurement_management->>'revision' ~ '^[1-9][0-9]{0,15}$'
                THEN (procurement_management->>'revision')::numeric <= 9007199254740991
                ELSE false END
        ) IS TRUE));
    END IF;
END $$;
