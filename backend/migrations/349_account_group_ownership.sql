-- Legacy/manual memberships remain SQL NULL. Do not claim existing bindings or
-- rewrite their priorities, model restrictions, creation times or group policy.
-- Rollback retains the column and its data; drain managed products before using
-- a binary that does not understand ownership. This migration enables no owner.
ALTER TABLE account_groups ADD COLUMN IF NOT EXISTS ownership JSONB;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'account_groups'::regclass
          AND conname = 'account_groups_ownership_contract'
    ) THEN
        ALTER TABLE account_groups ADD CONSTRAINT account_groups_ownership_contract
        CHECK (ownership IS NULL OR ((
            jsonb_typeof(ownership) = 'object'
            AND ownership - ARRAY['version', 'kind', 'revision']::text[] = '{}'::jsonb
            AND jsonb_typeof(ownership->'version') = 'number'
            AND ownership->>'version' = '1'
            AND ownership->>'kind' = 'procurement'
            AND jsonb_typeof(ownership->'revision') = 'number'
            AND CASE WHEN ownership->>'revision' ~ '^[1-9][0-9]{0,15}$'
                THEN (ownership->>'revision')::numeric <= 9007199254740991
                ELSE false END
        ) IS TRUE));
    END IF;
END $$;
