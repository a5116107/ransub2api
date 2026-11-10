package service

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/regional/ledger"
	"github.com/Wei-Shaw/sub2api/internal/regional/video"
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
}

func (*videoOwnerTestLedger) BindVideoTask(context.Context, string, string) error {
	panic("owner resolution must not bind or start tasks")
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
