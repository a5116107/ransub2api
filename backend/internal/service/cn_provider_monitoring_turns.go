package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
