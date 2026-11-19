package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestUpstreamCacheProbeInput(t *testing.T) {
	valid := UpstreamCacheProbeInput{Model: "public-model", IntervalsMS: []int{0, 1}, PrefixBlocks: 16, TimeoutSeconds: 10}
	require.NoError(t, valid.Validate())
	for _, change := range []func(*UpstreamCacheProbeInput){
		func(p *UpstreamCacheProbeInput) { p.Model = "" },
		func(p *UpstreamCacheProbeInput) { p.IntervalsMS = nil },
		func(p *UpstreamCacheProbeInput) { p.IntervalsMS = []int{-1} },
		func(p *UpstreamCacheProbeInput) { p.IntervalsMS = []int{10001} },
		func(p *UpstreamCacheProbeInput) { p.IntervalsMS = make([]int, 6) },
		func(p *UpstreamCacheProbeInput) { p.PrefixBlocks = 15 },
		func(p *UpstreamCacheProbeInput) { p.TimeoutSeconds = 121 },
		func(p *UpstreamCacheProbeInput) { p.ReasoningEffort = "other" },
		func(p *UpstreamCacheProbeInput) { p.ServiceTier = "other" },
	} {
		input := valid
		change(&input)
		require.Error(t, input.Validate())
	}
}

// Exercise the real account test entry point, parsers and HTTP observer. Only
// the transport and authoritative repository are local fixtures.
func TestUpstreamCacheProbeHTTP(t *testing.T) {
	for _, protocol := range []string{"responses", "chat"} {
		for _, scenario := range []string{"hit", "miss", "missing-usage", "invalid-usage", "cold-hit", "disconnect", "warm-disconnect", "warm-missing-usage", "warm-invalid-usage", "incomplete", "missing-terminal", "changed-key", "changed-route", "changed-model", "changed-protocol", "disabled", "budget", "settlement", "cancel", "wrong-identity"} {
			t.Run(protocol+"/"+scenario, func(t *testing.T) {
				calls := 0
				var bodies []map[string]any
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					require.Equal(t, "Bearer cache-probe-fixture", r.Header.Get("Authorization"))
					var body map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
					bodies = append(bodies, body)
					require.Equal(t, "wire-model", body["model"])
					require.Equal(t, "priority", body["service_tier"])
					read := 0
					if scenario == "cold-hit" || calls > 1 && scenario != "miss" {
						read = 1600
					}
					if scenario == "invalid-usage" || scenario == "warm-invalid-usage" && calls > 1 {
						read = 4000
					}
					usage := fmt.Sprintf(`,"usage":{"input_tokens":2000,"input_tokens_details":{"cached_tokens":%d},"output_tokens":2,"total_tokens":2002}`, read)
					if protocol == "chat" {
						usage = fmt.Sprintf(`,"usage":{"prompt_tokens":2000,"prompt_tokens_details":{"cached_tokens":%d},"completion_tokens":2,"total_tokens":2002}`, read)
					}
					if scenario == "missing-usage" || scenario == "warm-missing-usage" && calls > 1 {
						usage = ""
					}
					w.Header().Set("Content-Type", "text/event-stream")
					disconnect := scenario == "disconnect" || scenario == "warm-disconnect" && calls > 1
					if protocol == "responses" {
						require.Equal(t, "/v1/responses", r.URL.Path)
						_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"CACHE_OK\"}\n\n")
						if !disconnect {
							if scenario == "missing-terminal" {
								_, _ = io.WriteString(w, "data: [DONE]\n\n")
							} else {
								status := "completed"
								if scenario == "incomplete" {
									status = "incomplete"
								}
								_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":%q,\"model\":\"wire-model\"%s}}\n\n", status, usage)
							}
						}
					} else {
						require.Equal(t, "/v1/chat/completions", r.URL.Path)
						_, _ = io.WriteString(w, "data: {\"model\":\"wire-model\",\"choices\":[{\"delta\":{\"content\":\"CACHE_OK\"},\"finish_reason\":null}]}\n\n")
						if !disconnect {
							if scenario != "missing-terminal" {
								finish := "stop"
								if scenario == "incomplete" {
									finish = "length"
								}
								_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":%q}]}\n\n", finish)
							}
							_, _ = fmt.Fprintf(w, "data: {\"choices\":[]%s}\n\ndata: [DONE]\n\n", usage)
						}
					}
				}))
				t.Cleanup(server.Close)
				account := &Account{ID: 931, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 2,
					Credentials: map[string]any{"api_key": "cache-probe-fixture", "base_url": server.URL, "model_mapping": map[string]any{"public-model": "wire-model"}},
					Extra:       map[string]any{"openai_responses_mode": "force_responses"}}
				if protocol == "chat" {
					account.Extra["openai_responses_mode"] = "force_chat_completions"
				}
				repo := &upstreamCacheProbeRepo{account: account}
				budget := &monitoringBudgetRepoStub{}
				if scenario == "budget" {
					budget.denyAt = 2
					budget.err = &MonitoringProbeBudgetDenial{Reason: "token_limit", Scope: "key_minute", RetryAt: time.Now().Add(time.Minute)}
				}
				if scenario == "settlement" {
					budget.releaseErr = ErrMonitoringProbeBudgetUnavailable
				}
				transport := &codexModelsHTTPUpstreamStub{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
					req, trace := requesttiming.StartTransport(req)
					response, err := server.Client().Do(req)
					trace.Response(response, err)
					return response, err
				}}
				svc := &AccountTestService{accountRepo: repo, cfg: rawChatCompletionsTestConfig(), httpUpstream: monitoringBudgetHTTP{base: transport}}
				svc.SetMonitoringBudget(NewMonitoringProbeBudgetService(budget))
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				input := UpstreamCacheProbeInput{Model: "public-model", PrefixBlocks: 16, IntervalsMS: []int{0, 0}, TimeoutSeconds: 10, ServiceTier: "priority", ReasoningEffort: "low"}
				mutated := false
				result, err := svc.RunUpstreamCacheProbe(ctx, account.ID, input, func(run *UpstreamCacheProbeRun) {
					if mutated || len(run.Rounds) == 0 || run.Rounds[0].Status == "running" {
						return
					}
					mutated = true
					switch scenario {
					case "changed-key":
						account.Credentials["api_key"] = "replacement-fixture"
					case "changed-route":
						account.Credentials["base_url"] = "https://changed.invalid"
					case "changed-model":
						account.Credentials["model_mapping"] = map[string]any{"public-model": "other-model"}
					case "changed-protocol":
						account.Extra["openai_responses_mode"] = "auto"
					case "disabled":
						account.Schedulable = false
					case "cancel":
						cancel()
					case "wrong-identity":
						account.Credentials["header_overrides"] = map[string]any{"Authorization": "Bearer other-fixture"}
					}
				})
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, "not_verified", result.ModelIdentity)
				require.Equal(t, "not_observed", result.UpstreamGroup)
				require.Equal(t, "not_verified", result.CacheDomain)
				require.Nil(t, result.ValidUntil)
				switch scenario {
				case "hit", "miss", "cold-hit":
					rawResult, _ := json.Marshal(result)
					require.Equal(t, 3, calls, "%s", rawResult)
					require.Equal(t, "completed", result.Status)
					require.Equal(t, 2, result.ObservedWarmRounds)
					require.NotNil(t, result.TokenHitRate)
					expected := "hit_observed"
					if scenario == "miss" {
						expected = "no_hit_observed"
					}
					require.Equal(t, expected, result.Verdict)
					cold := "zero_read_observed"
					if scenario == "cold-hit" {
						cold = "cold_not_confirmed"
					}
					require.Equal(t, cold, result.ColdBaseline)
				case "missing-usage", "invalid-usage":
					require.Equal(t, 1, calls)
					require.Equal(t, "insufficient", result.Verdict)
					require.Nil(t, result.TokenHitRate)
				case "disconnect":
					require.Equal(t, 1, calls)
					require.Equal(t, "failed", result.Status)
				case "warm-disconnect", "warm-missing-usage", "warm-invalid-usage":
					require.Equal(t, 2, calls)
					require.Equal(t, 2, len(result.Rounds))
					require.Equal(t, "completed", result.Rounds[0].Status)
					require.Equal(t, "insufficient", result.Verdict)
					require.Zero(t, result.ObservedWarmRounds)
					require.Nil(t, result.TokenHitRate)
				default:
					require.Equal(t, 1, calls, "no other account or changed route may fill a missing round")
					require.NotEqual(t, "completed", result.Status)
					require.Nil(t, result.TokenHitRate)
				}
				require.Equal(t, calls, budget.released, "all physically admitted calls are settled")
				if scenario == "hit" {
					require.Equal(t, []MonitoringProbeTokenUsage{{Tokens: 2002, Reported: true}, {Tokens: 2002, Reported: true}, {Tokens: 2002, Reported: true}}, budget.usages)
					require.Equal(t, bodies[0]["max_output_tokens"], bodies[1]["max_output_tokens"])
					field := "input"
					if protocol == "chat" {
						field = "messages"
					}
					first, _ := json.Marshal(bodies[0][field])
					second, _ := json.Marshal(bodies[1][field])
					require.Greater(t, len(first), 7000)
					require.Greater(t, commonCacheProbePrefix(string(first), string(second)), 7000)
				}
				raw, err := json.Marshal(result)
				require.NoError(t, err)
				for _, secret := range []string{"cache-probe-fixture", "replacement-fixture", "Bearer ", server.URL, "Reference 1", "identity_reference", "fingerprint", "CACHE_OK"} {
					require.NotContains(t, string(raw), secret)
				}
			})
		}
	}
}

type upstreamCacheProbeRepo struct {
	AccountRepository
	account *Account
}

func (r *upstreamCacheProbeRepo) GetByID(_ context.Context, _ int64) (*Account, error) {
	return r.account, nil
}

func commonCacheProbePrefix(a, b string) int {
	for i := 0; i < min(len(a), len(b)); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

func TestUpstreamCacheProbeFinalPhysicalFence(t *testing.T) {
	for _, scenario := range []string{"valid", "wrong-key", "extra-identity", "wrong-model", "wrong-proxy", "changed-after-build", "unknown-body", "canceled", "second-send"} {
		t.Run(scenario, func(t *testing.T) {
			account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
				Credentials: map[string]any{"base_url": "https://upstream.example.test", "api_key": "cache-fixture", "model_mapping": map[string]any{"public-model": "wire-model"}}}
			svc := &AccountTestService{accountRepo: &upstreamCacheProbeRepo{account: account}}
			runtime := &upstreamCacheProbeRuntime{service: svc, accountID: 7, model: "public-model", fingerprint: AccountRouteFingerprint(account)}
			ctx, cancel := context.WithCancel(context.WithValue(t.Context(), upstreamCacheProbeKey{}, runtime))
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, "POST", "https://upstream.example.test/v1/responses", bytes.NewReader([]byte(`{"model":"wire-model","input":"public","stream":true}`)))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer cache-fixture")
			switch scenario {
			case "wrong-key":
				req.Header.Set("Authorization", "Bearer other-fixture")
			case "extra-identity":
				req.Header.Set("OpenAI-Project", "not-owned-by-key")
			case "wrong-model":
				runtime.expectedScope = &AvailabilityRequestScope{Version: 2, Model: "other"}
			case "changed-after-build":
				account.Credentials["api_key"] = "new-key-fixture"
			case "unknown-body":
				req.GetBody = nil
			case "canceled":
				cancel()
			}
			if scenario == "wrong-proxy" {
				require.Error(t, validateUpstreamCacheProbeEgress(req, account, "http://other.example.test"))
				require.Zero(t, runtime.physical.Load())
				return
			}
			if scenario == "valid" || scenario == "second-send" {
				require.NoError(t, admitUpstreamCacheProbePhysical(req))
				require.EqualValues(t, 1, runtime.physical.Load())
				if scenario == "second-send" {
					require.Error(t, admitUpstreamCacheProbePhysical(req))
					require.Equal(t, "additional_send_blocked", runtime.reason)
				}
			} else {
				require.Error(t, admitUpstreamCacheProbePhysical(req))
				require.Zero(t, runtime.physical.Load())
			}
		})
	}
}

func TestUpstreamCacheProbeWaitCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- upstreamCacheProbeWait(ctx, 10*time.Second) }()
		synctest.Wait()
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
	})
}

func TestUpstreamCacheProbeRejectsUnsupportedWithoutSending(t *testing.T) {
	for _, scenario := range []string{"oauth", "managed", "parent", "bps", "synthetic", "disabled", "passthrough", "missing-key", "image", "no-budget"} {
		t.Run(scenario, func(t *testing.T) {
			account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"base_url": "https://upstream.example.test", "api_key": "fixture"}, Extra: map[string]any{}}
			model := "gpt-test"
			switch scenario {
			case "oauth":
				account.Type = AccountTypeOAuth
			case "managed":
				account.Extra["openai_connections_managed"] = true
			case "parent":
				id := int64(99)
				account.ParentAccountID = &id
			case "bps":
				account.Type = AccountTypeOAuth
				account.Extra["openai_excel_bps"] = true
			case "synthetic":
				account.Extra["synthetic_ui_test"] = true
			case "disabled":
				account.Schedulable = false
			case "passthrough":
				account.Extra["openai_passthrough"] = true
			case "missing-key":
				delete(account.Credentials, "api_key")
			case "image":
				model = "gpt-image-1"
			}
			upstream := &codexModelsHTTPUpstreamStub{do: func(*http.Request, string, int64, int) (*http.Response, error) {
				t.Error("unsupported probe attempted a send")
				return nil, nil
			}}
			svc := &AccountTestService{accountRepo: &upstreamCacheProbeRepo{account: account}, httpUpstream: upstream}
			budget := &monitoringBudgetRepoStub{}
			if scenario != "no-budget" {
				svc.SetMonitoringBudget(NewMonitoringProbeBudgetService(budget))
			}
			result, err := svc.RunUpstreamCacheProbe(t.Context(), 7, UpstreamCacheProbeInput{Model: model, IntervalsMS: []int{0}, PrefixBlocks: 16, TimeoutSeconds: 10}, nil)
			require.Error(t, err)
			require.Nil(t, result)
			require.Empty(t, budget.requests)
		})
	}
}

func cacheProbeHTTPFixture(do func(*http.Request) (*http.Response, error)) (*AccountTestService, *monitoringBudgetRepoStub) {
	account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"api_key": "cache-fixture", "base_url": "https://upstream.example.test"},
		Extra:       map[string]any{"openai_responses_mode": "force_responses"}}
	budget := &monitoringBudgetRepoStub{}
	transport := &codexModelsHTTPUpstreamStub{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
		req, trace := requesttiming.StartTransport(req)
		response, err := do(req)
		trace.Response(response, err)
		return response, err
	}}
	svc := &AccountTestService{accountRepo: &upstreamCacheProbeRepo{account: account}, cfg: rawChatCompletionsTestConfig(), httpUpstream: monitoringBudgetHTTP{base: transport}}
	svc.SetMonitoringBudget(NewMonitoringProbeBudgetService(budget))
	return svc, budget
}

func TestUpstreamCacheProbeHeartbeatPreservesNoStore(t *testing.T) {
	for _, cacheControl := range []string{"", "public, max-age=600", "no-store", "private, No-Store"} {
		t.Run(cacheControl, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/cache-probe", nil)
			c.Header("Cache-Control", cacheControl)
			stop := startAccountTestSSEKeepalive(c, time.Minute)
			stop()
			expected := "no-cache"
			if strings.Contains(strings.ToLower(cacheControl), "no-store") {
				expected = cacheControl
			}
			require.Equal(t, expected, w.Result().Header.Get("Cache-Control"))
		})
	}
}

func TestUpstreamCacheProbeTimeoutAndInFlightCancel(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", canceled), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				calls := 0
				svc, budget := cacheProbeHTTPFixture(func(req *http.Request) (*http.Response, error) {
					calls++
					if canceled {
						cancel()
					}
					<-req.Context().Done()
					return nil, req.Context().Err()
				})
				input := UpstreamCacheProbeInput{Model: "public-model", PrefixBlocks: 16, IntervalsMS: []int{10000, 0}, TimeoutSeconds: 10}
				run, err := svc.RunUpstreamCacheProbe(ctx, 7, input, nil)
				require.NoError(t, err)
				require.Equal(t, 1, calls)
				require.Equal(t, 1, budget.released)
				require.Len(t, run.Rounds, 1)
				require.Equal(t, "insufficient", run.Verdict)
				require.Nil(t, run.TokenHitRate)
				require.Equal(t, []MonitoringProbeTokenUsage{{Reported: false}}, budget.usages)
				if canceled {
					require.Equal(t, "canceled", run.Reason)
					require.Equal(t, "stopped", run.Status)
				} else {
					require.Equal(t, "timeout", run.Reason)
					require.Equal(t, "failed", run.Status)
				}
			})
		})
	}
}

func TestUpstreamCacheProbeSSEAndDisconnectStopsWarmRounds(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"CACHE_OK\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":2000,\"input_tokens_details\":{\"cached_tokens\":0},\"output_tokens\":2,\"total_tokens\":2002}}}\n\n"
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(upstream.Close)
	svc, budget := cacheProbeHTTPFixture(upstream.Client().Do)
	svc.accountRepo.(*upstreamCacheProbeRepo).account.Credentials["base_url"] = upstream.URL
	done := make(chan error, 1)
	router := gin.New()
	router.POST("/cache-probe", func(c *gin.Context) {
		done <- svc.TestUpstreamCacheProbe(c, 7, UpstreamCacheProbeInput{Model: "public-model", PrefixBlocks: 16, IntervalsMS: []int{10000, 10000}, TimeoutSeconds: 10})
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/cache-probe", nil)
	require.NoError(t, err)
	response, err := server.Client().Do(req)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, "no-store", response.Header.Get("Cache-Control"))
	require.Equal(t, "text/event-stream", response.Header.Get("Content-Type"))
	reader := bufio.NewReader(response.Body)
	for {
		line, err := reader.ReadString('\n')
		require.NoError(t, err)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var event struct {
			Type string                `json:"type"`
			Data UpstreamCacheProbeRun `json:"data"`
		}
		require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event))
		require.Equal(t, "cache_probe", event.Type)
		require.Equal(t, "running", event.Data.Status, "unexpected terminal run: %s", line)
		for _, secret := range []string{"CACHE_OK", "cache-fixture", "Bearer", "Reference 1", "fingerprint"} {
			require.NotContains(t, line, secret)
		}
		if len(event.Data.Rounds) == 1 && event.Data.Rounds[0].Status == "completed" {
			break
		}
	}
	require.NoError(t, response.Body.Close())
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("disconnect did not cancel the pending warm-round wait")
	}
	require.EqualValues(t, 1, calls.Load())
	require.Equal(t, 1, budget.released)
	require.Equal(t, []MonitoringProbeTokenUsage{{Tokens: 2002, Reported: true}}, budget.usages)
}
