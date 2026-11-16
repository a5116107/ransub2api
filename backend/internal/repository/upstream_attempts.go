package repository

// Connection traces include unbilled failures. Usage details are a legacy
// fallback only: a trace is never counted twice or multiplied by usage joins.
// Public model/group are ingress labels, not historical physical upstream scope.
const upstreamAttemptsAggregate = `count(*) AS attempts,
 count(DISTINCT trace_id) AS logical_requests,
 count(*) FILTER (WHERE outcome='succeeded') AS succeeded,
 count(*) FILTER (WHERE outcome='failed') AS failed,
 count(*) FILTER (WHERE outcome='interrupted') AS interrupted,
 count(*) FILTER (WHERE outcome='unknown') AS unknown,
 count(*) FILTER (WHERE NOT identity_known) AS unattributed_attempts,
 count(*) FILTER (WHERE outcome IN ('succeeded','failed'))::numeric/NULLIF(count(*),0) AS outcome_coverage,
 count(*) FILTER (WHERE outcome='succeeded')::numeric/NULLIF(count(*) FILTER (WHERE outcome IN ('succeeded','failed')),0) AS confirmed_success_rate,
 count(*)::numeric/NULLIF(count(DISTINCT trace_id),0) AS attempts_per_request,
 max(started_at) AS latest_sample_at`

const upstreamAttemptCacheAggregate = `jsonb_build_object('version',1,'source','canonical_physical_attempt_cache_reports',
 'attempts',count(*),'observed_attempts',count(*) FILTER (WHERE cache_observed),
 'unknown_attempts',count(*) FILTER (WHERE NOT cache_observed),
 'hit_attempts',count(*) FILTER (WHERE cache_observed AND raw_read>0),
 'input_tokens',COALESCE(sum(raw_input) FILTER (WHERE cache_observed),0),
 'read_tokens',COALESCE(sum(raw_read) FILTER (WHERE cache_observed),0),
 'coverage',count(*) FILTER (WHERE cache_observed)::numeric/NULLIF(count(*),0),
 'attempt_hit_rate',count(*) FILTER (WHERE cache_observed AND raw_read>0)::numeric/NULLIF(count(*) FILTER (WHERE cache_observed),0),
 'token_read_rate',sum(raw_read) FILTER (WHERE cache_observed)/NULLIF(sum(raw_input) FILTER (WHERE cache_observed),0)::numeric,
 'latest_sample_at',max(created_at) FILTER (WHERE cache_observed))`

// Numeric JSON is bounded before casts so malformed retained diagnostics stay
// unknown instead of breaking the entire administrator query.
func upstreamOutputNumber(path string) string {
	return `CASE WHEN jsonb_typeof(` + path + `)='number' AND length((` + path + `)::text)<=24 THEN (` + path + `)::text::numeric END`
}

func upstreamOutputDistribution(value, partial string) string {
	return `jsonb_build_object('samples',count(` + value + `),'unknown_samples',count(*)-count(` + value + `),
 'partial_samples',count(` + partial + `),'coverage',count(` + value + `)::numeric/NULLIF(count(*),0),
 'mean_ms',avg(` + value + `),'p95_ms',percentile_cont(0.95) WITHIN GROUP (ORDER BY ` + value + `),
 'max_ms',max(` + value + `),'partial_max_ms',max(` + partial + `),
 'latest_sample_at',max(created_at) FILTER (WHERE ` + value + ` IS NOT NULL),
 'latest_partial_at',max(created_at) FILTER (WHERE ` + partial + ` IS NOT NULL))`
}

var upstreamAttemptOutputAggregate = `jsonb_build_object('version',1,'source','canonical_physical_attempt_output',
 'attempts',count(*),'max_gap',` + upstreamOutputDistribution("output_gap", "partial_gap") + `,
 'tail_wait',` + upstreamOutputDistribution("output_tail", "partial_tail") + `,
 'rate',jsonb_build_object('samples',count(output_tokens),'unknown_samples',count(*)-count(output_tokens),
 'coverage',count(output_tokens)::numeric/NULLIF(count(*),0),
 'tokens_per_second',sum(output_tokens)*1000/NULLIF(sum(output_window),0),
 'non_reasoning_tokens',sum(output_tokens),'window_ms',sum(output_window),
 'latest_sample_at',max(created_at) FILTER (WHERE output_tokens IS NOT NULL)))`

var upstreamAttemptOutputCTEs = `, output_fields AS (
 SELECT c.*,item->'output'->'text_stream' AS output_stream,item->'output'->'rate' AS output_rate,
 raw_attempt->'text_stream' AS raw_stream,raw_attempt->'output_usage' AS raw_output,
 ` + upstreamOutputNumber("detail->'total_ms'") + ` total_ms,
 ` + upstreamOutputNumber("raw_attempt->'start_ms'") + ` output_start,
 ` + upstreamOutputNumber("raw_attempt->'end_ms'") + ` output_end,
 ` + upstreamOutputNumber("raw_attempt->'text_stream'->'chunks'") + ` output_chunks,
 ` + upstreamOutputNumber("raw_attempt->'text_stream'->'first_ms'") + ` output_first,
 ` + upstreamOutputNumber("raw_attempt->'text_stream'->'last_ms'") + ` output_last,
 ` + upstreamOutputNumber("raw_attempt->'text_stream'->'max_gap_ms'") + ` raw_gap,
 ` + upstreamOutputNumber("raw_attempt->'text_stream'->'terminal_ms'") + ` output_terminal,
 ` + upstreamOutputNumber("item->'output'->'text_stream'->'span_ms'") + ` stream_span,
 ` + upstreamOutputNumber("item->'output'->'text_stream'->'tail_wait_ms'") + ` stream_tail,
 ` + upstreamOutputNumber("item->'output'->'rate'->'window_ms'") + ` rate_window,
 ` + upstreamOutputNumber("item->'output'->'rate'->'tokens_per_second'") + ` rate_value,
 ` + upstreamOutputNumber("raw_attempt->'output_usage'->'output_tokens'") + ` raw_output_tokens,
 ` + upstreamOutputNumber("raw_attempt->'output_usage'->'reasoning_tokens'") + ` raw_reasoning_tokens,
 next_send.boundary AS next_send_ms,next_send.valid AS next_send_valid
 FROM cache_samples c
 LEFT JOIN LATERAL (
  SELECT min(` + upstreamOutputNumber("n->'start_ms'") + `) boundary,
  bool_and(COALESCE(jsonb_typeof(n->'number')='number' AND n->>'number' ~ '^([1-9]|[12][0-9]|3[0-2])$'
   AND jsonb_typeof(n->'start_ms')='number' AND length((n->'start_ms')::text)<=24,false)) valid
  FROM jsonb_array_elements(CASE jsonb_typeof(detail->'attempts') WHEN 'array' THEN CASE WHEN jsonb_array_length(detail->'attempts')<=32 THEN detail->'attempts' ELSE '[]'::jsonb END ELSE '[]'::jsonb END) n
  WHERE n->>'kind'='http' AND (NOT COALESCE(jsonb_typeof(n->'number')='number'
   AND n->>'number' ~ '^([1-9]|[12][0-9]|3[0-2])$',false)
   OR ` + upstreamOutputNumber("n->'number'") + `>` + upstreamOutputNumber("item->'number'") + `)
 ) next_send ON item->>'transport'='http'
), output_validity AS (
 SELECT *,COALESCE(identity_known AND request_scope_known AND raw_attempt IS NOT NULL
 AND jsonb_typeof(item->'account_id')='number' AND item->'output'->'version'='1'::jsonb
 AND request_scope->'stream'='true'::jsonb
 AND item->>'protocol'=CASE request_scope->>'protocol' WHEN 'responses_compact' THEN 'responses' ELSE request_scope->>'protocol' END
 AND COALESCE(raw_stream->>'protocol','responses')=item->>'protocol'
 AND ((item->>'transport'='http' AND raw_attempt->'route'->>'endpoint'=request_scope->>'protocol'
   AND output_stream->>'scope'='physical_http_'||(item->>'protocol')||'_text_deltas_at_parser'
   AND output_stream->>'source'='typed_'||(item->>'protocol')||'_sse')
  OR (item->>'transport'='ws_turn' AND ws_wait IS NOT NULL
   AND output_stream->>'scope'='physical_websocket_turn_text_deltas_at_parser' AND output_stream->>'source'='typed_responses_ws'))
 AND output_stream->>'status' IN ('observed','partial')
 AND NOT COALESCE(raw_attempt->'events' ? 'typed_content_truncated',false)
 AND COALESCE(raw_stream->'invalid','false'::jsonb)='false'::jsonb
 AND total_ms>=0 AND output_start>=0 AND output_end BETWEEN output_start AND total_ms
 AND output_chunks BETWEEN 1 AND 1000000 AND trunc(output_chunks)=output_chunks
 AND output_first>=output_start AND output_last BETWEEN output_first AND total_ms
 AND (next_send_ms IS NULL OR output_last<=next_send_ms)
 AND COALESCE(next_send_valid,true)
 -- float64 differences are serialized independently of their operands.
 -- Match the parser's 1e-9 gap tolerance across decimal JSON arithmetic.
 AND raw_gap>=0 AND raw_gap<=output_last-output_first+0.000000001
 AND ((output_chunks=1 AND output_last=output_first AND raw_gap=0)
  OR (output_chunks>1 AND raw_gap+0.000000001>=(output_last-output_first)/NULLIF(output_chunks-1,0)))
 AND output_stream->'chunks'=raw_stream->'chunks'
 AND output_stream->'gap_samples'=to_jsonb(output_chunks-1)
 AND ((output_chunks=1 AND output_stream->'max_gap_ms'='null'::jsonb AND output_stream->'span_ms'='null'::jsonb)
  OR (output_chunks>1 AND output_stream->'max_gap_ms'=raw_stream->'max_gap_ms'
   AND abs(stream_span-(output_last-output_first))<=0.000001))
 AND ((COALESCE(raw_stream->>'terminal','')='' AND COALESCE(output_terminal,0)=0)
  OR (raw_stream->>'terminal' IN ('completed','failed','incomplete')
   AND output_terminal BETWEEN output_last AND total_ms AND (next_send_ms IS NULL OR output_terminal<=next_send_ms)))
 AND ((raw_stream->>'terminal' IN ('completed','failed','incomplete')
   AND COALESCE(raw_stream->'truncated','false'::jsonb)='false'::jsonb
   AND abs(stream_tail-(output_terminal-output_last))<=0.000001)
  OR output_stream->'tail_wait_ms'='null'::jsonb),false) output_valid,
 COALESCE(item->>'outcome'='succeeded' AND raw_attempt->>'terminal'='completed'
 AND COALESCE(raw_attempt->>'error','')='' AND raw_stream->>'terminal'='completed'
 AND COALESCE(raw_stream->'truncated','false'::jsonb)='false'::jsonb
 AND ((item->>'transport'='http' AND jsonb_typeof(raw_attempt->'status')='number' AND raw_attempt->>'status' ~ '^2[0-9]{2}$')
  OR (item->>'transport'='ws_turn' AND ws_wait IS NOT NULL)),false) output_complete
 FROM output_fields
), output_samples AS (
 SELECT *,CASE WHEN output_valid AND output_complete AND output_stream->>'status'='observed' AND output_chunks>1 THEN raw_gap END output_gap,
 CASE WHEN output_valid AND output_complete AND output_stream->>'status'='observed' THEN stream_tail END output_tail,
 CASE WHEN output_valid AND NOT output_complete AND output_stream->>'status'='partial' AND output_chunks>1 THEN raw_gap END partial_gap,
 CASE WHEN output_valid AND NOT output_complete AND output_stream->>'status'='partial' THEN stream_tail END partial_tail,
 CASE WHEN output_valid AND output_complete AND output_stream->>'status'='observed'
 AND output_rate->>'status'='estimated' AND output_rate->>'method'='non_reasoning_tokens_over_first_text_to_terminal'
 AND output_rate->>'source'=CASE item->>'protocol' WHEN 'responses' THEN 'canonical_responses_terminal_usage_and_text_events'
  ELSE 'canonical_'||(item->>'protocol')||'_usage_and_text_events' END
 AND output_rate->>'scope'=CASE item->>'transport' WHEN 'ws_turn' THEN 'physical_websocket_turn_responses_text_at_parser'
  ELSE 'physical_http_'||(item->>'protocol')||'_text_at_parser' END
 AND raw_output->>'status'='observed' AND COALESCE(raw_output->>'protocol','responses')=item->>'protocol'
 AND raw_output->>'content_status'=CASE item->>'protocol' WHEN 'responses' THEN 'text_and_reasoning' ELSE item->>'protocol'||'_text_and_reasoning' END
 AND (item->>'protocol'='responses' OR raw_output->'final'='true'::jsonb)
 AND NOT COALESCE(raw_attempt->'events' ? 'typed_output_multiple_candidates',false)
 AND COALESCE(raw_stream->'non_text_output','false'::jsonb)='false'::jsonb
 AND raw_output_tokens BETWEEN 0 AND 9007199254740991 AND trunc(raw_output_tokens)=raw_output_tokens
 AND raw_reasoning_tokens BETWEEN 0 AND raw_output_tokens AND trunc(raw_reasoning_tokens)=raw_reasoning_tokens
 AND raw_output_tokens-raw_reasoning_tokens>=32 AND output_chunks>=2 AND stream_span>=250
 AND output_rate->'output_tokens'=raw_output->'output_tokens' AND output_rate->'reasoning_tokens'=raw_output->'reasoning_tokens'
 AND output_rate->'non_reasoning_tokens'=to_jsonb(raw_output_tokens-raw_reasoning_tokens)
 AND ` + upstreamOutputNumber("raw_attempt->'events'->'first_visible_text'") + `=output_first
 AND rate_window>=250 AND abs(rate_window-(output_terminal-output_first))<=0.000001
 AND rate_value>0 AND abs(rate_value-(raw_output_tokens-raw_reasoning_tokens)*1000/NULLIF(rate_window,0))<=greatest(0.000001,rate_value*0.000000001)
 THEN raw_output_tokens-raw_reasoning_tokens END AS output_tokens
 FROM output_validity
), output_observed AS (
 SELECT *,CASE WHEN output_tokens IS NOT NULL THEN rate_window END output_window FROM output_samples
) `

var upstreamOperationsAttempts = `WITH ` + upstreamOperationsCohort + `, retained AS (
 SELECT trace_id,started_at,account_id,group_id,model AS public_model,stream,detail
 FROM request_connection_traces
 WHERE source='application' AND started_at >= $1 AND started_at < $2
 AND started_at >= statement_timestamp()-interval '30 days'
 AND ($6::bigint=0 OR group_id=$6) AND ($7='' OR $8<>'public' OR model=$7) AND $8::text IS NOT NULL
 UNION ALL
 SELECT d.trace_id,min(d.created_at),min(u.account_id),min(u.group_id),min(COALESCE(NULLIF(u.requested_model,''),u.model)),
 bool_or(u.stream),(array_agg(d.detail))[1]
 FROM request_timing_details d JOIN usage_logs u ON u.id=d.usage_log_id
 WHERE d.created_at >= $1 AND d.created_at < $2 AND d.created_at >= statement_timestamp()-interval '30 days'
 AND NOT EXISTS (SELECT 1 FROM request_connection_traces n WHERE n.trace_id=d.trace_id)
 GROUP BY d.trace_id HAVING count(*)=1
 AND ($6::bigint=0 OR min(u.group_id)=$6)
 AND ($7='' OR $8<>'public' OR min(COALESCE(NULLIF(u.requested_model,''),u.model))=$7)
), traces AS (
 SELECT *,detail->'physical_upstream' projection,` + upstreamTrafficSource("detail") + ` AS traffic_source,
 COALESCE(detail->>'version'='1' AND detail->'physical_upstream'->>'version'='1'
  AND CASE WHEN jsonb_typeof(detail->'physical_upstream'->'attempts')='array'
   THEN jsonb_array_length(detail->'physical_upstream'->'attempts')<=32 ELSE false END
  AND jsonb_typeof(detail->'physical_upstream'->'complete')='boolean',false) capture_known
 FROM retained
), expanded AS (
 SELECT t.*,item,count(*) OVER (PARTITION BY trace_id,item->>'number') AS number_count,
 CASE WHEN item->>'account_id' ~ '^[1-9][0-9]{0,17}$' THEN (item->>'account_id')::bigint ELSE 0 END physical_account_id
 FROM traces t CROSS JOIN LATERAL jsonb_array_elements(
  CASE WHEN capture_known AND jsonb_typeof(projection->'attempts')='array' THEN
   CASE WHEN jsonb_array_length(projection->'attempts')<=32 THEN projection->'attempts' ELSE '[]'::jsonb END
  ELSE '[]'::jsonb END) item
 WHERE item->>'transport' IN ('http','ws_turn') AND item->>'number' ~ '^([1-9]|[12][0-9]|3[0-2])$'
 AND jsonb_typeof(item->'number')='number'
), identities AS (
 SELECT *,CASE WHEN item->>'outcome' IN ('succeeded','failed','interrupted') THEN item->>'outcome' ELSE 'unknown' END outcome,
 COALESCE(item->>'identity_known'='true' AND jsonb_typeof(item->'identity_known')='boolean'
 AND item->>'credential_fingerprint' ~ '^[0-9a-f]{64}$' AND item->>'credential_fingerprint'<>repeat('0',64)
 AND item->>'route_fingerprint' ~ '^[0-9a-f]{64}$' AND item->>'route_fingerprint'<>repeat('0',64),false) identity_known
 FROM expanded e WHERE ($5::bigint=0 OR physical_account_id=$5)
 AND ($4='' OR physical_account_id IN (SELECT id FROM cohort))
 AND number_count=1
), selected AS (
 SELECT *,item->'upstream_request' AS request_scope,
 COALESCE(item->'upstream_request'->>'version'='1'
 AND jsonb_typeof(item->'upstream_request'->'version')='number'
 AND item->'upstream_request'->>'status'='observed'
 AND ((item->>'transport'='http' AND item->'upstream_request'->>'source'='http_transport_body'
  AND item->'upstream_request'->>'protocol' IN ('responses','responses_compact','chat_completions'))
 OR (item->>'transport'='ws_turn' AND item->'upstream_request'->>'source'='websocket_generation_payload'
  AND item->'upstream_request'->>'protocol'='responses'))
 AND jsonb_typeof(item->'upstream_request'->'model')='string'
 AND item->'upstream_request'->>'model' ~ '^[!-~]{1,200}$'
 AND item->'upstream_request'->>'service_tier' IN ('unspecified','auto','default','priority','flex','scale')
 AND item->'upstream_request'->>'reasoning_effort' IN ('unspecified','none','minimal','low','medium','high','xhigh','max')
 AND jsonb_typeof(item->'upstream_request'->'stream')='boolean',false) request_scope_known
 FROM identities
), wait_candidates AS (
 SELECT s.*,s.started_at AS created_at,s.detail AS timing_detail,
 physical.item AS raw_attempt,latency.item AS http_wait,ws_latency.item AS ws_wait
 FROM selected s
 LEFT JOIN LATERAL (
  SELECT (array_agg(expanded.item))[1] item FROM jsonb_array_elements(
   CASE jsonb_typeof(s.detail->'attempts') WHEN 'array' THEN
    CASE WHEN jsonb_array_length(s.detail->'attempts')<=32 THEN s.detail->'attempts' ELSE '[]'::jsonb END
   ELSE '[]'::jsonb END) AS expanded(item)
  WHERE expanded.item->>'number'=s.item->>'number' HAVING count(*)=1
 ) physical ON s.identity_known AND s.request_scope_known
  AND (NOT s.detail ? 'truncated' OR s.detail->'truncated'='false'::jsonb)
  AND jsonb_typeof(physical.item->'number')='number'
  AND jsonb_typeof(physical.item->'account_id')='number'
  AND physical.item->>'kind'=s.item->>'transport'
  AND physical.item->>'account_id'=s.item->>'account_id'
  AND physical.item->>'route_fingerprint'=s.item->>'route_fingerprint'
  AND physical.item->'upstream_credential'->>'status'='observed'
  AND physical.item->'upstream_credential'->>'source'=CASE s.item->>'transport'
   WHEN 'http' THEN 'http_transport_handoff' ELSE 'websocket_handshake_credentials' END
  AND physical.item->'upstream_credential'->>'fingerprint'=s.item->>'credential_fingerprint'
  AND physical.item->'upstream_request'=s.request_scope
 LEFT JOIN LATERAL (
  SELECT (array_agg(expanded.item))[1] item FROM jsonb_array_elements(
   CASE jsonb_typeof(s.detail->'latency'->'attempts') WHEN 'array' THEN
    CASE WHEN jsonb_array_length(s.detail->'latency'->'attempts')<=32 THEN s.detail->'latency'->'attempts' ELSE '[]'::jsonb END
   ELSE '[]'::jsonb END) AS expanded(item)
  WHERE expanded.item->>'number'=s.item->>'number' HAVING count(*)=1
 ) latency ON s.item->>'transport'='http' AND physical.item IS NOT NULL
  AND s.detail->'latency'->'version'='1'::jsonb AND jsonb_typeof(latency.item->'number')='number'
 LEFT JOIN LATERAL (SELECT s.detail->'latency'->'ws_turn' item) ws_latency
 ON s.item->>'transport'='ws_turn' AND physical.item IS NOT NULL
  AND s.detail->>'ttft_mode'='ws_turn' AND s.detail->'status'='0'::jsonb
  AND s.detail->'latency'->'version'='1'::jsonb
  AND ws_latency.item->>'source'='physical_ws_turn_generation_start'
  AND ws_latency.item->'number'='1'::jsonb
  AND COALESCE(s.detail->>'final_response_attempt','0') IN ('0','1')
  AND CASE jsonb_typeof(s.detail->'attempts') WHEN 'array' THEN jsonb_array_length(s.detail->'attempts')=1 ELSE false END
  AND physical.item->'number'='1'::jsonb AND physical.item->'status'='0'::jsonb
  AND COALESCE(physical.item->>'parent','0')='0'
), wait_samples AS (
 SELECT *,` + upstreamWaitValue("http_wait", "post_upload_first_byte_wait") + ` AS wait_first_byte,
 CASE WHEN NOT COALESCE(raw_attempt->'events' ? 'typed_content_truncated',false) THEN
  CASE item->>'transport' WHEN 'ws_turn' THEN ` + upstreamWaitValue("ws_wait", "first_event") + `
  ELSE ` + upstreamWaitValue("http_wait", "post_upload_first_event_wait") + ` END END AS wait_first_event,
 CASE WHEN NOT COALESCE(raw_attempt->'events' ? 'typed_content_truncated',false) THEN
  CASE item->>'transport' WHEN 'ws_turn' THEN ` + upstreamWaitValue("ws_wait", "first_reasoning") + `
  ELSE ` + upstreamWaitValue("http_wait", "post_upload_first_reasoning_wait") + ` END END AS wait_first_reasoning,
 CASE WHEN NOT COALESCE(raw_attempt->'events' ? 'typed_content_truncated',false) THEN
  CASE item->>'transport' WHEN 'ws_turn' THEN ` + upstreamWaitValue("ws_wait", "first_text") + `
  ELSE ` + upstreamWaitValue("http_wait", "post_upload_first_text_wait") + ` END END AS wait_first_text
 FROM wait_candidates
), cache_candidates AS (
 SELECT *,item->'raw_cache' AS cache FROM wait_samples
), cache_samples AS (
 SELECT *,COALESCE(identity_known AND request_scope_known AND raw_attempt IS NOT NULL
  AND jsonb_typeof(item->'account_id')='number'
  AND item->>'protocol'=CASE request_scope->>'protocol' WHEN 'responses_compact' THEN 'responses' ELSE request_scope->>'protocol' END
  AND item->>'outcome'='succeeded' AND cache->'version'='1'::jsonb AND cache->>'status'='observed'
  AND raw_attempt->>'terminal'='completed' AND COALESCE(raw_attempt->>'error','')=''
  AND NOT COALESCE(raw_attempt->'events' ? 'typed_content_truncated',false)
  AND COALESCE(raw_attempt->'text_stream'->'invalid','false'::jsonb)='false'::jsonb
  AND COALESCE(raw_attempt->'text_stream'->'truncated','false'::jsonb)='false'::jsonb
  AND jsonb_typeof(detail->'total_ms')='number' AND jsonb_typeof(raw_attempt->'start_ms')='number'
  AND jsonb_typeof(raw_attempt->'end_ms')='number'
  AND CASE WHEN jsonb_typeof(detail->'total_ms')='number' AND jsonb_typeof(raw_attempt->'start_ms')='number'
   AND jsonb_typeof(raw_attempt->'end_ms')='number' THEN
   (raw_attempt->>'start_ms')::numeric>=0 AND (raw_attempt->>'end_ms')::numeric
   BETWEEN (raw_attempt->>'start_ms')::numeric AND (detail->>'total_ms')::numeric ELSE false END
  AND ((item->>'transport'='http' AND jsonb_typeof(raw_attempt->'status')='number' AND raw_attempt->>'status' ~ '^2[0-9]{2}$'
   AND raw_attempt->'route'->>'endpoint'=request_scope->>'protocol'
   AND ((request_scope->>'protocol' IN ('responses','responses_compact')
    AND cache->>'source'='canonical_responses_terminal_usage' AND cache->>'scope'='physical_http_responses_terminal_usage'
    AND COALESCE(raw_attempt->'text_stream'->>'protocol','responses')='responses'
    AND COALESCE(raw_attempt->'cache_usage'->>'protocol','responses')='responses')
   OR (request_scope->>'protocol'='chat_completions' AND raw_attempt->'text_stream'->>'protocol'='chat_completions'
    AND raw_attempt->'cache_usage'->>'protocol'='chat_completions'
    AND cache->>'source'='canonical_chat_completions_usage' AND cache->>'scope'='physical_http_chat_completions_usage')))
  OR (item->>'transport'='ws_turn' AND detail->>'ttft_mode'='ws_turn' AND detail->'status'='0'::jsonb
   AND raw_attempt->'status'='0'::jsonb AND raw_attempt->'number'='1'::jsonb
   AND COALESCE(raw_attempt->>'parent','0')='0'
   AND CASE jsonb_typeof(detail->'attempts') WHEN 'array' THEN jsonb_array_length(detail->'attempts')=1 ELSE false END
   AND COALESCE(raw_attempt->'cache_usage'->>'protocol','responses')='responses'
   AND COALESCE(raw_attempt->'text_stream'->>'protocol','responses')='responses'
   AND cache->>'source'='canonical_responses_terminal_usage' AND cache->>'scope'='physical_websocket_turn_responses_terminal_usage'))
  AND raw_attempt->'cache_usage'->>'status'='observed'
  AND cache->'input_tokens'=raw_attempt->'cache_usage'->'input_tokens'
  AND cache->'read_tokens'=raw_attempt->'cache_usage'->'read_tokens'
  AND ((cache->'write_tokens'='null'::jsonb AND raw_attempt->'cache_usage'->'write_reported'='false'::jsonb)
   OR (raw_attempt->'cache_usage'->'write_reported'='true'::jsonb AND cache->'write_tokens'=raw_attempt->'cache_usage'->'write_tokens'))
  AND ` + rawCacheValidCounters + `,false) AS cache_observed
 FROM cache_candidates ` + rawCacheCounterJoins + `
 ) ` + upstreamAttemptOutputCTEs + `, attributed AS (
 SELECT trace_id,started_at,created_at,physical_account_id AS account_id,public_model,stream,outcome,identity_known,traffic_source,
 wait_first_byte,wait_first_event,wait_first_reasoning,wait_first_text,cache_observed,raw_input,raw_read,
 output_gap,output_tail,partial_gap,partial_tail,output_tokens,output_window,
 item->>'transport' AS transport,
 CASE WHEN item->>'protocol' IN ('responses','messages','gemini','chat_completions') THEN item->>'protocol' END protocol,
 CASE WHEN identity_known THEN item->>'credential_fingerprint' END credential_fingerprint,
 CASE WHEN identity_known THEN item->>'route_fingerprint' END route_fingerprint,
 CASE WHEN request_scope_known THEN request_scope->>'model' END upstream_model,
 CASE WHEN request_scope_known THEN request_scope->>'protocol' END upstream_protocol,
 CASE WHEN request_scope_known THEN request_scope->>'service_tier' END service_tier,
 CASE WHEN request_scope_known THEN request_scope->>'reasoning_effort' END reasoning_effort,
 CASE WHEN request_scope_known THEN (request_scope->>'stream')::boolean END upstream_stream,
 CASE WHEN request_scope_known THEN 'observed' ELSE 'not_observed' END upstream_model_status
 FROM output_observed
 WHERE ($7='' OR $8<>'upstream' OR request_scope_known AND request_scope->>'model'=$7)
), eligible_traces AS (
 SELECT * FROM traces t WHERE (($5::bigint=0 OR account_id=$5) AND ($4='' OR account_id IN (SELECT id FROM cohort)))
 OR EXISTS (SELECT 1 FROM selected s WHERE s.trace_id=t.trace_id)
), buckets AS (
 SELECT account_id,credential_fingerprint,route_fingerprint,transport,protocol,public_model,stream,traffic_source,
 upstream_model,upstream_protocol,service_tier,reasoning_effort,upstream_stream,upstream_model_status,
 'not_observed'::text AS upstream_group_status,
 ` + upstreamAttemptsAggregate + `,` + upstreamAttemptCacheAggregate + ` raw_cache,
 ` + upstreamAttemptOutputAggregate + ` output,
 jsonb_build_object('version',1,'source',CASE transport WHEN 'ws_turn' THEN 'canonical_physical_ws_turn_generation_start'
 ELSE 'canonical_physical_http_attempt' END,
 'first_byte',` + upstreamWaitAggregate("wait_first_byte") + `,
 'first_event',` + upstreamWaitAggregate("wait_first_event") + `,
 'first_reasoning',` + upstreamWaitAggregate("wait_first_reasoning") + `,
 'first_text',` + upstreamWaitAggregate("wait_first_text") + `) waits FROM attributed
 GROUP BY account_id,credential_fingerprint,route_fingerprint,transport,protocol,public_model,stream,traffic_source,
 upstream_model,upstream_protocol,service_tier,reasoning_effort,upstream_stream,upstream_model_status
), summary AS (SELECT ` + upstreamAttemptsAggregate + ` FROM attributed)
 SELECT jsonb_build_object('version',1,'source','retained_physical_transport_attempts',
 'scope','connection_trace_or_unique_usage_fallback; public_model_is_ingress; upstream_request_metadata_from_complete_transport_body_or_final_ws_payload; supplier_group_not_observed; traffic_is_server_entrypoint_not_caller_intent',
 'retention_days',30,'summary',(SELECT to_jsonb(s) FROM summary s),
 'traffic',` + upstreamTrafficSummary + `,
 'raw_cache',(SELECT ` + upstreamAttemptCacheAggregate + ` FROM attributed),
 'request_scope',(SELECT jsonb_build_object('attempts',count(*),
  'observed_attempts',count(*) FILTER (WHERE request_scope_known),
  'unknown_attempts',count(*) FILTER (WHERE NOT request_scope_known),
  'coverage',count(*) FILTER (WHERE request_scope_known)::numeric/NULLIF(count(*),0)) FROM selected),
 'capture', (SELECT jsonb_build_object('traces',count(*),
  'complete_traces',count(*) FILTER (WHERE capture_known AND projection->>'complete'='true'),
  'unknown_traces',count(*) FILTER (WHERE NOT capture_known OR projection->>'complete'<>'true'),
  'coverage',count(*) FILTER (WHERE capture_known AND projection->>'complete'='true')::numeric/NULLIF(count(*),0)) FROM eligible_traces),
 'total',(SELECT count(*) FROM buckets),'rows',COALESCE((SELECT jsonb_agg(to_jsonb(b)) FROM (
 SELECT * FROM buckets ORDER BY attempts DESC,account_id,credential_fingerprint NULLS LAST,route_fingerprint NULLS LAST,
 transport,protocol NULLS LAST,public_model,stream,upstream_model NULLS LAST,upstream_protocol NULLS LAST,
 service_tier NULLS LAST,reasoning_effort NULLS LAST,upstream_stream NULLS LAST,traffic_source LIMIT $9 OFFSET $10)b),'[]'::jsonb))`
