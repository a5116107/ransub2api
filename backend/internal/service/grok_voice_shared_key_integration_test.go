//go:build integration

package service_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGrokVoiceSharesRealRedisKeyCapacityAndRecovers(t *testing.T) {
	rdb := startKeyAdmissionIntegrationRedis(t)
	for _, endpoint := range []string{"realtime", "tts"} {
		for _, astra := range []bool{false, true} {
			name := endpoint + "-direct"
			if astra {
				name = endpoint + "-astra"
			}
			t.Run(name, func(t *testing.T) {
				var handshakes, otherRequests atomic.Int32
				closed := make(chan struct{}, 1)
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, err := coderws.Accept(w, r, nil)
					if err != nil {
						return
					}
					defer func() { _ = conn.CloseNow(); closed <- struct{}{} }()
					handshakes.Add(1)
					for {
						if _, _, err = conn.Read(r.Context()); err != nil {
							return
						}
					}
				}))
				t.Cleanup(server.Close)
				other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					otherRequests.Add(1)
					_, _ = io.WriteString(w, "ok")
				}))
				t.Cleanup(other.Close)
				originalTransport := http.DefaultTransport
				http.DefaultTransport = server.Client().Transport
				t.Cleanup(func() { http.DefaultTransport = originalTransport })
				cfg := &config.Config{}
				fingerprint := config.UpstreamKeyFingerprint("shared-fixture")
				cfg.Gateway.UpstreamKeyConcurrency = config.UpstreamKeyConcurrencyConfig{Revision: 1, Limits: map[string]int{fingerprint: 1}}
				cfg.Gateway.CodexGatewayPin.Enabled = astra
				transport := repository.ProvideHTTPUpstream(cfg, nil, rdb, nil)
				authority, ok := transport.(service.UpstreamKeyConcurrency)
				require.True(t, ok)
				svc := service.NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, transport, nil, nil, nil, nil, nil, nil, nil, nil)
				account := &service.Account{ID: 7, Platform: service.PlatformGrok, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"base_url": server.URL, "api_key": "shared-fixture"}}
				open := func() (*service.GrokRealtimeUpstream, error) {
					if endpoint == "realtime" {
						return svc.OpenGrokRealtime(t.Context(), account, "shared-fixture", "fixture")
					}
					return svc.OpenGrokTTSStream(t.Context(), account, "shared-fixture", url.Values{})
				}
				_, release, err := authority.AcquireUpstreamKeySlots(t.Context(), []string{fingerprint})
				require.NoError(t, err)
				require.NotNil(t, release)
				t.Cleanup(release)
				_, err = open()
				require.True(t, service.IsUpstreamKeyAdmissionFailure(err))
				require.Zero(t, handshakes.Load())
				release()
				voice, err := open()
				require.NoError(t, err)
				t.Cleanup(func() { _ = voice.Close() })
				require.EqualValues(t, 1, handshakes.Load())
				if endpoint == "realtime" && !astra {
					key := "concurrency:upstream_key:slots:" + fingerprint
					members, readErr := rdb.ZRangeWithScores(t.Context(), key, 0, -1).Result()
					require.NoError(t, readErr)
					require.Len(t, members, 1)
					initialScore := members[0].Score
					require.Eventually(t, func() bool {
						current, readErr := rdb.ZRangeWithScores(t.Context(), key, 0, -1).Result()
						return readErr == nil && len(current) == 1 && current[0].Score > initialScore
					}, 10*time.Second, 100*time.Millisecond, "active voice sessions must renew their shared-key lease")
				}
				request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, other.URL, strings.NewReader("{}"))
				require.NoError(t, err)
				request.Header.Set("Authorization", "Bearer shared-fixture")
				response, err := transport.Do(request, "", 8, 1)
				if response != nil {
					_ = response.Body.Close()
				}
				require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)
				require.Zero(t, otherRequests.Load())
				require.NoError(t, voice.Close())
				select {
				case <-closed:
				case <-time.After(5 * time.Second):
					t.Fatal("voice connection did not close")
				}
				request, err = http.NewRequestWithContext(context.Background(), http.MethodPost, other.URL, strings.NewReader("{}"))
				require.NoError(t, err)
				request.Header.Set("Authorization", "Bearer shared-fixture")
				response, err = transport.Do(request, "", 8, 1)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				require.EqualValues(t, 1, otherRequests.Load())
				if endpoint == "realtime" {
					testRepo := &quotaIntegrationAccountRepo{account: account}
					accountTester := service.NewAccountTestService(testRepo, nil, nil, nil, nil, transport, cfg, nil)
					_, hold, holdErr := authority.AcquireUpstreamKeySlots(t.Context(), []string{fingerprint})
					require.NoError(t, holdErr)
					require.NotNil(t, hold)
					t.Cleanup(hold)
					probeRecorder := httptest.NewRecorder()
					probeContext, _ := gin.CreateTestContext(probeRecorder)
					probeContext.Request = httptest.NewRequest(http.MethodPost, "/fixture", nil).WithContext(t.Context())
					err = accountTester.TestAccountConnection(probeContext, account.ID, "fixture", "", service.AccountTestModeGrokRealtime)
					require.Error(t, err)
					require.Contains(t, probeRecorder.Body.String(), "shared upstream Key")
					require.EqualValues(t, 1, handshakes.Load())
					hold()
					probeRecorder = httptest.NewRecorder()
					probeContext, _ = gin.CreateTestContext(probeRecorder)
					probeContext.Request = httptest.NewRequest(http.MethodPost, "/fixture", nil).WithContext(t.Context())
					err = accountTester.TestAccountConnection(probeContext, account.ID, "fixture", "", service.AccountTestModeGrokRealtime)
					require.NoError(t, err)
					require.Contains(t, probeRecorder.Body.String(), `"success":true`)
					require.EqualValues(t, 2, handshakes.Load())
				}
			})
		}
	}
}
