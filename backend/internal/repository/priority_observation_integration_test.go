//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestPriorityObservationPersistedRouteIsolation(t *testing.T) {
	for _, measurement := range []string{"legacy_forward", "attributed_v1"} {
		t.Run(measurement, func(t *testing.T) {
			f := newLatencyCacheFixture(t)
			account := f.usage.AccountID
			peer := mustCreateAccount(t, f.tx.Client(), &service.Account{Name: "route-peer", Platform: service.PlatformOpenAI})
			routeA, routeB := strings.Repeat("a", 64), strings.Repeat("b", 64)
			inbound := "/v1/responses"
			f.usage.InboundEndpoint = &inbound
			for _, mode := range []string{"current", "foreign", "missing", "zero", "malformed", "uppercase", "wrong_account", "earlier_only", "parent_last", "ambiguous", "future_version", "peer"} {
				id, route, ttft := account, routeA, 500
				if mode == "peer" {
					id, route, ttft = peer.ID, routeB, 700
				} else if mode != "current" {
					ttft = 25000
				}
				f.usage.AccountID, f.usage.FirstTokenMs, f.usage.ActualCost = id, &ttft, 1
 f.usage.OutputTokens = 20
 duration := 30000
 f.usage.DurationMs = &duration
 f.trace.StartedAt = time.Now().Add(-time.Minute)
				f.trace.Version, f.trace.Status, f.trace.Outcome, f.trace.Terminal, f.trace.BodyComplete = 1, 200, "success", "completed", true
				f.trace.Attempts = []requesttiming.Attempt{{Kind: "http", AccountID: id, RouteFingerprint: route, Status: 200, Terminal: "completed", Events: map[string]float64{"first_semantic": float64(ttft)}}}
				switch mode {
				case "foreign":
					f.trace.Attempts[0].RouteFingerprint = routeB
				case "missing":
					f.trace.Attempts[0].RouteFingerprint = ""
				case "zero":
					f.trace.Attempts[0].RouteFingerprint = strings.Repeat("0", 64)
				case "malformed":
					f.trace.Attempts[0].RouteFingerprint = "invalid-route"
				case "uppercase":
					f.trace.Attempts[0].RouteFingerprint = strings.ToUpper(routeA)
				case "wrong_account":
					f.trace.Attempts[0].AccountID = peer.ID
				case "earlier_only":
					last := f.trace.Attempts[0]
					last.RouteFingerprint = routeB
					f.trace.Attempts = append(f.trace.Attempts, last)
				case "parent_last":
					f.trace.Attempts = append(f.trace.Attempts, requesttiming.Attempt{Kind: "egress", AccountID: id, RouteFingerprint: routeA})
				case "future_version":
					f.trace.Version = 2
				}
				usageID := f.insert(t)
				if mode == "ambiguous" {
					raw, err := json.Marshal(f.trace)
					require.NoError(t, err)
					_, err = f.tx.ExecContext(t.Context(), `INSERT INTO request_timing_details(usage_log_id,trace_id,detail) VALUES($1,$2,$3::jsonb)`, usageID, uuid.NewString(), string(raw))
					require.NoError(t, err)
				}
			}
			_, err := f.tx.ExecContext(t.Context(), `INSERT INTO ops_error_logs(account_id,model,requested_model,inbound_endpoint,error_phase,error_type,severity,status_code,created_at) VALUES($1,'cache-fixture','cache-fixture',$2,'upstream','http_error','error',503,NOW()-INTERVAL '1 minute')`, account, inbound)
			require.NoError(t, err)
			query := service.PrioritySchedulingQuery{AccountIDs: []int64{account, peer.ID}, Model: "cache-fixture", InboundEndpoint: inbound, LatencyMeasurement: measurement, UsageSince: time.Now().Add(-time.Hour), QualitySince: time.Now().Add(-time.Hour),
				AccountRouteFingerprints: map[int64]string{account: routeA, peer.ID: routeB}}
			signals, err := f.repo.ReadPrioritySchedulingSignals(t.Context(), query)
			require.NoError(t, err)
			for id, want := range map[int64]float64{account: 500, peer.ID: 700} {
				require.Equal(t, 1, signals[id].Performance.TTFTSamples, "only the final physical attempt proving this candidate route may contribute")
				require.InDelta(t, want, *signals[id].Performance.AvgTTFTMs, .01)
				require.Greater(t, signals[id].ProfitSamples, 0, "financial history retains real model/group usage even across route changes")
				require.Zero(t, signals[id].ErrorSamples, "ops errors without route proof cannot be attributed to the current route")
			}
			buckets, err := f.repo.ReadPriorityLatencySeries(t.Context(), service.PriorityLatencySeriesQuery{AccountIDs: query.AccountIDs, Model: query.Model, Since: query.UsageSince, InboundEndpoint: inbound, LatencyMeasurement: measurement, AccountRouteFingerprints: query.AccountRouteFingerprints})
			require.NoError(t, err)
			counts := map[int64]int{}
			for _, bucket := range buckets {
				counts[bucket.AccountID] += bucket.Samples
				require.Zero(t, bucket.ErrorSamples)
			}
			require.Equal(t, map[int64]int{account: 1, peer.ID: 1}, counts)
			query.AccountRouteFingerprints = map[int64]string{}
			signals, err = f.repo.ReadPrioritySchedulingSignals(t.Context(), query)
			require.NoError(t, err)
			require.Zero(t, signals[account].Performance.TTFTSamples, "an explicitly scoped account with no route proof stays unknown")
			query.AccountRouteFingerprints = nil
			signals, err = f.repo.ReadPrioritySchedulingSignals(t.Context(), query)
			require.NoError(t, err)
			require.Greater(t, signals[account].Performance.TTFTSamples, 1, "explicit legacy unscoped repository calls retain their original population")
			require.Zero(t, signals[account].ErrorSamples, "legacy error rows are not the shared HTTP response evidence")
		})
	}
}

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
	require.Equal(t, 2, got[account.ID].ProfitSamples)
	require.NotNil(t, got[account.ID].Performance)
	require.Nil(t, got[account.ID].Performance.AvgTTFTMs, "untraced legacy usage is not valid performance evidence")
	require.Equal(t, 1, got[account.ID].Performance.QualitySamples)
	query.QualityModel = "different-route"
	qualityOther, err := committedReader.ReadPrioritySchedulingSignals(ctx, query)
	require.NoError(t, err)
	require.Equal(t, got[account.ID].Performance.QualitySamples, qualityOther[account.ID].Performance.QualitySamples, "account performance pools probe models")
	query.Model = "other-public"
	other, err := committedReader.ReadPrioritySchedulingSignals(ctx, query)
	require.NoError(t, err)
	require.Equal(t, 1, other[account.ID].ProfitSamples)
	query.Model, query.GroupID = "public-composite", nil
	other, err = committedReader.ReadPrioritySchedulingSignals(ctx, query)
	require.NoError(t, err)
	require.Equal(t, 1, other[account.ID].ProfitSamples)
	query.Model = "channel-route"
	other, err = committedReader.ReadPrioritySchedulingSignals(ctx, query)
	require.NoError(t, err)
	require.Zero(t, other[account.ID].ProfitSamples, "financial history retains public model identity")
}
