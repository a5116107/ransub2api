//go:build unit

package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
)

func TestAvailabilityBPSProbeScopeAndAdmission(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	account := excelAccount()
	collector := requesttiming.New(time.Now(), 0)
	observation := &availabilityProbeObservation{}
	probeCtx := context.WithValue(requesttiming.With(t.Context(), collector), availabilityProbeSnapshotKey{}, observation)
	probeCtx = context.WithValue(probeCtx, qualityProbeContextKey{}, true)
	probeBody, err := buildExcelBPSAccountTestBody("gpt-5.6-sol", "probe", "medium")
	require.NoError(t, err)
	calls := 0
	svc := openAIClientToolsTestService(nil)
	t.Cleanup(func() { require.NoError(t, svc.CloseExcelBPSImages()) })
	svc.httpUpstream = &bpsTestUpstream{send: func(req *http.Request, _ string) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Request: req,
			Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixture\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n"))}, nil
	}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(probeBody)).WithContext(probeCtx)
	_, err = svc.Forward(probeCtx, c, account, probeBody)
	require.NoError(t, err)
	require.Equal(t, &AvailabilityRequestScope{Version: 1, Protocol: "bps_responses", Stream: true, Model: "gpt-5.6-sol", ServiceTier: "unspecified", ReasoningEffort: "medium", Capability: "text"}, observation.scope)
	require.Equal(t, 1, observation.scopeAttempt)
	report := availabilityAdmissionPass(account.ID, "gpt-5.6-sol")
	report.Models[0].Decision.Scope = observation.scope
	svc.availabilityReader = &availabilityAdmissionRepo{reports: []*AvailabilityReport{report}}
	for _, tc := range []struct {
		name, body string
		allowed    bool
	}{
		{"same", `{"model":"gpt-5.6-sol","stream":true,"reasoning":{"effort":"medium"},"input":"different prompt"}`, true},
		{"different model", `{"model":"gpt-6-astra","stream":true,"reasoning":{"effort":"medium"},"input":"hi"}`, false},
		{"different effort", `{"model":"gpt-5.6-sol","stream":true,"reasoning":{"effort":"high"},"input":"hi"}`, false},
		{"different stream", `{"model":"gpt-5.6-sol","stream":false,"reasoning":{"effort":"medium"},"input":"hi"}`, false},
		{"priority tier", `{"model":"gpt-5.6-sol","stream":true,"service_tier":"priority","reasoning":{"effort":"medium"},"input":"hi"}`, false},
		{"tool", `{"model":"gpt-5.6-sol","stream":true,"reasoning":{"effort":"medium"},"input":"hi","tools":[{"type":"function","name":"fixture"}]}`, false},
		{"image", `{"model":"gpt-5.6-sol","stream":true,"reasoning":{"effort":"medium"},"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://fixture.invalid/a.png"}]}]}`, false},
		{"unknown", `{"model":"gpt-5.6-sol","stream":true,"reasoning":{"effort":"medium"},"input":"hi","unknown_capability":true}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := calls
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			body := []byte(tc.body)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			_, err := svc.Forward(t.Context(), c, account, body)
			if tc.allowed {
				require.NoError(t, err)
				require.Equal(t, before+1, calls)
			} else {
				require.Error(t, err)
				require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
				require.Equal(t, before, calls)
			}
		})
	}
	t.Run("failure and recovery", func(t *testing.T) {
		body := []byte(`{"model":"gpt-5.6-sol","stream":true,"reasoning":{"effort":"medium"},"input":"hi"}`)
		send := func() error {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			_, err := svc.Forward(t.Context(), c, account, body)
			return err
		}
		before := calls
		report.Models[0].Status = "fail"
		require.Error(t, send())
		require.Equal(t, before, calls)
		report.Models[0].Status = "pass"
		require.NoError(t, send())
		require.Equal(t, before+1, calls)
	})
	t.Run("inherited tools", func(t *testing.T) {
		send := func(body []byte) error {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			c.Request.Header.Set("Session-Id", "availability-inherited-tools")
			_, err := svc.Forward(t.Context(), c, account, body)
			return err
		}
		svc.availabilityReader = nil
		withTool := []byte(`{"model":"gpt-5.6-sol","stream":true,"reasoning":{"effort":"medium"},"input":"hi","tools":[{"type":"function","name":"fixture"}]}`)
		require.NoError(t, send(withTool))
		svc.availabilityReader = &availabilityAdmissionRepo{reports: []*AvailabilityReport{report}}
		before := calls
		withoutTool := []byte(`{"model":"gpt-5.6-sol","stream":true,"reasoning":{"effort":"medium"},"input":"hi"}`)
		require.Error(t, send(withoutTool))
		require.Equal(t, before, calls, "a tool catalog inherited from this session is not a text-only probe scope")
	})
}

func TestAvailabilityBPSScopeRejectsWireMutation(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","stream":true,"reasoning":{"effort":"medium"},"input":"hi"}`)
	wire, bridge, err := basispoints.Prepare(body, "fixture", nil)
	require.NoError(t, err)
	ctx := withAvailabilityBPSSource(t.Context(), body, bridge)
	for _, tc := range []struct {
		name, field string
		value       any
	}{
		{"model", "model", "other"},
		{"model selection", "model_selection", "automatic"},
		{"physical stream", "stream", false},
		{"context management", "context_management.0.compact_threshold", 42},
		{"agent iteration", "metadata.agent_iteration", "2"},
		{"extra option", "unknown_capability", true},
		{"nontext input", "input.0.content.0.type", "input_image"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated, err := sjson.SetBytes(wire, tc.field, tc.value)
			require.NoError(t, err)
			req, err := newExcelBPSRequest(ctx, mutated, "token", "account")
			require.NoError(t, err)
			require.Nil(t, availabilityScopeForRequest(req, mutated))
		})
	}
	req, err := newExcelBPSRequest(ctx, wire, "token", "account")
	require.NoError(t, err)
	require.NotNil(t, availabilityScopeForRequest(req, wire))
	req.Host = "other.invalid"
	require.Nil(t, availabilityScopeForRequest(req, wire))
	other, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://other.invalid/v1/responses", bytes.NewReader(body))
	require.NoError(t, err)
	require.Nil(t, availabilityScopeForRequest(other, body), "a BPS source must not be reclassified as an ordinary HTTP probe")
}

func TestAvailabilityBPSBackgroundProbeRecordsActualScope(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			account := excelAccount()
			upstream := &bpsTestUpstream{send: func(req *http.Request, _ string) (*http.Response, error) {
				wire := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"21\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixture\",\"status\":\"completed\",\"output\":[]}}\n\n"
				if status != http.StatusOK {
					wire = `{}`
				}
				return &http.Response{StatusCode: status, Request: req, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}, nil
			}}
			gateway := openAIClientToolsTestService(nil)
			gateway.httpUpstream = upstream
			t.Cleanup(func() { require.NoError(t, gateway.CloseExcelBPSImages()) })
			svc := &AccountTestService{accountRepo: &stateProbeAccountRepo{account: account}, httpUpstream: upstream, openaiGatewayService: gateway}
			cfg := bpsObservationConfig()
			cfg.TestChannel, cfg.Quality = "account", nil
			cfg.Availability = &AvailabilityPolicy{ValidForSeconds: 60}
			require.NoError(t, validateAvailabilityPolicy(cfg))
			result, err := svc.RunPelicanBackground(t.Context(), account.ID, "gpt-5.6-sol", cfg)
			require.NoError(t, err)
			require.NotNil(t, result.PelicanConfig.AvailabilityResult)
			observation := result.PelicanConfig.AvailabilityResult
			if status == http.StatusOK {
				require.Equal(t, "pass", observation.Status)
				require.NotNil(t, observation.Scope)
				require.Equal(t, "bps_responses", observation.Scope.Protocol)
				require.Equal(t, "high", observation.Scope.ReasoningEffort)
			} else {
				require.Equal(t, "fail", observation.Status)
				require.Equal(t, status, observation.HTTPStatus)
			}
		})
	}
}

func TestAvailabilityBPSAdmissionPreservesUnsentRetryEvidence(t *testing.T) {
	account := excelAccount()
	account.Extra["openai_excel_bps_mihomo"] = true
	body := []byte(`{"model":"gpt-5.6-sol","stream":true,"reasoning":{"effort":"medium"},"input":"hi"}`)
	wire, bridge, err := basispoints.Prepare(body, "fixture", nil)
	require.NoError(t, err)
	ctx := withAvailabilityBPSSource(t.Context(), body, bridge)
	report := availabilityAdmissionPass(account.ID, "gpt-5.6-sol")
	report.Models[0].Decision.Scope = &AvailabilityRequestScope{Version: 1, Protocol: "bps_responses", Stream: true, Model: "gpt-5.6-sol", ServiceTier: "unspecified", ReasoningEffort: "medium", Capability: "text"}
	calls, acquired := 0, 0
	leases := []*bpsTestLease{{}, {}}
	svc := &OpenAIGatewayService{availabilityReader: &availabilityAdmissionRepo{reports: []*AvailabilityReport{report}}, httpUpstream: &bpsTestUpstream{send: func(req *http.Request, _ string) (*http.Response, error) {
		calls++
		if calls == 1 {
			httptrace.ContextClientTrace(req.Context()).GetConn("bps.openai.com:443")
			return nil, io.EOF
		}
		actual, readErr := io.ReadAll(req.Body)
		require.NoError(t, readErr)
		require.Equal(t, wire, actual)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	}}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	response, lease, _, err := svc.doExcelBPSRequest(ctx, c, account, "fixture", wire, "token", "account", func(context.Context, string, ...string) (string, excelBPSLease, error) {
		lease := leases[acquired]
		acquired++
		return "http://127.0.0.1:19000", lease, nil
	})
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	require.Equal(t, 2, acquired)
	require.Equal(t, 1, leases[0].releases)
	require.NoError(t, response.Body.Close())
	lease.Release()
	require.Equal(t, 1, leases[1].releases)
}

func TestAvailabilityScopeMatchesOnlyObservedCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name, path, payload string
		allowed             bool
	}{
		{"same", "/v1/responses", `{"model":"target","input":"different prompt","stream":true,"service_tier":"priority","reasoning":{"effort":"high"}}`, true},
		{"different tier", "/v1/responses", `{"model":"target","input":"hi","stream":true,"service_tier":"default","reasoning":{"effort":"high"}}`, false},
		{"different effort", "/v1/responses", `{"model":"target","input":"hi","stream":true,"service_tier":"priority","reasoning":{"effort":"low"}}`, false},
		{"different stream", "/v1/responses", `{"model":"target","input":"hi","stream":false,"service_tier":"priority","reasoning":{"effort":"high"}}`, false},
		{"different protocol", "/v1/chat/completions", `{"model":"target","messages":[{"role":"user","content":"hi"}],"stream":true,"service_tier":"priority","reasoning_effort":"high"}`, false},
		{"tool", "/v1/responses", `{"model":"target","tools":[{"type":"function","name":"fixture"}]}`, false},
		{"image", "/v1/responses", `{"model":"target","input":[{"role":"user","content":[{"type":"input_image","image_url":"fixture"}]}]}`, false},
		{"unknown field", "/v1/responses", `{"model":"target","unknown_capability":true}`, false},
		{"unknown query", "/v1/responses?fast=true", `{"model":"target"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{ID: 901, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
			report := availabilityAdmissionPass(account.ID, "target")
			report.Models[0].Decision.Scope.Stream = true
			report.Models[0].Decision.Scope.ServiceTier = "priority"
			report.Models[0].Decision.Scope.ReasoningEffort = "high"
			svc := &OpenAIGatewayService{availabilityReader: &availabilityAdmissionRepo{reports: []*AvailabilityReport{report}}}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://fixture.invalid"+tc.path, strings.NewReader(tc.payload))
			require.NoError(t, err)
			err = svc.admitAvailabilityRequest(request.Context(), request, account)
			if tc.allowed {
				require.NoError(t, err)
			} else {
				require.True(t, IsOpenAITurnAdmissionError(err))
			}
		})
	}
}

func TestAvailabilityProbeScopeRequiresActualRequestAssociation(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		name := "direct"
		if redirect {
			name = "redirect"
		}
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if redirect && req.URL.Path == "/v1/responses" {
					http.Redirect(w, req, "/other/responses", http.StatusTemporaryRedirect)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(server.Close)
			collector := requesttiming.New(time.Now(), 0)
			observation := &availabilityProbeObservation{}
			ctx := context.WithValue(requesttiming.With(t.Context(), collector), availabilityProbeSnapshotKey{}, observation)
			payload := `{"model":"target","input":"hi","stream":true}`
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(payload))
			require.NoError(t, err)
			upstream := timedAccountTestUpstream(&runtimeFallbackUpstream{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
				return server.Client().Do(req)
			}})
			response, err := upstream.Do(request, "", 901, 1)
			require.NoError(t, err)
			_, err = io.Copy(io.Discard, response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			if redirect {
				require.Nil(t, observation.scope, "a redirect must not attest to the original wire scope")
			} else {
				require.Equal(t, &AvailabilityRequestScope{Version: 1, Protocol: "responses_http", Stream: true, Model: "target", ServiceTier: "unspecified", ReasoningEffort: "unspecified", Capability: "text"}, observation.scope)
				require.Equal(t, 1, observation.scopeAttempt)
			}
		})
	}
}

func TestAvailabilityRoundScopeRequiresAllSamplesToMatch(t *testing.T) {
	for _, kind := range []string{"same", "missing", "different"} {
		t.Run(kind, func(t *testing.T) {
			plan := pelicanPlan()
			plan.PelicanConfig.ParallelCount = 2
			first := availabilityAdmissionPass(901, plan.ModelID).Models[0].Decision
			second := *first
			copy := *first.Scope
			second.Scope = &copy
			if kind == "missing" {
				second.Scope = nil
			}
			if kind == "different" {
				second.Scope.ServiceTier = "priority"
			}
			outcomes := availabilityRoundOutcomes(plan, []*ScheduledTestResult{
				{PelicanConfig: &PelicanTestConfig{ModelID: plan.ModelID, AvailabilityResult: first}},
				{PelicanConfig: &PelicanTestConfig{ModelID: plan.ModelID, AvailabilityResult: &second}},
			})
			require.Len(t, outcomes, 1)
			require.Equal(t, "pass", outcomes[0].Probe.Status)
			if kind == "same" {
				require.Equal(t, first.Scope, outcomes[0].Probe.Scope)
			} else {
				require.Nil(t, outcomes[0].Probe.Scope)
			}
		})
	}
}

func TestAvailabilityWSBindingRereadsRulesForNewTurns(t *testing.T) {
	account := wsSSETestAccount()
	repo := &availabilityAdmissionRepo{}
	svc := &OpenAIGatewayService{availabilityReader: repo}
	binding := svc.bindOpenAIWSHandshake(account, "gpt-5.1", make(http.Header))
	require.NoError(t, svc.checkOpenAIWSBinding(t.Context(), account, "gpt-5.1", binding), "unconfigured accounts keep the original behavior")
	repo.reports = []*AvailabilityReport{availabilityAdmissionPass(account.ID, "gpt-5.1")}
	require.True(t, IsOpenAITurnAdmissionError(svc.checkOpenAIWSBinding(t.Context(), account, "gpt-5.1", binding)), "a new policy must apply to an already bound connection")
	repo.reports = nil
	require.NoError(t, svc.checkOpenAIWSBinding(t.Context(), account, "gpt-5.1", binding), "explicit policy removal restores legacy eligibility")
	require.Equal(t, 3, repo.reads)
}
