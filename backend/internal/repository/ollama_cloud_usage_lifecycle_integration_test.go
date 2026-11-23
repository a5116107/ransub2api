//go:build integration

package repository

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOllamaCloudUsageLifecycleHTTPPostgresCancelAndRecovery(t *testing.T) {
	for _, entry := range []string{"background", "external_due"} {
		t.Run(entry, func(t *testing.T) {
			db, client := newBillingAuditDatabase(t)
			ctx := t.Context()
			body, err := os.ReadFile("../service/testdata/ollama_settings_usage.html")
			require.NoError(t, err)
			var calls, canceled, invalid atomic.Int64
			var block atomic.Bool
			block.Store(true)
			entered := make(chan struct{}, 4)
			upstream := ollamaBudgetTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/settings" || r.Host != "ollama.com" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") == "" {
					invalid.Add(1)
					http.Error(w, "unexpected settings request", http.StatusBadRequest)
					return
				}
				calls.Add(1)
				if block.Load() {
					entered <- struct{}{}
					<-r.Context().Done()
					canceled.Add(1)
					return
				}
				w.Header().Set("Content-Type", "text/html")
				_, _ = w.Write(body)
			})
			settingsRepo := NewSettingRepository(client)
			require.NoError(t, settingsRepo.Set(ctx, service.SettingKeyOllamaCloudUsageSettings, `{"enabled":true,"interval_minutes":60,"debounce_minutes":1}`))
			budget := NewMonitoringProbeBudgetRepository(db)
			_, err = budget.SaveSettings(ctx, monitoringBudgetPolicy())
			require.NoError(t, err)
			old, lastUsed := time.Now().UTC().Add(-2*time.Hour), time.Now().UTC().Add(-2*time.Minute)
			rows := make(map[int64]string)
			var first int64
			for i := range 20 {
				account := mustCreateAccount(t, client, &service.Account{Name: fmt.Sprintf("ollama-lifecycle-%d", i), Platform: service.PlatformOpenAI,
					Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 2, LastUsedAt: &lastUsed,
					Credentials: map[string]any{"api_key": fmt.Sprintf("fixture-inference-%d", i), "base_url": "https://ollama.com"},
					Extra: map[string]any{service.OllamaCloudUsageSessionExtraKey: fmt.Sprintf("wos-session=lifecycle-%d", i),
						service.OllamaCloudUsageAutoRefreshExtraKey: true,
						service.OllamaCloudUsageSnapshotExtraKey: &service.OllamaCloudUsageSnapshot{Status: service.OllamaCloudUsageStatusOK,
							Data: &service.OllamaCloudUsageData{Plan: "previous"}, FetchedAt: &old, LastAttemptAt: old, NextRefreshAt: old}}})
				var row string
				require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, account.ID).Scan(&row))
				rows[account.ID] = row
				if first == 0 {
					first = account.ID
				}
			}
			newRunner := func() *service.OllamaCloudUsageService {
				accounts := newAccountRepositoryWithSQL(client, db, nil)
				runner := service.NewOllamaCloudUsageService(accounts, upstream, service.NewSettingService(settingsRepo, nil), ollamaBudgetFixtureEncryptor{}, true)
				runner.SetMonitoringBudget(service.NewMonitoringProbeBudgetService(NewMonitoringProbeBudgetRepository(db)))
				t.Cleanup(runner.Stop)
				return runner
			}
			runner := newRunner()
			done := make(chan error, 1)
			if entry == "background" {
				runner.Start()
			} else {
				go func() { done <- runner.RunDue(ctx) }()
			}
			for range 4 {
				select {
				case <-entered:
				case <-time.After(10 * time.Second):
					t.Fatal("original Ollama runner did not enter four physical requests")
				}
			}
			stopped := make(chan struct{})
			go func() { runner.Stop(); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				t.Fatal("Ollama automatic work did not cancel and drain")
			}
			if entry == "external_due" {
				require.ErrorIs(t, <-done, context.Canceled)
			}
			require.Eventually(t, func() bool { return canceled.Load() == 4 }, 5*time.Second, 5*time.Millisecond)
			require.EqualValues(t, 4, calls.Load(), "retired queued work must not send")
			require.ErrorIs(t, runner.RunDue(ctx), context.Canceled)
			for id, before := range rows {
				var after string
				require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, id).Scan(&after))
				require.Equal(t, before, after, "cancellation must preserve prior activity, evidence and session")
			}
			summary, err := budget.Summary(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 4, summary.DayUsed, "already sent calls must not be refunded")
			require.Zero(t, summary.ActiveRequests)
			require.Zero(t, summary.UsagePending)
			block.Store(false)
			state, err := runner.Refresh(ctx, first)
			require.NoError(t, err, "manual refresh retains its caller lifetime after automatic shutdown")
			require.Equal(t, service.OllamaCloudUsageStatusOK, state.Snapshot.Status)
			require.NoError(t, newRunner().RunDue(ctx))
			require.EqualValues(t, 24, calls.Load(), "only the remaining nineteen due accounts recover automatically")
			var fresh, failed, usage int64
			require.NoError(t, db.QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM accounts WHERE (extra#>>'{ollama_cloud_usage_snapshot,last_attempt_at}')::timestamptz>$1),
 (SELECT count(*) FROM accounts WHERE extra#>>'{ollama_cloud_usage_snapshot,status}'<>'ok'),
 (SELECT count(*) FROM usage_logs)`, old).Scan(&fresh, &failed, &usage))
			require.EqualValues(t, 20, fresh)
			require.Zero(t, failed)
			require.Zero(t, usage)
			summary, err = budget.Summary(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 24, summary.DayUsed)
			require.Zero(t, summary.DayTokens)
			require.Zero(t, summary.UsageReported)
			require.Zero(t, summary.UsageUnknown, "management-only cancellation must not create generation usage")
			require.Zero(t, summary.UsagePending)
			require.Zero(t, summary.ActiveRequests)
			require.Zero(t, invalid.Load())
		})
	}
}
