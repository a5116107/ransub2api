package repository

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

// The final physical attempt must prove the billed account's current route.
// A missing/malformed/foreign route is absent evidence, including in legacy
// latency mode. Nil is reserved for explicit unscoped repository callers.
func priorityRouteFilterSQL(parameter string) string {
	return strings.ReplaceAll(`($ROUTES::jsonb IS NULL OR (
    detail->>'version'='1' AND attempt->>'account_id'=account_id::text
    AND attempt->>'route_fingerprint' ~ '^[0-9a-f]{64}$'
    AND attempt->>'route_fingerprint'<>repeat('0',64)
    AND attempt->>'route_fingerprint'=($ROUTES::jsonb->>account_id::text)
   ))`, "$ROUTES", parameter)
}

func priorityRouteQueryValue(routes map[int64]string) (any, error) {
	if routes == nil {
		return nil, nil
	}
	data, err := json.Marshal(routes)
	if err != nil {
		return nil, err
	}
	return string(data), nil
}

// Performance reuses the shared metrics and formula. Request-scoped reads also
// require immutable route/protocol evidence; unscoped account monitoring keeps
// the upstream account-wide view. Financial history retains model/group scope.
func (r *usageLogRepository) ReadPrioritySchedulingSignals(ctx context.Context, query service.PrioritySchedulingQuery) (map[int64]service.PrioritySchedulingSignal, error) {
	out := make(map[int64]service.PrioritySchedulingSignal, len(query.AccountIDs))
	if len(query.AccountIDs) == 0 {
		return out, nil
	}
	if query.UsageUntil.IsZero() {
		query.UsageUntil = time.Now()
	}
	var scope *service.PrioritySchedulingQuery
	if query.AccountRouteFingerprints != nil || query.InboundEndpoint != "" || query.ObservationEffort != nil {
		scope = &query
	}
	performance, err := readAccountPerformance(ctx, r.sql, query.AccountIDs, query.UsageUntil, scope)
	if err != nil {
		return nil, err
	}
	for _, id := range query.AccountIDs {
		metrics, ok := performance[id]
		if !ok {
			continue
		}
		out[id] = service.PrioritySchedulingSignal{Performance: &metrics}
	}
	rows, err := r.sql.QueryContext(ctx, `
SELECT account_id, COUNT(*), COALESCE(SUM(actual_cost),0),
 COALESCE(SUM(COALESCE(account_stats_cost,total_cost)),0)
FROM usage_logs
WHERE account_id=ANY($1) AND created_at >= $2 AND created_at <= $3
 AND COALESCE(NULLIF(requested_model,''),model)=$4
 AND group_id IS NOT DISTINCT FROM $5::bigint
 AND ($6::text IS NULL OR COALESCE(reasoning_effort,'')=$6)
 AND (actual_cost>0 OR total_cost>0 OR account_stats_cost>0
  OR input_tokens+output_tokens+cache_read_tokens+cache_creation_tokens>0)
GROUP BY account_id`, pq.Array(query.AccountIDs), query.UsageSince, query.UsageUntil, query.Model, query.GroupID, query.ObservationEffort)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var financial service.PrioritySchedulingSignal
		if err := rows.Scan(&id, &financial.ProfitSamples, &financial.Revenue, &financial.BaseCost); err != nil {
			return nil, err
		}
		if signal, ok := out[id]; ok {
			signal.ProfitSamples, signal.Revenue, signal.BaseCost = financial.ProfitSamples, financial.Revenue, financial.BaseCost
			out[id] = signal
		}
	}
	return out, rows.Err()
}
