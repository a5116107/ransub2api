-- Dynamic video sale contracts must be authorized by the same durable grant
-- that funds them. Stop all funds writers and drain before upgrade/rollback.
-- Keep this column, immutable prices and financial evidence on app rollback.
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
  RAISE EXCEPTION 'drain regional funds, requests and outbox before video-price upgrade';
 END IF;
END $$;

ALTER TABLE regional_quota_leases
 ADD COLUMN IF NOT EXISTS video_price_versions JSONB,
 ADD COLUMN IF NOT EXISTS group_id BIGINT;
UPDATE regional_quota_leases SET video_price_versions='[]'::jsonb WHERE video_price_versions IS NULL;
UPDATE regional_quota_leases SET group_id=0 WHERE group_id IS NULL;
ALTER TABLE regional_quota_leases
 ALTER COLUMN video_price_versions SET NOT NULL,
 ALTER COLUMN group_id SET NOT NULL,
 DROP CONSTRAINT IF EXISTS regional_quota_video_prices;
ALTER TABLE regional_quota_leases ADD CONSTRAINT regional_quota_video_prices
 CHECK (group_id>=0 AND jsonb_typeof(video_price_versions)='array' AND jsonb_array_length(video_price_versions)<=128
 AND (jsonb_array_length(video_price_versions)=0 OR group_id>0));
-- No default: old authority writers must fail instead of dropping authorizations.
