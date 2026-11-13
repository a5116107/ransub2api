//go:build unit

package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type affinityAvailabilityReader struct {
	mu     sync.Mutex
	states map[int64]string
	stream bool
	reads  []int64
}

func (r *affinityAvailabilityReader) setState(id int64, state string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states[id] = state
}

func (r *affinityAvailabilityReader) readIDs() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int64(nil), r.reads...)
}

func (r *affinityAvailabilityReader) ReadAccountAvailability(_ context.Context, account *service.Account) ([]*service.AvailabilityReport, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads = append(r.reads, account.ID)
	now := time.Now()
	until := now.Add(time.Minute)
	status := r.states[account.ID]
	if status == "read_error" {
		return nil, errors.New("fixture availability authority failure")
	}
	report := &service.AvailabilityReport{AccountID: account.ID, Configured: true, Enabled: true, EvaluatedAt: now,
		Models: []service.AvailabilityModelState{{ModelID: "gpt-6-astra", Status: status, AvailabilityState: service.AvailabilityState{
			ValidUntil: &until, Decision: &service.AvailabilityProbeResult{Version: 1, Status: status, Scope: &service.AvailabilityRequestScope{
				Version: 2, Protocol: "responses_http", Stream: r.stream, Model: "gpt-6-astra", ServiceTier: "unspecified", ReasoningEffort: "unspecified", Capability: "text",
			}},
		}}}}
	switch status {
	case "stale":
		until = now.Add(-time.Minute)
	case "unknown":
		report.Models[0].Decision = nil
	case "paused":
		report.Enabled = false
	case "unobserved":
		report.Models[0].ModelID = "different-model"
	}
	return []*service.AvailabilityReport{report}, nil
}

// Exercise the real handler, scheduler, Redis binding and HTTP send together.
// A monitor rejection before the first physical send must permit a bounded
// same-pool reselect; recovery must not take a healthy migrated session back.
func TestOpenAIAvailabilityAffinityFailoverAndRecovery(t *testing.T) {
	for _, state := range []string{"fail", "stale", "unknown", "paused", "unobserved"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", state, stream), func(t *testing.T) {
				var sends atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					sends.Add(1)
					_, _ = io.Copy(io.Discard, req.Body)
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"done\",\"output_index\":0,\"content_index\":0}\n\n")
						resp := excelBPSCompleted()
						defer func() { _ = resp.Body.Close() }()
						_, _ = io.Copy(w, resp.Body)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"resp_local_affinity","object":"response","status":"completed","model":"gpt-6-astra","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":8,"output_tokens":1,"input_tokens_details":{"cached_tokens":4}}}`)
				}))
				t.Cleanup(upstream.Close)
				redisServer := miniredis.RunT(t)
				client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
				t.Cleanup(func() { _ = client.Close() })
				cache := repository.NewGatewayCache(client)
				repo := &initialAdmissionRepo{}
				for id := int64(1); id <= 2; id++ {
					repo.accounts = append(repo.accounts, service.Account{ID: id, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
						Status: service.StatusActive, Schedulable: true, GroupIDs: []int64{3131}, Priority: int(id), Concurrency: 1,
						Credentials: map[string]any{"api_key": fmt.Sprintf("synthetic-supplier-%d", id), "base_url": upstream.URL}, Extra: map[string]any{"openai_passthrough": true}})
				}
				cfg := &config.Config{RunMode: config.RunModeSimple}
				cfg.Security.URLAllowlist.AllowInsecureHTTP = true
				cfg.Security.URLAllowlist.AllowPrivateHosts = true
				priority := service.DefaultPrioritySchedulingConfig()
				priority.Enabled, priority.PreserveSessionAffinity = true, true
				settings := service.NewSettingService(&penaltyModeSettings{}, cfg)
				require.NoError(t, settings.SavePrioritySchedulingConfig(t.Context(), &priority))
				slots := &helperConcurrencyCacheStub{accountSeq: make([]bool, 10)}
				for i := range slots.accountSeq {
					slots.accountSeq[i] = true
				}
				concurrency := service.NewConcurrencyService(slots)
				monitor := &affinityAvailabilityReader{states: map[int64]string{1: "pass", 2: "pass"}, stream: stream}
				gateway := service.NewOpenAIGatewayService(repo, nil, nil, nil, nil, nil, nil, cache, cfg, nil, concurrency, nil, nil, nil,
					repository.NewHTTPUpstream(cfg), nil, nil, nil, nil, nil, nil, settings, nil, service.WithOpenAIAvailabilityReader(monitor))
				t.Cleanup(func() { _ = gateway.CloseExcelBPSImages() })
				billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
				t.Cleanup(billing.Stop)
				h := NewOpenAIGatewayHandler(gateway, concurrency, billing, service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
				h.maxAccountSwitches = 2
				body := fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"input":"synthetic affinity request"}`, stream)
				send := func(session string, want int64) {
					t.Helper()
					c, rec := newExcelBPSFailoverTestContext(t.Context(), stream)
					c.Request.Body = io.NopCloser(strings.NewReader(body))
					c.Request.ContentLength = int64(len(body))
					c.Request.Header.Set("X-Session-Id", session)
					hash := gateway.GenerateSessionHash(c, []byte(body))
					h.Responses(c)
					require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
					require.Contains(t, rec.Body.String(), "done")
					bound, err := cache.GetSessionAccountID(t.Context(), 3131, "openai:"+hash)
					require.NoError(t, err)
					require.Equal(t, want, bound)
					_, recorded := c.Get(service.OpsUpstreamErrorsKey)
					require.False(t, recorded, "a monitor admission rejection is not a supplier failure")
				}
				send("affinity-original", 1)
				monitor.setState(1, state)
				send("affinity-original", 2)
				monitor.setState(1, "pass")
				send("affinity-original", 2)
				send("affinity-new", 1)
				send("affinity-original", 2)
				require.Equal(t, int32(5), sends.Load(), "rejected accounts must produce no physical HTTP requests")
				for _, boundary := range []string{"read_error", "switch_budget", "all_failed"} {
					before := len(monitor.readIDs())
					monitor.setState(2, "fail")
					wantReads := []int64{2}
					switch boundary {
					case "read_error":
						monitor.setState(2, "read_error")
					case "switch_budget":
						h.maxAccountSwitches = 0
					case "all_failed":
						h.maxAccountSwitches = 2
						monitor.setState(1, "fail")
						wantReads = []int64{2, 1}
					}
					c, rec := newExcelBPSFailoverTestContext(t.Context(), stream)
					c.Request.Body = io.NopCloser(strings.NewReader(body))
					c.Request.ContentLength = int64(len(body))
					c.Request.Header.Set("X-Session-Id", "affinity-original")
					h.Responses(c)
					require.Equal(t, http.StatusServiceUnavailable, rec.Code, "%s: %s", boundary, rec.Body.String())
					require.Contains(t, rec.Body.String(), "admission_unavailable")
					require.Equal(t, wantReads, monitor.readIDs()[before:], boundary)
					require.Equal(t, int32(5), sends.Load(), "a rejected/exhausted request must not send")
					_, recorded := c.Get(service.OpsUpstreamErrorsKey)
					require.False(t, recorded)
				}
				slots.mu.Lock()
				acquired, released := slots.accountAcquireCalls, slots.accountReleaseCalls
				slots.mu.Unlock()
				require.Equal(t, acquired, released, "rejected and completed selections release every account slot")
			})
		}
	}
}
