package service

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProcurementRecipeConfirmationCommand(t *testing.T) {
	raw := `{"version":1,"preset_id":7,"observed_at":"2026-11-21T08:00:00Z","fingerprint":"` + strings.Repeat("a", 64) + `"}`
	command, err := ParseProcurementRecipeCommand([]byte(raw))
	require.NoError(t, err)
	require.EqualValues(t, 7, command.PresetID)
	require.NoError(t, command.Validate())
	var empty *ProcurementRecipeCommand
	require.ErrorIs(t, empty.Validate(), ErrProcurementRecipeCommand)
	for _, invalid := range []string{"null", "[]", "{}", raw + "{}", strings.Replace(raw, `"version":1`, `"version":2`, 1),
		strings.Replace(raw, `"version":1`, `"version":1,"version":1`, 1), strings.Replace(raw, `"preset_id":7`, `"preset_id":null`, 1),
		strings.Replace(raw, `"preset_id":7`, `"preset_id":9007199254740992`, 1), strings.Replace(raw, `"preset_id":7`, `"preset_id":1.5`, 1),
		strings.Replace(raw, `"observed_at":"2026-11-21T08:00:00Z"`, `"observed_at":null`, 1),
		strings.Replace(raw, strings.Repeat("a", 64), strings.Repeat("z", 64), 1), strings.Replace(raw, `"preset_id":7`, `"preset_id":7,"account_ids":[1]`, 1)} {
		_, err := ParseProcurementRecipeCommand([]byte(invalid))
		require.ErrorIs(t, err, ErrProcurementRecipeCommand)
	}
}

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
