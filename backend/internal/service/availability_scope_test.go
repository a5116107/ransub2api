//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
)

func TestAvailabilityMessagesScopeMatchesOnlyObservedCapabilities(t *testing.T) {
	payload, err := createPelicanClaudePayload("claude-opus-4-6", "probe", "high")
	require.NoError(t, err)
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	newRequest := func(raw []byte) *http.Request {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://fixture.invalid/v1/messages?beta=true", bytes.NewReader(raw))
		require.NoError(t, err)
		req.Header.Set("Anthropic-Version", "2023-06-01")
		return req
	}
	scope := availabilityScopeForRequest(newRequest(body), body)
	require.NotNil(t, scope, "the existing native probe, including ephemeral text caches, must be observable")
	require.Equal(t, "messages_http", scope.Protocol)
	require.Equal(t, "adaptive", scope.ThinkingMode)
	require.Equal(t, "high", scope.ReasoningEffort)
	require.Len(t, scope.ProtocolOptions, 64)
	account := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeAPIKey}
	report := availabilityAdmissionPass(account.ID, "claude-opus-4-6")
	report.Models[0].Decision.Scope = scope
	svc := &OpenAIGatewayService{availabilityReader: &availabilityAdmissionRepo{reports: []*AvailabilityReport{report}}}
	for _, tc := range []struct {
		name, field string
		value       any
		allowed     bool
	}{
		{"same", "", nil, true},
		{"different prompt", "messages.0.content.0.text", "another question", true},
		{"empty tools", "tools", []any{}, true},
		{"no tools", "tool_choice", map[string]any{"type": "none"}, true},
		{"model", "model", "unobserved-model", false},
		{"stream", "stream", false, false},
		{"tier", "service_tier", "standard_only", false},
		{"effort", "output_config.effort", "medium", false},
		{"disabled thinking", "thinking", map[string]any{"type": "disabled"}, false},
		{"budget thinking", "thinking", map[string]any{"type": "enabled", "budget_tokens": 2048}, false},
		{"unknown thinking option", "thinking.display", "omitted", false},
		{"unknown output option", "output_config.format", map[string]any{"type": "json"}, false},
		{"tools", "tools", []any{map[string]any{"name": "fixture"}}, false},
		{"tool result", "messages.0.content", []any{map[string]any{"type": "tool_result", "content": "ok"}}, false},
		{"image", "messages.0.content", []any{map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://fixture.invalid/image"}}}, false},
		{"cache ttl", "messages.0.content.0.cache_control.ttl", "1h", false},
		{"system image", "system", []any{map[string]any{"type": "image", "text": "hi"}}, false},
		{"unknown field", "unknown_capability", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := body
			if tc.field != "" {
				var err error
				raw, err = sjson.SetBytes(body, tc.field, tc.value)
				require.NoError(t, err)
			}
			req := newRequest(raw)
			err := svc.admitAvailabilityRequest(req.Context(), req, account)
			if tc.allowed {
				require.NoError(t, err)
			} else {
				require.True(t, IsOpenAITurnAdmissionError(err), "%v", err)
			}
		})
	}
	for _, header := range []string{"Anthropic-Version", "Anthropic-Beta"} {
		req := newRequest(body)
		req.Header.Set(header, "different-option")
		require.True(t, IsOpenAITurnAdmissionError(svc.admitAvailabilityRequest(req.Context(), req, account)))
		req.Header[http.CanonicalHeaderKey(header)] = []string{"first", "second"}
		require.Nil(t, availabilityScopeForRequest(req, body))
	}
	for _, budget := range []string{"0", "-1", "1.5", "1e3", "9223372036854775808", `"2048"`} {
		raw, err := sjson.SetRawBytes(body, "thinking", []byte(`{"type":"enabled","budget_tokens":`+budget+`}`))
		require.NoError(t, err)
		require.Nil(t, availabilityScopeForRequest(newRequest(raw), raw), budget)
	}
	first, err := sjson.SetBytes(body, "thinking", map[string]any{"type": "enabled", "budget_tokens": 2048})
	require.NoError(t, err)
	second, err := sjson.SetBytes(first, "thinking.budget_tokens", 4096)
	require.NoError(t, err)
	require.NotEqual(t, availabilityScopeForRequest(newRequest(first), first), availabilityScopeForRequest(newRequest(second), second), "different thinking budgets never share evidence")
}

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
	require.Equal(t, &AvailabilityRequestScope{Version: 2, Protocol: "bps_responses", Stream: true, Model: "gpt-5.6-sol", ServiceTier: "unspecified", ReasoningEffort: "medium", Capability: "text"}, observation.scope)
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
	t.Run("legacy authentication evidence", func(t *testing.T) {
		current := report.Models[0].Decision.Scope
		legacy := *current
		legacy.Version = 1
		report.Models[0].Decision.Scope = &legacy
		t.Cleanup(func() { report.Models[0].Decision.Scope = current })
		before := calls
		body := []byte(`{"model":"gpt-5.6-sol","stream":true,"reasoning":{"effort":"medium"},"input":"hi"}`)
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
		_, err := svc.Forward(t.Context(), c, account, body)
		require.Error(t, err)
		require.Equal(t, http.StatusServiceUnavailable, rec.Code)
		require.Equal(t, before, calls, "legacy scope must be reprobed before a BPS send")
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
	report.Models[0].Decision.Scope = &AvailabilityRequestScope{Version: 2, Protocol: "bps_responses", Stream: true, Model: "gpt-5.6-sol", ServiceTier: "unspecified", ReasoningEffort: "medium", Capability: "text"}
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
			}}, nil)
			response, err := upstream.Do(request, "", 901, 1)
			require.NoError(t, err)
			_, err = io.Copy(io.Discard, response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			if redirect {
				require.Nil(t, observation.scope, "a redirect must not attest to the original wire scope")
			} else {
				require.Equal(t, &AvailabilityRequestScope{Version: 2, Protocol: "responses_http", Stream: true, Model: "target", ServiceTier: "unspecified", ReasoningEffort: "unspecified", Capability: "text"}, observation.scope)
				require.Equal(t, 1, observation.scopeAttempt)
			}
		})
	}
}

type availabilityAuthenticationProtocolCase struct {
	name, path, payload, header string
}

func availabilityAuthenticationProtocolCases() []availabilityAuthenticationProtocolCase {
	return []availabilityAuthenticationProtocolCase{
		{"responses", "/v1/responses", `{"model":"target","input":"hi","stream":true}`, "Authorization"},
		{"responses_account", "/v1/responses", `{"model":"target","input":"hi","stream":true}`, "Chatgpt-Account-Id"},
		{"responses_account_alias", "/v1/responses", `{"model":"target","input":"hi","stream":true}`, "X-Openai-Account-Id"},
		{"responses_organization", "/v1/responses", `{"model":"target","input":"hi","stream":true}`, "OpenAI-Organization"},
		{"responses_project", "/v1/responses", `{"model":"target","input":"hi","stream":true}`, "OpenAI-Project"},
		{"chat", "/v1/chat/completions", `{"model":"target","messages":[{"role":"user","content":"hi"}],"stream":true}`, "Authorization"},
		{"messages_key", "/v1/messages", `{"model":"target","messages":[{"role":"user","content":"hi"}],"stream":true}`, "X-Api-Key"},
		{"messages_bearer", "/v1/messages", `{"model":"target","messages":[{"role":"user","content":"hi"}],"stream":true}`, "Authorization"},
		{"gemini_key", "/v1beta/models/target:streamGenerateContent?alt=sse", `{"contents":[{"parts":[{"text":"hi"}]}]}`, "X-Goog-Api-Key"},
		{"gemini_bearer", "/v1internal:streamGenerateContent?alt=sse", `{"model":"target","project":"fixture-project","request":{"contents":[{"parts":[{"text":"hi"}]}]}}`, "Authorization"},
	}
}

func TestAvailabilityHTTPProbeScopeRequiresUnchangedAuthentication(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		_, _ = io.WriteString(w, `{"fixture":"response"}`)
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	tlsServer := httptest.NewTLSServer(handler)
	t.Cleanup(tlsServer.Close)
	for _, protocol := range availabilityAuthenticationProtocolCases() {
		t.Run(protocol.name, func(t *testing.T) {
			for _, tls := range []bool{false, true} {
				mode := "Do"
				targetServer := server
				if tls {
					mode = "DoWithTLS"
					targetServer = tlsServer
				}
				t.Run(mode, func(t *testing.T) {
					for _, scenario := range []string{"same", "case_only", "unrelated", "no_auth", "rewrite", "rewrite_in_place", "remove", "empty", "duplicate", "case_duplicate", "add_alternative", "initial_duplicate", "initial_empty"} {
						t.Run(scenario, func(t *testing.T) {
							collector := requesttiming.New(time.Now(), 0)
							observation := &availabilityProbeObservation{}
							ctx := context.WithValue(requesttiming.With(t.Context(), collector), availabilityProbeSnapshotKey{}, observation)
							request, err := http.NewRequestWithContext(ctx, http.MethodPost, targetServer.URL+protocol.path, strings.NewReader(protocol.payload))
							require.NoError(t, err)
							if scenario != "no_auth" {
								request.Header.Set(protocol.header, "fixture-credential")
							}
							switch scenario {
							case "initial_duplicate":
								request.Header.Add(protocol.header, "fixture-credential")
							case "initial_empty":
								request.Header.Set(protocol.header, "")
							}
							upstream := timedAccountTestUpstream(&codexModelsHTTPUpstreamStub{do: func(sent *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
								actual := sent.Clone(sent.Context())
								if scenario == "rewrite_in_place" {
									actual = sent
								}
								switch scenario {
								case "rewrite", "rewrite_in_place":
									actual.Header.Set(protocol.header, "different-fixture-credential")
								case "remove":
									actual.Header.Del(protocol.header)
								case "empty":
									actual.Header.Set(protocol.header, "")
								case "duplicate":
									actual.Header.Add(protocol.header, "fixture-credential")
								case "case_duplicate":
									actual.Header[strings.ToLower(protocol.header)] = []string{"fixture-credential"}
								case "case_only":
									actual.Header.Del(protocol.header)
									actual.Header[strings.ToLower(protocol.header)] = []string{"fixture-credential"}
								case "unrelated":
									actual.Header.Set("X-Request-Id", "fixture-request")
								case "add_alternative":
									other := "Authorization"
									if protocol.header == other {
										other = "X-Api-Key"
									}
									actual.Header.Set(other, "alternative-fixture-credential")
								}
								return targetServer.Client().Do(actual)
							}}, &Account{ID: 901, Type: AccountTypeAPIKey})
							var response *http.Response
							if tls {
								response, err = upstream.DoWithTLS(request, "", 901, 1, nil)
							} else {
								response, err = upstream.Do(request, "", 901, 1)
							}
							require.NoError(t, err)
							require.NoError(t, response.Body.Close())
							switch scenario {
							case "same", "case_only", "unrelated", "no_auth":
								require.NotNil(t, observation.scope)
								encoded, err := json.Marshal(observation.scope)
								require.NoError(t, err)
								require.NotContains(t, string(encoded), "fixture-credential")
							default:
								require.Nil(t, observation.scope, "a different or ambiguous physical credential must not authorize the original route")
							}
						})
					}
				})
			}
		})
	}
}

func TestAvailabilityAuthenticationScopeUpgradeAndRegionalRecovery(t *testing.T) {
	protocols := append(availabilityAuthenticationProtocolCases(), availabilityAuthenticationProtocolCase{
		name: "websocket", path: "/v1/responses", payload: `{"type":"response.create","model":"target","input":"hi"}`, header: "Authorization",
	})
	for _, protocol := range protocols {
		t.Run(protocol.name, func(t *testing.T) {
			makeRequest := func() *http.Request {
				method, scheme := http.MethodPost, "https"
				if protocol.name == "websocket" {
					method, scheme = http.MethodGet, "wss"
				}
				request, err := http.NewRequestWithContext(t.Context(), method, scheme+"://fixture.invalid"+protocol.path, strings.NewReader(protocol.payload))
				require.NoError(t, err)
				request.Header.Set(protocol.header, "fixture-credential")
				return request
			}
			scope := availabilityScopeForRequest(makeRequest(), []byte(protocol.payload))
			require.NotNil(t, scope)
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true}
			report := availabilityAdmissionPass(account.ID, "target")
			report.PlanID, report.Models[0].Decision.Scope = 1, scope
			primary := &OpenAIGatewayService{availabilityReader: &availabilityAdmissionRepo{reports: []*AvailabilityReport{report}}}
			replica := regionalTestReplica(t, filepath.Join(t.TempDir(), "state"), true)
			reader, err := NewRegionalSchedulerReader(replica)
			require.NoError(t, err)
			scheduler := NewSchedulerSnapshotService(nil, nil, nil, nil, nil)
			scheduler.SetLocalSnapshotReader(reader)
			unavailable := &availabilityAdmissionRepo{readErr: errors.New("primary must remain offline")}
			regional := &OpenAIGatewayService{schedulerSnapshot: scheduler, availabilityReader: unavailable}
			for _, version := range []int{1, 2, 3} {
				scope.Version = version
				require.NoError(t, replica.PublishNext(t.Context(), "test", time.Minute, regionalAvailabilityDoc(t, account, []*AvailabilityReport{report})))
				for _, gateway := range []*OpenAIGatewayService{primary, regional} {
					request := makeRequest()
					err := gateway.admitAvailabilityRequest(request.Context(), request, account)
					if version == 2 {
						require.NoError(t, err, "a newly verified scope must restore admission")
					} else {
						var denied *OpenAITurnAdmissionError
						require.ErrorAs(t, err, &denied, "old or unknown evidence must not authorize sends: version %d", version)
						require.Equal(t, "availability_scope_unconfirmed", denied.Reason)
					}
				}
			}
			require.Zero(t, unavailable.reads, "regional admission must not fall back to primary evidence")
		})
	}
}

func TestAvailabilityManagedProbeScopeRequiresActualCredential(t *testing.T) {
	for _, scenario := range []string{"same", "rewrite", "duplicate", "wrong_initial"} {
		t.Run(scenario, func(t *testing.T) {
			account := &Account{ID: 901, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				AuthConnectionID: "connection-a", AuthConnectionRevision: 7,
				Credentials: map[string]any{"access_token": "synthetic-token"}}
			collector := requesttiming.New(time.Now(), 0)
			observation := &availabilityProbeObservation{}
			ctx := context.WithValue(requesttiming.With(t.Context(), collector), availabilityProbeSnapshotKey{}, observation)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://fixture.invalid/v1/responses", strings.NewReader(`{"model":"target"}`))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer synthetic-token")
			switch scenario {
			case "wrong_initial":
				req.Header.Set("Authorization", "Bearer different-token")
			case "duplicate":
				req.Header["authorization"] = []string{"Bearer different-token"}
			}
			upstream := timedAccountTestUpstream(&runtimeFallbackUpstream{do: func(sent *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
				actual := sent.Clone(sent.Context())
				if scenario == "rewrite" {
					actual.Header.Set("Authorization", "Bearer different-token")
				}
				return &http.Response{StatusCode: http.StatusOK, Request: actual, Header: make(http.Header), Body: http.NoBody}, nil
			}}, account)
			resp, err := upstream.Do(req, "", account.ID, 1)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			if scenario == "same" {
				require.NotNil(t, observation.scope)
				require.NotEmpty(t, observation.scope.ConnectionScope)
			} else {
				require.Nil(t, observation.scope, "unconfirmed wire authentication must not publish connection evidence")
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

func TestAvailabilityRoundAttributesFailuresOnlyToConsistentConnections(t *testing.T) {
	for _, scenario := range []string{"same", "different", "missing_scope", "missing_sample"} {
		t.Run(scenario, func(t *testing.T) {
			plan := pelicanPlan()
			plan.PelicanConfig.ParallelCount = 2
			first := availabilityAdmissionPass(901, plan.ModelID).Models[0].Decision
			first.Status = "fail"
			first.Scope.ConnectionScope = "first"
			second := *first
			second.Status = "pass"
			secondScope := *first.Scope
			second.Scope = &secondScope
			switch scenario {
			case "different":
				second.Scope.ConnectionScope = "second"
			case "missing_scope":
				second.Scope = nil
			}
			results := []*ScheduledTestResult{{PelicanConfig: &PelicanTestConfig{ModelID: plan.ModelID, AvailabilityResult: first}}}
			if scenario != "missing_sample" {
				results = append(results, &ScheduledTestResult{PelicanConfig: &PelicanTestConfig{ModelID: plan.ModelID, AvailabilityResult: &second}})
			}
			outcomes := availabilityRoundOutcomes(plan, results)
			require.Equal(t, "fail", outcomes[0].Probe.Status)
			if scenario == "same" {
				require.Equal(t, first.Scope, outcomes[0].Probe.Scope)
			} else {
				require.Nil(t, outcomes[0].Probe.Scope, "mixed or missing samples must not guess which connection failed")
			}
		})
	}
}

func TestAvailabilityWSBindingRereadsRulesForNewTurns(t *testing.T) {
	account := wsSSETestAccount()
	repo := &availabilityAdmissionRepo{}
	svc := &OpenAIGatewayService{availabilityReader: repo}
	binding := svc.bindOpenAIWSHandshake(account, "gpt-5.1", make(http.Header), "")
	require.NoError(t, svc.checkOpenAIWSBinding(t.Context(), account, "gpt-5.1", binding, nil), "unconfigured accounts keep the original behavior")
	repo.reports = []*AvailabilityReport{availabilityAdmissionPass(account.ID, "gpt-5.1")}
	require.True(t, IsOpenAITurnAdmissionError(svc.checkOpenAIWSBinding(t.Context(), account, "gpt-5.1", binding, nil)), "a new policy must apply to an already bound connection")
	repo.reports = nil
	require.NoError(t, svc.checkOpenAIWSBinding(t.Context(), account, "gpt-5.1", binding, nil), "explicit policy removal restores legacy eligibility")
	require.Equal(t, 3, repo.reads)
}
