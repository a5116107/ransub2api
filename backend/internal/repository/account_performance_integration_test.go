//go:build integration

package repository

import (
	"context"

	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestAccountPerformancePostgresSchedulingScopeRejectsUnprovenRoutes(t *testing.T) {
	ctx := t.Context()
	tx := testEntTx(t)
	client := tx.Client()
	id := uuid.NewString()
	user := mustCreateUser(t, client, &service.User{Email: id + "@test.invalid"})
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: id, Name: "scope"})
	account := mustCreateAccount(t, client, &service.Account{Name: id, Platform: service.PlatformOpenAI})
	group := mustCreateGroup(t, client, &service.Group{Name: id, Platform: service.PlatformOpenAI})
	usage := newUsageLogRepositoryWithSQL(client, tx)
	now := time.Now().UTC().Truncate(time.Second)
	effort := "high"
	for _, kind := range []string{"match", "wrong-model", "wrong-group", "wrong-endpoint", "wrong-effort"} {
		duration, ttft := 2000, 500
		model, endpoint, observedEffort := "scope-model", "/v1/responses", effort
		groupID := &group.ID
		switch kind {
		case "wrong-model":
			model = "other-model"
		case "wrong-group":
			groupID = nil
		case "wrong-endpoint":
			endpoint = "/v1/chat/completions"
		case "wrong-effort":
			observedEffort = "low"
		}
		if kind != "match" {
			ttft = 1900
		}
		row := &service.UsageLog{UserID: user.ID, APIKeyID: key.ID, AccountID: account.ID,
			RequestID: id + kind, Model: model, GroupID: groupID, InboundEndpoint: &endpoint, ReasoningEffort: &observedEffort,
			InputTokens: 10, CacheReadTokens: 90, OutputTokens: 100, DurationMs: &duration, FirstTokenMs: &ttft, Stream: true, CreatedAt: now.Add(-time.Second)}
		inserted, err := usage.Create(ctx, row)
		require.NoError(t, err)
		require.True(t, inserted)
	}
	var retired bool
	rows, err := tx.QueryContext(ctx, `SELECT to_regclass('public.request_timing_details') IS NULL AND to_regclass('public.request_connection_traces') IS NULL`)
	require.NoError(t, err)
	require.True(t, rows.Next())
	require.NoError(t, rows.Scan(&retired))
	require.NoError(t, rows.Close())
	require.True(t, retired, "execute against the actual post-retirement schema")
	global, err := readAccountPerformance(ctx, tx, []int64{account.ID}, now, nil)
	require.NoError(t, err)
	require.Equal(t, 5, global[account.ID].TTFTSamples, "official account metrics use recorded usage without diagnostic traces")
	require.InDelta(t, 1620, *global[account.ID].AvgTTFTMs, 0.001)
	query := service.PrioritySchedulingQuery{AccountIDs: []int64{account.ID}, Model: "scope-model", GroupID: &group.ID,
		InboundEndpoint: "/v1/responses", ObservationEffort: &effort, UsageUntil: now, UsageSince: now.Add(-time.Hour)}
	for _, routes := range []map[int64]string{{account.ID: strings.Repeat("a", 64)}, {}, {account.ID: ""}, {account.ID: strings.Repeat("0", 64)}} {
		query.AccountRouteFingerprints = routes
		got, err := usage.ReadPrioritySchedulingSignals(ctx, query)
		require.NoError(t, err, "route-specific reads must not reference retired traces or borrow account-wide history")
		metrics := got[account.ID].Performance
		require.NotNil(t, metrics)
		require.Nil(t, metrics.AvgTTFTMs)
		require.Zero(t, metrics.TTFTSamples)
		require.Nil(t, metrics.AvgTPS)
		require.Zero(t, metrics.TPSSamples)
		require.Nil(t, metrics.AvgCachePercent)
		require.Zero(t, metrics.CacheSamples)
		require.Nil(t, metrics.UpstreamError.Percent)
		require.Nil(t, metrics.DegradationPercent)
	}
	query.AccountRouteFingerprints = nil
	scoped, err := usage.ReadPrioritySchedulingSignals(ctx, query)
	require.NoError(t, err)
	require.Equal(t, 1, scoped[account.ID].Performance.TTFTSamples, "explicit unscoped-route reads still isolate model/group/endpoint/effort")
	require.Equal(t, 500.0, *scoped[account.ID].Performance.AvgTTFTMs)
}

// Uses only the harness's migrated disposable PostgreSQL container. testEntTx
// rolls back accounts, usage and probes together, even on assertion failure.
func TestAccountPerformanceHasNoVerifiedTimingSamples(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	account := mustCreateAccount(t, tx.Client(), &service.Account{Name: "retired-traces", Platform: service.PlatformOpenAI})
	metrics, err := readAccountPerformance(ctx, tx, []int64{account.ID}, time.Now(), nil)
	require.NoError(t, err)
	require.Nil(t, metrics[account.ID].AvgCachePercent)
}

func TestAccountPerformancePostgresCompletedQualityProbeRounds(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	suffix := uuid.NewString()
	account := mustCreateAccount(t, tx.Client(), &service.Account{Name: "performance-quality-" + suffix, Platform: service.PlatformOpenAI})
	now := time.Now().UTC().Truncate(time.Second)
	const config = `{"quality":{},"parallel_count":2,"model_ids":["probe-a","probe-b"]}`
	rows, err := tx.QueryContext(ctx, `INSERT INTO scheduled_test_plans(account_id,model_id,cron_expression,enabled,pelican_config,updated_at) VALUES($1,'probe-a','*/30 * * * *',true,$2::jsonb,$3) RETURNING id`, account.ID, config, now.Add(-2*time.Hour))
	require.NoError(t, err)
	require.True(t, rows.Next())
	var plan int64
	require.NoError(t, rows.Scan(&plan))
	require.NoError(t, rows.Close())

	probe := func(round, status, verdict, message, action string, at time.Time, cfg string) {
		t.Helper()
		_, err := tx.ExecContext(ctx, `INSERT INTO scheduled_test_results(plan_id,status,started_at,finished_at,created_at,pelican_config,quality_round_id,quality_action,quality_judgment,error_message) VALUES($1,$2,$3,$4,$5,$6::jsonb,$7,$8,jsonb_build_object('verdict',$9::text),$10)`, plan, status, at, at.Add(time.Second), now.Add(-time.Second), cfg, round, action, verdict, message)
		require.NoError(t, err)
	}
	// Two models × two samples. Partial saves must never count as full rounds.
	for _, round := range []string{"passed", "failed", "transport", "partial", "past15", "old", "future", "stale", "malformed"} {
		count := 4
		if round == "partial" {
			count = 3
		}
		for i := 0; i < count; i++ {
			status, verdict, message, action := "success", "correct", "", "passed"
			at, cfg := now.Add(-time.Minute), config
			if (round == "failed" || round == "partial") && i == 0 {
				status, verdict, message, action = "failed", "incorrect", "state_degraded", "failure_counted:1/2"
			}
			if round == "transport" && i == 0 {
				status, verdict, message, action = "failed", "unknown", "timeout", "inconclusive"
			}
			if round == "past15" {
				at = now.Add(-16 * time.Minute)
			}
			if round == "old" {
				at = now.Add(-61 * time.Minute)
			}
			if round == "future" {
				at = now.Add(time.Second)
			}
			if round == "stale" && i == 0 {
				action = "stale_run"
			}
			if round == "malformed" {
				cfg = `{"parallel_count":"invalid","model_ids":{}}`
			}
			probe(fmt.Sprintf("%s-%s", suffix, round), status, verdict, message, action, at, cfg)
		}
	}
	metrics, err := readAccountPerformance(ctx, tx, []int64{account.ID}, now, nil)
	require.NoError(t, err)
	got := metrics[account.ID]
	require.Equal(t, 3, got.QualitySamples, "60-minute default includes the complete conclusive round older than 15 minutes")
	require.NotNil(t, got.DegradationPercent)
	require.InDelta(t, 100.0/3, *got.DegradationPercent, 0.00001)
	require.Nil(t, got.AvgTPS)
	require.Nil(t, got.AvgTTFTMs)
	_, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES($1,'15') ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`, service.SettingKeyAccountPerformanceWindowMinutes)
	require.NoError(t, err)
	metrics, err = readAccountPerformance(ctx, tx, []int64{account.ID}, now, nil)
	require.NoError(t, err)
	require.Equal(t, 2, metrics[account.ID].QualitySamples)
	require.InDelta(t, 50, *metrics[account.ID].DegradationPercent, 0.00001)
	_, err = tx.ExecContext(ctx, `UPDATE scheduled_test_plans SET enabled=false WHERE id=$1`, plan)
	require.NoError(t, err)
	metrics, err = readAccountPerformance(ctx, tx, []int64{account.ID}, now, nil)
	require.NoError(t, err)
	require.Nil(t, metrics[account.ID].DegradationPercent)
	require.Zero(t, metrics[account.ID].QualitySamples)
}
