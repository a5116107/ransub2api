//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAccountIsSchedulable_QuotaExceeded(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name    string
		account *Account
		want    bool
	}{
		{
			name: "apikey daily quota exceeded",
			account: &Account{
				Status:      StatusActive,
				Schedulable: true,
				Type:        AccountTypeAPIKey,
				Extra: map[string]any{
					"quota_daily_limit": 10.0,
					"quota_daily_used":  10.0,
					"quota_daily_start": now.Add(-1 * time.Hour).Format(time.RFC3339),
				},
			},
			want: false,
		},
		{
			name: "apikey weekly quota exceeded",
			account: &Account{
				Status:      StatusActive,
				Schedulable: true,
				Type:        AccountTypeAPIKey,
				Extra: map[string]any{
					"quota_weekly_limit": 50.0,
					"quota_weekly_used":  50.0,
					"quota_weekly_start": now.Add(-2 * 24 * time.Hour).Format(time.RFC3339),
				},
			},
			want: false,
		},
		{
			name: "apikey total quota exceeded",
			account: &Account{
				Status:      StatusActive,
				Schedulable: true,
				Type:        AccountTypeAPIKey,
				Extra: map[string]any{
					"quota_limit": 100.0,
					"quota_used":  100.0,
				},
			},
			want: false,
		},
		{
			name: "apikey quota not exceeded",
			account: &Account{
				Status:      StatusActive,
				Schedulable: true,
				Type:        AccountTypeAPIKey,
				Extra: map[string]any{
					"quota_daily_limit": 10.0,
					"quota_daily_used":  5.0,
					"quota_daily_start": now.Add(-1 * time.Hour).Format(time.RFC3339),
				},
			},
			want: true,
		},
		{
			name: "apikey expired daily period restores schedulable",
			account: &Account{
				Status:      StatusActive,
				Schedulable: true,
				Type:        AccountTypeAPIKey,
				Extra: map[string]any{
					"quota_daily_limit": 10.0,
					"quota_daily_used":  10.0,
					"quota_daily_start": now.Add(-25 * time.Hour).Format(time.RFC3339),
				},
			},
			want: true,
		},
		{
			name: "oauth ignores quota exceeded",
			account: &Account{
				Status:      StatusActive,
				Schedulable: true,
				Type:        AccountTypeOAuth,
				Extra: map[string]any{
					"quota_daily_limit": 10.0,
					"quota_daily_used":  10.0,
					"quota_daily_start": now.Add(-1 * time.Hour).Format(time.RFC3339),
				},
			},
			want: true,
		},
		{
			name: "bedrock quota exceeded",
			account: &Account{
				Status:      StatusActive,
				Schedulable: true,
				Type:        AccountTypeBedrock,
				Extra: map[string]any{
					"quota_limit": 200.0,
					"quota_used":  200.0,
				},
			},
			want: false,
		},
		{
			name: "service_account total quota exceeded",
			account: &Account{
				Status:      StatusActive,
				Schedulable: true,
				Platform:    PlatformGemini,
				Type:        AccountTypeServiceAccount,
				Extra: map[string]any{
					"quota_limit": 50.0,
					"quota_used":  50.0,
				},
			},
			want: false,
		},
		{
			name: "service_account daily quota exceeded",
			account: &Account{
				Status:      StatusActive,
				Schedulable: true,
				Platform:    PlatformGemini,
				Type:        AccountTypeServiceAccount,
				Extra: map[string]any{
					"quota_daily_limit": 10.0,
					"quota_daily_used":  12.0,
					"quota_daily_start": now.Add(-1 * time.Hour).Format(time.RFC3339),
				},
			},
			want: false,
		},
		{
			name: "service_account without limit stays schedulable regardless of quota_used",
			account: &Account{
				Status:      StatusActive,
				Schedulable: true,
				Platform:    PlatformGemini,
				Type:        AccountTypeServiceAccount,
				Extra: map[string]any{
					"quota_used": 9999.0,
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.account.IsSchedulable())
		})
	}
}

func TestAccountSchedulingControlsAuthorityClockAndRecovery(t *testing.T) {
	at := time.Date(2031, 1, 6, 0, 0, 0, 0, time.UTC) // Monday; intentionally not the host clock.
	for _, tc := range []struct {
		name          string
		mutate        func(*Account)
		before, after string
	}{
		{"expiration", func(a *Account) { a.AutoPauseOnExpired = true; a.ExpiresAt = &at }, "", "account_expired"},
		{"overload", func(a *Account) { a.OverloadUntil = &at }, "account_overloaded", ""},
		{"rate limit", func(a *Account) { a.RateLimitResetAt = &at }, "account_rate_limited", ""},
		{"cooldown", func(a *Account) { a.TempUnschedulableUntil = &at }, "account_cooldown", ""},
		{"rolling day", func(a *Account) {
			a.Extra = map[string]any{"quota_daily_limit": 1, "quota_daily_used": 1, "quota_daily_start": at.Add(-24 * time.Hour).Format(time.RFC3339Nano)}
		}, "account_quota_exceeded", ""},
		{"rolling week", func(a *Account) {
			a.Extra = map[string]any{"quota_weekly_limit": 1, "quota_weekly_used": 1, "quota_weekly_start": at.Add(-7 * 24 * time.Hour).Format(time.RFC3339Nano)}
		}, "account_quota_exceeded", ""},
		{"fixed day UTC fallback", func(a *Account) {
			a.Extra = map[string]any{"quota_daily_limit": 1, "quota_daily_used": 1, "quota_daily_start": at.Add(-time.Hour).Format(time.RFC3339Nano), "quota_daily_reset_mode": "fixed", "quota_reset_timezone": "not-a-timezone"}
		}, "account_quota_exceeded", ""},
		{"fixed week", func(a *Account) {
			a.Extra = map[string]any{"quota_weekly_limit": 1, "quota_weekly_used": 1, "quota_weekly_start": at.Add(-time.Hour).Format(time.RFC3339Nano), "quota_weekly_reset_mode": "fixed"}
		}, "account_quota_exceeded", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Account{Status: StatusActive, Schedulable: true, Type: AccountTypeAPIKey}
			tc.mutate(a)
			require.Equal(t, tc.before, a.schedulingBlockReasonAt(at.Add(-time.Nanosecond)))
			require.Equal(t, tc.after, a.schedulingBlockReasonAt(at))
			require.Equal(t, tc.after, a.schedulingBlockReasonAt(at.Add(time.Nanosecond)))
		})
	}
	a := &Account{Status: StatusDisabled, Schedulable: false, PerformancePaused: true, AutoPauseOnExpired: true, ExpiresAt: &at}
	require.Equal(t, "account_inactive", a.schedulingBlockReasonAt(at))
	a.Status = StatusActive
	require.Equal(t, "scheduling_disabled", a.schedulingBlockReasonAt(at))
	a.Schedulable = true
	require.Equal(t, "performance_paused", a.schedulingBlockReasonAt(at))
	a.PerformancePaused = false
	require.Equal(t, "account_expired", a.schedulingBlockReasonAt(at))
	a.AutoPauseOnExpired = false
	require.Empty(t, a.schedulingBlockReasonAt(at))
}
