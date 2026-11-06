//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

type liveTransferCacheStub struct {
	stubConcurrencyCacheForTest
	transferCalls   int
	transferredKeys []string
	lastTransfer    LiveLeaseTransferRequest
	transferResult  bool
	transferErr     error
	legacyCalls     int
	legacyResult    bool
	abortCalls      int
	abortErr        error
	onTransfer      func()

	migrateCalls     int
	migratedAccounts []int64
	migrateRequest   LiveLeaseTransferRequest
	migrateResult    bool
	migrateErr       error
	accountRelease   int
	releaseLeaseCall int
	releasedLeaseID  string
}

func (c *liveTransferCacheStub) AcquireLiveLease(context.Context, int64, int, int64, int, int64, int, string, bool) (bool, error) {
	c.legacyCalls++
	return c.legacyResult, nil
}
func (c *liveTransferCacheStub) AcquireLiveLeaseTransferring(_ context.Context, request LiveLeaseTransferRequest) (bool, error) {
	c.transferCalls++
	c.lastTransfer = request
	c.transferredKeys = append(c.transferredKeys, request.KeyRequestID)
	if c.onTransfer != nil {
		c.onTransfer()
	}
	return c.transferResult, c.transferErr
}
func (c *liveTransferCacheStub) MigrateLiveLeaseAccount(_ context.Context, request LiveLeaseTransferRequest) (bool, error) {
	c.migrateCalls++
	c.migrateRequest = request
	c.migratedAccounts = append(c.migratedAccounts, request.AccountID)
	return c.migrateResult, c.migrateErr
}
func (c *liveTransferCacheStub) ReleaseLiveLeaseAccount(context.Context, int64, string) error {
	c.accountRelease++
	return nil
}
func (c *liveTransferCacheStub) AbortLiveLeaseTransfer(context.Context, LiveLeaseTransferRequest) error {
	c.abortCalls++
	return c.abortErr
}
func (c *liveTransferCacheStub) RefreshLiveLease(context.Context, int64, int64, int64, string) (bool, error) {
	return true, nil
}
func (c *liveTransferCacheStub) ReleaseLiveLease(_ context.Context, _, _, _ int64, leaseID string) error {
	c.releaseLeaseCall++
	c.releasedLeaseID = leaseID
	return nil
}

// liveLeaseOnlyCacheStub intentionally has no transfer support so the fallback
// branch is exercised.
type liveLeaseOnlyCacheStub struct {
	stubConcurrencyCacheForTest
	legacyCalls int
}

func (c *liveLeaseOnlyCacheStub) AcquireLiveLease(context.Context, int64, int, int64, int, int64, int, string, bool) (bool, error) {
	c.legacyCalls++
	return false, nil
}
func (c *liveLeaseOnlyCacheStub) RefreshLiveLease(context.Context, int64, int64, int64, string) (bool, error) {
	return true, nil
}
func (c *liveLeaseOnlyCacheStub) ReleaseLiveLease(context.Context, int64, int64, int64, string) error {
	return nil
}

func TestAcquireLiveLeaseForIdentityTransfersExactMembers(t *testing.T) {
	cache := &liveTransferCacheStub{transferResult: true}
	paused := 0
	pausedBeforeTransfer := false
	cache.onTransfer = func() { pausedBeforeTransfer = paused > 0 }
	reservation := &APIKeySlotReservation{
		apiKeyID:     9,
		requestID:    "key-req-1",
		limit:        1,
		release:      func() {},
		pauseRenewal: func() { paused++ },
	}

	acquired, err := acquireLiveLeaseForIdentity(
		context.Background(), cache,
		LiveCallIdentity{KeyReservation: reservation},
		1, 2, "acct-req", 2, 3, "user-req", 9, 1, "live-lease",
	)
	require.NoError(t, err)
	require.True(t, acquired)
	require.True(t, pausedBeforeTransfer, "ordinary key renewal stops before the atomic transfer")
	require.Equal(t, []string{"key-req-1"}, cache.transferredKeys)
	require.Zero(t, cache.legacyCalls, "reservation paths must not double count capacity")
	require.Empty(t, reservation.RequestID(), "consumed reservation cannot be transferred twice")
	require.Equal(t, "acct-req", cache.lastTransfer.AccountRequestID)
	require.Equal(t, "user-req", cache.lastTransfer.UserRequestID)
	require.Zero(t, cache.abortCalls)
}

func TestAcquireLiveLeaseForIdentityTransfersStatsOnlyMemberAtomically(t *testing.T) {
	cache := &liveTransferCacheStub{transferResult: true}
	released := 0
	paused := 0
	reservation := &APIKeySlotReservation{
		apiKeyID:     9,
		requestID:    "stats-member",
		limit:        0,
		release:      func() { released++ },
		pauseRenewal: func() { paused++ },
	}

	acquired, err := acquireLiveLeaseForIdentity(
		context.Background(), cache,
		LiveCallIdentity{KeyReservation: reservation},
		1, 0, "", 2, 0, "", 9, 0, "live-lease",
	)
	require.NoError(t, err)
	require.True(t, acquired)
	require.Equal(t, "stats-member", cache.lastTransfer.KeyRequestID,
		"stats-only member is moved by the same atomic transfer")
	require.Empty(t, reservation.RequestID())
	require.Positive(t, paused, "stats renewal worker is stopped on consume")
	// The original release cannot delete the transferred Live member.
	reservation.Release()
	require.Zero(t, released)
}

func TestAcquireLiveLeaseForIdentityLimitedKeyWithoutReservationFailsClosed(t *testing.T) {
	cache := &liveTransferCacheStub{transferResult: true}

	acquired, err := acquireLiveLeaseForIdentity(
		context.Background(), cache,
		LiveCallIdentity{},
		1, 0, "", 2, 0, "", 9, 2, "live-lease",
	)
	require.False(t, acquired)
	require.ErrorIs(t, err, ErrAPIKeyReservationLost)
	require.Zero(t, cache.transferCalls)
	require.Zero(t, cache.legacyCalls)
}

func TestAcquireLiveLeaseForIdentityUncertainAbortsExactTransfer(t *testing.T) {
	cache := &liveTransferCacheStub{transferErr: ErrLiveLeaseTransferUncertain}
	reservation := &APIKeySlotReservation{
		apiKeyID:     9,
		requestID:    "key-req-2",
		limit:        1,
		release:      func() {},
		pauseRenewal: func() {},
	}

	acquired, err := acquireLiveLeaseForIdentity(
		context.Background(), cache,
		LiveCallIdentity{KeyReservation: reservation},
		1, 0, "", 2, 0, "", 9, 1, "live-lease",
	)
	require.False(t, acquired)
	require.ErrorIs(t, err, ErrLiveUnavailable, "unknown transfer acknowledgement must not start SDP")
	require.Equal(t, 1, cache.abortCalls, "unknown acknowledgement must be fenced and cleaned")
	require.Empty(t, reservation.RequestID(), "reservation belongs to the aborted successor")
}

func TestAcquireLiveLeaseForIdentityDefiniteFailureKeepsSourcesReleasable(t *testing.T) {
	cache := &liveTransferCacheStub{transferErr: ErrLiveConcurrencyFull}
	released := 0
	reservation := &APIKeySlotReservation{
		apiKeyID:     9,
		requestID:    "key-req-3",
		limit:        1,
		release:      func() { released++ },
		pauseRenewal: func() {},
	}

	acquired, err := acquireLiveLeaseForIdentity(
		context.Background(), cache,
		LiveCallIdentity{KeyReservation: reservation},
		1, 0, "", 2, 0, "", 9, 1, "live-lease",
	)
	require.False(t, acquired)
	require.ErrorIs(t, err, ErrLiveConcurrencyFull)
	require.Zero(t, cache.abortCalls, "a definite refusal did not change Redis state")
	require.Equal(t, "key-req-3", reservation.RequestID())
	reservation.Release()
	require.Equal(t, 1, released, "ordinary sources stay owned by the caller")
}

func TestAcquireLiveLeaseForIdentityFallsBackWithRelease(t *testing.T) {
	cache := &liveLeaseOnlyCacheStub{}
	released := 0
	reservation := &APIKeySlotReservation{apiKeyID: 9, requestID: "key-req-4", limit: 1, release: func() { released++ }}

	acquired, err := acquireLiveLeaseForIdentity(
		context.Background(), cache,
		LiveCallIdentity{KeyReservation: reservation},
		1, 1, "acct-req", 2, 1, "user-req", 9, 1, "live-lease",
	)
	require.NoError(t, err)
	require.False(t, acquired)
	require.Equal(t, 1, cache.legacyCalls)
	require.Equal(t, 1, released, "a cache without transfer support must release the reservation")
}

func TestAcquireLiveLeaseForIdentityUncertainAbortFailureSurfacesError(t *testing.T) {
	cache := &liveTransferCacheStub{
		transferErr: ErrLiveLeaseTransferUncertain,
		abortErr:    errors.New("redis down"),
	}
	reservation := &APIKeySlotReservation{
		apiKeyID:     9,
		requestID:    "key-req-5",
		limit:        1,
		release:      func() {},
		pauseRenewal: func() {},
	}

	acquired, err := acquireLiveLeaseForIdentity(
		context.Background(), cache,
		LiveCallIdentity{KeyReservation: reservation},
		1, 0, "", 2, 0, "", 9, 1, "live-lease",
	)
	require.False(t, acquired)
	require.ErrorIs(t, err, ErrLiveUnavailable)
	require.Contains(t, err.Error(), "abort Live transfer")
	require.Empty(t, reservation.RequestID())
}

func TestMigrateLiveLeaseAccountKeepsJointLease(t *testing.T) {
	cache := &liveTransferCacheStub{migrateResult: true}
	acquired, err := migrateLiveLeaseAccount(
		context.Background(), cache,
		LiveCallIdentity{UserID: 2, APIKeyID: 9},
		&Account{ID: 77, Concurrency: 1},
		"acct-new",
		"joint-lease",
		55,
	)
	require.NoError(t, err)
	require.True(t, acquired)
	require.Equal(t, 1, cache.migrateCalls)
	require.Equal(t, int64(77), cache.migrateRequest.AccountID)
	require.Equal(t, "acct-new", cache.migrateRequest.AccountRequestID)
	require.Equal(t, "joint-lease", cache.migrateRequest.LeaseID)
	require.Equal(t, int64(55), cache.migrateRequest.ReplacedAccountID)
	require.Zero(t, cache.transferCalls, "retry must not re-enter the Key transfer")
}

func TestMigrateLiveLeaseAccountUncertainAbortsAndFailsClosed(t *testing.T) {
	cache := &liveTransferCacheStub{migrateErr: ErrLiveLeaseTransferUncertain}
	acquired, err := migrateLiveLeaseAccount(
		context.Background(), cache,
		LiveCallIdentity{UserID: 2, APIKeyID: 9},
		&Account{ID: 77, Concurrency: 1},
		"acct-new",
		"joint-lease",
		0,
	)
	require.False(t, acquired)
	require.ErrorIs(t, err, ErrLiveUnavailable)
	require.Equal(t, 1, cache.abortCalls)
}

// --- CreateLiveCall retry-loop joint lease cleanup regression ---

type liveLoopStoreStub struct {
	stubGatewayCache
	saves int
}

func (s *liveLoopStoreStub) SaveLiveCall(context.Context, *LiveCallRecord, time.Duration) error {
	s.saves++
	return nil
}
func (s *liveLoopStoreStub) GetLiveCall(context.Context, string) (*LiveCallRecord, error) {
	return nil, ErrLiveCallNotFound
}
func (s *liveLoopStoreStub) ClaimLiveController(context.Context, string, string, string) (bool, error) {
	return false, nil
}
func (s *liveLoopStoreStub) ReleaseLiveController(context.Context, string, string) (bool, error) {
	return false, nil
}
func (s *liveLoopStoreStub) GetLiveController(context.Context, string) (string, error) {
	return "", nil
}
func (s *liveLoopStoreStub) MarkLiveCallClosed(context.Context, string, time.Duration) (bool, error) {
	return false, nil
}

type liveLoopUpstreamStub struct {
	calls    int
	failures int
}

func (s *liveLoopUpstreamStub) Do(*http.Request, string, int64, int) (*http.Response, error) {
	s.calls++
	if s.calls <= s.failures {
		return nil, errors.New("live sdp transport failure")
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Location": {fmt.Sprintf("/backend-api/codex/call_%d", s.calls)}},
		Body:       io.NopCloser(strings.NewReader("v=0\r\n")),
	}, nil
}

func (s *liveLoopUpstreamStub) DoWithTLS(request *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return s.Do(request, proxyURL, accountID, accountConcurrency)
}

func newLiveLoopFixture(t *testing.T, upstream *liveLoopUpstreamStub, withAccessToken bool) (*OpenAIGatewayService, *liveTransferCacheStub, *liveLoopStoreStub) {
	t.Helper()
	snapshot := &snapshotHydrationCache{accounts: map[int64]*Account{}}
	for i := int64(1); i <= 6; i++ {
		credentials := map[string]any{
			"chatgpt_account_id": fmt.Sprintf("acct-%d", i),
			"model_mapping":      map[string]any{"gpt-live-test": "gpt-live-test"},
		}
		if withAccessToken {
			credentials["access_token"] = fmt.Sprintf("token-%d", i)
		}
		account := &Account{
			ID:          i,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeOAuth,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    int(i),
			Credentials: credentials,
		}
		snapshot.accounts[i] = account
		snapshot.snapshot = append(snapshot.snapshot, account)
	}

	cacheStub := &liveTransferCacheStub{
		stubConcurrencyCacheForTest: stubConcurrencyCacheForTest{acquireResult: true},
		transferResult:              true,
		migrateResult:               true,
	}
	store := &liveLoopStoreStub{}
	svc := &OpenAIGatewayService{
		// nil cfg keeps schedulingConfig() defaults so the load-aware selection
		// path (matching production) is exercised; nil accountRepo keeps the
		// scheduler-snapshot recheck from dropping the fixture accounts.
		cfg:                   nil,
		cache:                 store,
		schedulerSnapshot:     NewSchedulerSnapshotService(snapshot, nil, stubOpenAIAccountRepo{}, nil, nil),
		concurrencyService:    NewConcurrencyService(cacheStub),
		httpUpstream:          upstream,
		liveAttestation:       liveAttestationStub{header: `{"v":1,"s":0,"t":"v1.test"}`},
		liveAttestationCipher: newLiveAttestationCipher(&config.Config{JWT: config.JWTConfig{Secret: "live-loop-secret"}}),
	}
	return svc, cacheStub, store
}

func liveLoopIdentity() LiveCallIdentity {
	return LiveCallIdentity{
		UserID:                 20,
		APIKeyID:               9,
		APIKeyConcurrencyLimit: 1,
		UserRequestID:          "user-req",
		KeyReservation: &APIKeySlotReservation{
			apiKeyID:     9,
			requestID:    "key-req",
			limit:        1,
			release:      func() {},
			pauseRenewal: func() {},
		},
	}
}

func liveLoopRequest() *LiveCallRequest {
	return &LiveCallRequest{
		SDP:     "v=0\r\n",
		Session: json.RawMessage(`{"model":"gpt-live-test"}`),
	}
}

// TestCreateLiveCallExhaustedRetriesReleaseJointLease proves four retryable SDP
// failures release the retained Key/user members immediately (no TTL wait).
func TestCreateLiveCallExhaustedRetriesReleaseJointLease(t *testing.T) {
	svc, cacheStub, store := newLiveLoopFixture(t, &liveLoopUpstreamStub{}, false)

	created, err := svc.CreateLiveCall(context.Background(), liveLoopRequest(), liveLoopIdentity(), 1)
	require.Nil(t, created)
	require.Error(t, err, "all four attempts must fail on the missing access token")

	require.Equal(t, 1, cacheStub.transferCalls, "first attempt transfers the joint lease")
	require.Equal(t, 3, cacheStub.migrateCalls, "retries migrate the account member")
	require.Equal(t, 4, cacheStub.accountRelease, "each failure releases only its account member")
	require.Equal(t, 1, cacheStub.releaseLeaseCall,
		"exhausting the retries must release the retained joint Key/user members once")
	require.NotEmpty(t, cacheStub.releasedLeaseID)
	require.Equal(t, cacheStub.lastTransfer.LeaseID, cacheStub.releasedLeaseID,
		"the released lease is the retained joint lease")
	require.Zero(t, store.saves)
}

// TestCreateLiveCallSafeRetryRetainsJointLease proves the joint members survive
// a retryable failure until the call reaches a terminal outcome, and no
// terminal release happens while the live call continues.
func TestCreateLiveCallSafeRetryRetainsJointLease(t *testing.T) {
	upstream := &liveLoopUpstreamStub{failures: 1}
	svc, cacheStub, store := newLiveLoopFixture(t, upstream, true)

	created, err := svc.CreateLiveCall(context.Background(), liveLoopRequest(), liveLoopIdentity(), 1)
	require.NoError(t, err)
	require.NotNil(t, created)
	require.NotEmpty(t, created.CallID)

	require.Equal(t, 1, cacheStub.transferCalls)
	require.Equal(t, 1, cacheStub.migrateCalls)
	require.Equal(t, 1, cacheStub.accountRelease, "only the failed account member is released")
	require.Zero(t, cacheStub.releaseLeaseCall,
		"a live call keeps its joint Key/user lease; terminal release comes from finalize")
	require.Equal(t, 1, store.saves)
}

type liveKeyHTTPUpstreamStub struct {
	liveHTTPUpstreamStub
	lease           *PersistentUpstreamKeyLease
	acquireErr      error
	transportErr    error
	checkErr        error
	releaseErr      error
	status          int
	missingLocation bool
	brokenBody      bool
	onAcquire       func()
	onSend          func()
	calls           int
	checks          int
	releases        int
}

func (s *liveKeyHTTPUpstreamStub) AcquirePersistentUpstreamKeySlots(_ context.Context, req *http.Request) (*PersistentUpstreamKeyLease, error) {
	if s.onAcquire != nil {
		s.onAcquire()
	}
	if s.acquireErr != nil {
		return nil, s.acquireErr
	}
	fingerprints := config.UpstreamCredentialFingerprints(req)
	s.lease = &PersistentUpstreamKeyLease{Authority: "redis", Member: "live:fixture", Fingerprints: fingerprints, Resources: []PersistentUpstreamKeyResource{{Fingerprint: fingerprints[0]}}}
	return s.lease, nil
}

func (s *liveKeyHTTPUpstreamStub) CheckPersistentUpstreamKeySlots(context.Context, *PersistentUpstreamKeyLease) error {
	s.checks++
	return s.checkErr
}

func (s *liveKeyHTTPUpstreamStub) ReleasePersistentUpstreamKeySlots(context.Context, *PersistentUpstreamKeyLease) error {
	s.releases++
	return s.releaseErr
}

func (s *liveKeyHTTPUpstreamStub) Do(req *http.Request, proxy string, account int64, concurrency int) (*http.Response, error) {
	s.calls++
	if s.onSend != nil {
		s.onSend()
	}
	if s.transportErr != nil {
		return nil, s.transportErr
	}
	response, err := s.liveHTTPUpstreamStub.Do(req, proxy, account, concurrency)
	if err != nil {
		return nil, err
	}
	if s.status != 0 {
		response.StatusCode = s.status
	}
	if s.missingLocation {
		response.Header.Del("Location")
	}
	if s.brokenBody {
		response.Body = liveKeyBrokenBody{}
	}
	return response, nil
}

func (s *liveKeyHTTPUpstreamStub) DoWithTLS(req *http.Request, proxy string, account int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return s.Do(req, proxy, account, concurrency)
}

type liveKeyBrokenBody struct{}

func (liveKeyBrokenBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (liveKeyBrokenBody) Close() error             { return nil }

type liveKeyNoObserverStore struct{ *liveTestStore }

func (*liveKeyNoObserverStore) ClaimLiveController(context.Context, string, string, string) (bool, error) {
	return false, nil
}

func TestLiveSharedKeyCreationOutcomeAndNoUnsafeReplay(t *testing.T) {
	for _, test := range []struct {
		name      string
		upstream  liveKeyHTTPUpstreamStub
		uncertain bool
		releases  int
		wantError bool
	}{
		{name: "success retains session ownership"},
		{name: "unreadable transport reply", upstream: liveKeyHTTPUpstreamStub{transportErr: io.ErrUnexpectedEOF}, uncertain: true, wantError: true},
		{name: "server may have created before failing", upstream: liveKeyHTTPUpstreamStub{status: 503}, uncertain: true, wantError: true},
		{name: "explicit credential rejection", upstream: liveKeyHTTPUpstreamStub{status: 401}, releases: 1, wantError: true},
		{name: "local pre-send refusal", upstream: liveKeyHTTPUpstreamStub{transportErr: ErrUpstreamKeyPolicy}, releases: 1, wantError: true},
		{name: "body incomplete", upstream: liveKeyHTTPUpstreamStub{brokenBody: true}, uncertain: true, wantError: true},
		{name: "no call identity", upstream: liveKeyHTTPUpstreamStub{missingLocation: true}, uncertain: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := test.upstream
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: &upstream}
			account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "live-token", "chatgpt_account_id": "acct_test"}}
			created, err := svc.createUpstreamLiveCall(t.Context(), account, liveLoopRequest(), "attestation")
			if test.wantError {
				require.Error(t, err)
				require.Nil(t, created)
			} else {
				require.NoError(t, err)
				require.Equal(t, upstream.lease, created.UpstreamKeyLease)
				require.Same(t, upstream.lease, PersistentUpstreamKeyLeaseFromContext(upstream.request.Context()))
			}
			require.Equal(t, test.uncertain, errors.Is(err, ErrLiveUpstreamUncertain))
			if test.uncertain {
				require.False(t, svc.shouldFailoverLiveCreateError(account, err), "an unconfirmed call must not be duplicated through another account")
			}
			require.Equal(t, test.releases, upstream.releases)
			require.Equal(t, 1, upstream.calls)
		})
	}
}

func TestLiveSharedKeyLeaseLossStopsCreationReplay(t *testing.T) {
	for _, transportErr := range []error{nil, io.ErrUnexpectedEOF} {
		name := "created session"
		if transportErr != nil {
			name = "unknown session outcome"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			upstream := &liveKeyHTTPUpstreamStub{
				transportErr: transportErr,
				onSend:       func() { cancel(ErrAPIKeySlotLeaseLost) },
			}
			svc, cache, store := newLiveLoopFixture(t, &liveLoopUpstreamStub{}, true)
			svc.httpUpstream = upstream
			created, err := svc.CreateLiveCall(ctx, liveLoopRequest(), liveLoopIdentity(), 1)
			require.Nil(t, created)
			require.ErrorIs(t, err, ErrAPIKeySlotLeaseLost)
			if transportErr != nil {
				require.ErrorIs(t, err, ErrLiveUpstreamUncertain, "lease loss must preserve the upstream outcome")
			}
			require.False(t, svc.shouldFailoverLiveCreateError(nil, err))
			require.Equal(t, 1, upstream.calls, "lost joint ownership must never create a second session")
			require.Zero(t, upstream.releases, "a created or unconfirmed media session retains its member")
			require.Equal(t, 1, cache.transferCalls)
			require.Zero(t, cache.migrateCalls)
			require.Equal(t, 1, cache.releaseLeaseCall)
			require.Zero(t, store.saves)
		})
	}
}

func TestLiveSharedKeyHandoffAndPreSendCancellation(t *testing.T) {
	upstream := &liveKeyHTTPUpstreamStub{}
	svc, _, _ := newLiveLoopFixture(t, &liveLoopUpstreamStub{}, true)
	store := &liveKeyNoObserverStore{liveTestStore: &liveTestStore{GatewayCache: &stubGatewayCache{}}}
	svc.httpUpstream = upstream
	svc.cache = store
	created, err := svc.CreateLiveCall(t.Context(), liveLoopRequest(), liveLoopIdentity(), 1)
	require.NoError(t, err)
	loaded, err := store.GetLiveCall(t.Context(), hashLiveCallID(created.CallID))
	require.NoError(t, err)
	require.Equal(t, upstream.lease, loaded.UpstreamKeyLease, "the persistent controller record carries exact ownership")
	require.Zero(t, upstream.releases)

	ctx, cancel := context.WithCancel(t.Context())
	upstream = &liveKeyHTTPUpstreamStub{onAcquire: cancel}
	svc.httpUpstream = upstream
	account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "live-token", "chatgpt_account_id": "acct_test"}}
	_, err = svc.createUpstreamLiveCall(ctx, account, liveLoopRequest(), "attestation")
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, upstream.calls)
	require.Equal(t, 1, upstream.releases)
}

func TestLiveSharedKeyFinalizationRequiresUpstreamClosure(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(map[bool]string{false: "local expiry", true: "upstream closure"}[confirmed], func(t *testing.T) {
			upstream := &liveKeyHTTPUpstreamStub{}
			store := &liveTestStore{}
			fingerprint := config.UpstreamKeyFingerprint("live-finalize-fixture")
			record := &LiveCallRecord{CallID: "call_fixture", CallHash: hashLiveCallID("call_fixture"), LeaseID: "joint-fixture", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), UpstreamKeyLease: &PersistentUpstreamKeyLease{Authority: "redis", Member: "live:fixture", Fingerprints: []string{fingerprint}, Resources: []PersistentUpstreamKeyResource{{Fingerprint: fingerprint}}}}
			require.NoError(t, store.SaveLiveCall(t.Context(), record, time.Hour))
			usage := &liveTestUsageRepo{}
			svc := &OpenAIGatewayService{cache: store, httpUpstream: upstream, concurrencyService: NewConcurrencyService(&liveTestConcurrencyCache{}), usageLogRepo: usage}
			svc.finalizeLiveCall(record, confirmed)
			require.Len(t, usage.logs, 1)
			if confirmed {
				require.Equal(t, 1, upstream.releases)
			} else {
				require.Zero(t, upstream.releases, "local close/expiry cannot prove media termination")
			}
			upstream.releaseErr = io.ErrUnexpectedEOF
			svc.finalizeLiveCall(record, true)
			attempts := upstream.releases
			upstream.releaseErr = nil
			svc.finalizeLiveCall(record, true)
			require.Equal(t, attempts+1, upstream.releases, "retry a lost cleanup acknowledgement even when usage was already finalized")
			require.Len(t, usage.logs, 1, "cleanup retry must not duplicate billing")
		})
	}
}

func TestLiveSharedKeyClosureEvidenceAcrossControllers(t *testing.T) {
	for _, controller := range []string{"observer", "proxy"} {
		for _, event := range []string{"control connection closed", "session.closed", "session.ended"} {
			t.Run(controller+"/"+event, func(t *testing.T) {
				account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
					Credentials: map[string]any{"access_token": "live-token", "chatgpt_account_id": "live-account"}}
				fingerprint := config.UpstreamKeyFingerprint("live-close-fixture")
				record := &LiveCallRecord{CallID: "call_fixture", CallHash: hashLiveCallID("call_fixture"),
					AccountID: account.ID, APIKeyID: 9, UserID: 20, LeaseID: "joint-fixture",
					CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute), Controller: LiveControllerPending,
					UpstreamKeyLease: &PersistentUpstreamKeyLease{Authority: "redis", Member: "live:fixture",
						Fingerprints: []string{fingerprint}, Resources: []PersistentUpstreamKeyResource{{Fingerprint: fingerprint}}}}
				cipher := newLiveAttestationCipher(&config.Config{JWT: config.JWTConfig{Secret: "live-close-fixture"}})
				var err error
				record.AttestationCiphertext, err = cipher.Encrypt("fixture-attestation")
				require.NoError(t, err)
				store := &liveTestStore{GatewayCache: &stubGatewayCache{}}
				require.NoError(t, store.SaveLiveCall(t.Context(), record, time.Minute))
				upstream := newLiveTestFrameConn()
				if event == "control connection closed" {
					upstream.reads <- liveTestFrame{err: coderws.CloseError{Code: coderws.StatusNormalClosure}}
				} else {
					upstream.reads <- liveTestFrame{messageType: coderws.MessageText, payload: []byte(`{"type":"` + event + `"}`)}
				}
				keys, usage := &liveKeyHTTPUpstreamStub{}, &liveTestUsageRepo{}
				svc := &OpenAIGatewayService{accountRepo: &liveTestAccountRepo{account: account}, cache: store,
					httpUpstream: keys, concurrencyService: NewConcurrencyService(&liveTestConcurrencyCache{}),
					usageLogRepo: usage, liveAttestationCipher: cipher, openaiWSPassthroughDialer: &liveTestDialer{conn: upstream}}
				if controller == "observer" {
					svc.observeLiveCall(record)
				} else {
					result := make(chan error, 1)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						client, acceptErr := coderws.Accept(w, r, nil)
						if acceptErr != nil {
							result <- acceptErr
							return
						}
						defer func() { _ = client.CloseNow() }()
						result <- svc.ProxyLiveSideband(r.Context(), record, client)
					}))
					t.Cleanup(server.Close)
					ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
					defer cancel()
					client, _, dialErr := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
					require.NoError(t, dialErr)
					defer func() { _ = client.CloseNow() }()
					select {
					case runErr := <-result:
						require.ErrorIs(t, runErr, ErrLiveCallNotFound)
					case <-ctx.Done():
						t.Fatal("sideband did not complete after upstream closure")
					}
				}
				if event == "control connection closed" {
					require.Zero(t, keys.releases, "control closure alone must retain media capacity")
				} else {
					require.Equal(t, 1, keys.releases, "an explicit upstream terminal event releases media capacity")
				}
				require.Len(t, usage.logs, 1, "controller finalization still writes usage exactly once")
			})
		}
	}
}
