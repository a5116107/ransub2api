package service

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMonitoringUsageRequiredRefreshPrecedesExploration(t *testing.T) {
	for _, provider := range []string{"ollama", "opencode"} {
		t.Run(provider, func(t *testing.T) {
			now := time.Now().UTC()
			var completed, outOfOrder atomic.Bool
			var entered atomic.Int64
			before := func(req *http.Request) {
				if req.Header.Get("Authorization") == "Bearer key-8" || req.Header.Get("Cookie") == "wos-session=priority-8" {
					time.Sleep(30 * time.Millisecond)
					completed.Store(true)
				} else if !completed.Load() {
					outOfOrder.Store(true)
				}
				entered.Add(1)
			}
			accounts := make(map[int64]*Account)
			var due []Account
			for id := int64(1); id <= 8; id++ {
				a := openCodeGoUsageAccount(id)
				if provider == "ollama" {
					a = ollamaUsageAccount(id)
					a.Extra[OllamaCloudUsageSessionExtraKey] = fmt.Sprintf("cipher:wos-session=priority-%d", id)
					a.Extra[OllamaCloudUsageAutoRefreshExtraKey] = true
				} else {
					a.Extra[OpenCodeGoUsageAutoRefreshExtraKey] = true
				}
				a.CreatedAt = now.Add(-24 * time.Hour)
				if id == 8 {
					old, activity := now.Add(-time.Hour), now.Add(-10*time.Minute)
					a.LastUsedAt = &activity
					if provider == "ollama" {
						a.Extra[OllamaCloudUsageSnapshotExtraKey] = &OllamaCloudUsageSnapshot{Status: OllamaCloudUsageStatusOK, FetchedAt: &old, LastAttemptAt: old}
					} else {
						a.Extra[OpenCodeGoUsageSnapshotExtraKey] = &OpenCodeGoUsageSnapshot{Status: OpenCodeGoUsageStatusOK, FetchedAt: &old, LastAttemptAt: old}
					}
				}
				accounts[id], due = a, append(due, *a)
			}
			if provider == "ollama" {
				repo := &ollamaUsageTestRepo{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: accounts}, due: due}
				upstream := &ollamaUsageHTTPStub{body: ollamaUsageFixture(t), beforeResponse: before}
				settings := &upstreamBillingProbeSettingRepo{values: map[string]string{SettingKeyOllamaCloudUsageSettings: `{"enabled":true,"interval_minutes":15}`}}
				require.NoError(t, newOllamaUsageTestService(t, repo, upstream, settings, true).RunDue(t.Context()))
			} else {
				repo := &openCodeGoUsageTestRepo{accounts: accounts, due: due}
				upstream := &openCodeGoUsageHTTPStub{body: []byte(openCodeGoUsageFixture), beforeResponse: before}
				settings := &upstreamBillingProbeSettingRepo{values: map[string]string{SettingKeyOpenCodeGoUsageSettings: `{"enabled":true,"interval_minutes":15}`}}
				require.NoError(t, newOpenCodeGoUsageTestService(t, repo, upstream, settings).RunDue(t.Context()))
			}
			require.EqualValues(t, 8, entered.Load())
			require.True(t, completed.Load())
			require.False(t, outOfOrder.Load(), "exploration must wait for the required refresh, including in an unsorted repository fallback")
		})
	}
}

func TestMonitoringUsageProbePhasesUsePersistedAgeWithoutMutation(t *testing.T) {
	now := time.Date(2026, time.November, 24, 12, 0, 0, 123456000, time.UTC)
	accounts := []Account{{ID: 1, CreatedAt: now.Add(-time.Hour)}, {ID: 2, CreatedAt: now.Add(-2 * time.Hour)},
		{ID: 3}, {ID: 4}, {ID: 5}}
	due := map[int64]time.Time{3: now.Add(-time.Minute), 4: now.Add(-2*time.Minute + 789*time.Nanosecond), 5: now.Add(-2*time.Minute + 123*time.Nanosecond)}
	phases := monitoringUsageProbePhases(accounts, due)
	require.Equal(t, []int64{4, 5, 3}, []int64{phases[0][0].ID, phases[0][1].ID, phases[0][2].ID})
	require.Equal(t, []int64{2, 1}, []int64{phases[1][0].ID, phases[1][1].ID})
	require.Equal(t, now.Add(-2*time.Minute+789*time.Nanosecond), due[4], "only sorting truncates; hard attempt clocks stay exact")
	require.Len(t, monitoringUsageProbePhases(nil, nil), 2)
}
