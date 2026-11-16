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
 AND ($6::bigint=0 OR group_id=$6) AND ($7='' OR model=$7) AND $8::text IS NOT NULL
 UNION ALL
 SELECT d.trace_id,min(d.created_at),min(u.account_id),min(u.group_id),min(COALESCE(NULLIF(u.requested_model,''),u.model)),
 bool_or(u.stream),(array_agg(d.detail))[1]
 FROM request_timing_details d JOIN usage_logs u ON u.id=d.usage_log_id
 WHERE d.created_at >= $1 AND d.created_at < $2 AND d.created_at >= statement_timestamp()-interval '30 days'
 AND NOT EXISTS (SELECT 1 FROM request_connection_traces n WHERE n.trace_id=d.trace_id)
 GROUP BY d.trace_id HAVING count(*)=1
 AND ($6::bigint=0 OR min(u.group_id)=$6)
 AND ($7='' OR min(COALESCE(NULLIF(u.requested_model,''),u.model))=$7)
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
), selected AS (
 SELECT *,CASE WHEN item->>'outcome' IN ('succeeded','failed','interrupted') THEN item->>'outcome' ELSE 'unknown' END outcome,
 COALESCE(item->>'identity_known'='true' AND jsonb_typeof(item->'identity_known')='boolean'
 AND item->>'credential_fingerprint' ~ '^[0-9a-f]{64}$' AND item->>'credential_fingerprint'<>repeat('0',64)
 AND item->>'route_fingerprint' ~ '^[0-9a-f]{64}$' AND item->>'route_fingerprint'<>repeat('0',64),false) identity_known
 FROM expanded e WHERE ($5::bigint=0 OR physical_account_id=$5)
 AND ($4='' OR physical_account_id IN (SELECT id FROM cohort))
 AND number_count=1
), attributed AS (
 SELECT trace_id,started_at,physical_account_id AS account_id,public_model,stream,outcome,identity_known,
 item->>'transport' AS transport,
 CASE WHEN item->>'protocol' IN ('responses','messages','gemini','chat_completions') THEN item->>'protocol' END protocol,
 CASE WHEN identity_known THEN item->>'credential_fingerprint' END credential_fingerprint,
 CASE WHEN identity_known THEN item->>'route_fingerprint' END route_fingerprint
 FROM selected
), eligible_traces AS (
 SELECT * FROM traces t WHERE (($5::bigint=0 OR account_id=$5) AND ($4='' OR account_id IN (SELECT id FROM cohort)))
 OR EXISTS (SELECT 1 FROM selected s WHERE s.trace_id=t.trace_id)
), buckets AS (
 SELECT account_id,credential_fingerprint,route_fingerprint,transport,protocol,public_model,stream,
 'not_observed'::text AS upstream_model_status,'not_observed'::text AS upstream_group_status,
 ` + upstreamAttemptsAggregate + ` FROM attributed
 GROUP BY account_id,credential_fingerprint,route_fingerprint,transport,protocol,public_model,stream
), summary AS (SELECT ` + upstreamAttemptsAggregate + ` FROM attributed)
 SELECT jsonb_build_object('version',1,'source','retained_physical_transport_attempts',
 'scope','connection_trace_or_unique_usage_fallback; public_model_is_ingress; upstream_model_group_tier_source_not_observed',
 'retention_days',30,'summary',(SELECT to_jsonb(s) FROM summary s),
 'capture', (SELECT jsonb_build_object('traces',count(*),
  'complete_traces',count(*) FILTER (WHERE capture_known AND projection->>'complete'='true'),
  'unknown_traces',count(*) FILTER (WHERE NOT capture_known OR projection->>'complete'<>'true'),
  'coverage',count(*) FILTER (WHERE capture_known AND projection->>'complete'='true')::numeric/NULLIF(count(*),0)) FROM eligible_traces),
 'total',(SELECT count(*) FROM buckets),'rows',COALESCE((SELECT jsonb_agg(to_jsonb(b)) FROM (
 SELECT * FROM buckets ORDER BY attempts DESC,account_id,credential_fingerprint NULLS LAST,route_fingerprint NULLS LAST,
 transport,protocol NULLS LAST,public_model,stream LIMIT $9 OFFSET $10)b),'[]'::jsonb))`
