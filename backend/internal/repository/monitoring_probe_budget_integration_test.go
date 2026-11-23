//go:build integration

package repository

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	httppool "github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
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

type ollamaBudgetFixtureEncryptor struct{}

func (ollamaBudgetFixtureEncryptor) Encrypt(v string) (string, error) { return v, nil }
func (ollamaBudgetFixtureEncryptor) Decrypt(v string) (string, error) { return v, nil }

func monitoringBudgetTestUpstream(t *testing.T, handler http.HandlerFunc) service.HTTPUpstream {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	return monitoringBudgetPinnedUpstream(t, server.Listener.Addr().String(), server.Certificate().Raw)
}

func monitoringBudgetPinnedUpstream(t *testing.T, address string, certificate []byte) service.HTTPUpstream {
	t.Helper()
	cert, err := x509.ParseCertificate(certificate)
	require.NoError(t, err)
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "example.com", MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, address)
		}}
	t.Cleanup(transport.CloseIdleConnections)
	httpClient := &http.Client{Transport: &accountResponseTransport{base: transport}, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return gatewayPinDelegate{call: func(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		return httpClient.Do(req)
	}}
}

func TestOllamaCloudUsageMonitoringBudgetHTTPPostgresSessionAndRecovery(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	var calls atomic.Int32
	body, err := os.ReadFile("../service/testdata/ollama_settings_usage.html")
	require.NoError(t, err)
	upstream := monitoringBudgetTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/settings" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") == "" {
			http.Error(w, "unexpected settings request", http.StatusBadRequest)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(body)
	})
	create := func(name, cookie string) *service.Account {
		return mustCreateAccount(t, client, &service.Account{Name: name, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
			Status: service.StatusActive, Schedulable: true, Concurrency: 2, Credentials: map[string]any{"api_key": name, "base_url": "https://ollama.com"},
			Extra: map[string]any{service.OllamaCloudUsageSessionExtraKey: cookie, service.OllamaCloudUsageAutoRefreshExtraKey: true}})
	}
	first := create("inference-key-one", "session=second; wos-session=shared")
	alias := create("different-inference-key", "wos-session=shared; session=second")
	independent := create("third-inference-key", "wos-session=independent")
	budget := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.KeyRequestsPerMinute = 1
	policy, err = budget.SaveSettings(t.Context(), policy)
	require.NoError(t, err)
	row := func(id int64) string {
		var raw string
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, id).Scan(&raw))
		return raw
	}
	aliasBefore := row(alias.ID)
	settingsRepo := NewSettingRepository(client)
	require.NoError(t, settingsRepo.Set(t.Context(), service.SettingKeyOllamaCloudUsageSettings, `{"enabled":true,"interval_minutes":60,"debounce_minutes":1}`))
	accounts := newAccountRepositoryWithSQL(client, db, nil)
	runtimeRunner := service.NewOllamaCloudUsageService(accounts, upstream, service.NewSettingService(settingsRepo, nil), ollamaBudgetFixtureEncryptor{}, true)
	t.Cleanup(runtimeRunner.Stop)
	runtimeBudget := service.NewMonitoringProbeBudgetService(budget)
	runtimeBudget.SetOllamaUsage(runtimeRunner)
	runtimeRunner.SetMonitoringBudget(runtimeBudget)
	readRuntime := func() *service.OllamaMonitoringRuntime {
		t.Helper()
		before := calls.Load()
		preview, previewErr := runtimeBudget.PreviewCoverage(t.Context(), policy)
		require.NoError(t, previewErr)
		summary, summaryErr := runtimeBudget.Summary(t.Context())
		require.NoError(t, summaryErr)
		require.Equal(t, preview.OllamaUsage.InstanceID, summary.Coverage.OllamaUsage.InstanceID)
		require.Equal(t, preview.OllamaUsage.LastScan, summary.Coverage.OllamaUsage.LastScan)
		require.Equal(t, before, calls.Load(), "read-only API projection must not make a supplier request")
		require.EqualValues(t, before, summary.DayUsed)
		require.Zero(t, summary.ActiveRequests)
		raw, marshalErr := json.Marshal(preview.OllamaUsage)
		require.NoError(t, marshalErr)
		require.NotContains(t, string(raw), "wos-session")
		require.NotContains(t, string(raw), "inference-key")
		require.Contains(t, preview.UnestimatedDemandSources, "ollama_usage")
		return preview.OllamaUsage
	}
	require.Nil(t, readRuntime().LastScan, "constructing or reading the runner is not a due inventory scan")
	run := func(id int64) service.OllamaCloudUsageState {
		t.Helper()
		accounts := newAccountRepositoryWithSQL(client, db, nil)
		svc := service.NewOllamaCloudUsageService(accounts, upstream, service.NewSettingService(NewSettingRepository(client), nil), ollamaBudgetFixtureEncryptor{}, true)
		defer svc.Stop()
		svc.SetMonitoringBudget(service.NewMonitoringProbeBudgetService(NewMonitoringProbeBudgetRepository(db)))
		state, refreshErr := svc.Refresh(t.Context(), id)
		require.NoError(t, refreshErr)
		require.NotNil(t, state)
		raw, marshalErr := json.Marshal(state)
		require.NoError(t, marshalErr)
		require.NotContains(t, string(raw), "wos-session")
		require.NotContains(t, string(raw), "inference-key")
		return *state
	}
	result := run(first.ID)
	require.Nil(t, result.BudgetDeferral)
	require.Equal(t, service.OllamaCloudUsageStatusOK, result.Snapshot.Status)
	result = run(alias.ID)
	require.NotNil(t, result.BudgetDeferral)
	require.Equal(t, "request_limit", result.BudgetDeferral.Reason)
	require.Equal(t, "key_minute", result.BudgetDeferral.Scope)
	require.Nil(t, result.Snapshot)
	require.Equal(t, aliasBefore, row(alias.ID), "a different inference key must not bypass the final web session's limit or publish a failed snapshot")
	require.EqualValues(t, 1, calls.Load())
	result = run(independent.ID)
	require.Nil(t, result.BudgetDeferral)
	require.Equal(t, service.OllamaCloudUsageStatusOK, result.Snapshot.Status)
	policy.KeyRequestsPerMinute = 3
	policy, err = budget.SaveSettings(t.Context(), policy)
	require.NoError(t, err)
	result = run(alias.ID)
	require.Nil(t, result.BudgetDeferral)
	require.Equal(t, service.OllamaCloudUsageStatusOK, result.Snapshot.Status)
	require.NotEqual(t, aliasBefore, row(alias.ID))
	require.EqualValues(t, 3, calls.Load())
	// All three real SQL rows are now fresh and idle; a successful due scan is
	// empty, but says nothing about the total configured inventory or events.
	require.NoError(t, runtimeRunner.RunDue(t.Context()))
	scan := readRuntime().LastScan
	require.Equal(t, "observed", scan.State)
	require.Zero(t, *scan.Candidates)
	require.Zero(t, *scan.DueGroups)
	require.False(t, *scan.LimitReached)
	for range 3 {
		require.Equal(t, scan.ObservedAt, readRuntime().LastScan.ObservedAt)
	}
	runtimeRunner.Stop()
	require.Equal(t, "stopped", readRuntime().State)
	rebuilt := service.NewOllamaCloudUsageService(accounts, upstream, service.NewSettingService(settingsRepo, nil), ollamaBudgetFixtureEncryptor{}, true)
	t.Cleanup(rebuilt.Stop)
	runtimeBudget.SetOllamaUsage(rebuilt)
	restarted := readRuntime()
	require.NotEqual(t, runtimeRunner.MonitoringRuntime().InstanceID, restarted.InstanceID)
	require.Nil(t, restarted.LastScan, "process restart must not inherit a prior scan as fresh evidence")
	summary, err := budget.Summary(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 3, summary.DayUsed)
	require.Zero(t, summary.DayTokens)
	require.Zero(t, summary.UsageUnknown)
	require.Zero(t, summary.UsagePending)
	require.Zero(t, summary.ActiveRequests)
	var downstreamUsage int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM usage_logs`).Scan(&downstreamUsage))
	require.Zero(t, downstreamUsage)
}

// Preflight does not retroactively discard valid legacy policy JSON or invent
// capacity after inventory grows. Reproduce that old writer only in fixtures;
// the current admin save is independently asserted to reject the shortfall.
func persistLegacyMonitoringProbeBudget(t *testing.T, db *sql.DB, policy service.MonitoringProbeBudgetSettings) service.MonitoringProbeBudgetSettings {
	t.Helper()
	_, err := NewMonitoringProbeBudgetRepository(db).SaveSettings(t.Context(), policy)
	require.ErrorContains(t, err, "MONITORING_PROBE_COVERAGE_IMPOSSIBLE")
	policy.Revision++
	raw, err := json.Marshal(policy)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO settings(key,value,updated_at) VALUES($1,$2,NOW())
 ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,updated_at=EXCLUDED.updated_at`, service.MonitoringProbeBudgetSettingKey, string(raw))
	require.NoError(t, err)
	return policy
}

func TestAccountQuotaMonitoringBudgetPostgresOptionalFailureAndRecovery(t *testing.T) {
	for _, platform := range []string{service.PlatformCommandCode, service.PlatformCline, service.PlatformMistral} {
		t.Run(platform, func(t *testing.T) {
			db, client := newBillingAuditDatabase(t)
			var calls atomic.Int32
			responses := map[string]string{}
			allowed, complete := int64(1), int64(2)
			site := "https://api.mistral.ai"
			switch platform {
			case service.PlatformCommandCode:
				site = "https://api.commandcode.ai"
				allowed, complete = 2, 4
				responses = map[string]string{
					"/alpha/whoami":                `{"user":{"userName":"fixture"},"org":null}`,
					"/alpha/billing/credits":       `{"credits":{"belowThreshold":false,"monthlyCredits":35,"purchasedCredits":0,"freeCredits":0},"windowLimits":{"limited":true,"fiveHour":{"used":1,"cap":14},"weekly":{"used":3,"cap":35}}}`,
					"/alpha/billing/subscriptions": `{"data":{"planId":"individual-goat","status":"active","currentPeriodStart":"2026-11-01T00:00:00Z","currentPeriodEnd":"2026-12-01T00:00:00Z"}}`,
					"/alpha/usage/summary":         `{"totalCost":4,"totalMonthlyCredits":35}`,
				}
			case service.PlatformCline:
				site = "https://api.cline.bot"
				complete = 3
				responses = map[string]string{
					"/api/v1/users/me/plan/usage-limits": `{"success":true,"data":{"limits":[{"type":"five_hour","percentUsed":12.5,"resetsAt":"2099-01-01T00:00:00Z"}]}}`,
					"/api/v1/users/me":                   `{"success":true,"data":{"id":"u-fixture"}}`,
					"/api/v1/users/u-fixture/balance":    `{"balance":12340000}`,
				}
			case service.PlatformMistral:
				responses = map[string]string{
					"/v1/billing/budget":       `{"vibe_budget":{"usage_percentage":10,"initial_budget":300,"currency":"USD","reset_at":"2099-01-01T00:00:00Z","payg_enabled":false}}`,
					"/v1/billing/subscription": `{"plan_name":"INDIVIDUAL","vibe_payg_enabled":false}`,
				}
			}
			upstream := monitoringBudgetTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer mstrl_quota_fixture" {
					http.Error(w, "unexpected quota request", http.StatusBadRequest)
					return
				}
				body, ok := responses[r.URL.Path]
				if !ok {
					http.NotFound(w, r)
					return
				}
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, body)
			})
			account := mustCreateAccount(t, client, &service.Account{Name: "quota-account-budget", Platform: platform, Type: service.AccountTypeAPIKey,
				Status: service.StatusActive, Schedulable: true, Concurrency: 2, Credentials: map[string]any{"api_key": "mstrl_quota_fixture", "api_protocol": service.APIProtocolAdaptive}})
			alias := mustCreateAccount(t, client, &service.Account{Name: "quota-account-alias", Platform: platform, Type: account.Type,
				Status: account.Status, Schedulable: true, Concurrency: 2, Credentials: account.Credentials})
			budget := NewMonitoringProbeBudgetRepository(db)
			policy := monitoringBudgetPolicy()
			policy.KeyRequestsPerMinute = allowed
			policy, err := budget.SaveSettings(t.Context(), policy)
			require.NoError(t, err)
			before := func(id int64) string {
				var raw string
				require.NoError(t, db.QueryRowContext(t.Context(), `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, id).Scan(&raw))
				return raw
			}
			accountBefore, aliasBefore := before(account.ID), before(alias.ID)
			run := func(id int64) (*service.CNProviderQuotaProbeResult, error) {
				accounts := newAccountRepositoryWithSQL(client, db, nil)
				ctx := service.NewMonitoringProbeBudgetService(NewMonitoringProbeBudgetRepository(db)).Context(t.Context(), site, false)
				return service.NewCNProviderQuotaService(accounts, nil, upstream, &config.Config{}).QueryUsage(ctx, id)
			}
			result, err := run(account.ID)
			require.Nil(t, result)
			var denial *service.MonitoringProbeBudgetDenial
			require.ErrorAs(t, err, &denial)
			require.Equal(t, "key_minute", denial.Scope)
			require.Equal(t, "request_limit", denial.Reason)
			require.Equal(t, allowed, int64(calls.Load()))
			require.Equal(t, accountBefore, before(account.ID), "an optional local failure must not publish primary snapshots or clear cooldowns")
			result, err = run(alias.ID)
			require.Nil(t, result)
			require.ErrorAs(t, err, &denial)
			require.Equal(t, "key_minute", denial.Scope)
			require.Equal(t, aliasBefore, before(alias.ID))
			require.Equal(t, allowed, int64(calls.Load()), "an alias and fresh adapter must not reset the final credential's allowance")
			policy.KeyRequestsPerMinute = 100
			_, err = budget.SaveSettings(t.Context(), policy)
			require.NoError(t, err)
			result, err = run(alias.ID)
			require.NoError(t, err)
			require.True(t, result.Success)
			require.True(t, result.Persisted)
			require.Equal(t, allowed+complete, int64(calls.Load()))
			stats, err := budget.Summary(t.Context())
			require.NoError(t, err)
			require.Equal(t, allowed+complete, stats.DayUsed)
			require.Zero(t, stats.DayTokens)
			require.Zero(t, stats.UsageUnknown, "quota GETs must not create unknown generation usage")
			require.Zero(t, stats.UsagePending)
			require.Zero(t, stats.ActiveRequests)
		})
	}
}

func TestCNProviderBalanceDeadlineHTTPPostgresEvidenceAndRecovery(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/tenant/user/balance" || r.Header.Get("Authorization") != "Bearer deadline-balance-fixture" {
			http.Error(w, "unexpected balance fixture request", http.StatusBadRequest)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"10"}]}`)
	}))
	t.Cleanup(server.Close)
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true}}}
	accounts := newAccountRepositoryWithSQL(client, db, nil)
	create := func(name string) *service.Account {
		return mustCreateAccount(t, client, &service.Account{Name: name, Platform: service.PlatformDeepseek, Type: service.AccountTypeAPIKey,
			Status: service.StatusActive, Schedulable: true, Concurrency: 2,
			Credentials: map[string]any{"api_key": "deadline-balance-fixture", "base_url": server.URL + "/tenant", "account_mode": service.AccountModePayG},
			Extra:       map[string]any{"deepseek_balance": 2.0, "deepseek_balance_low": true, "deepseek_balance_updated_at": "2026-01-01T00:00:00Z", "manual_note": "preserved"}})
	}
	first, alias := create("deadline-balance"), create("deadline-balance-alias")
	row := func(id int64) string {
		t.Helper()
		var value string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, id).Scan(&value))
		return value
	}
	firstBefore, aliasBefore := row(first.ID), row(alias.ID)
	budget := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.RequestsPerDay = 1
	var err error
	policy, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	upstream := NewHTTPUpstream(cfg)
	native := pr612NativeTransport(t, upstream)
	t.Cleanup(func() {
		native.mu.Lock()
		defer native.mu.Unlock()
		for _, entry := range native.clients {
			entry.client.CloseIdleConnections()
		}
	})
	// Buffer a real, fully settled HTTP reply, then emulate a provider adapter
	// that returns it after the original request deadline instead of an error.
	late := gatewayPinDelegate{call: func(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		response, err := upstream.Do(req, proxy, id, concurrency)
		if err != nil {
			return response, err
		}
		body, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return nil, err
		}
		<-req.Context().Done()
		response.Body = io.NopCloser(strings.NewReader(string(body)))
		return response, nil
	}}
	run := func(id int64, transport service.HTTPUpstream) (*service.CNProviderBalanceResult, error) {
		ctx := service.NewMonitoringProbeBudgetService(NewMonitoringProbeBudgetRepository(db)).Context(ctx, server.URL, false)
		return service.NewCNProviderBalanceService(accounts, nil, transport, cfg).QueryBalance(ctx, id)
	}
	result, err := run(first.ID, late)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, result)
	require.NoError(t, ctx.Err(), "the upstream deadline must not cancel the fixture caller")
	require.Equal(t, firstBefore, row(first.ID), "late success must not replace the old balance, timestamp or low-balance guard")
	require.Equal(t, aliasBefore, row(alias.ID))
	require.EqualValues(t, 1, calls.Load())
	result, err = run(alias.ID, upstream)
	require.Nil(t, result)
	var denial *service.MonitoringProbeBudgetDenial
	require.ErrorAs(t, err, &denial)
	require.Equal(t, "global_day", denial.Scope)
	require.EqualValues(t, 1, calls.Load(), "adapter reconstruction must not refund the timed-out physical call")
	require.Equal(t, aliasBefore, row(alias.ID))
	policy.RequestsPerDay = 2
	_, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	result, err = run(alias.ID, upstream)
	require.NoError(t, err)
	require.True(t, result.Success)
	require.True(t, result.Persisted)
	require.Equal(t, 10.0, result.Balance)
	require.EqualValues(t, 2, calls.Load())
	require.Equal(t, firstBefore, row(first.ID), "recovery belongs to the queried account only")
	fresh, err := accounts.GetByID(ctx, alias.ID)
	require.NoError(t, err)
	require.Equal(t, false, fresh.Extra["deepseek_balance_low"])
	require.Equal(t, "preserved", fresh.Extra["manual_note"])
	summary, err := budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 2, summary.DayUsed)
	require.Zero(t, summary.DayTokens)
	require.Zero(t, summary.ActiveRequests)
	require.Zero(t, summary.UsagePending)
	require.Zero(t, summary.UsageUnknown)
	require.Zero(t, summary.UsageReported)
	var usage int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM usage_logs`).Scan(&usage))
	require.Zero(t, usage)
}

func TestCodexQuotaRequestDeadlineHTTPPostgresEvidenceAndRecovery(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/backend-api/codex/responses" || r.Host != "chatgpt.com" || r.Header.Get("Authorization") != "Bearer codex-deadline-fixture" {
			http.Error(w, "unexpected codex quota fixture request", http.StatusBadRequest)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("x-codex-primary-used-percent", "0")
		w.Header().Set("x-codex-primary-window-minutes", "300")
		w.Header().Set("x-codex-primary-reset-after-seconds", "18000")
		w.Header().Set("x-codex-secondary-used-percent", "0")
		w.Header().Set("x-codex-secondary-window-minutes", "10080")
		w.Header().Set("x-codex-secondary-reset-after-seconds", "604800")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":4,\"output_tokens\":3,\"total_tokens\":7}}}\n\n")
	}))
	t.Cleanup(server.Close)
	limited, reset := time.Now().UTC().Add(-time.Minute), time.Now().UTC().Add(time.Hour)
	account := mustCreateAccount(t, client, &service.Account{Name: "codex-quota-deadline", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Status: service.StatusActive, Schedulable: true, Concurrency: 2, RateLimitedAt: &limited, RateLimitResetAt: &reset,
		Credentials: map[string]any{"access_token": "codex-deadline-fixture", "chatgpt_account_id": "deadline-fixture"},
		Extra:       map[string]any{"codex_5h_used_percent": 77.0, "codex_usage_updated_at": "2026-01-01T00:00:00Z", "manual_note": "preserved"}})
	row := func() string {
		t.Helper()
		var raw string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, account.ID).Scan(&raw))
		return raw
	}
	before := row()
	host, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	proxy := &service.Proxy{Protocol: "http", Host: host, Port: port}
	httpClient, err := httppool.GetClient(httppool.Options{ProxyURL: proxy.URL(), Timeout: 15 * time.Second, ResponseHeaderTimeout: 10 * time.Second})
	require.NoError(t, err)
	baseURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	original := httpClient.Transport
	var delay atomic.Bool
	delay.Store(true)
	httpClient.Transport = githubReleaseRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		copy := req.Clone(req.Context())
		copy.URL.Scheme, copy.URL.Host = baseURL.Scheme, baseURL.Host
		response, err := http.DefaultTransport.RoundTrip(copy)
		if err != nil {
			return response, err
		}
		body, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return nil, err
		}
		if delay.Load() {
			<-req.Context().Done()
		}
		response.Body, response.Request = io.NopCloser(strings.NewReader(string(body))), req
		return response, nil
	})
	t.Cleanup(func() { httpClient.Transport = original; httppool.EvictProxyClients(proxy.URL()) })
	budget := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.RequestsPerDay = 1
	policy, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	accounts := newAccountRepositoryWithSQL(client, db, nil)
	run := func() *service.UsageInfo {
		t.Helper()
		account, err := accounts.GetByID(ctx, account.ID)
		require.NoError(t, err)
		proxyID := int64(1)
		account.ProxyID, account.Proxy = &proxyID, proxy
		usageService := service.NewAccountUsageService(accounts, nil, nil, nil, nil, nil, nil, nil, service.NewUsageCache(), nil, nil)
		callCtx := service.NewMonitoringProbeBudgetService(NewMonitoringProbeBudgetRepository(db)).Context(ctx, "https://chatgpt.com", false)
		usage, err := usageService.GetUsageForAccount(callCtx, account, true)
		require.NoError(t, err, "upstream timeout keeps the existing usage fallback, not a fabricated fresh snapshot")
		return usage
	}
	usage := run()
	require.NotNil(t, usage.FiveHour)
	require.Equal(t, 77.0, usage.FiveHour.Utilization)
	require.Equal(t, before, row(), "late zero-usage headers must not renew evidence or clear the original cooldown")
	summary, err := budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, summary.DayUsed)
	require.EqualValues(t, 7, summary.DayTokens, "the complete returned usage is charged even when its header evidence expires")
	require.Zero(t, summary.ActiveRequests)
	require.Zero(t, summary.UsagePending)
	require.Zero(t, summary.UsageUnknown)
	delay.Store(false)
	policy.RequestsPerDay = 2
	_, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	usage = run()
	require.Equal(t, 0.0, usage.FiveHour.Utilization)
	require.Eventually(t, func() bool {
		fresh, err := accounts.GetByID(ctx, account.ID)
		return err == nil && fresh.RateLimitedAt == nil && fresh.RateLimitResetAt == nil && fresh.Extra["codex_5h_used_percent"] == 0.0
	}, 5*time.Second, 5*time.Millisecond, "fresh in-deadline response must persist and recover only the observed cooldown")
	summary, err = budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 2, calls.Load())
	require.EqualValues(t, 2, summary.DayUsed)
	require.EqualValues(t, 14, summary.DayTokens)
	require.EqualValues(t, 2, summary.UsageReported)
	require.Zero(t, summary.UsagePending)
	require.Zero(t, summary.UsageUnknown)
	require.Zero(t, summary.ActiveRequests)
	var usageLogs int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM usage_logs`).Scan(&usageLogs))
	require.Zero(t, usageLogs)
}

func TestResponsesSupportMonitoringBudgetPostgresSharedKeyAndRecovery(t *testing.T) {
	for _, scope := range []string{"global_day", "key_tokens", "key_requests"} {
		t.Run(scope, func(t *testing.T) {
			db, client := newBillingAuditDatabase(t)
			var calls, usageMode atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/tenant/v1/responses" {
					http.Error(w, "unexpected capability request", http.StatusBadRequest)
					return
				}
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				usage := `,"usage":{"input_tokens":4,"input_tokens_details":{"cached_tokens":3},"output_tokens":3,"total_tokens":7}`
				switch usageMode.Load() {
				case 1:
					usage = ""
				case 2:
					usage = `,"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}`
				}
				_, _ = fmt.Fprintf(w, `{"status":"completed","output":[{"type":"function_call","name":"probe_ping"}]%s}`, usage)
			}))
			t.Cleanup(server.Close)
			create := func(name, key string) *service.Account {
				return mustCreateAccount(t, client, &service.Account{Name: name, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
					Status: service.StatusActive, Schedulable: true, Concurrency: 4,
					Credentials: map[string]any{"api_key": key, "base_url": server.URL + "/tenant/v1"},
					Extra:       map[string]any{"openai_responses_supported": false, "manual_note": "preserved"}})
			}
			first, alias := create("responses-budget", "responses-budget-fixture"), create("responses-alias", "responses-budget-fixture")
			independent := create("responses-independent", "responses-another-fixture")
			snapshot := func(id int64) string {
				var raw string
				require.NoError(t, db.QueryRowContext(t.Context(), `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, id).Scan(&raw))
				return raw
			}
			aliasBefore := snapshot(alias.ID)
			budget := NewMonitoringProbeBudgetRepository(db)
			policy := monitoringBudgetPolicy()
			switch scope {
			case "global_day":
				policy.TokensPerDay = 5
			case "key_tokens":
				policy.KeyTokensPerMinute = 5
			case "key_requests":
				policy.KeyRequestsPerMinute = 1
			}
			policy, err := budget.SaveSettings(t.Context(), policy)
			require.NoError(t, err)
			cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true}}}
			run := func(id int64) *service.MonitoringProbeBudgetDenial {
				tests := service.NewAccountTestService(newAccountRepositoryWithSQL(client, db, nil), nil, nil, nil, nil, NewHTTPUpstream(cfg), cfg, nil)
				tests.SetMonitoringBudget(service.NewMonitoringProbeBudgetService(NewMonitoringProbeBudgetRepository(db)))
				return tests.ProbeOpenAIAPIKeyResponsesSupport(t.Context(), id)
			}
			require.Nil(t, run(first.ID), "the 512-output-token payload is not a token reservation; only returned usage is charged")
			deferred := run(alias.ID)
			require.NotNil(t, deferred)
			wantScope, wantReason := "key_minute", "token_limit"
			if scope == "global_day" {
				wantScope = "global_day"
			} else if scope == "key_requests" {
				wantReason = "request_limit"
			}
			require.Equal(t, wantScope, deferred.Scope)
			require.Equal(t, wantReason, deferred.Reason)
			require.True(t, deferred.RetryAt.After(time.Now()))
			require.EqualValues(t, 1, calls.Load(), "same final Key must not gain a new allowance through an alias or service restart")
			require.Equal(t, aliasBefore, snapshot(alias.ID), "deferral must not refresh capability or account state")
			if scope == "global_day" {
				require.NotNil(t, run(independent.ID), "global exhaustion also stops a different credential")
			} else {
				require.Nil(t, run(independent.ID), "one credential's hard limit must not stop an independent credential")
			}
			completed := int64(calls.Load())
			stats, err := budget.Summary(t.Context())
			require.NoError(t, err)
			require.Equal(t, completed, stats.DayUsed)
			require.Equal(t, completed*7, stats.DayTokens)
			require.Equal(t, completed, stats.UsageReported)
			require.Zero(t, stats.UsageUnknown)
			require.Zero(t, stats.UsagePending)
			require.Zero(t, stats.ActiveRequests)

			policy.TokensPerDay, policy.KeyTokensPerMinute, policy.KeyRequestsPerMinute = 100, 100, 100
			_, err = budget.SaveSettings(t.Context(), policy)
			require.NoError(t, err)
			require.Nil(t, run(alias.ID))
			aliasAfter, err := newAccountRepositoryWithSQL(client, db, nil).GetByID(t.Context(), alias.ID)
			require.NoError(t, err)
			require.Equal(t, true, aliasAfter.Extra["openai_responses_supported"])
			require.Equal(t, "preserved", aliasAfter.Extra["manual_note"])
			require.True(t, aliasAfter.Schedulable)
			require.Equal(t, service.StatusActive, aliasAfter.Status)
			usageMode.Store(1)
			require.Nil(t, run(alias.ID))
			usageMode.Store(2)
			require.Nil(t, run(alias.ID))
			stats, err = NewMonitoringProbeBudgetRepository(db).Summary(t.Context())
			require.NoError(t, err)
			require.Equal(t, completed+3, stats.DayUsed)
			require.Equal(t, (completed+1)*7, stats.DayTokens, "cache reads are already included in input tokens; absent usage is not an estimated charge")
			require.Equal(t, completed+2, stats.UsageReported, "explicit zero is a complete report")
			require.EqualValues(t, 1, stats.UsageUnknown)
			require.Zero(t, stats.UsagePending)
			require.Zero(t, stats.ActiveRequests)
		})
	}
}

func TestResponsesSupportMonitoringBudgetPostgresInFlightExcess(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	var calls atomic.Int32
	ready := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 2 {
			close(ready)
		}
		select {
		case <-ready:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"completed","output":[{"type":"function_call"}],"usage":{"input_tokens":4,"output_tokens":3,"total_tokens":7}}`)
	}))
	t.Cleanup(server.Close)
	accounts := newAccountRepositoryWithSQL(client, db, nil)
	first := mustCreateAccount(t, client, &service.Account{Name: "responses-concurrent", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, Concurrency: 4, Credentials: map[string]any{"api_key": "responses-shared-fixture", "base_url": server.URL}})
	alias := mustCreateAccount(t, client, &service.Account{Name: "responses-concurrent-alias", Platform: first.Platform, Type: first.Type,
		Status: first.Status, Schedulable: true, Concurrency: 4, Credentials: first.Credentials})
	budget := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.TokensPerDay = 5
	_, err := budget.SaveSettings(t.Context(), policy)
	require.NoError(t, err)
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true}}}
	run := func(id int64) *service.MonitoringProbeBudgetDenial {
		tests := service.NewAccountTestService(accounts, nil, nil, nil, nil, NewHTTPUpstream(cfg), cfg, nil)
		tests.SetMonitoringBudget(service.NewMonitoringProbeBudgetService(NewMonitoringProbeBudgetRepository(db)))
		return tests.ProbeOpenAIAPIKeyResponsesSupport(t.Context(), id)
	}
	results := make(chan *service.MonitoringProbeBudgetDenial, 2)
	go func() { results <- run(first.ID) }()
	go func() { results <- run(alias.ID) }()
	require.Nil(t, <-results)
	require.Nil(t, <-results)
	deferred := run(first.ID)
	require.NotNil(t, deferred)
	require.Equal(t, "token_limit", deferred.Reason)
	require.EqualValues(t, 2, calls.Load())
	stats, err := budget.Summary(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 14, stats.DayTokens, "admitted in-flight calls may exceed the limit; their actual usage must not be clipped")
	require.EqualValues(t, 2, stats.DayUsed)
	require.EqualValues(t, 2, stats.UsageReported)
	require.Zero(t, stats.UsageUnknown)
	require.Zero(t, stats.UsagePending)
	require.Zero(t, stats.ActiveRequests)
}

type responsesProbeCancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b responsesProbeCancelOnCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

func TestResponsesSupportMonitoringBudgetPostgresCancellationAndRecovery(t *testing.T) {
	for _, supported := range []bool{true, false} {
		t.Run(fmt.Sprintf("supported_%t", supported), func(t *testing.T) {
			db, client := newBillingAuditDatabase(t)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
					http.Error(w, "unexpected capability request", http.StatusBadRequest)
					return
				}
				calls.Add(1)
				kind := "reasoning"
				if supported {
					kind = "function_call"
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"status":"completed","output":[{"type":%q}],"usage":{"input_tokens":4,"output_tokens":3,"total_tokens":7}}`, kind)
			}))
			t.Cleanup(server.Close)
			accounts := newAccountRepositoryWithSQL(client, db, nil)
			account := mustCreateAccount(t, client, &service.Account{Name: "responses-cancel-fixture", Platform: service.PlatformOpenAI,
				Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 4,
				Credentials: map[string]any{"api_key": "responses-cancel-fixture", "base_url": server.URL},
				Extra:       map[string]any{"openai_responses_supported": !supported, "manual_note": "preserved"}})
			snapshot := func() string {
				var row string
				require.NoError(t, db.QueryRowContext(t.Context(), `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, account.ID).Scan(&row))
				return row
			}
			before := snapshot()
			budget := NewMonitoringProbeBudgetRepository(db)
			policy, err := budget.SaveSettings(t.Context(), monitoringBudgetPolicy())
			require.NoError(t, err)
			cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true}}}
			upstream := NewHTTPUpstream(cfg)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cancelOnCloseEnabled := true
			cancelOnClose := gatewayPinDelegate{call: func(req *http.Request, proxy string, id int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
				resp, err := upstream.DoWithTLS(req, proxy, id, concurrency, profile)
				if cancelOnCloseEnabled && err == nil && resp != nil && resp.Body != nil {
					resp.Body = responsesProbeCancelOnCloseBody{ReadCloser: resp.Body, cancel: cancel}
				}
				return resp, err
			}}
			tests := service.NewAccountTestService(accounts, nil, nil, nil, nil, cancelOnClose, cfg, nil)
			tests.SetMonitoringBudget(service.NewMonitoringProbeBudgetService(budget))
			runtimeBudget := service.NewMonitoringProbeBudgetService(budget)
			runtimeBudget.SetResponsesCapability(tests)
			readRuntime := func() *service.ResponsesCapabilityMonitoringRuntime {
				t.Helper()
				beforeCalls := calls.Load()
				preview, previewErr := runtimeBudget.PreviewCoverage(t.Context(), policy)
				require.NoError(t, previewErr)
				summary, summaryErr := runtimeBudget.Summary(t.Context())
				require.NoError(t, summaryErr)
				require.Equal(t, preview.ResponsesCapability.InstanceID, summary.Coverage.ResponsesCapability.InstanceID)
				require.Equal(t, preview.ResponsesCapability.LastFinishedAt, summary.Coverage.ResponsesCapability.LastFinishedAt)
				require.Equal(t, beforeCalls, calls.Load(), "preview/summary must not trigger a probe")
				require.EqualValues(t, beforeCalls, summary.DayUsed)
				require.Zero(t, summary.ActiveRequests)
				require.Contains(t, preview.UnestimatedDemandSources, "responses_capability")
				raw, marshalErr := json.Marshal(preview.ResponsesCapability)
				require.NoError(t, marshalErr)
				require.NotContains(t, string(raw), "responses-cancel-fixture")
				require.NotContains(t, string(raw), server.URL)
				return preview.ResponsesCapability
			}
			initialRuntime := readRuntime()
			require.Zero(t, initialRuntime.StartedEvents)
			require.Nil(t, initialRuntime.LastStartedAt)
			require.Nil(t, tests.ProbeOpenAIAPIKeyResponsesSupport(ctx, account.ID), "cancellation is not a supplier or local budget failure")
			require.ErrorIs(t, ctx.Err(), context.Canceled)
			require.Equal(t, before, snapshot(), "neither a late pass nor a late unsupported result may alter the account")
			stats, err := budget.Summary(t.Context())
			require.NoError(t, err)
			require.EqualValues(t, 1, stats.DayUsed)
			require.EqualValues(t, 7, stats.DayTokens, "settled actual usage is retained despite cancellation")
			require.EqualValues(t, 1, stats.UsageReported)
			require.Zero(t, stats.UsageUnknown)
			require.Zero(t, stats.UsagePending)
			require.Zero(t, stats.ActiveRequests)
			cancelledRuntime := readRuntime()
			require.EqualValues(t, 1, cancelledRuntime.StartedEvents)
			require.EqualValues(t, 1, cancelledRuntime.FinishedEvents)
			require.Zero(t, cancelledRuntime.ActiveEvents)
			for range 2 {
				require.Equal(t, cancelledRuntime.LastFinishedAt, readRuntime().LastFinishedAt, "refreshing does not renew the last event")
			}

			// A later explicit event works normally; cancellation creates no retry,
			// supplier penalty, budget refund, or permanent cancellation state.
			cancelOnCloseEnabled = false
			require.Nil(t, tests.ProbeOpenAIAPIKeyResponsesSupport(t.Context(), account.ID))
			after, err := accounts.GetByID(t.Context(), account.ID)
			require.NoError(t, err)
			require.Equal(t, supported, after.Extra["openai_responses_supported"])
			require.Equal(t, "preserved", after.Extra["manual_note"])
			require.Equal(t, service.StatusActive, after.Status)
			require.True(t, after.Schedulable)
			require.EqualValues(t, 2, calls.Load())
			completedRuntime := readRuntime()
			require.Equal(t, initialRuntime.InstanceID, completedRuntime.InstanceID)
			require.EqualValues(t, 2, completedRuntime.StartedEvents)
			require.EqualValues(t, 2, completedRuntime.FinishedEvents)
			require.Zero(t, completedRuntime.ActiveEvents)
			stats, err = budget.Summary(t.Context())
			require.NoError(t, err)
			require.EqualValues(t, 2, stats.DayUsed)
			require.EqualValues(t, 14, stats.DayTokens)
			require.EqualValues(t, 2, stats.UsageReported)
			require.Zero(t, stats.UsageUnknown)
			require.Zero(t, stats.UsagePending)
			require.Zero(t, stats.ActiveRequests)
			var downstreamUsage int64
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM usage_logs`).Scan(&downstreamUsage))
			require.Zero(t, downstreamUsage, "capability detection must not produce downstream business usage")
		})
	}
}

func TestMonitoringProbeBudgetActualTokenStop(t *testing.T) {
	db, _ := newBillingAuditDatabase(t)
	for _, protocol := range []string{"chat", "vertex_json", "vertex_stream", "bedrock_apikey", "bedrock_sigv4"} {
		t.Run(protocol, func(t *testing.T) {
			_, err := db.ExecContext(t.Context(), `TRUNCATE monitoring_probe_budget_counters,monitoring_probe_budget_leases; DELETE FROM settings WHERE key='monitoring_probe_budget'`)
			require.NoError(t, err)
			policy := monitoringBudgetPolicy()
			// Use the wire contract so this test also exercises old policy JSON.
			require.NoError(t, json.Unmarshal([]byte(`{"tokens_per_day":5}`), &policy))
			repo := NewMonitoringProbeBudgetRepository(db)
			policy, err = repo.SaveSettings(t.Context(), policy)
			require.NoError(t, err)
			path, tokens := "/v1/chat/completions", int64(7)
			contentType := "application/json"
			body := `{"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`
			if protocol != "chat" {
				path, tokens = "/model/anthropic.claude:0/invoke", 14
				body = `{"type":"message","stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2,"cache_read_input_tokens":5,"cache_creation_input_tokens":4}}`
			}
			if strings.HasPrefix(protocol, "vertex_") {
				path = "/v1/projects/fixture/locations/global/publishers/anthropic/models/claude:rawPredict"
				if protocol == "vertex_stream" {
					path = strings.TrimSuffix(path, ":rawPredict") + ":streamRawPredict"
					contentType = "text/event-stream"
					body = "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":0,\"cache_read_input_tokens\":5,\"cache_creation_input_tokens\":4}}}\n\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
				}
			}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				require.Equal(t, path, r.URL.Path)
				w.Header().Set("Content-Type", contentType)
				_, _ = w.Write([]byte(body))
			}))
			t.Cleanup(server.Close)
			client := &http.Client{Transport: &accountResponseTransport{base: http.DefaultTransport}}
			send := func() error {
				ctx := service.NewMonitoringProbeBudgetService(NewMonitoringProbeBudgetRepository(db)).Context(t.Context(), server.URL, false)
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+path, strings.NewReader(`{}`))
				require.NoError(t, err)
				if protocol == "bedrock_sigv4" {
					signer := service.NewBedrockSigner("AKIAFIXTUREBUDGET1234", "fixture-secret", "", "us-east-1")
					require.NoError(t, signer.SignRequest(ctx, req, []byte(`{}`)))
				} else {
					req.Header.Set("Authorization", "Bearer token-budget-fixture")
				}
				resp, err := client.Do(req)
				if err == nil {
					_, err = io.Copy(io.Discard, resp.Body)
					require.NoError(t, resp.Body.Close())
				}
				return err
			}
			require.NoError(t, send(), "the in-flight response may exceed the budget")
			var denial *service.MonitoringProbeBudgetDenial
			require.ErrorAs(t, send(), &denial, "actual upstream usage must stop later sends after service reconstruction")
			require.Equal(t, "token_limit", denial.Reason)
			require.Equal(t, "global_day", denial.Scope)
			require.EqualValues(t, 1, calls.Load())
			usage, err := repo.Summary(t.Context())
			require.NoError(t, err)
			require.Equal(t, tokens, usage.DayTokens)
			require.EqualValues(t, 1, usage.UsageReported)
			require.Zero(t, usage.UsageUnknown)
			require.Zero(t, usage.ActiveRequests)
			require.Zero(t, usage.UsagePending)
			policy.TokensPerDay = 50
			_, err = repo.SaveSettings(t.Context(), policy)
			require.NoError(t, err)
			require.NoError(t, send(), "an explicit policy change restores future detection")
			require.EqualValues(t, 2, calls.Load())
			usage, err = repo.Summary(t.Context())
			require.NoError(t, err)
			require.Equal(t, 2*tokens, usage.DayTokens)
			require.EqualValues(t, 2, usage.UsageReported)
			require.Zero(t, usage.ActiveRequests)
			require.Zero(t, usage.UsagePending)
		})
	}
}

func TestMonitoringProbeBudgetActualTokenSharedSettlement(t *testing.T) {
	db, _ := newBillingAuditDatabase(t)
	ctx := t.Context()
	repo := NewMonitoringProbeBudgetRepository(db)
	for _, scope := range []string{"global_minute", "global_day", "site_minute", "key_minute"} {
		t.Run(scope, func(t *testing.T) {
			_, err := db.ExecContext(ctx, `TRUNCATE monitoring_probe_budget_counters,monitoring_probe_budget_leases; DELETE FROM settings WHERE key='monitoring_probe_budget'`)
			require.NoError(t, err)
			policy := monitoringBudgetPolicy()
			switch scope {
			case "global_minute":
				policy.TokensPerMinute = 5
			case "global_day":
				policy.TokensPerDay = 5
			case "site_minute":
				policy.SiteTokensPerMinute = 5
			case "key_minute":
				policy.KeyTokensPerMinute = 5
			}
			policy, err = repo.SaveSettings(ctx, policy)
			require.NoError(t, err)
			req := service.MonitoringProbeBudgetRequest{SiteHash: "token-site", KeyHashes: []string{"same-actual-key", "same-actual-key"}, MeterTokens: true}
			first, err := repo.Acquire(ctx, req)
			require.NoError(t, err)
			peer := NewMonitoringProbeBudgetRepository(db)
			second, err := peer.Acquire(ctx, req)
			require.NoError(t, err, "no guessed token reservation before sending")
			pending, err := peer.Summary(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 2, pending.UsagePending)
			for _, lease := range []string{first, second} {
				require.NoError(t, peer.Release(ctx, lease, service.MonitoringProbeTokenUsage{Tokens: 4, Reported: true}))
			}
			// Reconstruct a repository and concurrently replay the same receipts.
			var wg sync.WaitGroup
			for range 8 {
				wg.Go(func() {
					require.NoError(t, NewMonitoringProbeBudgetRepository(db).Release(ctx, first, service.MonitoringProbeTokenUsage{Tokens: 4, Reported: true}))
				})
			}
			wg.Wait()
			usage, err := peer.Summary(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 8, usage.DayTokens, "in-flight excess is actual usage, not truncated to the cap")
			require.EqualValues(t, 2, usage.UsageReported)
			require.Zero(t, usage.ActiveRequests)
			require.Zero(t, usage.UsagePending)
			_, err = NewMonitoringProbeBudgetRepository(db).Acquire(ctx, req)
			var denied *service.MonitoringProbeBudgetDenial
			require.ErrorAs(t, err, &denied)
			require.Equal(t, "token_limit", denied.Reason)
			require.Equal(t, scope, denied.Scope)
			require.True(t, denied.RetryAt.After(usage.ObservedAt))
			// Non-generating management observations are not starved by tokens.
			req.MeterTokens = false
			management, err := peer.Acquire(ctx, req)
			require.NoError(t, err)
			require.NoError(t, peer.Release(ctx, management, service.MonitoringProbeTokenUsage{}))
			policy.TokensPerMinute, policy.TokensPerDay, policy.SiteTokensPerMinute, policy.KeyTokensPerMinute = 50, 50, 50, 50
			policy, err = peer.SaveSettings(ctx, policy)
			require.NoError(t, err)
			req.MeterTokens = true
			restored, err := NewMonitoringProbeBudgetRepository(db).Acquire(ctx, req)
			require.NoError(t, err)
			require.NoError(t, repo.Release(ctx, restored, service.MonitoringProbeTokenUsage{}))
			zero, err := repo.Acquire(ctx, req)
			require.NoError(t, err)
			require.NoError(t, repo.Release(ctx, zero, service.MonitoringProbeTokenUsage{Reported: true}))
			partial, err := repo.Acquire(ctx, req)
			require.NoError(t, err)
			require.NoError(t, repo.Release(ctx, partial, service.MonitoringProbeTokenUsage{Tokens: 2}))
			usage, err = repo.Summary(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 10, usage.DayTokens)
			require.EqualValues(t, 3, usage.UsageReported, "explicit zero counts as reported")
			require.EqualValues(t, 2, usage.UsageUnknown, "missing and partial are unknown, never reported zero")
			policy.Enabled = false
			_, err = repo.SaveSettings(ctx, policy)
			require.NoError(t, err)
			observe, err := repo.Acquire(ctx, req)
			require.NoError(t, err)
			require.NoError(t, repo.Release(ctx, observe, service.MonitoringProbeTokenUsage{Tokens: 60, Reported: true}))
			usage, err = repo.Summary(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 70, usage.DayTokens, "observe-only also records actual usage")
		})
	}
}

func TestMonitoringProbeBudgetActualTokenExpiryAndMigration(t *testing.T) {
	db, _ := newBillingAuditDatabase(t)
	ctx := t.Context()
	repo := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.TokensPerDay, policy.RecoveryReservePercent = 10, 20
	_, err := repo.SaveSettings(ctx, policy)
	require.NoError(t, err)
	req := service.MonitoringProbeBudgetRequest{SiteHash: "token-site", KeyHashes: []string{"token-key"}, MeterTokens: true}
	id, err := repo.Acquire(ctx, req)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE monitoring_probe_budget_leases SET expires_at=clock_timestamp()-INTERVAL '1 day' WHERE id=$1`, id)
	require.NoError(t, err)
	other, err := repo.Acquire(ctx, req)
	require.NoError(t, err, "expired leases no longer occupy capacity")
	usage, err := repo.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 2, usage.UsagePending, "expired unsettled usage remains visible across pruning/restart")
	require.NoError(t, NewMonitoringProbeBudgetRepository(db).Release(ctx, id, service.MonitoringProbeTokenUsage{Tokens: 8, Reported: true}))
	require.NoError(t, repo.Release(ctx, other, service.MonitoringProbeTokenUsage{Reported: true}))
	_, err = repo.Acquire(ctx, req)
	var denied *service.MonitoringProbeBudgetDenial
	require.ErrorAs(t, err, &denied)
	require.Equal(t, "recovery_reserve", denied.Reason)
	req.Recovery = true
	recovery, err := repo.Acquire(ctx, req)
	require.NoError(t, err)
	require.NoError(t, repo.Release(ctx, recovery, service.MonitoringProbeTokenUsage{Tokens: 3, Reported: true}))
	_, err = repo.Acquire(ctx, req)
	require.ErrorAs(t, err, &denied)
	require.Equal(t, "token_limit", denied.Reason, "recovery still obeys the total actual token limit")
	ddl, err := os.ReadFile("../../migrations/345_monitoring_probe_token_usage.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, string(ddl))
	require.NoError(t, err)
	// Old SQL writers/readers still work after the additive migration.
	_, err = db.ExecContext(ctx, `INSERT INTO monitoring_probe_budget_leases(id,site_hash,key_hashes,recovery,expires_at)
 VALUES('00000000-0000-0000-0000-000000000001','old-site',ARRAY['old-key'],false,clock_timestamp()+INTERVAL '1 minute')`)
	require.NoError(t, err)
	var oldCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM monitoring_probe_budget_leases WHERE expires_at>clock_timestamp()`).Scan(&oldCount))
	require.Positive(t, oldCount)
	_, err = db.ExecContext(ctx, `UPDATE settings SET value=$2 WHERE key=$1`, service.MonitoringProbeBudgetSettingKey, `{"enabled":false,"revision":5,"requests_per_minute":600,"requests_per_day":100000,"site_requests_per_minute":600,"key_requests_per_minute":200,"concurrency":16,"site_concurrency":4,"key_concurrency":2,"recovery_reserve_percent":20}`)
	require.NoError(t, err)
	oldPolicy, err := NewMonitoringProbeBudgetRepository(db).GetSettings(ctx)
	require.NoError(t, err)
	require.Zero(t, oldPolicy.TokensPerDay, "old policy JSON must not silently enable a new token cap")
	usage, err = repo.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 11, usage.DayTokens)
}

func TestMonitoringProbeBudgetActualTokenSettlementRollbackAndWindow(t *testing.T) {
	db, _ := newBillingAuditDatabase(t)
	ctx := t.Context()
	repo := NewMonitoringProbeBudgetRepository(db)
	_, err := repo.SaveSettings(ctx, monitoringBudgetPolicy())
	require.NoError(t, err)
	req := service.MonitoringProbeBudgetRequest{SiteHash: "token-site", KeyHashes: []string{"token-key"}, MeterTokens: true}
	id, err := repo.Acquire(ctx, req)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `CREATE FUNCTION fixture_token_settlement_fail() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN RAISE EXCEPTION 'fixture settlement unavailable'; END $$;
 CREATE TRIGGER fixture_token_settlement_fail BEFORE INSERT OR UPDATE ON monitoring_probe_budget_counters
 FOR EACH ROW EXECUTE FUNCTION fixture_token_settlement_fail()`)
	require.NoError(t, err)
	returned := service.MonitoringProbeTokenUsage{Tokens: 7, Reported: true}
	require.Error(t, repo.Release(ctx, id, returned))
	usage, err := repo.Summary(ctx)
	require.NoError(t, err)
	require.Zero(t, usage.DayTokens, "failed settlement must not partially charge one dimension")
	require.EqualValues(t, 1, usage.UsagePending)
	_, err = db.ExecContext(ctx, `DROP TRIGGER fixture_token_settlement_fail ON monitoring_probe_budget_counters; DROP FUNCTION fixture_token_settlement_fail()`)
	require.NoError(t, err)
	require.NoError(t, NewMonitoringProbeBudgetRepository(db).Release(ctx, id, returned))
	// Freeze the old accounting windows in the past, then settle a live call.
	_, err = db.ExecContext(ctx, `UPDATE monitoring_probe_budget_counters SET bucket_start=bucket_start-INTERVAL '2 day'`)
	require.NoError(t, err)
	next, err := repo.Acquire(ctx, req)
	require.NoError(t, err)
	require.NoError(t, repo.Release(ctx, next, service.MonitoringProbeTokenUsage{Tokens: 3, Reported: true}))
	require.NoError(t, repo.Release(ctx, id, returned), "old receipt replay must not charge the new day")
	usage, err = repo.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 3, usage.DayTokens)
	require.EqualValues(t, 1, usage.UsageReported)
	require.Zero(t, usage.UsagePending)
}

func TestAstraGatewayMonitoringBudgetPostgresSharedKeyAndHistory(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "chatgpt.com" || r.Header.Get("Authorization") != "Bearer astra-diagnostic-fixture" {
			http.Error(w, "unexpected diagnostic fixture", http.StatusBadRequest)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("x-codex-turn-state", "fixture-ticket")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"))
	}))
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "example.com", MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		}}
	t.Cleanup(transport.CloseIdleConnections)
	httpClient := &http.Client{Transport: &accountResponseTransport{base: transport}, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	delegate := gatewayPinDelegate{call: func(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		return httpClient.Do(req)
	}}
	accounts := newAccountRepositoryWithSQL(client, db, nil)
	create := func(name string) *service.Account {
		return mustCreateAccount(t, client, &service.Account{Name: name, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
			Status: service.StatusActive, Schedulable: true, Concurrency: 2,
			Credentials: map[string]any{"access_token": "astra-diagnostic-fixture", "chatgpt_account_id": "fixture-workspace"}})
	}
	source, first, alias := create("diagnostic-source"), create("diagnostic-target"), create("diagnostic-alias")
	var accountBefore string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT json_agg(a ORDER BY id)::text FROM accounts a`).Scan(&accountBefore))
	budget := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.RequestsPerDay = 1
	policy, err := budget.SaveSettings(t.Context(), policy)
	require.NoError(t, err)
	run := func(id int64) service.AstraGatewayTestResult {
		cfg := &config.Config{}
		cfg.Gateway.CodexGatewayPin = config.CodexGatewayPinConfig{Enabled: true, SourceAccountIDs: []int64{source.ID}, TargetAccountIDs: []int64{first.ID, alias.ID}, TTLSeconds: 240}
		wrapper := &astraRoutingUpstream{cfg: cfg, delegate: delegate}
		pool := wrapper.current(t.Context())
		pool.recordSource(source.ID, time.Now(), &codexGatewayRoute{cookie: http.Cookie{Name: "__oailb", Value: "fixture-route", Path: "/", Secure: true}, expires: time.Now().Add(3 * time.Minute)}, true)
		var observations int
		pool.historyRecorder = func(service.AstraGatewayHistoryRecord, bool) { observations++ }
		gateway := service.NewOpenAIGatewayService(accounts, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, service.NewBillingService(cfg, nil), nil, nil, wrapper, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
		t.Cleanup(gateway.StopOpenAICodexTicketHarvester)
		t.Cleanup(gateway.CloseOpenAIWSPool)
		tests := service.NewAccountTestService(accounts, nil, nil, nil, nil, wrapper, cfg, nil)
		tests.SetOpenAIGatewayService(gateway)
		tests.SetMonitoringBudget(service.NewMonitoringProbeBudgetService(NewMonitoringProbeBudgetRepository(db)))
		result, err := tests.TestAstraGateway(t.Context(), "verify", id, 1)
		require.NoError(t, err)
		if result.BudgetDeferral != nil {
			require.Zero(t, observations)
		}
		return result
	}
	deferred := run(first.ID)
	require.False(t, deferred.Success)
	require.Equal(t, "global_day", deferred.BudgetDeferral.Scope)
	require.EqualValues(t, 1, calls.Load())
	aliasResult := run(alias.ID)
	require.Equal(t, "monitoring_budget_deferred", aliasResult.Reason)
	require.EqualValues(t, 1, calls.Load(), "independent services and repeated credentials share SQL limits")
	policy.RequestsPerDay = 20
	policy, err = budget.SaveSettings(t.Context(), policy)
	require.NoError(t, err)
	require.True(t, run(alias.ID).Success)
	require.EqualValues(t, 3, calls.Load())
	policy.Enabled = false
	_, err = budget.SaveSettings(t.Context(), policy)
	require.NoError(t, err)
	require.True(t, run(first.ID).Success)
	usage, err := budget.Summary(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 5, usage.DayUsed)
	require.Zero(t, usage.ActiveRequests)
	var accountAfter string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT json_agg(a ORDER BY id)::text FROM accounts a`).Scan(&accountAfter))
	require.Equal(t, accountBefore, accountAfter)
	// The original event repository, reconstructed independently, retains local
	// deferral metadata while historical/older-writer rows remain SQL NULL.
	flow := NewCodexHarvestFlowRepository(db)
	event := service.CodexHarvestFlowEvent{ID: "local-budget-fixture", At: time.Now().UTC(), Stage: "select", Kind: "candy_deferred", Reason: "candy_budget_deferred", BudgetDeferral: deferred.BudgetDeferral}
	require.NoError(t, flow.Append(t.Context(), event))
	// Exercise the pre-344 writer exactly: omit the optional column rather
	// than passing SQL NULL through the new repository implementation.
	_, err = db.ExecContext(t.Context(), `INSERT INTO codex_harvest_flow_events
 (event_id, at, stage, kind, account_id, account_name, model, node, http_status, length, blocks,
 expected_length, expected_blocks, accepted, standby, result, reason, detail)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		"old-writer-fixture", event.At, "select", "candy_pass", int64(0), "", "", "", 0, 0, 0, 0, 0, false, false, "candy_passed", "", "")
	require.NoError(t, err)
	// A rolled-back reader still projects its original 18 columns when the
	// same row carries the new optional scheduling metadata.
	var oldReader service.CodexHarvestFlowEvent
	err = db.QueryRowContext(t.Context(), `SELECT event_id, at, stage, kind, account_id, account_name, model, node,
 http_status, length, blocks, expected_length, expected_blocks, accepted, standby, result, reason, detail
 FROM codex_harvest_flow_events WHERE event_id = $1`, event.ID).Scan(
		&oldReader.ID, &oldReader.At, &oldReader.Stage, &oldReader.Kind, &oldReader.AccountID, &oldReader.AccountName,
		&oldReader.Model, &oldReader.Node, &oldReader.HTTPStatus, &oldReader.Length, &oldReader.Blocks,
		&oldReader.ExpectedLength, &oldReader.ExpectedBlocks, &oldReader.Accepted, &oldReader.Standby,
		&oldReader.Result, &oldReader.Reason, &oldReader.Detail)
	require.NoError(t, err)
	require.Equal(t, event.ID, oldReader.ID)
	require.Nil(t, oldReader.BudgetDeferral)
	rows, err := NewCodexHarvestFlowRepository(db).List(t.Context(), 200)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for _, row := range rows {
		if row.ID == event.ID {
			require.Equal(t, deferred.BudgetDeferral, row.BudgetDeferral)
		} else {
			require.Nil(t, row.BudgetDeferral)
		}
	}
	ddl, err := os.ReadFile("../../migrations/344_codex_harvest_flow_budget_deferral.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), string(ddl))
	require.NoError(t, err, "additive migration is idempotent and retains evidence for rollback")
}

func TestStateProbeMonitoringBudgetPostgresSharedKeyRestartAndRecovery(t *testing.T) {
	for _, scope := range []string{"global_day", "key_minute"} {
		t.Run(scope, func(t *testing.T) {
			db, client := newBillingAuditDatabase(t)
			var calls, redirected atomic.Int32
			var redirect atomic.Bool
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				redirected.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(target.Close)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Host != "chatgpt.com" || r.URL.Path != "/backend-api/codex/responses" || r.Header.Get("Authorization") != "Bearer state-budget-fixture" {
					http.Error(w, "unexpected state probe request", http.StatusBadRequest)
					return
				}
				calls.Add(1)
				if redirect.Load() {
					http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("x-codex-turn-state", "fixture-ticket")
				_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"))
			}))
			t.Cleanup(server.Close)
			roots := x509.NewCertPool()
			roots.AddCert(server.Certificate())
			transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "example.com", MinVersion: tls.VersionTLS12}}
			transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
			}
			t.Cleanup(transport.CloseIdleConnections)
			httpClient := &http.Client{Transport: &accountResponseTransport{base: transport}, Timeout: 5 * time.Second,
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			upstream := gatewayPinDelegate{call: func(r *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
				require.True(t, service.HTTPUpstreamRedirectsDisabled(r.Context()))
				return httpClient.Do(r)
			}}
			accounts := newAccountRepositoryWithSQL(client, db, nil)
			makeAccount := func(name string) *service.Account {
				return mustCreateAccount(t, client, &service.Account{Name: name, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
					Status: service.StatusActive, Schedulable: true, Concurrency: 2,
					Credentials: map[string]any{"access_token": "state-budget-fixture", "chatgpt_account_id": "state-fixture-account", "base_url": "https://unused.invalid/tenant"}})
			}
			first, alias := makeAccount("state-budget"), makeAccount("state-budget-alias")
			accountSnapshot := func(id int64) string {
				var snapshot string
				require.NoError(t, db.QueryRowContext(t.Context(), `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, id).Scan(&snapshot))
				return snapshot
			}
			before := map[int64]string{first.ID: accountSnapshot(first.ID), alias.ID: accountSnapshot(alias.ID)}
			budget := NewMonitoringProbeBudgetRepository(db)
			policy := monitoringBudgetPolicy()
			if scope == "global_day" {
				policy.RequestsPerDay = 1
			} else {
				policy.KeyRequestsPerMinute = 1
			}
			policy, err := budget.SaveSettings(t.Context(), policy)
			require.NoError(t, err)
			cfg := &config.Config{}
			run := func(accountID int64, scheduled bool) *service.OpenAICodexStateProbeResult {
				gateway := service.NewOpenAIGatewayService(accounts, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, service.NewBillingService(cfg, nil), nil, nil, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
				t.Cleanup(gateway.StopOpenAICodexTicketHarvester)
				t.Cleanup(gateway.CloseOpenAIWSPool)
				tests := service.NewAccountTestService(accounts, nil, nil, nil, nil, upstream, cfg, nil)
				tests.SetOpenAIGatewayService(gateway)
				tests.SetMonitoringBudget(service.NewMonitoringProbeBudgetService(NewMonitoringProbeBudgetRepository(db)))
				if scheduled {
					result, err := tests.RunPelicanBackground(t.Context(), accountID, "gpt-6-astra", &service.PelicanTestConfig{QuestionKind: service.OpenAICodexStateProbeQuestionKind,
						ParallelCount: 1, Quality: &service.QualityPolicy{Action: service.QualityActionEnableBPS, AutoRestore: true}})
					require.NoError(t, err)
					require.Equal(t, "answer_budget_deferred", result.ErrorMessage)
					require.Equal(t, "unknown", result.QualityJudgment.Verdict)
					require.Equal(t, scope, result.QualityJudgment.BudgetDeferral.Scope)
					raw, err := json.Marshal(result)
					require.NoError(t, err)
					var stored service.ScheduledTestResult
					require.NoError(t, json.Unmarshal(raw, &stored))
					require.Equal(t, result.QualityJudgment.BudgetDeferral, stored.QualityJudgment.BudgetDeferral)
					plan, err := NewScheduledTestPlanRepository(db).Create(t.Context(), &service.ScheduledTestPlan{AccountID: accountID,
						ModelID: "gpt-6-astra", CronExpression: "*/5 * * * *", Enabled: true, MaxResults: 20, PelicanConfig: result.PelicanConfig})
					require.NoError(t, err)
					result.PlanID = plan.ID
					saved, err := NewScheduledTestResultRepository(db).Create(t.Context(), result)
					require.NoError(t, err)
					read, err := NewScheduledTestResultRepository(db).GetResult(t.Context(), plan.ID, saved.ID)
					require.NoError(t, err)
					require.Equal(t, "answer_budget_deferred", read.ErrorMessage)
					require.Equal(t, result.QualityJudgment, read.QualityJudgment)
					require.Equal(t, "inconclusive", read.PelicanConfig.AvailabilityResult.Status)
					return nil
				}
				result, err := tests.ProbeOpenAICodexState(t.Context(), accountID, "")
				require.NoError(t, err)
				return result
			}
			deferred := run(first.ID, false)
			require.EqualValues(t, 1, calls.Load())
			require.Equal(t, service.OpenAICodexStateInconclusive, deferred.Verdict)
			require.Equal(t, "monitoring_budget_deferred", deferred.Failure)
			require.Equal(t, scope, deferred.BudgetDeferral.Scope)
			_ = run(alias.ID, true)
			require.EqualValues(t, 1, calls.Load(), "fresh services and duplicate accounts cannot reset SQL credential counters")
			usage, err := budget.Summary(t.Context())
			require.NoError(t, err)
			require.EqualValues(t, 1, usage.DayUsed)
			require.Zero(t, usage.ActiveRequests)
			policy.RequestsPerDay, policy.KeyRequestsPerMinute = 100, 100
			policy, err = budget.SaveSettings(t.Context(), policy)
			require.NoError(t, err)
			recovered := run(alias.ID, false)
			require.Equal(t, service.OpenAICodexStateHealthy, recovered.Verdict)
			require.Nil(t, recovered.BudgetDeferral)
			require.EqualValues(t, 3, calls.Load())
			policy.Enabled = false
			_, err = budget.SaveSettings(t.Context(), policy)
			require.NoError(t, err)
			recovered = run(first.ID, false)
			require.Equal(t, service.OpenAICodexStateHealthy, recovered.Verdict)
			require.EqualValues(t, 5, calls.Load(), "observe-only still accounts both physical shots")
			redirect.Store(true)
			rejected := run(first.ID, false)
			require.Equal(t, service.OpenAICodexStateInconclusive, rejected.Verdict)
			require.Nil(t, rejected.BudgetDeferral, "an upstream redirect is not local budget exhaustion")
			require.Zero(t, redirected.Load())
			usage, err = budget.Summary(t.Context())
			require.NoError(t, err)
			require.EqualValues(t, 6, usage.DayUsed)
			require.Zero(t, usage.ActiveRequests)
			for id, snapshot := range before {
				require.True(t, snapshot == accountSnapshot(id), "budget diagnostic and recovery do not modify the account")
			}
		})
	}
}

func TestModelTraceMonitoringBudgetPostgresSharedKeyAndRecovery(t *testing.T) {
	for _, scope := range []string{"global_day", "key_minute"} {
		t.Run(scope, func(t *testing.T) {
			db, client := newBillingAuditDatabase(t)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/tenant/v1/responses" || r.Header.Get("Authorization") != "Bearer modeltrace-budget-fixture" {
					http.Error(w, "unexpected fingerprint request", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", strings.Repeat("17,83,247,", 100))
			}))
			defer server.Close()
			cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true}}}
			accounts := newAccountRepositoryWithSQL(client, db, nil)
			createAccount := func(name string) *service.Account {
				return mustCreateAccount(t, client, &service.Account{Name: name, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
					Status: service.StatusActive, Schedulable: true, Concurrency: 2,
					Credentials: map[string]any{"api_key": "modeltrace-budget-fixture", "base_url": server.URL + "/tenant/v1"}})
			}
			first, alias := createAccount("modeltrace-budget"), createAccount("modeltrace-same-key")
			accountSnapshot := func(id int64) string {
				var raw string
				require.NoError(t, db.QueryRowContext(t.Context(), `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, id).Scan(&raw))
				return raw
			}
			before := map[int64]string{first.ID: accountSnapshot(first.ID), alias.ID: accountSnapshot(alias.ID)}
			budget := NewMonitoringProbeBudgetRepository(db)
			policy := monitoringBudgetPolicy()
			if scope == "global_day" {
				policy.RequestsPerDay = 1
			} else {
				policy.KeyRequestsPerMinute = 1
			}
			policy, err := budget.SaveSettings(t.Context(), policy)
			require.NoError(t, err)
			run := func(accountID int64) service.ModelTraceResult {
				// A fresh service and repository cannot bypass SQL counters, even
				// when the same credential is represented by another account.
				tests := service.NewAccountTestService(accounts, nil, nil, nil, nil, NewHTTPUpstream(cfg), cfg, nil)
				tests.SetMonitoringBudget(service.NewMonitoringProbeBudgetService(NewMonitoringProbeBudgetRepository(db)))
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest(http.MethodPost, "/admin/accounts/test/modeltrace-test", nil).WithContext(t.Context())
				require.NoError(t, tests.TestModelTraceAccountConnection(c, accountID, "fixture-model", ""))
				require.NotContains(t, w.Body.String(), "modeltrace-budget-fixture")
				for _, line := range strings.Split(w.Body.String(), "\n") {
					var event struct {
						Type string                   `json:"type"`
						Data service.ModelTraceResult `json:"data"`
					}
					if data, ok := strings.CutPrefix(line, "data:"); ok && json.Unmarshal([]byte(data), &event) == nil && event.Type == "modeltrace_complete" {
						return event.Data
					}
				}
				t.Fatal("completed fingerprint event missing")
				return service.ModelTraceResult{}
			}
			partial := run(first.ID)
			require.EqualValues(t, 1, calls.Load())
			require.Equal(t, "partial", partial.Status)
			require.Len(t, partial.Samples, 2)
			require.Equal(t, "monitoring_budget_deferred", partial.Samples[1].Error)
			require.Equal(t, partial.BudgetDeferral, partial.Samples[1].BudgetDeferral)
			require.Equal(t, scope, partial.BudgetDeferral.Scope)
			require.Equal(t, 1, partial.Analysis.UsedOutputs)
			deferred := run(alias.ID)
			require.EqualValues(t, 1, calls.Load())
			require.Equal(t, "insufficient", deferred.Status)
			require.Len(t, deferred.Samples, 1)
			require.Nil(t, deferred.Analysis)
			require.Equal(t, scope, deferred.BudgetDeferral.Scope)
			usage, err := budget.Summary(t.Context())
			require.NoError(t, err)
			require.EqualValues(t, 1, usage.DayUsed)
			require.Zero(t, usage.ActiveRequests, "normal completion and refusal leave no active lease")
			policy.RequestsPerDay, policy.KeyRequestsPerMinute = 100, 100
			_, err = budget.SaveSettings(t.Context(), policy)
			require.NoError(t, err)
			recovered := run(alias.ID)
			require.Equal(t, "complete", recovered.Status)
			require.Len(t, recovered.Samples, 3)
			require.Nil(t, recovered.BudgetDeferral)
			require.Equal(t, 3, recovered.Analysis.UsedOutputs)
			require.EqualValues(t, 4, calls.Load())
			usage, err = budget.Summary(t.Context())
			require.NoError(t, err)
			require.EqualValues(t, 4, usage.DayUsed)
			require.Zero(t, usage.ActiveRequests)
			for id, snapshot := range before {
				require.True(t, snapshot == accountSnapshot(id), "fingerprint diagnostics must not modify any persisted account field")
			}
		})
	}
}

func TestQualityMonitoringBudgetPostgresBPSAndRestart(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	db, client := newBillingAuditDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var calls, redirected atomic.Int32
	var redirect atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "bps.openai.com" || r.Header.Get("Authorization") != "Bearer bps-budget-fixture" {
			http.Error(w, "invalid BPS budget fixture", http.StatusBadRequest)
			return
		}
		n := calls.Add(1)
		if redirect.Load() {
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
			return
		}
		switch r.URL.Path {
		case "/basispoints/api/images/generations", "/basispoints/api/images/edits":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"b64_json":"` + strings.Repeat("a", (1<<20)+4) + `"}],"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30}}`))
			return
		case "/basispoints/api/attachments":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"openai_file_id":"file-budget123"}`))
			return
		case "/basispoints/api/responses":
		default:
			http.Error(w, "invalid BPS budget fixture path", http.StatusBadRequest)
			return
		}
		answer := "21"
		if n == 2 {
			answer = `{"verdict":"correct","reason":"equal"}`
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_budget\",\"status\":\"completed\",\"output\":[]}}\n\n", answer)
	}))
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "example.com", MinVersion: tls.VersionTLS12}}
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	t.Cleanup(transport.CloseIdleConnections)
	// Keep the final URL, Host and credentials unchanged. Only TCP goes to
	// this fixture, using trusted TLS and the production budget transport.
	httpClient := &http.Client{Transport: &accountResponseTransport{base: transport}, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	upstream := gatewayPinDelegate{call: func(r *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		return httpClient.Do(r)
	}}
	cfg := &config.Config{}
	cfg.Default.RateMultiplier = 1
	accounts := newAccountRepositoryWithSQL(client, db, nil)
	account := mustCreateAccount(t, client, &service.Account{Name: "bps-quality-budget", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Status: service.StatusActive, Schedulable: true, Credentials: map[string]any{"access_token": "bps-budget-fixture", "chatgpt_account_id": "fixture-account"},
		Extra: map[string]any{"openai_excel_bps": true, "openai_passthrough": true}})
	_, err := db.ExecContext(ctx, `UPDATE accounts SET concurrency=0 WHERE id=$1`, account.ID)
	require.NoError(t, err)
	var group int64
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO groups(name,platform,status) VALUES('bps-quality-budget','openai','active') RETURNING id`).Scan(&group))
	_, err = db.ExecContext(ctx, `INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, account.ID, group)
	require.NoError(t, err)
	budget := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.RequestsPerDay = 2
	policy, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	newServices := func() (*service.AccountTestService, *service.QualityJudgeService, *service.OpenAIGatewayService) {
		settings := service.NewSettingService(NewSettingRepository(client), cfg)
		gateway := service.NewOpenAIGatewayService(accounts, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, service.NewBillingService(cfg, nil), nil, nil, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, settings, nil)
		t.Cleanup(gateway.StopOpenAICodexTicketHarvester)
		t.Cleanup(gateway.CloseOpenAIWSPool)
		tests := service.NewAccountTestService(accounts, nil, nil, nil, nil, upstream, cfg, nil)
		tests.SetOpenAIGatewayService(gateway)
		tests.SetMonitoringBudget(service.NewMonitoringProbeBudgetService(NewMonitoringProbeBudgetRepository(db)))
		return tests, service.NewQualityJudgeService(accounts, NewGroupRepository(client, db), service.NewConcurrencyService(nil), tests, nil, NewMonitoringProbeBudgetRepository(db)), gateway
	}
	question := &service.PelicanTestConfig{QuestionKind: "candy", TestChannel: "bps", Prompt: "Return 21", Quality: &service.QualityPolicy{ExpectedAnswer: "21", Action: service.QualityActionObserveOnly,
		Judge: &service.QualityJudgeConfig{GroupID: group, ModelID: "gpt-6-astra", Prompt: "grade"}}}
	firstTests, firstJudge, _ := newServices()
	answer, err := firstTests.RunPelicanBackground(ctx, account.ID, "gpt-6-astra", question)
	require.NoError(t, err)
	require.Equal(t, "success", answer.Status, "%+v", answer)
	judged := firstJudge.Judge(ctx, account.ID+1, question, "21")
	require.Equal(t, "correct", judged.Verdict, "%+v", judged)
	secondTests, secondJudge, _ := newServices()
	answer, err = secondTests.RunPelicanBackground(ctx, account.ID, "gpt-6-astra", question)
	require.NoError(t, err)
	require.Equal(t, "answer_budget_deferred", answer.ErrorMessage)
	require.Equal(t, "unknown", answer.QualityJudgment.Verdict)
	judged = secondJudge.Judge(ctx, account.ID+1, question, "21")
	require.Equal(t, "judge_budget_deferred", judged.Reason)
	require.EqualValues(t, 2, calls.Load(), "independent exhausted instances must not send BPS requests")
	policy.Enabled = false
	policy, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	answer, err = secondTests.RunPelicanBackground(ctx, account.ID, "gpt-6-astra", question)
	require.NoError(t, err)
	require.Equal(t, "success", answer.Status, "%+v", answer)
	redirect.Store(true)
	answer, err = secondTests.RunPelicanBackground(ctx, account.ID, "gpt-6-astra", question)
	require.NoError(t, err)
	require.Equal(t, "failed", answer.Status)
	require.Nil(t, answer.QualityJudgment)
	require.Zero(t, redirected.Load(), "BPS must not forward credentials through redirects")
	usage, err := budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 4, calls.Load())
	require.EqualValues(t, 4, usage.DayUsed)
	require.Zero(t, usage.ActiveRequests)

	t.Run("media-shared-usage-and-restart", func(t *testing.T) {
		redirect.Store(false)
		alias := mustCreateAccount(t, client, &service.Account{Name: "bps-media-budget", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
			Status: service.StatusActive, Schedulable: true, Credentials: map[string]any{"access_token": "bps-budget-fixture", "chatgpt_account_id": "fixture-account", "base_url": "https://native.example/tenant"},
			Extra: map[string]any{"openai_excel_bps": true, "openai_passthrough": true, "openai_excel_bps_models": []string{"gpt-image-2", "gpt-6-astra"}}})
		_, err := db.ExecContext(ctx, `UPDATE accounts SET concurrency=0 WHERE id=$1`, alias.ID)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES($1,'true'),($2,'native')`, service.SettingKeyExcelBPSImageRelayEnabled, service.SettingKeyExcelBPSImageMode)
		require.NoError(t, err)
		snapshot := func() string {
			var raw string
			require.NoError(t, db.QueryRowContext(ctx, `SELECT jsonb_agg(to_jsonb(a) ORDER BY a.id)::text FROM accounts a`).Scan(&raw))
			return raw
		}
		before := snapshot()
		policy.Enabled, policy.TokensPerDay, policy.RequestsPerDay = true, 30, 100
		policy, err = budget.SaveSettings(ctx, policy)
		require.NoError(t, err)
		thirdTests, _, _ := newServices()
		image := func(tests *service.AccountTestService) error {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/test-pelican", nil).WithContext(ctx)
			return tests.TestPelicanAccountConnection(c, alias.ID, "gpt-image-2", "draw", "")
		}
		require.NoError(t, image(thirdTests))
		fourthTests, _, gateway := newServices()
		var denial *service.MonitoringProbeBudgetDenial
		require.ErrorAs(t, image(fourthTests), &denial)
		require.Equal(t, "token_limit", denial.Reason)
		require.EqualValues(t, 5, calls.Load(), "rebuilt services share the same actual-token budget")
		usage, err := budget.Summary(ctx)
		require.NoError(t, err)
		require.EqualValues(t, 30, usage.DayTokens)
		require.EqualValues(t, 1, usage.UsageReported)
		require.Zero(t, usage.UsagePending)

		// A non-generating upload is still counted after Token exhaustion,
		// but the following Responses generation must not be sent.
		body := []byte(`{"model":"gpt-6-astra","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAIAAAADCAYAAAC56t6BAAAAFElEQVR4nGJiYGBgYGBgYgADAgAA//8AOgAFpUq8lwAAAABJRU5ErkJggg=="}]}]}`)
		forward := func() error {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			probeCtx := service.NewMonitoringProbeBudgetService(NewMonitoringProbeBudgetRepository(db)).Context(ctx, "https://bps.openai.com", false)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body))).WithContext(probeCtx)
			_, err := gateway.Forward(probeCtx, c, account, body)
			return err
		}
		require.ErrorAs(t, forward(), &denial)
		require.Equal(t, "token_limit", denial.Reason)
		require.EqualValues(t, 6, calls.Load(), "upload and generation have independent physical admissions")
		usage, err = budget.Summary(ctx)
		require.NoError(t, err)
		require.EqualValues(t, 6, usage.DayUsed)
		require.EqualValues(t, 30, usage.DayTokens)
		require.EqualValues(t, 1, usage.UsageReported, "an upload is not missing generation usage")
		require.Zero(t, usage.ActiveRequests)
		require.Zero(t, usage.UsagePending)

		policy.TokensPerDay = 0
		_, err = budget.SaveSettings(ctx, policy)
		require.NoError(t, err)
		require.NoError(t, forward(), "explicitly lifting the Token limit restores the same service")
		require.EqualValues(t, 8, calls.Load())
		usage, err = budget.Summary(ctx)
		require.NoError(t, err)
		require.EqualValues(t, 8, usage.DayUsed)
		require.EqualValues(t, 30, usage.DayTokens)
		require.EqualValues(t, 1, usage.UsageReported)
		require.GreaterOrEqual(t, usage.UsageUnknown, int64(1), "unreported Responses usage remains unknown")
		require.Zero(t, usage.ActiveRequests)
		require.Equal(t, before, snapshot(), "diagnostics must not alter persisted account state")
		require.Zero(t, redirected.Load())
	})
}

func TestQualityMonitoringBudgetPostgresNativeHTTPAndRestart(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	var calls atomic.Int32
	var redirect atomic.Bool
	var redirectCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		if redirect.Load() {
			http.Redirect(w, req, target.URL+"/responses", http.StatusTemporaryRedirect)
			return
		}
		if req.URL.Path != "/tenant/v1/responses" || req.Header.Get("Authorization") != "Bearer native-budget-fixture" {
			http.Error(w, "invalid quality fixture request", 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"{\\\"verdict\\\":\\\"correct\\\",\\\"reason\\\":\\\"equal\\\"}\"}\n\ndata: {\"type\":\"response.completed\"}\n\n"))
	}))
	defer server.Close()
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true}}}
	accounts := newAccountRepositoryWithSQL(client, db, nil)
	account := mustCreateAccount(t, client, &service.Account{Name: "native-quality-budget", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true,
		Credentials: map[string]any{"api_key": "native-budget-fixture", "base_url": server.URL + "/tenant/v1"}})
	var group int64
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO groups(name,platform,status) VALUES('native-quality-budget','openai','active') RETURNING id`).Scan(&group))
	_, err := db.ExecContext(ctx, `UPDATE accounts SET concurrency=0 WHERE id=$1`, account.ID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, account.ID, group)
	require.NoError(t, err)
	budget := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.RequestsPerDay = 2
	policy, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	newServices := func() (*service.AccountTestService, *service.QualityJudgeService) {
		repo := NewMonitoringProbeBudgetRepository(db)
		tests := service.NewAccountTestService(accounts, nil, nil, nil, nil, NewHTTPUpstream(cfg), cfg, nil)
		tests.SetMonitoringBudget(service.NewMonitoringProbeBudgetService(repo))
		judge := service.NewQualityJudgeService(accounts, NewGroupRepository(client, db), service.NewConcurrencyService(nil), tests, nil, repo)
		return tests, judge
	}
	firstTests, firstJudge := newServices()
	question := &service.PelicanTestConfig{QuestionKind: "candy", Prompt: "compare fixture", Quality: &service.QualityPolicy{ExpectedAnswer: "21", Action: service.QualityActionObserveOnly,
		Judge: &service.QualityJudgeConfig{GroupID: group, ModelID: "fixture-model", Prompt: "compare values"}}}
	judged := firstJudge.Judge(ctx, account.ID+1, question, "21")
	require.Equal(t, "correct", judged.Verdict, "%+v", judged)
	answer, err := firstTests.RunPelicanBackground(ctx, account.ID, "fixture-model", question)
	require.NoError(t, err)
	require.Equal(t, "success", answer.Status, "%+v", answer)
	require.EqualValues(t, 2, calls.Load())
	// Independent instances share the exact same physical-request budget.
	secondTests, secondJudge := newServices()
	judged = secondJudge.Judge(ctx, account.ID+1, question, "21")
	require.Equal(t, "judge_budget_deferred", judged.Reason)
	require.Equal(t, "global_day", judged.BudgetDeferral.Scope)
	answer, err = secondTests.RunPelicanBackground(ctx, account.ID, "fixture-model", question)
	require.NoError(t, err)
	require.Equal(t, "answer_budget_deferred", answer.ErrorMessage)
	require.Equal(t, "unknown", answer.QualityJudgment.Verdict)
	require.Equal(t, "global_day", answer.QualityJudgment.BudgetDeferral.Scope)
	require.Equal(t, "inconclusive", answer.PelicanConfig.AvailabilityResult.Status)
	require.EqualValues(t, 2, calls.Load(), "both exhausted paths must send zero physical requests")
	usage, err := budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 2, usage.DayUsed)
	require.Zero(t, usage.ActiveRequests)
	policy.Enabled = false
	_, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	answer, err = secondTests.RunPelicanBackground(ctx, account.ID, "fixture-model", question)
	require.NoError(t, err)
	require.Equal(t, "success", answer.Status)
	require.Nil(t, answer.QualityJudgment)
	usage, err = budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 3, calls.Load())
	require.EqualValues(t, 3, usage.DayUsed)
	require.Zero(t, usage.ActiveRequests)
	redirect.Store(true)
	answer, err = secondTests.RunPelicanBackground(ctx, account.ID, "fixture-model", question)
	require.NoError(t, err)
	require.Equal(t, "failed", answer.Status)
	require.Nil(t, answer.QualityJudgment, "an upstream redirect is not a local budget deferral")
	require.Zero(t, redirectCalls.Load(), "quality requests must not forward credentials across redirects")
	usage, err = budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 4, calls.Load())
	require.EqualValues(t, 4, usage.DayUsed, "count the redirect response as one physical attempt")
	require.Zero(t, usage.ActiveRequests)
}

func TestQualityMonitoringBudgetPostgresNativeWSAndRestart(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var handshakes, frames, redirectCalls atomic.Int32
	var redirect atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	entered, proceed := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(proceed) })
	defer release()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handshakes.Add(1)
		if redirect.Load() {
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
			return
		}
		if r.URL.Path != "/tenant/v1/responses" || r.Header.Get("Authorization") != "Bearer ws-budget-fixture" {
			http.Error(w, "invalid WS budget fixture request", 400)
			return
		}
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		for {
			if _, _, err := conn.Read(r.Context()); err != nil {
				return
			}
			n := frames.Add(1)
			if n == 2 {
				close(entered)
				select {
				case <-proceed:
				case <-r.Context().Done():
					return
				}
			}
			for _, event := range []string{`{"type":"response.output_text.delta","delta":"21"}`, `{"type":"response.completed","response":{"id":"resp_budget","status":"completed","model":"target","usage":{"input_tokens":1,"output_tokens":1}}}`} {
				if err := conn.Write(r.Context(), coderws.MessageText, []byte(event)); err != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(server.Close)
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true}}}
	cfg.Default.RateMultiplier = 1
	cfg.Gateway.OpenAIWS.Enabled, cfg.Gateway.OpenAIWS.APIKeyEnabled, cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true, true, true
	cfg.Gateway.OpenAIWS.PrewarmGenerateEnabled = true
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount, cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1, 1
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds, cfg.Gateway.OpenAIWS.ReadTimeoutSeconds, cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3, 3, 3
	accounts := newAccountRepositoryWithSQL(client, db, nil)
	account := mustCreateAccount(t, client, &service.Account{Name: "native-ws-budget", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{"api_key": "ws-budget-fixture", "base_url": server.URL + "/tenant"}, Extra: map[string]any{"responses_websockets_v2_enabled": true}})
	budget := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.RequestsPerDay, policy.Concurrency, policy.SiteConcurrency, policy.KeyConcurrency = 3, 1, 1, 1
	policy, err := budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	newServices := func() *service.AccountTestService {
		upstream := NewHTTPUpstream(cfg)
		gateway := service.NewOpenAIGatewayService(accounts, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, service.NewBillingService(cfg, nil), nil, nil, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
		t.Cleanup(gateway.StopOpenAICodexTicketHarvester)
		t.Cleanup(gateway.CloseOpenAIWSPool)
		tests := service.NewAccountTestService(accounts, nil, nil, nil, nil, upstream, cfg, nil)
		tests.SetOpenAIGatewayService(gateway)
		tests.SetMonitoringBudget(service.NewMonitoringProbeBudgetService(NewMonitoringProbeBudgetRepository(db)))
		return tests
	}
	question := &service.PelicanTestConfig{QuestionKind: "candy", Prompt: "Return 21", ReasoningEffort: "high", ParallelCount: 1}
	first, second := newServices(), newServices()
	done := make(chan *service.ScheduledTestResult, 1)
	go func() {
		answer, _ := first.RunPelicanBackground(ctx, account.ID, "target", question)
		done <- answer
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("the final WS generation did not reach the upstream")
	}
	usage, err := budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 3, usage.DayUsed, "handshake, prewarm and generation each consume one attempt")
	require.Equal(t, 1, usage.ActiveRequests, "only the unfinished generation still holds a shared slot")
	release()
	select {
	case answer := <-done:
		require.Equal(t, "success", answer.Status, "%+v", answer)
	case <-ctx.Done():
		t.Fatal("the completed WS answer was not returned")
	}
	answer, err := second.RunPelicanBackground(ctx, account.ID, "target", question)
	require.NoError(t, err)
	require.Equal(t, "answer_budget_deferred", answer.ErrorMessage)
	require.Equal(t, "global_day", answer.QualityJudgment.BudgetDeferral.Scope)
	require.Equal(t, "inconclusive", answer.PelicanConfig.AvailabilityResult.Status)
	require.EqualValues(t, 1, handshakes.Load(), "the independent exhausted instance must not open a socket")
	require.EqualValues(t, 2, frames.Load())
	usage, err = budget.Summary(ctx)
	require.NoError(t, err)
	require.Zero(t, usage.ActiveRequests)
	require.EqualValues(t, 4, usage.DayTokens, "prewarm and answer each returned two actual tokens; the handshake returned none")
	require.EqualValues(t, 2, usage.UsageReported)
	require.Zero(t, usage.UsageUnknown)
	require.Zero(t, usage.UsagePending)
	// Remove the request-count constraint: the already pooled connection must
	// still stop at the actual token threshold, without sending another turn.
	policy.RequestsPerDay, policy.TokensPerDay = 30, 4
	policy, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	answer, err = first.RunPelicanBackground(ctx, account.ID, "target", question)
	require.NoError(t, err)
	require.Equal(t, "answer_budget_deferred", answer.ErrorMessage)
	require.Equal(t, "token_limit", answer.QualityJudgment.BudgetDeferral.Reason)
	require.Equal(t, "global_day", answer.QualityJudgment.BudgetDeferral.Scope)
	require.EqualValues(t, 2, frames.Load(), "a reused WS socket is not exempt from token admission")
	policy.Enabled = false
	_, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	answer, err = second.RunPelicanBackground(ctx, account.ID, "target", question)
	require.NoError(t, err)
	require.Equal(t, "success", answer.Status, "%+v", answer)
	usage, err = budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 6, usage.DayUsed, "observe-only mode still accounts for all three attempts")
	require.Zero(t, usage.ActiveRequests)
	beforeRedirect := handshakes.Load()
	redirect.Store(true)
	answer, err = newServices().RunPelicanBackground(ctx, account.ID, "target", question)
	require.NoError(t, err)
	require.Equal(t, "failed", answer.Status)
	require.Nil(t, answer.QualityJudgment, "a rejected upstream redirect is not a local budget deferral")
	require.Zero(t, redirectCalls.Load(), "WS monitoring must not follow a redirect or repeat it as HTTP")
	usage, err = budget.Summary(ctx)
	require.NoError(t, err)
	require.Greater(t, handshakes.Load(), beforeRedirect)
	require.EqualValues(t, 6+handshakes.Load()-beforeRedirect, usage.DayUsed, "each original WS retry must be counted as another handshake")
	require.Zero(t, usage.ActiveRequests)
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
		_, _ = w.Write([]byte(`{"model":"jev-latest","answers":{"quality":{"type":"choice","choice":"correct","confidence":0.95,"probabilities":{"correct":0.96,"incorrect":0.02,"unknown":0.02}}},"usage":{"input_tokens":3,"output_tokens":2}}`))
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
	require.EqualValues(t, 5, usage.DayTokens)
	require.EqualValues(t, 1, usage.UsageReported)
	require.Zero(t, usage.UsageUnknown)
	require.Zero(t, usage.UsagePending)
	require.Zero(t, usage.ActiveRequests)
	// Disabling enforcement does not disable shared accounting or mutate the
	// moderation pool. A second real request is now admitted and still counted.
	policy.Enabled = false
	policy, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	got = second.Judge(ctx, 1, question, "21")
	require.Equal(t, "correct", got.Verdict)
	require.Nil(t, got.BudgetDeferral)
	require.EqualValues(t, 2, calls.Load())
	usage, err = budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 2, usage.DayUsed)
	require.EqualValues(t, 10, usage.DayTokens)
	require.EqualValues(t, 2, usage.UsageReported)
	require.Zero(t, usage.ActiveRequests)
	policy.Enabled, policy.RequestsPerDay, policy.TokensPerDay = true, 100, 10
	policy, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	got = second.Judge(ctx, 1, question, "21")
	require.Equal(t, "unknown", got.Verdict)
	require.Equal(t, "judge_budget_deferred", got.Reason)
	require.NotNil(t, got.BudgetDeferral)
	require.Equal(t, "token_limit", got.BudgetDeferral.Reason)
	require.Equal(t, "global_day", got.BudgetDeferral.Scope)
	require.EqualValues(t, 2, calls.Load(), "JEV token limits must be independent of the request allowance")
	policy.TokensPerDay = 50
	_, err = budget.SaveSettings(ctx, policy)
	require.NoError(t, err)
	got = first.Judge(ctx, 1, question, "21")
	require.Equal(t, "correct", got.Verdict)
	require.Nil(t, got.BudgetDeferral)
	require.EqualValues(t, 3, calls.Load())
	usage, err = budget.Summary(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 15, usage.DayTokens)
	require.EqualValues(t, 3, usage.UsageReported)
	require.Zero(t, usage.UsageUnknown)
	require.Zero(t, usage.UsagePending)
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
		// Limit assertions below require one UTC minute. Leave enough room
		// for their physical SQL calls; rollover has its own explicit test.
		var now time.Time
		require.NoError(t, db.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now))
		remaining := now.UTC().Truncate(time.Minute).Add(time.Minute).Sub(now)
		if remaining < 5*time.Second {
			timer := time.NewTimer(remaining + 100*time.Millisecond)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				require.NoError(t, ctx.Err())
			case <-timer.C:
			}
		}
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
				require.NoError(t, repo.Release(ctx, id, service.MonitoringProbeTokenUsage{}))
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
	t.Run("minute-rollover-retains-day-usage", func(t *testing.T) {
		settings := monitoringBudgetPolicy()
		settings.RequestsPerMinute = 1
		reset(t, settings)
		id, err := repo.Acquire(ctx, request)
		require.NoError(t, err)
		require.NoError(t, repo.Release(ctx, id, service.MonitoringProbeTokenUsage{}))
		_, err = repo.Acquire(ctx, request)
		var denial *service.MonitoringProbeBudgetDenial
		require.ErrorAs(t, err, &denial)
		require.Equal(t, "global_minute", denial.Scope)
		// Move only the minute windows into the past; do not erase the
		// physical attempts or wait on a real wall-clock boundary.
		_, err = db.ExecContext(ctx, `UPDATE monitoring_probe_budget_counters SET bucket_start=bucket_start-INTERVAL '1 minute' WHERE dimension<>'global_day'`)
		require.NoError(t, err)
		id, err = NewMonitoringProbeBudgetRepository(db).Acquire(ctx, request)
		require.NoError(t, err)
		require.NoError(t, repo.Release(ctx, id, service.MonitoringProbeTokenUsage{}))
		summary, err := repo.Summary(ctx)
		require.NoError(t, err)
		require.EqualValues(t, 1, summary.MinuteUsed)
		require.EqualValues(t, 2, summary.DayUsed)
		require.Zero(t, summary.ActiveRequests)
	})
	t.Run("recovery-reserve-and-hard-cap", func(t *testing.T) {
		settings := monitoringBudgetPolicy()
		settings.RequestsPerMinute = 5
		settings.RecoveryReservePercent = 40
		reset(t, settings)
		for range 3 {
			id, err := repo.Acquire(ctx, request)
			require.NoError(t, err)
			require.NoError(t, repo.Release(ctx, id, service.MonitoringProbeTokenUsage{}))
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
			require.NoError(t, repo.Release(ctx, id, service.MonitoringProbeTokenUsage{}))
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
				require.NoError(t, repo.Release(ctx, id, service.MonitoringProbeTokenUsage{}))
			} else {
				require.Equal(t, scope, denial.Scope)
			}
			require.NoError(t, repo.Release(ctx, first, service.MonitoringProbeTokenUsage{}))
			require.NoError(t, repo.Release(ctx, second, service.MonitoringProbeTokenUsage{}))
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
			require.NoError(t, repo.Release(ctx, id, service.MonitoringProbeTokenUsage{}))
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
	policy.RequestsPerDay = 1
	policy = persistLegacyMonitoringProbeBudget(t, db, policy)
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
	policy.RequestsPerDay = 100
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
	require.EqualValues(t, 3, summary.DayUsed)
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
	require.EqualValues(t, 6, summary.DayUsed, "previews count every management request and page, including across a minute boundary")
	_, err = db.ExecContext(t.Context(), `TRUNCATE monitoring_probe_budget_counters,monitoring_probe_budget_leases`)
	require.NoError(t, err)
	calls.Store(0)
	policy := monitoringBudgetPolicy()
	policy.RequestsPerDay = 4
	policy = persistLegacyMonitoringProbeBudget(t, db, policy)
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
	policy.RequestsPerDay = 200
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
	require.EqualValues(t, 10, summary.DayUsed)
	require.Zero(t, summary.ActiveRequests)
}
