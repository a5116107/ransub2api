//go:build integration

package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/Wei-Shaw/sub2api/internal/regional"
	"github.com/Wei-Shaw/sub2api/internal/regional/control"
	"github.com/Wei-Shaw/sub2api/internal/regional/video"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/testutil/mtls"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type regionalForwardAuthSource struct{ record service.RegionalAuthRecord }

func (s regionalForwardAuthSource) ListRegionalAuth(context.Context, []int64) ([]service.RegionalAuthRecord, error) {
	return []service.RegionalAuthRecord{s.record}, nil
}

// The receiving ingress uses a genuine local auth replica, the production
// route classifier and policies; task ownership remains in the SQL fixture.
func regionalForwardGateway(t *testing.T, gateway *service.OpenAIGatewayService, identity, peer mtls.Identity, owner service.AsyncVideoKeyOwner, platform string) *gin.Engine {
	t.Helper()
	groupID := owner.GroupID
	key := &service.APIKey{ID: owner.APIKeyID, UserID: owner.UserID, GroupID: &groupID, Key: "downstream-fixture", Status: service.StatusActive,
		User: &service.User{ID: owner.UserID, Status: service.StatusActive}, Group: &service.Group{ID: groupID, Platform: platform, Status: service.StatusActive,
			BlockSubagents: true, ModelAllowlist: service.GroupModelAllowlist{Enabled: true, Models: []string{"model-fixture"}}}}
	keyMaterial, lookup := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	policy := regional.Config{RegionID: "us", MaxSnapshotAgeSeconds: 60}
	statePath := filepath.Join(t.TempDir(), "auth.snapshot")
	codec, err := regional.NewCodec(map[string][]byte{"fixture": keyMaterial})
	require.NoError(t, err)
	replica, err := regional.OpenReplica(statePath, policy, regional.SnapshotAuth, codec, service.RegionalAuthValidator([]int64{owner.UserID}), true)
	require.NoError(t, err)
	payload, err := service.ExportRegionalAuth(t.Context(), regionalForwardAuthSource{record: service.RegionalAuthRecord{APIKey: key}}, []int64{owner.UserID}, lookup)
	require.NoError(t, err)
	now := time.Now()
	require.NoError(t, replica.Publish(t.Context(), "fixture", 1, now, now.Add(55*time.Second), payload))
	runtime := service.RegionalSchedulerRuntimeConfig{Kind: regional.SnapshotAuth, AuthUserIDs: []int64{owner.UserID}, AuthLookupKey: lookup,
		Mode: "subscriber", Policy: policy, Keys: map[string][]byte{"fixture": keyMaterial}, KeyID: "fixture", StateFile: statePath,
		Endpoint: "https://127.0.0.1:1/internal/regional/auth", Listen: "127.0.0.1:0", CAFile: identity.CAFile,
		CertFile: identity.CertFile, KeyFile: identity.KeyFile, TrustedServerCertSHA256: peer.Pin,
		TrustedClientCertSHA256: []string{peer.Pin}, IntervalSeconds: 1, LifetimeSeconds: 30}
	raw, err := json.Marshal(runtime)
	require.NoError(t, err)
	configPath := filepath.Join(t.TempDir(), "regional-auth.json")
	require.NoError(t, os.WriteFile(configPath, raw, 0600))
	cfg := &config.Config{Runtime: config.RuntimeConfig{Role: config.RuntimeRoleGateway, RegionalAuthFile: configPath, RegionalFundsFile: "fixture"},
		Gateway: config.GatewayConfig{MaxBodySize: 1 << 20, TextMaxBodySize: 1 << 20}}
	keys, err := service.ProvideAPIKeyService(nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil)
	require.NoError(t, err)
	t.Cleanup(keys.StopAuthCacheInvalidationSubscriber)
	_, err = keys.GetRegionalByKey(t.Context(), key.Key)
	require.NoError(t, err)
	router := gin.New()
	router.Use(servermiddleware.RegionalAPIKeyAuth(keys, cfg), servermiddleware.GroupModelAllowlist(), servermiddleware.GroupCodexCLIOnly(nil), servermiddleware.GroupBlockSubagents())
	handleLookup := func(c *gin.Context) {
		endpoint, taskID := servermiddleware.RegionalVideoLookup(c)
		key, keyOK := servermiddleware.GetAPIKeyFromContext(c)
		subject, userOK := servermiddleware.GetAuthSubjectFromContext(c)
		if !keyOK || !userOK || !endpoint.IsVideoLookupRequest() || taskID == "" {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		handled, err := gateway.ForwardRegionalVideoQuery(c, endpoint, taskID, key, subject.UserID)
		if err != nil || !handled {
			c.AbortWithStatus(http.StatusServiceUnavailable)
		}
	}
	router.GET("/v1/videos/:request_id", handleLookup)
	router.GET("/v1/contents/generations/tasks/:task_id", handleLookup)
	return router
}

func TestRegionalAsyncVideoForwardingUsesOriginalAuthority(t *testing.T) {
	t.Setenv("SUB2API_REGIONAL_TEST_DSN", integrationPostgresDSN)
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	gin.SetMode(gin.TestMode)
	for _, decorated := range []bool{false, true} {
		for _, tc := range []struct {
			name, platform, body, terminal, path string
			create, status                       service.GrokMediaEndpoint
		}{
			{"grok", service.PlatformGrok, `{"model":"grok-imagine-video","prompt":"waves"}`, "done", "/v1/videos/:task", service.GrokMediaEndpointVideosGenerations, service.GrokMediaEndpointVideoStatus},
			{"seedance", service.PlatformOpenAI, `{"model":"video","content":[{"type":"text","text":"waves"}]}`, "succeeded", "/v1/contents/generations/tasks/:task", service.SeedanceEndpointCreate, service.SeedanceEndpointStatus},
			{"minimax", service.PlatformMiniMax, `{"model":"MiniMax-H3","prompt":"waves","seconds":"1"}`, "completed", "/v1/videos/:task", service.MiniMaxH3EndpointCreate, service.MiniMaxH3EndpointStatus},
		} {
			t.Run(tc.name+map[bool]string{false: "/direct", true: "/astra"}[decorated], func(t *testing.T) {
				rdb := regionalKeyRedis(t)
				cfg := upstreamKeyTestConfig(1, 2)
				cfg.Security.URLAllowlist.AllowInsecureHTTP = true
				primary, local, _ := regionalKeyFixture(t, cfg, rdb, 1)
				authorities := []*regionalConcurrencyCache{primary, local}
				identities := mtls.Pair(t)
				addresses := make([]string, 2)
				for i := range addresses {
					listener, err := net.Listen("tcp", "127.0.0.1:0")
					require.NoError(t, err)
					addresses[i] = listener.Addr().String()
					require.NoError(t, listener.Close())
				}
				var creates, polls, downloads, cancels atomic.Int32
				var terminal atomic.Bool
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.Header.Get("Authorization") != "Bearer supplier-fixture" {
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					if strings.HasSuffix(r.URL.Path, "/content") {
						downloads.Add(1)
						if tc.platform == service.PlatformMiniMax {
							w.Header().Set("Location", "https://video.example/output.mp4")
							w.WriteHeader(http.StatusFound)
							return
						}
						if r.Header.Get("Range") != "bytes=0-12" {
							w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
							return
						}
						w.Header().Set("Content-Type", "video/mp4")
						w.Header().Set("Content-Range", "bytes 0-12/100")
						w.Header().Set("Accept-Ranges", "bytes")
						w.Header().Set("Set-Cookie", "supplier-private=value")
						w.WriteHeader(http.StatusPartialContent)
						_, _ = io.WriteString(w, "fixture-video")
						return
					}
					if r.Method == http.MethodDelete {
						cancels.Add(1)
						w.WriteHeader(http.StatusNoContent)
						return
					}
					if r.Method == http.MethodPost {
						id := "primary-task"
						if creates.Add(1) == 2 {
							id = "local-task"
						}
						_, _ = io.WriteString(w, `{"id":"`+id+`"}`)
						return
					}
					polls.Add(1)
					state := "pending"
					if terminal.Load() {
						state = tc.terminal
					}
					_, _ = io.WriteString(w, `{"status":"`+state+`","video":{"url":"`+r.URL.Path+`/content","duration":6},"metadata":{"url":"https://video.example/result.mp4"},"usage":{"completion_tokens":1}}`)
				}))
				t.Cleanup(provider.Close)
				account := &service.Account{ID: 7, Platform: tc.platform, Type: service.AccountTypeAPIKey, Concurrency: 2,
					Credentials: map[string]any{"api_key": "supplier-fixture", "base_url": provider.URL, "openai_capabilities": []string{"seedance"}}}
				owner := service.AsyncVideoKeyOwner{UserID: 10, APIKeyID: 20, GroupID: 30, AccountID: account.ID, Provider: tc.create.AsyncVideoProvider(), RouteFingerprint: service.AccountRouteFingerprint(account)}
				key := &service.APIKey{ID: owner.APIKeyID, GroupID: &owner.GroupID}
				transports := make([]*httpUpstreamService, 2)
				services := make([]*service.OpenAIGatewayService, 2)
				var executions [2]atomic.Int32
				for i, authority := range authorities {
					forward := &video.ForwardConfig{Listen: addresses[i], Endpoint: "https://" + addresses[1-i], CAFile: identities[i].CAFile, CertFile: identities[i].CertFile, KeyFile: identities[i].KeyFile, PeerPin: identities[1-i].Pin,
						UserIDs: []int64{owner.UserID}, APIKeyIDs: []int64{owner.APIKeyID}, AccountIDs: []int64{owner.AccountID}, TimeoutSeconds: 10}
					if i == 0 {
						authority.runtimeConfig = &RegionalConcurrencyRuntimeConfig{Listen: "fixture", Scope: control.Scope{Region: "us"}, VideoForward: forward}
					} else {
						authority.runtimeConfig = &RegionalConcurrencyRuntimeConfig{Endpoint: "fixture", Region: "us", VideoForward: forward}
					}
					require.NoError(t, authority.prepareVideoForwarding())
					ctx, cancel := context.WithCancel(t.Context())
					done := make(chan struct{})
					go func() { defer close(done); authority.RunRegionalRuntime(ctx) }()
					t.Cleanup(func() {
						cancel()
						select {
						case <-done:
						case <-time.After(8 * time.Second):
							t.Error("video listener did not stop")
						}
					})
					transports[i] = liveKeyTestUpstream(t, cfg, rdb, authority)
					var transport service.HTTPUpstream = transports[i]
					if decorated {
						transport = &astraRoutingUpstream{delegate: transport}
					}
					services[i] = service.ProvideOpenAIGatewayService(service.OpenAIGatewayDependencies{Config: cfg, Upstream: transport})
					t.Cleanup(services[i].StopOpenAICodexTicketHarvester)
					t.Cleanup(services[i].StopAsyncVideoKeyReleaseRecovery)
				}
				forward := func(i int, ctx context.Context, c *gin.Context, endpoint service.GrokMediaEndpoint, task string, body []byte) (*service.OpenAIForwardResult, error) {
					ctx = service.WithAsyncVideoKeyOwner(ctx, owner)
					switch tc.platform {
					case service.PlatformOpenAI:
						return services[i].ForwardSeedance(ctx, c, account, endpoint, task, body)
					case service.PlatformMiniMax:
						return services[i].ForwardMiniMaxH3(ctx, c, account, endpoint, task, body)
					default:
						return services[i].ForwardGrokMedia(ctx, c, account, endpoint, task, body, "application/json")
					}
				}
				for i := range services {
					router := gin.New()
					registerQuery := func(method, path string, operation service.GrokMediaEndpoint) {
						router.Handle(method, path, func(c *gin.Context) {
							if c.GetHeader("Authorization") != "Bearer downstream-fixture" {
								c.Status(http.StatusUnauthorized)
								return
							}
							groupID, groupErr := service.ParseAPIKeyRequestGroup(c.Request.Header)
							if groupErr != nil || groupID != owner.GroupID || c.GetHeader("originator") != "origin-fixture" || c.Query("model") != "model-fixture" || service.IsCodexSubagentRequest(c, nil) {
								c.Status(http.StatusForbidden)
								return
							}
							task := c.Param("task")
							switch tc.platform {
							case service.PlatformOpenAI:
								task = service.SeedanceTaskKey(task)
							case service.PlatformMiniMax:
								task = service.MiniMaxH3TaskKey(task)
							}
							handled, err := services[i].ForwardRegionalVideoQuery(c, operation, task, key, owner.UserID)
							if err != nil || handled {
								c.Status(http.StatusServiceUnavailable)
								return
							}
							_, err = services[i].ResolveMediaVideoRequestAccount(c.Request.Context(), key.GroupID, task, owner.UserID, key.ID, operation)
							if err != nil {
								c.Status(http.StatusServiceUnavailable)
								return
							}
							executions[i].Add(1)
							result, err := forward(i, c.Request.Context(), c, operation, task, nil)
							if err != nil {
								c.Status(http.StatusBadGateway)
								return
							}
							if result.AsyncVideoTerminal {
								require.NoError(t, services[i].ReleaseAsyncVideoTaskLease(service.WithAsyncVideoKeyOwner(c.Request.Context(), owner), task))
							}
						})
					}
					registerQuery(http.MethodGet, tc.path, tc.status)
					switch tc.platform {
					case service.PlatformOpenAI:
						registerQuery(http.MethodDelete, tc.path, service.SeedanceEndpointDelete)
					case service.PlatformMiniMax:
						registerQuery(http.MethodGet, tc.path+"/content", service.MiniMaxH3EndpointContent)
					default:
						registerQuery(http.MethodGet, tc.path+"/content", service.GrokMediaEndpointVideoContent)
					}
					services[i].InstallMediaVideoForwarding(router)
				}
				syncDirectory := func() {
					t.Helper()
					for i, authority := range authorities {
						snapshot, err := authorities[1-i].exportAsyncVideoDirectory(t.Context())
						require.NoError(t, err)
						tx, err := authority.database().BeginTx(t.Context(), nil)
						require.NoError(t, err)
						require.NoError(t, installAsyncVideoDirectorySnapshot(t.Context(), tx, authority.persistentUpstreamKeyAuthority(), snapshot))
						require.NoError(t, tx.Commit())
					}
				}
				for origin := range services {
					terminal.Store(false)
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, "/fixture", strings.NewReader(tc.body))
					created, err := forward(origin, t.Context(), c, tc.create, "", []byte(tc.body))
					require.NoError(t, err)
					syncDirectory()
					queryOperation := func(credential string, operation service.GrokMediaEndpoint, subagent bool) (*httptest.ResponseRecorder, error) {
						recorder := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(recorder)
						c.Request = httptest.NewRequest(http.MethodGet, "/fixture?model=model-fixture", nil)
						c.Request.Header.Set("Authorization", "Bearer "+credential)
						c.Request.Header.Set("originator", "origin-fixture")
						c.Request.Header.Set("Range", "bytes=0-12")
						if subagent {
							c.Request.Header.Set("X-Codex-Parent-Thread-Id", "parent-fixture")
						}
						handled, err := services[1-origin].ForwardRegionalVideoQuery(c, operation, created.ResponseID, key, owner.UserID)
						require.True(t, handled)
						return recorder, err
					}
					query := func(credential string) (int, error) {
						recorder, err := queryOperation(credential, tc.status, false)
						return recorder.Code, err
					}
					before := executions[origin].Load()
					status, err := query("wrong-downstream")
					require.NoError(t, err)
					require.Equal(t, http.StatusUnauthorized, status)
					require.Equal(t, before, executions[origin].Load(), "the original router must authenticate before provider access")
					status, err = query("downstream-fixture")
					require.NoError(t, err)
					require.Equal(t, http.StatusOK, status)
					require.Equal(t, before+1, executions[origin].Load())
					if origin == 0 {
						gateway := regionalForwardGateway(t, services[1], identities[1], identities[0], owner, tc.platform)
						publicID := strings.TrimPrefix(strings.TrimPrefix(created.ResponseID, "seedance:"), "minimax-h3:")
						path := "/v1/videos/" + publicID
						if tc.platform == service.PlatformOpenAI {
							path = "/v1/contents/generations/tasks/" + publicID
						}
						req := httptest.NewRequest(http.MethodGet, path+"?model=model-fixture", nil)
						req.Header.Set("Authorization", "Bearer downstream-fixture")
						req.Header.Set("originator", "origin-fixture")
						w := httptest.NewRecorder()
						gateway.ServeHTTP(w, req)
						require.Equal(t, http.StatusOK, w.Code, w.Body.String())
						require.Equal(t, before+2, executions[origin].Load(), "the authenticated lookup must reach the SQL-proven original authority")
						req = httptest.NewRequest(http.MethodGet, path+"?model=model-fixture", nil)
						req.Header.Set("Authorization", "Bearer downstream-fixture")
						req.Header.Set("X-OpenAI-Subagent", "worker")
						w = httptest.NewRecorder()
						gateway.ServeHTTP(w, req)
						require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
						require.Equal(t, before+2, executions[origin].Load(), "local policy denial must not reach the origin")
						before++
					}
					recorder, err := queryOperation("downstream-fixture", tc.status, true)
					require.NoError(t, err)
					require.Equal(t, http.StatusForbidden, recorder.Code, "origin policy must receive the subagent classification")
					require.Equal(t, before+1, executions[origin].Load(), "a denied origin request must not reach the provider")
					var received int
					require.NoError(t, authorities[1-origin].database().QueryRowContext(t.Context(), `SELECT count(*) FROM regional_async_video_key_leases`).Scan(&received))
					require.Zero(t, received, "forwarding never recreates a generation member at the receiving node")
					if tc.platform == service.PlatformOpenAI {
						recorder, err := queryOperation("downstream-fixture", service.SeedanceEndpointDelete, false)
						require.NoError(t, err)
						require.Equal(t, http.StatusNoContent, recorder.Code)
						var active int
						require.NoError(t, authorities[origin].database().QueryRowContext(t.Context(), `SELECT count(*) FROM regional_async_video_key_leases WHERE state='bound' AND NOT fenced`).Scan(&active))
						require.Equal(t, 1, active, "cancel acknowledgement alone must keep the original generation member")
					}
					terminal.Store(true)
					status, err = query("downstream-fixture")
					require.NoError(t, err)
					require.Equal(t, http.StatusOK, status)
					syncDirectory()
					status, err = query("downstream-fixture")
					require.NoError(t, err)
					require.Equal(t, http.StatusOK, status, "completed receipts must still be located after active directory removal")
					if tc.platform != service.PlatformOpenAI {
						operation := service.GrokMediaEndpointVideoContent
						if tc.platform == service.PlatformMiniMax {
							operation = service.MiniMaxH3EndpointContent
						}
						recorder, err := queryOperation("downstream-fixture", operation, false)
						require.NoError(t, err)
						if tc.platform == service.PlatformMiniMax {
							require.Equal(t, http.StatusFound, recorder.Code)
							require.Equal(t, "https://video.example/output.mp4", recorder.Header().Get("Location"))
						} else {
							require.Equal(t, http.StatusPartialContent, recorder.Code)
							require.Equal(t, "fixture-video", recorder.Body.String())
							require.Equal(t, "bytes 0-12/100", recorder.Header().Get("Content-Range"))
							require.Empty(t, recorder.Header().Get("Set-Cookie"))
						}
					}
					before = executions[origin].Load()
					_, err = authorities[1-origin].database().ExecContext(t.Context(), `UPDATE regional_async_video_directory_snapshots SET payload=jsonb_set(jsonb_set(payload,'{issued_at}',to_jsonb(clock_timestamp()-interval '1 minute')),'{expires_at}',to_jsonb(clock_timestamp()-interval '31 seconds'))`)
					require.NoError(t, err)
					_, err = query("downstream-fixture")
					require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy)
					require.Equal(t, before, executions[origin].Load(), "an expired directory cannot trigger a remote data request")
					syncDirectory()
				}
				require.EqualValues(t, 2, creates.Load(), "queries must not resubmit either original task")
				expectedPolls := 7 // three direct queries per origin, plus one registered regional gateway query
				if tc.platform == service.PlatformGrok {
					expectedPolls += 2 // each download first verifies the original status
				}
				require.EqualValues(t, expectedPolls, polls.Load())
				if tc.platform == service.PlatformOpenAI {
					require.EqualValues(t, 2, cancels.Load())
					require.Zero(t, downloads.Load())
				} else {
					require.EqualValues(t, 2, downloads.Load())
					require.Zero(t, cancels.Load())
				}
			})
		}
	}
}
