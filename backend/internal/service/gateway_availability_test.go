//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func gatewayAvailabilitySend(mode string, request *http.Request, account *Account, limiter *RateLimitService, scheduler *SchedulerSnapshotService, upstream HTTPUpstream) (*http.Response, error) {
	switch mode {
	case "native", "native_tls":
		svc := &GatewayService{schedulerSnapshot: scheduler, httpUpstream: upstream, rateLimitService: limiter}
		if mode == "native_tls" {
			return svc.doGatewayUpstream(request, "", account, nil)
		}
		return svc.doGatewayUpstream(request, "", account)
	case "gemini":
		svc := &GeminiMessagesCompatService{schedulerSnapshot: scheduler, httpUpstream: upstream, rateLimitService: limiter}
		return svc.doGeminiUpstream(request, "", account)
	default:
		svc := &AntigravityGatewayService{schedulerSnapshot: scheduler, rateLimitService: limiter}
		return svc.doAntigravityUpstream(request, "", account, upstream)
	}
}

func TestGatewayAvailabilityRegionalMissingPolicyStopsEveryTransport(t *testing.T) {
	for _, mode := range []string{"native", "native_tls", "gemini", "antigravity"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.WriteString(w, `{}`)
			}))
			t.Cleanup(server.Close)
			upstream := &codexModelsHTTPUpstreamStub{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
				return server.Client().Do(req)
			}}
			scheduler := NewSchedulerSnapshotService(nil, nil, nil, nil, nil)
			scheduler.SetLocalSnapshotReader(NewLocalSchedulerSnapshotStore(nil))
			account := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeAPIKey}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL+"/v1/messages", strings.NewReader(`{"model":"target","messages":[{"role":"user","content":"hi"}]}`))
			require.NoError(t, err)
			response, err := gatewayAvailabilitySend(mode, req, account, nil, scheduler, upstream)
			if response != nil && response.Body != nil {
				require.NoError(t, response.Body.Close())
			}
			require.True(t, IsOpenAITurnAdmissionError(err), "missing regional policy must reject before sending: %v", err)
			require.Zero(t, calls.Load())
		})
	}
}

func TestGatewayAvailabilityStopRecoveryAndRegionalAuthority(t *testing.T) {
	for _, mode := range []string{"native", "native_tls", "gemini", "antigravity"} {
		for _, regional := range []bool{false, true} {
			name := "primary"
			if regional {
				name = "regional"
			}
			t.Run(mode+"/"+name, func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"ok"}]}`)
				}))
				t.Cleanup(server.Close)
				upstream := &codexModelsHTTPUpstreamStub{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
					return server.Client().Do(req)
				}}
				account := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
					Credentials: map[string]any{"base_url": server.URL, "api_key": "fixture-key"}}
				primary := &availabilityAdmissionRepo{}
				limiter := ProvideRateLimitService(nil, nil, &config.Config{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, primary)
				require.Same(t, primary, limiter.availabilityReader)
				var scheduler *SchedulerSnapshotService
				replica := regionalTestReplica(t, filepath.Join(t.TempDir(), "state"), true)
				if regional {
					reader, err := NewRegionalSchedulerReader(replica)
					require.NoError(t, err)
					scheduler = NewSchedulerSnapshotService(nil, nil, nil, nil, nil)
					scheduler.SetLocalSnapshotReader(reader)
					primary.readErr = errors.New("primary must stay offline")
				}
				for _, state := range []string{"pass", "fail", "paused", "expired", "unobserved_model", "unobserved_protocol", "unobserved_thinking", "unobserved_tier", "unobserved_headers", "read_error", "recovery", "no_rules"} {
					t.Run(state, func(t *testing.T) {
						body := `{"model":"target","messages":[{"role":"user","content":"hi"}],"stream":true}`
						req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/v1/messages?beta=true", strings.NewReader(body))
						require.NoError(t, err)
						req.Header.Set("Anthropic-Version", "2023-06-01")
						report := availabilityAdmissionPass(account.ID, "target")
						report.PlanID = 1
						report.Models[0].Decision.Scope = availabilityScopeForRequest(req, []byte(body))
						require.NotNil(t, report.Models[0].Decision.Scope)
						reports := []*AvailabilityReport{report}
						allowed := false
						switch state {
						case "pass", "recovery":
							allowed = true
						case "no_rules":
							reports, allowed = nil, true
						case "fail":
							report.Models[0].Status, report.Models[0].Decision.Status = "fail", "fail"
						case "paused":
							report.Enabled = false
						case "expired":
							expired := time.Now().Add(-time.Second)
							report.Models[0].ValidUntil = &expired
						case "unobserved_model":
							report.Models[0].ModelID = "other-model"
						case "unobserved_protocol":
							report.Models[0].Decision.Scope.Protocol = "responses_http"
						case "unobserved_thinking":
							report.Models[0].Decision.Scope.ThinkingMode = "adaptive"
						case "unobserved_tier":
							report.Models[0].Decision.Scope.ServiceTier = "standard_only"
						case "unobserved_headers":
							req.Header.Set("Anthropic-Beta", "new-feature")
						}
						before := calls.Load()
						if regional {
							raw := regionalAvailabilityDoc(t, account, reports)
							if state == "read_error" {
								raw, err = json.Marshal(RegionalSchedulerDocument{Version: 1, Buckets: []RegionalSchedulerBucket{{
									Bucket: SchedulerBucket{Platform: account.Platform, Mode: SchedulerModeSingle}, Accounts: []*Account{account},
								}}})
								require.NoError(t, err)
							}
							require.NoError(t, replica.PublishNext(t.Context(), "test", time.Minute, raw))
						} else {
							primary.reports, primary.readErr = reports, nil
							if state == "read_error" {
								primary.readErr = errors.New("fixture reader failure")
							}
						}
						response, err := gatewayAvailabilitySend(mode, req, account, limiter, scheduler, upstream)
						if response != nil {
							require.NoError(t, response.Body.Close())
						}
						if allowed {
							require.NoError(t, err)
							require.Equal(t, before+1, calls.Load())
						} else {
							require.True(t, IsOpenAITurnAdmissionError(err), "%v", err)
							require.Equal(t, before, calls.Load(), "no physical request may escape rejection")
						}
					})
				}
				if regional {
					require.Zero(t, primary.reads)
				}
			})
		}
	}
}

func TestGatewayAvailabilityUnknownProtocolAndProbeRecovery(t *testing.T) {
	for _, tc := range []struct{ mode, path string }{
		{"native", "/v1/messages/count_tokens"},
		{"gemini", "/v1beta/models/target:streamGenerateContent?alt=sse"},
		{"antigravity", "/v1internal:streamGenerateContent?alt=sse"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.WriteString(w, `{}`)
			}))
			t.Cleanup(server.Close)
			upstream := &codexModelsHTTPUpstreamStub{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
				return server.Client().Do(req)
			}}
			account := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeAPIKey}
			reader := &availabilityAdmissionRepo{reports: []*AvailabilityReport{availabilityAdmissionPass(1, "target")}}
			limiter := &RateLimitService{availabilityReader: reader}
			for _, state := range []string{"configured", "probe", "no_rules"} {
				ctx := t.Context()
				switch state {
				case "probe":
					ctx = context.WithValue(ctx, qualityProbeContextKey{}, true)
					reader.readErr = errors.New("unhealthy must still be testable")
				case "no_rules":
					reader.reports, reader.readErr = nil, nil
				}
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+tc.path, strings.NewReader(`{"model":"target"}`))
				require.NoError(t, err)
				before, reads := calls.Load(), reader.reads
				response, err := gatewayAvailabilitySend(tc.mode, req, account, limiter, nil, upstream)
				if response != nil {
					require.NoError(t, response.Body.Close())
				}
				if state == "configured" {
					require.True(t, IsOpenAITurnAdmissionError(err))
					require.Equal(t, before, calls.Load())
				} else {
					require.NoError(t, err)
					require.Equal(t, before+1, calls.Load())
					if state == "probe" {
						require.Equal(t, reads, reader.reads, "recovery probes are not blocked by their old availability")
					}
				}
			}
		})
	}
}
