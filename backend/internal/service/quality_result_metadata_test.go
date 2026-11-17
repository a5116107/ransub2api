package service

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestScheduledQualityWritesIndependentMeasurements(t *testing.T) {
	for _, verdict := range []string{"correct", "incorrect", "unknown"} {
		t.Run(verdict, func(t *testing.T) {
			plan := pelicanPlan()
			plan.PelicanConfig.QuestionKind = "candy"
			plan.PelicanConfig.Quality = &QualityPolicy{Action: "disable_scheduling", ExpectedAnswer: "42", Judge: &QualityJudgeConfig{Kind: "llm", GroupID: 9, ModelID: "judge-model", Prompt: "compare"}}
			plans, results := &qualityPlanRepo{}, &pelicanResults{}
			runner := &ScheduledTestRunnerService{planRepo: plans, scheduledSvc: NewScheduledTestService(plans, results)}
			runner.runPelican = func(_ context.Context, _ int64, model string, cfg *PelicanTestConfig) (*ScheduledTestResult, error) {
				started := time.Now().Add(-time.Second)
				snapshot := *cfg
				snapshot.ModelID = model
				snapshot.AvailabilityResult = &AvailabilityProbeResult{Version: 1, Status: "pass", Reason: "completed_response", StartedAt: started, FinishedAt: time.Now()}
				return &ScheduledTestResult{Status: "success", ResponseText: "42", StartedAt: started, FinishedAt: time.Now(), PelicanConfig: &snapshot}, nil
			}
			runner.judgeQuality = func(context.Context, int64, *PelicanTestConfig, string) *QualityJudgment {
				return &QualityJudgment{Kind: "llm", RulesVersion: qualityLLMRulesVersion, GroupID: 9, ModelID: "judge-model", Verdict: verdict}
			}
			require.True(t, runner.runOnePlan(t.Context(), plan))
			require.NotEmpty(t, results.results)
			for _, result := range results.results {
				raw, err := json.Marshal(result.PelicanConfig)
				require.NoError(t, err)
				var saved map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(raw, &saved))
				require.NotEmpty(t, saved["quality_measurement"], "saved results need independent, versioned measurement evidence")
				want := map[string]string{"correct": "pass", "incorrect": "fail", "unknown": "unknown"}[verdict]
				require.Equal(t, want, result.PelicanConfig.QualityMeasurement.Dimensions["answer_correctness"].Status)
				require.Equal(t, "pass", result.PelicanConfig.QualityMeasurement.Dimensions["transport"].Status)
				require.Equal(t, "unknown", result.PelicanConfig.QualityMeasurement.Dimensions["model_identity"].Status)
			}
			require.Nil(t, plan.PelicanConfig.QualityMeasurement, "result evidence must not mutate the reusable rule")
		})
	}
}

func TestQualityMeasurementBuiltinStateAndJevBoundaries(t *testing.T) {
	for _, scenario := range []string{"admission", "triggered-candy", "state", "missing-grader", "jev", "jev-failed", "jev-uncertain"} {
		t.Run(scenario, func(t *testing.T) {
			plan, result := qualityMeasurementFixture()
			wantCorrectness, wantGrading := "pass", "pass"
			switch scenario {
			case "admission", "triggered-candy":
				result.PelicanConfig.Prompt = CandyPrompt
				result.PelicanConfig.Quality.Judge = nil
				if scenario == "admission" {
					result.PelicanConfig.Quality.Action = QualityActionOpenAITwoFAAdmission
					result.QualityJudgment.Reason = "builtin_admission"
				} else {
					plan.TriggerSource = quality5xxSource
					result.QualityJudgment.Reason = "builtin_candy"
				}
			case "state":
				result.PelicanConfig.QuestionKind = OpenAICodexStateProbeQuestionKind
				wantCorrectness, wantGrading = "unknown", "unknown"
			case "missing-grader":
				result.PelicanConfig.Quality.Judge = nil
				wantCorrectness, wantGrading = "unknown", "unknown"
			case "jev", "jev-failed", "jev-uncertain":
				result.PelicanConfig.Quality.Judge.Kind = "jev"
				result.QualityJudgment.Kind = "jev"
				result.QualityJudgment.GroupID = 0
				result.QualityJudgment.RulesVersion = qualityJevRulesVersion
				if scenario != "jev" {
					result.Status, result.ErrorMessage, result.QualityJudgment.Verdict = "failed", "judge_inconclusive", "unknown"
					result.QualityJudgment.Reason = "judge_jev_timeout"
					wantCorrectness, wantGrading = "unknown", "fail"
					if scenario == "jev-uncertain" {
						result.QualityJudgment.Reason, wantGrading = "judge_jev_uncertain", "unknown"
					}
				}
			}
			recordQualityMeasurement(plan, result)
			m := result.PelicanConfig.QualityMeasurement
			require.Equal(t, wantCorrectness, m.Dimensions["answer_correctness"].Status)
			require.Equal(t, wantGrading, m.Dimensions["grading"].Status)
			require.Equal(t, "unknown", m.Dimensions["model_identity"].Status)
		})
	}
}

func TestQualityResultMetadataCannotConfigureRuleActions(t *testing.T) {
	plan := &ScheduledTestPlan{ModelID: "gpt-6-astra", CronExpression: "*/10 * * * *", MaxResults: 100, PelicanConfig: &PelicanTestConfig{QuestionKind: OpenAICodexStateProbeQuestionKind, ParallelCount: 1, ReasoningEffort: "high", Quality: &QualityPolicy{Action: "disable_scheduling"}}}
	plan.PelicanConfig.QualityModelOutcomes = map[string]string{"gpt-6-astra": "passed"}
	plan.PelicanConfig.QualityModelActions = map[string]string{"gpt-6-astra": "restored"}
	plan.PelicanConfig.AvailabilityResult = &AvailabilityProbeResult{Version: 1, Status: "pass", Reason: "forged"}
	plan.PelicanConfig.QualityMeasurement = &QualityMeasurement{Version: 1, MeasurementHash: "forged"}
	_, err := nextPlanRun(plan, time.Now())
	require.NoError(t, err)
	require.Nil(t, plan.PelicanConfig.QualityModelOutcomes)
	require.Nil(t, plan.PelicanConfig.QualityModelActions)
	require.Nil(t, plan.PelicanConfig.AvailabilityResult)
	require.Nil(t, plan.PelicanConfig.QualityMeasurement)
}

func TestQualityMeasurementIndependentDimensions(t *testing.T) {
	for _, tc := range []struct {
		name, status, errorMessage, verdict, reason, transport string
		correctness, grading                                   string
		missingVersion                                         bool
	}{
		{"correct", "success", "", "correct", "", "pass", "pass", "pass", false},
		{"incorrect", "failed", "answer_mismatch", "incorrect", "", "pass", "fail", "pass", false},
		{"judge-failed", "failed", "judge_inconclusive", "unknown", "judge_request_failed", "pass", "unknown", "fail", false},
		{"judge-uncertain", "failed", "judge_inconclusive", "unknown", "uncertain", "pass", "unknown", "unknown", false},
		{"unknown-source", "success", "", "correct", "", "pass", "unknown", "unknown", true},
		{"transport-failed", "failed", "timeout", "", "", "fail", "unknown", "unknown", false},
		{"conflicting-answer", "failed", "timeout", "correct", "", "pass", "unknown", "unknown", false},
		{"skipped", "skipped", "model_unsupported", "", "", "inconclusive", "unknown", "unknown", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, result := qualityMeasurementFixture()
			result.Status, result.ErrorMessage = tc.status, tc.errorMessage
			result.QualityJudgment.Verdict, result.QualityJudgment.Reason = tc.verdict, tc.reason
			result.PelicanConfig.AvailabilityResult.Status = tc.transport
			if tc.missingVersion {
				result.QualityJudgment.RulesVersion = ""
			}
			if tc.verdict == "" {
				result.QualityJudgment = nil
			}
			originalJudgment := result.QualityJudgment
			recordQualityMeasurement(plan, result)
			m := result.PelicanConfig.QualityMeasurement
			require.NotNil(t, m)
			require.Equal(t, tc.correctness, m.Dimensions["answer_correctness"].Status)
			require.Equal(t, tc.grading, m.Dimensions["grading"].Status)
			wantTransport := tc.transport
			if wantTransport == "inconclusive" {
				wantTransport = "unknown"
			}
			require.Equal(t, wantTransport, m.Dimensions["transport"].Status)
			require.Equal(t, "unknown", m.Dimensions["model_identity"].Status)
			require.Nil(t, m.Dimensions["model_identity"].Score)
			require.Zero(t, m.Dimensions["model_identity"].SampleCount)
			require.Nil(t, m.ValidUntil, "historical evidence must not invent a validity policy")
			require.Same(t, originalJudgment, result.QualityJudgment)
			require.Equal(t, tc.status, result.Status)
			require.Equal(t, tc.errorMessage, result.ErrorMessage)
			for _, dimension := range m.Dimensions {
				if dimension.Status == "unknown" {
					require.Nil(t, dimension.Score)
					require.Zero(t, dimension.SampleCount)
				} else {
					require.NotNil(t, dimension.Score)
					require.Equal(t, 1, dimension.SampleCount)
				}
			}
		})
	}
}

func qualityMeasurementFixture() (*ScheduledTestPlan, *ScheduledTestResult) {
	started := time.Now().UTC().Add(-time.Minute)
	cfg := &PelicanTestConfig{QuestionKind: "candy", ModelID: "public-model", Prompt: "bounded question", ActualPrompt: "bounded question", ParallelCount: 1, ReasoningEffort: "high",
		Quality: &QualityPolicy{Action: "disable_scheduling", ExpectedAnswer: "42", Judge: &QualityJudgeConfig{Kind: "llm", GroupID: 9, ModelID: "judge-model", Prompt: "compare"}},
		AvailabilityResult: &AvailabilityProbeResult{Version: 1, Status: "pass", StartedAt: started, FinishedAt: started.Add(time.Second),
			Scope: &AvailabilityRequestScope{Version: 4, Model: "wire-model", Protocol: "responses", Stream: true, ServiceTier: "priority", ReasoningEffort: "high", Capability: "text", ConnectionScope: "PRIVATE_GRANT"}}}
	return &ScheduledTestPlan{UpdatedAt: started.Add(-time.Hour), PelicanConfig: cfg}, &ScheduledTestResult{Status: "success", StartedAt: started, FinishedAt: started.Add(4 * time.Second), PelicanConfig: cfg,
		QualityJudgment: &QualityJudgment{Kind: "llm", RulesVersion: qualityLLMRulesVersion, ModelID: "judge-model", GroupID: 9, Verdict: "correct"}}
}

func TestQualityMeasurementValidityPolicyAndObservationDeadline(t *testing.T) {
	for _, seconds := range []int{-1, 1, 59, 60, 86400, 604800, 604801} {
		t.Run(fmt.Sprint(seconds), func(t *testing.T) {
			plan, result := qualityMeasurementFixture()
			plan.PelicanConfig.Quality.EvidenceValidForSeconds = seconds
			err := validateQualityPolicy(plan)
			if seconds < 60 || seconds > 604800 {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			recordQualityMeasurement(plan, result)
			measurement := result.PelicanConfig.QualityMeasurement
			require.Equal(t, seconds, measurement.ValidForSeconds)
			require.Equal(t, measurement.ObservedAt.Add(time.Duration(seconds)*time.Second), *measurement.ValidUntil)
			require.True(t, measurement.ValidUntil.Before(measurement.CompletedAt.Add(time.Duration(seconds)*time.Second)), "grading cannot renew upstream evidence")
		})
	}
	plan, _ := qualityMeasurementFixture()
	plan.PelicanConfig.QuestionKind = OpenAICodexStateProbeQuestionKind
	plan.PelicanConfig.Quality.EvidenceValidForSeconds = 60
	plan.PelicanConfig.Quality.ExpectedAnswer = ""
	require.Error(t, validateQualityPolicy(plan), "account-state probes do not measure answer quality")
}

func TestQualityMeasurementValidityDoesNotInventEvidence(t *testing.T) {
	for _, scenario := range []string{"legacy", "no-probe", "bad-probe-time", "unknown-answer", "invalid-time", "wrong-answer", "slow-grading"} {
		t.Run(scenario, func(t *testing.T) {
			plan, result := qualityMeasurementFixture()
			plan.PelicanConfig.Quality.EvidenceValidForSeconds = 60
			switch scenario {
			case "legacy":
				plan.PelicanConfig.Quality.EvidenceValidForSeconds = 0
			case "no-probe":
				result.PelicanConfig.AvailabilityResult = nil
			case "bad-probe-time":
				result.PelicanConfig.AvailabilityResult.FinishedAt = result.FinishedAt.Add(time.Second)
			case "unknown-answer":
				result.QualityJudgment.Verdict = "unknown"
			case "invalid-time":
				result.StartedAt = result.FinishedAt.Add(time.Second)
			case "wrong-answer":
				result.Status, result.ErrorMessage, result.QualityJudgment.Verdict = "failed", "answer_mismatch", "incorrect"
			case "slow-grading":
				result.FinishedAt = result.StartedAt.Add(2 * time.Minute)
			}
			recordQualityMeasurement(plan, result)
			m := result.PelicanConfig.QualityMeasurement
			if scenario == "wrong-answer" || scenario == "slow-grading" {
				require.NotNil(t, m.ValidUntil)
				if scenario == "slow-grading" {
					require.True(t, m.ValidUntil.Before(m.CompletedAt))
				}
			} else {
				require.Nil(t, m.ValidUntil)
			}
			require.Equal(t, "unknown", m.Dimensions["model_identity"].Status)
		})
	}
}

func TestQualityMeasurementValidityReadAssessment(t *testing.T) {
	for _, scenario := range []string{"within", "database-precision", "database-half-even", "database-half-odd", "boundary", "expired", "rule-changed", "paused", "missing-rule-version", "future", "legacy", "extended-deadline", "missing-grading", "invalid-hash", "unknown-version", "wrong-answer", "policy-changed"} {
		t.Run(scenario, func(t *testing.T) {
			plan, result := qualityMeasurementFixture()
			plan.PelicanConfig.Quality.EvidenceValidForSeconds = 60
			if scenario == "database-precision" || scenario == "database-half-even" || scenario == "database-half-odd" {
				fraction := 123456789 * time.Nanosecond
				if scenario == "database-half-even" {
					fraction = 123456500 * time.Nanosecond
				}
				if scenario == "database-half-odd" {
					fraction = 123457500 * time.Nanosecond
				}
				started := time.Now().UTC().Add(-time.Minute).Truncate(time.Second).Add(fraction)
				result.StartedAt, result.FinishedAt = started, started.Add(4*time.Second)
				result.PelicanConfig.AvailabilityResult.StartedAt = started
				result.PelicanConfig.AvailabilityResult.FinishedAt = started.Add(time.Second)
			}
			recordQualityMeasurement(plan, result)
			m := result.PelicanConfig.QualityMeasurement
			now, version, enabled := m.ObservedAt.Add(30*time.Second), plan.UpdatedAt, true
			status, reason := "within_window", "within_window"
			switch scenario {
			case "database-precision", "database-half-odd":
				result.StartedAt = result.StartedAt.Round(time.Microsecond)
				result.FinishedAt = result.FinishedAt.Round(time.Microsecond)
			case "database-half-even":
				result.StartedAt = result.StartedAt.Truncate(time.Microsecond)
				result.FinishedAt = result.FinishedAt.Truncate(time.Microsecond)
			case "boundary", "expired":
				now = *m.ValidUntil
				if scenario == "expired" {
					now = now.Add(time.Second)
				}
				status, reason = "expired", "expired"
			case "rule-changed":
				version = version.Add(time.Microsecond)
				status, reason = "unknown", "rule_changed"
			case "paused":
				enabled = false
				status, reason = "unknown", "rule_paused"
			case "missing-rule-version":
				m.RuleVersion = time.Time{}
				status, reason = "unknown", "rule_version_unknown"
			case "future":
				now = m.CompletedAt.Add(-time.Nanosecond)
				status, reason = "unknown", "invalid_measurement"
			case "legacy":
				m.ValidForSeconds, m.ValidUntil = 0, nil
				status, reason = "unknown", "validity_not_declared"
			case "extended-deadline":
				deadline := m.ValidUntil.Add(time.Second)
				m.ValidUntil = &deadline
				status, reason = "unknown", "invalid_measurement"
			case "missing-grading":
				delete(m.Dimensions, "grading")
				status, reason = "unknown", "answer_not_observed"
			case "invalid-hash":
				m.MeasurementHash = "forged"
				status, reason = "unknown", "invalid_measurement"
			case "unknown-version":
				m.Version = 99
				status, reason = "unknown", "invalid_measurement"
			case "wrong-answer":
				m.Dimensions["answer_correctness"] = measuredQualityDimension("fail", "graded_incorrect", 0)
			case "policy-changed":
				result.PelicanConfig.Quality.EvidenceValidForSeconds++
				status, reason = "unknown", "invalid_measurement"
			}
			before, err := json.Marshal(result)
			require.NoError(t, err)
			assessment := AssessQualityMeasurementValidity(result, version, enabled, now)
			require.Equal(t, status, assessment.Status)
			require.Equal(t, reason, assessment.Reason)
			require.Equal(t, now, assessment.EvaluatedAt)
			after, err := json.Marshal(result)
			require.NoError(t, err)
			require.Equal(t, before, after, "reading validity cannot rewrite historical evidence or renew its deadline")
		})
	}
}

func TestQualityMeasurementHashesTrackContractNotVerdict(t *testing.T) {
	plan, result := qualityMeasurementFixture()
	recordQualityMeasurement(plan, result)
	original := result.PelicanConfig.QualityMeasurement
	result.PelicanConfig.Quality.EvidenceValidForSeconds = 3600
	recordQualityMeasurement(plan, result)
	require.Equal(t, original.MeasurementHash, result.PelicanConfig.QualityMeasurement.MeasurementHash)
	require.NotEqual(t, original.PolicyHash, result.PelicanConfig.QualityMeasurement.PolicyHash, "evidence lifetime is a policy, not a new measurement algorithm")
	result.PelicanConfig.Quality.EvidenceValidForSeconds = 0
	require.Len(t, original.MeasurementHash, 64)
	require.Len(t, original.PolicyHash, 64)
	require.Equal(t, result.PelicanConfig.AvailabilityResult.FinishedAt, original.ObservedAt)
	require.True(t, original.CompletedAt.After(original.ObservedAt))
	require.Equal(t, "public-model", original.RequestedModel)
	require.Equal(t, "wire-model", original.Scope.Model)
	raw, err := json.Marshal(original)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "PRIVATE_GRANT")
	require.NotContains(t, string(raw), "bounded question")
	result.Status, result.ErrorMessage, result.QualityJudgment.Verdict = "failed", "answer_mismatch", "incorrect"
	result.QualityJudgment.AccountID = 123
	result.QualityJudgment.Reason = "PRIVATE_EXPLANATION"
	result.FinishedAt = result.FinishedAt.Add(time.Second)
	recordQualityMeasurement(plan, result)
	require.Equal(t, original.MeasurementHash, result.PelicanConfig.QualityMeasurement.MeasurementHash)
	result.PelicanConfig.Quality.Action = QualityActionObserveOnly
	recordQualityMeasurement(plan, result)
	require.Equal(t, original.MeasurementHash, result.PelicanConfig.QualityMeasurement.MeasurementHash)
	require.NotEqual(t, original.PolicyHash, result.PelicanConfig.QualityMeasurement.PolicyHash)
	for _, change := range []func(){
		func() { result.PelicanConfig.ReasoningEffort = "low" },
		func() { result.PelicanConfig.AvailabilityResult.Scope.Model = "other-wire-model" },
		func() { result.PelicanConfig.AvailabilityResult.Scope.Protocol = "chat" },
		func() { result.PelicanConfig.Quality.ExpectedAnswer = "43" },
		func() { result.QualityJudgment.RulesVersion = "new-grading-version" },
	} {
		before := result.PelicanConfig.QualityMeasurement.MeasurementHash
		change()
		recordQualityMeasurement(plan, result)
		require.NotEqual(t, before, result.PelicanConfig.QualityMeasurement.MeasurementHash)
	}
}

func TestQualityMeasurementRejectsForgedAndInvalidEvidence(t *testing.T) {
	for _, scenario := range []string{"coverage-only", "no-quality", "invalid-time", "invalid-transport-time", "unknown-transport-version"} {
		t.Run(scenario, func(t *testing.T) {
			plan, result := qualityMeasurementFixture()
			result.PelicanConfig.QualityMeasurement = &QualityMeasurement{Version: 999, MeasurementHash: "forged"}
			switch scenario {
			case "coverage-only":
				plan.TriggerSource = AvailabilityCoverageSource
			case "no-quality":
				plan.PelicanConfig.Quality = nil
			case "invalid-time":
				result.StartedAt = result.FinishedAt.Add(time.Second)
			case "invalid-transport-time":
				result.PelicanConfig.AvailabilityResult.FinishedAt = result.FinishedAt.Add(time.Second)
			case "unknown-transport-version":
				result.PelicanConfig.AvailabilityResult.Version = 99
			}
			recordQualityMeasurement(plan, result)
			m := result.PelicanConfig.QualityMeasurement
			if scenario == "coverage-only" || scenario == "no-quality" {
				require.Nil(t, m)
				return
			}
			require.NotEqual(t, "forged", m.MeasurementHash)
			if scenario == "invalid-time" {
				for _, d := range m.Dimensions {
					require.Equal(t, "unknown", d.Status)
				}
			} else {
				require.Equal(t, "unknown", m.Dimensions["transport"].Status)
				require.Nil(t, m.Scope)
			}
		})
	}
}
