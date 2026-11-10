-- A verified zero-price contract still creates a durable request and allocation.
-- Grants themselves remain positive; unresolved zero requests still block drain.
ALTER TABLE regional_local_requests DROP CONSTRAINT IF EXISTS regional_local_requests_maximum_check;
ALTER TABLE regional_local_requests ADD CONSTRAINT regional_local_requests_maximum_check CHECK (maximum >= 0 AND maximum % 10 = 0);

ALTER TABLE regional_primary_requests DROP CONSTRAINT IF EXISTS regional_primary_requests_maximum_check;
ALTER TABLE regional_primary_requests ADD CONSTRAINT regional_primary_requests_maximum_check CHECK (maximum >= 0 AND maximum % 10 = 0);

ALTER TABLE regional_budget_allocations DROP CONSTRAINT IF EXISTS regional_budget_allocations_maximum_check;
ALTER TABLE regional_budget_allocations ADD CONSTRAINT regional_budget_allocations_maximum_check CHECK (maximum >= 0 AND maximum % 10 = 0);
