package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

type guardKeyAuthorityFixture struct {
	acquire func(context.Context, []string) (context.Context, func(), error)
}

func (f guardKeyAuthorityFixture) AcquireUpstreamKeySlots(ctx context.Context, fingerprints []string) (context.Context, func(), error) {
	return f.acquire(ctx, fingerprints)
}

func TestAccountTokenGuardProbeSharedKeyAdmission(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"status":"active"}`)
	}))
	t.Cleanup(server.Close)
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "body-token-fixture"}}
	cfg := defaultAccountTokenGuardConfig()
	cfg.ProbeEndpoint = server.URL
	cfg.ProbeHeaders = map[string]string{"Authorization": "Bearer relay-token-fixture"}
	for _, tc := range []struct {
		name         string
		required     bool
		missing      bool
		admitErr     error
		wantState    string
		wantCode     string
		wantCalls    int32
		wantReleases int
	}{
		{"disabled", false, false, nil, AccountTokenGuardProbeOK, "ok", 1, 0},
		{"missing authority", true, true, nil, AccountTokenGuardProbeTransient, "upstream_key_admission_unavailable", 0, 0},
		{"full capacity", true, false, ErrUpstreamKeyCapacity, AccountTokenGuardProbeTransient, "upstream_key_admission_unavailable", 0, 1},
		{"lost during admission", true, false, ErrUpstreamKeyLease, AccountTokenGuardProbeTransient, "upstream_key_lease_lost", 0, 1},
		{"admitted", true, false, nil, AccountTokenGuardProbeOK, "ok", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls.Store(0)
			var fingerprints []string
			var releases int
			var authority UpstreamKeyConcurrency
			if !tc.missing {
				authority = guardKeyAuthorityFixture{acquire: func(ctx context.Context, actual []string) (context.Context, func(), error) {
					fingerprints = append(fingerprints, actual...)
					return ctx, func() { releases++ }, tc.admitErr
				}}
			}
			svc := &AccountTokenGuardService{httpClient: server.Client(), keyAuthority: authority, keyPolicyRequired: tc.required}
			result := svc.probe(context.Background(), cfg, account)
			if result.State != tc.wantState || result.Diagnostic.Code != tc.wantCode || calls.Load() != tc.wantCalls || releases != tc.wantReleases {
				t.Fatalf("state=%q code=%q calls=%d releases=%d", result.State, result.Diagnostic.Code, calls.Load(), releases)
			}
			if tc.required && !tc.missing {
				want := map[string]bool{config.UpstreamKeyFingerprint("body-token-fixture"): false, config.UpstreamKeyFingerprint("relay-token-fixture"): false}
				for _, fingerprint := range fingerprints {
					if _, ok := want[fingerprint]; ok {
						want[fingerprint] = true
					}
				}
				for _, present := range want {
					if !present {
						t.Fatal("final relay and body credentials must both enter shared admission")
					}
				}
			}
			if strings.Contains(result.Detail, "body-token-fixture") || strings.Contains(result.Detail, "relay-token-fixture") || strings.Contains(result.Detail, server.URL) {
				t.Fatal("probe diagnostics leaked credential or endpoint")
			}
		})
	}
}

func TestAccountTokenGuardProbeSharedKeyLeaseLoss(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var cancel context.CancelCauseFunc
			var released bool
			authority := guardKeyAuthorityFixture{acquire: func(ctx context.Context, _ []string) (context.Context, func(), error) {
				leaseCtx, stop := context.WithCancelCause(ctx)
				cancel = stop
				return leaseCtx, func() { released = true }, nil
			}}
			client := &http.Client{Transport: guardRecoveryRoundTripper(func(*http.Request) (*http.Response, error) {
				cancel(ErrUpstreamKeyLease)
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"status":"active"}`))}, nil
			})}
			svc := &AccountTokenGuardService{httpClient: client, keyAuthority: authority, keyPolicyRequired: true}
			cfg := defaultAccountTokenGuardConfig()
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "fixture"}}
			result := svc.probe(context.Background(), cfg, account)
			if result.State != AccountTokenGuardProbeTransient || result.Diagnostic.Code != "upstream_key_lease_lost" || !released {
				t.Fatalf("state=%q code=%q released=%t", result.State, result.Diagnostic.Code, released)
			}
		})
	}
}

func TestAccountTokenGuardProbeSharedKeyLeaseLossOnRelease(t *testing.T) {
	authority := guardKeyAuthorityFixture{acquire: func(ctx context.Context, _ []string) (context.Context, func(), error) {
		leaseCtx, cancel := context.WithCancelCause(ctx)
		return leaseCtx, func() { cancel(ErrUpstreamKeyLease) }, nil
	}}
	client := &http.Client{Transport: guardRecoveryRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"status":"active"}`))}, nil
	})}
	svc := &AccountTokenGuardService{httpClient: client, keyAuthority: authority, keyPolicyRequired: true}
	cfg := defaultAccountTokenGuardConfig()
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "fixture"}}
	result := svc.probe(context.Background(), cfg, account)
	if result.State != AccountTokenGuardProbeTransient || result.Diagnostic.Code != "upstream_key_lease_lost" {
		t.Fatalf("state=%q code=%q", result.State, result.Diagnostic.Code)
	}
}

func TestAccountTokenGuardProbeSharedKeyRedirectBoundary(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Add(1)
		_, _ = io.WriteString(w, `{"status":"active"}`)
	}))
	t.Cleanup(destination.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(origin.Close)
	for _, required := range []bool{false, true} {
		redirected.Store(0)
		authority := guardKeyAuthorityFixture{acquire: func(ctx context.Context, _ []string) (context.Context, func(), error) {
			return ctx, nil, nil
		}}
		svc := &AccountTokenGuardService{httpClient: origin.Client(), keyAuthority: authority, keyPolicyRequired: required}
		cfg := defaultAccountTokenGuardConfig()
		cfg.ProbeEndpoint = origin.URL
		account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "fixture"}}
		result := svc.probe(context.Background(), cfg, account)
		if required && (redirected.Load() != 0 || result.State != AccountTokenGuardProbeTransient || result.Diagnostic.HTTPStatus != http.StatusTemporaryRedirect) {
			t.Fatalf("configured redirect: calls=%d result=%+v", redirected.Load(), result)
		}
		if !required && (redirected.Load() != 1 || result.State != AccountTokenGuardProbeOK) {
			t.Fatalf("unconfigured redirect: calls=%d result=%+v", redirected.Load(), result)
		}
	}
}

func TestReadGuardResultReturnsWithoutEOF(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, `{"type":"result","payload":{"status":"active"}}`)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	svc := &AccountTokenGuardService{httpClient: server.Client()}
	cfg := defaultAccountTokenGuardConfig()
	cfg.ProbeEndpoint = server.URL
	cfg.ProbeTimeoutSeconds = 5
	account := &Account{ID: 1, Name: "test@example.com", Type: AccountTypeOAuth, Platform: PlatformOpenAI,
		Credentials: map[string]any{"access_token": "access-token-canary"}}

	started := time.Now()
	result := svc.probe(context.Background(), cfg, account)
	if result.State != AccountTokenGuardProbeOK {
		t.Fatalf("state = %q, detail = %s", result.State, result.Detail)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("probe waited for EOF instead of returning on result")
	}
	if strings.Contains(result.Detail, "access-token-canary") || strings.Contains(result.Detail, server.URL) {
		t.Fatalf("diagnostic leaked sensitive data: %s", result.Detail)
	}
}

func TestProbeClassifiesHTTP401AsTransient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "password=secret-response-body")
	}))
	defer server.Close()

	svc := &AccountTokenGuardService{httpClient: server.Client()}
	cfg := defaultAccountTokenGuardConfig()
	cfg.ProbeEndpoint = server.URL
	account := &Account{ID: 1, Name: "test@example.com", Type: AccountTypeOAuth, Platform: PlatformOpenAI,
		Credentials: map[string]any{"access_token": "token"}}

	result := svc.probe(context.Background(), cfg, account)
	if result.State != AccountTokenGuardProbeTransient {
		t.Fatalf("state = %q, want transient", result.State)
	}
	if result.Diagnostic.Code != "upstream_http_error" || result.Diagnostic.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("diagnostic = %+v", result.Diagnostic)
	}
	if strings.Contains(result.Detail, "secret-response-body") || strings.Contains(result.Detail, "password") {
		t.Fatalf("diagnostic leaked response body: %s", result.Detail)
	}
}

func TestProbeOnlyExplicitProtocolCodeTriggersRelogin(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		state string
	}{
		{"explicit auth", `{"type":"result","payload":{"status":"failed","error":{"code":"invalid_token","message":"do-not-log"}}}`, AccountTokenGuardProbeAuth},
		{"provider unauthorized text", `{"type":"result","payload":{"status":"failed","error":{"code":"provider_unauthorized","message":"token expired"}}}`, AccountTokenGuardProbeTransient},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()
			svc := &AccountTokenGuardService{httpClient: server.Client()}
			cfg := defaultAccountTokenGuardConfig()
			cfg.ProbeEndpoint = server.URL
			account := &Account{ID: 1, Name: "test@example.com", Type: AccountTypeOAuth, Platform: PlatformOpenAI,
				Credentials: map[string]any{"access_token": "token"}}
			result := svc.probe(context.Background(), cfg, account)
			if result.State != tt.state {
				t.Fatalf("state = %q, want %q; detail=%s", result.State, tt.state, result.Detail)
			}
			if strings.Contains(result.Detail, "do-not-log") || strings.Contains(result.Detail, "token expired") {
				t.Fatalf("diagnostic leaked error message: %s", result.Detail)
			}
		})
	}
}

func TestReadGuardResultDistinguishesIdleMalformedAndOversize(t *testing.T) {
	t.Run("idle", func(t *testing.T) {
		body := &blockingGuardBody{}
		_, _, err := readGuardResult(context.Background(), body, 100, 20*time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "idle timeout") {
			t.Fatalf("err = %v, want idle timeout", err)
		}
	})
	t.Run("malformed", func(t *testing.T) {
		_, _, err := readGuardResult(context.Background(), io.NopCloser(strings.NewReader(`{"type":"result"`)), 100, time.Second)
		if err == nil || !strings.Contains(err.Error(), "malformed") {
			t.Fatalf("err = %v, want malformed", err)
		}
	})
	t.Run("oversize", func(t *testing.T) {
		_, _, err := readGuardResult(context.Background(), io.NopCloser(strings.NewReader(`{"message":"`+strings.Repeat("x", 200)+`"}`)), 32, time.Second)
		if err == nil || !strings.Contains(err.Error(), "too large") {
			t.Fatalf("err = %v, want too large", err)
		}
	})
}

func TestRunCycleIgnoresCallerCancellationAfterAcceptance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(60 * time.Millisecond)
		_, _ = io.WriteString(w, `{"status":"active"}`)
	}))
	defer server.Close()

	repo := &guardMemoryRepo{}
	accounts := &guardMemoryAccounts{items: []Account{{ID: 1, Name: "test@example.com", Status: StatusActive, Type: AccountTypeOAuth, Platform: PlatformOpenAI,
		Credentials: map[string]any{"access_token": "token"}}}}
	svc := NewAccountTokenGuardService(nil, repo, accounts, nil, nil)
	cfg := defaultAccountTokenGuardConfig()
	cfg.ProbeEndpoint = server.URL
	cfg.ProbeConcurrency = 1
	cfg.MaxProbePerCycle = 1
	cfg.ProbeTimeoutSeconds = 5
	svc.config.Store(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stats, err := svc.RunCycle(ctx, false)
	if err != nil {
		t.Fatalf("RunCycle error after caller cancellation: %v", err)
	}
	if stats.Healthy != 1 || repo.upserts == 0 {
		t.Fatalf("stats=%+v upserts=%d", stats, repo.upserts)
	}
}

func TestStartRunDeduplicatesAndCanCancel(t *testing.T) {
	requestStarted := make(chan struct{})
	requestCanceled := make(chan struct{})
	releaseHandler := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain the POST body before signaling readiness so net/http can watch
		// the connection for cancellation while the handler is blocked.
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Errorf("read probe request body: %v", err)
			return
		}
		close(requestStarted)
		select {
		case <-r.Context().Done():
			close(requestCanceled)
		case <-releaseHandler:
		}
	}))
	defer func() {
		close(releaseHandler)
		server.Close()
	}()

	repo := &guardMemoryRepo{}
	accounts := &guardMemoryAccounts{items: []Account{{ID: 1, Name: "test@example.com", Status: StatusActive, Type: AccountTypeOAuth, Platform: PlatformOpenAI,
		Credentials: map[string]any{"access_token": "token"}}}}
	svc := NewAccountTokenGuardService(nil, repo, accounts, nil, nil)
	svc.httpClient = server.Client()
	defer svc.httpClient.CloseIdleConnections()
	cfg := defaultAccountTokenGuardConfig()
	cfg.ProbeEndpoint = server.URL
	cfg.ProbeConcurrency = 1
	cfg.MaxProbePerCycle = 1
	cfg.ProbeTimeoutSeconds = 5
	svc.config.Store(cfg)

	first, err := svc.StartRun(true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = svc.CancelRun(first.ID)
		svc.wg.Wait()
	}()
	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("probe request did not start")
	}
	second, err := svc.StartRun(true)
	if !errors.Is(err, ErrAccountTokenGuardRunning) || second == nil || second.ID != first.ID {
		t.Fatalf("duplicate start = job=%+v err=%v", second, err)
	}
	if _, err := svc.CancelRun(first.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, ok := svc.Job(first.ID)
		if ok && job.Status == AccountTokenGuardJobCanceled {
			select {
			case <-requestCanceled:
			case <-time.After(2 * time.Second):
				t.Fatal("probe handler did not observe request cancellation")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	job, _ := svc.Job(first.ID)
	t.Fatalf("job did not cancel: %+v", job)
}

func TestRunCycleReportsPersistenceErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"active"}`)
	}))
	defer server.Close()

	repo := &guardMemoryRepo{upsertErr: errors.New("store unavailable")}
	accounts := &guardMemoryAccounts{items: []Account{{ID: 1, Name: "test@example.com", Status: StatusActive, Type: AccountTypeOAuth, Platform: PlatformOpenAI,
		Credentials: map[string]any{"access_token": "token"}}}}
	svc := NewAccountTokenGuardService(nil, repo, accounts, nil, nil)
	cfg := defaultAccountTokenGuardConfig()
	cfg.ProbeEndpoint = server.URL
	cfg.ProbeConcurrency = 1
	cfg.MaxProbePerCycle = 1
	cfg.ProbeTimeoutSeconds = 5
	svc.config.Store(cfg)

	stats, err := svc.RunCycle(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.PersistenceErrors == 0 || stats.Failed == 0 {
		t.Fatalf("stats did not expose persistence failure: %+v", stats)
	}
}

type blockingGuardBody struct {
	mu   sync.Mutex
	once sync.Once
	done chan struct{}
}

func (b *blockingGuardBody) Read([]byte) (int, error) {
	b.mu.Lock()
	if b.done == nil {
		b.done = make(chan struct{})
	}
	done := b.done
	b.mu.Unlock()
	<-done
	return 0, io.EOF
}

func (b *blockingGuardBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.done == nil {
		b.done = make(chan struct{})
	}
	b.once.Do(func() { close(b.done) })
	return nil
}

type guardMemoryRepo struct {
	mu        sync.Mutex
	states    []AccountTokenGuardState
	upserts   int
	upsertErr error
}

func (r *guardMemoryRepo) UpsertState(_ context.Context, state AccountTokenGuardState) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.upserts++
	if r.upsertErr != nil {
		return r.upsertErr
	}
	for i, previous := range r.states {
		if previous.AccountID != state.AccountID {
			continue
		}
		if state.LastFixAt == nil {
			state.LastFixAt = previous.LastFixAt
			state.LastFixAction = previous.LastFixAction
			state.LastFixResult = previous.LastFixResult
		}
		r.states[i] = state
		return nil
	}
	r.states = append(r.states, state)
	return nil
}
func (r *guardMemoryRepo) DeleteStatesExcept(context.Context, []int64) error { return nil }
func (r *guardMemoryRepo) RecordEvent(context.Context, AccountTokenGuardEvent) error {
	return nil
}
func (r *guardMemoryRepo) ListEvents(context.Context, int, int) ([]AccountTokenGuardEvent, error) {
	return nil, nil
}
func (r *guardMemoryRepo) ListStates(context.Context) ([]AccountTokenGuardState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]AccountTokenGuardState(nil), r.states...), nil
}
func (r *guardMemoryRepo) PruneEvents(context.Context, time.Time) error { return nil }

type guardMemoryAccounts struct{ items []Account }

func (a *guardMemoryAccounts) GetByID(context.Context, int64) (*Account, error) {
	return nil, errors.New("not implemented")
}
func (a *guardMemoryAccounts) ListAllWithFilters(context.Context, string, string, string, string, int64, string) ([]Account, error) {
	return a.items, nil
}
func (a *guardMemoryAccounts) ClearError(context.Context, int64) error           { return nil }
func (a *guardMemoryAccounts) SetSchedulable(context.Context, int64, bool) error { return nil }
