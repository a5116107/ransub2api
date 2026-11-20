package service

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMonitoringProbePlanDraftParsing(t *testing.T) {
	valid := `{"plan_id":0,"account_id":42,"model_id":"model-a","cron_expression":"0 * * * *","enabled":true,"auto_recover":false,"max_results":50}`
	draft, err := ParseMonitoringProbePlanDraft([]byte(`{"plan_draft":` + valid + `}`))
	require.NoError(t, err)
	require.EqualValues(t, 42, draft.AccountID)
	old, err := ParseMonitoringProbePlanDraft([]byte(`{"revision":0,"enabled":false,"legacy":true}`))
	require.NoError(t, err)
	require.Nil(t, old, "settings-only payloads retain their contract")
	for name, raw := range map[string]string{
		"null": `{"plan_draft":null}`, "array": `{"plan_draft":[]}`, "empty": `{"plan_draft":{}}`,
		"duplicate draft": `{"plan_draft":` + valid + `,"plan_draft":` + valid + `}`,
		"duplicate field": `{"plan_draft":` + strings.Replace(valid, `"enabled":true`, `"enabled":true,"enabled":true`, 1) + `}`,
		"unknown field":   `{"plan_draft":` + strings.Replace(valid, `"plan_id":0`, `"plan_id":0,"future":1`, 1) + `}`,
		"null boolean":    `{"plan_draft":` + strings.Replace(valid, `"enabled":true`, `"enabled":null`, 1) + `}`,
		"null ID":         `{"plan_draft":` + strings.Replace(valid, `"plan_id":0`, `"plan_id":null`, 1) + `}`,
		"unsafe ID":       `{"plan_draft":` + strings.Replace(valid, `"plan_id":0`, `"plan_id":9007199254740992`, 1) + `}`,
		"bad cron":        `{"plan_draft":` + strings.Replace(valid, `0 * * * *`, `bad`, 1) + `}`,
		"empty model":     `{"plan_draft":` + strings.Replace(valid, `model-a`, ` `, 1) + `}`,
		"null config":     `{"plan_draft":` + strings.TrimSuffix(valid, "}") + `,"pelican_config":null}}`,
		"multiple values": `{"plan_draft":` + valid + `} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseMonitoringProbePlanDraft([]byte(raw))
			require.ErrorIs(t, err, ErrMonitoringProbePlanDraft)
		})
	}
}

func TestMonitoringProbePlanDraftNativeValidationAndIsolation(t *testing.T) {
	draft := &MonitoringProbePlanDraft{AccountID: 42, ModelID: "model-a", CronExpression: "*/30 * * * *", MaxResults: 100, Enabled: true,
		PelicanConfig: &PelicanTestConfig{QuestionKind: "candy", Prompt: "Answer one", ReasoningEffort: "medium", ParallelCount: 2,
			ModelIDs: []string{"model-a", "model-a", "model-b"}, Quality: &QualityPolicy{Action: QualityActionObserveOnly, ExpectedAnswer: "1"}}}
	before, err := json.Marshal(draft)
	require.NoError(t, err)
	at := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	plan, err := draft.Plan(at)
	require.NoError(t, err)
	require.Equal(t, at.Add(30*time.Minute), *plan.NextRunAt)
	require.Equal(t, []string{"model-a", "model-b"}, plan.PelicanConfig.ModelIDs)
	after, err := json.Marshal(draft)
	require.NoError(t, err)
	require.JSONEq(t, string(before), string(after), "native normalization must not mutate the draft")
	p := DefaultMonitoringProbeBudgetSettings()
	c := CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{}, nil, true, []MonitoringProbeCoveragePlan{{Plan: *plan}}, true, nil, true, nil, true, at)
	require.EqualValues(t, 192, c.ScheduledSamples)
	require.EqualValues(t, 192, c.MinimumScheduledRequests)
	draft.PelicanConfig.Quality.Judge = &QualityJudgeConfig{Kind: "jev", Prompt: "Compare the answer"}
	plan, err = draft.Plan(at)
	require.NoError(t, err)
	c = CompileMonitoringProbeCoverage(p, UpstreamBillingProbeSettings{}, nil, true, []MonitoringProbeCoveragePlan{{Plan: *plan}}, true, nil, true, nil, true, at)
	require.EqualValues(t, 192, c.MinimumJudgeRequests)
	require.EqualValues(t, 384, c.MinimumScheduledRequests, "external grading is part of the same physical call lower bound")
	draft.PelicanConfig.ParallelCount = 9
	_, err = draft.Plan(at)
	require.ErrorIs(t, err, ErrMonitoringProbePlanDraft)
}
