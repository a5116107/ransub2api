package service

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProcurementMonitoringRecipeNativePlan(t *testing.T) {
	at := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	valid := func() *QualityRulePreset {
		return &QualityRulePreset{ID: 7, Name: "Observe supplied models", ModelID: "model-a", CronExpression: "0 * * * *", Enabled: true, MaxResults: 100,
			PelicanConfig: &PelicanTestConfig{QuestionKind: "candy", Prompt: "Return one", ReasoningEffort: "medium", ParallelCount: 2,
				ModelIDs: []string{"model-a", "model-b"}, Quality: &QualityPolicy{Action: QualityActionObserveOnly, ExpectedAnswer: "1",
					Judge: &QualityJudgeConfig{Kind: "llm", GroupID: 8, ModelID: "grader", Prompt: "Grade values"}}}}
	}
	input := valid()
	before, err := json.Marshal(input)
	require.NoError(t, err)
	plan, err := ProcurementMonitoringRecipePlan(input, at)
	require.NoError(t, err)
	require.Equal(t, at.Add(time.Hour), *plan.NextRunAt)
	require.True(t, plan.Enabled)
	require.Zero(t, plan.ID)
	require.Equal(t, []string{"model-a", "model-b"}, plan.PelicanConfig.ModelIDs)
	plan.PelicanConfig.ModelIDs[0] = "mutated"
	after, err := json.Marshal(input)
	require.NoError(t, err)
	require.JSONEq(t, string(before), string(after))
	t.Run("random question observation retains native validation", func(t *testing.T) {
		preset := valid()
		preset.PelicanConfig.QuestionKind = QualityRandomQuestionKind
		preset.PelicanConfig.Prompt = ""
		preset.PelicanConfig.Quality.ExpectedAnswer = ""
		plan, err := ProcurementMonitoringRecipePlan(preset, at)
		require.NoError(t, err)
		require.Equal(t, QualityRandomQuestionKind, plan.PelicanConfig.QuestionKind)
		require.Equal(t, at.Add(time.Hour), *plan.NextRunAt)
	})
	for _, kind := range []string{"disabled", "action", "probe", "bps", "bad-cron", "invalid-samples", "nil-config", "nil-policy", "nil", "empty-action"} {
		t.Run(kind, func(t *testing.T) {
			preset := valid()
			switch kind {
			case "disabled":
				preset.Enabled = false
			case "action":
				preset.PelicanConfig.Quality.Action = "remove_groups"
			case "probe":
				preset.PelicanConfig.QuestionKind = OpenAICodexStateProbeQuestionKind
			case "bps":
				preset.PelicanConfig.TestChannel = "bps"
			case "bad-cron":
				preset.CronExpression = "bad"
			case "invalid-samples":
				preset.PelicanConfig.ParallelCount = 9
			case "nil-config":
				preset.PelicanConfig = nil
			case "nil-policy":
				preset.PelicanConfig.Quality = nil
			case "nil":
				preset = nil
			case "empty-action":
				preset.PelicanConfig.Quality.Action = ""
			}
			_, err := ProcurementMonitoringRecipePlan(preset, at)
			require.ErrorIs(t, err, ErrProcurementMonitoringRecipeInvalid)
		})
	}
}
