package service

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	httppool "github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/stretchr/testify/require"
)

func TestCodexUsageProbeMissingSharedAuthorityDoesNotSendAndRetries(t *testing.T) {
	var sends atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sends.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)
	host, portString, err := net.SplitHostPort(proxy.Listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portString)
	require.NoError(t, err)
	proxyID := int64(1)
	reset := time.Now().Add(time.Hour)
	account := &Account{
		ID: 201, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials:      map[string]any{"access_token": "shared-fixture"},
		Extra:            map[string]any{"codex_5h_used_percent": 77.0},
		RateLimitResetAt: &reset,
		ProxyID:          &proxyID, Proxy: &Proxy{Protocol: "http", Host: host, Port: port},
	}
	cfg := &config.Config{}
	cfg.Gateway.UpstreamKeyConcurrency.Revision = 1
	cache := NewUsageCache()
	// A configured policy and a transport without the optional authority must
	// fail closed; direct clients without a policy retain their legacy behavior.
	gateway := &OpenAIGatewayService{cfg: cfg, httpUpstream: &httpUpstreamRecorder{}}
	svc := ProvideAccountUsageService(nil, nil, nil, nil, nil, nil, nil, nil, cache, nil, nil, gateway)
	usage, err := svc.GetUsageForAccount(context.Background(), account)
	require.NoError(t, err)
	require.NotNil(t, usage)
	require.Zero(t, sends.Load(), "no CONNECT or model request may leave on a local admission failure")
	require.Equal(t, 77.0, account.Extra["codex_5h_used_percent"])
	require.Equal(t, reset, *account.RateLimitResetAt)
	require.Nil(t, svc.reserveOpenAICodexSnapshot(account.ID, time.Now(), false), "local retries must be bounded")
	require.Nil(t, svc.reserveOpenAICodexSnapshot(account.ID, time.Now(), true), "forced retries are also briefly bounded")
	require.NotNil(t, svc.reserveOpenAICodexSnapshot(account.ID, time.Now().Add(10*time.Second), false), "recovery must not be delayed for the full ten-minute supplier probe TTL")
}

func TestCodexUsageProbeAdmissionRefusalNeverSends(t *testing.T) {
	for _, scenario := range []string{"capacity", "policy", "lease", "nil-request", "nil-finish", "error-with-finish"} {
		t.Run(scenario, func(t *testing.T) {
			var sent, finished atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { sent.Add(1) }))
			t.Cleanup(server.Close)
			account := codexProbeFixtureAccount()
			codexProbeLoopback(t, account, server)
			upstream := &codexProbeUpstream{httpUpstreamRecorder: &httpUpstreamRecorder{}}
			upstream.begin = func(req *http.Request) (*http.Request, func(*http.Response, error), error) {
				switch scenario {
				case "capacity":
					return req, nil, ErrUpstreamKeyCapacity
				case "policy":
					return req, nil, ErrUpstreamKeyPolicy
				case "lease":
					return req, nil, ErrUpstreamKeyLease
				case "nil-request":
					return nil, func(_ *http.Response, err error) {
						if errors.Is(err, ErrUpstreamKeyPolicy) {
							finished.Add(1)
						}
					}, nil
				case "nil-finish":
					return req, nil, nil
				default:
					return req, func(_ *http.Response, err error) {
						if errors.Is(err, ErrUpstreamKeyCapacity) {
							finished.Add(1)
						}
					}, ErrUpstreamKeyCapacity
				}
			}
			svc := &AccountUsageService{upstreamKeyAdmission: upstream, requireUpstreamKeyAdmission: true}
			updates, err := svc.probeOpenAICodexSnapshot(t.Context(), account)
			require.Error(t, err)
			require.Empty(t, updates)
			require.Zero(t, sent.Load())
			if scenario == "nil-request" || scenario == "error-with-finish" {
				require.EqualValues(t, 1, finished.Load())
			} else {
				require.Zero(t, finished.Load())
			}
		})
	}
}

func TestCodexUsageProbeRegionalMissingAuthorityFailsClosed(t *testing.T) {
	for _, mode := range []string{"regional-concurrency", "regional-scheduler"} {
		t.Run(mode, func(t *testing.T) {
			cfg := &config.Config{}
			if mode == "regional-concurrency" {
				cfg.Runtime.RegionalConcurrencyFile = "fixture"
			} else {
				cfg.Runtime.RegionalSchedulerFile = "fixture"
			}
			gateway := &OpenAIGatewayService{cfg: cfg, httpUpstream: &httpUpstreamRecorder{}}
			svc := ProvideAccountUsageService(nil, nil, nil, nil, nil, nil, nil, nil, NewUsageCache(), nil, nil, gateway)
			require.True(t, svc.requireUpstreamKeyAdmission)
			updates, err := svc.probeOpenAICodexSnapshot(t.Context(), codexProbeFixtureAccount())
			require.ErrorIs(t, err, ErrUpstreamKeyPolicy)
			require.Empty(t, updates)
		})
	}
}

type codexProbeUpstream struct {
	*httpUpstreamRecorder
	begin func(*http.Request) (*http.Request, func(*http.Response, error), error)
}

func (s *codexProbeUpstream) BeginUpstreamKeyRequest(req *http.Request) (*http.Request, func(*http.Response, error), error) {
	return s.begin(req)
}

type codexProbeRoundTripFunc func(*http.Request) (*http.Response, error)

func (f codexProbeRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type codexProbeCloseBody struct {
	io.ReadCloser
	once    sync.Once
	onClose func()
}

func (b *codexProbeCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.onClose)
	return err
}

type codexProbeRecoveryRepo struct {
	accountUsageCodexProbeRepo
	cleared chan struct{}
}

func (r *codexProbeRecoveryRepo) ClearOpenAIRateLimitIfObserved(context.Context, int64, time.Time, time.Time) (bool, error) {
	r.cleared <- struct{}{}
	return true, nil
}

// The service still builds its regular pooled proxy client. Only this unique
// fixture's cached transport is replaced, so traffic terminates on loopback.
func codexProbeLoopback(t *testing.T, account *Account, upstream *httptest.Server) {
	t.Helper()
	host, portText, err := net.SplitHostPort(upstream.Listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	proxyID := int64(1)
	account.ProxyID, account.Proxy = &proxyID, &Proxy{Protocol: "http", Host: host, Port: port}
	proxyURL := account.Proxy.URL()
	client, err := httppool.GetClient(httppool.Options{ProxyURL: proxyURL, Timeout: 15 * time.Second, ResponseHeaderTimeout: 10 * time.Second})
	require.NoError(t, err)
	original := client.Transport
	base, err := url.Parse(upstream.URL)
	require.NoError(t, err)
	client.Transport = codexProbeRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		copy := req.Clone(req.Context())
		if copy.URL.Hostname() == "chatgpt.com" {
			copy.URL.Scheme, copy.URL.Host = base.Scheme, base.Host
		}
		return http.DefaultTransport.RoundTrip(copy)
	})
	t.Cleanup(func() {
		client.Transport = original
		httppool.EvictProxyClients(proxyURL)
	})
}

func codexProbeFixtureAccount() *Account {
	now := time.Now()
	limited, reset := now.Add(-time.Minute), now.Add(time.Hour)
	return &Account{ID: 202, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "shared-fixture", "chatgpt_account_id": "account-fixture"},
		Extra:       map[string]any{"codex_5h_used_percent": 77.0}, RateLimitedAt: &limited, RateLimitResetAt: &reset}
}

func TestCodexUsageProbeAdmitsFinalHeadersAndPreservesResponse(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusTooManyRequests} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var active, sent, released atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				sent.Add(1)
				if active.Load() != 1 || req.Host != "chatgpt.com" || req.Header.Get("Authorization") != "Bearer shared-fixture" ||
					req.Header.Get("Chatgpt-Account-Id") != "account-fixture" || req.Header.Get("Accept") != "text/event-stream" ||
					req.Header.Get("Openai-Beta") != "responses=experimental" || req.Header.Get("User-Agent") == "" {
					t.Error("probe reached server without its admitted identity and original headers")
				}
				w.Header().Set("x-codex-primary-used-percent", "0")
				w.Header().Set("x-codex-primary-window-minutes", "10080")
				w.Header().Set("x-codex-secondary-used-percent", "0")
				w.Header().Set("x-codex-secondary-window-minutes", "300")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "data: fixture\n\n")
			}))
			t.Cleanup(server.Close)
			account := codexProbeFixtureAccount()
			codexProbeLoopback(t, account, server)
			repo := &codexProbeRecoveryRepo{accountUsageCodexProbeRepo: accountUsageCodexProbeRepo{updateExtraCh: make(chan map[string]any, 1)}, cleared: make(chan struct{}, 1)}
			upstream := &codexProbeUpstream{httpUpstreamRecorder: &httpUpstreamRecorder{}}
			upstream.begin = func(req *http.Request) (*http.Request, func(*http.Response, error), error) {
				require.Equal(t, []string{config.UpstreamKeyFingerprint("shared-fixture")}, config.UpstreamCredentialFingerprints(req))
				active.Add(1)
				return req, func(resp *http.Response, err error) {
					require.NoError(t, err)
					require.NotNil(t, resp)
					resp.Body = &codexProbeCloseBody{ReadCloser: resp.Body, onClose: func() { active.Add(-1); released.Add(1) }}
				}, nil
			}
			cfg := &config.Config{}
			cfg.Gateway.UpstreamKeyConcurrency.Revision = 1
			svc := ProvideAccountUsageService(repo, nil, nil, nil, nil, nil, nil, nil, NewUsageCache(), nil, nil,
				&OpenAIGatewayService{cfg: cfg, httpUpstream: upstream})
			usage, err := svc.GetUsageForAccount(t.Context(), account)
			require.NoError(t, err)
			require.Equal(t, 0.0, usage.FiveHour.Utilization)
			require.Equal(t, 0.0, usage.SevenDay.Utilization)
			require.EqualValues(t, 1, sent.Load())
			require.EqualValues(t, 1, released.Load())
			require.Zero(t, active.Load())
			select {
			case <-repo.updateExtraCh:
			case <-time.After(time.Second):
				t.Fatal("response headers were not persisted")
			}
			if status == http.StatusOK {
				select {
				case <-repo.cleared:
				case <-time.After(time.Second):
					t.Fatal("complete successful zero-usage snapshot did not clear observed cooldown")
				}
			} else {
				select {
				case <-repo.cleared:
					t.Fatal("429 snapshot must not clear cooldown")
				case <-time.After(100 * time.Millisecond):
				}
			}
		})
	}
}

func TestCodexUsageProbeLeaseLossDiscardsBufferedHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-codex-primary-used-percent", "0")
		w.Header().Set("x-codex-primary-window-minutes", "300")
		_, _ = io.WriteString(w, "data: buffered\n\n")
	}))
	t.Cleanup(server.Close)
	account := codexProbeFixtureAccount()
	codexProbeLoopback(t, account, server)
	repo := &codexProbeRecoveryRepo{accountUsageCodexProbeRepo: accountUsageCodexProbeRepo{updateExtraCh: make(chan map[string]any, 1)}, cleared: make(chan struct{}, 1)}
	var closed atomic.Int32
	upstream := &codexProbeUpstream{httpUpstreamRecorder: &httpUpstreamRecorder{}}
	upstream.begin = func(req *http.Request) (*http.Request, func(*http.Response, error), error) {
		ctx, cancel := context.WithCancelCause(req.Context())
		return req.WithContext(ctx), func(resp *http.Response, _ error) {
			resp.Body = &codexProbeCloseBody{ReadCloser: resp.Body, onClose: func() { closed.Add(1) }}
			cancel(ErrUpstreamKeyLease)
		}, nil
	}
	svc := &AccountUsageService{accountRepo: repo, cache: NewUsageCache(), upstreamKeyAdmission: upstream, requireUpstreamKeyAdmission: true}
	usage, err := svc.GetUsageForAccount(t.Context(), account)
	require.NoError(t, err)
	require.Equal(t, 77.0, usage.FiveHour.Utilization)
	require.EqualValues(t, 1, closed.Load())
	require.Empty(t, repo.updateExtraCh)
	require.Empty(t, repo.cleared)
	require.Nil(t, svc.reserveOpenAICodexSnapshot(account.ID, time.Now(), true))
	require.NotNil(t, svc.reserveOpenAICodexSnapshot(account.ID, time.Now().Add(10*time.Second), false))
}

func TestCodexUsageProbeCancellationReleasesAdmission(t *testing.T) {
	started := make(chan struct{})
	allowExit := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		close(started)
		select {
		case <-req.Context().Done():
		case <-allowExit:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(allowExit) })
	account := codexProbeFixtureAccount()
	codexProbeLoopback(t, account, server)
	var finished atomic.Int32
	upstream := &codexProbeUpstream{httpUpstreamRecorder: &httpUpstreamRecorder{}}
	upstream.begin = func(req *http.Request) (*http.Request, func(*http.Response, error), error) {
		return req, func(_ *http.Response, err error) {
			if errors.Is(err, context.Canceled) {
				finished.Add(1)
			}
		}, nil
	}
	svc := &AccountUsageService{upstreamKeyAdmission: upstream, requireUpstreamKeyAdmission: true}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { _, err := svc.probeOpenAICodexSnapshot(ctx, account); done <- err }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("fixture request did not start")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled probe did not finish")
	}
	require.EqualValues(t, 1, finished.Load())
}

func TestCodexUsageProbeDoesNotRedirectConfiguredCredential(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Add(1)
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(destination.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, destination.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(origin.Close)
	account := codexProbeFixtureAccount()
	codexProbeLoopback(t, account, origin)
	upstream := &codexProbeUpstream{httpUpstreamRecorder: &httpUpstreamRecorder{}}
	upstream.begin = func(req *http.Request) (*http.Request, func(*http.Response, error), error) {
		return req, func(*http.Response, error) {}, nil
	}
	svc := &AccountUsageService{upstreamKeyAdmission: upstream, requireUpstreamKeyAdmission: true}
	_, err := svc.probeOpenAICodexSnapshot(t.Context(), account)
	require.ErrorContains(t, err, "status 307")
	require.Zero(t, redirected.Load())
	svc.upstreamKeyAdmission, svc.requireUpstreamKeyAdmission = nil, false
	_, err = svc.probeOpenAICodexSnapshot(t.Context(), account)
	require.NoError(t, err)
	require.EqualValues(t, 1, redirected.Load(), "unconfigured legacy redirect semantics remain unchanged")
}

func TestCodexUsageProbeReservationDoesNotClobberNewerAttempt(t *testing.T) {
	svc := &AccountUsageService{cache: NewUsageCache()}
	now := time.Now()
	first := svc.reserveOpenAICodexSnapshot(1, now, false)
	newer := svc.reserveOpenAICodexSnapshot(1, now, true)
	require.NotNil(t, first)
	require.NotNil(t, newer)
	require.NotSame(t, first, newer)
	svc.retryOpenAICodexSnapshotAfterLocalFailure(1, first)
	got, ok := svc.cache.openAIProbeCache.Load(int64(1))
	require.True(t, ok)
	require.Same(t, newer, got)
	svc.retryOpenAICodexSnapshotAfterLocalFailure(1, newer)
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if svc.reserveOpenAICodexSnapshot(1, now.Add(10*time.Second), false) != nil {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()
	require.EqualValues(t, 1, admitted.Load())
}

func TestCodexUsageProbeAgentIdentityHeadersReachAdmission(t *testing.T) {
	key, privateKey := newTestAgentIdentityKey(t)
	account := codexProbeFixtureAccount()
	account.Credentials = map[string]any{"auth_mode": OpenAIAuthModeAgentIdentity, "agent_runtime_id": key.runtimeID,
		"agent_private_key": privateKey, "task_id": key.taskID, "chatgpt_account_id": "account-fixture"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	t.Cleanup(server.Close)
	codexProbeLoopback(t, account, server)
	upstream := &codexProbeUpstream{httpUpstreamRecorder: &httpUpstreamRecorder{}}
	upstream.begin = func(req *http.Request) (*http.Request, func(*http.Response, error), error) {
		authorization := req.Header.Get("Authorization")
		require.True(t, strings.HasPrefix(authorization, "AgentAssertion "))
		require.False(t, strings.Contains(authorization, privateKey), "private key must not appear in the assertion")
		// AgentAssertion is not a stable supported fingerprint yet; observing it
		// here does not claim it shares the configured Bearer budget.
		require.Empty(t, config.UpstreamCredentialFingerprints(req))
		return req, func(*http.Response, error) {}, nil
	}
	svc := &AccountUsageService{upstreamKeyAdmission: upstream, requireUpstreamKeyAdmission: true}
	_, err := svc.probeOpenAICodexSnapshot(t.Context(), account)
	require.NoError(t, err)
}
