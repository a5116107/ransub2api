//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	hcplugin "github.com/hashicorp/go-plugin"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type monitoringPluginServer struct {
	pluginv1.UnimplementedTransportPluginServer
	calls atomic.Int64
	body  string
}

func (s *monitoringPluginServer) Forward(stream grpc.BidiStreamingServer[pluginv1.ForwardRequest, pluginv1.ForwardResponse]) error {
	s.calls.Add(1)
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	for _, frame := range []*pluginv1.ForwardResponse{
		{Frame: &pluginv1.ForwardResponse_Start{Start: &pluginv1.ForwardResponseStart{
			StatusCode: http.StatusOK, Headers: map[string]*pluginv1.HeaderValues{"Content-Type": {Values: []string{"text/event-stream"}}},
			ContentLength: int64(len(s.body)),
		}}},
		{Frame: &pluginv1.ForwardResponse_BodyChunk{BodyChunk: []byte(s.body)}},
		{Frame: &pluginv1.ForwardResponse_End{End: &pluginv1.ForwardResponseEnd{}}},
	} {
		if err := stream.Send(frame); err != nil {
			return err
		}
	}
	return nil
}

// Exercise the existing RPC stream, request/body lifecycle and routing without
// launching a third-party plugin or sending anything to an external provider.
func monitoringPluginFixture(t *testing.T, answer string) (*PluginManager, *monitoringPluginServer, *pluginRuntime) {
	t.Helper()
	response := qualityBudgetReply(answer)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	api := &monitoringPluginServer{body: string(body)}
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	pluginv1.RegisterTransportPluginServer(server, api)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })
	connection, err := grpc.NewClient("passthrough:///monitoring-plugin-fixture",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = connection.Close() })
	runtime := &pluginRuntime{client: hcplugin.NewClient(&hcplugin.ClientConfig{}), api: pluginv1.NewTransportPluginClient(connection), done: make(chan struct{})}
	manager := &PluginManager{}
	manager.route.Store(&pluginRoute{pluginID: 1, runtime: runtime, rolloutPercent: 100})
	return manager, api, runtime
}

func monitoringPluginAccountTests(t *testing.T, answer string) (*AccountTestService, *monitoringPluginServer, *pluginRuntime, *monitoringBudgetRepoStub, *queuedHTTPUpstream, *Account) {
	t.Helper()
	manager, api, runtime := monitoringPluginFixture(t, answer)
	budget := &monitoringBudgetRepoStub{}
	tests, native := qualityBudgetTestService(budget)
	account := stateProbeAccount()
	account.ID, account.Status, account.Schedulable = 2, StatusActive, true
	tests.pluginManager = manager
	tests.accountRepo = &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{2: account}}}
	return tests, api, runtime, budget, native, account
}

func TestPluginMonitoringBudgetBoundary(t *testing.T) {
	for _, mode := range []string{"automatic", "manual_budget", "business", "automatic_without_budget", "quality_marker_only", "wire_spoof", "not_bound"} {
		t.Run(mode, func(t *testing.T) {
			manager, api, runtime := monitoringPluginFixture(t, "21")
			budget := &monitoringBudgetRepoStub{}
			ctx := t.Context()
			if mode != "business" && mode != "automatic_without_budget" {
				ctx = NewMonitoringProbeBudgetService(budget).qualityContext(ctx)
			}
			if mode == "automatic" || mode == "automatic_without_budget" || mode == "not_bound" {
				ctx = withAutomaticMonitoringProbe(ctx)
			}
			if mode == "quality_marker_only" {
				ctx = context.WithValue(ctx, qualityProbeContextKey{}, true)
			}
			if mode == "not_bound" {
				manager.route.Store(&pluginRoute{pluginID: 1, runtime: runtime, rolloutPercent: 0})
			}
			closed := 0
			body := qualityBudgetCloseBody{ReadCloser: io.NopCloser(strings.NewReader(`{"stream":true}`)), close: func() { closed++ }}
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://fixture.invalid/v1/responses", body)
			require.NoError(t, err)
			request.Header.Set("X-Automatic-Monitoring-Probe", "true")
			response, handled, err := manager.RoundTripOpenAIOAuth(ctx, request, "", stateProbeAccount())
			if mode == "automatic" {
				require.Nil(t, response)
				require.True(t, handled, "a deferred plugin must not fall through to native HTTP")
				denial := monitoringProbeDeferral(err, time.Now())
				require.NotNil(t, denial)
				require.Equal(t, "plugin_metering_unavailable", denial.Reason)
				require.Equal(t, "plugin", denial.Scope)
				require.ErrorIs(t, monitoringProbeLocalError(ctx), err, "SSE must retain the local typed result")
				require.Equal(t, 1, closed)
				require.Zero(t, api.calls.Load())
			} else if mode == "not_bound" {
				require.False(t, handled)
				require.Nil(t, response)
				require.NoError(t, err)
				require.Zero(t, api.calls.Load())
				require.NoError(t, request.Body.Close())
			} else {
				require.NoError(t, err)
				require.True(t, handled)
				require.NotNil(t, response)
				_, readErr := io.ReadAll(response.Body)
				require.NoError(t, readErr)
				require.NoError(t, response.Body.Close())
				require.EqualValues(t, 1, api.calls.Load(), "ordinary callers retain the existing plugin path")
				require.Nil(t, monitoringProbeLocalError(ctx))
			}
			require.Empty(t, budget.requests, "the host must not fabricate physical egress metering from one RPC")
			require.Zero(t, runtime.inFlight.Load())
			require.Same(t, runtime, manager.route.Load().runtime, "a local deferral is not a plugin health failure")
		})
	}
}

func TestPluginMonitoringBudgetScheduledConnectivity(t *testing.T) {
	tests, api, runtime, budget, native, account := monitoringPluginAccountTests(t, "21")
	before, err := json.Marshal(account)
	require.NoError(t, err)
	plans, results := &scheduledBudgetPlanRepo{}, &pelicanResults{}
	runner := NewScheduledTestRunnerService(plans, NewScheduledTestService(plans, results), tests, nil, &config.Config{})
	plan := &ScheduledTestPlan{ID: 11, AccountID: account.ID, ModelID: "gpt-6-astra", CronExpression: "* * * * *", MaxResults: 50, AutoRecover: true}
	require.True(t, runner.runOnePlan(t.Context(), plan))
	require.Len(t, results.results, 1)
	result := results.results[0]
	require.Equal(t, "deferred", result.Status)
	require.Equal(t, "plugin_metering_unavailable", scheduledBudgetDeferral(t, result).Reason)
	require.Nil(t, result.QualityJudgment)
	require.Zero(t, api.calls.Load())
	require.Zero(t, runtime.inFlight.Load())
	require.Empty(t, native.requests)
	require.Empty(t, budget.requests)
	after, err := json.Marshal(account)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))
	// Removing the plugin binding restores the original native budgeted path,
	// without reusing a previous round's local deferral.
	tests.pluginManager.route.Store(nil)
	native.responses = append(native.responses, scheduledBudgetReply())
	require.True(t, runner.runOnePlan(t.Context(), plan))
	require.Equal(t, "success", results.results[1].Status)
	require.Nil(t, results.results[1].BudgetDeferral)
	require.Len(t, native.requests, 1)
	require.Len(t, budget.requests, 1)
}

func TestPluginMonitoringBudgetScheduledQuality(t *testing.T) {
	tests, api, runtime, budget, native, account := monitoringPluginAccountTests(t, "21")
	before, err := json.Marshal(account)
	require.NoError(t, err)
	plan := qualityAnswerRetryPlan()
	plan.AccountID = account.ID
	runner := &ScheduledTestRunnerService{runPelican: tests.RunPelicanBackground, judgeQuality: func(context.Context, int64, *PelicanTestConfig, string) *QualityJudgment {
		t.Fatal("a deferred answer must not invoke a grader")
		return nil
	}}
	result := runner.runPelicanSample(t.Context(), plan)
	require.Equal(t, "failed", result.Status)
	require.Equal(t, "unknown", result.QualityJudgment.Verdict)
	require.Equal(t, "plugin_metering_unavailable", result.QualityJudgment.BudgetDeferral.Reason)
	require.Equal(t, "inconclusive", result.PelicanConfig.AvailabilityResult.Status)
	require.Equal(t, "inconclusive", qualityRoundOutcome([]*ScheduledTestResult{result}))
	require.False(t, retryableQualityAnswer(plan.PelicanConfig, result))
	require.Zero(t, api.calls.Load())
	require.Zero(t, runtime.inFlight.Load())
	require.Empty(t, native.requests)
	require.Empty(t, budget.requests)
	after, err := json.Marshal(account)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))
}

func TestPluginMonitoringBudgetAutomaticGraderDoesNotSwitchAccounts(t *testing.T) {
	tests, api, runtime, budget, native, account := monitoringPluginAccountTests(t, `{"verdict":"correct","reason":"same"}`)
	judge, cfg, accounts, slots := judgeFixture()
	accounts.accounts[1] = *account
	judge.monitoringBudget = tests.monitoringBudget
	judge.request = tests.runQualityJudgeRequest
	// The answer succeeded elsewhere; only the automatic grading request uses
	// this plugin. Its local deferral must not consume another grading account.
	runner := &ScheduledTestRunnerService{judgeQuality: judge.Judge, runPelican: func(context.Context, int64, string, *PelicanTestConfig) (*ScheduledTestResult, error) {
		return &ScheduledTestResult{Status: "success", ResponseText: "21"}, nil
	}}
	plan := &ScheduledTestPlan{AccountID: 1, ModelID: "fixture-model", PelicanConfig: cfg}
	result := runner.runPelicanSample(t.Context(), plan)
	require.Equal(t, "unknown", result.QualityJudgment.Verdict)
	require.Equal(t, "judge_budget_deferred", result.QualityJudgment.Reason)
	require.Equal(t, "plugin_metering_unavailable", result.QualityJudgment.BudgetDeferral.Reason)
	require.Equal(t, "inconclusive", qualityRoundOutcome([]*ScheduledTestResult{result}))
	require.Equal(t, []int64{account.ID}, slots.acquired)
	require.Equal(t, slots.acquired, slots.released)
	require.Zero(t, api.calls.Load())
	require.Zero(t, runtime.inFlight.Load())
	require.Empty(t, native.requests)
	require.Empty(t, budget.requests)
}

func TestPluginMonitoringBudgetGatewayDoesNotFallBack(t *testing.T) {
	manager, api, runtime := monitoringPluginFixture(t, "21")
	native := &pluginRoutingHTTPUpstream{}
	gateway := &OpenAIGatewayService{pluginManager: manager, httpUpstream: native}
	ctx := withAutomaticMonitoringProbe(NewMonitoringProbeBudgetService(nil).qualityContext(t.Context()))
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://fixture.invalid/v1/responses", strings.NewReader(`{"stream":true}`))
	require.NoError(t, err)
	response, err := gateway.doOpenAIUpstream(request, "", stateProbeAccount())
	require.Nil(t, response)
	require.True(t, IsMonitoringProbeBudgetFailure(err))
	require.Zero(t, native.doCalls)
	require.Zero(t, native.doWithTLSCalls)
	require.Zero(t, api.calls.Load())
	require.Zero(t, runtime.inFlight.Load())
	manager.route.Store(&pluginRoute{pluginID: 1, rolloutPercent: 100, unavailable: "fixture unavailable"})
	response, handled, err := manager.RoundTripOpenAIOAuth(ctx, nil, "", stateProbeAccount())
	require.True(t, handled)
	require.Nil(t, response)
	require.True(t, IsMonitoringProbeBudgetFailure(err), "unmetered automatic probes do not diagnose plugin health")
}

func TestPluginMonitoringBudgetConcurrentIsolation(t *testing.T) {
	manager, api, runtime := monitoringPluginFixture(t, "21")
	budget := NewMonitoringProbeBudgetService(nil)
	var wg sync.WaitGroup
	for i := range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := budget.qualityContext(t.Context())
			if i%2 == 0 {
				ctx = withAutomaticMonitoringProbe(ctx)
			}
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://fixture.invalid/v1/responses", nil)
			require.NoError(t, err)
			response, handled, err := manager.RoundTripOpenAIOAuth(ctx, request, "", stateProbeAccount())
			require.True(t, handled)
			if i%2 == 0 {
				require.True(t, IsMonitoringProbeBudgetFailure(err))
				require.Nil(t, response)
			} else {
				require.NoError(t, err)
				_, err = io.ReadAll(response.Body)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				require.Nil(t, monitoringProbeLocalError(ctx))
			}
		}()
	}
	wg.Wait()
	require.EqualValues(t, 12, api.calls.Load())
	require.Zero(t, runtime.inFlight.Load())
	require.Nil(t, monitoringProbeLocalError(t.Context()))
}

func TestPluginMonitoringBudgetGroupEntrypoints(t *testing.T) {
	for _, mode := range []string{"scheduled", "manual", "candy"} {
		t.Run(mode, func(t *testing.T) {
			tests, api, runtime, budget, native, account := monitoringPluginAccountTests(t, "<html></html>")
			plan := groupTestPlan(1)
			repo := newGroupTestRepoFake(plan)
			repo.due = []*PelicanGroupTestPlan{plan}
			router := &groupTestRouterFake{steps: []routeStep{{account: account}, {account: stateProbeAccount()}}}
			groups, showcase := newGroupTestService(repo, router, tests.RunPelicanBackground)
			var result *MonitoringProbeBudgetDenial
			if mode == "candy" {
				candyRepo := &candyRepoFake{channelMonitorV2RepoStub: channelMonitorV2RepoStub{config: candyConfigFixture()}}
				candyServiceFixture(candyRepo, groups).RunDue(t.Context(), groupTestNow)
				require.Len(t, candyRepo.results, 1)
				require.Equal(t, "deferred", candyRepo.results[0].Verdict)
				require.Empty(t, candyRepo.results[0].AnswerPreview)
				result = candyRepo.results[0].BudgetDeferral
			} else {
				if mode == "manual" {
					require.NoError(t, groups.RunNow(t.Context(), plan.ID))
					groups.runs.Wait()
				} else {
					groups.RunDue(t.Context(), groupTestNow)
				}
				require.Len(t, repo.results, 1)
				if mode == "manual" {
					require.Equal(t, "success", repo.results[0].Status, repo.results[0].ErrorMessage)
					require.Nil(t, repo.results[0].BudgetDeferral)
					require.EqualValues(t, 1, api.calls.Load())
				} else {
					require.Equal(t, "deferred", repo.results[0].Status)
					require.Empty(t, repo.results[0].ErrorMessage)
					require.Empty(t, repo.results[0].Attempts)
					result = repo.results[0].BudgetDeferral
				}
			}
			if mode != "manual" {
				require.NotNil(t, result)
				require.Equal(t, "plugin_metering_unavailable", result.Reason)
				require.Zero(t, api.calls.Load())
				require.Empty(t, showcase.published)
			}
			require.Len(t, router.excluded, 1, "a local plugin deferral must not try another account")
			require.Equal(t, []int64{account.ID}, router.released)
			require.Empty(t, native.requests)
			require.Empty(t, budget.requests)
			require.Zero(t, runtime.inFlight.Load())
		})
	}
}
