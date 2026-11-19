package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestParsePreviewProcurementSources(t *testing.T) {
	for _, tc := range []struct{ ids, class string }{
		{"null", service.ProfitPreviewClassAdmitted}, {"[]", service.ProfitPreviewClassRejectedSource},
		{"[2]", service.ProfitPreviewClassAdmitted}, {"[3]", service.ProfitPreviewClassRejectedSource},
	} {
		input := `{"groups":[{"group":{"id":1,"platform":"openai","rate_multiplier":1,"profit_control_enabled":true,"procurement_policy":{"version":2,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":` + tc.ids + `}}},"accounts":[{"id":2,"platform":"openai","type":"apikey","rate_multiplier":0.02}]}]}`
		inputs, err := parsePreviewInputs([]byte(input), false)
		require.NoError(t, err)
		reports := service.PreviewProfitAdmission(inputs, time.Now())
		require.Equal(t, tc.class, reports[0].Verdicts[0].Class)
		require.Equal(t, inputs[0].Group.ProcurementPolicy.Sources, reports[0].ProcurementPolicy.Sources)
	}
}

func TestParsePreviewInputsProcurementInterval(t *testing.T) {
	input := `{"groups":[{"group":{"id":1,"platform":"openai","rate_multiplier":0.15,"profit_control_enabled":true,"procurement_policy":{"version":1,"revision":3,"min_rate":"0.02","max_rate":"0.03"}},"accounts":[{"id":2,"platform":"openai","type":"apikey","rate_multiplier":0.019999999999999997},{"id":3,"platform":"openai","type":"apikey","rate_multiplier":0.02},{"id":4,"platform":"openai","type":"apikey","rate_multiplier":0.03}],"models":["gpt-fixture"]}]}`
	inputs, err := parsePreviewInputs([]byte(input), false)
	require.NoError(t, err)
	require.NotNil(t, inputs[0].Group.ProcurementPolicy, "the real CLI input must not drop procurement before the shared gate")
	reports := service.PreviewProfitAdmission(inputs, time.Now())
	require.EqualValues(t, 3, reports[0].ProcurementPolicy.Revision)
	require.Equal(t, service.ProfitPreviewClassRejectedProcurement, reports[0].Verdicts[0].Class)
	require.Equal(t, service.ProfitPreviewClassAdmitted, reports[0].Verdicts[1].Class)
	require.Equal(t, service.ProfitPreviewClassRejectedProcurement, reports[0].Verdicts[2].Class)
	require.Equal(t, 1, reports[0].RemainingByModel["gpt-fixture"])
	inputs[0].Group.ProcurementPolicy.Version = 2
	reports = service.PreviewProfitAdmission(inputs, time.Now())
	require.Equal(t, service.ProfitPreviewClassUnknownProcurement, reports[0].Verdicts[1].Class)
}

func TestParsePreviewInputsPreservesFinalRequestScopes(t *testing.T) {
	input := `{"groups":[{"group":{"id":1,"platform":"openai","rate_multiplier":1,"profit_control_enabled":true},"accounts":[{"id":2,"platform":"openai","type":"apikey","rate_multiplier":0.2}],"request_scopes":[{"account_id":2,"public_model":"public-fixture","scope":{"version":2,"protocol":"responses_http","model":"wire-fixture","stream":false,"service_tier":"priority","reasoning_effort":"high","capability":"text"}}]}]}`
	inputs, err := parsePreviewInputs([]byte(input), false)
	require.NoError(t, err)
	reports := service.PreviewProfitAdmission(inputs, time.Now())
	raw, err := json.Marshal(reports[0])
	require.NoError(t, err)
	var report map[string]any
	require.NoError(t, json.Unmarshal(raw, &report))
	require.NotEmpty(t, report["scope_verdicts"], "explicit final scopes must not disappear into account-only counts")
}

func TestParsePreviewInputsPreservesResponsesOutputScope(t *testing.T) {
	input := `{"groups":[{"group":{"id":1,"platform":"openai","rate_multiplier":1,"profit_control_enabled":true},"accounts":[{"id":2,"platform":"openai","type":"apikey","rate_multiplier":0.2}],"request_scopes":[{"account_id":2,"public_model":"public-fixture","scope":{"version":3,"protocol":"responses_http","model":"wire-fixture","stream":true,"service_tier":"priority","reasoning_effort":"high","reasoning_summary":"auto","include_encrypted_content":true,"capability":"text"}}]}]}`
	inputs, err := parsePreviewInputs([]byte(input), false)
	require.NoError(t, err)
	require.Equal(t, "auto", inputs[0].RequestScopes[0].Scope.ReasoningSummary)
	require.True(t, inputs[0].RequestScopes[0].Scope.IncludeEncryptedContent)
}

func TestParsePreviewInputsRejectsAmbiguousRequestScopes(t *testing.T) {
	for _, scope := range []string{
		`{"account_id":2,"public_model":"fixture","scope":{"version":2,"protocol":"responses_http","model":"wire","service_tier":"priority","reasoning_effort":"high","capability":"text","unknown_price_option":true}}`,
		`{"account_id":99,"public_model":"fixture","scope":{"version":2,"protocol":"responses_http","model":"wire","service_tier":"priority","reasoning_effort":"high","capability":"text"}}`,
		`{"account_id":2,"public_model":"fixture","PUBLIC_MODEL":"fixture","scope":{"version":2,"protocol":"responses_http","model":"wire","service_tier":"priority","reasoning_effort":"high","capability":"text"}}`,
	} {
		input := `{"groups":[{"group":{"id":1,"platform":"openai"},"accounts":[{"id":2}],"request_scopes":[` + scope + `]}]}`
		_, err := parsePreviewInputs([]byte(input), false)
		require.Error(t, err, "unknown options or an absent target account are not an exact preview")
	}
	var err error
	_, err = service.ParseProfitPreviewRequestScope([]byte(`{"account_id":2,"public_model":"fixture","scope":{"version":2,"protocol":"responses_http","model":"wire","capability":"text","model":"other"}}`))
	require.Error(t, err, "differing duplicate selectors cannot choose the billed model")
	_, err = parsePreviewInputs([]byte(`{"groups":[{"group":{"id":1,"platform":"openai"},"accounts":[{"id":2},{"id":2}],"request_scopes":[{"account_id":2,"public_model":"fixture","scope":{}}]}]}`), false)
	require.ErrorContains(t, err, "ambiguous account")
}

func TestParsePreviewInputsIgnoresNullUserOverride(t *testing.T) {
	raw := []byte(`{
		"groups": [{
			"group": {
				"id": 50,
				"name": "preview",
				"platform": "openai",
				"rate_multiplier": 0.5,
				"subscription_type": "standard",
				"profit_control_enabled": false,
				"profit_min_margin": 0.1,
				"profit_safety_buffer": 0
			},
			"accounts": [{
				"id": 1,
				"name": "cheap",
				"platform": "openai",
				"type": "apikey",
				"rate_multiplier": 0.2
			}],
			"user_overrides": {"40": null, "41": 0.4},
			"models": ["gpt-test"]
		}]
	}`)

	inputs, err := parsePreviewInputs(raw, true)
	require.NoError(t, err)
	require.Len(t, inputs, 1)
	require.Equal(t, map[int64]float64{41: 0.4}, inputs[0].UserOverrides)
	require.True(t, inputs[0].AssumeEnabled)

	report := service.PreviewProfitAdmission(inputs, time.Date(2026, 1, 15, 8, 30, 0, 0, time.UTC))[0]
	require.InDelta(t, 0.4, report.MinEffectiveD, 1e-12, "null 覆盖不能被解码成 0 倍率")
	require.InDelta(t, 0.36, report.ThresholdMinD, 1e-12)
}

func TestParsePreviewInputsRejectsEmptyGroups(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`{"groups":null}`),
		[]byte(`{"groups":[]}`),
	} {
		inputs, err := parsePreviewInputs(raw, false)
		require.ErrorContains(t, err, "input contains no groups")
		require.Nil(t, inputs)
	}
}

func TestParsePreviewInputsPreservesAccountSaleRate(t *testing.T) {
	inputs, err := parsePreviewInputs([]byte(`{"groups":[{"group":{"id":1,"platform":"openai","rate_multiplier":1,"profit_control_enabled":true},"accounts":[{"id":2,"platform":"openai","type":"apikey","rate_multiplier":0.2,"group_rate_multiplier":2}],"models":["gpt-test"]}]}`), false)
	require.NoError(t, err)
	require.Len(t, inputs, 1)
	require.Len(t, inputs[0].Accounts, 1)
	require.NotNil(t, inputs[0].Accounts[0].GroupRateMultiplier)
	require.Equal(t, 2.0, *inputs[0].Accounts[0].GroupRateMultiplier)
	report := service.PreviewProfitAdmission(inputs, time.Now())[0]
	require.Equal(t, "rejected_account_sale_rate_mismatch", report.Verdicts[0].Class)
	require.Zero(t, report.RemainingByModel["gpt-test"])
}

// TestModelsWithZeroRemainingWarnings 钉死两档 D 的归零告警分工：
// 默认 D 归零由 modelsWithZeroRemaining 报告；默认 D 仍有账号但最低有效 D
// （分组内存在更低的用户级倍率覆盖）归零的模型必须单独告警——那些用户的该
// 模型会全黑，只看默认 D 完全看不出来。两档都为 0 时不重复告警。
func TestModelsWithZeroRemainingWarnings(t *testing.T) {
	report := service.ProfitPreviewGroupReport{
		RemainingByModel: map[string]int{
			"both-zero":      0,
			"min-d-zero":     2,
			"healthy":        3,
			"min-d-zero-alt": 1,
		},
		RemainingByModelMinD: map[string]int{
			"both-zero":      0,
			"min-d-zero":     0,
			"healthy":        3,
			"min-d-zero-alt": 0,
		},
	}

	if got := modelsWithZeroRemaining(report); len(got) != 1 || got[0] != "both-zero" {
		t.Fatalf("默认D归零告警应只覆盖 both-zero，got %v", got)
	}

	got := modelsWithZeroRemainingUnderMinD(report)
	want := []string{"min-d-zero", "min-d-zero-alt"}
	if len(got) != len(want) {
		t.Fatalf("最低有效D归零告警不符: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("最低有效D归零告警不符（应按模型名排序）: got %v want %v", got, want)
		}
	}
}

func TestParsePreviewProcurementBuckets(t *testing.T) {
	raw := `{"procurement_catalog":{"version":1,"revision":2,"buckets":[{"code":"low","name":"Low","min_rate":"0.02","max_rate":"0.03"}]},"groups":[{"group":{"id":1,"platform":"openai","rate_multiplier":1,"profit_control_enabled":true,"procurement_policy":{"version":3,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":null},"buckets":{"codes":["low"]}}},"accounts":[{"id":2,"platform":"openai","type":"apikey","rate_multiplier":0.025}]}]}`
	inputs, err := parsePreviewInputs([]byte(raw), false)
	require.NoError(t, err)
	reports := service.PreviewProfitAdmission(inputs, time.Now())
	require.Equal(t, service.ProfitPreviewClassAdmitted, reports[0].Verdicts[0].Class)
	require.EqualValues(t, 2, *reports[0].ProcurementCatalogRevision)
	inputs[0].Group.ProcurementCatalog = nil
	reports = service.PreviewProfitAdmission(inputs, time.Now())
	require.Equal(t, service.ProfitPreviewClassUnknownCatalog, reports[0].Verdicts[0].Class)
}
