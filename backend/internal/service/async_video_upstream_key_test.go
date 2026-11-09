//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/stretchr/testify/require"
)

type asyncVideoKeyGuardStub struct {
	HTTPUpstream
	supported bool
}

type asyncVideoBindStub struct {
	HTTPUpstream
	AsyncVideoKeyLeaseController
	bind func(context.Context, *AsyncVideoKeyHandoff, string) error
}

func (s *asyncVideoBindStub) BindAsyncVideoKeyLease(ctx context.Context, handoff *AsyncVideoKeyHandoff, taskID string) error {
	return s.bind(ctx, handoff, taskID)
}

func TestAsyncVideoKeyBindRecoversLostAcknowledgement(t *testing.T) {
	handoff := NewAsyncVideoKeyHandoff(AsyncVideoKeyOwner{})
	handoff.SetLease(&PersistentUpstreamKeyLease{})
	req := httptest.NewRequest(http.MethodPost, "/fixture", nil)
	req = req.WithContext(WithAsyncVideoKeyHandoff(req.Context(), handoff))
	for _, tc := range []struct {
		name     string
		errors   []error
		want     error
		attempts int
	}{
		{"lost acknowledgement", []error{io.ErrUnexpectedEOF, nil}, nil, 2},
		{"policy refusal", []error{ErrUpstreamKeyPolicy}, ErrUpstreamKeyPolicy, 1},
		{"lease refusal", []error{ErrUpstreamKeyLease}, ErrUpstreamKeyLease, 1},
		{"still unavailable", []error{io.ErrUnexpectedEOF, io.EOF}, io.EOF, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			var deadlineBounded bool
			stub := &asyncVideoBindStub{bind: func(ctx context.Context, got *AsyncVideoKeyHandoff, taskID string) error {
				calls++
				require.Same(t, handoff, got)
				require.Equal(t, "task-fixture", taskID)
				_, deadlineBounded = ctx.Deadline()
				return tc.errors[calls-1]
			}}
			svc := &OpenAIGatewayService{httpUpstream: stub}
			err := svc.bindAsyncVideoKeyLease(context.Background(), req, " task-fixture ")
			if tc.want == nil {
				require.NoError(t, err)
			} else {
				require.True(t, errors.Is(err, tc.want), "%v", err)
			}
			require.Equal(t, tc.attempts, calls)
			require.True(t, deadlineBounded, "binding recovery must have a bounded total wait")
		})
	}
}

func TestAsyncVideoKeyBindStopsAtDeadline(t *testing.T) {
	handoff := NewAsyncVideoKeyHandoff(AsyncVideoKeyOwner{})
	handoff.SetLease(&PersistentUpstreamKeyLease{})
	req := httptest.NewRequest(http.MethodPost, "/fixture", nil)
	req = req.WithContext(WithAsyncVideoKeyHandoff(req.Context(), handoff))
	var calls int
	stub := &asyncVideoBindStub{bind: func(ctx context.Context, _ *AsyncVideoKeyHandoff, _ string) error {
		calls++
		<-ctx.Done()
		return ctx.Err()
	}}
	svc := &OpenAIGatewayService{httpUpstream: stub}
	started := time.Now()
	err := svc.bindAsyncVideoKeyLease(context.Background(), req, "task")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, 1, calls)
	require.Less(t, time.Since(started), 5*time.Second)
}

func TestAsyncVideoKeyGuardPreservesExistingTaskOperations(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	for _, tc := range []struct {
		name     string
		endpoint GrokMediaEndpoint
		platform string
		method   string
		taskID   string
	}{
		{"grok-status", GrokMediaEndpointVideoStatus, PlatformGrok, http.MethodGet, "task"},
		{"seedance-status", SeedanceEndpointStatus, PlatformOpenAI, http.MethodGet, SeedanceTaskKey("task")},
		{"seedance-delete", SeedanceEndpointDelete, PlatformOpenAI, http.MethodDelete, SeedanceTaskKey("task")},
		{"minimax-status", MiniMaxH3EndpointStatus, PlatformMiniMax, http.MethodGet, MiniMaxH3TaskKey("task")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Gateway.UpstreamKeyConcurrency.Revision = 1
			upstream := &grokMediaContentUpstreamStub{response: grokMediaContentStatusResponse(`{"id":"task","request_id":"task","status":"queued"}`)}
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
			account := &Account{ID: 1, Platform: tc.platform, Type: AccountTypeAPIKey, Credentials: map[string]any{
				"api_key": "fixture", "base_url": "https://fixture.example/v1", "openai_capabilities": []string{"seedance"},
			}}
			c, _ := grokMediaContentTestContext(tc.method, "/fixture", nil)
			var err error
			switch tc.platform {
			case PlatformOpenAI:
				_, err = svc.ForwardSeedance(context.Background(), c, account, tc.endpoint, tc.taskID, nil)
			case PlatformMiniMax:
				_, err = svc.ForwardMiniMaxH3(context.Background(), c, account, tc.endpoint, tc.taskID, nil)
			default:
				_, err = svc.ForwardGrokMedia(context.Background(), c, account, tc.endpoint, tc.taskID, nil, "")
			}
			require.NoError(t, err)
			require.Len(t, upstream.requests, 1)
			require.Equal(t, tc.method, upstream.request.Method)
		})
	}
}

func (s *asyncVideoKeyGuardStub) SupportsAsyncVideoKeyGuard() bool { return s.supported }

func TestAsyncVideoKeyGuardRequestCapability(t *testing.T) {
	for _, endpoint := range []GrokMediaEndpoint{GrokMediaEndpointVideosGenerations, GrokMediaEndpointVideosEdits,
		GrokMediaEndpointVideosExtensions, SeedanceEndpointCreate, MiniMaxH3EndpointCreate} {
		t.Run(string(endpoint), func(t *testing.T) {
			for _, policy := range []string{"disabled", "shared", "regional_concurrency", "regional_scheduler"} {
				t.Run(policy, func(t *testing.T) {
					cfg := &config.Config{}
					switch policy {
					case "shared":
						cfg.Gateway.UpstreamKeyConcurrency.Revision = 1
					case "regional_concurrency":
						cfg.Runtime.RegionalConcurrencyFile = "fixture"
					case "regional_scheduler":
						cfg.Runtime.RegionalSchedulerFile = "fixture"
					}
					req := httptest.NewRequest(http.MethodPost, "/fixture", nil)
					for _, supported := range []bool{false, true} {
						svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: &asyncVideoKeyGuardStub{supported: supported}}
						got, err := svc.guardAsyncVideoKeyRequest(req, endpoint, nil)
						if policy != "disabled" && !supported {
							require.Nil(t, got)
							require.True(t, IsUpstreamKeyAdmissionFailure(err), "%v", err)
							continue
						}
						require.NoError(t, err)
						require.True(t, HTTPUpstreamRedirectsDisabled(got.Context()), "video creation must not replay through a redirect")
						active, required := AsyncVideoKeyGuardState(got)
						require.Equal(t, supported, active)
						require.Equal(t, supported && policy != "disabled", required)
						original, _ := AsyncVideoKeyGuardState(req)
						require.False(t, original, "the caller's request must remain unchanged")
						require.False(t, HTTPUpstreamRedirectsDisabled(req.Context()))
					}
				})
			}
		})
	}

	cfg := &config.Config{}
	cfg.Gateway.UpstreamKeyConcurrency.Revision = 1
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: &asyncVideoKeyGuardStub{supported: true}}
	for _, endpoint := range []GrokMediaEndpoint{GrokMediaEndpointImagesGenerations, GrokMediaEndpointVideoStatus,
		GrokMediaEndpointVideoContent, SeedanceEndpointStatus, SeedanceEndpointDelete, MiniMaxH3EndpointStatus, MiniMaxH3EndpointContent} {
		req := httptest.NewRequest(http.MethodGet, "/fixture", nil)
		got, err := svc.guardAsyncVideoKeyRequest(req, endpoint, nil)
		require.NoError(t, err)
		require.Same(t, req, got, "existing task operations and synchronous images must retain their HTTP behavior")
	}
}

func TestAsyncVideoTerminalResponseEvidence(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	for _, route := range []struct {
		name, terminal string
		endpoint       GrokMediaEndpoint
	}{
		{"grok-status", "done", GrokMediaEndpointVideoStatus},
		{"grok-content", "done", GrokMediaEndpointVideoContent},
		{"seedance-status", "succeeded", SeedanceEndpointStatus},
		{"seedance-cancel", "cancelled", SeedanceEndpointDelete},
		{"minimax-status", "completed", MiniMaxH3EndpointStatus},
	} {
		t.Run(route.name, func(t *testing.T) {
			for _, tc := range []struct {
				name, body      string
				valid, terminal bool
			}{
				{"matching", `{"id":"task-fixture","status":"%s","video":{"url":"https://vidgen.x.ai/result.mp4"}}`, true, true},
				{"no echoed ID", `{"status":"%s","video":{"url":"https://vidgen.x.ai/result.mp4"}}`, true, true},
				{"pending", `{"id":"task-fixture","status":"pending","FutureField":{"keep":true}}`, true, false},
				{"wrong task", `{"id":"another-task","status":"%s"}`, false, false},
				{"duplicate ID", `{"id":"task-fixture","id":"another-task","status":"%s"}`, false, false},
				{"duplicate same ID", `{"id":"task-fixture","id":"task-fixture","status":"%s"}`, false, false},
				{"duplicate status", `{"id":"task-fixture","status":"%s","status":"pending"}`, false, false},
				{"escaped duplicate", `{"status":"%s","sta\u0074us":"pending"}`, false, false},
				{"case variant", `{"Status":"pending","status":"%s"}`, false, false},
				{"numeric ID", `{"id":123,"status":"%s"}`, false, false},
				{"null ID", `{"id":null,"status":"%s"}`, false, false},
				{"numeric status", `{"id":"task-fixture","status":123}`, false, false},
				{"truncated", `{"id":"task-fixture","status":"%s"`, false, false},
				{"trailing garbage", `{"id":"task-fixture","status":"%s"}garbage`, false, false},
				{"multiple objects", `{"id":"task-fixture","status":"%s"}{"status":"pending"}`, false, false},
				{"array", `[{"id":"task-fixture","status":"%s"}]`, false, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					body := tc.body
					if strings.Contains(body, "%s") {
						body = fmt.Sprintf(body, route.terminal)
					}
					upstream := &grokMediaContentUpstreamStub{responses: []*http.Response{
						grokMediaContentStatusResponse(body), grokMediaContentStatusResponse("video-payload"),
					}}
					svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
					c, recorder := grokMediaContentTestContext(http.MethodGet, "/fixture", nil)
					var result *OpenAIForwardResult
					var err error
					switch route.endpoint {
					case SeedanceEndpointStatus, SeedanceEndpointDelete:
						result, err = svc.ForwardSeedance(t.Context(), c, seedanceTestAccount(), route.endpoint, "seedance:task-fixture", nil)
					case MiniMaxH3EndpointStatus:
						result, err = svc.ForwardMiniMaxH3(t.Context(), c, miniMaxH3TestAccount(), route.endpoint, "minimax-h3:task-fixture", nil)
					default:
						result, err = svc.ForwardGrokMedia(t.Context(), c, grokMediaContentTestAccount(), route.endpoint, "task-fixture", nil, "")
					}
					if tc.valid {
						require.NoError(t, err)
						require.Equal(t, tc.terminal, result.AsyncVideoTerminal)
					} else {
						require.Error(t, err, "ambiguous provider evidence must not authorize release or billing")
						require.Nil(t, result)
						require.Empty(t, recorder.Body.String(), "do not forward another task's payload")
						require.Len(t, upstream.requests, 1, "an invalid status must not start a content fetch")
					}
				})
			}
		})
	}
}

func TestAsyncVideoResponseEvidenceTaskAliases(t *testing.T) {
	t.Run("unknown dotted members retain compatibility", func(t *testing.T) {
		body := []byte(`{"id":"task-fixture","status":"done","data.id":"unrelated","video.task_id":"unrelated","FutureField":{"id":"unrelated"},"video":{"url":"https://vidgen.x.ai/result.mp4"}}`)
		id, status, err := asyncVideoResponseEvidence(body, "grok", "task-fixture")
		require.NoError(t, err)
		require.Equal(t, "task-fixture", id)
		require.Equal(t, "done", status)
		require.NotNil(t, ExtractGrokVideoBillingFromStatusBody(body, nil, "task-fixture"))
	})
	t.Run("pending nullable containers", func(t *testing.T) {
		id, status, err := asyncVideoResponseEvidence([]byte(`{"status":"pending","data":null,"video":null}`), "grok", "task-fixture")
		require.NoError(t, err)
		require.Empty(t, id)
		require.Equal(t, "pending", status)
	})
	for _, container := range []string{"", "data", "video"} {
		for _, field := range []string{"request_id", "id", "task_id"} {
			for _, tc := range []struct {
				name, value string
				valid       bool
			}{
				{"string", `"task-fixture"`, true},
				{"number", `123`, false},
				{"null", `null`, false},
				{"empty", `" "`, false},
			} {
				t.Run(container+"/"+field+"/"+tc.name, func(t *testing.T) {
					member := fmt.Sprintf(`%q:%s`, field, tc.value)
					if container == "video" {
						member += `,"url":"https://vidgen.x.ai/result.mp4"`
					}
					if container != "" {
						member = fmt.Sprintf(`%q:{%s}`, container, member)
					}
					if container != "video" {
						member += `,"video":{"url":"https://vidgen.x.ai/result.mp4"}`
					}
					body := []byte(`{` + member + `,"status":"done"}`)
					for _, expectedID := range []string{"", "task-fixture"} {
						id, status, err := asyncVideoResponseEvidence(body, "grok", expectedID)
						if !tc.valid {
							require.Error(t, err)
							require.Empty(t, extractGrokMediaVideoRequestID(body))
							continue
						}
						require.NoError(t, err)
						require.Equal(t, "task-fixture", id)
						require.Equal(t, "done", status)
						require.Equal(t, id, extractGrokMediaVideoRequestID(body))
						require.NotNil(t, ExtractGrokVideoBillingFromStatusBody(body, nil, "task-fixture"))
					}
					_, _, err := asyncVideoResponseEvidence(body, "grok", "other-task")
					require.Error(t, err, "no identity alias may authorize a different requested task")
				})
			}
		}
	}
	for _, body := range []string{
		`{"id":"task-fixture","data":{"request_id":"other-task"},"status":"done"}`,
		`{"video":{"id":"task-fixture","task_id":"other-task"},"status":"done"}`,
		`{"data":{"id":"task-fixture"},"data":{"id":"task-fixture"},"status":"done"}`,
		`{"video":{"id":"task-fixture","i\u0064":"task-fixture"},"status":"done"}`,
		`{"data":{"ID":"task-fixture"},"status":"done"}`,
		`{"task_id":"task-fixture","id":"other-task","status":"done","video":{"url":"https://vidgen.x.ai/result.mp4"}}`,
	} {
		t.Run(body, func(t *testing.T) {
			for _, expectedID := range []string{"", "task-fixture"} {
				_, _, err := asyncVideoResponseEvidence([]byte(body), "grok", expectedID)
				require.Error(t, err)
			}
			require.Empty(t, extractGrokMediaVideoRequestID([]byte(body)))
			require.False(t, grokVideoStatusTerminal([]byte(body)))
			require.Nil(t, ExtractGrokVideoBillingFromStatusBody([]byte(body), nil, "task-fixture"))
		})
	}
}
