//go:build integration

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/testutil/assertions"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newAPIIntegrationAccounts(t *testing.T) (*service.Account, *service.Account, string) {
	t.Helper()
	site := fmt.Sprintf("https://new-api-%d.example/tenant", time.Now().UnixNano())
	client := testEntClient(t)
	a := mustCreateAccount(t, client, &service.Account{Name: "new-api-first", Platform: "openai", Type: "apikey", Credentials: map[string]any{"api_key": "sk-first", "base_url": site + "/v1"}, Extra: map[string]any{service.AccountCostMultiplierExtraKey: 0.3}})
	b := mustCreateAccount(t, client, &service.Account{Name: "new-api-second", Platform: "openai", Type: "apikey", Credentials: map[string]any{"api_key": "sk-second", "base_url": site}, Extra: map[string]any{service.AccountCostMultiplierExtraKey: 0.7}})
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM new_api_site_authorizations WHERE site_url=$1`, site)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM scheduler_outbox WHERE account_id IN($1,$2)`, a.ID, b.ID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM accounts WHERE id IN($1,$2)`, a.ID, b.ID)
	})
	return a, b, site
}
func TestNewAPIIntegrationSharedEncryptedRotationAndUnbind(t *testing.T) {
	ctx := context.Background()
	a, b, site := newAPIIntegrationAccounts(t)
	r := NewNewAPIAuthorizationRepository(integrationDB)
	enc, e := NewAESEncryptor(&config.Config{Totp: config.TotpConfig{EncryptionKey: strings.Repeat("42", 32)}})
	require.NoError(t, e)
	cipher, e := enc.Encrypt("secret-first")
	require.NoError(t, e)
	p := &service.NewAPISiteAuthorization{SiteURL: site, UserID: 7, Ciphertext: cipher}
	require.NoError(t, r.Save(ctx, p, []service.NewAPIBindingSave{{Account: a, TokenID: 3, Group: "vip"}, {Account: b, TokenID: 4, Group: "default"}}))
	pa, e := r.GetProfile(ctx, site, 7)
	require.NoError(t, e)
	require.NotEqual(t, "secret-first", pa.Ciphertext)
	secret, e := enc.Decrypt(pa.Ciphertext)
	require.NoError(t, e)
	require.Equal(t, "secret-first", secret)
	ba, e := r.GetBinding(ctx, a.ID)
	require.NoError(t, e)
	bb, e := r.GetBinding(ctx, b.ID)
	require.NoError(t, e)
	require.Equal(t, ba.Profile.ID, bb.Profile.ID)
	var count int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT count(*) FROM new_api_site_authorizations WHERE site_url=$1`, site).Scan(&count))
	require.Equal(t, 1, count)
	ar := newAccountRepositoryWithSQL(testEntClient(t), integrationDB, nil)
	loaded, e := ar.GetByID(ctx, b.ID)
	require.NoError(t, e)
	now := time.Now().UTC()
	snap := &service.UpstreamBillingProbeSnapshot{Status: "ok", LastAttemptAt: now, NextProbeAt: now.Add(time.Hour), Data: map[string]any{"provider": "new_api", "object": "new_api.group_billing", "billing_scope": "group", "resolved_rate_multiplier": 9.0, "effective_rate_multiplier": 9.0, "peak_rate_enabled": false}}
	require.NoError(t, r.WriteSnapshot(ctx, loaded, bb, snap))
	updated, e := ar.GetByID(ctx, b.ID)
	require.NoError(t, e)
	require.Equal(t, 0.7, updated.CostMultiplier())
	require.Equal(t, 1.0, updated.BillingRateMultiplier())
	require.Equal(t, "new_api", updated.Extra[service.UpstreamBillingProviderExtraKey])
	require.True(t, assertions.Type[bool](t, updated.Extra[service.UpstreamBillingProbeEnabledExtraKey]))
	require.Contains(t, updated.Extra, service.UpstreamBillingProbeExtraKey)
	stale := *bb
	pa.Ciphertext, e = enc.Encrypt("secret-rotated")
	require.NoError(t, e)
	require.NoError(t, r.Save(ctx, pa, []service.NewAPIBindingSave{{Account: a, TokenID: 3, Group: "vip"}}))
	updated, e = ar.GetByID(ctx, b.ID)
	require.NoError(t, e)
	require.NotContains(t, updated.Extra, service.UpstreamBillingProbeExtraKey)
	require.ErrorIs(t, r.WriteSnapshot(ctx, updated, &stale, snap), service.ErrUpstreamBillingProbeIdentityChanged)
	require.NoError(t, r.Unbind(ctx, a.ID))
	ba, e = r.GetBinding(ctx, a.ID)
	require.NoError(t, e)
	require.Nil(t, ba)
	bb, e = r.GetBinding(ctx, b.ID)
	require.NoError(t, e)
	require.NotNil(t, bb)
	require.Equal(t, int64(2), bb.Profile.Revision)
	secret, e = enc.Decrypt(bb.Profile.Ciphertext)
	require.NoError(t, e)
	require.Equal(t, "secret-rotated", secret)
	updated, e = ar.GetByID(ctx, a.ID)
	require.NoError(t, e)
	require.NotContains(t, updated.Extra, service.UpstreamBillingProviderExtraKey)
}
func TestNewAPIIntegrationProbeWaitPersistsAcrossRunners(t *testing.T) {
	ctx := t.Context()
	db, client := newBillingAuditDatabase(t)
	const enabledAccounts = 100
	var phase atomic.Int32
	var calls [enabledAccounts + 4][6]atomic.Int64
	var active, maximum, unexpected atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for previous := maximum.Load(); current > previous && !maximum.CompareAndSwap(previous, current); previous = maximum.Load() {
		}
		// Observe overlap between independent slots without extending network deadlines.
		time.Sleep(3 * time.Millisecond)
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/line-"), "/", 2)
		if len(parts) != 2 {
			unexpected.Add(1)
			http.NotFound(w, r)
			return
		}
		index, err := strconv.Atoi(parts[0])
		if err != nil || index < 0 || index >= len(calls) || r.Header.Get("Authorization") != "Bearer fixture-management-secret" || r.Header.Get("New-Api-User") != "7" {
			unexpected.Add(1)
			http.Error(w, "unexpected fixture request", http.StatusBadRequest)
			return
		}
		step := -1
		body := ""
		switch r.Method + " /" + parts[1] {
		case "GET /api/status":
			step, body = 0, `{"success":true,"data":{"quota_per_unit":500000}}`
		case "GET /api/user/self":
			step, body = 1, `{"success":true,"data":{"id":7,"quota":1250000}}`
		case "GET /api/pricing":
			step, body = 2, `{"success":true,"group_ratio":{"vip":0.03}}`
			if index == 0 {
				switch phase.Load() {
				case 3:
					body = `{"success":true,"group_ratio":{"vip":null,"other":0.01}}`
				case 4:
					body = `{"success":true,"group_ratio":{"vip":0,"other":null}}`
				}
			}
		case "GET /api/token/":
			switch r.URL.Query().Get("p") {
			case "0":
				step, body = 3, `{"success":true,"data":{"items":[{"id":2,"key":"foreign","group":"vip"}],"total":2,"page":1,"page_size":1}}`
			case "2":
				step, body = 4, `{"success":true,"data":{"items":[{"id":3,"key":"abcd**********wxyz","group":"vip"}],"total":2,"page":2,"page_size":1}}`
			}
		case "POST /api/token/3/key":
			step, body = 5, fmt.Sprintf(`{"success":true,"data":{"key":"sk-abcd%d123456789wxyz"}}`, index)
		}
		if step < 0 {
			unexpected.Add(1)
			http.NotFound(w, r)
			return
		}
		calls[index][step].Add(1)
		w.Header().Set("Content-Type", "application/json")
		if phase.Load() == 1 && ((index < 10 && step == 2) || (index >= 10 && index < 20 && step == 4)) {
			w.Header().Set("Retry-After", "14400")
			code := http.StatusServiceUnavailable
			if index >= 10 {
				code = http.StatusTooManyRequests
			}
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":"fixture response must not be persisted"}`))
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true}}}
	upstream, ok := NewHTTPUpstream(cfg).(*httpUpstreamService)
	require.True(t, ok)
	t.Cleanup(func() {
		upstream.mu.Lock()
		defer upstream.mu.Unlock()
		for _, entry := range upstream.clients {
			entry.client.CloseIdleConnections()
		}
	})
	encryptor, err := NewAESEncryptor(&config.Config{Totp: config.TotpConfig{EncryptionKey: strings.Repeat("42", 32)}})
	require.NoError(t, err)
	ciphertext, err := encryptor.Encrypt("fixture-management-secret")
	require.NoError(t, err)
	auth := NewNewAPIAuthorizationRepository(db)
	settings := service.NewSettingService(NewSettingRepository(client), cfg)
	require.NoError(t, settings.SetUpstreamBillingProbeSettings(ctx, &service.UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: 5}))
	ids := make([]int64, len(calls))
	for i := range ids {
		site := fmt.Sprintf("%s/line-%d", server.URL, i)
		account := mustCreateAccount(t, client, &service.Account{
			Name: fmt.Sprintf("new-api-wait-%d", i), Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
			Status: service.StatusActive, Concurrency: 1,
			Credentials: map[string]any{"api_key": fmt.Sprintf("sk-abcd%d123456789wxyz", i), "base_url": site + "/v1"},
			Extra:       map[string]any{service.AccountCostMultiplierExtraKey: 0.7},
		})
		ids[i] = account.ID
		require.NoError(t, auth.Save(ctx, &service.NewAPISiteAuthorization{SiteURL: site, UserID: 7, Ciphertext: ciphertext}, []service.NewAPIBindingSave{{Account: account, TokenID: 3, Group: "vip"}}))
	}
	_, err = db.ExecContext(ctx, `UPDATE accounts SET extra=jsonb_set(extra,'{upstream_billing_probe_enabled}','false'::jsonb) WHERE id=$1`, ids[enabledAccounts])
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE accounts SET status='disabled' WHERE id=$1`, ids[enabledAccounts+1])
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE accounts SET type='oauth' WHERE id=$1`, ids[enabledAccounts+2])
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE accounts SET extra=jsonb_set(extra,'{upstream_billing_probe}',jsonb_build_object('status','ok','next_probe_at',$1::text)) WHERE id=$2`, time.Now().Add(24*time.Hour).UTC().Format(time.RFC3339Nano), ids[enabledAccounts+3])
	require.NoError(t, err)
	run := func(t *testing.T) {
		t.Helper()
		// Reconstruct every runner and repository: progress must come from SQL.
		repo := newAccountRepositoryWithSQL(client, db, nil)
		tester := service.NewAccountTestService(repo, nil, nil, nil, nil, upstream, cfg, nil)
		runner := service.NewUpstreamBillingProbeService(repo, tester, settings)
		runner.SetNewAPIAuthorization(NewNewAPIAuthorizationRepository(db), encryptor, true)
		defer runner.Stop()
		require.NoError(t, runner.RunDue(ctx))
	}
	read := func(index int) service.UpstreamBillingProbeSnapshot {
		t.Helper()
		var raw []byte
		require.NoError(t, db.QueryRowContext(ctx, `SELECT extra->'upstream_billing_probe' FROM accounts WHERE id=$1`, ids[index]).Scan(&raw))
		var snapshot service.UpstreamBillingProbeSnapshot
		require.NoError(t, json.Unmarshal(raw, &snapshot))
		require.NotContains(t, string(raw), "fixture-management-secret")
		require.NotContains(t, string(raw), "fixture response")
		return snapshot
	}
	due := func(index int) {
		t.Helper()
		_, err := db.ExecContext(ctx, `UPDATE accounts SET extra=jsonb_set(extra,'{upstream_billing_probe,next_probe_at}',to_jsonb($1::text)) WHERE id=$2`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), ids[index])
		require.NoError(t, err)
	}
	runAll := func(t *testing.T, priorCalls int64) {
		t.Helper()
		for cycle := 0; cycle < enabledAccounts/20; cycle++ {
			t.Run(fmt.Sprintf("cycle_%d", cycle+1), func(t *testing.T) {
				run(t)
				for i := 0; i < enabledAccounts; i++ {
					want := priorCalls
					if i < (cycle+1)*20 {
						want++
					}
					require.Equal(t, want, calls[i][0].Load(), "binding %d must get exactly one turn per scan", i)
				}
				repo := newAccountRepositoryWithSQL(client, db, nil)
				due, err := repo.ListDueUpstreamBillingProbeAccounts(ctx, time.Now(), enabledAccounts+4)
				require.NoError(t, err)
				require.Len(t, due, enabledAccounts-(cycle+1)*20)
			})
		}
	}
	t.Run("initial_hundred_bindings", func(t *testing.T) { runAll(t, 0) })
	for i := 0; i < enabledAccounts; i++ {
		require.Equal(t, service.UpstreamBillingProbeStatusOK, read(i).Status)
		due(i)
	}
	phase.Store(1)
	t.Run("waits_do_not_starve_healthy_bindings", func(t *testing.T) { runAll(t, 1) })
	for i := 0; i < 20; i++ {
		code := http.StatusServiceUnavailable
		if i >= 10 {
			code = http.StatusTooManyRequests
		}
		snapshot := read(i)
		require.Equal(t, service.UpstreamBillingProbeStatusFailed, snapshot.Status)
		require.Equal(t, code, snapshot.HTTPStatus)
		require.Equal(t, "new_api_request_failed", snapshot.LastError)
		require.GreaterOrEqual(t, snapshot.NextProbeAt.Sub(snapshot.LastAttemptAt), 4*time.Hour)
		require.Nil(t, snapshot.Data)
		require.Nil(t, snapshot.FreshUntil)
		require.Nil(t, snapshot.ReceivedAt)
	}
	for i := 20; i < enabledAccounts; i++ {
		require.Equal(t, service.UpstreamBillingProbeStatusOK, read(i).Status, "waiting lines must not prevent independent lines from being probed")
	}
	var before [len(calls)][6]int64
	var physicalCalls int64
	for i := range calls {
		for step := range calls[i] {
			want := int64(2)
			if (i < 10 && step > 2) || (i >= 10 && i < 20 && step > 4) {
				want = 1
			}
			if i >= enabledAccounts {
				want = 0
			}
			before[i][step] = calls[i][step].Load()
			physicalCalls += before[i][step]
			require.Equal(t, want, before[i][step], "line %d step %d must stop at its wait response", i, step)
		}
	}
	require.Equal(t, int64(1160), physicalCalls, "count management HTTP calls, not just logical probes")
	run(t)
	for i := range calls {
		for step := range calls[i] {
			require.Equal(t, before[i][step], calls[i][step].Load(), "a new runner must honor the persisted next_probe_at")
		}
	}
	phase.Store(2)
	for i := 0; i < 20; i++ {
		due(i)
	}
	run(t)
	for i := 0; i < enabledAccounts; i++ {
		snapshot := read(i)
		require.Equal(t, service.UpstreamBillingProbeStatusOK, snapshot.Status)
		require.Zero(t, snapshot.FailureCount)
		require.Zero(t, snapshot.HTTPStatus)
		require.Equal(t, 0.03, snapshot.Data["resolved_rate_multiplier"])
		require.Equal(t, service.UpstreamBillingProbeStatusOK, snapshot.Balance.Status)
		var cost, rate float64
		require.NoError(t, db.QueryRowContext(ctx, `SELECT (extra->>'cost_multiplier')::numeric, rate_multiplier FROM accounts WHERE id=$1`, ids[i]).Scan(&cost, &rate))
		require.Equal(t, 0.7, cost)
		require.Equal(t, 1.0, rate, "group evidence must not overwrite the account's token rate")
	}
	physicalCalls = 0
	for i := range calls {
		for step := range calls[i] {
			want := before[i][step]
			if i < 20 {
				want++
			}
			actual := calls[i][step].Load()
			require.Equal(t, want, actual, "only due failed bindings may be retried")
			physicalCalls += actual
		}
	}
	require.Equal(t, int64(1280), physicalCalls)
	require.Greater(t, maximum.Load(), int64(1), "exercise concurrent physical requests")
	require.LessOrEqual(t, maximum.Load(), int64(4), "management pagination and key verification share the existing probe slots")
	require.Zero(t, unexpected.Load())
	t.Logf("100 authorized bindings: five bounded cycles per scan, 1160 HTTP calls before recovery, %d total, max concurrency %d; disabled/OAuth/future bindings untouched", physicalCalls, maximum.Load())
	for _, tc := range []struct {
		name  string
		phase int32
		known bool
		rate  float64
	}{
		{"null_group_revokes_old_rate", 3, false, 0},
		{"explicit_zero_remains_known", 4, true, 0},
		{"numeric_group_recovers", 5, true, 0.03},
	} {
		t.Run(tc.name, func(t *testing.T) {
			phase.Store(tc.phase)
			due(0)
			run(t)
			snapshot := read(0)
			if !tc.known {
				require.Equal(t, service.UpstreamBillingProbeStatusUnsupported, snapshot.Status)
				require.Equal(t, "unsupported_group", snapshot.LastError)
				require.Nil(t, snapshot.ReceivedAt)
				require.Nil(t, snapshot.FreshUntil)
				for _, name := range []string{"group_rate_multiplier", "resolved_rate_multiplier", "effective_rate_multiplier"} {
					require.NotContains(t, snapshot.Data, name, "SQL must not retain a previous successful rate")
				}
			} else {
				require.Equal(t, service.UpstreamBillingProbeStatusOK, snapshot.Status)
				require.Empty(t, snapshot.LastError)
				require.NotNil(t, snapshot.ReceivedAt)
				require.NotNil(t, snapshot.FreshUntil)
				require.Equal(t, tc.rate, snapshot.Data["resolved_rate_multiplier"])
			}
			require.Equal(t, service.UpstreamBillingProbeStatusOK, snapshot.Balance.Status)
			require.Equal(t, 2.5, snapshot.Balance.Data["wallet_balance"])
			var cost, rate float64
			require.NoError(t, db.QueryRowContext(ctx, `SELECT (extra->>'cost_multiplier')::numeric, rate_multiplier FROM accounts WHERE id=$1`, ids[0]).Scan(&cost, &rate))
			require.Equal(t, 0.7, cost)
			require.Equal(t, 1.0, rate)
			for i := range calls {
				for step := range calls[i] {
					want := before[i][step]
					if i < 20 {
						want++
					}
					if i == 0 {
						want += int64(tc.phase - 2)
					}
					require.Equal(t, want, calls[i][step].Load(), "price recovery must not reprobe unrelated bindings")
				}
			}
		})
	}
	require.Zero(t, unexpected.Load())
}

func TestNewAPIIntegrationChangedSecondAccountSavesNothing(t *testing.T) {
	ctx := context.Background()
	a, b, site := newAPIIntegrationAccounts(t)
	r := NewNewAPIAuthorizationRepository(integrationDB)
	_, e := integrationDB.ExecContext(ctx, `UPDATE accounts SET credentials=jsonb_set(credentials,'{api_key}','"changed"'::jsonb) WHERE id=$1`, b.ID)
	require.NoError(t, e)
	e = r.Save(ctx, &service.NewAPISiteAuthorization{SiteURL: site, UserID: 7, Ciphertext: "encrypted-test-only"}, []service.NewAPIBindingSave{{Account: a, TokenID: 3, Group: "vip"}, {Account: b, TokenID: 4, Group: "default"}})
	require.ErrorIs(t, e, service.ErrUpstreamBillingProbeIdentityChanged)
	p, e := r.GetProfile(ctx, site, 7)
	require.NoError(t, e)
	require.Nil(t, p)
	ba, e := r.GetBinding(ctx, a.ID)
	require.NoError(t, e)
	require.Nil(t, ba)
	var extra []byte
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT extra FROM accounts WHERE id=$1`, a.ID).Scan(&extra))
	var data map[string]any
	require.NoError(t, json.Unmarshal(extra, &data))
	require.NotContains(t, data, service.UpstreamBillingProviderExtraKey)
	require.NotContains(t, data, service.UpstreamBillingProbeEnabledExtraKey)
}
func TestNewAPIIntegrationLegacyProbeCannotOverwriteConfiguredBinding(t *testing.T) {
	ctx := context.Background()
	a, _, site := newAPIIntegrationAccounts(t)
	ar := newAccountRepositoryWithSQL(testEntClient(t), integrationDB, nil)
	require.NoError(t, ar.UpdateExtra(ctx, a.ID, map[string]any{service.UpstreamBillingProbeEnabledExtraKey: true, service.UpstreamBillingRateSyncEnabledExtraKey: false}))
	legacy, e := ar.GetByID(ctx, a.ID)
	require.NoError(t, e)
	r := NewNewAPIAuthorizationRepository(integrationDB)
	require.NoError(t, r.Save(ctx, &service.NewAPISiteAuthorization{SiteURL: site, UserID: 7, Ciphertext: "test-cipher"}, []service.NewAPIBindingSave{{Account: legacy, TokenID: 3, Group: "vip"}}))
	snap := &service.UpstreamBillingProbeSnapshot{Status: "ok", LastAttemptAt: time.Now(), Data: map[string]any{"billing_scope": "token", "resolved_rate_multiplier": 4.0, "effective_rate_multiplier": 4.0, "peak_rate_enabled": false}}
	require.ErrorIs(t, ar.UpdateUpstreamBillingProbeSnapshot(ctx, legacy, snap, nil), service.ErrUpstreamBillingProbeIdentityChanged)
	current, e := ar.GetByID(ctx, a.ID)
	require.NoError(t, e)
	require.NotContains(t, current.Extra, service.UpstreamBillingProbeExtraKey)
	require.Equal(t, 0.3, current.CostMultiplier())
}
func TestNewAPIIntegrationAccountEditPreservesThenInvalidatesBindingMarker(t *testing.T) {
	ctx := context.Background()
	a, _, site := newAPIIntegrationAccounts(t)
	ar := newAccountRepositoryWithSQL(testEntClient(t), integrationDB, nil)
	r := NewNewAPIAuthorizationRepository(integrationDB)
	require.NoError(t, r.Save(ctx, &service.NewAPISiteAuthorization{SiteURL: site, UserID: 7, Ciphertext: "test-cipher"}, []service.NewAPIBindingSave{{Account: a, TokenID: 3, Group: "vip"}}))
	current, e := ar.GetByID(ctx, a.ID)
	require.NoError(t, e)
	current.Extra = map[string]any{service.UpstreamBillingProviderExtraKey: "forged"}
	current.Name = "unrelated edit"
	require.NoError(t, ar.Update(ctx, current))
	current, e = ar.GetByID(ctx, a.ID)
	require.NoError(t, e)
	require.Equal(t, "new_api", current.Extra[service.UpstreamBillingProviderExtraKey])
	current.Credentials["api_key"] = "changed"
	require.NoError(t, ar.Update(ctx, current))
	current, e = ar.GetByID(ctx, a.ID)
	require.NoError(t, e)
	require.NotContains(t, current.Extra, service.UpstreamBillingProviderExtraKey)
	binding, e := r.GetBinding(ctx, a.ID)
	require.NoError(t, e)
	require.Nil(t, binding)
}
func TestNewAPIIntegrationGroupRateSortReadsOnlyValidatedGroupSnapshot(t *testing.T) {
	ctx := context.Background()
	a, _, _ := newAPIIntegrationAccounts(t)
	snap := map[string]any{"status": "ok", "data": map[string]any{"provider": "new_api", "object": "new_api.group_billing", "billing_scope": "group", "resolved_rate_multiplier": 0.5, "effective_rate_multiplier": 0.5, "peak_rate_enabled": false}}
	raw, e := json.Marshal(map[string]any{service.UpstreamBillingProbeExtraKey: snap})
	require.NoError(t, e)
	_, e = integrationDB.ExecContext(ctx, `UPDATE accounts SET extra=extra||$1::jsonb WHERE id=$2`, string(raw), a.ID)
	require.NoError(t, e)
	var rate sql.NullFloat64
	e = integrationDB.QueryRowContext(ctx, `SELECT `+upstreamBillingRateSortExpression("extra")+` FROM accounts WHERE id=$1`, a.ID).Scan(&rate)
	require.NoError(t, e)
	require.True(t, rate.Valid)
	require.Equal(t, 0.5, rate.Float64)
}
func TestNewAPIIntegrationDirectOrBulkCredentialsUnbind(t *testing.T) {
	for _, mode := range []string{"direct", "bulk"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			a, _, site := newAPIIntegrationAccounts(t)
			r := NewNewAPIAuthorizationRepository(integrationDB)
			require.NoError(t, r.Save(ctx, &service.NewAPISiteAuthorization{SiteURL: site, UserID: 7, Ciphertext: "test-cipher"}, []service.NewAPIBindingSave{{Account: a, TokenID: 3, Group: "vip"}}))
			ar := newAccountRepositoryWithSQL(testEntClient(t), integrationDB, nil)
			if mode == "direct" {
				_, e := integrationDB.ExecContext(ctx, `UPDATE accounts SET credentials=jsonb_set(credentials,'{api_key}','"new-key"'::jsonb) WHERE id=$1`, a.ID)
				require.NoError(t, e)
			} else {
				_, e := ar.BulkUpdate(ctx, []int64{a.ID}, service.AccountBulkUpdate{Credentials: map[string]any{"api_key": "new-key"}, Extra: map[string]any{service.UpstreamBillingProbeExtraKey: nil}})
				require.NoError(t, e)
			}
			binding, e := r.GetBinding(ctx, a.ID)
			require.NoError(t, e)
			require.Nil(t, binding)
			current, e := ar.GetByID(ctx, a.ID)
			require.NoError(t, e)
			require.NotContains(t, current.Extra, service.UpstreamBillingProviderExtraKey)
			require.NotContains(t, current.Extra, service.UpstreamBillingProbeExtraKey)
		})
	}
}
