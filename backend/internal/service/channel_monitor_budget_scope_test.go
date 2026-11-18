//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestChannelMonitorBudgetCredentialScopeWireContracts(t *testing.T) {
	for _, provider := range []string{MonitorProviderOpenAI, MonitorProviderAnthropic, MonitorProviderGemini, MonitorProviderGrok,
		MonitorProviderKimi, MonitorProviderZhipu, MonitorProviderDeepseek, MonitorProviderMiniMax} {
		for _, apiMode := range []string{MonitorAPIModeChatCompletions, MonitorAPIModeResponses} {
			if validateAPIMode(provider, apiMode) != nil {
				continue
			}
			t.Run(provider+"/"+apiMode, func(t *testing.T) {
				swapMonitorHTTPClient(t)
				var actual []string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					actual = config.UpstreamCredentialFingerprints(req)
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"content":[{"type":"text","text":"ok"}],"candidates":[{"content":{"parts":[{"text":"ok"}]}}],"output_text":"ok"}`)
				}))
				defer server.Close()
				m := ChannelMonitor{Provider: provider, APIMode: apiMode, APIKey: "opaque-random-ciphertext",
					ExtraHeaders: map[string]string{"Api-Key": "second-final-fixture", "authorization": "Bearer overridden-fixture"}}
				m.setBudgetCredentialScope("original-fixture")
				keys := ValidatedChannelMonitorBudgetCredentials(&m)
				require.NotEmpty(t, keys)
				_, _, status, err := callProvider(t.Context(), provider, server.URL, "original-fixture", "model-a", "hello", &CheckOptions{APIMode: apiMode, ExtraHeaders: m.ExtraHeaders})
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, status)
				require.ElementsMatch(t, actual, keys, "evidence must match actual wire credentials, not the stored primary key")
				keys[0] = "modified"
				require.NotContains(t, ValidatedChannelMonitorBudgetCredentials(&m), "modified")
				encoded, err := json.Marshal(m.BudgetCredentialScope)
				require.NoError(t, err)
				for _, raw := range []string{"original-fixture", "overridden-fixture", "second-final-fixture", m.APIKey} {
					require.NotContains(t, string(encoded), raw)
				}
			})
		}
	}
}

func TestChannelMonitorBudgetCredentialScopeInvalidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*ChannelMonitor)
	}{
		{"ciphertext", func(m *ChannelMonitor) { m.APIKey += "-rotated" }},
		{"provider", func(m *ChannelMonitor) { m.Provider = MonitorProviderAnthropic }},
		{"api_mode", func(m *ChannelMonitor) { m.APIMode = MonitorAPIModeResponses }},
		{"mode", func(m *ChannelMonitor) { m.CheckMode = MonitorCheckModeQuota }},
		{"headers", func(m *ChannelMonitor) { m.ExtraHeaders["Api-Key"] = "other" }},
		{"version", func(m *ChannelMonitor) { m.BudgetCredentialScope.Version++ }},
		{"source", func(m *ChannelMonitor) { m.BudgetCredentialScope.Source = "stale" }},
		{"duplicate", func(m *ChannelMonitor) {
			m.BudgetCredentialScope.Keys = append(m.BudgetCredentialScope.Keys, m.BudgetCredentialScope.Keys[0])
		}},
		{"bad_key", func(m *ChannelMonitor) { m.BudgetCredentialScope.Keys[0] = config.UnverifiedUpstreamCredential }},
		{"uppercase", func(m *ChannelMonitor) {
			m.BudgetCredentialScope.Keys[0] = strings.ToUpper(m.BudgetCredentialScope.Keys[0])
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := ChannelMonitor{Provider: MonitorProviderOpenAI, APIKey: "cipher-v1", ExtraHeaders: map[string]string{}}
			m.setBudgetCredentialScope("fixture-key")
			require.Len(t, ValidatedChannelMonitorBudgetCredentials(&m), 1)
			tc.edit(&m)
			require.Empty(t, ValidatedChannelMonitorBudgetCredentials(&m))
		})
	}
	m := ChannelMonitor{Provider: MonitorProviderOpenAI, APIKey: "cipher-v1", ExtraHeaders: map[string]string{}}
	m.setBudgetCredentialScope("fixture-key")
	m.Endpoint, m.PrimaryModel, m.IntervalSeconds = "https://other.invalid/tenant/v1", "changed-model", 300
	m.ExtraHeaders[ChannelMonitorDuplicateOperationIDMetadataKey] = "internal-operation"
	require.Len(t, ValidatedChannelMonitorBudgetCredentials(&m), 1, "identity is endpoint/model/cadence independent; demand uses current scheduling inputs")
	m.ExtraHeaders["Authorization"] = "Custom unresolved-fixture"
	m.setBudgetCredentialScope("fixture-key")
	require.Nil(t, m.BudgetCredentialScope, "ambiguous final credentials are unknown, not guessed")
}

type budgetScopeEncryptor struct{ decrypts int }

func (e *budgetScopeEncryptor) Encrypt(v string) (string, error) { return "enc:" + v, nil }
func (e *budgetScopeEncryptor) Decrypt(v string) (string, error) {
	e.decrypts++
	if !strings.HasPrefix(v, "enc:") {
		return "", errors.New("fixture decryption failure")
	}
	return strings.TrimPrefix(v, "enc:"), nil
}

type budgetScopeRepo struct {
	quotaModeRepoStub
	published []*ChannelMonitor
	err       error
	deadline  time.Time
}

func (r *budgetScopeRepo) RecordBudgetCredentialScope(ctx context.Context, expected *ChannelMonitor) (bool, error) {
	r.deadline, _ = ctx.Deadline()
	clone := *expected
	r.published = append(r.published, &clone)
	if r.err != nil {
		return false, r.err
	}
	r.monitor.BudgetCredentialScope = expected.BudgetCredentialScope
	return true, nil
}

func (r *budgetScopeRepo) ListEnabled(context.Context) ([]*ChannelMonitor, error) {
	clone := *r.monitor
	return []*ChannelMonitor{&clone}, nil
}

type budgetScopeRuntime struct{}

func (budgetScopeRuntime) GetChannelMonitorRuntime(context.Context) ChannelMonitorRuntime {
	return ChannelMonitorRuntime{Enabled: true, Mode: ChannelMonitorModeV1}
}

func TestChannelMonitorBudgetCredentialScopeLegacyEntryPoints(t *testing.T) {
	originalPing := monitorPingHTTPClient
	t.Cleanup(func() { monitorPingHTTPClient = originalPing })
	for _, entry := range []string{"startup", "run_check", "publication_failure", "read_only_get", "cancelled"} {
		t.Run(entry, func(t *testing.T) {
			swapMonitorHTTPClient(t)
			monitorPingHTTPClient = monitorHTTPClient
			var requests int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				requests++
				if req.Method == http.MethodHead {
					require.Empty(t, req.Header.Get("Authorization"))
					return
				}
				require.Equal(t, "Bearer fixture-key", req.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": "ok"}}}})
			}))
			defer server.Close()
			m := &ChannelMonitor{ID: 17, Provider: MonitorProviderOpenAI, APIKey: "enc:fixture-key", Enabled: true,
				Endpoint: server.URL, PrimaryModel: "model-a", IntervalSeconds: 60, BodyOverrideMode: MonitorBodyOverrideModeReplace,
				BodyOverride: map[string]any{"model": "model-a", "messages": []any{map[string]string{"role": "user", "content": "hello"}}}}
			repo := &budgetScopeRepo{quotaModeRepoStub: quotaModeRepoStub{monitor: m}}
			cipher := &budgetScopeEncryptor{}
			svc := NewChannelMonitorService(repo, cipher)
			svc.SetRuntimeReader(budgetScopeRuntime{})
			ctx := t.Context()
			if entry == "publication_failure" {
				repo.err = errors.New("storage unavailable")
			}
			if entry == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			switch entry {
			case "startup", "cancelled":
				_, err := svc.ListEnabledMonitors(ctx)
				require.NoError(t, err)
				require.Zero(t, requests)
			case "read_only_get":
				_, err := svc.Get(ctx, m.ID)
				require.NoError(t, err)
				require.Empty(t, repo.published)
				return
			default:
				results, err := svc.RunCheck(ctx, m.ID)
				require.NoError(t, err)
				require.Len(t, results, 1)
				require.Equal(t, MonitorStatusOperational, results[0].Status, results[0].Message)
				require.Equal(t, 2, requests)
			}
			if entry == "cancelled" {
				require.Empty(t, repo.published)
				return
			}
			require.Len(t, repo.published, 1)
			require.Equal(t, "enc:fixture-key", repo.published[0].APIKey)
			require.WithinDuration(t, time.Now().Add(2*time.Second), repo.deadline, time.Second)
			if entry == "publication_failure" {
				require.Nil(t, m.BudgetCredentialScope)
				return
			}
			require.Equal(t, []string{config.UpstreamKeyFingerprint("fixture-key")}, ValidatedChannelMonitorBudgetCredentials(m))
			_, err := svc.ListEnabledMonitors(ctx)
			require.NoError(t, err)
			require.Len(t, repo.published, 1, "unchanged evidence does not add writes")
		})
	}
}

type budgetScopeStartupRepo struct {
	quotaModeRepoStub
	blocked bool
	writes  int
}

func (r *budgetScopeStartupRepo) ListEnabled(context.Context) ([]*ChannelMonitor, error) {
	var monitors []*ChannelMonitor
	for i := int64(1); i <= 3; i++ {
		monitors = append(monitors, &ChannelMonitor{ID: i, Provider: MonitorProviderOpenAI, APIKey: "enc:fixture-key"})
	}
	return monitors, nil
}

func (r *budgetScopeStartupRepo) RecordBudgetCredentialScope(ctx context.Context, _ *ChannelMonitor) (bool, error) {
	r.writes++
	if r.blocked {
		<-ctx.Done()
		return false, ctx.Err()
	}
	return true, nil
}

func TestChannelMonitorBudgetCredentialScopeStartupBatchDeadline(t *testing.T) {
	repo := &budgetScopeStartupRepo{blocked: true}
	svc := NewChannelMonitorService(repo, &budgetScopeEncryptor{})
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	monitors, err := svc.ListEnabledMonitors(ctx)
	require.NoError(t, err)
	require.Len(t, monitors, 3)
	require.Equal(t, 1, repo.writes, "the entire batch shares one publication deadline instead of waiting once per monitor")
	for _, m := range monitors {
		require.Equal(t, "fixture-key", m.APIKey, "failed best-effort publication must not interrupt scheduler decryption")
		require.False(t, m.APIKeyDecryptFailed)
		require.Nil(t, m.BudgetCredentialScope)
	}
	repo.blocked = false
	monitors, err = svc.ListEnabledMonitors(t.Context())
	require.NoError(t, err)
	require.Equal(t, 4, repo.writes, "later loads may backfill all monitors once storage recovers")
	for _, m := range monitors {
		require.NotNil(t, m.BudgetCredentialScope)
		require.Equal(t, []string{config.UpstreamKeyFingerprint("fixture-key")}, m.BudgetCredentialScope.Keys)
	}
}

func TestMonitoringProbeCoveragePeriodicCredentials(t *testing.T) {
	for _, tc := range []string{"shared_v1", "independent_v1", "billing_overlap", "additional", "duplicate", "unknown"} {
		t.Run(tc, func(t *testing.T) {
			policy := DefaultMonitoringProbeBudgetSettings()
			policy.Enabled, policy.KeyRequestsPerMinute, policy.RecoveryReservePercent = true, 2, 80
			billing := UpstreamBillingProbeSettings{IntervalMinutes: 30}
			active := []ChannelMonitor{{Enabled: true, Provider: MonitorProviderOpenAI, APIKey: "cipher-a", Endpoint: "https://relay-a.invalid/tenant/v1", PrimaryModel: "model-a", IntervalSeconds: 60},
				{Enabled: true, Provider: MonitorProviderOpenAI, APIKey: "cipher-b", Endpoint: "https://relay-b.invalid/tenant/v1", PrimaryModel: "model-a", IntervalSeconds: 60}}
			var accounts []MonitoringProbeCoverageAccount
			for i := range active {
				plain := "shared-fixture"
				if tc == "independent_v1" {
					plain += active[i].APIKey
				}
				active[i].setBudgetCredentialScope(plain)
			}
			if tc == "billing_overlap" {
				active = active[:1]
				billing.Enabled = true
				accounts = coverageAccounts(1)
				accounts[0].Account.Credentials["api_key"] = "shared-fixture"
			}
			if tc == "additional" || tc == "duplicate" {
				active = active[:1]
				key := "second-fixture"
				if tc == "duplicate" {
					key = "shared-fixture"
				}
				active[0].ExtraHeaders = map[string]string{"Api-Key": key}
				active[0].setBudgetCredentialScope("shared-fixture")
			}
			if tc == "unknown" {
				active = active[:1]
				active[0].APIKey = "rotated-cipher-without-evidence"
			}
			c := CompileMonitoringProbeCoverage(policy, billing, accounts, true, nil, true, nil, true, active, true, time.Now())
			keys := c.PeriodicCredentials
			require.EqualValues(t, 2880, keys.KeyDayCapacity, "global reserve does not reduce per-key allowance")
			if tc == "shared_v1" || tc == "billing_overlap" {
				require.False(t, c.CanSave)
				require.EqualValues(t, 1, keys.OverLimitCredentialGroups)
				require.Contains(t, c.BlockingReasons, "periodic_key_capacity")
			} else {
				require.True(t, c.CanSave)
				require.Zero(t, keys.OverLimitCredentialGroups)
			}
			if tc == "additional" {
				require.EqualValues(t, 2, keys.CredentialGroups)
				require.EqualValues(t, 5760, keys.ActiveModelAttributionsPerDay+keys.OriginPingAttributionsPerDay)
				require.EqualValues(t, 2880, c.MinimumTotalRequestsPerDay, "attributions do not multiply physical demand")
			}
			if tc == "duplicate" {
				require.EqualValues(t, 1, keys.CredentialGroups)
			}
			if tc == "unknown" {
				require.EqualValues(t, 1, keys.UnresolvedActiveTargets)
				require.Zero(t, keys.CredentialGroups)
				require.Contains(t, c.Warnings, "periodic_key_unresolved")
			}
			encoded, err := json.Marshal(c)
			require.NoError(t, err)
			for _, private := range []string{"shared-fixture", "cipher-a", "relay-a.invalid", config.UpstreamKeyFingerprint("shared-fixture")} {
				require.NotContains(t, string(encoded), private)
			}
			policy.Enabled = false
			require.True(t, CompileMonitoringProbeCoverage(policy, billing, accounts, true, nil, true, nil, true, active, true, time.Now()).CanSave)
		})
	}
}
