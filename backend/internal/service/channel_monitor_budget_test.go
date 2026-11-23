//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestChannelMonitorCanceledRunKeepsHistoryAndFreshness(t *testing.T) {
	for _, phase := range []string{"before", "request", "late_success"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var sends atomic.Int32
			oldModel, oldPing := monitorHTTPClient, monitorPingHTTPClient
			defer func() { monitorHTTPClient, monitorPingHTTPClient = oldModel, oldPing }()
			monitorHTTPClient = &http.Client{Transport: monitorBudgetRoundTripper(func(req *http.Request) (*http.Response, error) {
				sends.Add(1)
				cancel()
				if phase == "request" {
					return nil, ctx.Err()
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"ok"}}]}`))}, nil
			})}
			monitorPingHTTPClient = &http.Client{Transport: monitorBudgetRoundTripper(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
			})}
			repo := &quotaModeRepoStub{monitor: &ChannelMonitor{ID: 1, Provider: MonitorProviderOpenAI,
				Endpoint: "https://fixture.example/v1", APIKey: "OLD:fixture", PrimaryModel: "fixture-model",
				BodyOverrideMode: MonitorBodyOverrideModeReplace, BodyOverride: map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hello"}}}}}
			if phase == "before" {
				cancel()
			}
			results, err := newQuotaModeService(repo).RunCheck(ctx, 1)
			require.ErrorIs(t, err, context.Canceled, "caller cancellation is not a new upstream verdict")
			require.Nil(t, results)
			require.Empty(t, repo.history)
			require.Empty(t, repo.markedIDs, "canceled work must not renew observation freshness")
			if phase == "before" {
				pauses := sends.Load()
				require.Zero(t, pauses, "already-canceled checks must not call the provider")
			}
		})
	}
}

func TestChannelMonitorProviderTimeoutStillRecordsFault(t *testing.T) {
	oldModel, oldPing := monitorHTTPClient, monitorPingHTTPClient
	defer func() { monitorHTTPClient, monitorPingHTTPClient = oldModel, oldPing }()
	monitorHTTPClient = &http.Client{Transport: monitorBudgetRoundTripper(func(*http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})}
	monitorPingHTTPClient = &http.Client{Transport: monitorBudgetRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	repo := &quotaModeRepoStub{monitor: &ChannelMonitor{ID: 1, Provider: MonitorProviderOpenAI,
		Endpoint: "https://fixture.example/v1", APIKey: "OLD:fixture", PrimaryModel: "fixture-model"}}
	results, err := newQuotaModeService(repo).RunCheck(t.Context(), 1)
	require.NoError(t, err)
	require.NoError(t, t.Context().Err(), "the provider's own timeout did not cancel its caller")
	require.Len(t, results, 1)
	require.Equal(t, MonitorStatusError, results[0].Status)
	require.Nil(t, results[0].BudgetDeferral)
	require.Len(t, repo.history, 1, "actual transport failures still produce the existing upstream observation")
	require.Equal(t, []int64{1}, repo.markedIDs)
}

func TestChannelMonitorBudgetRejectsBeforeGeneration(t *testing.T) {
	for _, cause := range []error{
		&MonitoringProbeBudgetDenial{Reason: "token_limit", Scope: "global_day", RetryAt: time.Now().Add(time.Hour)},
		&MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "key_minute", RetryAt: time.Now().Add(time.Minute)},
		ErrMonitoringProbeBudgetUnavailable,
	} {
		t.Run(cause.Error(), func(t *testing.T) {
			swapMonitorHTTPClient(t)
			var sends atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				sends.Add(1)
				_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
			}))
			t.Cleanup(server.Close)
			repo := &monitoringBudgetRepoStub{denyAt: 1, err: cause}
			ctx := NewMonitoringProbeBudgetService(repo).Context(t.Context(), server.URL, false)
			result := runCheckForModel(ctx, MonitorProviderOpenAI, server.URL, "fixture", "gpt-test", &CheckOptions{
				BodyOverrideMode: MonitorBodyOverrideModeReplace,
				BodyOverride:     map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hello"}}},
			})
			require.True(t, result.Skipped, "local budget rejection must not become an upstream failure")
			require.NotNil(t, result.BudgetDeferral)
			require.Nil(t, result.LatencyMs)
			require.Zero(t, sends.Load())
			require.Len(t, repo.requests, 1)
		})
	}
}

func TestChannelMonitorBudgetAllProviderUsageContracts(t *testing.T) {
	for _, tc := range []struct {
		provider, mode, response string
		tokens                   int64
	}{
		{MonitorProviderOpenAI, MonitorAPIModeChatCompletions, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":4,"completion_tokens":3}}`, 7},
		{MonitorProviderOpenAI, MonitorAPIModeResponses, `{"output_text":"ok","usage":{"input_tokens":4,"output_tokens":3}}`, 7},
		{MonitorProviderGrok, "", `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":4,"completion_tokens":3}}`, 7},
		{MonitorProviderKimi, "", `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":4,"completion_tokens":3}}`, 7},
		{MonitorProviderZhipu, "", `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":4,"completion_tokens":3}}`, 7},
		{MonitorProviderDeepseek, "", `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":4,"completion_tokens":3}}`, 7},
		{MonitorProviderMiniMax, "", `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":4,"completion_tokens":3}}`, 7},
		{MonitorProviderAnthropic, "", `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":3,"cache_read_input_tokens":2,"cache_creation_input_tokens":1}}`, 10},
		{MonitorProviderGemini, "", `{"candidates":[{"content":{"parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":3,"thoughtsTokenCount":2,"totalTokenCount":9}}`, 9},
	} {
		t.Run(tc.provider+"/"+tc.mode, func(t *testing.T) {
			swapMonitorHTTPClient(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.response)
			}))
			t.Cleanup(server.Close)
			repo := &monitoringBudgetRepoStub{}
			ctx := NewMonitoringProbeBudgetService(repo).Context(t.Context(), server.URL, false)
			text, _, _, err := callProvider(ctx, tc.provider, server.URL, "fixture", "model-a", "hi", &CheckOptions{APIMode: tc.mode})
			require.NoError(t, err)
			require.Equal(t, "ok", text)
			require.Len(t, repo.requests, 1)
			require.True(t, repo.requests[0].MeterTokens)
			require.Equal(t, []MonitoringProbeTokenUsage{{Tokens: tc.tokens, Reported: true}}, repo.usages)
			require.Equal(t, 1, repo.released)
		})
	}
}

func TestChannelMonitorBudgetRunCheckPersistenceAndRecovery(t *testing.T) {
	swapMonitorHTTPClient(t)
	originalPing := monitorPingHTTPClient
	monitorPingHTTPClient = monitorHTTPClient
	t.Cleanup(func() { monitorPingHTTPClient = originalPing })
	var heads, generations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodHead {
			heads.Add(1)
			require.Empty(t, req.Header.Get("Authorization"), "origin pings must never expose probe credentials")
			return
		}
		generations.Add(1)
		var body map[string]any
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": answerFromOpenAIRequest(body)}}},
		})
	}))
	t.Cleanup(server.Close)
	repo := &quotaModeRepoStub{monitor: &ChannelMonitor{ID: 81, Provider: MonitorProviderOpenAI,
		Endpoint: server.URL + "/tenant/v1", APIKey: "OLD:unused", PrimaryModel: "primary", ExtraModels: []string{"extra"},
		ExtraHeaders: map[string]string{"authorization": "Bearer final-fixture"}}}
	svc := newQuotaModeService(repo)
	budget := &monitoringBudgetRepoStub{denyAt: 1, err: &MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "global_day", RetryAt: time.Now().Add(time.Hour)}}
	svc.SetMonitoringBudget(NewMonitoringProbeBudgetService(budget))
	results, err := svc.RunCheck(t.Context(), 81)
	require.NoError(t, err)
	for _, result := range results {
		require.True(t, result.Skipped)
		require.Equal(t, "request_limit", result.BudgetDeferral.Reason)
	}
	require.Empty(t, repo.history)
	require.Empty(t, repo.markedIDs)
	require.Zero(t, heads.Load())
	require.Zero(t, generations.Load())
	require.Len(t, budget.requests, 3, "HEAD and both generation admissions share the same physical budget")

	budget.requests, budget.denyAt = nil, 0
	budget.acquire = func(_ MonitoringProbeBudgetRequest, n int) (string, error) {
		if n == 3 {
			return "", &MonitoringProbeBudgetDenial{Reason: "concurrency_limit", Scope: "key", RetryAt: time.Now().Add(time.Minute)}
		}
		return "fixture", nil
	}
	results, err = svc.RunCheck(t.Context(), 81)
	require.NoError(t, err)
	require.Len(t, repo.history, 1, "one deferred model must not taint the successful sibling")
	require.Len(t, repo.markedIDs, 1)
	for _, result := range results {
		if !result.Skipped {
			require.Equal(t, MonitorStatusOperational, result.Status)
			require.Equal(t, result.Model, repo.history[0].Model)
		}
	}
	require.EqualValues(t, 1, heads.Load())
	require.EqualValues(t, 1, generations.Load())
	require.Len(t, budget.requests, 3)
	require.False(t, budget.requests[0].MeterTokens, "HEAD is a physical request but not generated usage")
	require.Equal(t, monitoringProbeHash(server.URL), budget.requests[0].SiteHash)
	for _, request := range budget.requests {
		require.Equal(t, []string{config.UpstreamKeyFingerprint("final-fixture")}, request.KeyHashes)
		require.False(t, request.Recovery)
	}
	require.Equal(t, 2, budget.released)

	budget.acquire = nil
	results, err = svc.RunCheck(t.Context(), 81)
	require.NoError(t, err)
	for _, result := range results {
		require.False(t, result.Skipped)
		require.Nil(t, result.BudgetDeferral)
	}
	require.Len(t, repo.history, 3)
	require.Equal(t, 5, budget.released)
}

type monitorBudgetRoundTripper func(*http.Request) (*http.Response, error)

func (f monitorBudgetRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestChannelMonitorBudgetLeaseLossInvalidatesBufferedPass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		original := monitorHTTPClient
		monitorHTTPClient = &http.Client{Transport: monitorBudgetRoundTripper(func(req *http.Request) (*http.Response, error) {
			time.Sleep(41 * time.Second)
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":4,"completion_tokens":3}}`))}, nil
		})}
		t.Cleanup(func() { monitorHTTPClient = original })
		repo := &monitoringBudgetRepoStub{renew: func(context.Context) (bool, error) { return false, nil }}
		ctx := NewMonitoringProbeBudgetService(repo).Context(t.Context(), "https://fixture.example", false)
		result := runCheckForModel(ctx, MonitorProviderOpenAI, "https://fixture.example", "fixture", "model-a", &CheckOptions{
			BodyOverrideMode: MonitorBodyOverrideModeReplace, BodyOverride: map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}},
		})
		require.True(t, result.Skipped)
		require.Equal(t, "budget_unavailable", result.BudgetDeferral.Reason)
		require.Nil(t, result.LatencyMs)
		require.Equal(t, 1, repo.released)
	})
}

func TestChannelMonitorBudgetSharedKeyPrecedesPhysicalBudget(t *testing.T) {
	swapMonitorHTTPClient(t)
	for _, blocked := range []bool{true, false} {
		t.Run(map[bool]string{true: "shared_key", false: "monitoring_budget"}[blocked], func(t *testing.T) {
			key := &monitorKeyAdmissionStub{}
			key.blocked.Store(blocked)
			budget := &monitoringBudgetRepoStub{denyAt: 1, err: ErrMonitoringProbeBudgetUnavailable}
			ctx := NewMonitoringProbeBudgetService(budget).Context(t.Context(), "https://fixture.example", false)
			_, _, err := postRawJSON(ctx, "https://fixture.example/v1/chat/completions", []byte(`{}`), map[string]string{"Authorization": "Bearer fixture"}, &CheckOptions{keyAdmission: key, requireKeyAdmission: true})
			require.Error(t, err)
			if blocked {
				require.Empty(t, budget.requests)
				require.Zero(t, key.finished.Load())
			} else {
				require.Len(t, budget.requests, 1)
				require.EqualValues(t, 1, key.finished.Load(), "budget rejection must release the acquired shared Key")
			}
		})
	}
}

func TestChannelMonitorBudgetTransportFailureSettlement(t *testing.T) {
	for _, settlementFailed := range []bool{false, true} {
		t.Run(map[bool]string{false: "supplier_transport", true: "budget_settlement"}[settlementFailed], func(t *testing.T) {
			original := monitorHTTPClient
			monitorHTTPClient = &http.Client{Transport: monitorBudgetRoundTripper(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("fixture network failure")
			})}
			t.Cleanup(func() { monitorHTTPClient = original })
			repo := &monitoringBudgetRepoStub{}
			if settlementFailed {
				repo.releaseErr = errors.New("fixture settlement failure")
			}
			ctx := NewMonitoringProbeBudgetService(repo).Context(t.Context(), "https://fixture.example", false)
			result := runCheckForModel(ctx, MonitorProviderOpenAI, "https://fixture.example", "fixture", "model-a", nil)
			require.Equal(t, settlementFailed, result.Skipped, "a local settlement failure must remain typed even when transport also fails")
			if settlementFailed {
				require.Equal(t, "budget_unavailable", result.BudgetDeferral.Reason)
			} else {
				require.Equal(t, MonitorStatusError, result.Status)
				require.Nil(t, result.BudgetDeferral)
			}
			require.Equal(t, 1, repo.released)
			require.Equal(t, []MonitoringProbeTokenUsage{{}}, repo.usages)
		})
	}
}

func TestChannelMonitorBudgetMetersFinalWireUsage(t *testing.T) {
	swapMonitorHTTPClient(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		require.Equal(t, "/tenant/v1/chat/completions", req.URL.Path)
		require.Equal(t, "Bearer final-fixture", req.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`)
	}))
	t.Cleanup(server.Close)
	repo := &monitoringBudgetRepoStub{}
	ctx := NewMonitoringProbeBudgetService(repo).Context(t.Context(), server.URL+"/tenant/v1", false)
	text, _, status, err := callProvider(ctx, MonitorProviderOpenAI, server.URL+"/tenant/v1", "unused-fixture", "gpt-test", "hi", &CheckOptions{
		ExtraHeaders: map[string]string{"authorization": "Bearer final-fixture"},
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "ok", text)
	require.Len(t, repo.requests, 1)
	require.Equal(t, []string{config.UpstreamKeyFingerprint("final-fixture")}, repo.requests[0].KeyHashes)
	require.Equal(t, monitoringProbeHash(server.URL+"/tenant"), repo.requests[0].SiteHash)
	require.False(t, repo.requests[0].Recovery)
	require.True(t, repo.requests[0].MeterTokens)
	require.Equal(t, 1, repo.released)
	require.Equal(t, []MonitoringProbeTokenUsage{{Tokens: 7, Reported: true}}, repo.usages)
}

func TestChannelMonitorBudgetSettlementFailureInvalidatesBufferedPass(t *testing.T) {
	swapMonitorHTTPClient(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": answerFromOpenAIRequest(body)}}},
			"usage":   map[string]any{"prompt_tokens": 4, "completion_tokens": 3},
		})
	}))
	t.Cleanup(server.Close)
	repo := &monitoringBudgetRepoStub{releaseErr: errors.New("fixture unavailable")}
	ctx := NewMonitoringProbeBudgetService(repo).Context(context.Background(), server.URL, false)
	result := runCheckForModel(ctx, MonitorProviderOpenAI, server.URL, "fixture", "gpt-test", nil)
	require.True(t, result.Skipped, "body closure and settlement must finish before publishing a valid observation")
	require.Nil(t, result.LatencyMs)
	require.Equal(t, 1, repo.released)
	require.Equal(t, []MonitoringProbeTokenUsage{{Tokens: 7, Reported: true}}, repo.usages)
}

func TestChannelMonitorBudgetDoesNotFollowRedirect(t *testing.T) {
	swapMonitorHTTPClient(t)
	var destinations atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		destinations.Add(1)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	t.Cleanup(destination.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, destination.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(origin.Close)
	repo := &monitoringBudgetRepoStub{}
	ctx := NewMonitoringProbeBudgetService(repo).Context(t.Context(), origin.URL, false)
	_, status, err := postRawJSON(ctx, origin.URL+"/v1/chat/completions", []byte(`{}`), map[string]string{"Authorization": "Bearer fixture"}, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusTemporaryRedirect, status)
	require.Zero(t, destinations.Load())
	require.Len(t, repo.requests, 1)
	require.Equal(t, 1, repo.released)
	require.Equal(t, []MonitoringProbeTokenUsage{{}}, repo.usages)
	require.NotContains(t, strings.Join(repo.requests[0].KeyHashes, ""), "fixture")
}
