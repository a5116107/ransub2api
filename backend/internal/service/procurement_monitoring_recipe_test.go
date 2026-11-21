package service

import (
	"encoding/json"
	"fmt"
	"slices"
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

func sharedMonitoringPreset(models []string, expression string) *QualityRulePreset {
	return &QualityRulePreset{ModelID: models[0], CronExpression: expression, Enabled: true, MaxResults: 20,
		PelicanConfig: &PelicanTestConfig{QuestionKind: "candy", Prompt: "Return one", ReasoningEffort: "medium", ParallelCount: 2,
			ModelIDs: append([]string(nil), models...), Quality: &QualityPolicy{Action: QualityActionObserveOnly, ExpectedAnswer: "1"}}}
}

func TestProcurementSharedMonitoringCommand(t *testing.T) {
	raw := `{"version":1,"products":[{"product_id":2,"preset_id":3},{"product_id":4,"preset_id":3}]}`
	c, err := ParseProcurementSharedMonitoringCommand([]byte(raw))
	require.NoError(t, err)
	require.Len(t, c.Products, 2)
	var missing *ProcurementSharedMonitoringCommand
	require.ErrorIs(t, missing.Validate(), ErrProcurementSharedMonitoringCommand)
	for _, bad := range []string{"null", "[]", "{}", raw + "{}", strings.Replace(raw, `"version":1`, `"version":3`, 1),
		strings.Replace(raw, `"version":1`, `"version":1,"version":1`, 1), strings.Replace(raw, `"product_id":2`, `"product_id":2,"product_id":2`, 1),
		strings.Replace(raw, `"product_id":4`, `"product_id":2`, 1), strings.Replace(raw, `"preset_id":3`, `"preset_id":null`, 1),
		strings.Replace(raw, `"product_id":2`, `"product_id":9007199254740992`, 1), strings.Replace(raw, `"product_id":2`, `"product_id":1.5`, 1),
		`{"version":1,"products":[]}`, `{"version":1,"products":null}`, strings.Replace(raw, `"preset_id":3`, `"preset_id":3,"account_ids":[1]`, 1)} {
		_, err := ParseProcurementSharedMonitoringCommand([]byte(bad))
		require.ErrorIs(t, err, ErrProcurementSharedMonitoringCommand, bad)
	}
	for i := 0; i < 19; i++ {
		c.Products = append(c.Products, ProcurementMonitoringProductSelection{ProductID: int64(i + 10), PresetID: 3})
	}
	require.ErrorIs(t, c.Validate(), ErrProcurementSharedMonitoringCommand)
}

func TestProcurementSharedMonitoringPersistentCommands(t *testing.T) {
	for _, raw := range []string{`{"version":2,"products":[]}`, `{"version":2,"products":[],"release_ownership":true}`, `{"version":2,"products":[{"product_id":2,"preset_id":3}],"release_ownership":false}`} {
		_, err := ParseProcurementSharedMonitoringCommand([]byte(raw))
		require.NoError(t, err)
	}
	for _, raw := range []string{`{"version":2,"products":null}`, `{"version":2,"products":[],"release_ownership":null}`, `{"version":1,"products":[{"product_id":2,"preset_id":3}],"release_ownership":false}`, `{"version":2,"products":[{"product_id":2,"preset_id":3}],"release_ownership":true}`} {
		_, err := ParseProcurementSharedMonitoringCommand([]byte(raw))
		require.ErrorIs(t, err, ErrProcurementSharedMonitoringCommand, raw)
	}
	raw := `{"version":2,"products":[{"product_id":2,"preset_id":3}],"release_ownership":false,"revision":0,"observed_at":"2026-11-21T08:00:00Z","fingerprint":"` + strings.Repeat("a", 64) + `","confirm":true}`
	c, err := ParseProcurementSharedMonitoringApplyCommand([]byte(raw))
	require.NoError(t, err)
	require.NoError(t, c.Validate())
	encoded, err := json.Marshal(c)
	require.NoError(t, err)
	roundTrip, err := ParseProcurementSharedMonitoringApplyCommand(encoded)
	require.NoError(t, err)
	require.Equal(t, c, roundTrip)
	var empty *ProcurementSharedMonitoringApplyCommand
	require.ErrorIs(t, empty.Validate(), ErrProcurementSharedMonitoringCommand)
	for _, bad := range []string{raw + `{}`, strings.Replace(raw, `"revision":0`, `"revision":null`, 1), strings.Replace(raw, `"revision":0`, `"revision":-1`, 1), strings.Replace(raw, `"revision":0`, `"revision":9007199254740991`, 1), strings.Replace(raw, `"confirm":true`, `"confirm":false`, 1), strings.Replace(raw, `"confirm":true`, `"confirm":true,"confirm":true`, 1), strings.Replace(raw, `"release_ownership":false`, `"release_ownership":null`, 1), strings.Replace(raw, `"product_id":2`, `"product_id":2,"product_id":2`, 1), strings.Replace(raw, `"version":2`, `"version":1`, 1), strings.Replace(raw, strings.Repeat("a", 64), strings.Repeat("A", 64), 1), strings.Replace(raw, `"observed_at":"2026-11-21T08:00:00Z"`, `"observed_at":null`, 1), strings.Replace(raw, `"confirm":true`, `"confirm":true,"extra":1`, 1)} {
		_, err := ParseProcurementSharedMonitoringApplyCommand([]byte(bad))
		require.ErrorIs(t, err, ErrProcurementSharedMonitoringCommand, bad)
	}
}

func TestProcurementSharedMonitoringMergePreservesNativeRequirements(t *testing.T) {
	at := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	a := sharedMonitoringPreset([]string{"b", "a"}, "*/10 * * * *")
	b := sharedMonitoringPreset([]string{"c", "b"}, "*/5 * * * *")
	a.MaxResults, b.MaxResults = 4, 4
	inputs := []*QualityRulePreset{a, b}
	before, err := json.Marshal(inputs)
	require.NoError(t, err)
	plan, reason := MergeProcurementMonitoringRecipes(inputs, at)
	require.Equal(t, "compatible", reason)
	require.Equal(t, []string{"a", "b", "c"}, plan.PelicanConfig.ModelIDs)
	require.Equal(t, "a", plan.ModelID)
	require.Equal(t, 6, plan.MaxResults, "retain the full combined native quality round")
	require.Equal(t, at.Add(5*time.Minute), *plan.NextRunAt)
	reversed, reason := MergeProcurementMonitoringRecipes([]*QualityRulePreset{b, a}, at)
	require.Equal(t, "compatible", reason)
	require.Equal(t, plan, reversed)
	after, err := json.Marshal(inputs)
	require.NoError(t, err)
	require.Equal(t, before, after)
	plan.PelicanConfig.ModelIDs[0] = "changed"
	after, err = json.Marshal(inputs)
	require.NoError(t, err)
	require.Equal(t, before, after)
	for _, variant := range []string{"prompt", "effort", "parallel", "question", "lifetime", "timezone", "models", "samples", "invalid", "empty"} {
		t.Run(variant, func(t *testing.T) {
			a := sharedMonitoringPreset([]string{"a"}, "0 * * * *")
			b := sharedMonitoringPreset([]string{"b"}, "*/5 * * * *")
			inputs := []*QualityRulePreset{a, b}
			want := "observation_settings_conflict"
			switch variant {
			case "prompt":
				b.PelicanConfig.Prompt = "other"
			case "effort":
				b.PelicanConfig.ReasoningEffort = "high"
			case "parallel":
				b.PelicanConfig.ParallelCount = 1
			case "question":
				b.PelicanConfig.QuestionKind = QualityRandomQuestionKind
			case "lifetime":
				b.PelicanConfig.Quality.EvidenceValidForSeconds = 60
			case "timezone":
				a.CronExpression = "CRON_TZ=UTC 0 * * * *"
				b.CronExpression = "CRON_TZ=Asia/Hong_Kong 0 * * * *"
				want = "schedule_timezones_conflict"
			case "models":
				for i := 0; i < 26; i++ {
					a.PelicanConfig.ModelIDs = append(a.PelicanConfig.ModelIDs, fmt.Sprintf("a-%d", i))
					b.PelicanConfig.ModelIDs = append(b.PelicanConfig.ModelIDs, fmt.Sprintf("b-%d", i))
				}
				a.MaxResults, b.MaxResults = 100, 100
				want = "native_model_limit"
			case "samples":
				for i := 0; i < 7; i++ {
					a.PelicanConfig.ModelIDs = append(a.PelicanConfig.ModelIDs, fmt.Sprintf("a-%d", i))
					b.PelicanConfig.ModelIDs = append(b.PelicanConfig.ModelIDs, fmt.Sprintf("b-%d", i))
				}
				a.PelicanConfig.ParallelCount, b.PelicanConfig.ParallelCount = 8, 8
				a.MaxResults, b.MaxResults = 100, 100
				want = "native_model_limit"
			case "invalid":
				b.Enabled = false
				want = "invalid_preset"
			case "empty":
				inputs = nil
				want = "invalid_requirements"
			}
			p, reason := MergeProcurementMonitoringRecipes(inputs, at)
			require.Nil(t, p)
			require.Equal(t, want, reason)
		})
	}
}

func TestProcurementSharedMonitoringCronCoversEveryRequiredTick(t *testing.T) {
	for _, expressions := range [][]string{
		{"*/10 * * * *", "*/5 * * * *"},
		{"0 0 * * *", "30 12 * * *"},
		{"0 0 * * MON", "0 12 * * TUE"},
		{"0 0 1 * *", "0 12 * * MON"},
		{"0 0 1 * MON", "0 12 15 * FRI"},
		{"0 0 */2 * MON", "0 12 */3 * TUE"},
		{"0 0 */2 * */2", "0 12 */3 * */3"},
		{"0 0 29 2 *", "0 12 31 1,3,5,7,8,10,12 *"},
		{"CRON_TZ=America/New_York 30 1 * * *", "CRON_TZ=America/New_York 30 2 * * *"},
		{"CRON_TZ=Asia/Hong_Kong 0 0 1 * *", "CRON_TZ=Asia/Hong_Kong 0 12 * * MON"},
	} {
		t.Run(strings.Join(expressions, "+"), func(t *testing.T) {
			merged, ok := mergeProcurementMonitoringCron(expressions)
			require.True(t, ok)
			schedule, err := scheduledTestCronParser.Parse(merged)
			require.NoError(t, err)
			reversed := slices.Clone(expressions)
			slices.Reverse(reversed)
			other, ok := mergeProcurementMonitoringCron(reversed)
			require.True(t, ok)
			require.Equal(t, merged, other)
			for _, expr := range expressions {
				original, err := scheduledTestCronParser.Parse(expr)
				require.NoError(t, err)
				at := time.Date(2023, 12, 30, 0, 0, 0, 0, time.UTC)
				end := at.AddDate(5, 0, 0)
				ticks := 0
				for ticks < 20000 {
					at = original.Next(at)
					if at.IsZero() || !at.Before(end) {
						break
					}
					ticks++
					require.Equal(t, at, schedule.Next(at.Add(-time.Nanosecond)), "missing original tick %s of %s in %s", at, expr, merged)
				}
				require.Positive(t, ticks)
			}
		})
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
