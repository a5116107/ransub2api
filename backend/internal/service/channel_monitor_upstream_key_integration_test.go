//go:build integration

package service_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type monitorIntegrationRepo struct {
	service.ChannelMonitorRepository
	monitor *service.ChannelMonitor
	history []*service.ChannelMonitorHistoryRow
	marked  int
}

func (r *monitorIntegrationRepo) GetByID(context.Context, int64) (*service.ChannelMonitor, error) {
	m := *r.monitor
	return &m, nil
}
func (r *monitorIntegrationRepo) InsertHistoryBatch(_ context.Context, rows []*service.ChannelMonitorHistoryRow) error {
	r.history = append(r.history, rows...)
	return nil
}
func (r *monitorIntegrationRepo) MarkChecked(context.Context, int64, time.Time) error {
	r.marked++
	return nil
}

type monitorIntegrationEncryptor struct{}

func (monitorIntegrationEncryptor) Encrypt(v string) (string, error) { return v, nil }
func (monitorIntegrationEncryptor) Decrypt(v string) (string, error) { return v, nil }

type monitorIntegrationRuntime struct{}

func (monitorIntegrationRuntime) GetChannelMonitorRuntime(context.Context) service.ChannelMonitorRuntime {
	return service.ChannelMonitorRuntime{Enabled: true, Mode: service.ChannelMonitorModeV1}
}

func TestChannelMonitorSharesRealRedisKeyCapacityAndRecovers(t *testing.T) {
	binary, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skip("redis-server is required for this isolated integration")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	_, port, err := net.SplitHostPort(address)
	require.NoError(t, err)
	require.NoError(t, listener.Close())
	cmd := exec.Command(binary, "--bind", "127.0.0.1", "--port", port, "--dir", t.TempDir(), "--save", "", "--appendonly", "no")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	rdb := redis.NewClient(&redis.Options{Addr: address, MaxRetries: -1, DialTimeout: 200 * time.Millisecond})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	require.Eventually(t, func() bool { return rdb.Ping(t.Context()).Err() == nil }, 3*time.Second, 10*time.Millisecond)

	for _, tc := range []struct {
		name           string
		wrap, override bool
	}{
		{"direct", false, false}, {"direct-override", false, true},
		{"astra", true, false}, {"astra-override", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var probes, otherRequests atomic.Int32
			var slow atomic.Bool
			var observedHeader atomic.Value
			started, finishBody := make(chan struct{}, 1), make(chan struct{})
			var closeBody sync.Once
			defer closeBody.Do(func() { close(finishBody) })
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method == http.MethodHead {
					w.WriteHeader(http.StatusOK)
					return
				}
				if req.URL.Path == "/held" {
					_, _ = io.WriteString(w, "held")
					if err := http.NewResponseController(w).Flush(); err != nil {
						return
					}
					<-req.Context().Done()
					return
				}
				if req.URL.Path == "/other" {
					otherRequests.Add(1)
					_, _ = io.WriteString(w, "ok")
					return
				}
				probes.Add(1)
				observedHeader.Store(req.Header.Get("Authorization"))
				if slow.Load() {
					_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"`)
					if err := http.NewResponseController(w).Flush(); err != nil {
						return
					}
					started <- struct{}{}
					select {
					case <-finishBody:
					case <-req.Context().Done():
						return
					}
					_, _ = io.WriteString(w, `ok"}}]}`)
					return
				}
				_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
			}))
			t.Cleanup(server.Close)
			restore := service.SetChannelMonitorHTTPClientForTesting(server.Client())
			t.Cleanup(restore)
			cfg := &config.Config{}
			cfg.Gateway.UpstreamKeyConcurrency = config.UpstreamKeyConcurrencyConfig{Revision: 1, Limits: map[string]int{config.UpstreamKeyFingerprint("shared-fixture"): 1}}
			cfg.Gateway.CodexGatewayPin.Enabled = tc.wrap
			transport := repository.ProvideHTTPUpstream(cfg, nil, rdb, nil)
			repo := &monitorIntegrationRepo{monitor: &service.ChannelMonitor{ID: 1, Provider: service.MonitorProviderOpenAI,
				Endpoint: server.URL, APIKey: "shared-fixture", PrimaryModel: "fixture-model", BodyOverrideMode: service.MonitorBodyOverrideModeReplace,
				BodyOverride: map[string]any{"messages": []any{map[string]any{"role": "user", "content": "test"}}}}}
			if tc.override {
				repo.monitor.APIKey = "unused-default-fixture"
				repo.monitor.ExtraHeaders = map[string]string{"authorization": "Bearer shared-fixture"}
			}
			svc := service.ProvideChannelMonitorService(repo, monitorIntegrationEncryptor{}, nil, cfg, transport)
			svc.SetRuntimeReader(monitorIntegrationRuntime{})
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/held", strings.NewReader("{}"))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer shared-fixture")
			busy, err := transport.Do(req, "", 10, 4)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, busy.Body.Close()) })
			results, err := svc.RunCheck(t.Context(), 1)
			require.NoError(t, err)
			require.Len(t, results, 1)
			require.True(t, results[0].Skipped)
			require.Zero(t, probes.Load())
			require.Empty(t, repo.history)
			require.Zero(t, repo.marked)
			require.NoError(t, busy.Body.Close())
			results, err = svc.RunCheck(t.Context(), 1)
			require.NoError(t, err)
			require.False(t, results[0].Skipped)
			require.Equal(t, service.MonitorStatusOperational, results[0].Status)
			require.EqualValues(t, 1, probes.Load())
			require.Equal(t, "Bearer shared-fixture", observedHeader.Load())
			require.Len(t, repo.history, 1)
			require.Equal(t, 1, repo.marked)

			// A probe retains the same slot until its whole response body is read.
			slow.Store(true)
			type outcome struct {
				results []*service.CheckResult
				err     error
			}
			resultCh := make(chan outcome, 1)
			go func() { values, runErr := svc.RunCheck(t.Context(), 1); resultCh <- outcome{values, runErr} }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("probe did not start")
			}
			other, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/other", strings.NewReader("{}"))
			require.NoError(t, err)
			other.Header.Set("Authorization", "Bearer shared-fixture")
			response, err := transport.Do(other, "", 11, 4)
			if response != nil {
				_ = response.Body.Close()
			}
			require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)
			require.Zero(t, otherRequests.Load())
			closeBody.Do(func() { close(finishBody) })
			select {
			case out := <-resultCh:
				require.NoError(t, out.err)
				require.False(t, out.results[0].Skipped)
				require.Equal(t, service.MonitorStatusOperational, out.results[0].Status)
			case <-time.After(5 * time.Second):
				t.Fatal("probe did not finish")
			}
			other, err = http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/other", strings.NewReader("{}"))
			require.NoError(t, err)
			other.Header.Set("Authorization", "Bearer shared-fixture")
			response, err = transport.Do(other, "", 11, 4)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			require.EqualValues(t, 1, otherRequests.Load())
		})
	}
}
