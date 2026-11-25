package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type operationsAccount struct {
	ID             int64           `json:"id"`
	Name           string          `json:"name"`
	Key            string          `json:"key"`
	Attribution    string          `json:"attribution"`
	UpdatedAt      time.Time       `json:"updated_at"`
	Deleted        bool            `json:"deleted"`
	Schedulable    bool            `json:"schedulable"`
	Concurrency    int             `json:"concurrency"`
	Status         string          `json:"status"`
	Platform       string          `json:"platform"`
	Type           string          `json:"type"`
	RateMultiplier float64         `json:"rate_multiplier"`
	CostMultiplier *float64        `json:"cost_multiplier"`
	BillingProbe   json.RawMessage `json:"billing_probe"`
}

// The projection never includes credentials. Only the selected endpoint fields
// are read internally, and neither DB errors nor raw URLs are logged/returned.
func operationsAccounts(ctx context.Context, tx *sql.Tx, f service.UpstreamOperationsFilter) ([]byte, error) {
	var grokMode string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM settings WHERE key=$1),'')`, service.SettingKeyGrokDefaultBaseURLMode).Scan(&grokMode); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT a.id,a.name,a.platform,a.type,
 jsonb_build_object('base_url',a.credentials->'base_url','api_base_urls',a.credentials->'api_base_urls','account_mode',a.credentials->'account_mode','api_protocol',a.credentials->'api_protocol','provider_profile',a.credentials->'provider_profile','client_id',a.credentials->'client_id','auth_mode',a.credentials->'auth_mode','openai_auth_mode',a.credentials->'openai_auth_mode'),
 jsonb_build_object('custom_base_url_enabled',a.extra->'custom_base_url_enabled','custom_base_url',a.extra->'custom_base_url','openai_siwc',a.extra->'openai_siwc','openai_prism_browser',a.extra->'openai_prism_browser','openai_excel_bps',a.extra->'openai_excel_bps'),
 a.updated_at,a.deleted_at IS NOT NULL,a.parent_account_id,a.schedulable,a.concurrency,a.status,a.rate_multiplier,a.extra->'cost_multiplier',
 CASE WHEN jsonb_typeof(a.extra->'upstream_billing_probe')='object' THEN jsonb_build_object(
 'status',a.extra->'upstream_billing_probe'->'status','received_at',a.extra->'upstream_billing_probe'->'received_at','fresh_until',a.extra->'upstream_billing_probe'->'fresh_until',
 'last_attempt_at',a.extra->'upstream_billing_probe'->'last_attempt_at','next_probe_at',a.extra->'upstream_billing_probe'->'next_probe_at',
 'data',(SELECT jsonb_object_agg(key,value) FROM jsonb_each(CASE WHEN jsonb_typeof(a.extra->'upstream_billing_probe'->'data')='object' THEN a.extra->'upstream_billing_probe'->'data' ELSE '{}'::jsonb END)
 WHERE key IN ('provider','object','schema_version','billing_scope','group','group_rate_source','public_group_rate_multiplier','group_rate_multiplier','user_rate_multiplier','resolved_rate_multiplier','effective_rate_multiplier','peak_rate_enabled','peak_start','peak_end','peak_rate_multiplier','timezone')))
 ELSE 'null'::jsonb END
 FROM accounts a WHERE ($3::bigint=0 OR a.id=$3) AND (a.deleted_at IS NULL OR EXISTS(SELECT 1 FROM usage_logs u WHERE u.account_id=a.id AND u.created_at >= $1 AND u.created_at < $2))`, f.From, f.Until, f.AccountID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []operationsAccount{}
	for rows.Next() {
		var a service.Account
		var creds, extra []byte
		var deleted bool
		var parent sql.NullInt64
		var rate float64
		var cost, billing []byte
		if err = rows.Scan(&a.ID, &a.Name, &a.Platform, &a.Type, &creds, &extra, &a.UpdatedAt, &deleted, &parent, &a.Schedulable, &a.Concurrency, &a.Status, &rate, &cost, &billing); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(creds, &a.Credentials); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(extra, &a.Extra); err != nil {
			return nil, err
		}
		if parent.Valid {
			a.ParentAccountID = &parent.Int64
		}
		key, attribution := service.ResolveOperationsUpstream(&a, service.GrokBaseURLForMode(grokMode))
		if key == "" {
			key = "unknown:" + attribution
		}
		var costValue *float64
		_ = json.Unmarshal(cost, &costValue)
		out = append(out, operationsAccount{ID: a.ID, Name: a.Name, Key: key, Attribution: attribution, UpdatedAt: a.UpdatedAt, Deleted: deleted, Schedulable: a.Schedulable, Concurrency: a.Concurrency, Status: a.Status, Platform: a.Platform, Type: a.Type, RateMultiplier: rate, CostMultiplier: costValue, BillingProbe: billing})
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

const upstreamOperationsCohort = `accounts_config AS (
 SELECT * FROM jsonb_to_recordset($3::jsonb) AS c(id bigint,name text,key text,attribution text,updated_at timestamptz,deleted boolean,schedulable boolean,concurrency integer,status text,platform text,type text,rate_multiplier numeric,cost_multiplier numeric,billing_probe jsonb)
), cohort AS (
 SELECT * FROM accounts_config WHERE ($4='' OR key=$4) AND ($5::bigint=0 OR id=$5)
)`
const upstreamOperationsBase = `WITH ` + upstreamOperationsCohort + `, scoped AS (
 SELECT u.*,COALESCE(c.key,'unknown:account_missing') AS upstream_key,
 COALESCE(c.attribution,'account_missing') AS attribution,c.name AS account_name,c.updated_at AS config_updated_at,c.deleted,
 COALESCE(NULLIF(u.requested_model,''),u.model) AS public_model
 FROM usage_logs u LEFT JOIN accounts_config c ON c.id=u.account_id
 WHERE u.created_at >= $1 AND u.created_at < $2 AND ($5::bigint=0 OR u.account_id=$5)
 AND ($4='' OR COALESCE(c.key,'unknown:account_missing')=$4) AND ($6::bigint=0 OR u.group_id=$6)
 AND ($7='' OR CASE $8 WHEN 'public' THEN COALESCE(NULLIF(u.requested_model,''),u.model) WHEN 'recorded' THEN u.model WHEN 'upstream' THEN u.upstream_model END=$7)
), priced AS (
 SELECT *,
 CASE WHEN stream AND first_token_ms>0 AND (actual_cost>0 OR total_cost>0 OR account_stats_cost>0 OR input_tokens>0 OR output_tokens>0 OR cache_read_tokens>0 OR cache_creation_tokens>0) THEN first_token_ms END AS ttft,
 CASE WHEN duration_ms>0 AND (actual_cost>0 OR total_cost>0 OR account_stats_cost>0 OR input_tokens>0 OR output_tokens>0 OR cache_read_tokens>0 OR cache_creation_tokens>0) THEN duration_ms END AS measured_duration
 FROM scoped
)`

var upstreamOperationsAggregate = `count(*) AS requests,
 count(*) FILTER(WHERE attribution='current_config') AS attributed_requests,
 count(*) FILTER(WHERE attribution<>'current_config') AS unknown_upstream_requests,
 count(*) FILTER(WHERE billing_type=1) AS subscription_requests,
 count(*) FILTER(WHERE deleted) AS deleted_account_requests,
 count(*) FILTER(WHERE ttft IS NOT NULL) AS samples,
 count(*) FILTER(WHERE stream AND ttft IS NULL) AS unknown_samples,
 count(*) FILTER(WHERE actual_cost=0 AND total_cost=0 AND COALESCE(account_stats_cost,0)=0 AND input_tokens=0 AND output_tokens=0 AND cache_read_tokens=0 AND cache_creation_tokens=0) AS unmetered_zero_records,
 percentile_cont(.5) WITHIN GROUP(ORDER BY ttft) AS p50_ms,
 percentile_cont(.9) WITHIN GROUP(ORDER BY ttft) AS p90_ms,
 percentile_cont(.95) WITHIN GROUP(ORDER BY ttft) AS p95_ms,
 percentile_cont(.9) WITHIN GROUP(ORDER BY measured_duration) AS duration_p90_ms,
 max(created_at) AS latest_sample_at,min(config_updated_at) AS oldest_config_updated_at,max(config_updated_at) AS latest_config_updated_at` + queueInclusiveOperationsAggregate("measured_duration")

func (r *channelRepository) ReadUpstreamOperations(ctx context.Context, f service.UpstreamOperationsFilter) (*service.UpstreamOperationsSnapshot, error) {
	if err := service.ValidateUpstreamOperationsFilter(f); err != nil {
		return nil, err
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	raw, err := operationsAccounts(ctx, tx, f)
	if err != nil {
		return nil, err
	}
	args := []any{f.From, f.Until, string(raw), f.Upstream, f.AccountID, f.GroupID, f.Model, f.ModelScope, f.PageSize, (f.Page - 1) * f.PageSize}
	dimension, name := `upstream_key`, `upstream_key`
	switch f.Dimension {
	case "account":
		dimension = `account_id::text`
		name = `COALESCE(max(account_name),'Deleted account') || ' #' || account_id::text`
	case "model":
		dimension = `CASE $8 WHEN 'public' THEN public_model WHEN 'recorded' THEN model WHEN 'upstream' THEN COALESCE(upstream_model,'(unknown upstream model)') END`
		name = dimension
	}
	order := f.Sort + " " + f.Direction + " NULLS LAST,key ASC"
	// Sort and direction are strictly validated before SQL construction.
	query := upstreamOperationsBase + `, buckets AS (SELECT ` + dimension + ` AS key,` + name + ` AS name,` + upstreamOperationsAggregate + ` FROM priced GROUP BY ` + dimension + `), summary AS (SELECT ` + upstreamOperationsAggregate + ` FROM priced)
 SELECT jsonb_build_object('summary',(SELECT to_jsonb(s) FROM summary s),'total',(SELECT count(*) FROM buckets),
 'rows',COALESCE((SELECT jsonb_agg(to_jsonb(b)) FROM (SELECT * FROM buckets ORDER BY ` + order + ` LIMIT $9 OFFSET $10)b),'[]'::jsonb),
 'accounts',COALESCE((SELECT jsonb_agg(to_jsonb(c)) FROM (SELECT id,name,key,attribution,updated_at,deleted FROM cohort WHERE ($5::bigint>0 OR id IN (SELECT account_id FROM scoped)) ORDER BY id LIMIT 100)c),'[]'::jsonb),
 'members',COALESCE((SELECT jsonb_agg(to_jsonb(m)) FROM (SELECT c.*,(SELECT to_jsonb(stats) FROM (SELECT ` + upstreamOperationsAggregate + ` FROM priced WHERE account_id=c.id)stats) AS metrics FROM cohort c WHERE ($6::bigint=0 OR EXISTS(SELECT 1 FROM account_groups ag WHERE ag.account_id=c.id AND ag.group_id=$6)) ORDER BY id LIMIT $9 OFFSET $10)m),'[]'::jsonb),
 'member_total',(SELECT count(*) FROM cohort c WHERE ($6::bigint=0 OR EXISTS(SELECT 1 FROM account_groups ag WHERE ag.account_id=c.id AND ag.group_id=$6))),
 'catalog',COALESCE((SELECT jsonb_agg(to_jsonb(g)) FROM (SELECT key AS key,key AS name,count(*) AS accounts FROM cohort c WHERE NOT deleted AND ($6::bigint=0 OR EXISTS(SELECT 1 FROM account_groups ag WHERE ag.account_id=c.id AND ag.group_id=$6)) GROUP BY key ORDER BY key LIMIT $9 OFFSET $10)g),'[]'::jsonb),
 'capacity',(SELECT jsonb_build_object('accounts',count(*),'schedulable',count(*) FILTER(WHERE schedulable AND status='active'),'configured_concurrency',COALESCE(sum(concurrency),0)) FROM cohort c WHERE NOT deleted AND ($6::bigint=0 OR EXISTS(SELECT 1 FROM account_groups ag WHERE ag.account_id=c.id AND ag.group_id=$6))),
 'catalog_total',(SELECT count(DISTINCT key) FROM cohort c WHERE NOT deleted AND ($6::bigint=0 OR EXISTS(SELECT 1 FROM account_groups ag WHERE ag.account_id=c.id AND ag.group_id=$6))),
 'account_details_limit',100,'historical_upstream_snapshot_requests',0,'error_rate',NULL)`
	out := &service.UpstreamOperationsSnapshot{Scope: f, QueriedAt: time.Now().UTC(), Attribution: "current_account_config_estimate_no_historical_url_snapshot"}
	if err = tx.QueryRowContext(ctx, query, args...).Scan(&out.Usage); err != nil {
		return nil, fmt.Errorf("read upstream usage: %w", err)
	}
	out.Errors = readUpstreamSection(ctx, tx, upstreamOperationsErrors, args[:8]...)
	out.RawCache = readUpstreamSection(ctx, tx, upstreamOperationsRawCache, args...)
	if f.Model != "" && f.ModelScope != "public" {
		out.ModelTests = service.ChannelOperationsSection{Status: "unsupported_scope", Data: json.RawMessage("null"), Error: "Tests record their requested model; mapped or billed aliases cannot be inferred", QueriedAt: out.QueriedAt}
	} else {
		out.ModelTests = readUpstreamSection(ctx, tx, upstreamOperationsModelTests, args...)
	}
	if f.Model != "" && f.ModelScope == "recorded" {
		out.Attempts = service.ChannelOperationsSection{Status: "unsupported_scope", Data: json.RawMessage("null"), Error: "Per-attempt billing model is not observed; final usage aliases cannot be borrowed", QueriedAt: out.QueriedAt}
	} else {
		out.Attempts = readUpstreamSection(ctx, tx, upstreamOperationsAttempts, args...)
	}
	if f.Model != "" && f.ModelScope != "upstream" {
		out.Quality = service.ChannelOperationsSection{Status: "unsupported_scope", Data: json.RawMessage("null"), Error: "Quality uses probe model identity; public/recorded aliases cannot be inferred", QueriedAt: out.QueriedAt}
	} else {
		// Reuse canonical complete frozen plan×round×model classification. Only the
		// account cohort changes; filtering by model still follows round integrity.
		quality := `WITH ` + upstreamOperationsCohort + `, scoped AS (
          SELECT p.account_id,r.id,r.plan_id,r.status,r.created_at,r.pelican_config,r.quality_round_id,r.quality_action,r.quality_judgment,r.error_message,` + qualityMeasuredModel + ` AS measured_model
          FROM scheduled_test_results r JOIN scheduled_test_plans p ON p.id=r.plan_id
          WHERE p.account_id IN (SELECT id FROM cohort) AND r.created_at >= $1 AND r.created_at < $2
          AND ($6::bigint=0 OR EXISTS(SELECT 1 FROM account_groups ag WHERE ag.account_id=p.account_id AND ag.group_id=$6))
          AND jsonb_typeof(r.pelican_config->'quality')='object' AND $8::text IS NOT NULL
        ` + operationsQualityClassification
		quality += ` SELECT jsonb_build_object('source','scheduled_test_results complete frozen plan+round+model; current account cohort', 'summary',(SELECT jsonb_build_object('rounds',count(*),'passed',count(*) FILTER(WHERE outcome='passed'),'degraded',count(*) FILTER(WHERE outcome='degraded'),'inconclusive',count(*) FILTER(WHERE outcome='inconclusive')) FROM classified),'rows',COALESCE((SELECT jsonb_agg(to_jsonb(c)) FROM (SELECT * FROM classified ORDER BY id DESC LIMIT $9 OFFSET $10)c),'[]'::jsonb),'total',(SELECT count(*) FROM classified))`
		out.Quality = readUpstreamSection(ctx, tx, quality, args...)
	}
	out.Availability = readUpstreamAvailability(ctx, tx, f, args)
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func readUpstreamSection(ctx context.Context, tx *sql.Tx, query string, args ...any) service.ChannelOperationsSection {
	out := service.ChannelOperationsSection{Status: "query_failed", Data: json.RawMessage("null"), QueriedAt: time.Now().UTC(), Error: "Data query failed; retry"}
	if _, err := tx.ExecContext(ctx, "SAVEPOINT operations_section"); err != nil {
		return out
	}
	var raw []byte
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&raw); err != nil {
		_, _ = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT operations_section")
		return out
	}
	if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT operations_section"); err != nil {
		return out
	}
	out.Status = "ready"
	out.Error = ""
	out.Data = raw
	return out
}

const upstreamOperationsErrors = `WITH ` + upstreamOperationsCohort + `, params AS (SELECT $8::text AS model_scope), scoped AS (
 SELECT e.error_type,e.error_phase,e.status_code,e.upstream_status_code,e.created_at FROM ops_error_logs e
 WHERE e.created_at >= $1 AND e.created_at < $2 AND COALESCE(e.is_count_tokens,false)=false
 AND ($4='' OR e.account_id IN (SELECT id FROM cohort)) AND ($5::bigint=0 OR e.account_id=$5) AND ($6::bigint=0 OR e.group_id=$6)
 AND ($7='' OR CASE $8 WHEN 'public' THEN COALESCE(NULLIF(e.requested_model,''),e.model) WHEN 'recorded' THEN e.model WHEN 'upstream' THEN e.upstream_model END=$7)
), buckets AS (SELECT error_type,error_phase,status_code,count(*) AS records FROM scoped GROUP BY error_type,error_phase,status_code)
 SELECT jsonb_build_object('source','retained ops_error_logs; current account cohort; count_tokens excluded; no complete request failure denominator','records',(SELECT count(*) FROM scoped),'upstream_5xx',(SELECT count(*) FROM scoped WHERE upstream_status_code>=500),'latest_sample_at',(SELECT max(created_at) FROM scoped),'rows',COALESCE((SELECT jsonb_agg(to_jsonb(b)) FROM (SELECT * FROM buckets ORDER BY records DESC,error_type,error_phase,status_code LIMIT 20)b),'[]'::jsonb),'bucket_total',(SELECT count(*) FROM buckets),'error_rate',NULL)`

var _ service.UpstreamOperationsRepository = (*channelRepository)(nil)

type operationsAvailability struct {
	Version      int                           `json:"version"`
	Source       string                        `json:"source"`
	Reports      []*service.AvailabilityReport `json:"reports"`
	PlanTotal    int                           `json:"plan_total"`
	ModelTotal   int                           `json:"model_total"`
	ModelsLoaded int                           `json:"models_loaded"`
	Truncated    bool                          `json:"truncated"`
}

// Current decisions use the original authority in this SAME snapshot; neither
// historical usage nor group successes are substituted for account evidence.
func readUpstreamAvailability(ctx context.Context, tx *sql.Tx, f service.UpstreamOperationsFilter, args []any) service.ChannelOperationsSection {
	out := service.ChannelOperationsSection{Status: "query_failed", Data: json.RawMessage("null"), Error: "Availability query failed; retry"}
	if f.Model != "" && f.ModelScope != "public" {
		out.Status, out.Error = "unsupported_scope", "Availability model filters use configured request models, not billed or mapped aliases"
		return out
	}
	if _, err := tx.ExecContext(ctx, "SAVEPOINT operations_availability"); err != nil {
		return out
	}
	success := false
	defer func() {
		if !success {
			_, _ = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT operations_availability")
		}
		_, _ = tx.ExecContext(ctx, "RELEASE SAVEPOINT operations_availability")
	}()
	query := `WITH ` + upstreamOperationsCohort + `,
 members AS (SELECT id FROM cohort c WHERE ($6::bigint=0 OR EXISTS(SELECT 1 FROM account_groups ag WHERE ag.account_id=c.id AND ag.group_id=$6)) ORDER BY id LIMIT $9 OFFSET $10),
 plans AS (SELECT id,CASE WHEN jsonb_typeof(pelican_config->'model_ids')='array' THEN GREATEST(jsonb_array_length(pelican_config->'model_ids'),1) ELSE 1 END AS models,
 octet_length(pelican_config::text) AS config_bytes FROM scheduled_test_plans WHERE account_id IN(SELECT id FROM members) AND pelican_config ? 'availability'
 AND ($7='' OR model_id=$7 OR (pelican_config->'model_ids') ? $7) AND $1::timestamptz IS NOT NULL AND $2::timestamptz IS NOT NULL AND $8::text IS NOT NULL)
 SELECT id,models,config_bytes,count(*) OVER(),sum(models) OVER() FROM plans ORDER BY id LIMIT 100`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return out
	}
	type plan struct {
		id            int64
		models, bytes int
	}
	var plans []plan
	data := operationsAvailability{Version: 1, Source: "current_account_availability_authority", Reports: []*service.AvailabilityReport{}}
	for rows.Next() {
		var p plan
		if err = rows.Scan(&p.id, &p.models, &p.bytes, &data.PlanTotal, &data.ModelTotal); err != nil {
			break
		}
		plans = append(plans, p)
	}
	rowErr := rows.Err()
	_ = rows.Close()
	if err != nil || rowErr != nil {
		return out
	}
	readModels := 0
	for _, p := range plans {
		if p.models > 50 || p.bytes > 65536 || readModels+p.models > 100 {
			data.Truncated = true
			continue
		}
		readModels += p.models
		reports, readErr := readAvailabilityReportsInTx(ctx, tx, "id", p.id, nil)
		if readErr != nil {
			return out
		}
		for _, report := range reports {
			if f.Model != "" {
				models := report.Models[:0]
				for _, model := range report.Models {
					if model.ModelID == f.Model {
						models = append(models, model)
					}
				}
				report.Models = models
			}
			data.ModelsLoaded += len(report.Models)
			data.Reports = append(data.Reports, report)
		}
	}
	data.Truncated = data.Truncated || data.PlanTotal > len(data.Reports)
	// Re-evaluate after ALL reads so an earlier report cannot outlive its TTL.
	var at time.Time
	if err = tx.QueryRowContext(ctx, "SELECT clock_timestamp()").Scan(&at); err != nil {
		return out
	}
	for _, report := range data.Reports {
		report.EvaluatedAt = at
		for i := range report.Models {
			model := &report.Models[i]
			if model.Reason == "latest_decision" || model.Reason == "expired" {
				model.Status = model.StatusAt(at)
				if model.Status == "stale" {
					model.Reason = "expired"
				}
			}
		}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return out
	}
	out.Status, out.Error, out.Data, out.QueriedAt = "ready", "", raw, at
	success = true
	return out
}

// Only persisted metadata is projected. Artwork remains behind the original
// admin result endpoint; previous attempts never become successful account tests.
const upstreamOperationsModelTests = `WITH ` + upstreamOperationsCohort + `,
 members AS (SELECT id FROM cohort c WHERE NOT deleted AND ($6::bigint=0 OR EXISTS(SELECT 1 FROM account_groups ag WHERE ag.account_id=c.id AND ag.group_id=$6)) ORDER BY id LIMIT $9 OFFSET $10),
 observed AS (
 SELECT r.id,r.plan_id,p.group_id,r.account_id,r.status,
 CASE WHEN jsonb_typeof(r.pelican_config->'model_id')='string' THEN NULLIF(r.pelican_config->>'model_id','') END AS model,
 CASE WHEN jsonb_typeof(r.pelican_config->'reasoning_effort')='string' THEN r.pelican_config->>'reasoning_effort' END AS reasoning_effort,
 r.started_at,r.finished_at,r.latency_ms,(r.status='success' AND r.response_text<>'') AS has_output,
 CASE WHEN jsonb_typeof(r.attempts)='array' THEN (SELECT COALESCE(jsonb_agg(jsonb_build_object('account_id',a->'account_id')),'[]'::jsonb) FROM jsonb_array_elements(r.attempts) a) ELSE '[]'::jsonb END AS attempts
 FROM pelican_group_test_results r JOIN pelican_group_test_plans p ON p.id=r.plan_id
 WHERE r.account_id IN (SELECT id FROM members) AND r.started_at >= $1 AND r.started_at < $2
 AND ($6::bigint=0 OR p.group_id=$6) AND ($7='' OR r.pelican_config->>'model_id'=$7) AND $8::text IS NOT NULL
), latest AS (
 SELECT DISTINCT ON(account_id,group_id,model,reasoning_effort) * FROM observed ORDER BY account_id,group_id,model,reasoning_effort,started_at DESC,id DESC
)
 SELECT jsonb_build_object('version',1,'source','persisted_pelican_group_test_frozen_model','rows',COALESCE((SELECT jsonb_agg(to_jsonb(x)) FROM (
 SELECT l.*,(SELECT to_jsonb(art) FROM (SELECT o.id,o.account_id,o.group_id,o.model,o.reasoning_effort,o.started_at FROM observed o WHERE o.has_output AND (o.started_at,o.id)<=(l.started_at,l.id) AND o.account_id=l.account_id AND o.group_id=l.group_id AND o.model IS NOT DISTINCT FROM l.model AND o.reasoning_effort IS NOT DISTINCT FROM l.reasoning_effort ORDER BY o.started_at DESC,o.id DESC LIMIT 1)art) AS artwork
 FROM latest l ORDER BY account_id,started_at DESC,id DESC LIMIT 100)x),'[]'::jsonb),'total',(SELECT count(*) FROM latest))`
