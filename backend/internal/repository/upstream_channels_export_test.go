//go:build integration

package repository

import (
	"context"
	"database/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"testing"
)

func UpstreamChannelsTestDatabase(t *testing.T) (*sql.DB, *dbent.Client) {
	t.Helper()
	return newImageUpgradeDatabase(t, context.Background())
}

// Test-only bridge lets the real admin handler consume the native frozen
// quality fixture without introducing a repository/handler import cycle.
func ProcurementQualityTestRound(t *testing.T) (*sql.DB, *service.ScheduledTestPlan, *service.ScheduledTestResult) {
	t.Helper()
	plan, sample := procurementQualityFixture(t)
	saveProcurementQualityRound(t, plan, sample)
	return integrationDB, plan, sample
}
