//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type availabilityWSHTTPUpstream struct{ client *http.Client }

func (u availabilityWSHTTPUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.client.Do(req)
}

func (u availabilityWSHTTPUpstream) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, id, concurrency)
}

func availabilityWSTestServices(t *testing.T, server *httptest.Server) (*AccountTestService, *OpenAIGatewayService, *Account) {
	t.Helper()
	account := &Account{ID: 901, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{"api_key": "fixture", "base_url": server.URL},
		Extra:       map[string]any{"responses_websockets_v2_enabled": true}}
	cfg := newOpenAIWSExecutionScopeTestConfig()
	upstream := availabilityWSHTTPUpstream{client: server.Client()}
	gateway := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, cache: &stubGatewayCache{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector()}
	t.Cleanup(gateway.CloseOpenAIWSPool)
	svc := &AccountTestService{cfg: cfg, httpUpstream: upstream, openaiGatewayService: gateway,
		accountRepo: &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{account.ID: account}}}}
	return svc, gateway, account
}

func TestAvailabilityWSProbeUsesConfiguredTransport(t *testing.T) {
	var httpSends, wsSends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		events := []string{
			`{"type":"response.created","response":{"id":"resp_ws_probe","model":"target"}}`,
			`{"type":"response.output_text.delta","delta":"21"}`,
			`{"type":"response.completed","response":{"id":"resp_ws_probe","status":"completed","model":"target","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"21"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`,
		}
		if r.Method == http.MethodPost {
			httpSends.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			for _, event := range events {
				if _, err := fmt.Fprintf(w, "data: %s\n\n", event); err != nil {
					return
				}
			}
			return
		}
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
		wsSends.Add(1)
		for _, event := range events {
			if err := conn.Write(ctx, coderws.MessageText, []byte(event)); err != nil {
				return
			}
		}
		_, _, _ = conn.Read(ctx)
	}))
	t.Cleanup(server.Close)
	svc, _, account := availabilityWSTestServices(t, server)
	account.Credentials["model_mapping"] = map[string]any{"alias": "target", "target": "must-not-map-twice"}
	result, err := svc.RunPelicanBackground(t.Context(), account.ID, "alias", &PelicanTestConfig{
		QuestionKind: "candy", Prompt: CandyPrompt, ReasoningEffort: "high", ParallelCount: 1,
		Availability: &AvailabilityPolicy{ValidForSeconds: 60},
	})
	require.NoError(t, err)
	require.Equal(t, "success", result.Status, result.ErrorMessage)
	require.EqualValues(t, 1, wsSends.Load(), "the scheduled availability test must exercise the configured WebSocket transport")
	require.Zero(t, httpSends.Load(), "an ordinary HTTP pass is not WebSocket evidence")
	probe := result.PelicanConfig.AvailabilityResult
	require.Equal(t, "pass", probe.Status)
	require.Equal(t, "responses_ws_v2", probe.Scope.Protocol)
	require.Equal(t, "target", probe.Scope.Model)
	require.Equal(t, "high", probe.Scope.ReasoningEffort)
	require.NotContains(t, strings.TrimSpace(result.ResponseText), "fixture")
}

func TestAvailabilityWSAdmissionScopeAndGrantRevision(t *testing.T) {
	account := &Account{ID: 901, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		AuthConnectionID: "connection-a", AuthConnectionRevision: 7,
		Credentials: map[string]any{"api_key": "fixture"},
		Extra:       map[string]any{"openai_connections_managed": true}}
	headers := http.Header{"Authorization": {"Bearer fixture"}}
	svc := &OpenAIGatewayService{}
	binding := svc.bindOpenAIWSHandshake(account, "target", headers, "wss://fixture.invalid/v1/responses")
	body := []byte(`{"type":"response.create","model":"target","input":"hi","reasoning":{"effort":"high"}}`)
	scope := binding.availability.scope(account, body)
	require.NotNil(t, scope)
	require.True(t, scope.Stream, "WS is streaming even though response.create omits the HTTP stream field")
	require.Equal(t, "responses_ws_v2", scope.Protocol)
	require.NotEmpty(t, scope.ConnectionScope)
	report := availabilityAdmissionPass(account.ID, "target")
	report.Models[0].Decision.Scope = scope
	repo := &availabilityAdmissionRepo{reports: []*AvailabilityReport{report}}
	svc.availabilityReader = repo
	for _, name := range []string{"same", "legacy_scope", "other_grant", "new_revision", "missing_revision", "managed_parent", "wrong_token", "duplicate_auth", "no_binding", "http_evidence", "paused", "stale", "read_error", "probe", "removed"} {
		t.Run(name, func(t *testing.T) {
			candidate, bound := *account, binding
			candidateReport := *report
			candidateReport.Models = append([]AvailabilityModelState(nil), report.Models...)
			repo.reports, repo.readErr = []*AvailabilityReport{&candidateReport}, nil
			ctx := t.Context()
			allowed := false
			switch name {
			case "same":
				allowed = true
			case "legacy_scope":
				decision := *report.Models[0].Decision
				legacy := *scope
				legacy.Version = 1
				decision.Scope = &legacy
				candidateReport.Models[0].Decision = &decision
			case "other_grant":
				candidate.AuthConnectionID = "connection-b"
			case "new_revision":
				candidate.AuthConnectionRevision++
			case "missing_revision":
				candidate.AuthConnectionRevision = 0
			case "managed_parent":
				candidate.AuthConnectionID = ""
			case "wrong_token", "duplicate_auth":
				changed := headers.Clone()
				if name == "wrong_token" {
					changed.Set("Authorization", "Bearer other")
				} else {
					changed["authorization"] = []string{"Bearer other"}
				}
				bound = svc.bindOpenAIWSHandshake(account, "target", changed, "wss://fixture.invalid/v1/responses")
			case "no_binding":
				bound = nil
			case "http_evidence":
				decision := *report.Models[0].Decision
				other := *scope
				other.Protocol = "responses_http"
				decision.Scope = &other
				candidateReport.Models[0].Decision = &decision
			case "paused":
				candidateReport.Enabled = false
			case "stale":
				past := time.Now().Add(-time.Second)
				candidateReport.Models[0].ValidUntil = &past
			case "read_error":
				repo.readErr = errors.New("fixture-state-unavailable")
			case "probe":
				ctx = context.WithValue(ctx, qualityProbeContextKey{}, true)
				repo.readErr, allowed = errors.New("probe does not need an existing pass"), true
			case "removed":
				repo.reports, allowed = nil, true
			}
			err := svc.checkOpenAIWSBinding(ctx, &candidate, "target", bound, body)
			if allowed {
				require.NoError(t, err)
			} else {
				require.True(t, IsOpenAITurnAdmissionError(err), "%s: %v", name, err)
			}
		})
	}
	repo.reports, repo.readErr = []*AvailabilityReport{report}, nil
	for _, value := range []any{body, json.RawMessage(body), openAIWSPreparedJSON(body), map[string]any{
		"type": "response.create", "model": "target", "input": "a new prompt", "reasoning": map[string]any{"effort": "high"},
	}} {
		require.NoError(t, svc.admitAvailabilityWS(t.Context(), account, binding, value), "all existing encoder representations must use the same wire scope")
	}
	for _, tc := range []struct {
		name, field string
		value       any
	}{
		{"model", "model", "other"}, {"tier", "service_tier", "priority"}, {"effort", "reasoning.effort", "low"},
		{"tools", "tools", []any{map[string]any{"type": "function", "name": "fixture"}}},
		{"file", "input", []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_file", "file_id": "file_fixture"}}}}},
		{"encrypted", "input", []any{map[string]any{"type": "reasoning", "encrypted_content": "fixture"}}},
		{"continuation", "previous_response_id", "resp_fixture"}, {"prewarm", "generate", false},
		{"session_update", "type", "session.update"}, {"unknown", "unknown_capability", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := sjson.SetBytes(body, tc.field, tc.value)
			require.NoError(t, err)
			require.True(t, IsOpenAITurnAdmissionError(svc.admitAvailabilityWS(t.Context(), account, binding, payload)))
		})
	}
}

func TestAvailabilityWSProbeRequiresBoundPhysicalHandshake(t *testing.T) {
	for _, name := range []string{"same", "same_api_key", "added_api_key", "changed_api_key", "missing_api_key", "duplicate_api_key", "empty_api_key", "initial_duplicate_api_key", "same_account", "added_account", "changed_account", "missing_account", "duplicate_account", "empty_account", "initial_duplicate_account", "redirect", "credential_rewrite", "host_rewrite", "missing_auth", "opaque_transport", "custom_dialer"} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if name == "redirect" && r.URL.Path == "/v1/responses" {
					http.Redirect(w, r, "/other/responses", http.StatusTemporaryRedirect)
					return
				}
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer func() { _ = conn.CloseNow() }()
				ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
				defer cancel()
				_, _, _ = conn.Read(ctx)
			}))
			t.Cleanup(server.Close)
			account := &Account{ID: 901, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
				AuthConnectionID: "connection-a", AuthConnectionRevision: 7, Credentials: map[string]any{"api_key": "fixture"}}
			target := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
			headers := http.Header{"Authorization": {"Bearer fixture"}}
			if name == "same_api_key" || name == "changed_api_key" || name == "missing_api_key" || name == "initial_duplicate_api_key" {
				headers.Set("X-Api-Key", "fixture-key")
			}
			if name == "initial_duplicate_api_key" {
				headers.Add("X-Api-Key", "fixture-key")
			}
			if name == "same_account" || name == "changed_account" || name == "missing_account" || name == "initial_duplicate_account" {
				headers.Set("Chatgpt-Account-Id", "fixture-account")
			}
			if name == "initial_duplicate_account" {
				headers.Add("Chatgpt-Account-Id", "fixture-account")
			}
			svc := &OpenAIGatewayService{}
			binding := svc.bindOpenAIWSHandshake(account, "target", headers, target)
			wireHeaders := headers.Clone()
			if name == "credential_rewrite" {
				wireHeaders.Set("Authorization", "Bearer other")
			}
			if name == "missing_auth" {
				wireHeaders.Del("Authorization")
			}
			switch name {
			case "added_api_key", "changed_api_key":
				wireHeaders.Set("X-Api-Key", "other-fixture-key")
			case "missing_api_key":
				wireHeaders.Del("X-Api-Key")
			case "duplicate_api_key":
				wireHeaders["X-Api-Key"] = []string{"fixture-key", "fixture-key"}
			case "empty_api_key":
				wireHeaders.Set("X-Api-Key", "")
			case "added_account", "changed_account":
				wireHeaders.Set("Chatgpt-Account-Id", "other-fixture-account")
			case "missing_account":
				wireHeaders.Del("Chatgpt-Account-Id")
			case "duplicate_account":
				wireHeaders["Chatgpt-Account-Id"] = []string{"fixture-account", "fixture-account"}
			case "empty_account":
				wireHeaders.Set("Chatgpt-Account-Id", "")
			}
			dialer := newDefaultOpenAIWSClientDialer()
			if name == "opaque_transport" || name == "host_rewrite" {
				// A plugin may clone/rewrite the request while net/http still
				// reports the original as Response.Request. That is not wire proof.
				client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					changed := req.Clone(req.Context())
					if name == "host_rewrite" {
						changed.Host = "other.invalid"
					} else {
						changed.Header.Set("Authorization", "Bearer other")
					}
					resp, err := server.Client().Transport.RoundTrip(changed)
					if resp != nil && name == "opaque_transport" {
						resp.Request = req
					}
					return resp, err
				})}
				dialer = &coderOpenAIWSClientDialer{proxyClients: map[string]*openAIWSProxyClientEntry{
					"http://fixture.invalid:1": {client: client, lastUsedUnixNano: time.Now().UnixNano()},
				}}
			}
			proxy := ""
			if name == "opaque_transport" || name == "host_rewrite" {
				proxy = "http://fixture.invalid:1"
			}
			wire, _, _, err := dialer.Dial(t.Context(), target, wireHeaders, proxy)
			require.NoError(t, err)
			closer, ok := wire.(openAIWSForceCloser)
			require.True(t, ok)
			t.Cleanup(func() { require.NoError(t, closer.CloseNow()) })
			conn := &openAIWSConn{ws: wire, turnBinding: binding}
			if name == "custom_dialer" {
				conn.ws = &openAIWSCaptureConn{}
			}
			started := time.Now()
			collector := requesttiming.New(started, 0)
			observation := &availabilityProbeObservation{}
			ctx := context.WithValue(requesttiming.With(t.Context(), collector), availabilityProbeSnapshotKey{}, observation)
			ctx, written, finish := beginAvailabilityWSProbe(ctx, account, target)
			written(conn, []byte(`{"type":"response.create","model":"target","input":"hi"}`))
			finish(nil)
			requesttiming.Outcome(ctx, "success", false)
			collector.WhenFinished(func(s requesttiming.Snapshot) { observation.snapshot = &s })
			collector.Finish(http.StatusOK, false)
			result := availabilityProbeResult(started, time.Now(), observation, true, false, nil)
			if name == "same" || name == "same_api_key" || name == "same_account" {
				require.Equal(t, "pass", result.Status)
				require.NotEmpty(t, result.Scope.ConnectionScope)
			} else {
				require.Equal(t, "inconclusive", result.Status)
				require.Nil(t, result.Scope, "only the exact physical socket and its authenticated handshake can attest to a grant")
			}
		})
	}
}

type availabilityWSLiveReader struct {
	report *AvailabilityReport
	failed atomic.Bool
	reads  atomic.Int32
	failAt atomic.Int32
}

func (r *availabilityWSLiveReader) ReadAccountAvailability(context.Context, *Account) ([]*AvailabilityReport, error) {
	reads := r.reads.Add(1)
	report := *r.report
	report.Models = append([]AvailabilityModelState(nil), report.Models...)
	if failAt := r.failAt.Load(); r.failed.Load() || (failAt > 0 && reads >= failAt) {
		report.Models[0].Status = "fail"
	}
	return []*AvailabilityReport{&report}, nil
}

func TestAvailabilityWSProbeToBusinessAndPooledRecovery(t *testing.T) {
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
			id := sends.Add(1)
			for _, event := range []string{`{"type":"response.output_text.delta","delta":"21"}`,
				fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","model":"target","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`, id)} {
				if err := conn.Write(ctx, coderws.MessageText, []byte(event)); err != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(server.Close)
	tester, gateway, account := availabilityWSTestServices(t, server)
	result, err := tester.RunPelicanBackground(t.Context(), account.ID, "target", &PelicanTestConfig{
		QuestionKind: "candy", Prompt: CandyPrompt, ReasoningEffort: "high", ParallelCount: 1, Availability: &AvailabilityPolicy{ValidForSeconds: 60},
	})
	require.NoError(t, err)
	require.Equal(t, "pass", result.PelicanConfig.AvailabilityResult.Status)
	report := availabilityAdmissionPass(account.ID, "target")
	report.Models[0].Decision = result.PelicanConfig.AvailabilityResult
	reader := &availabilityWSLiveReader{report: report}
	gateway.availabilityReader = reader
	body := []byte(`{"model":"target","input":"a new prompt","stream":true,"reasoning":{"effort":"high"}}`)
	send := func(payload []byte) error {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		c.Request.Header.Set("Session-Id", "availability-ws-business")
		_, err := gateway.Forward(t.Context(), c, account, payload)
		return err
	}
	require.NoError(t, send(body), "a completed native WS probe must admit a matching business frame")
	before := sends.Load()
	reader.failed.Store(true)
	require.True(t, IsOpenAITurnAdmissionError(send(body)), "a reused connection must reread the latest state")
	require.Equal(t, before, sends.Load(), "a stopped route must send zero new frames")
	reader.failed.Store(false)
	require.NoError(t, send(body), "fresh passing state restores the original route")
	before = sends.Load()
	for _, field := range []string{"tools", "previous_response_id", "service_tier", "reasoning.effort"} {
		var value any = "priority"
		switch field {
		case "tools":
			value = []any{map[string]any{"type": "function", "name": "fixture", "parameters": map[string]any{"type": "object"}}}
		case "previous_response_id":
			value = "resp_fixture"
		case "reasoning.effort":
			value = "low"
		}
		payload, err := sjson.SetBytes(body, field, value)
		require.NoError(t, err)
		require.True(t, IsOpenAITurnAdmissionError(send(payload)), "%s is not the observed text scope", field)
	}
	require.Equal(t, before, sends.Load())
	reader.failAt.Store(reader.reads.Load() + 2)
	require.True(t, IsOpenAITurnAdmissionError(send(body)), "a pass before the handshake is not permission after a wait")
	require.Equal(t, before, sends.Load())
}

func TestAvailabilityWSNativeIngressRechecksEachTurn(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModeDedicated, OpenAIWSIngressModePassthrough} {
		for _, policy := range []string{"pass", "force_priority", "filter_priority"} {
			t.Run(mode+"/"+policy, func(t *testing.T) {
				cfg := passthroughLifecycleConfig()
				upstream := newStagedPassthroughConn()
				svc := newPassthroughLifecycleService(cfg, upstream)
				settings := DefaultOpenAIFastPolicySettings()
				switch policy {
				case "force_priority":
					settings = &OpenAIFastPolicySettings{Rules: []OpenAIFastPolicyRule{{ServiceTier: OpenAIFastTierMissing, Action: OpenAIFastPolicyActionForcePriority, Scope: BetaPolicyScopeAll}}}
				case "filter_priority":
					settings = openAIFastFilterPriorityPolicy()
				}
				svc.settingService = newOpenAIGatewayServiceWithSettings(t, settings).settingService
				pool := newOpenAIWSConnPool(cfg)
				pool.setClientDialerForTest(&stagedPassthroughDialer{conn: &availabilityWSStagedWriter{upstream}})
				t.Cleanup(pool.Close)
				svc.openaiWSPool = pool
				account := passthroughLifecycleAccount()
				account.Extra["openai_apikey_responses_websockets_v2_mode"] = mode
				report := availabilityAdmissionPass(account.ID, "gpt-5.1")
				report.Models[0].Decision.Scope.Protocol = "responses_ws_v2"
				report.Models[0].Decision.Scope.Stream = true
				if policy == "force_priority" {
					report.Models[0].Decision.Scope.ServiceTier = "priority"
				}
				reader := &availabilityWSLiveReader{report: report}
				svc.availabilityReader = reader
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				server, serverErr := startPassthroughLifecycleServer(t, ctx, svc, account)
				t.Cleanup(func() { cancel(); server.Close() })
				payload := `{"type":"response.create","model":"gpt-5.1","stream":true,"input":"hi"}`
				if policy == "filter_priority" {
					payload = `{"type":"response.create","model":"gpt-5.1","service_tier":"priority","stream":true,"input":"hi"}`
				}
				client := dialPassthroughLifecycleClientWithPayload(t, server, payload)
				t.Cleanup(func() { _ = client.CloseNow() })
				assertWrite := func() {
					t.Helper()
					select {
					case wire := <-upstream.writes:
						if policy == "force_priority" {
							require.Equal(t, "priority", gjson.GetBytes(wire, "service_tier").String())
						} else {
							require.False(t, gjson.GetBytes(wire, "service_tier").Exists())
						}
					case <-time.After(3 * time.Second):
						t.Fatal("an admitted turn did not reach upstream")
					}
				}
				assertWrite()
				upstream.Send(`{"type":"response.output_text.delta","delta":"21","response_id":"fixture"}`)
				upstream.Send(`{"type":"response.completed","response":{"id":"fixture","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`)
				for range 2 {
					_, err := readPassthroughLifecycleFrame(t, client, 3*time.Second)
					require.NoError(t, err)
				}
				require.NoError(t, client.Write(t.Context(), coderws.MessageText, []byte(payload)))
				assertWrite()
				upstream.Send(`{"type":"response.output_text.delta","delta":"21","response_id":"fixture_second"}`)
				upstream.Send(`{"type":"response.completed","response":{"id":"fixture_second","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`)
				for range 2 {
					_, err := readPassthroughLifecycleFrame(t, client, 3*time.Second)
					require.NoError(t, err, "later turns must match the post-policy wire tier, just like the first turn")
				}
				reader.failed.Store(true)
				require.NoError(t, client.Write(t.Context(), coderws.MessageText, []byte(payload)))
				_, readErr := readPassthroughLifecycleFrame(t, client, 3*time.Second)
				require.Error(t, readErr, "the next turn must be denied on the existing client connection")
				select {
				case err := <-serverErr:
					require.Error(t, err)
				case <-time.After(5 * time.Second):
					t.Fatal("native WS admission did not stop the next turn")
				}
				require.Empty(t, upstream.writes, "the failed third turn must never reach upstream")
			})
		}
	}
}

var _ AccountAvailabilityReader = (*availabilityWSLiveReader)(nil)

// The existing staged fixture records passthrough writes; record pooled JSON
// writes in the same channel so both native ingress modes share the assertion.
type availabilityWSStagedWriter struct{ *stagedPassthroughConn }

func (c *availabilityWSStagedWriter) WriteJSON(ctx context.Context, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.WriteFrame(ctx, coderws.MessageText, raw)
}

func TestAvailabilityWSRequestScopeRejectsUnobservedWire(t *testing.T) {
	for _, target := range []string{"wss://fixture.invalid/v1/responses?fast=true", "wss://fixture.invalid/v1/responses#fragment", "wss://fixture.invalid/v1/chat/completions", "https://fixture.invalid/v1/responses"} {
		t.Run(target, func(t *testing.T) {
			address, err := url.Parse(target)
			require.NoError(t, err)
			require.Nil(t, availabilityScopeForRequest(&http.Request{Method: http.MethodGet, URL: address, Header: make(http.Header)}, []byte(`{"type":"response.create","model":"target"}`)))
		})
	}
}

func TestAvailabilityWSProbeFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name, want, reason string
		err                error
		sent               bool
	}{
		{"local_admission", "inconclusive", "response_not_confirmed", denyOpenAITurn("fixture-local-denial"), false},
		{"unknown_dial", "inconclusive", "response_not_confirmed", &openAIWSDialError{Err: errors.New("fixture-local-error")}, false},
		{"network_dial", "fail", "transport_error", &openAIWSDialError{networkAttempted: true, Err: errors.New("fixture-network-error")}, false},
		{"handshake_auth", "fail", "authentication_rejected", &openAIWSDialError{StatusCode: 401, Err: errors.New("fixture-auth")}, false},
		{"handshake_rate", "fail", "rate_limited", &openAIWSDialError{StatusCode: 429, Err: errors.New("fixture-rate")}, false},
		{"handshake_request", "inconclusive", "request_rejected", &openAIWSDialError{StatusCode: 400, Err: errors.New("fixture-request")}, false},
		{"truncated", "fail", "transport_error", errors.New("fixture-truncated-stream"), true},
		{"timeout", "fail", "upstream_timeout", context.DeadlineExceeded, true},
		{"canceled", "inconclusive", "canceled", context.Canceled, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := time.Now()
			collector := requesttiming.New(started, 0)
			observation := &availabilityProbeObservation{}
			ctx := context.WithValue(requesttiming.With(t.Context(), collector), availabilityProbeSnapshotKey{}, observation)
			ctx, written, finish := beginAvailabilityWSProbe(ctx, &Account{ID: 901}, "wss://fixture.invalid/v1/responses")
			if tc.sent {
				written(&openAIWSConn{ws: &coderOpenAIWSClientConn{}}, nil)
			} else if tc.name == "unknown_dial" {
				written(&openAIWSConn{ws: &openAIWSCaptureConn{}}, nil)
			}
			finish(tc.err)
			requesttiming.Outcome(ctx, "error", false)
			collector.WhenFinished(func(s requesttiming.Snapshot) { observation.snapshot = &s })
			collector.Finish(http.StatusOK, false)
			result := availabilityProbeResult(started, time.Now(), observation, false, false, nil)
			require.Equal(t, tc.want, result.Status)
			require.Equal(t, tc.reason, result.Reason)
		})
	}
}

func TestAvailabilityWSProbeFallbackDoesNotInheritSemanticFailure(t *testing.T) {
	started := time.Now()
	collector := requesttiming.New(started, 0)
	observation := &availabilityProbeObservation{}
	ctx := context.WithValue(requesttiming.With(t.Context(), collector), availabilityProbeSnapshotKey{}, observation)
	wsCtx, written, finish := beginAvailabilityWSProbe(ctx, &Account{ID: 901}, "wss://fixture.invalid/v1/responses")
	written(&openAIWSConn{ws: &coderOpenAIWSClientConn{}}, nil)
	observeAvailabilityProbeFailure(wsCtx, []byte(`{"type":"error","error":{"code":"server_error","message":"fixture-earlier-error"}}`), "fixture-earlier-error")
	finish(errors.New("fixture-earlier-error"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(server.Close)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"target","input":"hi","stream":true}`))
	require.NoError(t, err)
	response, err := timedAccountTestUpstream(availabilityWSHTTPUpstream{server.Client()}, &Account{ID: 901}).Do(request, "", 901, 1)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	requesttiming.Outcome(ctx, "success", false)
	collector.WhenFinished(func(s requesttiming.Snapshot) { observation.snapshot = &s })
	collector.Finish(http.StatusOK, false)
	result := availabilityProbeResult(started, time.Now(), observation, true, false, nil)
	require.Equal(t, "pass", result.Status, "only the final completed attempt determines availability")
	require.Zero(t, result.SemanticStatus, "an earlier WS error is not evidence against a later HTTP response")
	require.Equal(t, "responses_http", result.Scope.Protocol, "fallback success must never attest to WS")
	require.Equal(t, 2, result.AttemptCount)
}
