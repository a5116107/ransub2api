//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/stretchr/testify/require"
)

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
			require.NotSame(t, cfg, result.PelicanConfig, "per-sample evidence must not mutate the reusable plan")
		})
	}
}
