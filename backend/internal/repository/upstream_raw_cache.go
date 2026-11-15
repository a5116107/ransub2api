package repository

// The current account cohort only selects the operator's requested inventory.
// Every cache row uses the immutable final transport projection, never today's
// account credential/group. Two traces for one billed request remain unknown.
const upstreamRawCacheAggregate = `count(*) AS requests,
 count(*) FILTER (WHERE observed) AS observed_requests,
 count(*) FILTER (WHERE NOT observed) AS unknown_requests,
 count(*) FILTER (WHERE NOT identity_known) AS unattributed_requests,
 count(*) FILTER (WHERE observed AND raw_read>0) AS hit_requests,
 COALESCE(sum(raw_input) FILTER (WHERE observed),0) AS input_tokens,
 COALESCE(sum(raw_read) FILTER (WHERE observed),0) AS read_tokens,
 count(*) FILTER (WHERE observed)::numeric/NULLIF(count(*),0) AS coverage,
 count(*) FILTER (WHERE observed AND raw_read>0)::numeric/NULLIF(count(*) FILTER (WHERE observed),0) AS request_hit_rate,
 sum(raw_read) FILTER (WHERE observed)/NULLIF(sum(raw_input) FILTER (WHERE observed),0)::numeric AS token_read_rate,
 max(created_at) FILTER (WHERE observed) AS latest_sample_at`

const upstreamOperationsRawCache = upstreamOperationsBase + `, cache_traces AS (
 SELECT scoped.*,trace.detail->'raw_cache' cache,trace.detail->'raw_upstream' identity,
 trace.detail->>'version' trace_version
 FROM scoped LEFT JOIN LATERAL (
  SELECT (array_agg(detail))[1] detail FROM (
   SELECT detail,created_at FROM request_timing_details
   WHERE usage_log_id=scoped.id AND scoped.created_at>=statement_timestamp()-interval '30 days' LIMIT 2
  ) retained HAVING count(*)=1 AND min(created_at)>=statement_timestamp()-interval '30 days'
 ) trace ON true
), raw_samples AS (
 SELECT *,COALESCE(trace_version='1' AND identity->>'version'='1' AND identity->>'status'='observed'
  AND identity->>'account_id'=account_id::text AND identity->>'transport' IN ('http','ws_turn')
  AND identity->>'credential_source'=CASE identity->>'transport' WHEN 'http' THEN 'http_transport_handoff' ELSE 'websocket_handshake_credentials' END
  AND identity->>'credential_fingerprint' ~ '^[0-9a-f]{64}$' AND identity->>'credential_fingerprint'<>repeat('0',64)
  AND identity->>'route_fingerprint' ~ '^[0-9a-f]{64}$' AND identity->>'route_fingerprint'<>repeat('0',64),false) identity_known
 FROM cache_traces ` + rawCacheObservationJoins + `
), attributed AS (
 SELECT *,raw_valid AND identity_known AS observed,
 CASE WHEN identity_known THEN identity->>'credential_fingerprint' END credential_fingerprint,
 CASE WHEN identity_known THEN identity->>'route_fingerprint' END route_fingerprint,
 CASE WHEN identity_known THEN identity->>'transport' END transport,
 CASE cache->>'source' WHEN 'canonical_responses_terminal_usage' THEN 'responses'
 WHEN 'canonical_messages_cumulative_usage' THEN 'messages' WHEN 'canonical_gemini_usage_metadata' THEN 'gemini'
 WHEN 'canonical_chat_completions_usage' THEN 'chat_completions' END protocol
 FROM raw_samples
), buckets AS (
 SELECT account_id,credential_fingerprint,route_fingerprint,transport,protocol,
 public_model,upstream_model,service_tier,reasoning_effort,stream,
 'not_observed'::text AS upstream_group_status,` + upstreamRawCacheAggregate + `
 FROM attributed GROUP BY account_id,credential_fingerprint,route_fingerprint,transport,protocol,
 public_model,upstream_model,service_tier,reasoning_effort,stream
), summary AS (SELECT ` + upstreamRawCacheAggregate + ` FROM attributed)
 SELECT jsonb_build_object('source','canonical_final_transport_cache_reports',
 'scope','recorded_usage_unique_retained_trace_final_credentials; upstream_group_not_observed',
 'retention_days',30,'summary',(SELECT to_jsonb(s) FROM summary s),'total',(SELECT count(*) FROM buckets),
 'rows',COALESCE((SELECT jsonb_agg(to_jsonb(b)) FROM (
  SELECT * FROM buckets ORDER BY requests DESC,account_id,credential_fingerprint NULLS LAST,
  route_fingerprint NULLS LAST,transport NULLS LAST,protocol NULLS LAST,public_model,upstream_model NULLS LAST,
  service_tier NULLS LAST,reasoning_effort NULLS LAST,stream LIMIT $9 OFFSET $10
 )b),'[]'::jsonb))`
