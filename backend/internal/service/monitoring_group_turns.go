package service

import (
	"context"
	"time"
)

// Longer than one bounded queue attempt and its independent result/age saves.
// It is a crash-recovery fence, not permission to bypass the original slot claim.
const GroupMonitoringTurnLeaseTTL = 3 * time.Minute

// GroupMonitoringTurn is private scheduling metadata, never a probe verdict.
// ScopeHash and IdentityHash keep raw models, routes and credentials out of the
// shared queue. The original per-slot claims remain the execution authority.
type GroupMonitoringTurn struct {
	GroupID                 int64
	ScopeHash, IdentityHash string
	Due                     bool
	Served, Tried           int64
	WaitingSince            time.Time
	ClaimUntil              time.Time
}

type GroupMonitoringQueue struct {
	Round int64
	Turns []GroupMonitoringTurn
}

type GroupMonitoringQueueConfig struct {
	Version   int
	IdleProbe *ChannelMonitorV3IdleProbe
}

// Both native V2 and V3 repositories implement this contract. Its failure is
// not permission to recreate process-local ages or change supplier health.
type GroupMonitoringTurnRepository interface {
	SyncGroupMonitoringTurns(context.Context, GroupMonitoringQueueConfig, []GroupMonitoringTurn) (*GroupMonitoringQueue, error)
	ClaimGroupMonitoringTurn(context.Context, GroupMonitoringTurn) (GroupMonitoringTurn, bool, error)
	// A zero round releases an unserved attempt without changing its age.
	FinishGroupMonitoringTurn(context.Context, GroupMonitoringTurn, int64) (bool, error)
}
