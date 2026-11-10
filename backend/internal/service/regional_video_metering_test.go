package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/regional"
	"github.com/Wei-Shaw/sub2api/internal/regional/ledger"
	"github.com/Wei-Shaw/sub2api/internal/regional/video"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type videoOwnerTestLedger struct {
	meterTestLedger
	owner     *video.Owner
	err       error
	calls     int
	principal video.Owner
	taskID    string
	load      func(context.Context, video.Owner, string) (ledger.VideoTaskRecord, error)
	complete  func(context.Context, ledger.Event) error
	bind      func(context.Context, string, string) error
	reject    func(context.Context, string) error
}

func (l *videoOwnerTestLedger) Complete(ctx context.Context, event ledger.Event) error {
	if l.complete != nil {
		return l.complete(ctx, event)
	}
	return l.meterTestLedger.Complete(ctx, event)
}

func (l *videoOwnerTestLedger) BindVideoTask(ctx context.Context, id, taskID string) error {
	if l.bind != nil {
		return l.bind(ctx, id, taskID)
	}
	panic("owner resolution must not bind or start tasks")
}

func (l *videoOwnerTestLedger) RejectVideoTask(ctx context.Context, id string) error {
	if l.reject != nil {
		return l.reject(ctx, id)
	}
	panic("owner resolution must not reject or refund tasks")
}

func (l *videoOwnerTestLedger) LoadVideoTask(ctx context.Context, owner video.Owner, taskID string) (ledger.VideoTaskRecord, error) {
	if l.load == nil {
		panic("account selection must resolve the original owner before loading its exact route")
	}
	return l.load(ctx, owner, taskID)
}

func (l *videoOwnerTestLedger) ResolveVideoTaskOwner(_ context.Context, principal video.Owner, taskID string) (*video.Owner, error) {
	l.calls++
	l.principal, l.taskID = principal, taskID
	if principal.AccountID != 0 || principal.RouteFingerprint != [32]byte{} {
		return nil, ledger.ErrConflict
	}
	return l.owner, l.err
}

func TestRegionalVideoAccountResolutionRejectsMissingOrConflictingAuthority(t *testing.T) {
	group := int64(3)
	owner := video.Owner{UserID: 1, APIKeyID: 2, GroupID: group, AccountID: 17, Provider: "grok", RouteFingerprint: [32]byte{1}}
	for _, s := range []*OpenAIGatewayService{
		{cfg: &config.Config{Runtime: config.RuntimeConfig{RegionalFundsFile: "fixture"}}},
		{regionalMetering: &RegionalMetering{}},
		{regionalMetering: &RegionalMetering{Ledger: &meterTestLedger{}}},
		{regionalMetering: &RegionalMetering{Ledger: &videoOwnerTestLedger{}}},
	} {
		account, err := s.ResolveMediaVideoRequestAccount(t.Context(), &group, "task", owner.UserID, owner.APIKeyID, GrokMediaEndpointVideoStatus)
		require.ErrorIs(t, err, ErrUpstreamKeyPolicy)
		require.Zero(t, account)
	}
	for _, mutate := range []func(*video.Owner){
		func(o *video.Owner) { o.UserID++ },
		func(o *video.Owner) { o.APIKeyID++ },
		func(o *video.Owner) { o.GroupID++ },
		func(o *video.Owner) { o.Provider = "seedance" },
		func(o *video.Owner) { o.AccountID = 0 },
		func(o *video.Owner) { o.RouteFingerprint = [32]byte{} },
	} {
		wrong := owner
		mutate(&wrong)
		s := &OpenAIGatewayService{regionalMetering: &RegionalMetering{Ledger: &videoOwnerTestLedger{owner: &wrong}}}
		account, err := s.ResolveMediaVideoRequestAccount(t.Context(), &group, "task", owner.UserID, owner.APIKeyID, GrokMediaEndpointVideoStatus)
		require.ErrorIs(t, err, ErrUpstreamKeyPolicy)
		require.Zero(t, account)
	}
	l := &videoOwnerTestLedger{owner: &owner}
	s := &OpenAIGatewayService{regionalMetering: &RegionalMetering{Ledger: l}}
	query := video.Query{TaskID: "task", Owner: owner}
	query.Owner.RouteFingerprint[0]++
	ctx := context.WithValue(t.Context(), asyncVideoForwardProofKey{}, query)
	account, err := s.ResolveMediaVideoRequestAccount(ctx, &group, query.TaskID, owner.UserID, owner.APIKeyID, GrokMediaEndpointVideoStatus)
	require.ErrorIs(t, err, video.ErrConflict)
	require.Zero(t, account)
	query.Owner = owner
	ctx = context.WithValue(t.Context(), asyncVideoForwardProofKey{}, query)
	account, err = s.ResolveMediaVideoRequestAccount(ctx, &group, query.TaskID, owner.UserID, owner.APIKeyID, GrokMediaEndpointVideoStatus)
	require.NoError(t, err)
	require.Equal(t, owner.AccountID, account)
}

func TestRegionalVideoAccountResolutionUsesPermanentFinancialOwner(t *testing.T) {
	for _, endpoint := range []GrokMediaEndpoint{MiniMaxH3EndpointStatus, MiniMaxH3EndpointContent, GrokMediaEndpointVideoStatus, GrokMediaEndpointVideoContent, SeedanceEndpointStatus, SeedanceEndpointDelete} {
		t.Run(string(endpoint), func(t *testing.T) {
			group := int64(3)
			owner := video.Owner{UserID: 1, APIKeyID: 2, GroupID: group, AccountID: 17, Provider: endpoint.AsyncVideoProvider(), RouteFingerprint: [32]byte{1}}
			l := &videoOwnerTestLedger{owner: &owner}
			s := &OpenAIGatewayService{regionalMetering: &RegionalMetering{Ledger: l}}
			_, err := s.ResolveMediaVideoRequestAccount(t.Context(), &group, "permanent-task", owner.UserID, owner.APIKeyID, GrokMediaEndpointVideosGenerations)
			require.ErrorIs(t, err, ErrUpstreamKeyPolicy)
			require.Zero(t, l.calls, "generation requests must not enter the lookup resolver")
			account, err := s.ResolveMediaVideoRequestAccount(t.Context(), &group, "permanent-task", owner.UserID, owner.APIKeyID, endpoint)
			require.NoError(t, err, "SQL financial owner must survive expired slot records and absent sticky cache")
			require.Equal(t, owner.AccountID, account)
			require.Equal(t, 1, l.calls)
			require.Equal(t, video.Owner{UserID: owner.UserID, APIKeyID: owner.APIKeyID, GroupID: group, Provider: endpoint.AsyncVideoProvider()}, l.principal)
			require.Equal(t, "permanent-task", l.taskID)
			for _, lookupErr := range []error{sql.ErrNoRows, errors.New("original ledger unavailable")} {
				l.err = lookupErr
				account, err = s.ResolveMediaVideoRequestAccount(t.Context(), &group, "permanent-task", owner.UserID, owner.APIKeyID, endpoint)
				require.ErrorIs(t, err, ErrUpstreamKeyPolicy)
				require.ErrorIs(t, err, lookupErr)
				require.Zero(t, account)
			}
		})
	}
}

type videoLookupTestUpstream struct {
	HTTPUpstream
	AsyncVideoKeyLeaseController
	lookups int
}

func (s *videoLookupTestUpstream) LoadAsyncVideoKeyLeaseByTask(context.Context, AsyncVideoKeyOwner, string) (*PersistentUpstreamKeyLease, error) {
	s.lookups++
	return nil, nil
}

func TestRegionalVideoLookupRechecksPermanentRoute(t *testing.T) {
	for _, endpoint := range []GrokMediaEndpoint{MiniMaxH3EndpointStatus, MiniMaxH3EndpointContent, GrokMediaEndpointVideoStatus, GrokMediaEndpointVideoContent, SeedanceEndpointStatus, SeedanceEndpointDelete} {
		for _, scenario := range []string{"matching", "missing_sql", "changed_key", "changed_address", "changed_proxy", "changed_route_setting", "conflicting_forward", "missing_owner", "canceled"} {
			t.Run(string(endpoint)+"/"+scenario, func(t *testing.T) {
				account := &Account{ID: 17, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture-key", "base_url": "https://original.example"}}
				owner := video.Owner{UserID: 1, APIKeyID: 2, GroupID: 3, AccountID: account.ID, Provider: endpoint.AsyncVideoProvider(), RouteFingerprint: AccountRouteFingerprint(account)}
				original := owner
				loads := 0
				l := &videoOwnerTestLedger{load: func(ctx context.Context, requested video.Owner, taskID string) (ledger.VideoTaskRecord, error) {
					loads++
					require.Equal(t, "permanent-task", taskID)
					if err := ctx.Err(); err != nil {
						return ledger.VideoTaskRecord{}, err
					}
					if scenario == "missing_sql" {
						return ledger.VideoTaskRecord{}, sql.ErrNoRows
					}
					if requested != original {
						return ledger.VideoTaskRecord{}, ledger.ErrConflict
					}
					return ledger.VideoTaskRecord{TaskID: taskID, State: "started", Reservation: ledger.Reservation{VideoOrigin: &ledger.VideoOrigin{Owner: original}}}, nil
				}}
				upstream := &videoLookupTestUpstream{}
				s := &OpenAIGatewayService{httpUpstream: upstream, regionalMetering: &RegionalMetering{Ledger: l, Readiness: func(context.Context) error {
					panic("existing task lookup must not acquire new funds or require new admission")
				}}}
				ctx := t.Context()
				switch scenario {
				case "changed_key":
					account.Credentials["api_key"] = "replacement-key"
				case "changed_address":
					account.Credentials["base_url"] = "https://replacement.example"
				case "changed_proxy":
					proxyID := int64(29)
					account.ProxyID = &proxyID
				case "changed_route_setting":
					account.Extra = map[string]any{"openai_responses_mode": "replacement-mode"}
				case "conflicting_forward":
					proof := video.Query{TaskID: "permanent-task", Owner: owner}
					proof.Owner.RouteFingerprint[0] ^= 1
					ctx = context.WithValue(ctx, asyncVideoForwardProofKey{}, proof)
				case "canceled":
					canceled, cancel := context.WithCancel(ctx)
					cancel()
					ctx = canceled
				}
				// The handler constructs this context from the selected account's
				// current fields. Comparing it only to that account misses edits
				// made after the provider task was originally submitted.
				owner.RouteFingerprint = AccountRouteFingerprint(account)
				if scenario != "missing_owner" {
					ctx = WithAsyncVideoKeyOwner(ctx, owner)
				}
				method := http.MethodGet
				if endpoint == SeedanceEndpointDelete {
					method = http.MethodDelete
				}
				req := httptest.NewRequestWithContext(ctx, method, "https://original.example/task", nil)
				prepared, err := s.prepareAsyncVideoKeyLookup(req, account, endpoint, "permanent-task")
				if scenario == "matching" {
					require.NoError(t, err)
					require.Equal(t, 1, loads)
					require.Equal(t, 1, upstream.lookups)
					taskID, bound := AsyncVideoKeyLookupTaskID(prepared)
					require.True(t, bound)
					require.Equal(t, "permanent-task", taskID)
					require.True(t, HTTPUpstreamRedirectsDisabled(prepared.Context()), "permanent financial ownership must bind even a completed task query to one physical target")
				} else {
					require.ErrorIs(t, err, ErrUpstreamKeyPolicy, "a current account or expired slot must not replace the permanent route proof")
					require.Nil(t, prepared)
					require.Zero(t, upstream.lookups, "SQL or principal rejection must precede slot lookup and physical send")
				}
				require.Zero(t, l.calls, "preparation must verify the exact route, never select another owner")
				require.Zero(t, l.reservations)
				require.Zero(t, l.starts)
			})
		}
	}
}

func TestRegionalVideoLookupRejectsMissingFinancialAuthority(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "https://original.example/task", nil)
	for _, s := range []*OpenAIGatewayService{
		{cfg: &config.Config{Runtime: config.RuntimeConfig{RegionalFundsFile: "fixture"}}, httpUpstream: &videoLookupTestUpstream{}},
		{regionalMetering: &RegionalMetering{}, httpUpstream: &videoLookupTestUpstream{}},
		{regionalMetering: &RegionalMetering{Ledger: &meterTestLedger{}}, httpUpstream: &videoLookupTestUpstream{}},
		{regionalMetering: &RegionalMetering{Ledger: &videoOwnerTestLedger{}}},
	} {
		prepared, err := s.prepareAsyncVideoKeyLookup(req, nil, GrokMediaEndpointVideoStatus, "permanent-task")
		require.ErrorIs(t, err, ErrUpstreamKeyPolicy)
		require.Nil(t, prepared)
	}
}

type regionalVideoResponseTestUpstream struct {
	videoLookupTestUpstream
	body    string
	status  int
	headers http.Header
}

func (u *regionalVideoResponseTestUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	status := u.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{StatusCode: status, Header: u.headers.Clone(), Body: io.NopCloser(strings.NewReader(u.body)), Request: req}, nil
}

func regionalVideoMeterFixture(t *testing.T, provider string) (*Account, ledger.VideoTaskRecord) {
	t.Helper()
	account := &Account{ID: 17, Platform: PlatformMiniMax, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture-key", "base_url": "https://video.example", "account_mode": AccountModePayG, "openai_capabilities": []string{"seedance"}}}
	price := ledger.Price{Model: "video-alias", Video: &ledger.VideoPrice{VideoRequest: ledger.VideoRequest{Provider: provider, UpstreamModel: "MiniMax-H3", Resolution: "768p", DurationSeconds: 5}, Basis: "request", UnitNano: 20_000_000, SaleNumerator: 1, SaleDenominator: 10}}
	switch provider {
	case "grok":
		account.Platform = PlatformGrok
		price.Video.VideoRequest = ledger.VideoRequest{Provider: provider, UpstreamModel: "grok-imagine-video", Resolution: "720p", DurationSeconds: 6}
		price.Video.Basis = "second"
	case "seedance":
		account.Platform = PlatformOpenAI
		price.Video.VideoRequest = ledger.VideoRequest{Provider: provider, UpstreamModel: "ep-seedance"}
		price.Video.Basis, price.Video.UnitNano, price.Video.MaxCompletionTokens = "completion_tokens", 28_000_000_000, 1_000_000
	}
	maximum, err := price.VideoMaximum(price.Video.VideoRequest)
	require.NoError(t, err)
	owner := video.Owner{UserID: 1, APIKeyID: 2, GroupID: 3, AccountID: account.ID, Provider: provider, RouteFingerprint: AccountRouteFingerprint(account)}
	r := ledger.Reservation{RequestID: "original-financial-request", UserID: 1, APIKeyID: 2, AccountID: account.ID, Model: price.Model, PricingVersion: price.Version(), AdmittedAt: time.Now().Add(-time.Minute), Maximum: maximum, PayloadSHA256: strings.Repeat("a", 64), Video: &price.Video.VideoRequest, VideoOrigin: &ledger.VideoOrigin{Member: "video:00000000-0000-4000-8000-000000000001", Owner: owner}}
	taskID := provider + ":task"
	if provider == "grok" {
		taskID = "task"
	}
	return account, ledger.VideoTaskRecord{Reservation: r, Price: price, TaskID: taskID, State: "started"}
}

func TestRegionalVideoCreateRequiresFinancialAuthority(t *testing.T) {
	for _, provider := range []string{"grok", "seedance", "minimax-h3"} {
		t.Run(provider, func(t *testing.T) {
			account, record := regionalVideoMeterFixture(t, provider)
			upstream := &httpUpstreamRecorder{responses: []*http.Response{newOpenAIRejectedFieldTestResponse(200, `{"id":"created","request_id":"created"}`)}}
			cfg := &config.Config{Runtime: config.RuntimeConfig{RegionalFundsFile: "fixture"}}
			cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			s := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
			ctx := WithAsyncVideoKeyOwner(t.Context(), record.Reservation.VideoOrigin.Owner)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequestWithContext(ctx, http.MethodPost, "/fixture", nil)
			c.Set("api_key", &APIKey{ID: 2, UserID: 1})
			var err error
			switch provider {
			case "grok":
				_, err = s.ForwardGrokMedia(ctx, c, account, GrokMediaEndpointVideosGenerations, "", []byte(`{"model":"grok-imagine-video","prompt":"waves"}`), "application/json")
			case "seedance":
				_, err = s.ForwardSeedance(ctx, c, account, SeedanceEndpointCreate, "", []byte(`{"model":"video-alias","content":[{"type":"text","text":"waves"}]}`))
			default:
				_, err = s.ForwardMiniMaxH3(ctx, c, account, MiniMaxH3EndpointCreate, "", []byte(`{"model":"MiniMax-H3","prompt":"waves","seconds":"5"}`))
			}
			require.Error(t, err, "financial mode must not submit an unreserved video")
			require.Empty(t, upstream.bodies)
		})
	}
}

type videoCreateTestLedger struct {
	videoOwnerTestLedger
	reservation ledger.Reservation
	reserveErr  error
}

func (l *videoCreateTestLedger) Reserve(_ context.Context, r ledger.Reservation) (bool, error) {
	l.reservations++
	l.reservation = r
	return l.reserveErr == nil, l.reserveErr
}

type videoCreateTestUpstream struct {
	HTTPUpstream
	send func(*http.Request) (*http.Response, error)
}

func (u *videoCreateTestUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.send(req)
}

func TestRegionalVideoCreateReservationAndBinding(t *testing.T) {
	for _, endpoint := range []GrokMediaEndpoint{GrokMediaEndpointVideosGenerations, GrokMediaEndpointVideosEdits, GrokMediaEndpointVideosExtensions, SeedanceEndpointCreate, MiniMaxH3EndpointCreate} {
		for _, scenario := range []string{"accepted", "bind_ack_lost", "bind_failed", "start_ack_lost", "start_rejected", "reserve_failed", "rejected", "server_error", "redirect", "missing_task", "invalid_price", "wrong_owner", "ambiguous_fields", "local_capacity", "canceled"} {
			t.Run(string(endpoint)+"/"+scenario, func(t *testing.T) {
				account, record := regionalVideoMeterFixture(t, endpoint.AsyncVideoProvider())
				price := record.Price
				account.Credentials["model_mapping"] = map[string]any{price.Model: price.Video.UpstreamModel}
				record.Reservation.VideoOrigin.Owner.RouteFingerprint = AccountRouteFingerprint(account)
				body := []byte(`{"model":"video-alias","prompt":"waves","duration":6,"resolution":"720p"}`)
				if endpoint.IsMiniMaxH3() {
					body = []byte(`{"model":"video-alias","prompt":"waves","seconds":"5"}`)
				} else if endpoint.IsSeedance() {
					body = []byte(`{"model":"video-alias","content":[{"type":"text","text":"waves"}]}`)
				}
				if scenario == "ambiguous_fields" {
					body = []byte(`{"model":"video-alias","Model":"other","prompt":"waves","seconds":"5","content":[{"type":"text","text":"waves"}]}`)
				}
				l := &videoCreateTestLedger{}
				var binds, rejects, sends int
				l.bind = func(ctx context.Context, id, taskID string) error {
					binds++
					require.NoError(t, ctx.Err())
					require.Equal(t, record.Reservation.RequestID, id)
					require.Equal(t, record.TaskID, taskID)
					require.Equal(t, 1, l.starts)
					if scenario == "bind_failed" || scenario == "bind_ack_lost" && binds == 1 {
						return io.ErrUnexpectedEOF
					}
					return nil
				}
				l.reject = func(ctx context.Context, id string) error {
					rejects++
					require.NoError(t, ctx.Err())
					require.Equal(t, record.Reservation.RequestID, id)
					return nil
				}
				if scenario == "start_ack_lost" {
					l.startErr = io.ErrUnexpectedEOF
				}
				if scenario == "start_rejected" {
					l.startErr = errors.Join(ledger.ErrStartRejected, ledger.ErrConflict)
				}
				if scenario == "reserve_failed" {
					l.reserveErr = ledger.ErrExhausted
				}
				if scenario == "invalid_price" {
					price.Video.UpstreamModel = "different-model"
				}
				ctx := WithAsyncVideoKeyOwner(t.Context(), record.Reservation.VideoOrigin.Owner)
				if scenario == "canceled" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				}
				upstream := &videoCreateTestUpstream{send: func(req *http.Request) (*http.Response, error) {
					sends++
					require.Equal(t, 1, l.starts, "Start must precede every provider byte")
					require.Equal(t, l.reservation.VideoOrigin.Member, AsyncVideoKeyHandoffMember(req))
					payload, err := io.ReadAll(req.Body)
					require.NoError(t, err)
					digest := sha256.Sum256(payload)
					require.Equal(t, hex.EncodeToString(digest[:]), l.reservation.PayloadSHA256)
					if scenario == "local_capacity" {
						return nil, ErrUpstreamKeyCapacity
					}
					status, response := http.StatusOK, `{"id":"task","request_id":"task","status":"queued"}`
					switch scenario {
					case "rejected":
						status, response = http.StatusTooManyRequests, `{"error":"rate limited"}`
					case "server_error":
						status, response = http.StatusInternalServerError, `{"error":"unknown"}`
					case "redirect":
						status, response = http.StatusTemporaryRedirect, ""
					case "missing_task":
						response = `{"status":"queued"}`
					}
					return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
				}}
				meter := &RegionalMetering{Ledger: l, Readiness: func(ctx context.Context) error { return ctx.Err() }, Admit: func(_ context.Context, _ *APIKey, _ *Account, payload []byte) (ledger.Reservation, ledger.Price, error) {
					require.Equal(t, body, payload, "pricing resolves the client's billing model before upstream mapping")
					r := record.Reservation
					return r, price, nil
				}}
				s := &OpenAIGatewayService{regionalMetering: meter, httpUpstream: upstream}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequestWithContext(ctx, http.MethodPost, "/fixture", nil)
				groupID := int64(3)
				key := &APIKey{ID: 2, UserID: 1, GroupID: &groupID}
				if scenario == "wrong_owner" {
					key.UserID++
				}
				c.Set("api_key", key)
				var result *OpenAIForwardResult
				var err error
				switch {
				case endpoint.IsMiniMaxH3():
					result, err = s.ForwardMiniMaxH3(ctx, c, account, endpoint, "", body)
				case endpoint.IsSeedance():
					result, err = s.ForwardSeedance(ctx, c, account, endpoint, "", body)
				default:
					result, err = s.ForwardGrokMedia(ctx, c, account, endpoint, "", body, "application/json")
				}
				switch scenario {
				case "accepted", "bind_ack_lost":
					require.NoError(t, err)
					require.True(t, result.RegionalMetered)
					require.Equal(t, record.TaskID, result.ResponseID)
					require.Equal(t, 1, sends)
					require.Zero(t, rejects)
					require.Zero(t, l.canceled)
					if scenario == "bind_ack_lost" {
						require.Equal(t, 2, binds)
					}
				case "invalid_price", "wrong_owner", "ambiguous_fields", "reserve_failed", "canceled", "start_ack_lost", "start_rejected":
					require.Error(t, err)
					if scenario == "start_ack_lost" || scenario == "start_rejected" {
						require.ErrorIs(t, err, ErrRegionalVideoSettlement, "local SQL failures must not become provider health failures")
						require.Equal(t, 1, l.starts)
						if scenario == "start_rejected" {
							require.ErrorIs(t, err, ledger.ErrStartRejected)
							require.Equal(t, 1, l.canceled, "a proven pre-transition rejection must refund the unsent reservation")
						} else {
							require.Zero(t, l.canceled, "a lost Start acknowledgement must retain funds even with zero provider sends")
						}
					}
					require.Zero(t, sends)
					require.Zero(t, rejects)
				case "rejected", "local_capacity":
					require.Error(t, err)
					require.False(t, IsAsyncVideoSubmissionUnconfirmed(err))
					require.Equal(t, 1, rejects)
				default:
					require.Error(t, err)
					require.True(t, IsAsyncVideoSubmissionUnconfirmed(err), "%v", err)
					require.Equal(t, 1, sends)
					require.Zero(t, rejects)
					require.Zero(t, l.canceled, "ambiguous submission must retain its original funds")
				}
				require.Zero(t, l.completed, "create never settles or fabricates a terminal event")
			})
		}
	}
}

func queryRegionalVideoMeterFixture(t *testing.T, s *OpenAIGatewayService, account *Account, record ledger.VideoTaskRecord, endpoint GrokMediaEndpoint) (*OpenAIForwardResult, *httptest.ResponseRecorder, error) {
	t.Helper()
	ctx := WithAsyncVideoKeyOwner(t.Context(), record.Reservation.VideoOrigin.Owner)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequestWithContext(ctx, http.MethodGet, "/video/task", nil)
	var result *OpenAIForwardResult
	var err error
	switch record.Reservation.Video.Provider {
	case "grok":
		result, err = s.ForwardGrokMedia(ctx, c, account, endpoint, record.TaskID, nil, "")
	case "seedance":
		result, err = s.ForwardSeedance(ctx, c, account, endpoint, record.TaskID, nil)
	default:
		result, err = s.ForwardMiniMaxH3(ctx, c, account, endpoint, record.TaskID, nil)
	}
	if err == nil {
		c.Writer.WriteHeaderNow()
	}
	return result, w, err
}

func TestRegionalVideoProviderTerminalEvidenceAndRecovery(t *testing.T) {
	for _, tc := range []struct {
		provider string
		endpoint GrokMediaEndpoint
		body     string
		charge   regional.Amount
	}{
		{"grok", GrokMediaEndpointVideoStatus, `{"request_id":"task","status":"done","model":"grok-imagine-video","video":{"url":"https://cdn.example/video.mp4","duration":99}}`, 12_000_000},
		{"seedance", SeedanceEndpointStatus, `{"id":"task","status":"succeeded","model":"ep-seedance","usage":{"completion_tokens":12345}}`, 34_566_000},
		{"minimax-h3", MiniMaxH3EndpointStatus, `{"id":"task","status":"completed","metadata":{"url":"https://cdn.example/video.mp4"}}`, 2_000_000},
	} {
		for _, scenario := range []string{"success", "settlement_unavailable", "lost_ack", "stored_event", "price_changed"} {
			t.Run(tc.provider+"/"+scenario, func(t *testing.T) {
				account, record := regionalVideoMeterFixture(t, tc.provider)
				l := &videoOwnerTestLedger{load: func(context.Context, video.Owner, string) (ledger.VideoTaskRecord, error) { return record, nil }}
				calls := 0
				l.complete = func(ctx context.Context, event ledger.Event) error {
					require.NoError(t, ctx.Err())
					calls++
					if scenario == "settlement_unavailable" && calls == 1 {
						return errors.New("original funds temporarily unavailable")
					}
					record.State, record.Event = "completed", &event
					if scenario == "lost_ack" && calls == 1 {
						return errors.New("committed but acknowledgement lost")
					}
					return l.meterTestLedger.Complete(ctx, event)
				}
				upstream := &regionalVideoResponseTestUpstream{body: tc.body}
				s := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, regionalMetering: &RegionalMetering{Ledger: l}}
				if scenario == "price_changed" {
					changed := *record.Price.Video
					changed.SaleNumerator = 9
					record.Price.Video = &changed
				}
				result, w, err := queryRegionalVideoMeterFixture(t, s, account, record, tc.endpoint)
				if scenario == "settlement_unavailable" || scenario == "price_changed" {
					require.Error(t, err)
					require.ErrorIs(t, err, ErrRegionalVideoSettlement)
					require.Nil(t, result)
					require.Empty(t, w.Body.String(), "failed financial completion must not publish upstream success")
					require.Zero(t, l.completed)
					if scenario == "price_changed" {
						return
					}
					result, _, err = queryRegionalVideoMeterFixture(t, s, account, record, tc.endpoint)
				}
				require.NoError(t, err)
				require.True(t, result.RegionalMetered)
				require.True(t, result.AsyncVideoTerminal)
				require.Equal(t, tc.charge, l.event.Usage.Charge)
				require.Equal(t, record.Reservation.RequestID, l.event.Usage.EventID)
				if scenario == "stored_event" {
					previous := l.event
					upstream.body = `{"status":"pending"}`
					result, _, err = queryRegionalVideoMeterFixture(t, s, account, record, tc.endpoint)
					require.NoError(t, err)
					require.True(t, result.RegionalMetered)
					require.Equal(t, previous, l.event, "recovery replays exact persisted event, including completion time")
				}
				require.Zero(t, l.reservations)
				require.Zero(t, l.starts)
				require.Zero(t, l.canceled)
			})
		}
	}
}

func TestRegionalVideoAmbiguousBillingRetainsHold(t *testing.T) {
	for _, body := range []string{
		`{"status":"succeeded","usage":{"completion_tokens":-1}}`,
		`{"status":"succeeded","usage":{"completion_tokens":null}}`,
		`{"status":"succeeded","usage":{"completion_tokens":"12345"}}`,
		`{"status":"succeeded","usage":{"completion_tokens":1.5}}`,
		`{"status":"succeeded","usage":{"completion_tokens":1e4}}`,
		`{"status":"succeeded","usage":{"completion_tokens":1000001}}`,
		`{"status":"succeeded","usage":{"completion_tokens":12345,"completion_tokens":0}}`,
		`{"status":"succeeded","usage":{"completion_tokens":12345,"COMPLETION_TOKENS":0}}`,
		`{"status":"succeeded","usage":{"completion_tokens":12345},"usage":{"completion_tokens":0}}`,
		`{"status":"succeeded","usage":null}`,
		`{"status":"succeeded"}`,
		`{"status":"succeeded","model":"replacement","usage":{"completion_tokens":12345}}`,
		`{"status":"succeeded","model":"ep-seedance","model":"replacement","usage":{"completion_tokens":12345}}`,
		`{"status":"failed","usage":{"completion_tokens":12345}}`,
	} {
		t.Run(body, func(t *testing.T) {
			account, record := regionalVideoMeterFixture(t, "seedance")
			l := &videoOwnerTestLedger{load: func(context.Context, video.Owner, string) (ledger.VideoTaskRecord, error) { return record, nil }}
			u := &regionalVideoResponseTestUpstream{body: body}
			s := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: u, regionalMetering: &RegionalMetering{Ledger: l}}
			result, w, err := queryRegionalVideoMeterFixture(t, s, account, record, SeedanceEndpointStatus)
			require.Error(t, err)
			require.Nil(t, result)
			require.Empty(t, w.Body.String())
			require.Zero(t, l.completed)
			u.body = `{"status":"succeeded","usage":{"completion_tokens":12345}}`
			result, _, err = queryRegionalVideoMeterFixture(t, s, account, record, SeedanceEndpointStatus)
			require.NoError(t, err)
			require.True(t, result.RegionalMetered)
			require.Equal(t, 1, l.completed)
		})
	}
}

func TestRegionalVideoContentAndEmptyDelete(t *testing.T) {
	for _, endpoint := range []GrokMediaEndpoint{MiniMaxH3EndpointContent, SeedanceEndpointDelete} {
		t.Run(string(endpoint), func(t *testing.T) {
			account, record := regionalVideoMeterFixture(t, endpoint.AsyncVideoProvider())
			l := &videoOwnerTestLedger{load: func(context.Context, video.Owner, string) (ledger.VideoTaskRecord, error) { return record, nil }}
			u := &regionalVideoResponseTestUpstream{status: http.StatusNoContent}
			if endpoint == MiniMaxH3EndpointContent {
				u.status, u.headers = http.StatusFound, http.Header{"Location": {"https://cdn.example/task.mp4"}}
			}
			s := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: u, regionalMetering: &RegionalMetering{Ledger: l}}
			result, w, err := queryRegionalVideoMeterFixture(t, s, account, record, endpoint)
			require.NoError(t, err)
			require.True(t, result.RegionalMetered)
			require.Equal(t, u.status, w.Code)
			require.Equal(t, endpoint == MiniMaxH3EndpointContent, result.AsyncVideoTerminal)
			if endpoint == MiniMaxH3EndpointContent {
				require.Equal(t, 1, l.completed)
				require.EqualValues(t, 2_000_000, l.event.Usage.Charge)
			} else {
				require.Zero(t, l.completed, "204 cancellation acknowledgement does not prove terminal state")
			}
		})
	}
}

func TestRegionalVideoAmbiguousOutputRetainsHold(t *testing.T) {
	for _, tc := range []struct {
		provider string
		endpoint GrokMediaEndpoint
		bodies   []string
	}{
		{"grok", GrokMediaEndpointVideoStatus, []string{
			`{"status":"done","video":{"url":"https://cdn.example/video.mp4","url":null}}`,
			`{"status":"done","video":{"url":"https://cdn.example/video.mp4"},"VIDEO":null}`,
			`{"status":"done","video":{"url":"https://cdn.example/video.mp4","URL":"https://other.example/video.mp4"}}`,
			`{"status":"done","video":{}}`,
		}},
		{"minimax-h3", MiniMaxH3EndpointStatus, []string{
			`{"status":"completed","metadata":{"url":"https://cdn.example/video.mp4","url":null}}`,
			`{"status":"completed","metadata":{"url":"https://cdn.example/video.mp4"},"metadata":null}`,
			`{"status":"completed","metadata":{"url":"https://cdn.example/video.mp4","URL":"https://other.example/video.mp4"}}`,
			`{"status":"completed","metadata":{}}`,
		}},
	} {
		for _, body := range tc.bodies {
			t.Run(tc.provider+"/"+body, func(t *testing.T) {
				account, record := regionalVideoMeterFixture(t, tc.provider)
				l := &videoOwnerTestLedger{load: func(context.Context, video.Owner, string) (ledger.VideoTaskRecord, error) { return record, nil }}
				s := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: &regionalVideoResponseTestUpstream{body: body}, regionalMetering: &RegionalMetering{Ledger: l}}
				result, w, err := queryRegionalVideoMeterFixture(t, s, account, record, tc.endpoint)
				require.Error(t, err)
				require.Nil(t, result)
				require.Empty(t, w.Body.String())
				require.Zero(t, l.completed)
			})
		}
	}
}

func TestRegionalVideoTerminalUsesOriginalFundsLedger(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		state  string
		charge regional.Amount
	}{
		{"completed", `{"id":"task","status":"completed","metadata":{"url":"https://cdn.example/task.mp4"}}`, "completed", 2_000_000},
		{"failed", `{"id":"task","status":"failed"}`, "failed", 0},
		{"pending", `{"id":"task","status":"pending"}`, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{ID: 17, Platform: PlatformMiniMax, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture-key", "base_url": "https://video.example", "account_mode": AccountModePayG}}
			owner := video.Owner{UserID: 1, APIKeyID: 2, GroupID: 3, AccountID: account.ID, Provider: "minimax-h3", RouteFingerprint: AccountRouteFingerprint(account)}
			price := ledger.Price{Model: "video-alias", Video: &ledger.VideoPrice{VideoRequest: ledger.VideoRequest{Provider: "minimax-h3", UpstreamModel: "MiniMax-H3", Resolution: "768p", DurationSeconds: 5}, Basis: "request", UnitNano: 20_000_000, SaleNumerator: 1, SaleDenominator: 10}}
			r := ledger.Reservation{RequestID: "original-financial-request", UserID: 1, APIKeyID: 2, AccountID: account.ID, Model: price.Model, PricingVersion: price.Version(), AdmittedAt: time.Now().Add(-time.Minute), Maximum: 2_000_000, PayloadSHA256: strings.Repeat("a", 64), Video: &price.Video.VideoRequest, VideoOrigin: &ledger.VideoOrigin{Member: "video:00000000-0000-4000-8000-000000000001", Owner: owner}}
			l := &videoOwnerTestLedger{load: func(context.Context, video.Owner, string) (ledger.VideoTaskRecord, error) {
				return ledger.VideoTaskRecord{Reservation: r, Price: price, TaskID: "minimax-h3:task", State: "started"}, nil
			}}
			upstream := &regionalVideoResponseTestUpstream{body: tc.body}
			s := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, regionalMetering: &RegionalMetering{Ledger: l}}
			ctx := WithAsyncVideoKeyOwner(t.Context(), owner)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/videos/task", nil)
			result, err := s.ForwardMiniMaxH3(ctx, c, account, MiniMaxH3EndpointStatus, "minimax-h3:task", nil)
			require.NoError(t, err)
			require.True(t, result.RegionalMetered, "financial video queries must never enter legacy asynchronous billing")
			if tc.state != "" {
				require.Equal(t, 1, l.completed, "an actual terminal observation must settle the original reservation before responding")
				require.Equal(t, r.RequestID, l.event.Usage.EventID)
				require.Equal(t, tc.charge, l.event.Usage.Charge)
				require.Equal(t, r.VideoOrigin, l.event.VideoOrigin)
				require.Equal(t, &ledger.VideoUsage{VideoRequest: price.Video.VideoRequest, TaskID: "minimax-h3:task", State: tc.state}, l.event.Video)
			} else {
				require.Zero(t, l.completed)
			}
			require.Zero(t, l.reservations)
			require.Zero(t, l.starts)
		})
	}
}
