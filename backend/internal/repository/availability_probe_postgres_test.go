//go:build unit

package repository

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAvailabilityProbePostgresHistoryRetainsIndependentEvidence(t *testing.T) {
	db := quality5xxPostgresDB(t)
	_, plan := quality5xxPostgresPlan(t, db, service.AccountTypeAPIKey)
	_, err := db.Exec(`CREATE TABLE scheduled_test_results (
 id BIGSERIAL PRIMARY KEY,plan_id BIGINT NOT NULL REFERENCES scheduled_test_plans(id),
 status TEXT,response_text TEXT,error_message TEXT,latency_ms BIGINT,
 started_at TIMESTAMPTZ,finished_at TIMESTAMPTZ,created_at TIMESTAMPTZ,
 pelican_config JSONB,quality_action TEXT,quality_judgment JSONB,quality_round_id TEXT)`)
	require.NoError(t, err)
	repo := NewScheduledTestResultRepository(db)
	started := time.Now().UTC().Truncate(time.Microsecond)
	for _, tc := range []struct{ availability, quality string }{
		{"pass", "incorrect"}, {"pass", "unknown"}, {"fail", "unknown"}, {"inconclusive", "unknown"},
	} {
		t.Run(tc.availability+"/"+tc.quality, func(t *testing.T) {
			cfg := *plan.PelicanConfig
			cfg.AvailabilityResult = &service.AvailabilityProbeResult{Version: 1, Status: tc.availability, Reason: "fixture", StartedAt: started, FinishedAt: started.Add(time.Second), AttemptCount: 1, HTTPStatus: 200}
			saved, err := repo.Create(t.Context(), &service.ScheduledTestResult{PlanID: plan.ID, Status: "failed", StartedAt: started, FinishedAt: started.Add(2 * time.Second), PelicanConfig: &cfg, QualityJudgment: &service.QualityJudgment{Verdict: tc.quality}, QualityRoundID: "fixture-round"})
			require.NoError(t, err)
			loaded, err := repo.GetResult(t.Context(), plan.ID, saved.ID)
			require.NoError(t, err)
			require.Equal(t, cfg.AvailabilityResult, loaded.PelicanConfig.AvailabilityResult)
			require.Equal(t, tc.quality, loaded.QualityJudgment.Verdict)
			require.Equal(t, "failed", loaded.Status)
			items, err := repo.ListByPlanID(t.Context(), plan.ID, 1, false)
			require.NoError(t, err)
			require.Len(t, items, 1)
			require.Empty(t, items[0].ResponseText)
			require.Equal(t, cfg.AvailabilityResult, items[0].PelicanConfig.AvailabilityResult, "lazy body loading must retain structured availability")
		})
	}
}
