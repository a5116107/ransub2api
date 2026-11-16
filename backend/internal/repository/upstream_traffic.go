package repository

// The argument is a fixed SQL expression, never request input. Historical
// records stay unknown: routes, user agents, sessions and current config are
// deliberately not used to reconstruct their purpose.
func upstreamTrafficSource(detail string) string {
	return `CASE WHEN ` + detail + `->'version'='1'::jsonb AND ` + detail + `->'traffic_source'->'version'='1'::jsonb THEN
 CASE WHEN ` + detail + `->'traffic_source'->>'source'='gateway_ingress'
  AND ` + detail + `->'traffic_source'->>'kind'='gateway' THEN 'gateway_ingress'
 WHEN ` + detail + `->'traffic_source'->>'source'='account_connection_test'
  AND ` + detail + `->'traffic_source'->>'kind'='synthetic' THEN 'account_connection_test'
 ELSE 'unknown' END ELSE 'unknown' END`
}

// Both reports expose the composition of their own, different denominators.
const upstreamTrafficSummary = `(SELECT jsonb_build_object('version',1,'source','server_recorded_entrypoints',
 'samples',count(*),'known_samples',count(*) FILTER (WHERE traffic_source<>'unknown'),
 'unknown_samples',count(*) FILTER (WHERE traffic_source='unknown'),
 'coverage',count(*) FILTER (WHERE traffic_source<>'unknown')::numeric/NULLIF(count(*),0),
 'rows',COALESCE((SELECT jsonb_agg(to_jsonb(t) ORDER BY traffic_source) FROM (
  SELECT traffic_source,count(*) AS samples FROM attributed GROUP BY traffic_source
 )t),'[]'::jsonb)) FROM attributed)`
