//go:build integration

package repository

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func monitoringBudgetPolicy() service.MonitoringProbeBudgetSettings {
	s := service.DefaultMonitoringProbeBudgetSettings()
	s.Enabled, s.RecoveryReservePercent = true, 0
	s.RequestsPerMinute, s.RequestsPerDay = 1000, 10000
	s.SiteRequestsPerMinute, s.KeyRequestsPerMinute = 1000, 1000
	s.Concurrency, s.SiteConcurrency, s.KeyConcurrency = 100, 100, 100
	return s
}

func TestQualityJevMonitoringBudgetPostgresAndHTTP(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		if req.URL.Path != "/v1/systemone" || req.Header.Get("Authorization") != "Bearer jev-budget-fixture" {
			http.Error(w, "invalid fixture request", 400)
			return
		}
		_, _ = w.Write([]byte(`{"model":"jev-latest","answers":{"quality":{"type":"choice","choice":"correct","confidence":0.95,"probabilities":{"correct":0.96,"incorrect":0.02,"unknown":0.02}}}}`))
	}))
	defer server.Close()
	settings := NewSettingRepository(client)
	require.NoError(t, settings.Set(ctx, service.SettingKeyContentModerationConfig, fmt.Sprintf(`{"enabled":true,"mode":"observe","typesafe":{"base_url":%q,"model":"jev-latest","api_keys":["jev-budget-fixture"],"timeout_ms":5000,"retry_count":1}}`, server.URL)))
	require.NoError(t, settings.Set(ctx, service.SettingKeyRiskControlEnabled, "true"))
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.AllowInsecureHTTP, cfg.Security.URLAllowlist.AllowPrivateHosts = true, true
	tests := service.NewAccountTestService(nil, nil, nil, nil, nil, nil, cfg, nil)
	audit := service.NewContentModerationService(settings, nil, nil, nil, nil, nil, nil, nil)
	budget := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.RequestsPerDay = 1
	policy, err := budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	first := service.NewQualityJudgeService(nil, nil, nil, tests, audit, budget)
	second := service.NewQualityJudgeService(nil, nil, nil, tests, audit, NewMonitoringProbeBudgetRepository(db))
	question := &service.PelicanTestConfig{QuestionKind: "candy", Prompt: "compare fixture", Quality: &service.QualityPolicy{ExpectedAnswer: "21", Judge: &service.QualityJudgeConfig{Kind: "jev", Prompt: "compare values"}}}
	got := first.Judge(ctx, 1, question, "21")
	require.Equal(t, "correct", got.Verdict, "real Jev request must complete before exhausting quota: %+v", got)
	require.EqualValues(t, 1, calls.Load())
	got = second.Judge(ctx, 1, question, "21")
	require.Equal(t, "unknown", got.Verdict)
	require.Equal(t, "judge_budget_deferred", got.Reason)
	require.NotNil(t, got.BudgetDeferral)
	require.Equal(t, "global_day", got.BudgetDeferral.Scope)
	require.EqualValues(t, 1, calls.Load(), "another service instance must not send an over-budget request")
	usage, err := budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, usage.DayUsed)
	require.Zero(t, usage.ActiveRequests)
	// Disabling enforcement does not disable shared accounting or mutate the
	// moderation pool. A second real request is now admitted and still counted.
	policy.Enabled = false
	_, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	got = second.Judge(ctx, 1, question, "21")
	require.Equal(t, "correct", got.Verdict)
	require.Nil(t, got.BudgetDeferral)
	require.EqualValues(t, 2, calls.Load())
	usage, err = budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 2, usage.DayUsed)
	require.Zero(t, usage.ActiveRequests)
}

func TestMonitoringProbeBudgetPostgresLimitsAndLifecycle(t *testing.T) {
	db, _ := newBillingAuditDatabase(t)
	ctx := t.Context()
	repo := NewMonitoringProbeBudgetRepository(db)
	reset := func(t *testing.T, settings service.MonitoringProbeBudgetSettings) {
		t.Helper()
		_, err := db.ExecContext(ctx, `TRUNCATE monitoring_probe_budget_counters,monitoring_probe_budget_leases; DELETE FROM settings WHERE key='monitoring_probe_budget'`)
		require.NoError(t, err)
		_, err = repo.SaveSettings(ctx, settings)
		require.NoError(t, err)
	}
	request := service.MonitoringProbeBudgetRequest{SiteHash: "fixture-site", KeyHashes: []string{"fixture-key", "fixture-key"}}
	t.Run("migration-replay", func(t *testing.T) {
		ddl, err := os.ReadFile("../../migrations/343_monitoring_probe_budget.sql")
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, string(ddl))
		require.NoError(t, err)
	})
	t.Run("policy-cas", func(t *testing.T) {
		reset(t, monitoringBudgetPolicy())
		peer := NewMonitoringProbeBudgetRepository(db)
		stale, err := peer.GetSettings(ctx)
		require.NoError(t, err)
		updated, err := repo.SaveSettings(ctx, stale)
		require.NoError(t, err)
		require.Equal(t, stale.Revision+1, updated.Revision)
		_, err = peer.SaveSettings(ctx, stale)
		require.ErrorIs(t, err, service.ErrMonitoringProbeBudgetConflict)
		got, err := peer.GetSettings(ctx)
		require.NoError(t, err)
		require.Equal(t, updated, got)
	})
	for _, dimension := range []string{"global_minute", "global_day", "site_minute", "key_minute"} {
		t.Run(dimension, func(t *testing.T) {
			settings := monitoringBudgetPolicy()
			switch dimension {
			case "global_minute":
				settings.RequestsPerMinute = 2
			case "global_day":
				settings.RequestsPerDay = 2
			case "site_minute":
				settings.SiteRequestsPerMinute = 2
			case "key_minute":
				settings.KeyRequestsPerMinute = 2
			}
			reset(t, settings)
			for range 2 {
				id, err := repo.Acquire(ctx, request)
				require.NoError(t, err)
				require.NoError(t, repo.Release(ctx, id))
			}
			_, err := NewMonitoringProbeBudgetRepository(db).Acquire(ctx, request)
			var denial *service.MonitoringProbeBudgetDenial
			require.ErrorAs(t, err, &denial)
			require.Equal(t, dimension, denial.Scope)
			require.Equal(t, "request_limit", denial.Reason)
			require.True(t, denial.RetryAt.After(time.Now()))
			summary, err := repo.Summary(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 2, summary.MinuteUsed)
			require.EqualValues(t, 2, summary.DayUsed)
			require.Zero(t, summary.ActiveRequests)
			if dimension == "key_minute" || dimension == "site_minute" {
				_, err = repo.Acquire(ctx, service.MonitoringProbeBudgetRequest{SiteHash: "other-site", KeyHashes: []string{"other-key"}})
				require.NoError(t, err)
			}
		})
	}
	t.Run("recovery-reserve-and-hard-cap", func(t *testing.T) {
		settings := monitoringBudgetPolicy()
		settings.RequestsPerMinute = 5
		settings.RecoveryReservePercent = 40
		reset(t, settings)
		for range 3 {
			id, err := repo.Acquire(ctx, request)
			require.NoError(t, err)
			require.NoError(t, repo.Release(ctx, id))
		}
		_, err := repo.Acquire(ctx, request)
		var denial *service.MonitoringProbeBudgetDenial
		require.ErrorAs(t, err, &denial)
		require.Equal(t, "recovery_reserve", denial.Reason)
		recovery := request
		recovery.Recovery = true
		for range 2 {
			id, err := repo.Acquire(ctx, recovery)
			require.NoError(t, err)
			require.NoError(t, repo.Release(ctx, id))
		}
		_, err = repo.Acquire(ctx, recovery)
		require.ErrorAs(t, err, &denial)
		require.Equal(t, "request_limit", denial.Reason)
		summary, err := repo.Summary(ctx)
		require.NoError(t, err)
		require.EqualValues(t, 5, summary.MinuteUsed)
		require.EqualValues(t, 3, summary.MinuteRegular)
	})
	for _, scope := range []string{"global", "site", "key", "reserve"} {
		t.Run("concurrency-"+scope, func(t *testing.T) {
			settings := monitoringBudgetPolicy()
			switch scope {
			case "global":
				settings.Concurrency = 2
			case "site":
				settings.SiteConcurrency = 2
			case "key":
				settings.KeyConcurrency = 2
			case "reserve":
				settings.Concurrency = 4
				settings.RecoveryReservePercent = 50
			}
			reset(t, settings)
			first, err := repo.Acquire(ctx, request)
			require.NoError(t, err)
			second, err := repo.Acquire(ctx, request)
			require.NoError(t, err)
			_, err = repo.Acquire(ctx, request)
			var denial *service.MonitoringProbeBudgetDenial
			require.ErrorAs(t, err, &denial)
			if scope == "reserve" {
				require.Equal(t, "recovery_reserve", denial.Reason)
				recovery := request
				recovery.Recovery = true
				id, err := repo.Acquire(ctx, recovery)
				require.NoError(t, err)
				require.NoError(t, repo.Release(ctx, id))
			} else {
				require.Equal(t, scope, denial.Scope)
			}
			require.NoError(t, repo.Release(ctx, first))
			require.NoError(t, repo.Release(ctx, second))
			next, err := repo.Acquire(ctx, request)
			require.NoError(t, err)
			renewed, err := repo.Renew(ctx, next)
			require.NoError(t, err)
			require.True(t, renewed)
			_, err = db.ExecContext(ctx, `UPDATE monitoring_probe_budget_leases SET expires_at=clock_timestamp()-INTERVAL '1 second' WHERE id=$1`, next)
			require.NoError(t, err)
			renewed, err = repo.Renew(ctx, next)
			require.NoError(t, err)
			require.False(t, renewed, "expired ownership must not be resurrected")
			_, err = repo.Acquire(ctx, request)
			require.NoError(t, err)
		})
	}
	t.Run("multiple-nodes-do-not-oversubscribe", func(t *testing.T) {
		settings := monitoringBudgetPolicy()
		settings.Concurrency = 4
		reset(t, settings)
		var admitted atomic.Int64
		var group sync.WaitGroup
		leases := make(chan string, 24)
		for range 24 {
			group.Go(func() {
				id, err := NewMonitoringProbeBudgetRepository(db).Acquire(ctx, request)
				if err == nil {
					admitted.Add(1)
					leases <- id
					return
				}
				var denial *service.MonitoringProbeBudgetDenial
				if !errors.As(err, &denial) {
					t.Errorf("unexpected admission error: %v", err)
				}
			})
		}
		group.Wait()
		close(leases)
		require.EqualValues(t, 4, admitted.Load())
		summary, err := repo.Summary(ctx)
		require.NoError(t, err)
		require.Equal(t, 4, summary.ActiveRequests)
		for id := range leases {
			require.NoError(t, repo.Release(ctx, id))
		}
		summary, err = repo.Summary(ctx)
		require.NoError(t, err)
		require.Zero(t, summary.ActiveRequests)
	})
	t.Run("single-slot-keeps-ordinary-probes", func(t *testing.T) {
		settings := monitoringBudgetPolicy()
		settings.Concurrency, settings.RequestsPerMinute, settings.RecoveryReservePercent = 1, 1, 20
		reset(t, settings)
		_, err := repo.Acquire(ctx, request)
		require.NoError(t, err)
	})
	t.Run("observe-only-retains-accounting", func(t *testing.T) {
		settings := monitoringBudgetPolicy()
		settings.Enabled = false
		settings.RequestsPerMinute, settings.Concurrency = 1, 1
		reset(t, settings)
		for range 3 {
			_, err := repo.Acquire(ctx, request)
			require.NoError(t, err)
		}
		summary, err := repo.Summary(ctx)
		require.NoError(t, err)
		require.EqualValues(t, 3, summary.MinuteUsed)
		require.Equal(t, 3, summary.ActiveRequests)
	})
	t.Run("coordination-failure-does-not-admit", func(t *testing.T) {
		reset(t, monitoringBudgetPolicy())
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		_, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, monitoringProbeBudgetLockID)
		require.NoError(t, err)
		limited, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		_, err = repo.Acquire(limited, request)
		require.Error(t, err)
		require.NoError(t, tx.Rollback())
		summary, err := repo.Summary(ctx)
		require.NoError(t, err)
		require.Zero(t, summary.MinuteUsed)
		_, err = repo.Acquire(ctx, request)
		require.NoError(t, err)
	})
}

func TestMonitoringProbeBudgetDeferralRejectsConcurrentEdits(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	repo := newAccountRepositoryWithSQL(client, db, nil)
	for _, change := range []struct {
		name  string
		patch string
	}{
		{"disabled", `{"upstream_billing_probe_enabled":false}`},
		{"site", `{"upstream_billing_site":"https://other.example"}`},
		{"user", `{"upstream_billing_user_id":8}`},
		{"authorization_rotation", `{}`},
	} {
		t.Run(change.name, func(t *testing.T) {
			account := mustCreateAccount(t, client, &service.Account{
				Name: "budget-stale-" + change.name, Platform: "openai", Type: "apikey", Status: service.StatusActive, Concurrency: 1,
				Credentials: map[string]any{"api_key": "fixture-key", "base_url": "https://example.com"},
				Extra: map[string]any{service.UpstreamBillingProbeEnabledExtraKey: true,
					"upstream_billing_provider": "new_api", "upstream_billing_site": "https://example.com", "upstream_billing_user_id": 7},
			})
			before, err := repo.GetByID(t.Context(), account.ID)
			require.NoError(t, err)
			// Authorization rotation also advances the account row revision when
			// the site, user and absent observation otherwise remain identical.
			_, err = db.ExecContext(t.Context(), `UPDATE accounts SET extra=extra||$1::jsonb,updated_at=clock_timestamp() WHERE id=$2`, change.patch, account.ID)
			require.NoError(t, err)
			denial := &service.MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "global_minute", RetryAt: time.Now().UTC().Add(time.Minute)}
			require.ErrorIs(t, repo.DeferUpstreamBillingProbe(t.Context(), before, denial), service.ErrUpstreamBillingProbeIdentityChanged)
			current, err := repo.GetByID(t.Context(), account.ID)
			require.NoError(t, err)
			require.Nil(t, current.Extra[service.UpstreamBillingProbeDeferralExtraKey])
			require.NoError(t, repo.DeferUpstreamBillingProbe(t.Context(), current, denial), "a new observation may use the current configuration")
		})
	}
}

func TestMonitoringProbeBudgetPhysicalBillingDeferralAndRecovery(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/v1/usage" {
			_, _ = w.Write([]byte(`{"mode":"unrestricted","isValid":true,"remaining":12.34,"unit":"USD","balance":12.34}`))
			return
		}
		_, _ = fmt.Fprintf(w, `{"object":"sub2api.key_billing","schema_version":1,"billing_scope":"token","group_rate_multiplier":0.03,"resolved_rate_multiplier":0.03,"peak_rate_enabled":false,"effective_rate_multiplier":0.03,"observed_at":%q}`, time.Now().UTC().Format(time.RFC3339Nano))
	}))
	defer server.Close()
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true}}}
	upstream := NewHTTPUpstream(cfg)
	accounts := newAccountRepositoryWithSQL(client, db, nil)
	old := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	account := mustCreateAccount(t, client, &service.Account{Name: "budget-physical", Platform: "openai", Type: "apikey", Status: service.StatusActive, Concurrency: 4,
		Credentials: map[string]any{"api_key": "fixture-upstream-secret", "base_url": server.URL},
		Extra:       map[string]any{service.UpstreamBillingProbeEnabledExtraKey: true, service.UpstreamBillingProbeExtraKey: map[string]any{"status": "failed", "failure_count": 3, "last_attempt_at": old, "next_probe_at": old}}})
	budgetRepo := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.RequestsPerMinute = 1
	policy, err := budgetRepo.SaveSettings(t.Context(), policy)
	require.NoError(t, err)
	run := func() *service.UpstreamBillingProbeService {
		tester := service.NewAccountTestService(accounts, nil, nil, nil, nil, upstream, cfg, nil)
		runner := service.NewUpstreamBillingProbeService(accounts, tester, service.NewSettingService(NewSettingRepository(client), cfg))
		runner.SetMonitoringBudget(service.NewMonitoringProbeBudgetService(NewMonitoringProbeBudgetRepository(db)))
		return runner
	}
	before, err := accounts.GetByID(t.Context(), account.ID)
	require.NoError(t, err)
	_, err = run().ProbeAccount(t.Context(), account.ID)
	var denial *service.MonitoringProbeBudgetDenial
	require.ErrorAs(t, err, &denial)
	require.EqualValues(t, 1, calls.Load(), "balance is a separate physical request")
	deferred, err := accounts.GetByID(t.Context(), account.ID)
	require.NoError(t, err)
	require.Equal(t, before.Extra[service.UpstreamBillingProbeExtraKey], deferred.Extra[service.UpstreamBillingProbeExtraKey])
	require.NotNil(t, deferred.Extra[service.UpstreamBillingProbeDeferralExtraKey])
	require.NoError(t, run().RunDue(t.Context()))
	require.EqualValues(t, 1, calls.Load(), "restart honors persisted deferral")
	policy.RequestsPerMinute = 100
	_, err = budgetRepo.SaveSettings(t.Context(), policy)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `UPDATE accounts SET extra=jsonb_set(extra,'{upstream_billing_probe_deferral,retry_at}',to_jsonb($1::text)) WHERE id=$2`, old.Format(time.RFC3339Nano), account.ID)
	require.NoError(t, err)
	require.NoError(t, run().RunDue(t.Context()))
	require.EqualValues(t, 3, calls.Load())
	restored, err := accounts.GetByID(t.Context(), account.ID)
	require.NoError(t, err)
	require.Nil(t, restored.Extra[service.UpstreamBillingProbeDeferralExtraKey])
	require.Equal(t, "ok", restored.Extra[service.UpstreamBillingProbeExtraKey].(map[string]any)["status"])
	require.ErrorIs(t, accounts.DeferUpstreamBillingProbe(t.Context(), before, denial), service.ErrUpstreamBillingProbeIdentityChanged, "late deferral cannot overwrite a newer snapshot")
	summary, err := budgetRepo.Summary(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 3, summary.MinuteUsed)
	require.Zero(t, summary.ActiveRequests)
}

func TestMonitoringProbeBudgetNewAPIPhysicalPaginationRecovery(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body := "{}"
		switch r.Method + " " + r.URL.RequestURI() {
		case "GET /api/status":
			body = `{"success":true,"data":{"quota_per_unit":500000}}`
		case "GET /api/user/self":
			body = `{"success":true,"data":{"id":7,"quota":1250000}}`
		case "GET /api/pricing":
			body = `{"success":true,"group_ratio":{"vip":0.5}}`
		case "GET /api/token/?p=0&page_size=100":
			body = `{"success":true,"data":{"items":[{"id":2,"key":"foreign","group":"vip"}],"total":2,"page":0,"page_size":1}}`
		case "GET /api/token/?p=1&page_size=100":
			body = `{"success":true,"data":{"items":[{"id":3,"key":"abcd**********wxyz","group":"vip"}],"total":2,"page":1,"page_size":1}}`
		case "POST /api/token/3/key":
			body = `{"success":true,"data":{"key":"sk-abcd123456789wxyz"}}`
		default:
			t.Errorf("unexpected request: %s", r.URL.RequestURI())
		}
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true}}, Totp: config.TotpConfig{EncryptionKey: strings.Repeat("42", 32)}}
	accounts := newAccountRepositoryWithSQL(client, db, nil)
	account := mustCreateAccount(t, client, &service.Account{Name: "budget-new-api", Platform: "openai", Type: "apikey", Status: service.StatusActive, Concurrency: 4, Credentials: map[string]any{"api_key": "sk-abcd123456789wxyz", "base_url": server.URL}})
	upstream := NewHTTPUpstream(cfg)
	runner := service.NewUpstreamBillingProbeService(accounts, service.NewAccountTestService(accounts, nil, nil, nil, nil, upstream, cfg, nil), service.NewSettingService(NewSettingRepository(client), cfg))
	enc, err := NewAESEncryptor(cfg)
	require.NoError(t, err)
	runner.SetNewAPIAuthorization(NewNewAPIAuthorizationRepository(db), enc, true)
	budget := NewMonitoringProbeBudgetRepository(db)
	runner.SetMonitoringBudget(service.NewMonitoringProbeBudgetService(budget))
	_, err = runner.SaveNewAPIConfig(t.Context(), account.ID, service.NewAPIConfigRequest{UserID: 7, AccessToken: "fixture-management-secret", AccountIDs: []int64{account.ID}})
	require.NoError(t, err)
	summary, err := budget.Summary(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 6, summary.MinuteUsed, "previews count every management request and page")
	_, err = db.ExecContext(t.Context(), `TRUNCATE monitoring_probe_budget_counters,monitoring_probe_budget_leases`)
	require.NoError(t, err)
	calls.Store(0)
	policy := monitoringBudgetPolicy()
	policy.RequestsPerMinute = 4
	policy, err = budget.SaveSettings(t.Context(), policy)
	require.NoError(t, err)
	before, err := accounts.GetByID(t.Context(), account.ID)
	require.NoError(t, err)
	_, err = runner.ProbeAccount(t.Context(), account.ID)
	var denial *service.MonitoringProbeBudgetDenial
	require.ErrorAs(t, err, &denial)
	require.EqualValues(t, 4, calls.Load())
	deferred, err := accounts.GetByID(t.Context(), account.ID)
	require.NoError(t, err)
	require.Equal(t, before.Extra[service.UpstreamBillingProbeExtraKey], deferred.Extra[service.UpstreamBillingProbeExtraKey])
	require.NotNil(t, deferred.Extra[service.UpstreamBillingProbeDeferralExtraKey])
	policy.RequestsPerMinute = 100
	_, err = budget.SaveSettings(t.Context(), policy)
	require.NoError(t, err)
	snapshot, err := runner.ProbeAccount(t.Context(), account.ID)
	require.NoError(t, err)
	require.Equal(t, "ok", snapshot.Status)
	require.EqualValues(t, 10, calls.Load())
	restored, err := accounts.GetByID(t.Context(), account.ID)
	require.NoError(t, err)
	require.Nil(t, restored.Extra[service.UpstreamBillingProbeDeferralExtraKey])
	summary, err = budget.Summary(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 10, summary.MinuteUsed)
	require.Zero(t, summary.ActiveRequests)
}
