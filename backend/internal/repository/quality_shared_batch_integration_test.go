//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Disposable migrated PostgreSQL only. No real model calls or production data.
func TestQualitySharedBatchPostgresFinalizationAndRestart(t *testing.T) {
	for _, action := range []string{"disable_scheduling", service.QualityActionRemoveModel, service.QualityActionIsolateModels} {
		scenarios := []string{"all-wrong", "passing-peer-finishes-last", "peer-paused", "target-edited", "mapping-edited", "late-peer", "different-grader-rules", "missing-grader-rules", "different-grader-model", "different-grader-kind", "different-grader-group", "both-missing-grader-rules", "rotated-grader-account", "different-grader-explanation", "different-grader-latency"}
		if action != "disable_scheduling" {
			scenarios = append(scenarios, "collection-lost-fingerprint", "collection-replaced-fingerprint", "legacy-missing-fingerprint", "key-edited", "unrelated-edited", "recovery-after-restart")
		}
		for _, scenario := range scenarios {
			t.Run(action+"/"+scenario, func(t *testing.T) {
				isolates := scenario == "passing-peer-finishes-last" || scenario == "rotated-grader-account" || scenario == "different-grader-explanation" || scenario == "different-grader-latency" || scenario == "collection-lost-fingerprint" || scenario == "unrelated-edited" || scenario == "recovery-after-restart"
				ctx := context.Background()
				now := time.Now().UTC().Truncate(time.Microsecond)
				start := now.Add(-time.Minute)
				batch := &service.QualitySharedBatch{ID: "synthetic-" + uuid.NewString(), StartedAt: start,
					StartDeadline: now.Add(time.Minute), ClosesAt: now.Add(11 * time.Minute),
					Questions: []service.QualityBatchQuestion{{Prompt: "Synthetic question", Expected: "42",
						Question: &service.QualityRandomQuestion{ID: "synthetic-common", Family: "paths", Version: "fixture"}}}}
				defer func() {
					_, _ = integrationDB.ExecContext(ctx, `DELETE FROM quality_probe_batches WHERE id=$1`, batch.ID)
				}()
				plans := NewScheduledTestPlanRepository(integrationDB)
				repo := plans.(*scheduledTestPlanRepository)
				var group int64
				if action == service.QualityActionIsolateModels {
					require.NoError(t, integrationDB.QueryRowContext(ctx, `INSERT INTO groups(name,platform) VALUES('shared-batch-fixture','openai') RETURNING id`).Scan(&group))
					defer func() { _, _ = integrationDB.ExecContext(ctx, `DELETE FROM groups WHERE id=$1`, group) }()
				}
				var runs []*service.QualityBatchRun
				for i := 0; i < 2; i++ {
					var account int64
					require.NoError(t, integrationDB.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,status,schedulable)
 VALUES('shared-batch-fixture','openai','apikey','active',true) RETURNING id`).Scan(&account))
					defer func() {
						_, _ = integrationDB.ExecContext(ctx, `DELETE FROM scheduler_outbox WHERE account_id=$1`, account)
						_, _ = integrationDB.ExecContext(ctx, `DELETE FROM accounts WHERE id=$1`, account)
					}()
					due := now.Add(-time.Minute)
					policy := &service.QualityPolicy{Action: action, AutoRestore: true, Judge: &service.QualityJudgeConfig{Kind: "jev", Prompt: "Compare answers"}}
					if action == service.QualityActionRemoveModel {
						policy.RemoveModels = []string{"test-model"}
					}
					if group != 0 {
						policy.RemoveGroupIDs = []int64{group}
						_, err := integrationDB.ExecContext(ctx, `INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, account, group)
						require.NoError(t, err)
					}
					plan, err := plans.Create(ctx, &service.ScheduledTestPlan{AccountID: account, ModelID: "test-model",
						CronExpression: "*/5 * * * *", Enabled: true, MaxResults: 100, NextRunAt: &due,
						PelicanConfig: &service.PelicanTestConfig{QuestionKind: service.QualityRandomQuestionKind,
							ReasoningEffort: "medium", ParallelCount: 1,
							Quality: policy}})
					require.NoError(t, err)
					until := now.Add(15 * time.Minute)
					ok, err := plans.ClaimPelican(ctx, plan, now, until, now.Add(5*time.Minute))
					require.NoError(t, err)
					require.True(t, ok)
					// A second primary proposes a different question; the persisted batch wins.
					candidate := *batch
					if i == 1 {
						candidate.Questions = []service.QualityBatchQuestion{{Prompt: "must not replace the first question", Expected: "99"}}
					}
					actual, err := repo.StartQualityBatch(ctx, &candidate, plan, until)
					require.NoError(t, err)
					require.Equal(t, batch.Questions, actual.Questions)
					cfg, samplePolicy := *plan.PelicanConfig, *plan.PelicanConfig.Quality
					cfg.Quality = &samplePolicy
					cfg.ModelID, cfg.Prompt, cfg.Quality.ExpectedAnswer, cfg.RandomQuestion = "test-model", actual.Questions[0].Prompt, actual.Questions[0].Expected, actual.Questions[0].Question
					cfg.QualityBatch = &service.QualityBatchEvidence{ID: batch.ID, StartedAt: batch.StartedAt,
						StartDeadline: batch.StartDeadline, ClosesAt: batch.ClosesAt, Decision: "pending", Route: "synthetic", MappedModel: "test-model"}
					sampleStarted := time.Now().UTC().Truncate(time.Microsecond)
					result := &service.ScheduledTestResult{PlanID: plan.ID, Status: "failed", ErrorMessage: "answer_mismatch", ResponseText: "41",
						StartedAt: sampleStarted, FinishedAt: sampleStarted.Add(time.Millisecond), QualityRoundID: until.Format(time.RFC3339Nano), QualityAction: "comparison_pending",
						PelicanConfig: &cfg, QualityJudgment: &service.QualityJudgment{Kind: "jev", ModelID: "jev-latest", RulesVersion: "quality-choice-v1", Verdict: "incorrect"}}
					if i == 1 && scenario != "all-wrong" {
						result.Status, result.ErrorMessage, result.ResponseText, result.QualityJudgment.Verdict = "success", "", "42", "correct"
						switch scenario {
						case "different-grader-rules":
							result.QualityJudgment.RulesVersion = "quality-choice-v2"
						case "missing-grader-rules":
							result.QualityJudgment.RulesVersion = ""
						case "different-grader-model":
							result.QualityJudgment.ModelID = "other-judge"
						case "different-grader-kind":
							result.QualityJudgment.Kind = "llm"
						case "different-grader-group":
							result.QualityJudgment.GroupID = 99
						case "rotated-grader-account":
							result.QualityJudgment.AccountID = 99
						case "different-grader-explanation":
							result.QualityJudgment.Reason = "different explanation"
						case "different-grader-latency":
							result.QualityJudgment.LatencyMS = 250
						}
					}
					if scenario == "both-missing-grader-rules" {
						result.QualityJudgment.RulesVersion = ""
					}
					if i == 1 && scenario == "late-peer" {
						result.StartedAt, result.FinishedAt = batch.StartDeadline, batch.StartDeadline.Add(time.Second)
					}
					run := &service.QualityBatchRun{Apply: true, Plan: plan, Until: until, Results: []*service.ScheduledTestResult{result}}
					runs = append(runs, run)
					if i == 0 {
						switch scenario {
						case "collection-lost-fingerprint":
							plan.QualityAccountFingerprint = nil
						case "legacy-missing-fingerprint":
							_, err = integrationDB.ExecContext(ctx, `UPDATE quality_probe_batch_runs SET snapshot=snapshot-'account_fingerprint' WHERE batch_id=$1 AND plan_id=$2`, batch.ID, plan.ID)
							require.NoError(t, err)
						case "collection-replaced-fingerprint":
							_, err = integrationDB.ExecContext(ctx, `UPDATE accounts SET credentials='{"api_key":"fixture-replaced"}'::jsonb WHERE id=$1`, account)
							require.NoError(t, err)
							tx, txErr := integrationDB.BeginTx(ctx, nil)
							require.NoError(t, txErr)
							fingerprint, fingerprintErr := accountRouteFingerprint(ctx, tx, account, false)
							require.NoError(t, tx.Rollback())
							require.NoError(t, fingerprintErr)
							plan.QualityAccountFingerprint = &fingerprint
						}
					}
					require.NoError(t, repo.CollectQualityBatch(ctx, run))
					require.NoError(t, repo.CollectQualityBatch(ctx, run), "collection is idempotent")
					// Private result evidence is independent of the public batch
					// ledger. Finalization/restart must preserve even an unknown
					// version verbatim rather than drop or backfill it.
					_, routeErr := integrationDB.ExecContext(ctx, `UPDATE scheduled_test_results SET pelican_config=jsonb_set(pelican_config,'{_quality_observed_route}','{"version":99,"fingerprint":"private-round-fixture"}') WHERE plan_id=$1`, plan.ID)
					require.NoError(t, routeErr)
					require.NoError(t, repo.FinalizeQualityBatches(ctx), "start window still open")
					var enabled bool
					require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT schedulable FROM accounts WHERE id=$1`, runs[0].Plan.AccountID).Scan(&enabled))
					require.True(t, enabled, "a fast wrong answer must not be isolated before controls finish")
				}
				if scenario == "peer-paused" {
					_, err := integrationDB.ExecContext(ctx, `UPDATE scheduled_test_plans SET enabled=false WHERE id=$1`, runs[1].Plan.ID)
					require.NoError(t, err)
				}
				if scenario == "target-edited" {
					_, err := integrationDB.ExecContext(ctx, `UPDATE scheduled_test_plans SET updated_at=clock_timestamp() WHERE id=$1`, runs[0].Plan.ID)
					require.NoError(t, err)
				}
				if scenario == "mapping-edited" {
					_, err := integrationDB.ExecContext(ctx, `UPDATE accounts SET credentials='{"model_mapping":{"test-model":"different-target"}}'::jsonb WHERE id=$1`, runs[0].Plan.AccountID)
					require.NoError(t, err)
				}
				if scenario == "key-edited" || scenario == "unrelated-edited" {
					query := `UPDATE accounts SET credentials='{"api_key":"fixture-rotated"}'::jsonb WHERE id=$1`
					if scenario == "unrelated-edited" {
						query = `UPDATE accounts SET concurrency=23,updated_at=clock_timestamp() WHERE id=$1`
					}
					_, err := integrationDB.ExecContext(ctx, query, runs[0].Plan.AccountID)
					require.NoError(t, err)
				}
				accountSnapshot := func() string {
					var snapshot string
					require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT jsonb_build_object(
 'account',(SELECT to_jsonb(a) FROM accounts a WHERE id=$1),
 'state',(SELECT state FROM account_quality_states WHERE plan_id=$2),
 'groups',(SELECT jsonb_agg(to_jsonb(g) ORDER BY group_id) FROM account_groups g WHERE account_id=$1),
 'events',(SELECT count(*) FROM scheduler_outbox WHERE account_id=$1))::text`, runs[0].Plan.AccountID, runs[0].Plan.ID).Scan(&snapshot))
					return snapshot
				}
				modelSchedulable := func() bool {
					account := &service.Account{}
					var credentials, extra []byte
					require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT platform,type,status,schedulable,credentials,extra FROM accounts WHERE id=$1`, runs[0].Plan.AccountID).Scan(&account.Platform, &account.Type, &account.Status, &account.Schedulable, &credentials, &extra))
					require.NoError(t, json.Unmarshal(credentials, &account.Credentials))
					require.NoError(t, json.Unmarshal(extra, &account.Extra))
					return account.IsSchedulableForModel("test-model")
				}
				beforeFinalization := accountSnapshot()
				// Simulate closing the start window without sleeping or making requests.
				_, err := integrationDB.ExecContext(ctx, `UPDATE quality_probe_batches SET start_deadline=clock_timestamp()-interval '1 second' WHERE id=$1`, batch.ID)
				require.NoError(t, err)
				restarted := NewScheduledTestPlanRepository(integrationDB).(*scheduledTestPlanRepository)
				require.NoError(t, restarted.FinalizeQualityBatches(ctx))
				require.NoError(t, restarted.FinalizeQualityBatches(ctx), "finalization is idempotent")
				if scenario == "all-wrong" || scenario == "passing-peer-finishes-last" {
					var completed bool
					require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT (pelican_config->'_quality_latest_round'->>'completed_at')::timestamptz=last_run_at
 FROM scheduled_test_plans WHERE id=$1`, runs[0].Plan.ID).Scan(&completed))
					require.True(t, completed, "restarted batch finalization completes the same registered round")
				}
				var enabled bool
				require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT schedulable FROM accounts WHERE id=$1`, runs[0].Plan.AccountID).Scan(&enabled))
				require.Equal(t, action != "disable_scheduling" || !isolates, enabled)
				var status, verdict, rulesVersion, recordedAction string
				var raw []byte
				require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT status,quality_judgment->>'verdict',COALESCE(quality_judgment->>'rules_version',''),quality_action,pelican_config FROM scheduled_test_results WHERE plan_id=$1`, runs[0].Plan.ID).Scan(&status, &verdict, &rulesVersion, &recordedAction, &raw))
				require.Equal(t, "failed", status)
				require.Contains(t, string(raw), `"fingerprint": "private-round-fixture"`, "shared finalization preserves the original private result envelope across restart")
				require.Equal(t, "incorrect", verdict, "raw verdicts are never rewritten as passes")
				if scenario == "both-missing-grader-rules" {
					require.Empty(t, rulesVersion, "missing provenance is never backfilled from the current process")
				} else {
					require.Equal(t, "quality-choice-v1", rulesVersion, "raw grading provenance survives collection and restart")
				}
				var cfg service.PelicanTestConfig
				require.NoError(t, json.Unmarshal(raw, &cfg))
				if isolates {
					wantAction := "scheduling_disabled"
					switch runs[0].Plan.PelicanConfig.Quality.Action {
					case service.QualityActionRemoveModel:
						wantAction = "models_cooled"
					case service.QualityActionIsolateModels:
						wantAction = "groups_removed"
					}
					require.Equal(t, wantAction, recordedAction)
					require.Equal(t, "peer_passed", cfg.QualityBatch.Decision)
				} else if scenario == "target-edited" || scenario == "mapping-edited" || scenario == "key-edited" || scenario == "legacy-missing-fingerprint" || scenario == "collection-replaced-fingerprint" {
					require.Equal(t, "stale_run", recordedAction)
					require.Equal(t, beforeFinalization, accountSnapshot(), "stale results cannot mutate account, ownership, membership or events")
				} else {
					require.Equal(t, "inconclusive", recordedAction)
					if scenario == "both-missing-grader-rules" {
						require.Equal(t, "inconclusive", cfg.QualityBatch.Decision)
						require.Empty(t, cfg.QualityBatch.ComparisonKey)
					} else {
						require.Equal(t, "no_passing_peer", cfg.QualityBatch.Decision)
					}
					metrics, err := readAccountPerformance(ctx, integrationDB, []int64{runs[0].Plan.AccountID}, time.Now().Add(time.Second), nil)
					require.NoError(t, err)
					require.Zero(t, metrics[runs[0].Plan.AccountID].QualitySamples, "uncalibrated wrong answers cannot penalize the performance monitor")
				}
				if action != "disable_scheduling" {
					require.Equal(t, !isolates, modelSchedulable())
				}
				if scenario == "recovery-after-restart" {
					// A new persisted passing round must release only this rule's
					// quarantine after another process reconstruction.
					next := time.Now().UTC().Truncate(time.Microsecond)
					recoveryBatch := &service.QualitySharedBatch{ID: "synthetic-" + uuid.NewString(), StartedAt: next.Add(-time.Minute), StartDeadline: next.Add(time.Minute), ClosesAt: next.Add(11 * time.Minute), Questions: batch.Questions}
					defer func() {
						_, _ = integrationDB.ExecContext(ctx, `DELETE FROM quality_probe_batches WHERE id=$1`, recoveryBatch.ID)
					}()
					_, err := integrationDB.ExecContext(ctx, `UPDATE scheduled_test_plans SET next_run_at=$2 WHERE id=$1`, runs[0].Plan.ID, next.Add(-time.Minute))
					require.NoError(t, err)
					plan, err := plans.GetByID(ctx, runs[0].Plan.ID)
					require.NoError(t, err)
					until := next.Add(15 * time.Minute)
					claimed, err := plans.ClaimPelican(ctx, plan, next, until, next.Add(5*time.Minute))
					require.NoError(t, err)
					require.True(t, claimed)
					_, err = restarted.StartQualityBatch(ctx, recoveryBatch, plan, until)
					require.NoError(t, err)
					result, cfg, judgment := *runs[0].Results[0], *runs[0].Results[0].PelicanConfig, *runs[0].Results[0].QualityJudgment
					result.ID, result.Status, result.ErrorMessage, result.ResponseText = 0, "success", "", "42"
					result.StartedAt, result.FinishedAt, result.QualityRoundID = next, next.Add(time.Millisecond), until.Format(time.RFC3339Nano)
					judgment.Verdict = "correct"
					cfg.QualityBatch = &service.QualityBatchEvidence{ID: recoveryBatch.ID, StartedAt: recoveryBatch.StartedAt, StartDeadline: recoveryBatch.StartDeadline, ClosesAt: recoveryBatch.ClosesAt, Route: "synthetic", MappedModel: "test-model", Decision: "pending"}
					result.PelicanConfig, result.QualityJudgment = &cfg, &judgment
					plan.QualityAccountFingerprint = nil
					require.NoError(t, restarted.CollectQualityBatch(ctx, &service.QualityBatchRun{Apply: true, Plan: plan, Until: until, Results: []*service.ScheduledTestResult{&result}}))
					_, err = integrationDB.ExecContext(ctx, `UPDATE quality_probe_batches SET start_deadline=clock_timestamp()-interval '1 second' WHERE id=$1`, recoveryBatch.ID)
					require.NoError(t, err)
					recovered := NewScheduledTestPlanRepository(integrationDB).(*scheduledTestPlanRepository)
					require.NoError(t, recovered.FinalizeQualityBatches(ctx))
					require.NoError(t, recovered.FinalizeQualityBatches(ctx))
					var restored string
					require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT quality_action FROM scheduled_test_results WHERE plan_id=$1 AND quality_round_id=$2`, plan.ID, result.QualityRoundID).Scan(&restored))
					require.Equal(t, "restored", restored)
					require.True(t, modelSchedulable())
					if group != 0 {
						var memberships int
						require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT count(*) FROM account_groups WHERE account_id=$1 AND group_id=$2`, plan.AccountID, group).Scan(&memberships))
						require.Equal(t, 1, memberships)
					}
				}
			})
		}
	}
}
