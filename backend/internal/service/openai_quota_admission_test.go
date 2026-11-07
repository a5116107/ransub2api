package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

func TestOpenAIQuotaProviderClientSharesConfiguredKeyAdmission(t *testing.T) {
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		sends.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(req.URL.Path, "/wham/usage"):
			_, _ = w.Write([]byte(`{"email":"quota@example.com","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600}}}`))
		case strings.HasSuffix(req.URL.Path, "/rate-limit-reset-credits"):
			_, _ = w.Write([]byte(`{"available_count":0}`))
		case strings.HasSuffix(req.URL.Path, "/rate-limit-reset-credits/consume"):
			_, _ = w.Write([]byte(`{"code":"ok","windows_reset":1}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	account := &Account{ID: 909, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "quota-token", "chatgpt_account_id": "quota-account"}}
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	tokens := &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(account): "quota-token"}}
	provider := NewOpenAITokenProvider(repo, tokens, nil)
	admission := &clientAdmissionFixture{}
	admission.blocked.Store(true)
	quota := NewOpenAIQuotaService(repo, nil, provider, newQuotaRedirectingFactory(server), nil)
	quota.requireUpstreamKeyAdmission = true
	quota.upstreamKeyAdmission = admission

	usage, err := quota.QueryUsage(context.Background(), account.ID)
	require.Nil(t, usage)
	require.True(t, IsUpstreamKeyAdmissionFailure(err))
	require.Zero(t, sends.Load(), "full shared Key must block quota usage before transport")

	admission.blocked.Store(false)
	usage, err = quota.QueryUsage(context.Background(), account.ID)
	require.NoError(t, err)
	require.NotNil(t, usage)
	require.GreaterOrEqual(t, sends.Load(), int32(1))

	reset, err := quota.ResetCreditTargeted(context.Background(), account.ID, "credit-1", "redeem-1")
	require.NoError(t, err)
	require.Equal(t, "ok", reset.Code)
	require.GreaterOrEqual(t, admission.starts.Load(), int32(2))
	require.Equal(t, admission.starts.Load(), admission.finish.Load())
}

func TestOpenAIQuotaProviderClientDoesNotMutateSharedClient(t *testing.T) {
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sends.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	shared, err := newQuotaRedirectingFactory(server)("")
	require.NoError(t, err)
	factory := func(string) (*req.Client, error) { return shared, nil }
	account := &Account{ID: 910, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "quota-token", "chatgpt_account_id": "quota-account"}}
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	tokens := &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(account): "quota-token"}}
	admission := &clientAdmissionFixture{}
	quota := NewOpenAIQuotaService(repo, nil, NewOpenAITokenProvider(repo, tokens, nil), factory, nil)
	quota.requireUpstreamKeyAdmission = true
	quota.upstreamKeyAdmission = admission

	for i := 0; i < 2; i++ {
		_, err := quota.QueryUsage(t.Context(), account.ID)
		require.NoError(t, err)
	}
	require.EqualValues(t, 4, sends.Load())
	require.EqualValues(t, 4, admission.starts.Load(), "each request must acquire exactly once")
	require.EqualValues(t, 4, admission.finish.Load())

	_, err = shared.R().SetContext(t.Context()).Get("https://chatgpt.com/unrelated-privacy-request")
	require.NoError(t, err)
	require.EqualValues(t, 5, sends.Load())
	require.EqualValues(t, 4, admission.starts.Load(), "the cached client must not acquire quota wrappers")
}

func TestOpenAIQuotaConcurrentRequestsReuseSharedClient(t *testing.T) {
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sends.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(server.Close)
	shared, err := newQuotaRedirectingFactory(server)("")
	require.NoError(t, err)
	account := &Account{ID: 913, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "quota-token", "chatgpt_account_id": "quota-account"}}
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	tokens := &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(account): "quota-token"}}
	admission := &clientAdmissionFixture{}
	quota := NewOpenAIQuotaService(repo, nil, NewOpenAITokenProvider(repo, tokens, nil), func(string) (*req.Client, error) { return shared, nil }, nil)
	quota.requireUpstreamKeyAdmission = true
	quota.upstreamKeyAdmission = admission

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, queryErr := quota.QueryUsage(t.Context(), account.ID)
			errs <- queryErr
		}()
	}
	wg.Wait()
	close(errs)
	for queryErr := range errs {
		require.NoError(t, queryErr)
	}
	require.EqualValues(t, workers*2, sends.Load())
	require.EqualValues(t, workers*2, admission.starts.Load())
	require.Equal(t, admission.starts.Load(), admission.finish.Load())
}

func TestOpenAIQuotaRedirectPolicyIsIsolatedFromSharedClient(t *testing.T) {
	var destination atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/destination" {
			destination.Add(1)
			_, _ = io.WriteString(w, `{}`)
			return
		}
		http.Redirect(w, r, "/destination", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(server.Close)
	shared, err := newQuotaRedirectingFactory(server)("")
	require.NoError(t, err)
	account := &Account{ID: 914, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "quota-token", "chatgpt_account_id": "quota-account"}}
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	tokens := &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(account): "quota-token"}}
	quota := NewOpenAIQuotaService(repo, nil, NewOpenAITokenProvider(repo, tokens, nil), func(string) (*req.Client, error) { return shared, nil }, nil)
	quota.requireUpstreamKeyAdmission = true
	quota.upstreamKeyAdmission = &clientAdmissionFixture{}

	_, err = quota.QueryUsage(t.Context(), account.ID)
	require.Error(t, err)
	require.Zero(t, destination.Load(), "admitted quota requests must not follow redirects")
	resp, err := shared.R().SetContext(t.Context()).Get("https://chatgpt.com/legacy")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.EqualValues(t, 1, destination.Load(), "other uses of the shared client keep their redirect policy")
}

func TestOpenAIQuotaAgentIdentityShadowAdmissionProof(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(server.Close)
	parent, key := agentKeyAccount(t, 915)
	parent.Credentials["chatgpt_account_id"] = "quota-account"
	shadow := &Account{ID: 916, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &parent.ID}
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{parent.ID: parent, shadow.ID: shadow}}
	var starts atomic.Int32
	admission := &codexProbeUpstream{begin: func(r *http.Request) (*http.Request, func(*http.Response, error), error) {
		require.Equal(t, []string{agentKeyFingerprint(t, key)}, config.UpstreamCredentialFingerprints(r))
		starts.Add(1)
		return r, func(*http.Response, error) {}, nil
	}}
	quota := NewOpenAIQuotaService(repo, nil, nil, newQuotaRedirectingFactory(server), nil)
	quota.requireUpstreamKeyAdmission = true
	quota.upstreamKeyAdmission = admission
	_, err := quota.QueryUsage(t.Context(), shadow.ID)
	require.NoError(t, err)
	require.EqualValues(t, 2, starts.Load())
}

func TestOpenAIQuotaLocalCapacityUsesShortSnapshotRetry(t *testing.T) {
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sends.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	account := &Account{ID: 911, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "quota-token", "chatgpt_account_id": "quota-account"},
		Extra:       map[string]any{"openai_excel_bps": true}}
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	tokens := &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(account): "quota-token"}}
	admission := &clientAdmissionFixture{}
	admission.blocked.Store(true)
	quota := NewOpenAIQuotaService(repo, nil, NewOpenAITokenProvider(repo, tokens, nil), newQuotaRedirectingFactory(server), nil)
	quota.requireUpstreamKeyAdmission = true
	quota.upstreamKeyAdmission = admission
	svc := &AccountUsageService{accountRepo: repo, openAIQuotaService: quota, cache: NewUsageCache()}

	_, err := svc.getOpenAIUsage(t.Context(), account, true)
	require.NoError(t, err)
	require.Zero(t, sends.Load())
	value, ok := svc.cache.openAIProbeCache.Load(account.ID)
	require.True(t, ok)
	reservation, ok := value.(*openAIProbeReservation)
	require.True(t, ok)
	require.True(t, reservation.localRetry, "local capacity denial must not start the normal snapshot cooldown")
	require.LessOrEqual(t, time.Until(reservation.nextAttempt), openAIProbeLocalRetryTTL)

	admission.blocked.Store(false)
	_, err = svc.getOpenAIUsage(t.Context(), account, true)
	require.NoError(t, err)
	require.Zero(t, sends.Load(), "forced refresh must respect the short local retry window")
	svc.cache.openAIProbeCache.Store(account.ID, &openAIProbeReservation{nextAttempt: time.Now().Add(-time.Second), localRetry: true})
	_, err = svc.getOpenAIUsage(t.Context(), account, true)
	require.NoError(t, err)
	require.EqualValues(t, 2, sends.Load(), "after the local retry window quota polling resumes")
}

func TestOpenAIQuotaLeaseLossDuringBodyDoesNotPublishSnapshot(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"rate_limit":`)
		if err := http.NewResponseController(w).Flush(); err != nil {
			return
		}
		close(started)
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	account := &Account{ID: 912, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "quota-token", "chatgpt_account_id": "quota-account"}}
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	tokens := &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(account): "quota-token"}}
	leaseCancel := make(chan context.CancelCauseFunc, 1)
	admission := &codexProbeUpstream{begin: func(r *http.Request) (*http.Request, func(*http.Response, error), error) {
		ctx, cancel := context.WithCancelCause(r.Context())
		leaseCancel <- cancel
		return r.WithContext(ctx), func(*http.Response, error) {}, nil
	}}
	quota := NewOpenAIQuotaService(repo, nil, NewOpenAITokenProvider(repo, tokens, nil), newQuotaRedirectingFactory(server), nil)
	quota.requireUpstreamKeyAdmission = true
	quota.upstreamKeyAdmission = admission
	done := make(chan error, 1)
	go func() { _, err := quota.QueryUsage(t.Context(), account.ID); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("quota response did not start")
	}
	(<-leaseCancel)(ErrUpstreamKeyLease)
	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrUpstreamKeyLease)
	case <-time.After(5 * time.Second):
		t.Fatal("quota query did not stop after lease loss")
	}
	require.Zero(t, repo.extraUpdateCalls, "a response without a valid lease must not update scheduling")
}
