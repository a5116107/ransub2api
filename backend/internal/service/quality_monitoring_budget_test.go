//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func qualityBudgetTestService(repo *monitoringBudgetRepoStub, responses ...*http.Response) (*AccountTestService, *queuedHTTPUpstream) {
	upstream := &queuedHTTPUpstream{responses: responses}
	account := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 2,
		Credentials: map[string]any{"api_key": "quality-budget-fixture", "base_url": "https://quality.example/tenant/v1"}}
	tests := &AccountTestService{cfg: &config.Config{}, httpUpstream: monitoringBudgetHTTP{base: upstream},
		accountRepo: &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{2: account}}}}
	tests.SetMonitoringBudget(NewMonitoringProbeBudgetService(repo))
	return tests, upstream
}

func qualityBudgetReply(answer string) *http.Response {
	response := newJSONResponse(http.StatusOK, "")
	response.Header.Set("Content-Type", "text/event-stream")
	response.Body = io.NopCloser(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":" + strconv.Quote(answer) + "}\n\ndata: {\"type\":\"response.completed\"}\n\n"))
	return response
}

func TestQualityMonitoringBudgetPreservesLocalErrorThroughSSE(t *testing.T) {
	denial := &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "global_day", RetryAt: time.Now().Add(time.Hour)}
	repo := &monitoringBudgetRepoStub{denyAt: 1, err: denial}
	tests, upstream := qualityBudgetTestService(repo)
	ctx := NewMonitoringProbeBudgetService(repo).Context(t.Context(), "https://quality.example/tenant", false)
	_, err := tests.runQualityJudgeRequest(ctx, 2, "test-model", "compare")
	var got *MonitoringProbeBudgetDenial
	require.ErrorAs(t, err, &got, "SSE must not erase the local budget error identity")
	require.Equal(t, denial, got)
	require.Empty(t, upstream.requests)
}

func TestQualityMonitoringBudgetAnswerDeferral(t *testing.T) {
	denial := &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "global_day", RetryAt: time.Now().Add(time.Hour)}
	repo := &monitoringBudgetRepoStub{denyAt: 1, err: denial}
	tests, upstream := qualityBudgetTestService(repo)
	ctx := NewMonitoringProbeBudgetService(repo).Context(t.Context(), "https://quality.example/tenant", false)
	plan := qualityAnswerRetryPlan()
	plan.AccountID = 2
	runner := &ScheduledTestRunnerService{runPelican: tests.RunPelicanBackground, judgeQuality: func(context.Context, int64, *PelicanTestConfig, string) *QualityJudgment {
		t.Fatal("an answer blocked locally must not invoke a grader")
		return nil
	}}
	result := runner.runPelicanSample(ctx, plan)
	require.Equal(t, "inconclusive", qualityOutcome([]*ScheduledTestResult{result}))
	require.NotNil(t, result.QualityJudgment, "persist a structured local deferral in the existing result JSON")
	require.Equal(t, "answer_budget_deferred", result.QualityJudgment.Reason)
	require.Equal(t, denial, result.QualityJudgment.BudgetDeferral)
	require.Equal(t, "inconclusive", result.PelicanConfig.AvailabilityResult.Status)
	require.Empty(t, upstream.requests)
	require.Len(t, repo.requests, 1, "local budget denial must not retry the same answer")
}

func TestQualityMonitoringBudgetLLMCountsAndStopsBeforeGlobalLimit(t *testing.T) {
	denial := &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "global_day", RetryAt: time.Now().Add(time.Hour)}
	repo := &monitoringBudgetRepoStub{denyAt: 1, err: denial}
	tests, upstream := qualityBudgetTestService(repo, qualityBudgetReply(`{"verdict":"correct","reason":"equal"}`))
	judge, cfg, accounts, slots := judgeFixture()
	accounts.accounts[1].Platform, accounts.accounts[1].Type = PlatformOpenAI, AccountTypeAPIKey
	accounts.accounts[1].Credentials = map[string]any{"api_key": "quality-budget-fixture", "base_url": "https://quality.example/tenant/v1"}
	judge.monitoringBudget = NewMonitoringProbeBudgetService(repo)
	judge.request = tests.runQualityJudgeRequest
	got := judge.Judge(t.Context(), 1, cfg, "21")
	require.Equal(t, "unknown", got.Verdict)
	require.Equal(t, "judge_budget_deferred", got.Reason)
	require.Equal(t, denial, got.BudgetDeferral)
	require.Empty(t, upstream.requests)
	require.Equal(t, slots.acquired, slots.released)
	require.Len(t, repo.requests, 1, "global exhaustion ends the grading operation")
}

func TestQualityMonitoringBudgetLLMFailoverAndFinalCredentials(t *testing.T) {
	for _, scope := range []string{"upstream_503", "global_day", "key_minute", "site", "system"} {
		t.Run(scope, func(t *testing.T) {
			denial := &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: scope, RetryAt: time.Now().Add(time.Minute)}
			repo := &monitoringBudgetRepoStub{acquire: func(_ MonitoringProbeBudgetRequest, n int) (string, error) {
				if scope == "global_day" && n == 2 || (scope == "key_minute" || scope == "site" || scope == "system") && n == 1 {
					return "", denial
				}
				return "fixture-lease", nil
			}}
			responses := []*http.Response{qualityBudgetReply(`{"verdict":"correct","reason":"equal"}`)}
			if scope == "upstream_503" || scope == "global_day" {
				responses = append([]*http.Response{newJSONResponse(503, `{"error":"busy"}`)}, responses...)
			}
			tests, upstream := qualityBudgetTestService(repo, responses...)
			loaded := tests.accountRepo.(*openAIAccountTestRepo).accountsByID
			second := *loaded[2]
			second.ID = 3
			second.Credentials = map[string]any{"api_key": "second-quality-fixture", "base_url": "https://second.example/another/v1"}
			loaded[3] = &second
			judge, cfg, accounts, slots := judgeFixture()
			// Selection snapshots deliberately differ: the physical transport
			// must use the reloaded account and final outgoing credential.
			accounts.accounts[1].Credentials = map[string]any{"base_url": "https://stale.example", "api_key": "stale-fixture"}
			judge.monitoringBudget, judge.request = NewMonitoringProbeBudgetService(repo), tests.runQualityJudgeRequest
			got := judge.Judge(t.Context(), 1, cfg, "21")
			require.Equal(t, slots.acquired, slots.released)
			for _, request := range repo.requests {
				require.False(t, request.Recovery, "grader failover cannot consume billing recovery reserves")
			}
			switch scope {
			case "global_day", "system":
				require.Equal(t, "judge_budget_deferred", got.Reason)
				require.Equal(t, "unknown", got.Verdict)
				require.Equal(t, denial, got.BudgetDeferral)
				require.Len(t, upstream.requests, len(repo.requests)-1)
			default:
				require.Equal(t, "correct", got.Verdict)
				require.Nil(t, got.BudgetDeferral)
				require.EqualValues(t, 3, got.AccountID)
				require.Len(t, repo.requests, 2)
				require.Equal(t, monitoringProbeHash("https://quality.example/tenant"), repo.requests[0].SiteHash)
				require.Equal(t, monitoringProbeHash("https://second.example/another"), repo.requests[1].SiteHash)
				require.Equal(t, config.UpstreamCredentialFingerprints(upstream.requests[len(upstream.requests)-1]), repo.requests[1].KeyHashes)
			}
			require.Equal(t, len(upstream.requests), repo.released, "only admitted physical attempts hold a lease")
		})
	}
}

func TestQualityMonitoringBudgetAnswerRetriesCountEachPhysicalAttempt(t *testing.T) {
	for rejects := 0; rejects < 3; rejects++ {
		t.Run(strconv.Itoa(rejects), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				repo := &monitoringBudgetRepoStub{}
				responses := make([]*http.Response, 0, rejects+1)
				for range rejects {
					responses = append(responses, newJSONResponse(503, `{"error":"busy"}`))
				}
				responses = append(responses, qualityBudgetReply("21"))
				tests, upstream := qualityBudgetTestService(repo, responses...)
				plan := qualityAnswerRetryPlan()
				plan.AccountID = 2
				runner := &ScheduledTestRunnerService{runPelican: tests.RunPelicanBackground, judgeQuality: func(context.Context, int64, *PelicanTestConfig, string) *QualityJudgment {
					return &QualityJudgment{Verdict: "correct"}
				}}
				result := runner.runPelicanSample(t.Context(), plan)
				require.Equal(t, "passed", qualityOutcome([]*ScheduledTestResult{result}))
				require.Len(t, upstream.requests, rejects+1)
				require.Len(t, repo.requests, rejects+1)
				require.Equal(t, rejects+1, repo.released)
				for _, request := range repo.requests {
					require.False(t, request.Recovery)
					require.Equal(t, monitoringProbeHash("https://quality.example/tenant"), request.SiteHash)
				}
			})
		})
	}
}

func TestQualityMonitoringBudgetUnavailableAndUnmarkedConnection(t *testing.T) {
	for _, cause := range []error{ErrMonitoringProbeBudgetUnavailable, errors.New("PRIVATE_DATABASE_FAILURE")} {
		repo := &monitoringBudgetRepoStub{denyAt: 1, err: cause}
		tests, upstream := qualityBudgetTestService(repo, qualityBudgetReply("21"))
		plan := qualityAnswerRetryPlan()
		plan.AccountID = 2
		runner := &ScheduledTestRunnerService{runPelican: tests.RunPelicanBackground}
		result := runner.runPelicanSample(t.Context(), plan)
		require.Equal(t, "answer_budget_deferred", result.ErrorMessage)
		require.Equal(t, "budget_unavailable", result.QualityJudgment.BudgetDeferral.Reason)
		require.NotContains(t, result.ErrorMessage, "PRIVATE")
		require.Empty(t, upstream.requests)
		// The connection endpoint is outside quality generation; merely wiring
		// this service must not put unmarked traffic into the monitoring budget.
		c, _ := newTestContext()
		require.NoError(t, tests.TestAccountConnection(c, 2, "test-model", "hi", AccountTestModeDefault))
		require.Len(t, upstream.requests, 1)
		require.Len(t, repo.requests, 1)
	}
}

func TestQualityMonitoringBudgetDeferralNeverChangesAccountQuality(t *testing.T) {
	for _, phase := range []string{"answer", "llm"} {
		t.Run(phase, func(t *testing.T) {
			plan, result := qualityMeasurementFixture()
			plan.PelicanConfig.Quality.EvidenceValidForSeconds = 120
			plan.PelicanConfig.Quality.Judge = &QualityJudgeConfig{Kind: "llm", GroupID: 8, ModelID: "fixture-model", Prompt: "compare"}
			judgment := &QualityJudgment{Verdict: "unknown", Reason: phase + "_budget_deferred",
				BudgetDeferral: &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "global_day", RetryAt: time.Now().Add(time.Minute)}}
			if phase == "llm" {
				judgment.Kind, judgment.Reason, judgment.RulesVersion = "llm", "judge_budget_deferred", qualityLLMRulesVersion
				judgment.GroupID, judgment.ModelID = 8, "fixture-model"
			}
			applyQualityJudgment(result, judgment)
			recordQualityMeasurement(plan, result)
			require.Nil(t, result.PelicanConfig.QualityMeasurement.ValidUntil)
			require.Equal(t, "unknown", result.PelicanConfig.QualityMeasurement.Dimensions["grading"].Status)
			require.Equal(t, "unknown", result.PelicanConfig.QualityMeasurement.Dimensions["answer_correctness"].Status)
			plans, results := &qualityPlanRepo{}, &pelicanResults{}
			runner := &ScheduledTestRunnerService{planRepo: plans, scheduledSvc: NewScheduledTestService(plans, results)}
			runner.runPelican = func(context.Context, int64, string, *PelicanTestConfig) (*ScheduledTestResult, error) {
				if phase == "answer" {
					sample := *result
					config := *result.PelicanConfig
					sample.PelicanConfig = &config
					return &sample, nil
				}
				return &ScheduledTestResult{Status: "success", ResponseText: "42"}, nil
			}
			runner.judgeQuality = func(context.Context, int64, *PelicanTestConfig, string) *QualityJudgment { return judgment }
			scheduled := pelicanPlan()
			scheduled.PelicanConfig.QuestionKind = "candy"
			scheduled.PelicanConfig.Quality = plan.PelicanConfig.Quality
			require.True(t, runner.runOnePlan(t.Context(), scheduled))
			require.Equal(t, []string{"inconclusive"}, plans.outcomes)
			require.NotEmpty(t, results.results)
			for _, saved := range results.results {
				require.Equal(t, judgment.BudgetDeferral, saved.QualityJudgment.BudgetDeferral)
			}
		})
	}
}

type qualityBudgetCloseBody struct {
	io.ReadCloser
	close func()
}

func (b qualityBudgetCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.close()
	return err
}

func TestQualityMonitoringBudgetLeaseLostOnCloseIsNotSuccessfulSSE(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		response := qualityBudgetReply("21")
		// Lose only the real budget lease's child context via its renewal.
		// Canceling the caller instead would correctly prohibit all SSE writes.
		response.Body = qualityBudgetCloseBody{ReadCloser: response.Body, close: func() {
			time.Sleep(41 * time.Second)
			synctest.Wait()
		}}
		repo := &monitoringBudgetRepoStub{renew: func(context.Context) (bool, error) { return false, nil }}
		tests, upstream := qualityBudgetTestService(repo, response)
		c, recorder := newTestContext()
		c.Request = c.Request.WithContext(t.Context())
		err := tests.TestPelicanAccountConnection(c, 2, "fixture-model", "answer", "medium")
		require.ErrorIs(t, err, ErrMonitoringProbeBudgetLeaseLost)
		require.NoError(t, c.Request.Context().Err(), "budget loss must not cancel the downstream caller")
		_, message := parsePelicanOutput(recorder.Body.String())
		require.NotEmpty(t, message, "the interactive SSE consumer must also see the lease loss after the body closed")
		require.Len(t, upstream.requests, 1)
		require.Equal(t, 1, repo.released)
	})
}

func TestQualityMonitoringBudgetAdaptiveNativeBases(t *testing.T) {
	for _, platform := range []string{PlatformKimi, PlatformDeepseek, PlatformZhipu, PlatformMiniMax} {
		for _, bases := range []string{"separate_hosts", "tenant_paths", "defaults"} {
			t.Run(platform+"/"+bases, func(t *testing.T) {
				account := adaptiveCNAccountTestAccount(2, platform)
				if bases == "defaults" {
					delete(account.Credentials, "api_base_urls")
				} else if bases == "tenant_paths" {
					account.Credentials["api_base_urls"] = map[string]any{
						APIProtocolChatCompletions: "https://quality.example/chat/v1",
						APIProtocolAnthropic:       "https://quality.example/anthropic",
						APIProtocolResponses:       "https://quality.example/responses/v1",
					}
				}
				account.Credentials["api_key"] = "final-quality-fixture"
				responses := []*http.Response{adaptiveCNChatTestResponse(), adaptiveCNAnthropicTestResponse()}
				protocols := []string{APIProtocolChatCompletions, APIProtocolAnthropic}
				if account.SupportsNativeCNResponses() {
					responses = append(responses, adaptiveCNResponsesTestResponse())
					protocols = append(protocols, APIProtocolResponses)
				}
				tests, upstream := adaptiveCNAccountTestService(account, responses...)
				repo := &monitoringBudgetRepoStub{}
				tests.httpUpstream = monitoringBudgetHTTP{base: tests.httpUpstream}
				tests.SetMonitoringBudget(NewMonitoringProbeBudgetService(repo))
				c, _ := newTestContext()
				require.NoError(t, tests.TestPelicanAccountConnection(c, 2, account.providerDefaultTestModel(), "answer", "medium"))
				require.Len(t, upstream.requests, len(protocols))
				require.Len(t, repo.requests, len(protocols))
				require.Equal(t, len(protocols), repo.released)
				for i, protocol := range protocols {
					site, err := CanonicalNewAPISite(account.GetCNProtocolBaseURL(protocol))
					require.NoError(t, err)
					require.Equal(t, monitoringProbeHash(site), repo.requests[i].SiteHash)
					require.Equal(t, config.UpstreamCredentialFingerprints(upstream.requests[i]), repo.requests[i].KeyHashes)
					credential := upstream.requests[i].Header.Get("Authorization")
					if protocol == APIProtocolAnthropic {
						credential = upstream.requests[i].Header.Get("x-api-key")
					}
					require.Contains(t, credential, "final-quality-fixture")
					require.False(t, repo.requests[i].Recovery)
				}
			})
		}
	}
}

func TestQualityMonitoringBudgetAdaptiveDenialStopsAnswer(t *testing.T) {
	for _, at := range []int{2, 3} {
		t.Run(strconv.Itoa(at), func(t *testing.T) {
			account := adaptiveCNAccountTestAccount(2, PlatformKimi)
			tests, upstream := adaptiveCNAccountTestService(account, adaptiveCNChatTestResponse(), adaptiveCNAnthropicTestResponse(), adaptiveCNResponsesTestResponse())
			denial := &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "global_day", RetryAt: time.Now().Add(time.Hour)}
			repo := &monitoringBudgetRepoStub{denyAt: at, err: denial}
			tests.httpUpstream = monitoringBudgetHTTP{base: tests.httpUpstream}
			tests.SetMonitoringBudget(NewMonitoringProbeBudgetService(repo))
			plan := qualityAnswerRetryPlan()
			plan.AccountID = 2
			runner := &ScheduledTestRunnerService{runPelican: tests.RunPelicanBackground, judgeQuality: func(context.Context, int64, *PelicanTestConfig, string) *QualityJudgment {
				t.Fatal("a partially completed answer deferred locally must not invoke a grader")
				return nil
			}}
			result := runner.runPelicanSample(t.Context(), plan)
			require.Equal(t, "inconclusive", qualityOutcome([]*ScheduledTestResult{result}))
			require.Equal(t, "answer_budget_deferred", result.QualityJudgment.Reason)
			require.Equal(t, denial, result.QualityJudgment.BudgetDeferral)
			require.NotEmpty(t, result.ResponseText, "retain the answer already observed before local deferral")
			require.Len(t, upstream.requests, at-1)
			require.Len(t, repo.requests, at, "local deferral must not restart the multi-protocol answer")
			require.Equal(t, at-1, repo.released)
		})
	}
}
