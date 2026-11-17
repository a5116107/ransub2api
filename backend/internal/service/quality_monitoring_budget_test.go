//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
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
	read  func()
}

func (b qualityBudgetCloseBody) Read(p []byte) (int, error) {
	if b.read != nil {
		b.read()
	}
	return b.ReadCloser.Read(p)
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

func qualityBudgetWSServer(t *testing.T, beforeReply ...func(int32)) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	handshakes, frames := &atomic.Int32{}, &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handshakes.Add(1)
		if r.Header.Get("Authorization") != "Bearer fixture" {
			http.Error(w, "invalid quality budget fixture credential", http.StatusUnauthorized)
			return
		}
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		for {
			_, _, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			n := frames.Add(1)
			if len(beforeReply) != 0 {
				beforeReply[0](n)
			}
			if err := conn.Write(r.Context(), coderws.MessageText, []byte(`{"type":"response.output_text.delta","delta":"21"}`)); err != nil {
				return
			}
			message := `{"type":"response.completed","response":{"id":"resp_budget_` + strconv.Itoa(int(n)) + `","status":"completed","model":"target","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"21"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`
			if err := conn.Write(r.Context(), coderws.MessageText, []byte(message)); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return server, handshakes, frames
}

func TestQualityMonitoringBudgetNativeWSPhysicalAttempts(t *testing.T) {
	for _, tc := range []struct {
		name               string
		denyAt             int
		prewarm            bool
		handshakes, frames int32
		acquires, releases int
	}{
		{"denied_handshake", 1, false, 0, 0, 1, 0},
		{"denied_answer", 2, false, 1, 0, 2, 1},
		{"answer", 0, false, 1, 1, 2, 2},
		{"denied_prewarm", 2, true, 1, 0, 2, 1},
		{"denied_after_prewarm", 3, true, 1, 1, 3, 2},
		{"prewarm_and_answer", 0, true, 1, 2, 3, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, handshakes, frames := qualityBudgetWSServer(t)
			tests, gateway, account := availabilityWSTestServices(t, server)
			gateway.cfg.Gateway.OpenAIWS.PrewarmGenerateEnabled = tc.prewarm
			denial := &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "global_day", RetryAt: time.Now().Add(time.Hour)}
			repo := &monitoringBudgetRepoStub{denyAt: tc.denyAt, err: denial}
			tests.SetMonitoringBudget(NewMonitoringProbeBudgetService(repo))
			result, err := tests.RunPelicanBackground(t.Context(), account.ID, "target", &PelicanTestConfig{
				QuestionKind: "candy", Prompt: CandyPrompt, ReasoningEffort: "high", ParallelCount: 1,
				Availability: &AvailabilityPolicy{ValidForSeconds: 60},
			})
			require.NoError(t, err)
			if tc.denyAt > 0 {
				require.Equal(t, "answer_budget_deferred", result.ErrorMessage)
				require.Equal(t, "unknown", result.QualityJudgment.Verdict)
				require.Equal(t, denial, result.QualityJudgment.BudgetDeferral)
				require.Equal(t, "inconclusive", result.PelicanConfig.AvailabilityResult.Status)
			} else {
				require.Equal(t, "success", result.Status, result.ErrorMessage)
				require.Nil(t, result.QualityJudgment)
			}
			require.Equal(t, tc.handshakes, handshakes.Load())
			require.Equal(t, tc.frames, frames.Load())
			require.Len(t, repo.requests, tc.acquires)
			require.Equal(t, tc.releases, repo.released)
			for _, request := range repo.requests {
				require.Equal(t, monitoringProbeHash(server.URL), request.SiteHash)
				require.Equal(t, []string{config.UpstreamKeyFingerprint("fixture")}, request.KeyHashes)
				require.False(t, request.Recovery)
			}
		})
	}
}

func TestQualityMonitoringBudgetNativeWSHoldsTurnSlot(t *testing.T) {
	entered, proceed := make(chan struct{}), make(chan struct{})
	server, handshakes, frames := qualityBudgetWSServer(t, func(int32) { close(entered); <-proceed })
	defer close(proceed)
	tests, _, account := availabilityWSTestServices(t, server)
	repo := &monitoringBudgetRepoStub{}
	tests.SetMonitoringBudget(NewMonitoringProbeBudgetService(repo))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan *ScheduledTestResult, 1)
	go func() {
		result, _ := tests.RunPelicanBackground(ctx, account.ID, "target", &PelicanTestConfig{QuestionKind: "candy", Prompt: CandyPrompt})
		done <- result
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("the admitted turn did not reach the upstream")
	}
	repo.mu.Lock()
	acquired, released := len(repo.requests), repo.released
	repo.mu.Unlock()
	require.Equal(t, 2, acquired, "one handshake plus one generation attempt")
	require.Equal(t, 1, released, "the handshake is complete; the generation slot must still be held")
	require.EqualValues(t, 1, handshakes.Load())
	require.EqualValues(t, 1, frames.Load())
	cancel()
	select {
	case result := <-done:
		require.NotEqual(t, "success", result.Status)
	case <-time.After(2 * time.Second):
		t.Fatal("a canceled monitoring request must stop without detached draining")
	}
	repo.mu.Lock()
	released = repo.released
	repo.mu.Unlock()
	require.Equal(t, 2, released, "cancellation returns the remaining generation slot")
}

type qualityBudgetWSDialer func(context.Context, string, http.Header, string) (openAIWSClientConn, int, http.Header, error)

func (dial qualityBudgetWSDialer) Dial(ctx context.Context, target string, headers http.Header, proxy string) (openAIWSClientConn, int, http.Header, error) {
	return dial(ctx, target, headers, proxy)
}

func TestQualityMonitoringBudgetNativeWSLeaseLoss(t *testing.T) {
	for _, phase := range []string{"handshake", "prewarm", "answer"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tests, gateway, account := availabilityWSTestServices(t, &httptest.Server{URL: "https://fixture.invalid"})
				gateway.cfg.Gateway.OpenAIWS.PrewarmGenerateEnabled = phase == "prewarm"
				gateway.cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 300
				gateway.cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 300
				repo := &monitoringBudgetRepoStub{renew: func(context.Context) (bool, error) { return false, nil }}
				tests.SetMonitoringBudget(NewMonitoringProbeBudgetService(repo))
				conn := &openAIWSCaptureConn{events: [][]byte{[]byte(`{"type":"response.completed"}`)}, readDelays: []time.Duration{100 * time.Second}}
				dials := 0
				gateway.getOpenAIWSConnPool().setClientDialerForTest(qualityBudgetWSDialer(func(ctx context.Context, _ string, _ http.Header, _ string) (openAIWSClientConn, int, http.Header, error) {
					dials++
					require.True(t, HTTPUpstreamRedirectsDisabled(ctx))
					if phase == "handshake" {
						// Even a dialer that returns a connection after losing its
						// lease must not publish that socket or a passing result.
						time.Sleep(41 * time.Second)
					}
					return conn, 0, nil, nil
				}))
				started := time.Now()
				result, err := tests.RunPelicanBackground(t.Context(), account.ID, "target", &PelicanTestConfig{QuestionKind: "candy", Prompt: CandyPrompt})
				require.NoError(t, err)
				require.Equal(t, "answer_budget_deferred", result.ErrorMessage)
				require.Equal(t, "unknown", result.QualityJudgment.Verdict)
				require.Equal(t, "budget_unavailable", result.QualityJudgment.BudgetDeferral.Reason)
				require.Equal(t, "inconclusive", result.PelicanConfig.AvailabilityResult.Status)
				require.Less(t, time.Since(started), 45*time.Second, "local lease loss must not enter the business drain/reconnect path")
				require.NoError(t, t.Context().Err(), "the downstream parent is still valid")
				require.Equal(t, 1, dials)
				require.True(t, conn.closed)
				require.Equal(t, len(repo.requests), repo.released)
			})
		})
	}
}

func TestQualityMonitoringBudgetNativeWSFrozenCredentialsAndReuse(t *testing.T) {
	repo := &monitoringBudgetRepoStub{}
	ctx := NewMonitoringProbeBudgetService(repo).Context(t.Context(), "https://fixture.invalid/tenant", false)
	pool := newOpenAIWSConnPool(newOpenAIWSExecutionScopeTestConfig())
	t.Cleanup(pool.Close)
	dialer := &openAIWSCaptureDialer{conn: &openAIWSCaptureConn{}}
	pool.setClientDialerForTest(dialer)
	req := openAIWSAcquireRequest{Account: &Account{ID: 42}, WSURL: "wss://fixture.invalid/tenant/v1/responses?key=query-fixture",
		Headers: http.Header{"Authorization": {"Bearer old-fixture"}},
		HeadersFactory: func(_ context.Context, h http.Header) (http.Header, error) {
			h.Set("Authorization", "Bearer final-fixture")
			return h, nil
		}}
	lease, err := pool.Acquire(ctx, req)
	require.NoError(t, err)
	connID := lease.ConnID()
	for turn := 0; turn < 2; turn++ {
		turnCtx, finish, err := lease.beginMonitoringProbeRequest(ctx)
		require.NoError(t, err)
		require.NoError(t, finishMonitoringProbeWSRequest(turnCtx, finish))
		require.NoError(t, ctx.Err())
		require.False(t, lease.conn.isClosed(), "finishing a turn must not close the reusable socket")
		lease.Release()
		if turn == 0 {
			lease, err = pool.Acquire(ctx, req)
			require.NoError(t, err)
			require.Equal(t, connID, lease.ConnID())
		}
	}
	require.Equal(t, 1, dialer.DialCount())
	require.Len(t, repo.requests, 3, "one handshake and two independently accounted turns")
	require.Equal(t, 3, repo.released)
	for _, request := range repo.requests {
		require.Equal(t, monitoringProbeHash("https://fixture.invalid/tenant"), request.SiteHash)
		require.ElementsMatch(t, []string{config.UpstreamKeyFingerprint("final-fixture"), config.UpstreamKeyFingerprint("query-fixture")}, request.KeyHashes)
		require.NotContains(t, request.KeyHashes, config.UpstreamKeyFingerprint("old-fixture"))
	}
}

func TestQualityMonitoringBudgetNativeWSBackgroundPrewarm(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(strconv.FormatBool(denied), func(t *testing.T) {
			repo := &monitoringBudgetRepoStub{}
			ctx, cancel := context.WithCancel(NewMonitoringProbeBudgetService(repo).Context(t.Context(), "https://fixture.invalid/tenant", false))
			defer cancel()
			pool := newOpenAIWSConnPool(newOpenAIWSExecutionScopeTestConfig())
			t.Cleanup(pool.Close)
			dialer := qualityBudgetWSDialer(func(ctx context.Context, _ string, _ http.Header, _ string) (openAIWSClientConn, int, http.Header, error) {
				require.True(t, HTTPUpstreamRedirectsDisabled(ctx))
				return &openAIWSCaptureConn{}, 0, nil, nil
			})
			pool.setClientDialerForTest(dialer)
			req := openAIWSAcquireRequest{Account: &Account{ID: 42}, WSURL: "wss://fixture.invalid/tenant/v1/responses", Headers: http.Header{"Authorization": {"Bearer final-fixture"}}}
			lease, err := pool.Acquire(ctx, req)
			require.NoError(t, err)
			lease.Release()
			ap, ok := pool.getAccountPool(42)
			require.True(t, ok)
			ap.mu.Lock()
			background := cloneOpenAIWSAcquireRequest(*ap.lastAcquire)
			generation := ap.generation
			ap.mu.Unlock()
			if denied {
				repo.denyAt, repo.err = 2, &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "global_day", RetryAt: time.Now().Add(time.Hour)}
			}
			cancel()
			pool.prewarmConns(42, background, 1, generation)
			require.Len(t, repo.requests, 2, "the delayed dial must retain its originating budget")
			require.Nil(t, monitoringProbeLocalError(ctx), "background results must not mutate an already completed quality test")
			ap.mu.Lock()
			failures := ap.prewarmFails
			ap.mu.Unlock()
			require.Zero(t, failures, "a local deferral is not an upstream failure")
			if denied {
				require.Equal(t, 1, repo.released)
			} else {
				require.Equal(t, 2, repo.released)
			}
		})
	}
}

func TestQualityMonitoringBudgetBPSPhysicalAttempts(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(strconv.FormatBool(denied), func(t *testing.T) {
			repo := &monitoringBudgetRepoStub{}
			if denied {
				repo.denyAt, repo.err = 1, &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "global_day", RetryAt: time.Now().Add(time.Hour)}
			}
			upstream := &httpUpstreamRecorder{resp: qualityBudgetReply("21")}
			gateway := openAIClientToolsTestService(upstream)
			gateway.httpUpstream = monitoringBudgetHTTP{base: upstream}
			account := excelAccount()
			// A native Codex base does not change the fixed BPS destination.
			account.Credentials["base_url"] = "https://native.example/tenant"
			tests := &AccountTestService{cfg: gateway.cfg, accountRepo: &stateProbeAccountRepo{account: account}, httpUpstream: gateway.httpUpstream, openaiGatewayService: gateway}
			tests.SetMonitoringBudget(NewMonitoringProbeBudgetService(repo))
			result, err := tests.RunPelicanBackground(t.Context(), account.ID, "gpt-6-astra", bpsObservationConfig())
			require.NoError(t, err)
			if denied {
				require.Equal(t, "answer_budget_deferred", result.ErrorMessage)
				require.Equal(t, "unknown", result.QualityJudgment.Verdict)
				require.Equal(t, repo.err, result.QualityJudgment.BudgetDeferral)
				require.Empty(t, upstream.requests)
				require.Zero(t, repo.released)
			} else {
				require.Equal(t, "success", result.Status, result.ErrorMessage)
				require.Equal(t, "21", result.ResponseText)
				require.Len(t, upstream.requests, 1)
				require.True(t, HTTPUpstreamRedirectsDisabled(upstream.requests[0].Context()))
				require.Equal(t, []string{config.UpstreamKeyFingerprint("test-token")}, repo.requests[0].KeyHashes)
				require.Equal(t, 1, repo.released)
			}
			require.Len(t, repo.requests, 1)
			require.Equal(t, monitoringProbeHash("https://bps.openai.com"), repo.requests[0].SiteHash)
			require.False(t, repo.requests[0].Recovery)
			require.True(t, account.IsExcelBPSEnabled())
			require.True(t, account.Schedulable)
		})
	}
}

func TestQualityMonitoringBudgetBPSLeaseLossIsLocal(t *testing.T) {
	for _, phase := range []string{"read", "close"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				repo := &monitoringBudgetRepoStub{renew: func(context.Context) (bool, error) { return false, nil }}
				response := qualityBudgetReply("21")
				delay := sync.OnceFunc(func() { time.Sleep(41 * time.Second); synctest.Wait() })
				body := qualityBudgetCloseBody{ReadCloser: response.Body, close: func() {}}
				if phase == "read" {
					body.read = delay
				} else {
					body.close = delay
				}
				response.Body = body
				upstream := &httpUpstreamRecorder{resp: response}
				gateway := openAIClientToolsTestService(upstream)
				gateway.httpUpstream = monitoringBudgetHTTP{base: upstream}
				ctx := NewMonitoringProbeBudgetService(repo).Context(t.Context(), "https://bps.openai.com", false)
				ctx = withPelicanTestOptions(ctx, pelicanTestOptions{observeOnly: true})
				c, _ := newTestContext()
				c.Request = c.Request.WithContext(ctx)
				_, err := gateway.Forward(ctx, c, excelAccount(), []byte(`{"model":"gpt-6-astra","stream":true,"input":"Return 21"}`))
				require.ErrorIs(t, err, ErrMonitoringProbeBudgetLeaseLost)
				require.ErrorIs(t, monitoringProbeLocalError(ctx), ErrMonitoringProbeBudgetLeaseLost)
				require.NoError(t, ctx.Err(), "lease loss only cancels its budget child")
				_, fault := c.Get(OpsUpstreamErrorsKey)
				require.False(t, fault, "local lease loss must not be recorded as an upstream stream failure")
				require.Len(t, upstream.requests, 1)
				require.Len(t, repo.requests, 1)
				require.Equal(t, 1, repo.released)
			})
		})
	}
}

func TestQualityMonitoringBudgetBPSRetriesAndRepairs(t *testing.T) {
	for _, phase := range []string{"encrypted_retry", "tool_repair", "unknown_tool"} {
		for _, denied := range []bool{false, true} {
			t.Run(phase+"/denied="+strconv.FormatBool(denied), func(t *testing.T) {
				repo := &monitoringBudgetRepoStub{}
				if denied {
					repo.denyAt, repo.err = 2, &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "global_day", RetryAt: time.Now().Add(time.Hour)}
				}
				first := newJSONResponse(http.StatusOK, excelBPSRepairWire(t, "original", "Run client tool", "text(42);"))
				second := newJSONResponse(http.StatusOK, excelBPSRepairWire(t, "fixed", "codex2api.custom/functions.exec", "text(42);"))
				body := []byte(`{"model":"gpt-6-astra","stream":true,"input":"Return a tool call","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]}]}`)
				switch phase {
				case "encrypted_retry":
					body = excelBPSEncryptedHistoryRequest(true)
					first = newJSONResponse(http.StatusBadRequest, excelBPSInvalidCiphertext)
					second = qualityBudgetReply("21")
				case "unknown_tool":
					body = []byte(`{"model":"gpt-6-astra","stream":false,"input":"Return a tool call","tools":[{"type":"function","name":"shell"}]}`)
					first = newJSONResponse(http.StatusOK, excelBPSRepairWire(t, "original", "Run", `{"name":"missing","arguments":{}}`))
					second = newJSONResponse(http.StatusOK, excelBPSRepairWire(t, "fixed", "Run", `{"name":"shell","arguments":{"cmd":"fixture"}}`))
				}
				upstream := &httpUpstreamRecorder{responses: []*http.Response{first, second}}
				gateway := openAIClientToolsTestService(upstream)
				gateway.httpUpstream = monitoringBudgetHTTP{base: upstream}
				ctx := NewMonitoringProbeBudgetService(repo).Context(t.Context(), "https://bps.openai.com", false)
				ctx = withPelicanTestOptions(ctx, pelicanTestOptions{observeOnly: true})
				c, recorder := newTestContext()
				c.Request = c.Request.WithContext(ctx)
				_, err := gateway.Forward(ctx, c, excelAccount(), body)
				if denied {
					require.ErrorIs(t, err, repo.err)
					require.Len(t, upstream.requests, 1, "a deferred correction must not send or try another route")
					require.Equal(t, 1, repo.released)
					require.NotContains(t, recorder.Body.String(), "response.completed")
				} else {
					require.NoError(t, err)
					require.Len(t, upstream.requests, 2)
					require.Equal(t, 2, repo.released)
				}
				require.Len(t, repo.requests, 2, "each original and retry/correction is a physical attempt")
				for _, request := range repo.requests {
					require.Equal(t, monitoringProbeHash("https://bps.openai.com"), request.SiteHash)
					require.Equal(t, []string{config.UpstreamKeyFingerprint("test-token")}, request.KeyHashes)
					require.False(t, request.Recovery)
				}
			})
		}
	}
}
