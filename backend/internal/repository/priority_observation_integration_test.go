//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestPriorityObservationPersistedEndpointIsolation(t *testing.T) {
	ctx := t.Context()
	tx := testEntTx(t)
	client := tx.Client()
	repo := newUsageLogRepositoryWithSQL(client, tx)
	user := mustCreateUser(t, client, &service.User{Email: "endpoint-observation@test.invalid"})
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "synthetic-endpoint-observation", Name: "fixture"})
	account := mustCreateAccount(t, client, &service.Account{Name: "endpoint-observation", Platform: service.PlatformOpenAI})
	query := service.PrioritySchedulingQuery{AccountIDs: []int64{account.ID}, Model: "public-model", UsageSince: time.Now().Add(-time.Hour), QualitySince: time.Now().Add(-time.Hour)}
	endpoints := []string{"/v1/responses", "/v1/chat/completions", "/v1/responses/compact", "/v1/messages", "/unknown", "", "missing"}
	for i, endpoint := range endpoints {
		ttft := 1000 * (i + 1)
		mediumTTFT := 90000
		high, medium := "xhigh", "medium"
		var inbound *string
		if endpoint != "missing" {
			inbound = &endpoint
		}
		for n, sample := range []struct {
			ttft   *int
			effort *string
		}{{&ttft, &high}, {nil, &high}, {&mediumTTFT, &medium}} {
			usage := &service.UsageLog{UserID: user.ID, APIKeyID: key.ID, AccountID: account.ID, RequestID: fmt.Sprintf("endpoint-%d-%d", i, n), Model: "public-model", InboundEndpoint: inbound, ReasoningEffort: sample.effort, Stream: true, FirstTokenMs: sample.ttft, InputTokens: 10, ActualCost: 1, CreatedAt: time.Now().Add(-time.Minute)}
			_, err := repo.Create(ctx, usage)
			require.NoError(t, err)
			if sample.ttft != nil {
				detail, err := json.Marshal(map[string]any{
					"version": 1, "status": 200, "outcome": "success", "terminal": "completed", "truncated": false, "body_complete": true, "canceled": false, "client_disconnect": false, "downstream_error": false,
					"attempts": []any{map[string]any{"kind": "http", "account_id": account.ID, "status": 200, "terminal": "completed", "start_ms": 100, "events": map[string]int{"first_semantic": 100 + *sample.ttft}}},
				})
				require.NoError(t, err)
				_, err = tx.ExecContext(ctx, `INSERT INTO request_timing_details(usage_log_id,trace_id,detail) VALUES($1,$2,$3::jsonb)`, usage.ID, uuid.NewString(), string(detail))
				require.NoError(t, err)
			}
		}
		for n := range i + 1 {
			_, err := tx.ExecContext(ctx, `INSERT INTO ops_error_logs(account_id,request_id,model,requested_model,request_path,inbound_endpoint,error_phase,error_type,severity,status_code,created_at)
			 VALUES($1,$2,'public-model','public-model','/v1/responses',$3,'upstream','http_error','error',503,NOW()-INTERVAL '1 minute')`, account.ID, fmt.Sprintf("endpoint-error-%d-%d", i, n), inbound)
			require.NoError(t, err)
		}
	}
	for i, endpoint := range endpoints[:5] {
		t.Run(endpoint, func(t *testing.T) {
			for _, measurement := range []string{"legacy_forward", "attributed_v1"} {
				for _, effort := range []string{"all", "xhigh", "medium"} {
					t.Run(measurement+"/"+effort, func(t *testing.T) {
						query.InboundEndpoint, query.LatencyMeasurement, query.ObservationEffort = endpoint, measurement, nil
						samples, unknown, profit := 2, 1, 3
						want := 81000.0 + 100.0*float64(i+1)
						if effort != "all" {
							query.ObservationEffort = &effort
							samples, profit, want = 1, 2, 1000.0*float64(i+1)
							if effort == "medium" {
								unknown, profit, want = 0, 1, 90000
							}
						}
						signals, err := repo.ReadPrioritySchedulingSignals(ctx, query)
						require.NoError(t, err)
						signal := signals[account.ID]
						require.Equal(t, samples, signal.Samples, "missing and other ingress records must not become current-endpoint samples")
						require.Equal(t, unknown, signal.LatencyUnknownSamples)
						require.Equal(t, profit, signal.ProfitSamples)
						require.InDelta(t, want, signal.P90TTFTMs, .01)
						require.Equal(t, i+1, signal.ErrorSamples, "raw request_path is not a substitute for the recorded canonical ingress")
						require.InDelta(t, float64(i+1)/float64(i+1+samples), signal.UpstreamErrorRate, .00001)
						seriesQuery := service.PriorityLatencySeriesQuery{AccountIDs: query.AccountIDs, Model: query.Model, Since: query.UsageSince, LatencyMeasurement: measurement, InboundEndpoint: endpoint, ObservationEffort: query.ObservationEffort}
						buckets, err := repo.ReadPriorityLatencySeries(ctx, seriesQuery)
						require.NoError(t, err)
						var bucketSamples, bucketErrors int
						for _, bucket := range buckets {
							bucketSamples += bucket.Samples
							bucketErrors += bucket.ErrorSamples
						}
						require.Equal(t, samples, bucketSamples, "the trend must use the same endpoint and effort as the scored summary")
						require.Equal(t, i+1, bucketErrors)
					})
				}
			}
		})
	}
	query.InboundEndpoint, query.LatencyMeasurement, query.ObservationEffort = "", "legacy_forward", nil
	signals, err := repo.ReadPrioritySchedulingSignals(ctx, query)
	require.NoError(t, err)
	require.Equal(t, 14, signals[account.ID].Samples, "explicit unscoped repository reads retain their legacy aggregate")
	require.Equal(t, 7, signals[account.ID].LatencyUnknownSamples)
	require.Equal(t, 28, signals[account.ID].ErrorSamples)
	query.InboundEndpoint = "/not-observed"
	signals, err = repo.ReadPrioritySchedulingSignals(ctx, query)
	require.NoError(t, err)
	require.Zero(t, signals[account.ID].Samples)
	require.Zero(t, signals[account.ID].ErrorSamples)
	require.Zero(t, signals[account.ID].ProfitSamples)
}

func TestPriorityObservationPersistedPublicAliasAndGroup(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	repo := newUsageLogRepositoryWithSQL(client, tx)
	user := mustCreateUser(t, client, &service.User{Email: "observation@test.invalid"})
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "sk-observation-synthetic", Name: "test"})
	account := mustCreateAccount(t, client, &service.Account{Name: "observation", Platform: service.PlatformOpenAI})
	group := mustCreateGroup(t, client, &service.Group{Name: "observation", Platform: service.PlatformOpenAI})
	query := service.PrioritySchedulingQuery{AccountIDs: []int64{account.ID}, Model: "public-composite", QualityModel: "channel-route", GroupID: &group.ID, UsageSince: time.Now().Add(-time.Hour), QualitySince: time.Now().Add(-time.Hour)}
	committedReader := newUsageLogRepositoryWithSQL(integrationEntClient, integrationDB)
	before, err := committedReader.ReadPrioritySchedulingSignals(ctx, query)
	require.NoError(t, err)
	require.Zero(t, before[account.ID].Samples)
	ttft := 1234
	upstream := "account-upstream"
	add := func(id, public string, groupID *int64, first *int) {
		t.Helper()
		_, err := repo.Create(ctx, &service.UsageLog{UserID: user.ID, APIKeyID: key.ID, AccountID: account.ID, RequestID: id, Model: "channel-route", UpstreamModel: &upstream, RequestedModel: public, GroupID: groupID, Stream: true, FirstTokenMs: first, InputTokens: 10, ActualCost: 1, TotalCost: 2, CreatedAt: time.Now()})
		require.NoError(t, err)
	}
	add("observation-completed", "public-composite", &group.ID, &ttft)
	add("observation-unknown", "public-composite", &group.ID, nil)
	add("observation-other-model", "other-public", &group.ID, &ttft)
	add("observation-other-group", "public-composite", nil, &ttft)
	// Quality remains the route/account probe population, independently of the
	// public usage alias. This must exercise the SQL QualityModel parameter.
	rows, err := tx.QueryContext(ctx, `INSERT INTO scheduled_test_plans(account_id,model_id,cron_expression,enabled,pelican_config,updated_at) VALUES($1,'channel-route','*/30 * * * *',true,'{"quality":{},"parallel_count":1}',NOW()-INTERVAL '1 hour') RETURNING id`, account.ID)
	require.NoError(t, err)
	var plan int64
	require.True(t, rows.Next())
	require.NoError(t, rows.Scan(&plan))
	require.NoError(t, rows.Close())
	_, err = tx.ExecContext(ctx, `INSERT INTO scheduled_test_results(plan_id,status,started_at,finished_at,created_at,pelican_config,quality_round_id,quality_action,quality_judgment) VALUES($1,'success',NOW(),NOW(),NOW(),'{"model_id":"channel-route","parallel_count":1}','observation-round','passed','{"verdict":"correct"}')`, plan)
	require.NoError(t, err)
	// A separate SQL connection must see no uncommitted usage, then see the
	// completed canonical inserts after commit. No in-flight timing injection.
	uncommitted, err := committedReader.ReadPrioritySchedulingSignals(ctx, query)
	require.NoError(t, err)
	require.Zero(t, uncommitted[account.ID].Samples)
	require.NoError(t, tx.Commit())
	t.Cleanup(func() {
		cleanup := []struct {
			sql string
			id  int64
		}{
			{`DELETE FROM scheduled_test_results WHERE plan_id=$1`, plan},
			{`DELETE FROM scheduled_test_plans WHERE id=$1`, plan},
			{`DELETE FROM usage_logs WHERE account_id=$1`, account.ID},
			{`DELETE FROM accounts WHERE id=$1`, account.ID},
			{`DELETE FROM api_keys WHERE id=$1`, key.ID},
			{`DELETE FROM groups WHERE id=$1`, group.ID},
			{`DELETE FROM users WHERE id=$1`, user.ID},
		}
		for _, q := range cleanup {
			_, err := integrationDB.ExecContext(ctx, q.sql, q.id)
			require.NoError(t, err)
		}
	})
	got, err := committedReader.ReadPrioritySchedulingSignals(ctx, query)
	require.NoError(t, err)
	require.Equal(t, 1, got[account.ID].Samples)
	require.Equal(t, 1, got[account.ID].LatencyUnknownSamples, "NULL TTFT remains unknown")
	require.Equal(t, 2, got[account.ID].ProfitSamples)
	require.Equal(t, 1, got[account.ID].QualitySamples)
	require.Equal(t, 1, got[account.ID].QualityPassed)
	require.InDelta(t, 1234, got[account.ID].P90TTFTMs, .01)
	query.QualityModel = "different-route"
	qualityOther, err := committedReader.ReadPrioritySchedulingSignals(ctx, query)
	require.NoError(t, err)
	require.Zero(t, qualityOther[account.ID].QualitySamples)
	require.Equal(t, 1, qualityOther[account.ID].Samples)
	query.QualityModel = "channel-route"
	query.Model = "other-public"
	other, err := committedReader.ReadPrioritySchedulingSignals(ctx, query)
	require.NoError(t, err)
	require.Equal(t, 1, other[account.ID].Samples)
	require.Zero(t, other[account.ID].LatencyUnknownSamples)
	query.Model = "public-composite"
	query.GroupID = nil
	other, err = committedReader.ReadPrioritySchedulingSignals(ctx, query)
	require.NoError(t, err)
	require.Equal(t, 1, other[account.ID].Samples)
	query.Model = "channel-route"
	other, err = committedReader.ReadPrioritySchedulingSignals(ctx, query)
	require.NoError(t, err)
	require.Zero(t, other[account.ID].Samples, "routing identity cannot borrow a different public model")
}
