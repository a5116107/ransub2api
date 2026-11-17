package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

const qualityMeasurementVersion = 1

// QualityMeasurement is immutable sample evidence, not current admission. Each
// dimension states only what this execution observed; missing baselines and
// validity policies never acquire a passing conclusion from another dimension.
type QualityMeasurement struct {
	Version         int                                    `json:"version"`
	MeasurementHash string                                 `json:"measurement_hash"`
	PolicyHash      string                                 `json:"policy_hash"`
	RuleVersion     time.Time                              `json:"rule_version"`
	RequestedModel  string                                 `json:"requested_model"`
	QuestionKind    string                                 `json:"question_kind"`
	QuestionVersion string                                 `json:"question_version,omitempty"`
	ObservedAt      time.Time                              `json:"observed_at"`
	CompletedAt     time.Time                              `json:"completed_at"`
	ValidUntil      *time.Time                             `json:"valid_until"`
	ValidForSeconds int                                    `json:"valid_for_seconds,omitempty"`
	Scope           *AvailabilityRequestScope              `json:"scope"`
	Dimensions      map[string]QualityDimensionMeasurement `json:"dimensions"`
}

// QualityMeasurementValidity keeps a detail read's rule/window, registered round
// and configured route assessments separate. None certifies current admission.
type QualityMeasurementValidity struct {
	Status      string                           `json:"status"`
	Reason      string                           `json:"reason"`
	EvaluatedAt time.Time                        `json:"evaluated_at"`
	Round       *QualityMeasurementRoundValidity `json:"round,omitempty"`
	Route       *QualityMeasurementRouteValidity `json:"route,omitempty"`
}

// QualityMeasurementRoundValidity compares a sample with the last registered
// quality claim. Completeness is not a passing answer or current route admission.
type QualityMeasurementRoundValidity struct {
	Status              string `json:"status"`
	Reason              string `json:"reason"`
	SampleCount         int64  `json:"sample_count"`
	ExpectedSampleCount *int64 `json:"expected_sample_count"`
}

func validQualityEvidenceLifetime(seconds int) bool {
	return seconds >= 60 && seconds <= 604800
}

// Frozen JSON retains nanoseconds; PostgreSQL timestamps use microseconds and
// round exact half-microseconds to even. Go's Time.Round rounds those ties up.
func qualityMeasurementSQLTime(value time.Time) time.Time {
	base := value.Truncate(time.Microsecond)
	remainder := value.Nanosecond() % 1000
	if remainder > 500 || (remainder == 500 && (base.Nanosecond()/1000)%2 != 0) {
		return base.Add(time.Microsecond)
	}
	return base
}

func qualityMeasurementProbe(result *ScheduledTestResult) *AvailabilityProbeResult {
	if result == nil || result.PelicanConfig == nil || result.StartedAt.IsZero() || qualityMeasurementSQLTime(result.FinishedAt).Before(qualityMeasurementSQLTime(result.StartedAt)) {
		return nil
	}
	probe := result.PelicanConfig.AvailabilityResult
	if probe == nil || probe.Version != 1 || probe.StartedAt.IsZero() || probe.FinishedAt.Before(probe.StartedAt) ||
		qualityMeasurementSQLTime(probe.StartedAt).Before(qualityMeasurementSQLTime(result.StartedAt)) || qualityMeasurementSQLTime(probe.FinishedAt).After(qualityMeasurementSQLTime(result.FinishedAt)) {
		return nil
	}
	return probe
}

func qualityMeasuredAnswer(m *QualityMeasurement) bool {
	answer, grading := m.Dimensions["answer_correctness"], m.Dimensions["grading"]
	return answer.SampleCount == 1 && answer.Score != nil && grading.SampleCount == 1 && grading.Score != nil &&
		grading.Status == "pass" && grading.Reason == "grading_completed" && *grading.Score == 1 &&
		((answer.Status == "pass" && answer.Reason == "graded_correct" && *answer.Score == 1) ||
			(answer.Status == "fail" && answer.Reason == "graded_incorrect" && *answer.Score == 0))
}

// AssessQualityMeasurementValidity never mutates or extends frozen evidence.
// The repository supplies the current rule and database time in the same read.
func AssessQualityMeasurementValidity(result *ScheduledTestResult, ruleVersion time.Time, enabled bool, now time.Time) *QualityMeasurementValidity {
	if result == nil || result.PelicanConfig == nil || result.PelicanConfig.QualityMeasurement == nil {
		return nil
	}
	out := &QualityMeasurementValidity{Status: "unknown", Reason: "invalid_measurement", EvaluatedAt: now}
	cfg, m := result.PelicanConfig, result.PelicanConfig.QualityMeasurement
	validHash := func(value string) bool {
		decoded, err := hex.DecodeString(value)
		return err == nil && len(value) == sha256.Size*2 && len(decoded) == sha256.Size
	}
	if m.Version != qualityMeasurementVersion || !validHash(m.MeasurementHash) || !validHash(m.PolicyHash) ||
		now.IsZero() || m.ObservedAt.IsZero() || m.CompletedAt.Before(m.ObservedAt) || now.Before(m.CompletedAt) ||
		!qualityMeasurementSQLTime(m.CompletedAt).Equal(qualityMeasurementSQLTime(result.FinishedAt)) {
		return out
	}
	if m.RuleVersion.IsZero() || ruleVersion.IsZero() {
		out.Reason = "rule_version_unknown"
		return out
	}
	if !m.RuleVersion.Equal(ruleVersion) {
		out.Reason = "rule_changed"
		return out
	}
	if !enabled {
		out.Reason = "rule_paused"
		return out
	}
	if m.ValidForSeconds == 0 && m.ValidUntil == nil {
		out.Reason = "validity_not_declared"
		return out
	}
	if cfg.Quality == nil || !isQualityAnswerQuestion(cfg.QuestionKind) || !validQualityEvidenceLifetime(m.ValidForSeconds) ||
		cfg.Quality.EvidenceValidForSeconds != m.ValidForSeconds ||
		m.PolicyHash != qualityMeasurementHash([]any{qualityMeasurementVersion, cfg.Quality, cfg.ModelIDs, cfg.ParallelCount}) {
		return out
	}
	if !qualityMeasuredAnswer(m) {
		out.Reason = "answer_not_observed"
		return out
	}
	probe := qualityMeasurementProbe(result)
	if probe == nil || !m.ObservedAt.Equal(probe.FinishedAt) || m.ValidUntil == nil ||
		!m.ValidUntil.Equal(m.ObservedAt.Add(time.Duration(m.ValidForSeconds)*time.Second)) {
		return out
	}
	out.Status, out.Reason = "within_window", "within_window"
	if !now.Before(*m.ValidUntil) {
		out.Status, out.Reason = "expired", "expired"
	}
	return out
}

type QualityDimensionMeasurement struct {
	Status      string   `json:"status"`
	Reason      string   `json:"reason"`
	Score       *float64 `json:"score"`
	SampleCount int      `json:"sample_count"`
}

func measuredQualityDimension(status, reason string, score float64) QualityDimensionMeasurement {
	return QualityDimensionMeasurement{Status: status, Reason: reason, Score: &score, SampleCount: 1}
}

func qualityMeasurementHash(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func recordQualityMeasurement(plan *ScheduledTestPlan, result *ScheduledTestResult) {
	if result == nil || result.PelicanConfig == nil {
		return
	}
	result.QualityRouteEvidence = nil
	// Never retain metadata supplied by an execution adapter or reusable rule.
	cfg := *result.PelicanConfig
	cfg.QualityMeasurement = nil
	result.PelicanConfig = &cfg
	if plan == nil || plan.PelicanConfig == nil || plan.PelicanConfig.Quality == nil ||
		plan.TriggerSource == AvailabilityCoverageSource || cfg.Quality == nil {
		return
	}
	m := &QualityMeasurement{Version: qualityMeasurementVersion, RuleVersion: plan.UpdatedAt,
		RequestedModel: cfg.ModelID, QuestionKind: cfg.QuestionKind,
		ValidForSeconds: cfg.Quality.EvidenceValidForSeconds,
		ObservedAt:      result.FinishedAt, CompletedAt: result.FinishedAt,
		Dimensions: map[string]QualityDimensionMeasurement{
			"answer_correctness": {Status: "unknown", Reason: "grading_not_observed"},
			"transport":          {Status: "unknown", Reason: "transport_not_observed"},
			"grading":            {Status: "unknown", Reason: "grading_not_observed"},
			"model_identity":     {Status: "unknown", Reason: "independent_baseline_not_observed"},
		}}
	cfg.QualityMeasurement = m
	if cfg.RandomQuestion != nil {
		m.QuestionVersion = cfg.RandomQuestion.Version
	}
	if probe := qualityMeasurementProbe(result); probe != nil {
		m.ObservedAt = probe.FinishedAt // Grading time must not refresh answer evidence.
		if probe.Scope != nil {
			scope := *probe.Scope
			scope.ConnectionScope = ""
			m.Scope = &scope
		}
		switch probe.Status {
		case "pass":
			m.Dimensions["transport"] = measuredQualityDimension("pass", "completed_response", 1)
		case "fail":
			m.Dimensions["transport"] = measuredQualityDimension("fail", "request_failed", 0)
		}
	}
	judge := result.QualityJudgment
	grader := struct {
		Kind, Version, Model string
		Group                int64
	}{}
	if judge != nil {
		grader.Kind, grader.Version, grader.Model, grader.Group = judge.Kind, judge.RulesVersion, judge.ModelID, judge.GroupID
	}
	knownGrading := qualityJudgeRuntimeIdentityKnown(&cfg, judge)
	// Only these two existing exact-answer paths bypass an external grader.
	// The account-state probe measures account state, not model answer quality.
	if judge != nil && cfg.Quality.Judge == nil && isBuiltinCandyPlan(&cfg) &&
		((cfg.Quality.Action == QualityActionOpenAITwoFAAdmission && judge.Reason == "builtin_admission") ||
			(plan.TriggerSource == quality5xxSource && judge.Reason == "builtin_candy")) {
		knownGrading = true
		grader.Kind, grader.Version = "builtin_exact", "quality-builtin-v1"
	}
	if knownGrading && !isOpenAICodexStateProbePlan(&cfg) {
		switch qualityOutcome([]*ScheduledTestResult{result}) {
		case "passed":
			m.Dimensions["answer_correctness"] = measuredQualityDimension("pass", "graded_correct", 1)
			m.Dimensions["grading"] = measuredQualityDimension("pass", "grading_completed", 1)
		case "failed":
			m.Dimensions["answer_correctness"] = measuredQualityDimension("fail", "graded_incorrect", 0)
			m.Dimensions["grading"] = measuredQualityDimension("pass", "grading_completed", 1)
		default:
			m.Dimensions["answer_correctness"] = QualityDimensionMeasurement{Status: "unknown", Reason: "grading_inconclusive"}
			if judge != nil && judge.Verdict == "unknown" && qualityGradingFailed(judge.Reason) {
				m.Dimensions["grading"] = measuredQualityDimension("fail", "grading_failed", 0)
			}
		}
	} else if cfg.Quality.Judge != nil {
		m.Dimensions["grading"] = QualityDimensionMeasurement{Status: "unknown", Reason: "grader_identity_not_observed"}
	}
	m.MeasurementHash = qualityMeasurementHash([]any{qualityMeasurementVersion, cfg.QuestionKind, cfg.ModelID,
		cfg.Prompt, cfg.ActualPrompt, cfg.RandomQuestion, cfg.PelicanQuestion, cfg.ReasoningEffort, cfg.TestChannel,
		cfg.Quality.ExpectedAnswer, cfg.Quality.Judge, grader, m.Scope})
	m.PolicyHash = qualityMeasurementHash([]any{qualityMeasurementVersion, cfg.Quality, cfg.ModelIDs, cfg.ParallelCount})
	if probe := qualityMeasurementProbe(result); probe != nil && probe.route != nil && probe.route.AccountID == plan.AccountID {
		evidence := *probe.route
		evidence.MeasurementHash, evidence.ObservedAt = m.MeasurementHash, m.ObservedAt
		if evidence.MatchesResult(result) {
			result.QualityRouteEvidence = &evidence
		}
	}
	if result.StartedAt.IsZero() || result.FinishedAt.Before(result.StartedAt) || m.MeasurementHash == "" || m.PolicyHash == "" {
		for dimension := range m.Dimensions {
			m.Dimensions[dimension] = QualityDimensionMeasurement{Status: "unknown", Reason: "invalid_measurement"}
		}
	}
	if validQualityEvidenceLifetime(m.ValidForSeconds) && qualityMeasurementProbe(result) != nil && qualityMeasuredAnswer(m) {
		deadline := m.ObservedAt.Add(time.Duration(m.ValidForSeconds) * time.Second)
		m.ValidUntil = &deadline
	}
}

func qualityGradingFailed(reason string) bool {
	switch reason {
	case "judge_request_failed", "judge_invalid_response", "judge_group_unavailable", "judge_accounts_unavailable",
		"judge_capacity_unavailable", "judge_no_available_account", "judge_jev_capacity", "judge_jev_keys_unavailable",
		"judge_jev_transport_unavailable", "judge_jev_timeout", "judge_jev_credentials_rejected",
		"judge_jev_rate_limited", "judge_jev_invalid_response", "judge_jev_request_failed":
		return true
	default:
		return false
	}
}
