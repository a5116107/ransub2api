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

// Columns and keys below are fixed implementation identifiers, never query input.
func upstreamWaitAggregate(column string) string {
	return `jsonb_build_object('samples',count(` + column + `),'unknown_samples',count(*)-count(` + column + `),
 'mean_ms',avg(` + column + `),'coverage',count(` + column + `)::numeric/NULLIF(count(*),0),
 'latest_sample_at',max(created_at) FILTER (WHERE ` + column + ` IS NOT NULL))`
}

func upstreamWaitValue(key string) string {
	return `CASE WHEN latency.item->'` + key + `'->>'status'='measured'
 AND jsonb_typeof(latency.item->'` + key + `'->'ms')='number'
 AND jsonb_typeof(timing_detail->'total_ms')='number'
 THEN CASE WHEN (latency.item->'` + key + `'->>'ms')::numeric BETWEEN 0 AND (timing_detail->>'total_ms')::numeric
 THEN (latency.item->'` + key + `'->>'ms')::numeric END END`
}

var upstreamOperationsRawCache = upstreamOperationsBase + `, cache_traces AS (
 SELECT scoped.*,trace.detail->'raw_cache' cache,trace.detail->'raw_upstream' identity,
 trace.detail->>'version' trace_version,trace.detail timing_detail
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
), wait_samples AS (
 SELECT attributed.*,` + upstreamWaitValue("post_upload_first_byte_wait") + ` wait_first_byte,
 CASE WHEN NOT COALESCE(physical.item->'events' ? 'typed_content_truncated',false)
 THEN ` + upstreamWaitValue("post_upload_first_event_wait") + ` END wait_first_event,
 CASE WHEN NOT COALESCE(physical.item->'events' ? 'typed_content_truncated',false)
 THEN ` + upstreamWaitValue("post_upload_first_reasoning_wait") + ` END wait_first_reasoning,
 CASE WHEN NOT COALESCE(physical.item->'events' ? 'typed_content_truncated',false)
 THEN ` + upstreamWaitValue("post_upload_first_text_wait") + ` END wait_first_text
 FROM attributed
 LEFT JOIN LATERAL (
  SELECT (array_agg(expanded.item))[1] item FROM jsonb_array_elements(
   CASE jsonb_typeof(timing_detail->'attempts') WHEN 'array' THEN
    CASE WHEN jsonb_array_length(timing_detail->'attempts')<=32 THEN timing_detail->'attempts' ELSE '[]'::jsonb END
   ELSE '[]'::jsonb END) AS expanded(item)
  WHERE expanded.item->>'number'=identity->>'attempt_number' HAVING count(*)=1
 ) physical ON identity_known AND transport='http' AND identity->>'attempt_number' ~ '^([1-9]|[12][0-9]|3[0-2])$'
  AND jsonb_typeof(identity->'attempt_number')='number' AND COALESCE(timing_detail->>'truncated','false')='false'
 LEFT JOIN LATERAL (
  SELECT (array_agg(expanded.item))[1] item FROM jsonb_array_elements(
   CASE jsonb_typeof(timing_detail->'latency'->'attempts') WHEN 'array' THEN
    CASE WHEN jsonb_array_length(timing_detail->'latency'->'attempts')<=32 THEN timing_detail->'latency'->'attempts' ELSE '[]'::jsonb END
   ELSE '[]'::jsonb END) AS expanded(item)
  WHERE expanded.item->>'number'=identity->>'attempt_number' HAVING count(*)=1
 ) latency ON timing_detail->'latency'->>'version'='1' AND physical.item->>'kind'='http'
  AND physical.item->>'account_id'=identity->>'account_id'
  AND physical.item->>'route_fingerprint'=route_fingerprint
  AND physical.item->'upstream_credential'->>'status'='observed'
  AND physical.item->'upstream_credential'->>'source'='http_transport_handoff'
  AND physical.item->'upstream_credential'->>'fingerprint'=credential_fingerprint
), buckets AS (
 SELECT account_id,credential_fingerprint,route_fingerprint,transport,protocol,
 public_model,upstream_model,service_tier,reasoning_effort,stream,
 'not_observed'::text AS upstream_group_status,` + upstreamRawCacheAggregate + `,
 jsonb_build_object('version',1,'source','canonical_final_physical_http_attempt',
 'first_byte',` + upstreamWaitAggregate("wait_first_byte") + `,
 'first_event',` + upstreamWaitAggregate("wait_first_event") + `,
 'first_reasoning',` + upstreamWaitAggregate("wait_first_reasoning") + `,
 'first_text',` + upstreamWaitAggregate("wait_first_text") + `) waits
 FROM wait_samples GROUP BY account_id,credential_fingerprint,route_fingerprint,transport,protocol,
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
