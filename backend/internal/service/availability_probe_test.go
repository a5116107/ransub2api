//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestQualityReportedModelRealHTTP(t *testing.T) {
	for _, protocol := range []string{"responses", "chat"} {
		for _, scenario := range []string{"matching", "different", "missing", "conflicting", "incomplete", "opaque-request", "long-name", "invalid-name"} {
			t.Run(protocol+"/"+scenario, func(t *testing.T) {
				reported := "wire-model"
				if scenario == "different" {
					reported = "another-model"
				}
				if scenario == "missing" {
					reported = ""
				}
				if scenario == "long-name" {
					reported = strings.Repeat("m", 201)
				}
				if scenario == "invalid-name" {
					reported = "untrusted name"
				}
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					calls.Add(1)
					body, err := io.ReadAll(request.Body)
					require.NoError(t, err)
					require.Equal(t, "wire-model", gjson.GetBytes(body, "model").String(), "compare to the mapped physical model, not the public alias")
					w.Header().Set("Content-Type", "text/event-stream")
					if protocol == "chat" {
						require.Equal(t, "/v1/chat/completions", request.URL.Path)
						if scenario == "conflicting" {
							_, _ = io.WriteString(w, "data: {\"model\":\"another-model\",\"choices\":[]}\n\n")
						}
						_, _ = fmt.Fprintf(w, "data: {\"model\":%q,\"choices\":[{\"delta\":{\"content\":\"42\"}}]}\n\n", reported)
						if scenario != "incomplete" {
							_, _ = io.WriteString(w, "data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
						}
						return
					}
					require.Equal(t, "/v1/responses", request.URL.Path)
					if scenario == "conflicting" {
						_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"model\":\"another-model\"}}\n\n")
					}
					_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"42\"}\n\n")
					if scenario != "incomplete" {
						_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"model\":%q}}\n\n", reported)
					}
				}))
				t.Cleanup(server.Close)
				account := &Account{ID: 781, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
					Credentials: map[string]any{"api_key": "declaration-fixture-key", "base_url": server.URL, "model_mapping": map[string]any{"public-model": "wire-model"}},
					Extra:       map[string]any{"openai_responses_mode": "force_responses"}}
				if protocol == "chat" {
					account.Extra["openai_responses_mode"] = "force_chat_completions"
				}
				transport := &codexModelsHTTPUpstreamStub{do: func(request *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
					request, trace := requesttiming.StartTransport(request)
					response, err := server.Client().Do(request)
					trace.Response(response, err)
					if scenario == "opaque-request" && response != nil {
						response.Request = nil
					}
					return response, err
				}}
				svc := pelicanClaudeTestService(account, transport)
				svc.cfg = rawChatCompletionsTestConfig()
				cfg := &PelicanTestConfig{QuestionKind: "candy", Prompt: "Return 42", ParallelCount: 1,
					Quality: &QualityPolicy{Action: QualityActionObserveOnly, ExpectedAnswer: "42"}}
				result, err := svc.RunPelicanBackground(t.Context(), account.ID, "public-model", cfg)
				require.NoError(t, err)
				require.EqualValues(t, 1, calls.Load())
				recordQualityMeasurement(&ScheduledTestPlan{AccountID: account.ID, PelicanConfig: cfg}, result)
				measurement := result.PelicanConfig.QualityMeasurement
				wantStatus, wantReason := "unknown", "model_declaration_not_observed"
				switch scenario {
				case "matching":
					wantStatus, wantReason = "pass", "upstream_model_matches_request"
				case "different":
					wantStatus, wantReason = "fail", "upstream_model_differs_from_request"
				case "conflicting":
					wantReason = "model_declaration_conflict"
				}
				require.Equal(t, wantStatus, measurement.Dimensions["reported_model"].Status)
				require.Equal(t, wantReason, measurement.Dimensions["reported_model"].Reason)
				require.Equal(t, "unknown", measurement.Dimensions["model_identity"].Status)
				raw, err := json.Marshal(result)
				require.NoError(t, err)
				for _, private := range []string{"declaration-fixture-key", server.URL, "another-model", "untrusted name", "reportedModelStatus"} {
					require.NotContains(t, string(raw), private)
				}
				var roundTrip ScheduledTestResult
				require.NoError(t, json.Unmarshal(raw, &roundTrip))
				require.Equal(t, measurement.Dimensions["reported_model"], roundTrip.PelicanConfig.QualityMeasurement.Dimensions["reported_model"])
			})
		}
	}
}

func TestQualityReportedModelAttemptAndScopeBoundaries(t *testing.T) {
	for _, protocol := range []string{"responses_http", "chat_http", "responses_ws_v2", "bps_responses", "messages_http", "unknown"} {
		t.Run(protocol, func(t *testing.T) {
			observer := &upstreamResponseModelObserver{}
			observer.Observe("wire-model", true)
			want := "unknown"
			if protocol == "responses_http" || protocol == "chat_http" || protocol == "responses_ws_v2" {
				want = "match"
			}
			require.Equal(t, want, availabilityReportedModelStatus(&AvailabilityRequestScope{Protocol: protocol, Model: "wire-model"}, observer))
		})
	}
	observation := &availabilityProbeObservation{reportedModel: &upstreamResponseModelObserver{first: "stale-model"}}
	collector := requesttiming.New(time.Now(), 0)
	ctx := context.WithValue(requesttiming.With(t.Context(), collector), availabilityProbeSnapshotKey{}, observation)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://fixture.invalid/v1/responses", strings.NewReader(`{"model":"wire-model","input":"sample"}`))
	require.NoError(t, err)
	request, trace := requesttiming.StartAttempt(request, 1, 0)
	observeAvailabilityProbeRequest(request, trace, nil, "")
	require.Nil(t, observation.reportedModel, "a new HTTP attempt must not inherit an earlier declaration")
	observation.reportedModel = &upstreamResponseModelObserver{first: "stale-model"}
	_, _, _ = beginAvailabilityWSProbe(ctx, &Account{ID: 1}, "wss://fixture.invalid/v1/responses")
	require.Nil(t, observation.reportedModel, "a new socket attempt must not inherit an earlier declaration")
}

func TestAvailabilityProbeNativeMessagesRequiresCompleteProtocol(t *testing.T) {
	start := "data: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"model\":\"claude-opus-4-6\",\"usage\":{\"input_tokens\":2}}}\n\n"
	text := "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"21\"}}\n\n"
	stop := "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
	for _, tc := range []struct {
		name, wire, status string
	}{
		{"complete", start + text + stop, "pass"},
		{"missing stop", start + text, "inconclusive"},
		{"done is not message stop", start + text + "data: [DONE]\n\n", "inconclusive"},
		{"orphan stop", text + stop, "inconclusive"},
		{"late start", text + start + stop, "inconclusive"},
		{"duplicate start", start + text + start + stop, "inconclusive"},
		{"no text", start + stop, "inconclusive"},
		{"error after text", start + text + "data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"fixture error\"}}\n\n", "inconclusive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/messages" {
					t.Errorf("unexpected probe path: %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.wire)
			}))
			t.Cleanup(server.Close)
			account := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
				Credentials: map[string]any{"base_url": server.URL, "api_key": "fixture-key"}}
			upstream := &codexModelsHTTPUpstreamStub{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
				return server.Client().Do(req)
			}}
			svc := pelicanClaudeTestService(account, upstream)
			svc.cfg = rawChatCompletionsTestConfig()
			result, err := svc.RunPelicanBackground(t.Context(), account.ID, "claude-opus-4-6", &PelicanTestConfig{QuestionKind: "candy", Prompt: CandyPrompt, ReasoningEffort: "high", ParallelCount: 1})
			require.NoError(t, err)
			probe := result.PelicanConfig.AvailabilityResult
			require.NotNil(t, probe)
			require.Equal(t, tc.status, probe.Status)
			require.NotNil(t, probe.Scope, "scope comes from the actual HTTP response request")
			require.Equal(t, "messages_http", probe.Scope.Protocol)
			require.Equal(t, "adaptive", probe.Scope.ThinkingMode)
			require.Equal(t, "high", probe.Scope.ReasoningEffort)
			require.Equal(t, 1, probe.AttemptCount)
			raw, err := json.Marshal(probe)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "fixture-key")
			require.NotContains(t, string(raw), server.URL)
		})
	}
}

func availabilityProbeJSON(t *testing.T, result *ScheduledTestResult) map[string]any {
	t.Helper()
	require.NotNil(t, result)
	raw, err := json.Marshal(result.PelicanConfig)
	require.NoError(t, err)
	var config map[string]any
	require.NoError(t, json.Unmarshal(raw, &config))
	probe, ok := config["availability_result"].(map[string]any)
	require.True(t, ok, "upstream availability must be recorded separately from grading")
	return probe
}

func TestAvailabilityProbeTransportBoundaries(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name, failure, want, reason            string
		physical, canceled, limited, truncated bool
	}{
		{"unobserved_local_failure", "transport_error", "inconclusive", "response_not_confirmed", false, false, false, false},
		{"local_timeout", "timeout", "inconclusive", "response_not_confirmed", false, false, false, false},
		{"network_timeout", "timeout", "fail", "upstream_timeout", true, false, false, false},
		{"network_failure", "transport_error", "fail", "transport_error", true, false, false, false},
		{"canceled_request", "canceled", "inconclusive", "canceled", true, true, false, false},
		{"capture_limit", "canceled", "inconclusive", "capture_limited", true, true, true, false},
		{"truncated_observations", "transport_error", "inconclusive", "truncated_evidence", true, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempt := requesttiming.Attempt{Number: 1, Kind: "egress", Error: tc.failure}
			if tc.physical {
				attempt.Events = map[string]float64{"connection_start": 0}
			}
			observation := &availabilityProbeObservation{snapshot: &requesttiming.Snapshot{Attempts: []requesttiming.Attempt{attempt}, Canceled: tc.canceled, Truncated: tc.truncated}}
			result := availabilityProbeResult(now, now.Add(time.Second), observation, false, tc.limited, nil)
			require.Equal(t, tc.want, result.Status)
			require.Equal(t, tc.reason, result.Reason)
		})
	}
	// An earlier failed attempt must not poison a later completed retry.
	observation := &availabilityProbeObservation{snapshot: &requesttiming.Snapshot{Outcome: "success", Attempts: []requesttiming.Attempt{
		{Number: 1, Kind: "egress", Status: 503}, {Number: 2, Kind: "http", Parent: 1, Status: 503},
		{Number: 3, Kind: "egress", Status: 200}, {Number: 4, Kind: "http", Parent: 3, Status: 200},
	}}}
	result := availabilityProbeResult(now, now.Add(time.Second), observation, true, false, nil)
	require.Equal(t, "pass", result.Status)
	require.Equal(t, 2, result.AttemptCount, "physical children must not double-count egress attempts")
	observation.snapshot.Attempts = observation.snapshot.Attempts[:2]
	result = availabilityProbeResult(now, now.Add(time.Second), observation, true, false, nil)
	require.Equal(t, "inconclusive", result.Status, "a successful but unobserved fallback must not be attributed to an earlier rejection")
	require.Equal(t, "conflicting_evidence", result.Reason)
}

func TestAvailabilityProbeLocalSampleFailuresDoNotBecomeUpstreamFailures(t *testing.T) {
	for _, failure := range []string{"panic", "error", "nil_result", "nil_config"} {
		t.Run(failure, func(t *testing.T) {
			runner := &ScheduledTestRunnerService{runPelican: func(context.Context, int64, string, *PelicanTestConfig) (*ScheduledTestResult, error) {
				if failure == "panic" {
					panic("fixture-local-panic")
				}
				if failure == "error" {
					return nil, errors.New("fixture-local-error")
				}
				return nil, nil
			}}
			plan := &ScheduledTestPlan{ID: 1, AccountID: 1, ModelID: "target", PelicanConfig: &PelicanTestConfig{QuestionKind: "candy"}}
			if failure == "nil_config" {
				plan.PelicanConfig = nil
			}
			result := runner.runPelicanSample(t.Context(), plan)
			require.Equal(t, "failed", result.Status)
			require.Equal(t, "inconclusive", availabilityProbeJSON(t, result)["status"])
		})
	}
}

func TestAvailabilityProbeRealHTTPBrokenBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "999999")
		if _, err := io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"); err != nil {
			return
		}
	}))
	t.Cleanup(server.Close)
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	account := adaptiveCNAccountTestAccount(1, PlatformDeepseek)
	account.Credentials["api_protocol"] = APIProtocolResponses
	svc, _ := adaptiveCNAccountTestService(account)
	svc.httpUpstream = &accountTestHTTPUpstream{target: target}
	result, err := svc.RunPelicanBackground(t.Context(), account.ID, "target", &PelicanTestConfig{QuestionKind: "candy", Prompt: CandyPrompt, ParallelCount: 1})
	require.NoError(t, err)
	probe := availabilityProbeJSON(t, result)
	require.Equal(t, "failed", result.Status)
	require.Equal(t, "fail", probe["status"], "HTTP 200 plus a broken body is not a completed response")
	require.Equal(t, "transport_error", probe["reason"])
}

func TestAvailabilityProbeSyntheticSuccessIsNotNetworkEvidence(t *testing.T) {
	account := adaptiveCNAccountTestAccount(1, PlatformDeepseek)
	account.Extra = map[string]any{"synthetic_ui_test": true}
	svc, upstream := adaptiveCNAccountTestService(account)
	result, err := svc.RunPelicanBackground(t.Context(), account.ID, "target", &PelicanTestConfig{QuestionKind: "candy", Prompt: CandyPrompt, ParallelCount: 1})
	require.NoError(t, err)
	require.Empty(t, upstream.requests)
	require.Equal(t, "inconclusive", availabilityProbeJSON(t, result)["status"])
	require.Equal(t, "not_observed", result.PelicanConfig.AvailabilityResult.Reason)
}

func TestAvailabilityProbeRealBackgroundResponse(t *testing.T) {
	for _, tc := range []struct {
		name, wire, want, reason string
		status                   int
	}{
		{"complete", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"21\"}\n\ndata: {\"type\":\"response.completed\"}\n\n", "pass", "completed_response", 200},
		{"wrong_answer", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"7\"}\n\ndata: {\"type\":\"response.completed\"}\n\n", "pass", "completed_response", 200},
		{"headers_only", "", "inconclusive", "response_not_confirmed", 200},
		{"request_error", `{}`, "inconclusive", "request_rejected", 400},
		{"policy_error", `{}`, "inconclusive", "request_rejected", 403},
		{"unsupported_model", `{}`, "inconclusive", "request_rejected", 404},
		{"auth", `{}`, "fail", "authentication_rejected", 401},
		{"rate_limit", `{}`, "fail", "rate_limited", 429},
		{"server_error", `{}`, "fail", "upstream_error", 503},
		{"semantic_request_error", "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"invalid_prompt\",\"message\":\"fixture-invalid-prompt\"}}}\n\n", "inconclusive", "request_rejected", 200},
		{"semantic_failure", "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"fixture-upstream-error\"}}}\n\n", "fail", "upstream_error", 200},
		{"semantic_rate_limit", "data: {\"type\":\"error\",\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"fixture-throttle\"}}\n\n", "fail", "rate_limited", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := adaptiveCNAccountTestAccount(1, PlatformDeepseek)
			account.Credentials["api_protocol"] = APIProtocolResponses
			response := &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(tc.wire))}
			svc, upstream := adaptiveCNAccountTestService(account, response)
			cfg := &PelicanTestConfig{QuestionKind: "candy", Prompt: CandyPrompt, ParallelCount: 1}
			result, err := svc.RunPelicanBackground(context.Background(), account.ID, "target", cfg)
			require.NoError(t, err)
			require.NotEmpty(t, upstream.requests)
			probe := availabilityProbeJSON(t, result)
			require.Equal(t, tc.want, probe["status"])
			require.Equal(t, tc.reason, probe["reason"])
			require.EqualValues(t, tc.status, probe["http_status"])
			require.NotEmpty(t, probe["started_at"])
			require.NotEmpty(t, probe["finished_at"])
			if tc.name == "wrong_answer" {
				require.Equal(t, "failed", result.Status, "availability must not replace answer validation")
			}
		})
	}
}

func TestAvailabilityProbeSurvivesJudgeResultAndSampleSnapshot(t *testing.T) {
	for _, verdict := range []string{"correct", "incorrect", "unknown", "panic"} {
		t.Run(verdict, func(t *testing.T) {
			account := adaptiveCNAccountTestAccount(1, PlatformDeepseek)
			account.Credentials["api_protocol"] = APIProtocolResponses
			response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"21\"}\n\ndata: {\"type\":\"response.completed\"}\n\n"))}
			svc, _ := adaptiveCNAccountTestService(account, response)
			cfg := &PelicanTestConfig{QuestionKind: "candy", Prompt: CandyPrompt, ParallelCount: 1, Quality: &QualityPolicy{Action: QualityActionObserveOnly, ExpectedAnswer: "21"}}
			runner := &ScheduledTestRunnerService{runPelican: svc.RunPelicanBackground,
				judgeQuality: func(context.Context, int64, *PelicanTestConfig, string) *QualityJudgment {
					if verdict == "panic" {
						panic("fixture-local-judge-panic")
					}
					return &QualityJudgment{Verdict: verdict, Reason: "fixture_judge"}
				}}
			result := runner.runPelicanSample(context.Background(), &ScheduledTestPlan{AccountID: account.ID, ModelID: "target", PelicanConfig: cfg})
			if verdict == "panic" {
				require.Equal(t, "failed", result.Status)
				require.Nil(t, result.QualityJudgment)
			} else {
				require.Equal(t, verdict, result.QualityJudgment.Verdict)
			}
			require.Equal(t, "pass", availabilityProbeJSON(t, result)["status"])
			require.Equal(t, intelligenceTestPrompt(cfg), result.PelicanConfig.ActualPrompt, "prompt and transport evidence must both survive grading, including panic recovery")
			require.Empty(t, cfg.ActualPrompt, "result evidence must not be copied back into the reusable rule")
			require.NotSame(t, cfg, result.PelicanConfig, "per-sample evidence must not mutate the reusable plan")
		})
	}
}

func TestAvailabilityProbeDoesNotTrustRuleEvidenceWithoutExecutorSnapshot(t *testing.T) {
	plan := &ScheduledTestPlan{ID: 1, AccountID: 1, ModelID: "target", PelicanConfig: &PelicanTestConfig{
		QuestionKind: "candy", Prompt: CandyPrompt,
		AvailabilityResult: &AvailabilityProbeResult{Version: 1, Status: "pass", Reason: "client_supplied"},
	}}
	runner := &ScheduledTestRunnerService{runPelican: func(context.Context, int64, string, *PelicanTestConfig) (*ScheduledTestResult, error) {
		return &ScheduledTestResult{Status: "success"}, nil
	}}
	result := runner.runPelicanSample(t.Context(), plan)
	require.Equal(t, "inconclusive", availabilityProbeJSON(t, result)["status"])
	require.Equal(t, "not_observed", result.PelicanConfig.AvailabilityResult.Reason)
	require.Equal(t, "pass", plan.PelicanConfig.AvailabilityResult.Status, "rule snapshot must remain untouched")
}
