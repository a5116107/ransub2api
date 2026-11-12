//go:build integration

package repository

import (
	"context"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestPriorityAccountCostMultiplierRoundTrip(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	billing, group := 1.0, 5.0
	account := mustCreateAccount(t, tx.Client(), &service.Account{Name: "cost-roundtrip", Platform: service.PlatformOpenAI, RateMultiplier: &billing, GroupRateMultiplier: &group, Extra: map[string]any{"untouched": true}})
	require.NoError(t, repo.Update(ctx, account))
	require.Equal(t, 0.1, account.CostMultiplier())
	for _, cost := range []float64{0.25, 0} {
		require.NoError(t, repo.UpdateExtra(ctx, account.ID, map[string]any{service.AccountCostMultiplierExtraKey: cost}))
		loaded, err := repo.GetByID(ctx, account.ID)
		require.NoError(t, err)
		require.Equal(t, cost, loaded.CostMultiplier())
		require.Equal(t, billing, loaded.BillingRateMultiplier())
		require.Equal(t, group, loaded.UserGroupRateMultiplier())
		require.Equal(t, true, loaded.Extra["untouched"])
	}
	require.True(t, shouldEnqueueSchedulerOutboxForExtraUpdates(map[string]any{service.AccountCostMultiplierExtraKey: 0.1}), "cost edits invalidate cached scheduler accounts")
}

func TestPrioritySchedulingFinanceScopeAndMissingPerformance(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	repo := newUsageLogRepositoryWithSQL(client, tx)
	user := mustCreateUser(t, client, &service.User{Email: "priority-finance@test.invalid"})
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "synthetic-priority-finance", Name: "test"})
	account := mustCreateAccount(t, client, &service.Account{Name: "finance", Platform: service.PlatformOpenAI})
	now := time.Now()
	for _, model := range []string{"model-a", "model-b"} {
		_, err := repo.Create(ctx, &service.UsageLog{UserID: user.ID, APIKeyID: key.ID, AccountID: account.ID, RequestID: model, Model: model, InputTokens: 10, ActualCost: 2, TotalCost: 4, CreatedAt: now})
		require.NoError(t, err)
	}
	query := service.PrioritySchedulingQuery{AccountIDs: []int64{account.ID}, Model: "model-a", UsageSince: now.Add(-time.Hour), UsageUntil: now.Add(time.Second)}
	got, err := repo.ReadPrioritySchedulingSignals(ctx, query)
	require.NoError(t, err)
	require.Equal(t, 1, got[account.ID].ProfitSamples)
	require.Equal(t, 2.0, got[account.ID].Revenue)
	require.NotNil(t, got[account.ID].Performance)
	require.Nil(t, got[account.ID].Performance.AvgTPS)
	require.Nil(t, got[account.ID].Performance.AvgTTFTMs)
	require.Nil(t, got[account.ID].Performance.DegradationPercent)
	query.Model = "missing-model"
	missing, err := repo.ReadPrioritySchedulingSignals(ctx, query)
	require.NoError(t, err)
	require.Zero(t, missing[account.ID].ProfitSamples)
	require.Equal(t, got[account.ID].Performance, missing[account.ID].Performance)
}

// Exercise the public scheduler and physical HTTP send with independent gateway
// caches and slot owners. Neither an in-memory binding nor a fake slot result
// can establish that an eligible session survives a request on another node.
func TestPrioritySessionAffinityAcrossGateways(t *testing.T) {
	var sends atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/hold" {
			w.WriteHeader(http.StatusOK)
			flusher, ok := w.(http.Flusher)
			if !ok {
				return
			}
			flusher.Flush()
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, `{"id":"resp_affinity","object":"response","status":"completed","model":"gpt-test","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":3,"output_tokens":1}}`)
	}))
	t.Cleanup(upstream.Close)
	f := NewPrioritySessionFixture(t, []string{upstream.URL, upstream.URL})
	accounts, groups, settingsRepo, group, pool, clients := f.Accounts, f.Groups, f.Settings, f.Group, f.Pool, f.Clients
	var nodes [2]*service.OpenAIGatewayService
	var transports [2]service.HTTPUpstream
	var snapshots [2]service.SchedulerCache
	var caches [2]service.GatewayCache
	var slots [2]service.ConcurrencyCache
	for i := range nodes {
		cfg := upstreamKeyTestConfig(1, 1)
		cfg.Security.URLAllowlist.AllowPrivateHosts = true
		cfg.Security.URLAllowlist.AllowInsecureHTTP = true
		cfg.Gateway.Scheduling.LoadBatchEnabled = true
		cfg.Gateway.Scheduling.StickySessionWaitTimeout = 75 * time.Millisecond
		cfg.Gateway.Scheduling.StickySessionMaxWaiting = 3
		settings := service.NewSettingService(settingsRepo, cfg)
		policy := service.DefaultPrioritySchedulingConfig()
		policy.Enabled, policy.PreserveSessionAffinity = true, true
		policy.GroupIDs, policy.Models = []int64{group.ID}, []string{"gpt-test"}
		require.NoError(t, settings.SavePrioritySchedulingConfig(t.Context(), &policy))
		cache := NewSchedulerCache(clients[i])
		snapshots[i] = cache
		bucket := service.SchedulerBucket{GroupID: group.ID, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
		token, err := cache.CaptureBucketWriteToken(t.Context(), bucket)
		require.NoError(t, err)
		require.NoError(t, cache.SetSnapshot(t.Context(), bucket, token, pool))
		slots[i], caches[i] = NewConcurrencyCache(clients[i], 15, 60), NewGatewayCache(clients[i])
		transports[i] = newHTTPUpstream(cfg, nil, clients[i], slots[i])
		nodes[i] = service.ProvideOpenAIGatewayService(service.OpenAIGatewayDependencies{
			Accounts: accounts, Config: cfg, Settings: settings, Cache: caches[i], Concurrency: service.NewConcurrencyService(slots[i]),
			Scheduler: service.NewSchedulerSnapshotService(cache, nil, accounts, groups, cfg),
			Upstream:  transports[i],
		})
		node := nodes[i]
		t.Cleanup(node.StopOpenAICodexTicketHarvester)
		t.Cleanup(node.StopAsyncVideoKeyReleaseRecovery)
		t.Cleanup(node.CloseOpenAIWSPool)
		t.Cleanup(func() { require.NoError(t, node.CloseExcelBPSImages()) })
	}
	selectAccount := func(node int, session string, excluded map[int64]struct{}) *service.AccountSelectionResult {
		t.Helper()
		bucket := service.SchedulerBucket{GroupID: group.ID, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
		metadata, hit, err := snapshots[node].GetSnapshot(t.Context(), bucket)
		require.NoError(t, err)
		require.True(t, hit, "seeded snapshot must remain readable before selecting %s", session)
		require.Len(t, metadata, 2)
		ctx := context.WithValue(t.Context(), ctxkey.Group, group)
		ctx = service.WithPrioritySchedulingObservation(ctx, &group.ID, "gpt-test", "/v1/responses")
		selected, _, err := nodes[node].SelectAccountWithSchedulerForCapability(ctx, &group.ID, "", session, "gpt-test", excluded, service.OpenAIUpstreamTransportAny, service.OpenAIEndpointCapabilityResponses, false, false, true, service.PlatformOpenAI)
		require.NoError(t, err)
		require.NotNil(t, selected)
		if selected.ReleaseFunc != nil {
			t.Cleanup(selected.ReleaseFunc)
		}
		return selected
	}
	bind := func(node int, session string, selected *service.AccountSelectionResult) {
		t.Helper()
		ctx := service.ContextWithSelectionProfitGate(t.Context(), selected)
		require.NoError(t, nodes[node].BindStickySessionAfterProfitAdmission(ctx, &group.ID, session, selected.Account.ID))
	}
	assertBinding := func(session string, want int64) {
		t.Helper()
		for _, cache := range caches {
			id, err := cache.GetSessionAccountID(t.Context(), group.ID, "openai:"+session)
			require.NoError(t, err)
			require.Equal(t, want, id)
		}
	}
	const session = "shared-session"
	require.NoError(t, nodes[0].BindStickySession(t.Context(), &group.ID, session, pool[0].ID))
	first := selectAccount(0, session, nil)
	require.Equal(t, pool[0].ID, first.Account.ID)
	require.True(t, first.Acquired)
	bound := selectAccount(1, session, nil)
	require.Equal(t, pool[0].ID, bound.Account.ID)
	require.False(t, bound.Acquired, "a second process must not manufacture a free slot")
	require.NotNil(t, bound.WaitPlan)
	require.Equal(t, 75*time.Millisecond, bound.WaitPlan.Timeout)
	require.Equal(t, 3, bound.WaitPlan.MaxWaiting)
	assertBinding(session, pool[0].ID)
	newSession := selectAccount(1, "new-session", nil)
	require.Equal(t, pool[1].ID, newSession.Account.ID, "a new session must use spare eligible capacity")
	require.True(t, newSession.Acquired)
	bind(1, "new-session", newSession)
	assertBinding("new-session", pool[1].ID)
	assertBinding(session, pool[0].ID)
	newSession.ReleaseFunc()
	first.ReleaseFunc()
	recovered := selectAccount(1, session, nil)
	require.Equal(t, pool[0].ID, recovered.Account.ID)
	require.True(t, recovered.Acquired, "the other node must observe the released slot")
	recovered.ReleaseFunc()

	// A manual hard stop is not a soft score preference. The old Redis bucket
	// remains warm deliberately: the fresh terminal check must reject it.
	require.NoError(t, accounts.SetSchedulable(t.Context(), pool[0].ID, false))
	switched := selectAccount(1, session, nil)
	require.Equal(t, pool[1].ID, switched.Account.ID)
	bind(1, session, switched)
	assertBinding(session, pool[1].ID)
	switched.ReleaseFunc()
	require.NoError(t, accounts.SetSchedulable(t.Context(), pool[0].ID, true))
	retained := selectAccount(0, session, nil)
	require.Equal(t, pool[1].ID, retained.Account.ID, "recovery must not reclaim a healthy migrated session")
	retained.ReleaseFunc()

	// Same upstream Key on two account records still has one HTTP allowance.
	// Hold an actual HTTP response on node A and verify node B sends nothing.
	keyAuthority, ok := transports[0].(service.UpstreamKeyConcurrency)
	require.True(t, ok)
	holdRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost, upstream.URL+"/hold", strings.NewReader("{}"))
	require.NoError(t, err)
	holdRequest.Header.Set("Authorization", "Bearer supplier-fixture")
	held, err := transports[0].Do(holdRequest, "", pool[0].ID, pool[0].Concurrency)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, held.Body.Close()) })
	probeRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost, upstream.URL+"/v1/responses", strings.NewReader("{}"))
	require.NoError(t, err)
	probeRequest.Header.Set("Authorization", "Bearer supplier-fixture")
	probe, err := transports[1].Do(probeRequest, "", pool[1].ID, pool[1].Concurrency)
	require.Nil(t, probe)
	require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)
	selected := selectAccount(1, session, nil)
	require.True(t, selected.Acquired)
	forward := func() (*service.OpenAIForwardResult, *httptest.ResponseRecorder, error) {
		t.Helper()
		body := []byte(`{"model":"gpt-test","input":"hello","stream":false}`)
		ctx := service.ContextWithSelectionProfitGate(t.Context(), selected)
		ctx = service.ContextWithSelectionPriorityRequest(ctx, selected)
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body))).WithContext(ctx)
		c.Set("api_key", f.Key)
		result, err := nodes[1].Forward(ctx, c, selected.Account, body)
		return result, recorder, err
	}
	result, _, err := forward()
	require.Nil(t, result)
	var admission *service.UpstreamFailoverError
	require.ErrorAs(t, err, &admission)
	require.Equal(t, http.StatusServiceUnavailable, admission.StatusCode)
	require.Equal(t, service.UpstreamKeyAdmissionReason, admission.Reason)
	require.EqualValues(t, 1, sends.Load(), "only the first node's held request may reach the upstream")
	assertBinding(session, pool[1].ID)
	require.NoError(t, held.Body.Close())
	result, response, err := forward()
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, http.StatusOK, response.Code)
	require.Contains(t, response.Body.String(), "done")
	require.EqualValues(t, 2, sends.Load())
	selected.ReleaseFunc()
	for _, cache := range slots {
		for _, a := range pool {
			count, err := cache.GetAccountConcurrency(t.Context(), a.ID)
			require.NoError(t, err)
			require.Zero(t, count, "all gateway account members must be released")
		}
	}
	_, releaseKey, err := keyAuthority.AcquireUpstreamKeySlots(t.Context(), []string{config.UpstreamKeyFingerprint("supplier-fixture")})
	require.NoError(t, err, "the completed HTTP response must release its Key member")
	require.NotNil(t, releaseKey)
	releaseKey()
}

type PrioritySessionFixture struct {
	Accounts service.AccountRepository
	Groups   service.GroupRepository
	Settings service.SettingRepository
	Group    *service.Group
	Pool     []service.Account
	Clients  []*redis.Client
	Prefix   string
	DSN      string
	Key      *service.APIKey
}

func NewPrioritySessionFixture(t *testing.T, upstreams []string) *PrioritySessionFixture {
	t.Helper()
	// Background readers need real independent SQL connections, not concurrent
	// operations on one outer transaction. Only this fixture's rows are removed.
	f := &PrioritySessionFixture{
		DSN:      integrationPostgresDSN,
		Accounts: newAccountRepositoryWithSQL(integrationEntClient, integrationDB, nil),
		Groups:   newGroupRepositoryWithSQL(integrationEntClient, integrationDB),
		Settings: NewSettingRepository(integrationEntClient),
	}
	previous, err := f.Settings.GetValue(t.Context(), "priority_scheduling_v1")
	missing := err != nil
	if missing {
		require.ErrorIs(t, err, service.ErrSettingNotFound)
	}
	t.Cleanup(func() {
		if missing {
			_, err := integrationDB.Exec(`DELETE FROM settings WHERE key='priority_scheduling_v1'`)
			require.NoError(t, err)
		} else {
			require.NoError(t, f.Settings.Set(context.Background(), "priority_scheduling_v1", previous))
		}
	})
	f.Group = mustCreateGroup(t, integrationEntClient, &service.Group{Name: "affinity", Platform: service.PlatformOpenAI, RateMultiplier: 1, ProfitControlEnabled: true})
	user := mustCreateUser(t, integrationEntClient, &service.User{Email: fmt.Sprintf("affinity-%d@test.invalid", f.Group.ID), Balance: 100, Concurrency: 8})
	f.Key = mustCreateApiKey(t, integrationEntClient, &service.APIKey{UserID: user.ID, Key: fmt.Sprintf("sk-affinity-%d", f.Group.ID), Name: "affinity", GroupID: &f.Group.ID})
	f.Key.User, f.Key.Group = user, f.Group
	t.Cleanup(func() {
		for _, statement := range []string{
			`DELETE FROM usage_billing_events WHERE api_key_id=$1`,
			`DELETE FROM usage_logs WHERE api_key_id=$1`,
			`DELETE FROM api_keys WHERE id=$1`,
		} {
			_, err := integrationDB.Exec(statement, f.Key.ID)
			require.NoError(t, err)
		}
		_, err := integrationDB.Exec(`DELETE FROM users WHERE id=$1`, user.ID)
		require.NoError(t, err)
		_, err = integrationDB.Exec(`DELETE FROM account_groups WHERE group_id=$1`, f.Group.ID)
		require.NoError(t, err)
		for _, a := range f.Pool {
			_, err = integrationDB.Exec(`DELETE FROM accounts WHERE id=$1`, a.ID)
			require.NoError(t, err)
		}
		_, err = integrationDB.Exec(`DELETE FROM groups WHERE id=$1`, f.Group.ID)
		require.NoError(t, err)
	})
	f.Clients, f.Prefix = sharedConcurrencyRedisClients(t, 2)
	for i, upstream := range upstreams {
		a := mustCreateAccount(t, integrationEntClient, &service.Account{Name: fmt.Sprintf("affinity-%d", i), Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Concurrency: 1, Priority: i + 1,
			Credentials: map[string]any{"api_key": "supplier-fixture", "base_url": upstream}})
		rate := 0.1
		a.RateMultiplier = &rate
		require.NoError(t, f.Accounts.Update(t.Context(), a))
		require.NoError(t, f.Accounts.BindGroups(t.Context(), a.ID, []int64{f.Group.ID}))
		loaded, err := f.Accounts.GetByID(t.Context(), a.ID)
		require.NoError(t, err)
		f.Pool = append(f.Pool, *loaded)
	}
	return f
}
