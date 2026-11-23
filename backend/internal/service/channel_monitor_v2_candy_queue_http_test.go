//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The real repository returns a fresh Account value for each GetByID. The
// generic test double returns one pointer, including its mutable lazy caches.
type candyQueueAccountRepository struct{ AccountRepository }

func (r candyQueueAccountRepository) GetByID(ctx context.Context, id int64) (*Account, error) {
	account, err := r.AccountRepository.GetByID(ctx, id)
	if err != nil || account == nil {
		return account, err
	}
	copy := *account
	return &copy, nil
}

func candyQueueLocalHTTP(t *testing.T, upstream *httptest.Server) HTTPUpstream {
	t.Helper()
	local, err := url.Parse(upstream.URL)
	require.NoError(t, err)
	// Only the network destination changes: keep the native request shape,
	// final credentials and the original physical-budget boundary.
	return monitoringBudgetHTTP{base: &codexModelsHTTPUpstreamStub{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
		forward := req.Clone(req.Context())
		forward.URL.Scheme, forward.URL.Host, forward.Host = local.Scheme, local.Host, local.Host
		return upstream.Client().Do(forward)
	}}}
}

func TestChannelMonitorV2CandyPhysicalBudgetFairRecovery(t *testing.T) {
	const capacity = 4
	var attempts atomic.Int32
	budget := &monitoringBudgetRepoStub{acquire: func(MonitoringProbeBudgetRequest, int) (string, error) {
		if attempts.Add(1) > capacity {
			return "", &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "global_minute"}
		}
		return "fixture-lease", nil
	}}
	tests, _ := qualityBudgetTestService(budget)
	tests.accountRepo = candyQueueAccountRepository{tests.accountRepo}
	var mu sync.Mutex
	covered := make(map[string]int)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct{ Model string }
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		require.Equal(t, "Bearer quality-budget-fixture", r.Header.Get("Authorization"))
		mu.Lock()
		covered[payload.Model]++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"21\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n\n")
	}))
	t.Cleanup(upstream.Close)
	tests.httpUpstream = candyQueueLocalHTTP(t, upstream)
	repo := &candyRepoFake{channelMonitorV2RepoStub: channelMonitorV2RepoStub{config: candyConfigFixture()}}
	repo.config.CandyProbes = nil
	steps := make([]routeStep, channelCandyMaxProbes*channelCandyMaxProbes)
	for i := range steps {
		steps[i].account = account(2, "native account")
	}
	for i := range channelCandyMaxProbes {
		p := candyProbeFixture()
		p.Model = fmt.Sprintf("probe-%03d", i)
		repo.config.CandyProbes = append(repo.config.CandyProbes, p)
	}
	router := &groupTestRouterFake{steps: steps}
	groups, _ := newGroupTestService(newGroupTestRepoFake(), router, tests.RunPelicanBackground)
	s := candyServiceFixture(repo, groups)
	for round := range channelCandyMaxProbes / capacity {
		attempts.Store(0)
		s.RunDue(t.Context(), groupTestNow.Add(time.Duration(round)*time.Minute))
		require.LessOrEqual(t, int(attempts.Load()), capacity+monitoringProbeQueueWorkers)
	}
	require.Len(t, covered, channelCandyMaxProbes)
	for model, count := range covered {
		require.Equal(t, 1, count, model)
	}
	require.Equal(t, channelCandyMaxProbes, budget.released, "all admitted physical requests settle before the next round")
	for _, usage := range budget.usages {
		require.Equal(t, MonitoringProbeTokenUsage{Tokens: 5, Reported: true}, usage)
	}
	correct := 0
	for _, result := range repo.results {
		if result.Verdict == "correct" {
			correct++
			continue
		}
		require.Equal(t, "deferred", result.Verdict)
		require.NotNil(t, result.BudgetDeferral)
		require.Empty(t, result.AnswerPreview, "local budget exhaustion is not a model-quality sample")
	}
	require.Equal(t, channelCandyMaxProbes, correct)
	require.Len(t, router.released, len(budget.requests), "denied attempts also release the selected account slot")
}

func TestChannelMonitorV2CandyPhysicalCancelAndNextTick(t *testing.T) {
	const targets = 12
	entered, release := make(chan struct{}, monitoringProbeQueueWorkers), make(chan struct{})
	var unblock sync.Once
	var blocking atomic.Bool
	blocking.Store(true)
	var sends atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		if blocking.Load() {
			entered <- struct{}{}
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"21\"}\n\ndata: {\"type\":\"response.completed\"}\n\n")
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(func() { unblock.Do(func() { close(release) }) })
	budget := &monitoringBudgetRepoStub{}
	tests, _ := qualityBudgetTestService(budget)
	tests.accountRepo = candyQueueAccountRepository{tests.accountRepo}
	tests.httpUpstream = candyQueueLocalHTTP(t, upstream)
	steps := make([]routeStep, targets+monitoringProbeQueueWorkers)
	for i := range steps {
		steps[i].account = account(2, "native account")
	}
	repo := &candyRepoFake{channelMonitorV2RepoStub: channelMonitorV2RepoStub{config: candyConfigFixture()}}
	repo.config.CandyProbes = nil
	for i := range targets {
		probe := candyProbeFixture()
		probe.Model = fmt.Sprintf("probe-%03d", i)
		repo.config.CandyProbes = append(repo.config.CandyProbes, probe)
	}
	router := &groupTestRouterFake{steps: steps}
	groups, _ := newGroupTestService(newGroupTestRepoFake(), router, tests.RunPelicanBackground)
	s := candyServiceFixture(repo, groups)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		unblock.Do(func() { close(release) })
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("the physical probe fixture did not drain")
		}
	})
	go func() { defer close(done); s.RunDue(ctx, groupTestNow) }()
	for range monitoringProbeQueueWorkers {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("the original adapter did not reach its bounded physical requests")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the canceled tick did not drain its requests and saves")
	}
	require.EqualValues(t, monitoringProbeQueueWorkers, sends.Load())
	require.Len(t, repo.claims, monitoringProbeQueueWorkers)
	require.Equal(t, monitoringProbeQueueWorkers, budget.released)
	require.Len(t, router.released, monitoringProbeQueueWorkers)
	for _, result := range repo.results {
		require.Equal(t, "error", result.Verdict, "retain the existing canceled-history contract, not a correct or incorrect answer")
		require.Equal(t, "timeout", result.Reason)
	}
	blocking.Store(false)
	unblock.Do(func() { close(release) })
	s.RunDue(t.Context(), groupTestNow.Add(time.Minute))
	require.EqualValues(t, targets+monitoringProbeQueueWorkers, sends.Load())
	require.Equal(t, targets+monitoringProbeQueueWorkers, budget.released)
	require.Len(t, router.released, targets+monitoringProbeQueueWorkers)
}
