//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type judgeDependencyFailure struct{}

func (judgeDependencyFailure) ListSchedulableByGroupID(context.Context, int64) ([]Account, error) {
	return nil, errors.New("synthetic-private-database-error")
}
func (judgeDependencyFailure) AcquireAccountSlot(context.Context, int64, int) (*AcquireResult, error) {
	return nil, errors.New("synthetic-private-redis-error")
}

func TestQualityJudgeDependencyMissingInventoryIsUnknown(t *testing.T) {
	for _, scenario := range []string{"group_unavailable", "model_not_allowed", "accounts_unavailable", "input_too_large", "capacity_unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			svc, cfg, _, _ := judgeFixture()
			answer := "21"
			switch scenario {
			case "group_unavailable":
				svc.groups = judgeGroupsStub{}
			case "model_not_allowed":
				svc.groups = judgeGroupsStub{&Group{ID: 8, Status: StatusActive, ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"other-model"}}}}
			case "accounts_unavailable":
				svc.accounts = judgeDependencyFailure{}
			case "capacity_unavailable":
				svc.slots = judgeDependencyFailure{}
			case "input_too_large":
				answer = strings.Repeat("x", 64001)
			}
			result := svc.Judge(t.Context(), 1, cfg, answer)
			require.NotNil(t, result.Dependency)
			require.Equal(t, "judge_"+scenario, result.Dependency.Reason)
			require.Equal(t, "blocked", result.Dependency.Status)
			require.Zero(t, result.Dependency.RequestCalls)
			if scenario == "capacity_unavailable" {
				require.Equal(t, 2, *result.Dependency.CandidateAccounts)
				require.Equal(t, 1, result.Dependency.SlotChecks)
			} else {
				require.Nil(t, result.Dependency.CandidateAccounts, "an unread inventory is not an empty inventory")
			}
			raw, err := json.Marshal(result)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "synthetic-private")
		})
	}
}

func TestQualityJudgeDependencyBudgetIsNotProviderFailure(t *testing.T) {
	for _, scope := range []string{"key", "key_minute", "site", "site_minute", "global_day", "system"} {
		t.Run(scope, func(t *testing.T) {
			svc, cfg, _, _ := judgeFixture()
			calls := 0
			svc.request = func(context.Context, int64, string, string) (string, error) {
				calls++
				if calls == 1 {
					return "", &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: scope, RetryAt: time.Now().Add(time.Minute)}
				}
				return `{"verdict":"correct","reason":"equal"}`, nil
			}
			result := svc.Judge(t.Context(), 1, cfg, "21")
			d := result.Dependency
			require.NotNil(t, d)
			require.Zero(t, d.FailedCalls)
			require.Equal(t, 1, d.BudgetDeferredCalls)
			if scope == "global_day" || scope == "system" {
				require.Equal(t, 1, d.RequestCalls)
				require.Equal(t, "blocked", d.Status)
				require.Equal(t, "judge_budget_deferred", d.Reason)
				require.NotNil(t, result.BudgetDeferral)
			} else {
				require.Equal(t, 2, d.RequestCalls)
				require.Equal(t, "responded", d.Status)
				require.Nil(t, result.BudgetDeferral, "a recovered call keeps the historical deferral count, not an active denial")
			}
		})
	}
}

func TestQualityJudgeDependencyRuleIsolationIsNotIndependence(t *testing.T) {
	for _, action := range []string{"remove_groups", QualityActionIsolateModels, QualityActionObserveOnly, "disable_scheduling"} {
		t.Run(action, func(t *testing.T) {
			svc, cfg, _, _ := judgeFixture()
			cfg.Quality.Action, cfg.Quality.RemoveGroupIDs = action, []int64{8}
			svc.request = func(context.Context, int64, string, string) (string, error) {
				return `{"verdict":"correct","reason":"equal"}`, nil
			}
			result := svc.Judge(t.Context(), 1, cfg, "21")
			require.Equal(t, action == "remove_groups" || action == QualityActionIsolateModels, result.Dependency.RuleRemovesGradingGroup)
			require.Equal(t, "not_established", result.Dependency.Independence, "a different group/account/URL is never proof of independent supply")
		})
	}
	_, cfg, _, _ := judgeFixture()
	svc := &QualityJudgeService{}
	require.Nil(t, svc.Judge(t.Context(), 1, nil, "21").Dependency)
	cfg.Quality.Judge.Kind = "jev"
	require.Nil(t, svc.Judge(t.Context(), 1, cfg, "21").Dependency, "an external grader must not invent ordinary-pool counts")
}

func TestQualityJudgeDependencyRuntimeObservation(t *testing.T) {
	for _, scenario := range []string{"responded", "incorrect", "uncertain", "no_candidates", "all_busy", "request_failed", "invalid_response", "failover", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			svc, cfg, accounts, slots := judgeFixture()
			ctx := t.Context()
			calls := 0
			svc.request = func(context.Context, int64, string, string) (string, error) {
				calls++
				switch scenario {
				case "request_failed":
					return "", errors.New("synthetic-private-endpoint")
				case "invalid_response":
					return "synthetic-private-response", nil
				case "failover":
					if calls == 1 {
						return "", errors.New("first route failed")
					}
				case "incorrect":
					return `{"verdict":"incorrect","reason":"not equal"}`, nil
				case "uncertain":
					return `{"verdict":"unknown","reason":"ambiguous"}`, nil
				}
				return `{"verdict":"correct","reason":"equal"}`, nil
			}
			switch scenario {
			case "no_candidates":
				accounts.accounts = accounts.accounts[:1] // Tested account is not its own grader.
			case "all_busy":
				slots.busy[2], slots.busy[3] = true, true
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			result := svc.Judge(ctx, 1, cfg, "21")
			raw, err := json.Marshal(result)
			require.NoError(t, err)
			d := gjson.GetBytes(raw, "dependency")
			require.True(t, d.Exists(), "ordinary grading must expose actual dependency observations")
			require.EqualValues(t, 1, d.Get("version").Int())
			require.Equal(t, "not_established", d.Get("independence").String())
			require.True(t, d.Get("self_grading_excluded").Bool())
			require.False(t, d.Get("rule_removes_grading_group").Bool())
			require.EqualValues(t, calls, d.Get("request_calls").Int(), "adapter calls are not physical/billed requests")
			require.EqualValues(t, len(slots.acquired), d.Get("slot_checks").Int())
			require.NotContains(t, string(raw), "synthetic-private")
			switch scenario {
			case "responded", "incorrect", "failover":
				require.Equal(t, "responded", d.Get("status").String())
			case "uncertain":
				require.Equal(t, "inconclusive", d.Get("status").String())
			default:
				require.Equal(t, "blocked", d.Get("status").String())
			}
			if scenario == "no_candidates" {
				require.EqualValues(t, 0, d.Get("candidate_accounts").Int())
			} else {
				require.EqualValues(t, 2, d.Get("candidate_accounts").Int())
			}
			if scenario == "all_busy" {
				require.EqualValues(t, 2, d.Get("busy_accounts").Int())
			}
			if scenario == "failover" {
				require.EqualValues(t, 1, d.Get("failed_calls").Int())
			}
			acquired := 0
			for _, id := range slots.acquired {
				if !slots.busy[id] {
					acquired++
				}
			}
			require.Len(t, slots.released, acquired, "only acquired slots are released")
		})
	}
}
