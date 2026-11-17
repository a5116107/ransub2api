//go:build integration

package repository

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func qualityRouteFixture(t *testing.T, managed bool) (*service.ScheduledTestPlan, *service.ScheduledTestResult) {
	t.Helper()
	ctx := t.Context()
	var accountID int64
	extra := `{}`
	if managed {
		extra = `{"openai_connections_managed":true}`
	}
	require.NoError(t, integrationDB.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,status,credentials,extra)
 VALUES('route-fixture','openai','apikey','active','{"api_key":"private-route-fixture","base_url":"https://route.invalid"}',$1) RETURNING id`, extra).Scan(&accountID))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM accounts WHERE id=$1`, accountID)
	})
	cfg := &service.PelicanTestConfig{QuestionKind: "candy", ModelID: "fixture-model", Prompt: "42", ParallelCount: 1,
		Quality: &service.QualityPolicy{Action: service.QualityActionObserveOnly, ExpectedAnswer: "42"}}
	plan, err := NewScheduledTestPlanRepository(integrationDB).Create(ctx, &service.ScheduledTestPlan{AccountID: accountID, ModelID: cfg.ModelID,
		Enabled: true, CronExpression: "*/5 * * * *", MaxResults: 20, PelicanConfig: cfg})
	require.NoError(t, err)
	observed := time.Now().UTC().Add(-5 * time.Second).Truncate(time.Microsecond)
	scope := &service.AvailabilityRequestScope{Version: 2, Model: cfg.ModelID, Protocol: "responses_http", Capability: "text"}
	cfg.AvailabilityResult = &service.AvailabilityProbeResult{Version: 1, Status: "pass", StartedAt: observed.Add(-time.Second), FinishedAt: observed, Scope: scope}
	cfg.QualityMeasurement = &service.QualityMeasurement{Version: 1, MeasurementHash: strings.Repeat("a", 64), PolicyHash: strings.Repeat("b", 64),
		RuleVersion: plan.UpdatedAt, RequestedModel: cfg.ModelID, QuestionKind: cfg.QuestionKind, ObservedAt: observed, CompletedAt: observed.Add(time.Second), Scope: scope}
	tx, err := integrationDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	fingerprint, err := accountRouteFingerprint(ctx, tx, accountID, false)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	evidence := &service.QualityRouteEvidence{Version: 1, AccountID: accountID, Fingerprint: hex.EncodeToString(fingerprint[:]),
		MeasurementHash: cfg.QualityMeasurement.MeasurementHash, ObservedAt: observed}
	if managed {
		evidence.ConnectionID, evidence.ConnectionRevision = uuid.NewString(), 7
		_, err = integrationDB.ExecContext(ctx, `INSERT INTO openai_auth_connection_policies(account_id) VALUES($1)`, accountID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, `INSERT INTO openai_auth_connections(id,account_id,label,kind,status,revision,capabilities)
 VALUES($1,$2,'route-fixture','api_key','active',7,'["responses"]')`, evidence.ConnectionID, accountID)
		require.NoError(t, err)
	}
	result := &service.ScheduledTestResult{PlanID: plan.ID, Status: "failed", ErrorMessage: "answer_mismatch", ResponseText: "41",
		StartedAt: cfg.AvailabilityResult.StartedAt, FinishedAt: cfg.QualityMeasurement.CompletedAt, PelicanConfig: cfg, QualityRouteEvidence: evidence}
	require.True(t, evidence.MatchesResult(result))
	return plan, result
}

func TestQualityMeasurementRouteCurrentness(t *testing.T) {
	for _, scenario := range []string{"matching", "key", "address", "mapping", "unrelated", "deleted-account", "managed", "managed-chat", "chat-wrong-capability", "revision", "deleted-connection", "wrong-owner", "disabled", "refresh", "capability", "cooldown", "expired-cooldown", "managed-flag", "legacy", "malformed", "future-version", "wrong-account", "wrong-sample", "wrong-time", "wrong-scope", "future-observation"} {
		t.Run(scenario, func(t *testing.T) {
			managed := scenario == "managed" || scenario == "managed-chat" || scenario == "chat-wrong-capability" || scenario == "revision" || scenario == "deleted-connection" || scenario == "wrong-owner" || scenario == "disabled" || scenario == "refresh" || scenario == "capability" || scenario == "cooldown" || scenario == "expired-cooldown"
			plan, sample := qualityRouteFixture(t, managed)
			if scenario == "managed-chat" || scenario == "chat-wrong-capability" {
				sample.PelicanConfig.QualityMeasurement.Scope.Protocol = "chat_http"
				if scenario == "managed-chat" {
					_, e := integrationDB.ExecContext(t.Context(), `UPDATE openai_auth_connections SET capabilities='["chat_completions"]' WHERE account_id=$1`, plan.AccountID)
					require.NoError(t, e)
				}
			}
			results := NewScheduledTestResultRepository(integrationDB)
			created, err := results.Create(t.Context(), sample)
			require.NoError(t, err)
			require.Nil(t, created.QualityRouteEvidence, "private evidence never returns through normal API structs")
			exec := func(query string, args ...any) {
				_, e := integrationDB.ExecContext(t.Context(), query, args...)
				require.NoError(t, e)
			}
			want, reason := "matching", "route_matches"
			switch scenario {
			case "managed-chat":
			case "chat-wrong-capability":
				want, reason = "unavailable", "connection_unavailable"
			case "key", "address", "mapping":
				field, value := "api_key", `"changed-fixture"`
				if scenario == "address" {
					field, value = "base_url", `"https://different.invalid"`
				}
				if scenario == "mapping" {
					field, value = "model_mapping", `{"fixture-model":"different-model"}`
				}
				exec(`UPDATE accounts SET credentials=jsonb_set(credentials,ARRAY[$2]::text[],$3::jsonb) WHERE id=$1`, plan.AccountID, field, value)
				want, reason = "changed", "route_changed"
			case "unrelated":
				exec(`UPDATE accounts SET name='renamed',concurrency=8,updated_at=clock_timestamp() WHERE id=$1`, plan.AccountID)
			case "deleted-account":
				exec(`UPDATE accounts SET deleted_at=clock_timestamp() WHERE id=$1`, plan.AccountID)
				want, reason = "unavailable", "account_unavailable"
			case "revision", "deleted-connection", "wrong-owner":
				if scenario == "revision" {
					exec(`UPDATE openai_auth_connections SET revision=8 WHERE account_id=$1`, plan.AccountID)
				}
				if scenario == "deleted-connection" {
					exec(`DELETE FROM openai_auth_connections WHERE account_id=$1`, plan.AccountID)
				}
				if scenario == "wrong-owner" {
					other, _ := qualityRouteFixture(t, false)
					exec(`UPDATE openai_auth_connections SET account_id=$2 WHERE account_id=$1`, plan.AccountID, other.AccountID)
				}
				want, reason = "changed", "connection_changed"
			case "disabled", "refresh", "capability", "cooldown":
				query := map[string]string{"disabled": `status='disabled'`, "refresh": `refresh_state='in_progress'`, "capability": `capabilities='[]'`, "cooldown": `cooldown_until=clock_timestamp()+interval '1 minute'`}[scenario]
				exec(`UPDATE openai_auth_connections SET `+query+` WHERE account_id=$1`, plan.AccountID)
				want, reason = "unavailable", "connection_unavailable"
			case "expired-cooldown":
				exec(`UPDATE openai_auth_connections SET cooldown_until=clock_timestamp()-interval '1 second' WHERE account_id=$1`, plan.AccountID)
			case "managed-flag":
				exec(`INSERT INTO openai_auth_connection_policies(account_id) VALUES($1)`, plan.AccountID)
				exec(`INSERT INTO openai_auth_connections(id,account_id,label,kind,status) VALUES($1,$2,'route-fixture','api_key','active')`, uuid.NewString(), plan.AccountID)
				want, reason = "changed", "connection_changed"
			case "legacy":
				exec(`UPDATE scheduled_test_results SET pelican_config=pelican_config-'_quality_observed_route' WHERE id=$1`, created.ID)
				want, reason = "unknown", "route_not_observed"
			default:
				if scenario != "matching" && scenario != "managed" {
					evidence := *sample.QualityRouteEvidence
					switch scenario {
					case "future-version":
						evidence.Version = 99
					case "wrong-account":
						evidence.AccountID++
					case "wrong-sample":
						evidence.MeasurementHash = strings.Repeat("c", 64)
					case "wrong-time":
						evidence.ObservedAt = evidence.ObservedAt.Add(time.Second)
					case "wrong-scope":
						exec(`UPDATE scheduled_test_results SET pelican_config=jsonb_set(pelican_config,'{quality_measurement,scope,model}','"other-model"') WHERE id=$1`, created.ID)
					case "future-observation":
						future := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
						cfg := sample.PelicanConfig
						cfg.QualityMeasurement.ObservedAt, cfg.QualityMeasurement.CompletedAt = future, future.Add(time.Second)
						cfg.AvailabilityResult.StartedAt, cfg.AvailabilityResult.FinishedAt = future.Add(-time.Second), future
						evidence.ObservedAt = future
						exec(`UPDATE scheduled_test_results SET started_at=$2,finished_at=$3,pelican_config=$4 WHERE id=$1`, created.ID, cfg.AvailabilityResult.StartedAt, cfg.QualityMeasurement.CompletedAt, marshalPelicanConfig(cfg))
					}
					raw, e := json.Marshal(evidence)
					require.NoError(t, e)
					if scenario == "malformed" {
						raw = []byte(`"invalid"`)
					}
					exec(`UPDATE scheduled_test_results SET pelican_config=jsonb_set(pelican_config,'{_quality_observed_route}',$2::jsonb) WHERE id=$1`, created.ID, string(raw))
					want, reason = "unknown", "invalid_route_evidence"
				}
			}
			read, err := NewScheduledTestResultRepository(integrationDB).GetResult(t.Context(), plan.ID, created.ID)
			require.NoError(t, err)
			require.NotNil(t, read.QualityMeasurementValidity.Route)
			require.Equal(t, &service.QualityMeasurementRouteValidity{Status: want, Reason: reason}, read.QualityMeasurementValidity.Route)
			require.Equal(t, "failed", read.Status)
			require.Equal(t, "41", read.ResponseText)
			raw, err := json.Marshal(read)
			require.NoError(t, err)
			for _, private := range []string{"_quality_observed_route", "private-route-fixture", "connection_revision", "fingerprint", sample.QualityRouteEvidence.ConnectionID} {
				if private != "" {
					require.NotContains(t, string(raw), private)
				}
			}
			var stored []byte
			require.NoError(t, integrationDB.QueryRowContext(t.Context(), `SELECT pelican_config FROM scheduled_test_results WHERE id=$1`, created.ID).Scan(&stored))
			require.NotContains(t, string(stored), "quality_measurement_validity")
		})
	}
}

func TestQualityMeasurementRouteSnapshotAndForgedInput(t *testing.T) {
	plan, sample := qualityRouteFixture(t, false)
	raw, err := json.Marshal(sample.QualityRouteEvidence)
	require.NoError(t, err)
	tx, err := integrationDB.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	var value int
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT count(*) FROM accounts WHERE id=$1`, plan.AccountID).Scan(&value))
	_, err = integrationDB.ExecContext(t.Context(), `UPDATE accounts SET credentials='{"api_key":"changed"}' WHERE id=$1`, plan.AccountID)
	require.NoError(t, err)
	read, err := readQualityRoute(t.Context(), tx, plan.AccountID, sample, raw)
	require.NoError(t, err)
	require.Equal(t, "matching", read.assess(time.Now()).Status, "same snapshot cannot mix a later account with an earlier result")
	require.NoError(t, tx.Commit())
	created, err := NewScheduledTestResultRepository(integrationDB).Create(t.Context(), sample)
	require.NoError(t, err)
	detail, err := NewScheduledTestResultRepository(integrationDB).GetResult(t.Context(), plan.ID, created.ID)
	require.NoError(t, err)
	require.Equal(t, "changed", detail.QualityMeasurementValidity.Route.Status)
	var input service.ScheduledTestResult
	require.NoError(t, json.Unmarshal([]byte(`{"QualityRouteEvidence":{"version":1},"quality_route_evidence":{"version":1},"pelican_config":{"_quality_observed_route":{"version":1}}}`), &input))
	require.Nil(t, input.QualityRouteEvidence)
	public, err := json.Marshal(input)
	require.NoError(t, err)
	require.NotContains(t, string(public), "_quality_observed_route")
}

func TestQualityMeasurementRouteDetailSnapshotAndCancellation(t *testing.T) {
	for _, scenario := range []string{"snapshot", "fault-snapshot", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			plan, sample := qualityRouteFixture(t, false)
			plan, sample = claimQualityFaultSample(t, plan, sample)
			created, err := NewScheduledTestResultRepository(integrationDB).Create(t.Context(), sample)
			require.NoError(t, err)
			db, err := sql.Open("postgres", integrationPostgresDSN)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			db.SetMaxOpenConns(1)
			db.SetMaxIdleConns(1)
			var readerPID int
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&readerPID))
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			gate, err := integrationDB.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			defer func() { _ = gate.Rollback() }()
			var gatePID int
			require.NoError(t, gate.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&gatePID))
			_, err = gate.ExecContext(ctx, `LOCK TABLE accounts IN ACCESS EXCLUSIVE MODE`)
			require.NoError(t, err)
			type outcome struct {
				result *service.ScheduledTestResult
				err    error
			}
			finished := make(chan outcome, 1)
			repo := NewScheduledTestResultRepository(db)
			go func() {
				result, readErr := repo.GetResult(ctx, plan.ID, created.ID)
				finished <- outcome{result, readErr}
			}()
			require.Eventually(t, func() bool {
				var waiting bool
				readErr := integrationDB.QueryRowContext(ctx, `SELECT EXISTS(
 SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND state='active' AND wait_event_type='Lock'
 AND query LIKE '%FROM accounts WHERE id=%' AND $2=ANY(pg_blocking_pids(pid)))`, readerPID, gatePID).Scan(&waiting)
				return readErr == nil && waiting
			}, 5*time.Second, 10*time.Millisecond, "the real detail read must reach the account query after freezing its result snapshot")
			var changedAt time.Time
			if scenario == "cancel" {
				cancel()
			} else if scenario == "fault-snapshot" {
				require.NoError(t, gate.QueryRowContext(ctx, `UPDATE scheduled_test_plans
 SET availability_fault_version=availability_fault_version+1 WHERE id=$1 RETURNING clock_timestamp()`, plan.ID).Scan(&changedAt))
				require.NoError(t, gate.Commit())
			} else {
				require.NoError(t, gate.QueryRowContext(ctx, `UPDATE accounts SET credentials='{"api_key":"changed-during-read"}'
 WHERE id=$1 RETURNING clock_timestamp()`, plan.AccountID).Scan(&changedAt))
				require.NoError(t, gate.Commit())
			}
			var read outcome
			select {
			case read = <-finished:
			case <-time.After(5 * time.Second):
				t.Fatal("detail read did not finish after releasing or cancelling its database wait")
			}
			if scenario == "cancel" {
				require.Error(t, read.err)
				require.Nil(t, read.result, "a failed route lookup must not return a partially matching result")
				require.ErrorIs(t, ctx.Err(), context.Canceled)
				require.NoError(t, gate.Rollback())
			} else {
				require.NoError(t, read.err)
				require.Equal(t, "matching", read.result.QualityMeasurementValidity.Route.Status, "all detail reads share the original snapshot")
				require.Equal(t, "unchanged", read.result.QualityMeasurementValidity.Fault.Status, "the fault version belongs to the same frozen detail snapshot")
				require.False(t, read.result.QualityMeasurementValidity.EvaluatedAt.Before(changedAt), "evaluate the clock after the blocked route read, never before it")
			}
			recovered, err := repo.GetResult(t.Context(), plan.ID, created.ID)
			require.NoError(t, err, "the same pool must recover without a leaked transaction")
			want := "changed"
			if scenario == "cancel" || scenario == "fault-snapshot" {
				want = "matching"
			}
			require.Equal(t, want, recovered.QualityMeasurementValidity.Route.Status)
			wantFault := "unchanged"
			if scenario == "fault-snapshot" {
				wantFault = "invalidated"
			} else if scenario == "snapshot" {
				wantFault = "unknown"
			}
			require.Equal(t, wantFault, recovered.QualityMeasurementValidity.Fault.Status)
		})
	}
}
