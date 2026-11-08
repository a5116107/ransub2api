//go:build integration

package repository

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestTokenGuardProbeSharesUpstreamKeyCapacity(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	accounts := newAccountRepositoryWithSQL(client, tx, nil)
	guardRepo := NewAccountTokenGuardRepository(integrationDB)
	token := "guard-capacity-fixture"
	account := mustCreateAccount(t, client, &service.Account{Name: "guard-capacity@example.com", Platform: service.PlatformOpenAI,
		Type: service.AccountTypeOAuth, Status: service.StatusActive, Credentials: map[string]any{"access_token": token}})
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(context.Background(), "DELETE FROM account_token_guard_states WHERE account_id=$1", account.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(context.Background(), "DELETE FROM account_token_guard_events WHERE account_id=$1", account.ID)
		require.NoError(t, err)
	})
	var probeCalls, businessCalls atomic.Int32
	var holdProbe atomic.Bool
	businessRelease := make(chan struct{})
	probeRelease := make(chan struct{})
	probeStarted := make(chan struct{}, 1)
	var releaseBusiness, releaseProbe sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("test server does not support flushing")
			return
		}
		if r.URL.Path == "/business" {
			businessCalls.Add(1)
			w.WriteHeader(http.StatusOK)
			flusher.Flush()
			select {
			case <-businessRelease:
			case <-r.Context().Done():
			}
			return
		}
		probeCalls.Add(1)
		if holdProbe.Load() {
			w.WriteHeader(http.StatusOK)
			flusher.Flush()
			probeStarted <- struct{}{}
			select {
			case <-probeRelease:
			case <-r.Context().Done():
				return
			}
		}
		_, _ = io.WriteString(w, `{"status":"active"}`)
	}))
	t.Cleanup(func() {
		releaseBusiness.Do(func() { close(businessRelease) })
		releaseProbe.Do(func() { close(probeRelease) })
		server.Close()
	})
	appCfg := &config.Config{}
	appCfg.Runtime.Role = config.RuntimeRoleGateway
	appCfg.Security.URLAllowlist.AllowInsecureHTTP = true
	appCfg.Gateway.UpstreamKeyConcurrency = config.UpstreamKeyConcurrencyConfig{Revision: 1,
		Limits: map[string]int{config.UpstreamKeyFingerprint(token): 1}}
	redisOptions := *integrationRedis.Options()
	redisOptions.DB = 14
	rdb := redis.NewClient(&redisOptions)
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	transport := ProvideHTTPUpstream(appCfg, nil, rdb, nil)
	svc := service.ProvideAccountTokenGuardService(NewSettingRepository(client), guardRepo, accounts, nil, nil, appCfg, transport)
	guardCfg := service.AccountTokenGuardConfig{IntervalSeconds: 300, ProbeTimeoutSeconds: 5, ProbeConcurrency: 1,
		MaxProbePerCycle: 1, FailStreakThreshold: 2, ProbeEndpoint: server.URL + "/probe"}
	_, err := svc.SaveConfig(ctx, guardCfg)
	require.NoError(t, err)
	businessRequest := func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/business", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		return transport.Do(req, "", account.ID, 1)
	}
	first, err := businessRequest()
	require.NoError(t, err)
	require.NotNil(t, first)
	t.Cleanup(func() { _ = first.Body.Close() })
	stats, err := svc.RunCycle(ctx, false)
	require.NoError(t, err)
	require.Zero(t, probeCalls.Load(), "a full shared key must block the independent probe before sending")
	require.Equal(t, 1, stats.Transient)
	require.Zero(t, stats.AuthFailed)
	require.NoError(t, first.Body.Close())
	releaseBusiness.Do(func() { close(businessRelease) })
	stats, err = svc.RunCycle(ctx, false)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Healthy)
	require.EqualValues(t, 1, probeCalls.Load())

	holdProbe.Store(true)
	done := make(chan error, 1)
	go func() {
		_, runErr := svc.RunCycle(ctx, false)
		done <- runErr
	}()
	select {
	case <-probeStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("probe did not start")
	}
	before := businessCalls.Load()
	competing, err := businessRequest()
	if competing != nil {
		_ = competing.Body.Close()
	}
	require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)
	require.Equal(t, before, businessCalls.Load(), "an in-flight probe must keep the shared slot")
	releaseProbe.Do(func() { close(probeRelease) })
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("probe did not finish")
	}
	last, err := businessRequest()
	require.NoError(t, err)
	require.NoError(t, last.Body.Close())
}

// Exercise the service with the real account queries. In-memory repositories
// previously returned error accounts even for the active-only scheduling query.
func TestTokenGuardRecoveryRealRepositoryScope(t *testing.T) {
	for _, selectedGroups := range []bool{false, true} {
		name := "all_groups"
		if selectedGroups {
			name = "selected_groups"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			tx := testEntTx(t)
			client := tx.Client()
			accounts := newAccountRepositoryWithSQL(client, tx, nil)
			guardRepo := NewAccountTokenGuardRepository(integrationDB)
			group1 := mustCreateGroup(t, client, &service.Group{Name: "guard-one"})
			group2 := mustCreateGroup(t, client, &service.Group{Name: "guard-two"})
			create := func(name, status string) *service.Account {
				return mustCreateAccount(t, client, &service.Account{Name: name, Platform: service.PlatformOpenAI,
					Type: service.AccountTypeOAuth, Status: status, Credentials: map[string]any{"access_token": "test-access"}})
			}
			active := create("active@example.com", service.StatusActive)
			// The admin "active" filter also filters scheduling/cooldowns;
			// these must not narrow credential inspection.
			err := client.Account.UpdateOneID(active.ID).SetSchedulable(false).
				SetRateLimitResetAt(time.Now().Add(time.Hour)).Exec(ctx)
			require.NoError(t, err)
			failed := create("error@example.com", service.StatusError)
			disabled := create("disabled@example.com", service.StatusDisabled)
			deleted := create("deleted@example.com", service.StatusError)
			require.NoError(t, client.Account.UpdateOneID(deleted.ID).SetDeletedAt(time.Now()).Exec(ctx))
			outside := create("outside@example.com", service.StatusError)
			managed := create("operations@example.com", service.StatusError)
			_, err = tx.ExecContext(ctx, "INSERT INTO account_token_guard_v2_accounts(account_id,enabled,auto_relogin_enabled) VALUES($1,false,true)", managed.ID)
			require.NoError(t, err)
			mustBindAccountToGroup(t, client, managed.ID, group1.ID, 1)
			for _, account := range []*service.Account{active, failed, disabled, deleted} {
				mustBindAccountToGroup(t, client, account.ID, group1.ID, 1)
				mustBindAccountToGroup(t, client, account.ID, group2.ID, 1)
			}
			ids := []int64{active.ID, failed.ID, disabled.ID, deleted.ID, outside.ID, managed.ID}
			t.Cleanup(func() {
				_, cleanupErr := integrationDB.ExecContext(context.Background(), "DELETE FROM account_token_guard_states WHERE account_id = ANY($1)", pq.Array(ids))
				require.NoError(t, cleanupErr)
				_, cleanupErr = integrationDB.ExecContext(context.Background(), "DELETE FROM account_token_guard_events WHERE account_id = ANY($1)", pq.Array(ids))
				require.NoError(t, cleanupErr)
			})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer server.Close()
			svc := service.NewAccountTokenGuardService(NewSettingRepository(client), guardRepo, accounts, nil, nil)
			cfg := service.AccountTokenGuardConfig{IntervalSeconds: 300, ProbeTimeoutSeconds: 5, ProbeConcurrency: 1,
				MaxProbePerCycle: 100, FailStreakThreshold: 2, ProbeEndpoint: server.URL, AutoRelogin: false}
			want := []int64{active.ID, failed.ID, outside.ID}
			if selectedGroups {
				cfg.GroupIDs = []int64{group1.ID, group2.ID}
				want = []int64{active.ID, failed.ID}
			}
			_, err = svc.SaveConfig(ctx, cfg)
			require.NoError(t, err)
			stats, err := svc.RunCycle(ctx, false)
			require.NoError(t, err)
			require.Equal(t, len(want), stats.Probed)
			require.Equal(t, len(want), stats.Transient)
			states, err := guardRepo.ListStates(ctx)
			require.NoError(t, err)
			var got []int64
			for _, state := range states {
				for _, id := range ids {
					if state.AccountID == id {
						got = append(got, id)
					}
				}
			}
			require.ElementsMatch(t, want, got)
		})
	}
}
