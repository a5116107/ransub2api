//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

func TestQualityMeasurementRoutePhysicalHTTP(t *testing.T) {
	for _, scenario := range []string{"http", "tls-adapter", "managed", "changed-during-send", "wrong-key", "duplicate-auth", "extra-identity", "other-address", "wrong-proxy", "parent", "oauth", "opaque-response", "redirect", "rewritten-body", "synthetic-response", "truncated", "later-opaque-attempt", "later-unwritten-http"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				if scenario == "redirect" && request.URL.Path == "/v1/responses" {
					http.Redirect(w, request, "/redirected", http.StatusTemporaryRedirect)
					return
				}
				if request.Body != nil {
					_, _ = io.Copy(io.Discard, request.Body)
				}
				_, _ = w.Write([]byte(`{"output_text":"41"}`))
			}))
			t.Cleanup(server.Close)
			account := &Account{ID: 901, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "route-fixture-secret", "base_url": server.URL}}
			if scenario == "managed" {
				account.Extra = map[string]any{"openai_connections_managed": true}
				parent := AccountRouteFingerprint(account)
				account.AuthConnectionID, account.AuthConnectionRevision, account.AuthConnectionParentRoute = "private-grant", 7, &parent
			}
			if scenario == "parent" {
				parent := int64(2)
				account.ParentAccountID = &parent
			}
			if scenario == "oauth" {
				account.Type = AccountTypeOAuth
			}
			originalRoute, _ := AvailabilityPreparedRouteFingerprint(account)
			started := time.Now()
			observation := &availabilityProbeObservation{}
			collector := requesttiming.New(started, 0)
			collector.WhenFinished(func(snapshot requesttiming.Snapshot) { observation.snapshot = &snapshot })
			ctx := context.WithValue(requesttiming.With(t.Context(), collector), availabilityProbeSnapshotKey{}, observation)
			payload := []byte(`{"model":"wire-model","input":"sample"}`)
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", bytes.NewReader(payload))
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer route-fixture-secret")
			if scenario == "wrong-key" {
				request.Header.Set("Authorization", "Bearer another-fixture")
			}
			if scenario == "duplicate-auth" {
				request.Header["authorization"] = []string{"Bearer route-fixture-secret"}
			}
			if scenario == "extra-identity" {
				request.Header.Set("OpenAI-Project", "unverified-project")
			}
			if scenario == "other-address" {
				account.Credentials["base_url"] = "https://different.invalid"
			}
			proxy := ""
			if scenario == "wrong-proxy" {
				proxy = "http://different.invalid:8080"
			}
			upstream := timedAccountTestUpstream(&bpsTestUpstream{send: func(req *http.Request, _ string) (*http.Response, error) {
				if scenario == "synthetic-response" {
					return &http.Response{StatusCode: 200, Header: make(http.Header), Request: req, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
				}
				response, sendErr := server.Client().Do(req)
				if response == nil {
					return nil, sendErr
				}
				switch scenario {
				case "changed-during-send":
					account.Credentials["api_key"] = "new-fixture-secret"
				case "opaque-response":
					response.Request = nil
				case "later-unwritten-http":
					_, trace := requesttiming.StartTransport(req)
					trace.Response(&http.Response{StatusCode: 200}, nil)
				case "rewritten-body":
					response.Request = req.Clone(ctx)
					response.Request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(`{"model":"other-model"}`)), nil }
				}
				return response, sendErr
			}}, account)
			var response *http.Response
			if scenario == "tls-adapter" {
				upstream = timedAccountTestUpstream(availabilityWSHTTPUpstream{client: server.Client()}, account)
				response, err = upstream.DoWithTLS(request, proxy, account.ID, 1, nil)
			} else {
				response, err = upstream.Do(request, proxy, account.ID, 1)
			}
			require.NoError(t, err)
			require.NotNil(t, response)
			_, err = io.ReadAll(response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			if scenario == "later-opaque-attempt" {
				_, trace := requesttiming.StartAttempt(request, account.ID, 0)
				trace.Response(&http.Response{StatusCode: 200}, nil)
			}
			requesttiming.Outcome(ctx, "success", false)
			collector.Finish(200, false)
			if scenario == "truncated" {
				observation.snapshot.Truncated = true
			}
			finished := time.Now()
			probe := availabilityProbeResult(started, finished, observation, true, false, nil)
			cfg := &PelicanTestConfig{QuestionKind: "candy", ModelID: "wire-model", AvailabilityResult: probe,
				Quality: &QualityPolicy{Action: QualityActionObserveOnly, ExpectedAnswer: "42"}}
			result := &ScheduledTestResult{Status: "failed", ErrorMessage: "answer_mismatch", StartedAt: started, FinishedAt: finished, PelicanConfig: cfg}
			recordQualityMeasurement(&ScheduledTestPlan{AccountID: account.ID, PelicanConfig: cfg}, result)
			if scenario == "http" || scenario == "tls-adapter" || scenario == "managed" || scenario == "changed-during-send" {
				require.NotNil(t, result.QualityRouteEvidence)
				require.True(t, result.QualityRouteEvidence.MatchesResult(result))
				decoded, decodeErr := hex.DecodeString(result.QualityRouteEvidence.Fingerprint)
				require.NoError(t, decodeErr)
				require.Equal(t, originalRoute[:], decoded, "freeze the send configuration, never re-read it after the call")
			} else {
				require.Nil(t, result.QualityRouteEvidence)
			}
			raw, err := json.Marshal(result)
			require.NoError(t, err)
			for _, private := range []string{"route-fixture-secret", "private-grant", "fingerprint", "connection_revision", "_quality_observed_route"} {
				require.NotContains(t, string(raw), private)
			}
			if scenario == "synthetic-response" {
				require.Zero(t, calls.Load())
			} else if scenario == "redirect" {
				require.EqualValues(t, 2, calls.Load())
			} else {
				require.EqualValues(t, 1, calls.Load())
			}
		})
	}
}

func TestQualityMeasurementRouteManagedSocket(t *testing.T) {
	for _, scenario := range []string{"bound", "revision-changed", "unprepared"} {
		t.Run(scenario, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				conn, err := coderws.Accept(w, request, nil)
				if err != nil {
					return
				}
				defer func() { _ = conn.CloseNow() }()
				ctx, cancel := context.WithTimeout(request.Context(), 5*time.Second)
				defer cancel()
				if _, _, err = conn.Read(ctx); err == nil {
					_ = conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.completed"}`))
				}
			}))
			t.Cleanup(server.Close)
			account := &Account{ID: 901, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "socket-fixture-secret", "base_url": server.URL},
				Extra:       map[string]any{"openai_connections_managed": true}, AuthConnectionID: "private-socket-grant", AuthConnectionRevision: 7}
			parent := AccountRouteFingerprint(account)
			if scenario != "unprepared" {
				account.AuthConnectionParentRoute = &parent
			}
			target := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
			headers := http.Header{"Authorization": {"Bearer socket-fixture-secret"}}
			gateway := &OpenAIGatewayService{}
			binding := gateway.bindOpenAIWSHandshake(account, "wire-model", headers, target)
			wire, _, _, err := newDefaultOpenAIWSClientDialer().Dial(t.Context(), target, headers, "")
			require.NoError(t, err)
			t.Cleanup(func() { _ = wire.(openAIWSForceCloser).CloseNow() })
			if scenario == "revision-changed" {
				account.AuthConnectionRevision++
			}
			started := time.Now()
			collector := requesttiming.New(started, 0)
			observation := &availabilityProbeObservation{}
			collector.WhenFinished(func(snapshot requesttiming.Snapshot) { observation.snapshot = &snapshot })
			ctx := context.WithValue(requesttiming.With(t.Context(), collector), availabilityProbeSnapshotKey{}, observation)
			ctx, written, finish := beginAvailabilityWSProbe(ctx, account, target)
			payload := json.RawMessage(`{"type":"response.create","model":"wire-model","input":"sample"}`)
			require.NoError(t, wire.WriteJSON(ctx, payload))
			written(&openAIWSConn{ws: wire, turnBinding: binding}, payload)
			_, err = wire.ReadMessage(ctx)
			require.NoError(t, err)
			finish(nil)
			requesttiming.Outcome(ctx, "success", false)
			collector.Finish(200, false)
			finished := time.Now()
			cfg := &PelicanTestConfig{QuestionKind: "candy", ModelID: "wire-model", Quality: &QualityPolicy{Action: QualityActionObserveOnly},
				AvailabilityResult: availabilityProbeResult(started, finished, observation, true, false, nil)}
			result := &ScheduledTestResult{StartedAt: started, FinishedAt: finished, PelicanConfig: cfg}
			recordQualityMeasurement(&ScheduledTestPlan{AccountID: account.ID, PelicanConfig: cfg}, result)
			if scenario == "bound" {
				require.NotNil(t, result.QualityRouteEvidence)
				require.Equal(t, "private-socket-grant", result.QualityRouteEvidence.ConnectionID)
				require.EqualValues(t, 7, result.QualityRouteEvidence.ConnectionRevision)
				require.True(t, result.QualityRouteEvidence.MatchesResult(result))
			} else {
				require.Nil(t, result.QualityRouteEvidence, "a live physical socket cannot repair missing or changed grant ownership")
			}
			raw, err := json.Marshal(result)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "private-socket-grant")
			require.NotContains(t, string(raw), "socket-fixture-secret")
		})
	}
}
