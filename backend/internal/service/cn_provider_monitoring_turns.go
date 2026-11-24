package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"time"
)

// CNMonitoringTurn is scheduling state only. An admitted attempt consumes a
// turn even when the provider fails; these ordinals never certify freshness.
type CNMonitoringTurn struct {
	AccountID    int64
	IdentityHash string
	Served       int64
	Tried        int64
}

type CNMonitoringQueue struct {
	Round int64
	Turns map[int64]CNMonitoringTurn
}

// The production account repository owns this private state. Missing or failed
// storage stops automatic dispatch rather than recreating a process-local age.
type CNMonitoringTurnRepository interface {
	SyncCNMonitoringTurns(context.Context, []CNMonitoringTurn, bool) (*CNMonitoringQueue, error)
	ClaimCNMonitoringTurn(context.Context, CNMonitoringTurn) (CNMonitoringTurn, bool, error)
	FinishCNMonitoringTurn(context.Context, CNMonitoringTurn, int64) (bool, error)
}

func cnMonitoringTurn(target accountProbeTarget) (CNMonitoringTurn, error) {
	a := target.account
	var proxy *Proxy
	if a.Proxy != nil {
		copy := *a.Proxy
		copy.Name, copy.ExpiryWarnDays = "", 0
		copy.CreatedAt, copy.UpdatedAt = time.Time{}, time.Time{}
		proxy = &copy
	}
	// Private routing/credential values contribute only to a one-way digest.
	// Observation fields and schedulable state do not reset a turn; they change
	// on routine monitoring and may describe a recoverable paused account.
	raw, err := json.Marshal(struct {
		Platform, Type string
		Credentials    map[string]any
		ProxyID        *int64
		Proxy          *Proxy
		Balance        bool
	}{a.Platform, a.Type, a.Credentials, a.ProxyID, proxy, target.balance})
	if err != nil {
		return CNMonitoringTurn{}, err
	}
	digest := sha256.Sum256(raw)
	return CNMonitoringTurn{AccountID: a.ID, IdentityHash: hex.EncodeToString(digest[:])}, nil
}

func (s *CNProviderBalanceCheckService) observeTurnCoordination(available bool) {
	s.lifecycleMu.Lock()
	s.turnCoordinationFailed = !available
	s.lifecycleMu.Unlock()
}

// Only native observations of this route/provider may establish a required
// cadence review. This is queue metadata, not a health or freshness verdict;
// a quota reset is not evidence expiry. Missing data remains routine work.
func cnMonitoringReviewAt(target accountProbeTarget, interval time.Duration, now time.Time) time.Time {
	a := target.account
	if a == nil || interval <= 0 {
		return time.Time{}
	}
	provider := a.Platform
	if !target.balance {
		provider = a.GetCodingPlanProvider()
		if provider == "" {
			return time.Time{}
		}
	}
	var earliest time.Time
	observe := func(suffix string, usable bool) {
		if !usable {
			return
		}
		stamp, ok := a.Extra[cnExtraKey(provider, suffix)].(string)
		if !ok {
			return
		}
		observed, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil || observed.Unix() <= 0 || observed.After(now) {
			return
		}
		due := observed.Add(interval)
		if due.After(now) {
			return
		}
		if earliest.IsZero() || due.Before(earliest) {
			earliest = due
		}
	}
	validNumber := func(suffix string, nonnegative bool) bool {
		n, ok := cnParseF64(a.Extra[cnExtraKey(provider, suffix)])
		return ok && !math.IsNaN(n) && !math.IsInf(n, 0) && (!nonnegative || n >= 0)
	}
	if target.balance || provider == PlatformCommandCode || provider == PlatformCline || provider == PlatformMistral {
		observe(cnBalanceExtraSuffixUpdated, validNumber(cnBalanceExtraSuffixBalance, false))
	}
	if !target.balance {
		usable := validNumber(cnExtraSuffix5hUsed, true) || validNumber(cnExtraSuffixWeeklyUsed, true) ||
			(cnQuotaHasMonthlyWindow(provider) || provider == PlatformCline) && validNumber(cnExtraSuffixMonthlyUsed, true)
		observe(cnExtraSuffixUsageUpdated, usable)
	}
	return earliest
}
