package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

func TestUpstreamBillingProbePrioritizesRechecksAndEvidenceDeadlines(t *testing.T) {
	now := time.Date(2026, time.November, 24, 12, 0, 0, 123456000, time.UTC)
	for _, scenario := range []string{"failure_before_exploration", "refresh_before_exploration", "earliest_evidence_deadline"} {
		t.Run(scenario, func(t *testing.T) {
			repo := &upstreamBillingProbeAccountRepo{accounts: make(map[int64]*Account)}
			for id := int64(1); id <= upstreamBillingProbeMaxPerCycle+1; id++ {
				a := &Account{ID: id, Name: fmt.Sprintf("priority-%d", id), Platform: PlatformOpenAI,
					Type: AccountTypeAPIKey, Status: StatusActive, Concurrency: 1,
					Credentials: map[string]any{"api_key": fmt.Sprintf("priority-fixture-%d", id), "base_url": "https://upstream.example"},
					CreatedAt:   now.Add(-24 * time.Hour), Extra: map[string]any{UpstreamBillingProbeEnabledExtraKey: true}}
				if scenario == "earliest_evidence_deadline" || id == upstreamBillingProbeMaxPerCycle+1 {
					snapshot := &UpstreamBillingProbeSnapshot{Status: UpstreamBillingProbeStatusOK, NextProbeAt: now.Add(-time.Minute),
						ReceivedAt: probeTimePtr(now.Add(-time.Hour)), FreshUntil: probeTimePtr(now.Add(time.Hour)), LastAttemptAt: now.Add(-time.Hour)}
					if scenario == "failure_before_exploration" {
						snapshot.Status = UpstreamBillingProbeStatusFailed
					}
					if scenario == "earliest_evidence_deadline" && id == upstreamBillingProbeMaxPerCycle+1 {
						snapshot.FreshUntil = probeTimePtr(now.Add(-time.Minute))
					}
					a.Extra[UpstreamBillingProbeExtraKey] = snapshot
				}
				repo.accounts[id] = a
			}
			upstream := &upstreamBillingProbeHTTPStub{}
			runner := newUpstreamBillingProbeTestService(repo, upstream, &upstreamBillingProbeSettingRepo{})
			runner.now = func() time.Time { return now }
			require.NoError(t, runner.RunDue(t.Context()))
			selected := decodeUpstreamBillingProbeSnapshot(repo.accounts[upstreamBillingProbeMaxPerCycle+1].Extra)
			require.NotNil(t, selected)
			require.Equal(t, now, selected.LastAttemptAt, "the required recheck/earliest deadline must be admitted before the twentieth exploration/less urgent check")
			require.EqualValues(t, upstreamBillingProbeMaxPerCycle, upstream.calls.Load())
			require.EqualValues(t, upstreamBillingProbeMaxPerCycle, upstream.balanceCalls.Load())
		})
	}
}

func TestUpstreamBillingProbePriorityPreservesSchedulingWithDamagedEvidence(t *testing.T) {
	now := time.Date(2026, time.November, 24, 12, 0, 0, 123456000, time.UTC)
	for _, tc := range []struct {
		name                    string
		snapshot                map[string]any
		recheck                 bool
		dueOffset, urgentOffset time.Duration
	}{
		{"valid_expiry", map[string]any{"received_at": now.Add(-time.Hour), "fresh_until": now.Add(-time.Minute)}, true, -2 * time.Minute, -time.Minute},
		{"invalid_received", map[string]any{"received_at": "invalid", "fresh_until": now.Add(-time.Minute)}, true, -2 * time.Minute, -2 * time.Minute},
		{"future_received", map[string]any{"received_at": now.Add(time.Minute), "fresh_until": now.Add(time.Hour)}, true, -2 * time.Minute, -2 * time.Minute},
		{"reversed_window", map[string]any{"received_at": now.Add(-time.Hour), "fresh_until": now.Add(-2 * time.Hour)}, true, -2 * time.Minute, -2 * time.Minute},
		{"invalid_fresh", map[string]any{"received_at": now.Add(-time.Hour), "fresh_until": "2026-99-99T12:00:00Z"}, true, -2 * time.Minute, -2 * time.Minute},
		{"future_retry_survives_bad_evidence", map[string]any{"next_probe_at": now.Add(time.Hour), "received_at": false}, true, time.Hour, time.Hour},
		{"zero_next_is_exploration", map[string]any{"next_probe_at": time.Time{}}, false, -24 * time.Hour, -24 * time.Hour},
		{"invalid_next_is_exploration", map[string]any{"next_probe_at": "2026-99-99T12:00:00Z"}, false, -24 * time.Hour, -24 * time.Hour},
		{"unsupported_is_exploration", map[string]any{"status": UpstreamBillingProbeStatusUnsupported}, false, -2 * time.Minute, -2 * time.Minute},
		{"unknown_status_is_exploration", map[string]any{"status": "future-version", "next_probe_at": now.Add(time.Hour)}, false, -24 * time.Hour, -24 * time.Hour},
		{"nano_times_share_sql_precision", map[string]any{"next_probe_at": now.Add(-2*time.Minute + 789*time.Nanosecond)}, true, -2 * time.Minute, -2 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := map[string]any{"status": UpstreamBillingProbeStatusOK, "next_probe_at": now.Add(-2 * time.Minute)}
			for key, value := range tc.snapshot {
				snapshot[key] = value
			}
			account := &Account{CreatedAt: now.Add(-24 * time.Hour), Extra: map[string]any{UpstreamBillingProbeExtraKey: snapshot}}
			before, err := json.Marshal(account.Extra)
			require.NoError(t, err)
			priority := upstreamBillingProbePriority(account, now)
			require.Equal(t, tc.recheck, priority.recheck)
			require.True(t, now.Add(tc.dueOffset).Equal(priority.dueAt), "due=%v", priority.dueAt)
			require.True(t, now.Add(tc.urgentOffset).Equal(priority.urgentAt), "urgency=%v", priority.urgentAt)
			after, err := json.Marshal(account.Extra)
			require.NoError(t, err)
			require.Equal(t, before, after, "queue priority must not renew evidence or rewrite history")
		})
	}
}

func TestUpstreamBillingProbeDueAtPreservesIndependentBudgetClock(t *testing.T) {
	now := time.Date(2026, time.November, 24, 12, 0, 0, 123456000, time.UTC)
	for _, tc := range []struct {
		name     string
		deferred map[string]any
		want     time.Time
	}{
		{"future_retry", map[string]any{"retry_at": now.Add(time.Hour)}, now.Add(time.Hour)},
		{"damaged_reason", map[string]any{"reason": 123, "retry_at": now.Add(time.Hour)}, now.Add(time.Hour)},
		{"nanoseconds", map[string]any{"retry_at": now.Add(time.Hour + 789*time.Nanosecond)}, now.Add(time.Hour + 789*time.Nanosecond)},
		{"invalid_calendar", map[string]any{"retry_at": "2026-99-99T12:00:00Z"}, now.Add(-time.Minute)},
		{"zero_year", map[string]any{"retry_at": "0000-01-01T00:00:00Z"}, now.Add(-time.Minute)},
		{"non_rfc3339_fraction", map[string]any{"retry_at": "2026-11-24T13:00:00,123456Z"}, now.Add(-time.Minute)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{Extra: map[string]any{UpstreamBillingProbeExtraKey: map[string]any{
				"status": UpstreamBillingProbeStatusFailed, "next_probe_at": now.Add(-time.Minute), "received_at": false},
				UpstreamBillingProbeDeferralExtraKey: tc.deferred}}
			require.True(t, tc.want.Equal(upstreamBillingProbeDueAt(account)))
			require.True(t, tc.want.Truncate(time.Microsecond).Equal(upstreamBillingProbePriority(account, now).dueAt),
				"only ordering loses sub-microsecond precision; hard retry windows stay intact")
		})
	}
	require.True(t, upstreamBillingProbeDueAt(nil).IsZero())
}

type upstreamBillingPriorityHTTP struct {
	*upstreamBillingProbeHTTPStub
	onRequest func(int64)
}

func (u *upstreamBillingPriorityHTTP) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	u.onRequest(accountID)
	return u.upstreamBillingProbeHTTPStub.DoWithTLS(req, proxyURL, accountID, concurrency, profile)
}

func TestUpstreamBillingProbeRunnerCompletesRequiredRechecksBeforeExploration(t *testing.T) {
	now := time.Now().UTC()
	repo := &upstreamBillingProbeAccountRepo{accounts: make(map[int64]*Account)}
	for id := int64(1); id <= 8; id++ {
		repo.accounts[id] = &Account{ID: id, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive,
			CreatedAt: now.Add(-time.Hour), Credentials: map[string]any{"api_key": fmt.Sprintf("priority-fixture-%d", id), "base_url": "https://upstream.example"},
			Extra: map[string]any{UpstreamBillingProbeEnabledExtraKey: true}}
	}
	const recheckID int64 = 8
	repo.accounts[recheckID].Extra[UpstreamBillingProbeExtraKey] = &UpstreamBillingProbeSnapshot{
		Status: UpstreamBillingProbeStatusFailed, NextProbeAt: now.Add(-time.Minute)}
	var recheckRequests atomic.Int64
	var explorationBeforeRecheck atomic.Bool
	upstream := &upstreamBillingPriorityHTTP{upstreamBillingProbeHTTPStub: &upstreamBillingProbeHTTPStub{}, onRequest: func(id int64) {
		if id == recheckID {
			// Both the declaration and balance request belong to the necessary
			// review. Keep that review active so racing exploration is observable.
			time.Sleep(20 * time.Millisecond)
			recheckRequests.Add(1)
		} else if recheckRequests.Load() < 2 {
			explorationBeforeRecheck.Store(true)
		}
	}}
	runner := newUpstreamBillingProbeTestService(repo, upstream, &upstreamBillingProbeSettingRepo{})
	runner.now = func() time.Time { return now }
	require.NoError(t, runner.RunDue(t.Context()))
	require.False(t, explorationBeforeRecheck.Load(), "new inventory cannot consume physical quota while an admitted necessary review is unfinished")
	require.EqualValues(t, 8, upstream.calls.Load())
	require.EqualValues(t, 8, upstream.balanceCalls.Load())
}
