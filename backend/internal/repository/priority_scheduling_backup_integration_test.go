//go:build integration

package repository_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Exercise the existing administrator backup wire format, real creation rules,
// SQL persistence and startup snapshot rebuild together. No provider is called.
func TestPriorityAccountBackupRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		cost float64
		auto any
	}{
		{"manual", 0.17, false},
		{"automatic", 0.23, true},
		{"legacy_zero", 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			db, client := repository.UpstreamChannelsTestDatabase(t)
			groups := repository.NewAdminGroupRepository(client, db)
			accounts := repository.NewAdminAccountRepository(client, db, nil)
			cfg := &config.Config{RunMode: config.RunModeStandard}
			settings := service.NewSettingService(repository.NewSettingRepository(client), cfg)
			management := service.NewAdminService(cfg, nil, groups, accounts, repository.NewProxyRepository(client, db),
				nil, nil, nil, nil, nil, nil, nil, nil, client, settings, nil, nil, nil, nil, nil, nil, nil, nil)
			group := &service.Group{Name: "backup", Platform: service.PlatformOpenAI, Status: service.StatusActive, RateMultiplier: 1.4}
			require.NoError(t, groups.Create(ctx, group))
			extra := map[string]any{
				service.AccountCostMultiplierExtraKey: tc.cost,
				"openai_responses_mode":               "http",
				"base_rpm":                            120.0,
				"ordinary_backup_field":               "retained",
			}
			if tc.auto != nil {
				extra[service.AccountCostAutoSyncExtraKey] = tc.auto
			}
			rate, groupRate, loadFactor := 1.25, 1.5, 8
			source, err := management.CreateAccount(ctx, &service.CreateAccountInput{
				Name: "backup source", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "synthetic-backup-key", "base_url": "http://127.0.0.1:1/fixture",
					"model_mapping": map[string]any{"gpt-test": "supplier-model"}},
				Extra: extra, Concurrency: 3, Priority: 27, RateMultiplier: &rate, GroupRateMultiplier: &groupRate, LoadFactor: &loadFactor,
				GroupIDs: []int64{group.ID}, SkipMixedChannelCheck: true,
			})
			require.NoError(t, err)
			// A backup may contain old system state. Import must never promote it
			// to fresh evidence or enable automatic polling on a newly created row.
			require.NoError(t, accounts.UpdateExtra(ctx, source.ID, map[string]any{
				service.UpstreamBillingProbeEnabledExtraKey:    true,
				service.UpstreamBillingRateSyncEnabledExtraKey: true,
				service.UpstreamBillingProbeExtraKey:           map[string]any{"status": "ok", "received_at": time.Now().UTC().Format(time.RFC3339)},
			}))
			h := admin.NewAccountHandler(management, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			router := gin.New()
			router.GET("/accounts/data", h.ExportData)
			router.POST("/accounts/data", h.ImportData)
			router.PUT("/accounts/:id", h.Update)
			router.POST("/accounts/bulk-update", h.BulkUpdate)
			exported := httptest.NewRecorder()
			router.ServeHTTP(exported, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/accounts/data?ids=%d&include_proxies=false", source.ID), nil))
			require.Equal(t, http.StatusOK, exported.Code, exported.Body.String())
			var backup struct {
				Code int               `json:"code"`
				Data admin.DataPayload `json:"data"`
			}
			require.NoError(t, json.Unmarshal(exported.Body.Bytes(), &backup))
			require.Zero(t, backup.Code)
			require.Len(t, backup.Data.Accounts, 1)
			require.Equal(t, tc.cost, backup.Data.Accounts[0].Extra[service.AccountCostMultiplierExtraKey])
			require.Contains(t, backup.Data.Accounts[0].Extra, service.UpstreamBillingProbeExtraKey)
			backup.Data.Accounts[0].Name = "restored backup"
			body, err := json.Marshal(admin.DataImportRequest{Data: backup.Data, GroupIDs: []int64{group.ID}})
			require.NoError(t, err)
			imported := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/accounts/data", bytes.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(imported, request)
			require.Equal(t, http.StatusOK, imported.Code, imported.Body.String())
			var result struct {
				Code int                    `json:"code"`
				Data admin.DataImportResult `json:"data"`
			}
			require.NoError(t, json.Unmarshal(imported.Body.Bytes(), &result))
			require.Zero(t, result.Code)
			require.Equal(t, 1, result.Data.AccountCreated)
			require.Zero(t, result.Data.AccountFailed)
			var restoredID int64
			require.NoError(t, db.QueryRowContext(ctx, `SELECT id FROM accounts WHERE name='restored backup'`).Scan(&restoredID))
			require.NotEqual(t, source.ID, restoredID)
			restored, err := accounts.GetByID(ctx, restoredID)
			require.NoError(t, err)
			require.Equal(t, source.Credentials, restored.Credentials)
			require.Equal(t, tc.cost, restored.CostMultiplier())
			require.Equal(t, tc.auto != false, restored.CostMultiplierAutoSyncEnabled())
			if tc.auto == nil {
				require.NotContains(t, restored.Extra, service.AccountCostAutoSyncExtraKey)
			} else {
				require.Equal(t, tc.auto, restored.Extra[service.AccountCostAutoSyncExtraKey])
			}
			require.Equal(t, rate, *restored.RateMultiplier)
			require.Equal(t, groupRate, *restored.GroupRateMultiplier)
			require.NotNil(t, restored.LoadFactor, "backup must retain a configured scheduler load factor")
			require.Equal(t, loadFactor, *restored.LoadFactor)
			require.Equal(t, source.Concurrency, restored.Concurrency)
			require.Equal(t, source.Priority, restored.Priority)
			require.Equal(t, "retained", restored.Extra["ordinary_backup_field"])
			require.ElementsMatch(t, []int64{group.ID}, restored.GroupIDs)
			for _, key := range []string{service.UpstreamBillingProbeExtraKey, service.UpstreamBillingProbeEnabledExtraKey, service.UpstreamBillingRateSyncEnabledExtraKey} {
				require.NotContains(t, restored.Extra, key, "an imported account needs its own probe evidence")
			}
			// Editing unrelated fields through the normal and bulk HTTP paths
			// must not overwrite the restored scheduling/cost configuration.
			for _, edit := range []struct{ method, path, body string }{
				{http.MethodPut, fmt.Sprintf("/accounts/%d", restoredID), `{"notes":"edited after restore"}`},
				{http.MethodPost, "/accounts/bulk-update", fmt.Sprintf(`{"account_ids":[%d],"priority":31}`, restoredID)},
			} {
				response := httptest.NewRecorder()
				request := httptest.NewRequest(edit.method, edit.path, bytes.NewBufferString(edit.body))
				request.Header.Set("Content-Type", "application/json")
				router.ServeHTTP(response, request)
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				current, err := accounts.GetByID(ctx, restoredID)
				require.NoError(t, err)
				require.Equal(t, restored.Credentials, current.Credentials)
				require.Equal(t, restored.Extra, current.Extra)
				require.Equal(t, restored.LoadFactor, current.LoadFactor)
				require.Equal(t, restored.RateMultiplier, current.RateMultiplier)
				require.Equal(t, restored.GroupRateMultiplier, current.GroupRateMultiplier)
			}
			edited, err := accounts.GetByID(ctx, restoredID)
			require.NoError(t, err)
			require.Equal(t, 31, edited.Priority)
			require.Equal(t, "edited after restore", *edited.Notes)
			for _, invalid := range []struct {
				key   string
				value any
			}{
				{service.AccountCostMultiplierExtraKey, -1},
				{service.AccountCostAutoSyncExtraKey, "false"},
				{"load_factor", 10001},
			} {
				item := backup.Data.Accounts[0]
				item.Name = "invalid backup"
				item.Extra = maps.Clone(item.Extra)
				if invalid.key == "load_factor" {
					item.LoadFactor = new(10001)
				} else {
					item.Extra[invalid.key] = invalid.value
				}
				rejectedData := backup.Data
				rejectedData.Accounts = []admin.DataAccount{item}
				body, err := json.Marshal(admin.DataImportRequest{Data: rejectedData, GroupIDs: []int64{group.ID}})
				require.NoError(t, err)
				response := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, "/accounts/data", bytes.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				router.ServeHTTP(response, request)
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				var rejected struct{ Data admin.DataImportResult }
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &rejected))
				require.Zero(t, rejected.Data.AccountCreated)
				require.Equal(t, 1, rejected.Data.AccountFailed)
			}
			var accountCount int
			require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM accounts`).Scan(&accountCount))
			require.Equal(t, 2, accountCount, "invalid cost settings must not create an account")
			// Rebuild from SQL into a fresh namespace, not a pre-filled snapshot.
			raw := repository.Issue594TestRedis()
			opts := *raw.Options()
			prefix := fmt.Sprintf("priority-backup:%d:%s:", time.Now().UnixNano(), tc.name)
			rdb := repository.NewNamespacedRedisClient(&opts, prefix)
			t.Cleanup(func() {
				keys, err := raw.Keys(context.Background(), prefix+"*").Result()
				require.NoError(t, err)
				if len(keys) > 0 {
					require.NoError(t, raw.Del(context.Background(), keys...).Err())
				}
				require.NoError(t, rdb.Close())
			})
			cache := repository.NewSchedulerCache(rdb)
			reader := repository.NewAccountRepository(client, db, nil)
			snapshot := service.NewSchedulerSnapshotService(cache, nil, reader, groups, cfg)
			snapshot.Start()
			t.Cleanup(snapshot.Stop)
			bucket := service.SchedulerBucket{GroupID: group.ID, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
			var candidates []*service.Account
			require.Eventually(t, func() bool {
				var hit bool
				candidates, hit, err = cache.GetSnapshot(ctx, bucket)
				return err == nil && hit && len(candidates) == 2
			}, 10*time.Second, 20*time.Millisecond, "startup must publish both persisted accounts")
			for _, candidate := range candidates {
				require.Equal(t, tc.cost, candidate.CostMultiplier())
				require.Equal(t, rate, *candidate.RateMultiplier)
				require.Equal(t, groupRate, *candidate.GroupRateMultiplier)
				require.NotNil(t, candidate.LoadFactor)
				require.Equal(t, loadFactor, *candidate.LoadFactor)
				require.Equal(t, "http", candidate.Extra["openai_responses_mode"])
				require.Equal(t, 120.0, candidate.Extra["base_rpm"])
			}
			hydrated, err := cache.GetAccount(ctx, restoredID)
			require.NoError(t, err)
			require.NotNil(t, hydrated)
			require.Equal(t, restored.Credentials, hydrated.Credentials)
			require.Equal(t, restored.Extra, hydrated.Extra)
			reloadedGroup, err := groups.GetByID(ctx, group.ID)
			require.NoError(t, err)
			require.Equal(t, group.RateMultiplier, reloadedGroup.RateMultiplier, "backup import must not rewrite downstream price")
		})
	}
}
