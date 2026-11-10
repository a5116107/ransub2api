-- Separate downstream sale funds from the account's base-cost quota. Existing
-- deployments must drain requests/grants/outbox before upgrade or rollback;
-- the old application does not maintain the new counters. Keep these columns
-- on application rollback and retain all permanent request/financial evidence.
LOCK TABLE regional_quota_leases, regional_local_leases, regional_local_requests,
 regional_primary_requests, regional_budget_allocations, regional_usage_outbox
 IN ACCESS EXCLUSIVE MODE;
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM regional_quota_leases WHERE state <> 'closed')
 OR EXISTS(SELECT 1 FROM regional_local_leases WHERE state <> 'closed')
 OR EXISTS(SELECT 1 FROM regional_primary_requests WHERE state IN ('reserved','started'))
 OR EXISTS(SELECT 1 FROM regional_local_requests WHERE state IN ('reserved','started'))
 OR EXISTS(SELECT 1 FROM regional_budget_allocations WHERE state = 'held')
 OR EXISTS(SELECT 1 FROM regional_usage_outbox WHERE NOT acknowledged) THEN
  RAISE EXCEPTION 'drain regional funds, requests and outbox before account-cost upgrade';
 END IF;
END $$;

ALTER TABLE regional_quota_leases
 ADD COLUMN IF NOT EXISTS account_granted BIGINT NOT NULL DEFAULT 0
 CHECK (account_granted >= 0 AND account_granted % 10 = 0);

ALTER TABLE regional_local_leases
 ADD COLUMN IF NOT EXISTS account_granted BIGINT,
 ADD COLUMN IF NOT EXISTS account_reserved BIGINT,
 ADD COLUMN IF NOT EXISTS account_spent BIGINT;
UPDATE regional_local_leases SET account_granted=granted,account_reserved=reserved,account_spent=spent
 WHERE account_granted IS NULL;
ALTER TABLE regional_local_leases
 ALTER COLUMN account_granted SET NOT NULL,
 ALTER COLUMN account_reserved SET NOT NULL,
 ALTER COLUMN account_spent SET NOT NULL,
 DROP CONSTRAINT IF EXISTS regional_local_account_budget;
ALTER TABLE regional_local_leases
 ADD CONSTRAINT regional_local_account_budget CHECK (
  account_granted > 0 AND account_granted % 10 = 0 AND
  account_reserved >= 0 AND account_reserved % 10 = 0 AND
  account_spent >= 0 AND account_spent % 10 = 0 AND
  account_reserved <= account_granted-account_spent);

ALTER TABLE regional_local_requests ADD COLUMN IF NOT EXISTS account_maximum BIGINT;
UPDATE regional_local_requests SET account_maximum=maximum WHERE account_maximum IS NULL;
ALTER TABLE regional_local_requests
 ALTER COLUMN account_maximum SET NOT NULL,
 DROP CONSTRAINT IF EXISTS regional_local_request_account_maximum;
ALTER TABLE regional_local_requests ADD CONSTRAINT regional_local_request_account_maximum
 CHECK (account_maximum >= 0 AND account_maximum % 10 = 0);

ALTER TABLE regional_budget_allocations
 ADD COLUMN IF NOT EXISTS account_maximum BIGINT,
 ADD COLUMN IF NOT EXISTS account_spent BIGINT;
UPDATE regional_budget_allocations SET account_maximum=maximum,account_spent=spent
 WHERE account_maximum IS NULL;
ALTER TABLE regional_budget_allocations
 ALTER COLUMN account_maximum SET NOT NULL,
 ALTER COLUMN account_spent SET NOT NULL,
 DROP CONSTRAINT IF EXISTS regional_allocation_account_budget;
ALTER TABLE regional_budget_allocations ADD CONSTRAINT regional_allocation_account_budget
 CHECK (account_maximum >= 0 AND account_maximum % 10 = 0 AND
        account_spent >= 0 AND account_spent % 10 = 0 AND account_spent <= account_maximum);
