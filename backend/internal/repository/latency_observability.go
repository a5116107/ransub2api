package repository

import (
	"context"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Usage and ops share boundary arithmetic. Ops additionally retains unbilled
// application traces with unknown cache/effort, deduplicated against usage.
// Physical HTTP samples exclude parents; absent boundaries are never zeros.
func latencyReportSQL(f service.LatencyReportFilter) (string, []any) {
	args := []any{f.Start, f.End}
	filterWhere := func(alias, timeColumn, model string) string {
		where := alias + "." + timeColumn + " >= $1 AND " + alias + "." + timeColumn + " < $2"
		for _, field := range []struct {
			name  string
			value int64
		}{
			{"user_id", f.UserID}, {"api_key_id", f.APIKeyID}, {"account_id", f.AccountID}, {"group_id", f.GroupID},
		} {
			if field.value > 0 {
				args = append(args, field.value)
				where += fmt.Sprintf(" AND %s.%s=$%d", alias, field.name, len(args))
			}
		}
		if f.Model != "" {
			args = append(args, f.Model)
			where += fmt.Sprintf(" AND %s=$%d", model, len(args))
		}
		if f.Platform != "" {
			args = append(args, f.Platform)
			where += fmt.Sprintf(" AND EXISTS (SELECT 1 FROM accounts p WHERE p.id=%s.account_id AND p.platform=$%d)", alias, len(args))
		}
		return where
	}
	where := filterWhere("u", "created_at", "COALESCE(NULLIF(u.requested_model,''),u.model)")
	const cohort = "body_bucket,cache,effort,account_id,model,upstream_model,service_tier,upstream_endpoint,route_fingerprint,proxy_id,stream,cache_source"
	const recentTextWait = `final_attempt AND a->>'account_id'=account_id::text AND upload IS NOT NULL
 AND text_first>=written AND text_first<=parsed_ended
 AND created_at>=LEAST($2::timestamptz,statement_timestamp())-interval '15 minutes'
 AND created_at<=LEAST($2::timestamptz,statement_timestamp())`
	unbilled := ""
	if f.IncludeUnbilled {
		connectionWhere := filterWhere("c", "started_at", "c.model")
		unbilled = ` UNION ALL
 SELECT 'unbilled:'||c.trace_id::text,c.account_id,c.model,'unknown',c.detail,c.trace_id,
 CASE WHEN jsonb_typeof(c.detail->'body_bytes')='number' THEN (c.detail->>'body_bytes')::numeric END,
 'unknown','unknown','unknown',c.stream,0,c.started_at,NULL::numeric,NULL::numeric,NULL::numeric,NULL::jsonb
 FROM request_connection_traces c WHERE c.source='application' AND ` + connectionWhere + `
 AND NOT EXISTS (SELECT 1 FROM request_timing_details d WHERE d.trace_id=c.trace_id)
 AND NOT EXISTS (SELECT 1 FROM usage_logs u WHERE u.api_key_id=c.api_key_id
 AND (u.request_id=c.request_id OR (c.client_request_id<>'' AND u.request_id='client:'||c.client_request_id)))`
	}
	return `WITH usage_traces AS (
 SELECT u.id::text id,u.account_id,COALESCE(NULLIF(u.requested_model,''),u.model) model, COALESCE(NULLIF(u.reasoning_effort,''),'unknown') effort,
 d.detail,d.trace_id,
 CASE WHEN jsonb_typeof(d.detail->'body_bytes')='number' THEN (d.detail->>'body_bytes')::numeric END body,
 COALESCE(NULLIF(u.upstream_model,''),'unknown') upstream_model,
 COALESCE(NULLIF(u.service_tier,''),'unspecified') service_tier,
 COALESCE(NULLIF(u.upstream_endpoint,''),'unknown') upstream_endpoint,
 u.stream,COALESCE(d.trace_total,0) trace_total,u.created_at,
 u.input_tokens::numeric stored_input,u.cache_read_tokens::numeric stored_read,u.cache_creation_tokens::numeric stored_write,
 CASE WHEN jsonb_typeof(d.detail->'attempts')='array' THEN d.detail->'attempts'->-1 END cache_attempt
 FROM usage_logs u LEFT JOIN LATERAL (
  SELECT detail,trace_id,COUNT(*) OVER() trace_total FROM request_timing_details WHERE usage_log_id=u.id
 ) d ON true WHERE ` + where + `
),traces AS (
 SELECT * FROM usage_traces` + unbilled + `
), cache_raw AS (
 SELECT *,cache_attempt->'cache_usage' cu,
 CASE WHEN cache_attempt->>'kind'='http' AND cache_attempt->>'account_id'=account_id::text
  AND cache_attempt->>'route_fingerprint' ~ '^[0-9a-f]{64}$'
  AND cache_attempt->>'route_fingerprint'<>repeat('0',64)
  THEN cache_attempt->>'route_fingerprint' ELSE 'unknown' END route_fingerprint,
 CASE WHEN cache_attempt->>'proxy_id' ~ '^[0-9]{1,18}$' THEN (cache_attempt->>'proxy_id')::bigint ELSE -1 END proxy_id
 FROM traces
), cache_values AS (
 SELECT *,
 CASE WHEN jsonb_typeof(cu->'input_tokens')='number' AND cu->>'input_tokens' ~ '^[0-9]{1,16}$' THEN (cu->>'input_tokens')::numeric END cache_input,
 CASE WHEN jsonb_typeof(cu->'read_tokens')='number' AND cu->>'read_tokens' ~ '^[0-9]{1,16}$' THEN (cu->>'read_tokens')::numeric END cache_read,
 CASE WHEN jsonb_typeof(cu->'write_tokens')='number' AND cu->>'write_tokens' ~ '^[0-9]{1,16}$' THEN (cu->>'write_tokens')::numeric END cache_write
 FROM cache_raw
), cache_checked AS (
 SELECT *,(
  trace_total=1 AND detail->>'version'='1' AND detail->>'truncated'='false'
  AND detail->>'canceled'='false' AND detail->>'client_disconnect'='false' AND detail->>'downstream_error'='false'
  AND COALESCE(detail->>'ttft_mode','')<>'ws_turn'
  AND COALESCE(detail->>'outcome','') IN ('','success') AND COALESCE(detail->>'terminal','') IN ('','completed')
  AND detail->>'status' ~ '^2[0-9]{2}$' AND cache_attempt->>'kind'='http'
  AND cache_attempt->>'account_id'=account_id::text AND cache_attempt->>'status' ~ '^2[0-9]{2}$'
  AND cache_attempt->>'terminal'='completed' AND COALESCE(cache_attempt->>'error','')=''
  AND NOT (cache_attempt->'events' ? 'typed_content_truncated') AND cu->>'status'='observed'
  AND COALESCE(cache_attempt->'text_stream'->>'invalid','false')='false'
  AND COALESCE(cache_attempt->'text_stream'->>'truncated','false')='false'
  AND cache_input>0 AND cache_input<=9007199254740991 AND cache_read>=0 AND cache_read<=cache_input
  AND stored_input>=0 AND stored_read>=0 AND stored_write>=0
  AND stored_input+stored_read+stored_write=cache_input AND stored_read=cache_read
  AND (cu->>'write_reported'='false' OR (cu->>'write_reported'='true'
   AND cache_write>=0 AND cache_write<=cache_input-cache_read AND cache_write=stored_write))
 ) canonical_cache,
 (cu IS NULL AND trace_total<=1 AND stored_input>=0 AND stored_read>0 AND stored_write>=0
  AND COALESCE(detail->>'ttft_mode','')<>'ws_turn' AND COALESCE(cache_attempt->>'kind','')<>'ws_turn'
  AND stored_input+stored_read+stored_write>0) legacy_positive
 FROM cache_values
), classified AS (
 SELECT *,CASE WHEN canonical_cache THEN
  CASE WHEN cache_read=0 THEN '0%' WHEN cache_read/cache_input<.8 THEN '<80%'
   WHEN cache_read/cache_input<.95 THEN '80-95%' ELSE '95%+' END
  WHEN legacy_positive THEN CASE WHEN stored_read/(stored_input+stored_read+stored_write)<.8 THEN '<80%'
   WHEN stored_read/(stored_input+stored_read+stored_write)<.95 THEN '80-95%' ELSE '95%+' END
  ELSE 'unknown' END cache,
 CASE WHEN canonical_cache THEN 'canonical_responses_terminal_usage' WHEN legacy_positive THEN 'billed_usage_positive_read'
  WHEN cu IS NOT NULL THEN 'unverified_terminal_usage' ELSE 'none' END cache_source
 FROM cache_checked
), grouped_traces AS (
 SELECT *,CASE WHEN body IS NULL OR body<0 THEN 'unknown' WHEN body<65536 THEN '<64KiB'
 WHEN body<262144 THEN '64-256KiB' WHEN body<=1048576 THEN '256KiB-1MiB'
 WHEN body<=4194304 THEN '1-4MiB' ELSE '4MiB+' END body_bucket FROM classified
), trace_counts AS (
 SELECT ` + cohort + `,COUNT(DISTINCT id) requests,COUNT(trace_id) traces,
 COUNT(*) FILTER (WHERE detail->'latency'->>'large_body'='true' AND detail->'latency'->>'status'='measured') failover_samples,
 SUM(CASE WHEN detail->'latency'->>'large_body'='true' AND detail->'latency'->>'status'='measured' THEN COALESCE((detail->'latency'->>'account_switches')::bigint,0) ELSE 0 END) switches,
 SUM(CASE WHEN detail->'latency'->>'large_body'='true' AND detail->'latency'->>'status'='measured' THEN COALESCE((detail->'latency'->>'same_account_retries')::bigint,0) ELSE 0 END) retries,
 COUNT(*) FILTER (WHERE detail->'latency'->>'cost_status'='measured_components') costs,
 SUM((detail->'latency'->>'extra_upload_component_ms')::float8) extra_upload,
 SUM((detail->'latency'->>'extra_wait_component_ms')::float8) extra_wait
 FROM grouped_traces GROUP BY ` + cohort + `
), attempts AS (
 SELECT t.*,a.value a,a.n=CASE WHEN jsonb_typeof(detail->'attempts')='array' THEN jsonb_array_length(detail->'attempts') ELSE 0 END AS final_attempt FROM grouped_traces t CROSS JOIN LATERAL jsonb_array_elements(
 CASE WHEN jsonb_typeof(detail->'attempts')='array' THEN detail->'attempts' ELSE '[]'::jsonb END) WITH ORDINALITY a(value,n)
 WHERE a.value->>'kind'='http'
), boundaries AS (
 SELECT *,CASE WHEN jsonb_typeof(a->'start_ms')='number' THEN (a->>'start_ms')::numeric END started,
 CASE WHEN jsonb_typeof(a->'end_ms')='number' THEN (a->>'end_ms')::numeric END ended,
 CASE WHEN jsonb_typeof(detail->'total_ms')='number' THEN (detail->>'total_ms')::numeric END request_ended,
 CASE WHEN jsonb_typeof(a->'events'->'headers_written')='number' THEN (a->'events'->>'headers_written')::numeric END headers,
 CASE WHEN jsonb_typeof(a->'events'->'request_written')='number' THEN (a->'events'->>'request_written')::numeric
 WHEN jsonb_typeof(a->'events'->'wrote_request')='number' THEN (a->'events'->>'wrote_request')::numeric END written,
 CASE WHEN jsonb_typeof(a->'events'->'first_semantic')='number' THEN (a->'events'->>'first_semantic')::numeric END semantic,
 CASE WHEN jsonb_typeof(a->'events'->'first_visible')='number' THEN (a->'events'->>'first_visible')::numeric END visible,
 CASE WHEN NOT (a->'events' ? 'typed_content_truncated') AND jsonb_typeof(a->'events'->'first_reasoning')='number' THEN (a->'events'->>'first_reasoning')::numeric END reasoning,
 CASE WHEN NOT (a->'events' ? 'typed_content_truncated') AND jsonb_typeof(a->'events'->'first_visible_text')='number' THEN (a->'events'->>'first_visible_text')::numeric END text_first,
 CASE WHEN jsonb_typeof(a->'events'->'first_byte')='number' THEN (a->'events'->>'first_byte')::numeric END byte_first
 FROM attempts
), measured AS (
 SELECT *,CASE WHEN ended IS NOT NULL THEN GREATEST(ended,request_ended) END parsed_ended,CASE WHEN started>=0 AND headers>=started AND written>=headers AND ended>=written AND NOT (a->'events' ? 'request_write_failed') THEN written-headers END upload
 FROM boundaries
), samples AS (
 SELECT ` + cohort + `,COUNT(*) attempts,COUNT(upload) uploads,AVG(upload)::float8 upload_avg,
 COUNT(*) FILTER (WHERE upload IS NOT NULL AND semantic>=written AND parsed_ended>=semantic) waits,
 AVG(CASE WHEN upload IS NOT NULL AND semantic>=written AND parsed_ended>=semantic THEN semantic-written END)::float8 wait_avg,
 COUNT(*) FILTER (WHERE upload IS NOT NULL AND visible>=written AND parsed_ended>=visible) visibles,
 AVG(CASE WHEN upload IS NOT NULL AND visible>=written AND parsed_ended>=visible THEN visible-written END)::float8 visible_avg,
 COUNT(*) FILTER (WHERE final_attempt AND a->>'account_id'=account_id::text AND reasoning>=started AND reasoning<=parsed_ended) reasonings,
 AVG(CASE WHEN final_attempt AND a->>'account_id'=account_id::text AND reasoning>=started AND reasoning<=parsed_ended THEN reasoning END)::float8 reasoning_avg,
 COUNT(*) FILTER (WHERE final_attempt AND a->>'account_id'=account_id::text AND text_first>=started AND text_first<=parsed_ended) texts,
 AVG(CASE WHEN final_attempt AND a->>'account_id'=account_id::text AND text_first>=started AND text_first<=parsed_ended THEN text_first END)::float8 text_avg,
 COUNT(*) FILTER (WHERE final_attempt AND a->>'account_id'=account_id::text AND byte_first>=started AND byte_first<=ended) bytes,
 AVG(CASE WHEN final_attempt AND a->>'account_id'=account_id::text AND byte_first>=started AND byte_first<=ended THEN byte_first END)::float8 byte_avg,
 percentile_cont(.5) WITHIN GROUP (ORDER BY text_first) FILTER (WHERE final_attempt AND a->>'account_id'=account_id::text AND text_first>=started AND text_first<=parsed_ended) text_p50,
 percentile_cont(.9) WITHIN GROUP (ORDER BY text_first) FILTER (WHERE final_attempt AND a->>'account_id'=account_id::text AND text_first>=started AND text_first<=parsed_ended) text_p90,
 percentile_cont(.99) WITHIN GROUP (ORDER BY text_first) FILTER (WHERE final_attempt AND a->>'account_id'=account_id::text AND text_first>=started AND text_first<=parsed_ended) text_p99,
 COUNT(*) FILTER (WHERE ` + recentTextWait + `) text_wait_samples,
 percentile_cont(.9) WITHIN GROUP (ORDER BY text_first-written) FILTER (WHERE ` + recentTextWait + `) text_wait_p90,
 MAX(created_at) FILTER (WHERE ` + recentTextWait + `) text_wait_latest
 FROM measured GROUP BY ` + cohort + `
)
 SELECT c.body_bucket,c.cache,c.effort,c.requests,c.traces,COALESCE(s.attempts,0),COALESCE(s.uploads,0),COALESCE(s.waits,0),COALESCE(s.visibles,0),
 s.upload_avg,s.wait_avg,s.visible_avg,COALESCE(c.switches,0),c.failover_samples,COALESCE(c.retries,0),c.costs,c.extra_upload,c.extra_wait,c.account_id,c.model,
 COALESCE(s.reasonings,0),s.reasoning_avg,COALESCE(s.texts,0),s.text_avg,COALESCE(s.bytes,0),s.byte_avg,s.text_p50,s.text_p90,s.text_p99,
 c.upstream_model,c.service_tier,c.upstream_endpoint,c.route_fingerprint,c.proxy_id,c.stream,c.cache_source,
 COALESCE(s.text_wait_samples,0),s.text_wait_p90,s.text_wait_latest
 FROM trace_counts c LEFT JOIN samples s USING(` + cohort + `)
 ORDER BY c.body_bucket,c.cache,c.effort,c.account_id,c.model,c.upstream_model,c.service_tier,c.upstream_endpoint,c.route_fingerprint,c.proxy_id,c.stream,c.cache_source LIMIT 1000`, args
}

func readLatencyGroups(ctx context.Context, sql sqlExecutor, f service.LatencyReportFilter) ([]service.LatencyGroup, error) {
	query, args := latencyReportSQL(f)
	rows, err := sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []service.LatencyGroup{}
	for rows.Next() {
		var v service.LatencyGroup
		if err := rows.Scan(&v.BodyBucket, &v.CacheBucket, &v.ReasoningEffort, &v.Requests, &v.Traces, &v.Attempts, &v.UploadSamples, &v.WaitSamples, &v.VisibleSamples, &v.UploadAvgMS, &v.WaitAvgMS, &v.VisibleAvgMS, &v.LargeBodySwitches, &v.FailoverSamples, &v.LargeBodyRetries, &v.CostSamples, &v.ExtraUploadMS, &v.ExtraWaitMS, &v.AccountID, &v.Model, &v.ReasoningSamples, &v.ReasoningAvgMS, &v.TextSamples, &v.TextAvgMS, &v.ByteSamples, &v.ByteAvgMS, &v.TextP50MS, &v.TextP90MS, &v.TextP99MS, &v.UpstreamModel, &v.ServiceTier, &v.UpstreamEndpoint, &v.RouteFingerprint, &v.ProxyID, &v.Stream, &v.CacheSource, &v.TextWaitSamples, &v.TextWaitP90MS, &v.TextWaitLatest); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (r *usageLogRepository) ReadLatencyReport(ctx context.Context, f service.LatencyReportFilter) ([]service.LatencyGroup, error) {
	return readLatencyGroups(ctx, r.sql, f)
}
func (r *opsRepository) ReadLatencyReport(ctx context.Context, f service.LatencyReportFilter) ([]service.LatencyGroup, error) {
	return readLatencyGroups(ctx, r.db, f)
}
