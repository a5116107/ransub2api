//go:build integration

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/testutil/assertions"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestUpstreamOperationsHalfOpenPagingGroupsAndPathIsolation(t *testing.T) {
	f := newChannelOperationsFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	from := now.Add(-24 * time.Hour)
	for _, a := range []struct {
		id   int64
		base string
	}{{f.account, "https://same.example:8443/A/v1/"}, {f.outsider, "https://same.example:8443/B/v1/"}} {
		_, err := integrationDB.Exec(`UPDATE accounts SET type='apikey',credentials=jsonb_build_object('base_url',$2::text),extra='{"cost_multiplier":0}' WHERE id=$1`, a.id, a.base)
		require.NoError(t, err)
		for i, at := range []time.Time{from, now, from.Add(-time.Microsecond)} {
			_, err = integrationDB.Exec(`INSERT INTO usage_logs(user_id,api_key_id,account_id,request_id,model,total_cost,actual_cost,group_id,created_at) VALUES($1,$2,$3,$4,'per-request',2,1,$5,$6)`, f.user, f.key, a.id, fmt.Sprintf("range-%d-%d", a.id, i), f.group, at)
			require.NoError(t, err)
		}
	}
	q := service.UpstreamOperationsFilter{From: from, Until: now, GroupID: f.group, Dimension: "upstream", ModelScope: "public", Sort: "name", Direction: "asc", Page: 1, PageSize: 1}
	read := func() map[string]any {
		out, err := f.repo.ReadUpstreamOperations(context.Background(), q)
		require.NoError(t, err)
		var data map[string]any
		require.NoError(t, json.Unmarshal(out.Usage, &data))
		require.Equal(t, "ready", out.Errors.Status)
		if q.Model != "" && q.ModelScope != "upstream" {
			require.Equal(t, "unsupported_scope", out.Quality.Status)
		} else {
			require.Equal(t, "ready", out.Quality.Status)
		}
		return data
	}
	first := read()
	require.Equal(t, 2.0, first["total"])
	require.Equal(t, 2.0, assertions.Type[map[string]any](t, first["summary"])["requests"])
	require.Len(t, first["rows"], 1)
	require.Equal(t, "https://same.example:8443/A/v1", assertions.Type[map[string]any](t, assertions.Type[[]any](t, first["rows"])[0])["key"])
	q.Page = 2
	second := read()
	require.Equal(t, first["summary"], second["summary"])
	require.Equal(t, "https://same.example:8443/B/v1", assertions.Type[map[string]any](t, assertions.Type[[]any](t, second["rows"])[0])["key"])
	q.Model = "nonexistent"
	empty := read()
	require.Equal(t, 0.0, assertions.Type[map[string]any](t, empty["summary"])["requests"])
}

func TestUpstreamOperationsQualityRetainsCompleteFrozenRoundBoundary(t *testing.T) {
	f := newChannelOperationsFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	_, err := integrationDB.Exec(`UPDATE accounts SET type='apikey',credentials='{"base_url":"https://quality.example/v1"}' WHERE id=$1`, f.account)
	require.NoError(t, err)
	var plan int64
	require.NoError(t, integrationDB.QueryRow(`INSERT INTO scheduled_test_plans(account_id,model_id,cron_expression,enabled,pelican_config) VALUES($1,'current-model','*/30 * * * *',false,'{"quality":{}}') RETURNING id`, f.account).Scan(&plan))
	_, err = integrationDB.Exec(`INSERT INTO scheduled_test_results(plan_id,status,started_at,finished_at,created_at,pelican_config,quality_round_id,quality_action,quality_judgment,error_message) VALUES($1,'success',$2,$2,$2,'{"quality":{},"model_id":"a","model_ids":["a","b"],"parallel_count":1}','frozen-peer','passed','{"verdict":"correct"}','')`, plan, now.Add(-time.Minute))
	require.NoError(t, err)
	q := service.UpstreamOperationsFilter{From: now.Add(-time.Hour), Until: now, Upstream: "https://quality.example/v1", AccountID: f.account, Model: "a", ModelScope: "upstream", Dimension: "model", Sort: "requests", Direction: "desc", Page: 1, PageSize: 20}
	out, err := f.repo.ReadUpstreamOperations(context.Background(), q)
	require.NoError(t, err)
	require.Equal(t, "ready", out.Quality.Status)
	var quality struct {
		Summary struct{ Passed, Inconclusive int }
		Rows    []struct {
			Complete bool `json:"round_complete"`
			Outcome  string
		}
	}
	require.NoError(t, json.Unmarshal(out.Quality.Data, &quality))
	require.Zero(t, quality.Summary.Passed, "missing peer still cannot pass after changing upstream cohort")
	require.Equal(t, 1, quality.Summary.Inconclusive)
	require.Len(t, quality.Rows, 1)
	require.False(t, quality.Rows[0].Complete)
}

func TestUpstreamOperationsIdleAccountsDiscoveredWithoutAnyModelChannel(t *testing.T) {
	var id int64
	require.NoError(t, integrationDB.QueryRow(`INSERT INTO accounts(name,platform,type,credentials) VALUES('idle upstream discovery','openai','apikey','{"base_url":"https://idle.example:8443/A/v1/"}') RETURNING id`).Scan(&id))
	t.Cleanup(func() { _, _ = integrationDB.Exec(`DELETE FROM accounts WHERE id=$1`, id) })
	now := time.Now().UTC()
	q := service.UpstreamOperationsFilter{From: now.Add(-time.Hour), Until: now, AccountID: id, Dimension: "upstream", ModelScope: "public", Sort: "requests", Direction: "desc", Page: 1, PageSize: 20}
	out, err := (&channelRepository{db: integrationDB}).ReadUpstreamOperations(context.Background(), q)
	require.NoError(t, err)
	var data struct {
		Summary struct{ Requests int }
		Catalog []struct {
			Key      string
			Accounts int
		}
		Members []struct {
			ID  int64
			Key string
		}
		MemberTotal int `json:"member_total"`
	}
	require.NoError(t, json.Unmarshal(out.Usage, &data))
	require.Zero(t, data.Summary.Requests)
	require.Len(t, data.Catalog, 1)
	require.Equal(t, "https://idle.example:8443/A/v1", data.Catalog[0].Key)
	require.Equal(t, 1, data.Catalog[0].Accounts)
	require.Len(t, data.Members, 1)
	require.Equal(t, id, data.Members[0].ID)
	require.Equal(t, 1, data.MemberTotal)
}

func TestUpstreamOperationsLineCardsPriceAndFrozenPelicanModels(t *testing.T) {
	f := newChannelOperationsFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	_, err := integrationDB.Exec(`UPDATE accounts SET type='apikey',rate_multiplier=0.2,credentials='{"api_key":"fixture-must-not-leak","base_url":"https://line.example/v1"}',extra='{"cost_multiplier":0.08,"upstream_billing_probe":{"status":"unsupported","data":{"provider":"new_api","object":"new_api.group_billing","billing_scope":"group","group_rate_source":"unconfirmed","public_group_rate_multiplier":0.1,"api_key":"must-not-leak","raw_body":"must-not-leak"}}}' WHERE id=$1`, f.account)
	require.NoError(t, err)
	repo := NewPelicanGroupTestRepository(integrationDB)
	plan, err := repo.CreatePlan(t.Context(), newPelicanGroupTestPlan(f.group, true, now.Add(time.Hour)))
	require.NoError(t, err)
	cfg := &service.PelicanTestConfig{ModelID: "frozen-model", ReasoningEffort: "low"}
	old, err := repo.CreateResult(t.Context(), &service.PelicanGroupTestResult{PlanID: plan.ID, AccountID: f.account, Status: "success", ResponseText: "<svg>fixture-body-do-not-return</svg>", PelicanConfig: cfg, StartedAt: now.Add(-20 * time.Minute), FinishedAt: now.Add(-19 * time.Minute)})
	require.NoError(t, err)
	latest, err := repo.CreateResult(t.Context(), &service.PelicanGroupTestResult{PlanID: plan.ID, AccountID: f.account, Status: "failed", ErrorMessage: "fixture-error-do-not-return", Attempts: []service.PelicanGroupTestAttempt{{AccountID: f.outsider, Error: "fixture-attempt-do-not-return"}}, PelicanConfig: cfg, StartedAt: now.Add(-10 * time.Minute), FinishedAt: now.Add(-9 * time.Minute)})
	require.NoError(t, err)
	_, err = repo.CreateResult(t.Context(), &service.PelicanGroupTestResult{PlanID: plan.ID, AccountID: f.outsider, Status: "success", ResponseText: "<svg>outsider</svg>", PelicanConfig: cfg, StartedAt: now.Add(-5 * time.Minute), FinishedAt: now.Add(-4 * time.Minute)})
	require.NoError(t, err)
	_, err = repo.CreateResult(t.Context(), &service.PelicanGroupTestResult{PlanID: plan.ID, AccountID: f.account, Status: "success", ResponseText: "<svg>legacy</svg>", StartedAt: now.Add(-3 * time.Minute), FinishedAt: now.Add(-2 * time.Minute)})
	require.NoError(t, err)
	_, err = integrationDB.Exec(`UPDATE pelican_group_test_plans SET model_id='changed-plan-model' WHERE id=$1`, plan.ID)
	require.NoError(t, err)
	q := service.UpstreamOperationsFilter{From: now.Add(-time.Hour), Until: now, AccountID: f.account, GroupID: f.group, Model: "frozen-model", ModelScope: "public", Dimension: "account", Sort: "name", Direction: "asc", Page: 1, PageSize: 20}
	out, err := f.repo.ReadUpstreamOperations(t.Context(), q)
	require.NoError(t, err)
	require.Equal(t, "ready", out.ModelTests.Status, string(out.ModelTests.Data))
	var tests struct {
		Rows []struct {
			ID, AccountID int64
			Model         string
			Status        string
			Artwork       struct{ ID int64 } `json:"artwork"`
		} `json:"rows"`
	}
	require.NoError(t, json.Unmarshal(out.ModelTests.Data, &tests))
	require.Len(t, tests.Rows, 1)
	require.Equal(t, latest.ID, tests.Rows[0].ID)
	require.Equal(t, "failed", tests.Rows[0].Status)
	require.Equal(t, "frozen-model", tests.Rows[0].Model)
	require.Equal(t, old.ID, tests.Rows[0].Artwork.ID)
	for _, secret := range []string{"fixture-body-do-not-return", "fixture-error-do-not-return", "fixture-attempt-do-not-return", "changed-plan-model"} {
		require.NotContains(t, string(out.ModelTests.Data), secret)
	}
	require.NotContains(t, string(out.Usage), "must-not-leak")
	var usage struct {
		Members []struct {
			Rate    float64 `json:"rate_multiplier"`
			Cost    float64 `json:"cost_multiplier"`
			Metrics struct {
				Requests int64 `json:"requests"`
			}
			Billing json.RawMessage `json:"billing_probe"`
		} `json:"members"`
	}
	require.NoError(t, json.Unmarshal(out.Usage, &usage))
	require.Len(t, usage.Members, 1)
	require.Equal(t, 0.2, usage.Members[0].Rate)
	require.Equal(t, 0.08, usage.Members[0].Cost)
	require.Contains(t, string(usage.Members[0].Billing), "public_group_rate_multiplier")
	q.ModelScope = "upstream"
	out, err = f.repo.ReadUpstreamOperations(t.Context(), q)
	require.NoError(t, err)
	require.Equal(t, "unsupported_scope", out.ModelTests.Status)
	q.ModelScope = "public"
	q.Model = "changed-plan-model"
	out, err = f.repo.ReadUpstreamOperations(t.Context(), q)
	require.NoError(t, err)
	require.Contains(t, string(out.ModelTests.Data), `"total": 0`)
}

func TestUpstreamOperationsAvailabilityUsesCurrentAuthority(t *testing.T) {
	f := newChannelOperationsFixture(t)
	_, err := integrationDB.Exec(`UPDATE accounts SET type='apikey' WHERE id=$1`, f.account)
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Microsecond)
	var plan int64
	require.NoError(t, integrationDB.QueryRow(`INSERT INTO scheduled_test_plans(account_id,model_id,cron_expression,enabled,pelican_config) VALUES($1,'target','*/30 * * * *',true,'{"availability":{"valid_for_seconds":60}}') RETURNING id`, f.account).Scan(&plan))
	filter := service.UpstreamOperationsFilter{From: now.Add(-time.Hour), Until: now, AccountID: f.account, Model: "target", ModelScope: "public", Dimension: "account", Sort: "name", Direction: "asc", Page: 1, PageSize: 20}
	read := func() service.ChannelOperationsSection {
		t.Helper()
		out, err := f.repo.ReadUpstreamOperations(t.Context(), filter)
		require.NoError(t, err)
		raw, err := json.Marshal(out)
		require.NoError(t, err)
		var projection struct {
			Availability service.ChannelOperationsSection `json:"availability"`
		}
		require.NoError(t, json.Unmarshal(raw, &projection))
		return projection.Availability
	}
	first := read()
	require.Equal(t, "ready", first.Status)
	require.Contains(t, string(first.Data), `"model_id":"target"`)
	require.Contains(t, string(first.Data), `"status":"unknown"`)
	require.Contains(t, string(first.Data), `"reason":"not_observed"`)
	writeState := func(status string, age time.Duration) {
		t.Helper()
		tx, err := integrationDB.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		fp, err := accountRouteFingerprintWithBinding(t.Context(), tx, f.account, false, nil)
		require.NoError(t, err)
		var observed time.Time
		require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT clock_timestamp()-interval '1 second'`).Scan(&observed))
		observed = observed.Add(-age)
		until := observed.Add(time.Minute)
		probe := &service.AvailabilityProbeResult{Version: 1, Status: status, Reason: "completed_response", StartedAt: observed.Add(-time.Second), FinishedAt: observed, AttemptCount: 1, Scope: &service.AvailabilityRequestScope{Version: 2, Protocol: "responses_http", Model: "wire-target", Capability: "text", ServiceTier: "default", ReasoningEffort: "low"}}
		fault := int64(0)
		state, err := json.Marshal(availabilityStoredState{AvailabilityState: service.AvailabilityState{Decision: probe, Observation: &service.AvailabilityProbeResult{Version: 1, Status: "inconclusive", StartedAt: observed, FinishedAt: observed}, ValidUntil: &until}, FaultVersion: &fault})
		require.NoError(t, err)
		_, err = tx.ExecContext(t.Context(), `INSERT INTO account_availability_states(plan_id,model_id,plan_version,route_fingerprint,round_lease,state) SELECT id,'target',updated_at,$2,clock_timestamp(),$3 FROM scheduled_test_plans WHERE id=$1 ON CONFLICT(plan_id,model_id) DO UPDATE SET plan_version=EXCLUDED.plan_version,route_fingerprint=EXCLUDED.route_fingerprint,state=EXCLUDED.state`, plan, fp[:], state)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
	}
	for _, status := range []string{"pass", "fail", "pass"} {
		writeState(status, 0)
		var before, after string
		require.NoError(t, integrationDB.QueryRow(`SELECT state::text FROM account_availability_states WHERE plan_id=$1`, plan).Scan(&before))
		section := read()
		require.Contains(t, string(section.Data), `"status":"`+status+`"`)
		require.Contains(t, string(section.Data), `"status":"inconclusive"`)
		require.Contains(t, string(section.Data), `"model":"wire-target"`)
		require.NotContains(t, string(section.Data), "route_fingerprint")
		require.NotContains(t, string(section.Data), "fault_version")
		require.NotContains(t, string(section.Data), "synthetic-fixture-only")
		require.NoError(t, integrationDB.QueryRow(`SELECT state::text FROM account_availability_states WHERE plan_id=$1`, plan).Scan(&after))
		require.Equal(t, before, after, "snapshot reads must not change availability state")
	}
	writeState("pass", 2*time.Minute)
	require.Contains(t, string(read().Data), `"reason":"expired"`)
	writeState("pass", 0)
	_, err = integrationDB.Exec(`UPDATE scheduled_test_plans SET availability_fault_version=availability_fault_version+1 WHERE id=$1`, plan)
	require.NoError(t, err)
	require.Contains(t, string(read().Data), `"reason":"new_fault_event"`)
	_, err = integrationDB.Exec(`UPDATE scheduled_test_plans SET availability_fault_version=0 WHERE id=$1`, plan)
	require.NoError(t, err)
	require.Contains(t, string(read().Data), `"reason":"latest_decision"`)
	filter.ModelScope = "upstream"
	require.Equal(t, "unsupported_scope", read().Status)
	filter.ModelScope = "public"
	filter.Model = "other-model"
	require.NotContains(t, string(read().Data), `"model_id":"target"`)
	filter.Model = "target"
	_, err = integrationDB.Exec(`UPDATE accounts SET credentials='{"api_key":"rotated-fixture-only"}' WHERE id=$1`, f.account)
	require.NoError(t, err)
	require.Contains(t, string(read().Data), `"reason":"route_changed"`)
	writeState("pass", 0)
	_, err = integrationDB.Exec(`UPDATE scheduled_test_plans SET enabled=false WHERE id=$1`, plan)
	require.NoError(t, err)
	require.Contains(t, string(read().Data), `"reason":"rule_paused"`)
}

func TestUpstreamOperationsAvailabilityLimitsPagingAndFailureRecovery(t *testing.T) {
	f := newChannelOperationsFixture(t)
	_, err := integrationDB.Exec(`INSERT INTO scheduled_test_plans(account_id,model_id,cron_expression,enabled,pelican_config) SELECT $1,'target','*/30 * * * *',false,'{"availability":{"valid_for_seconds":60}}'::jsonb FROM generate_series(1,101)`, f.account)
	require.NoError(t, err)
	now := time.Now().UTC()
	filter := service.UpstreamOperationsFilter{From: now.Add(-time.Hour), Until: now, AccountID: f.account, Dimension: "upstream", ModelScope: "public", Sort: "name", Direction: "asc", Page: 1, PageSize: 1}
	read := func() *service.UpstreamOperationsSnapshot {
		t.Helper()
		out, readErr := f.repo.ReadUpstreamOperations(t.Context(), filter)
		require.NoError(t, readErr)
		return out
	}
	out := read()
	require.Equal(t, "ready", out.Availability.Status)
	var data operationsAvailability
	require.NoError(t, json.Unmarshal(out.Availability.Data, &data))
	require.Equal(t, 101, data.PlanTotal)
	require.Equal(t, 100, data.ModelsLoaded)
	require.Len(t, data.Reports, 100)
	require.True(t, data.Truncated)
	filter.Page = 2
	out = read()
	require.NoError(t, json.Unmarshal(out.Availability.Data, &data))
	require.Empty(t, data.Reports)
	filter.Page = 1
	_, err = integrationDB.Exec(`UPDATE scheduled_test_plans SET pelican_config=jsonb_set(pelican_config,'{availability}','"bad"'::jsonb) WHERE account_id=$1`, f.account)
	require.NoError(t, err)
	out = read()
	require.Equal(t, "query_failed", out.Availability.Status)
	_, err = integrationDB.Exec(`UPDATE scheduled_test_plans SET pelican_config='{"availability":{"valid_for_seconds":"bad"}}' WHERE account_id=$1`, f.account)
	require.NoError(t, err)
	out = read()
	require.Equal(t, "query_failed", out.Availability.Status)
	require.Contains(t, string(out.Usage), "member_total", "a failed availability section must not roll back other reads")
	require.Equal(t, "ready", out.Errors.Status)
	_, err = integrationDB.Exec(`UPDATE scheduled_test_plans SET pelican_config='{"availability":{"valid_for_seconds":60}}' WHERE account_id=$1`, f.account)
	require.NoError(t, err)
	require.Equal(t, "ready", read().Availability.Status)
	tx, err := integrationDB.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	raw, err := operationsAccounts(t.Context(), tx, filter)
	require.NoError(t, err)
	_, err = integrationDB.Exec(`UPDATE scheduled_test_plans SET pelican_config=pelican_config-'availability' WHERE account_id=$1`, f.account)
	require.NoError(t, err)
	args := []any{filter.From, filter.Until, string(raw), filter.Upstream, filter.AccountID, filter.GroupID, filter.Model, filter.ModelScope, filter.PageSize, 0}
	sameSnapshot := readUpstreamAvailability(t.Context(), tx, filter, args)
	require.Equal(t, "ready", sameSnapshot.Status)
	require.NoError(t, json.Unmarshal(sameSnapshot.Data, &data))
	require.Equal(t, 101, data.PlanTotal, "the availability projection must not open a second, newer transaction")
	require.NoError(t, tx.Commit())
	require.NoError(t, json.Unmarshal(read().Availability.Data, &data))
	require.Empty(t, data.Reports)
}
