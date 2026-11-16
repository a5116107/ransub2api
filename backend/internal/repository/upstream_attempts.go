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

const upstreamOperationsAttempts = `WITH ` + upstreamOperationsCohort + `, retained AS (
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
 SELECT *,detail->'physical_upstream' projection,
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
), attributed AS (
 SELECT trace_id,started_at,physical_account_id AS account_id,public_model,stream,outcome,identity_known,
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
 FROM selected
 WHERE ($7='' OR $8<>'upstream' OR request_scope_known AND request_scope->>'model'=$7)
), eligible_traces AS (
 SELECT * FROM traces t WHERE (($5::bigint=0 OR account_id=$5) AND ($4='' OR account_id IN (SELECT id FROM cohort)))
 OR EXISTS (SELECT 1 FROM selected s WHERE s.trace_id=t.trace_id)
), buckets AS (
 SELECT account_id,credential_fingerprint,route_fingerprint,transport,protocol,public_model,stream,
 upstream_model,upstream_protocol,service_tier,reasoning_effort,upstream_stream,upstream_model_status,
 'not_observed'::text AS upstream_group_status,
 ` + upstreamAttemptsAggregate + ` FROM attributed
 GROUP BY account_id,credential_fingerprint,route_fingerprint,transport,protocol,public_model,stream,
 upstream_model,upstream_protocol,service_tier,reasoning_effort,upstream_stream,upstream_model_status
), summary AS (SELECT ` + upstreamAttemptsAggregate + ` FROM attributed)
 SELECT jsonb_build_object('version',1,'source','retained_physical_transport_attempts',
 'scope','connection_trace_or_unique_usage_fallback; public_model_is_ingress; upstream_request_metadata_from_complete_transport_body_or_final_ws_payload; supplier_group_and_business_source_not_observed',
 'retention_days',30,'summary',(SELECT to_jsonb(s) FROM summary s),
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
 service_tier NULLS LAST,reasoning_effort NULLS LAST,upstream_stream NULLS LAST LIMIT $9 OFFSET $10)b),'[]'::jsonb))`
