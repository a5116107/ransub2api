//go:build integration

package repository

import (
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func readV1ClaimSnapshot(t *testing.T, repo service.ChannelMonitorRepository, m *service.ChannelMonitor) *service.ChannelMonitor {
	t.Helper()
	current, err := repo.GetByID(t.Context(), m.ID)
	require.NoError(t, err)
	return current
}

func TestChannelMonitorV1ScheduledClaimsPostgresBoundaries(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	repo := NewChannelMonitorRepository(client, db)
	claims := repo.(service.ChannelMonitorScheduledRepository)
	m := &service.ChannelMonitor{Name: "V1 claims", Provider: service.MonitorProviderOpenAI,
		APIMode: service.MonitorAPIModeChatCompletions, Endpoint: "https://example.com/tenant/v1", APIKey: "fixture-encrypted",
		PrimaryModel: "model-a", Enabled: true, IntervalSeconds: 60, ExtraHeaders: map[string]string{}}
	require.NoError(t, repo.Create(ctx, m))
	m = readV1ClaimSnapshot(t, repo, m)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var won []*service.ChannelMonitorScheduledClaim
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claim, err := claims.ClaimScheduledCheck(ctx, m, 2*time.Minute, time.Minute)
			if claim == nil {
				var wait *service.ChannelMonitorScheduledWait
				require.ErrorAs(t, err, &wait)
			} else {
				require.NoError(t, err)
			}
			if claim != nil {
				mu.Lock()
				won = append(won, claim)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	require.Len(t, won, 1)
	first := won[0]
	other := *m
	other.ID, other.Name = 0, "V1 independent monitor"
	require.NoError(t, repo.Create(ctx, &other))
	other = *readV1ClaimSnapshot(t, repo, &other)
	otherClaim, err := claims.ClaimScheduledCheck(ctx, &other, 2*time.Minute, 75*time.Second)
	require.NoError(t, err)
	require.NotNil(t, otherClaim, "one monitor's in-flight owner must not serialize independent monitors")
	require.InDelta(t, 75, otherClaim.NextDelay.Seconds(), 2)
	other.PrimaryModel = "model-b"
	require.NoError(t, repo.Update(ctx, &other))
	other.PrimaryModel = "model-a"
	require.NoError(t, repo.Update(ctx, &other))
	otherRenewed, err := claims.RenewScheduledCheck(ctx, other.ID, otherClaim, 2*time.Minute)
	require.NoError(t, err)
	require.False(t, otherRenewed, "A-to-B-to-A configuration saves must not resurrect the A owner's evidence")
	require.NoError(t, claims.ReleaseScheduledCheck(ctx, other.ID, otherClaim, false))
	require.NoError(t, repo.Delete(ctx, other.ID))
	before, err := repo.GetByID(ctx, m.ID)
	require.NoError(t, err)
	require.Nil(t, before.LastCheckedAt, "claiming is not freshness")
	renewed, err := claims.RenewScheduledCheck(ctx, m.ID, first, 2*time.Minute)
	require.NoError(t, err)
	require.True(t, renewed)
	old := time.Now().Add(-time.Hour)
	rows := []*service.ChannelMonitorHistoryRow{{MonitorID: m.ID, Model: "model-a", Status: service.MonitorStatusOperational, CheckedAt: old}}
	finished, err := claims.FinishScheduledCheck(ctx, m.ID, first, rows)
	require.NoError(t, err)
	require.True(t, finished)
	finished, err = claims.FinishScheduledCheck(ctx, m.ID, first, rows)
	require.NoError(t, err)
	require.False(t, finished, "a duplicate completion cannot duplicate history")
	after, err := repo.GetByID(ctx, m.ID)
	require.NoError(t, err)
	require.NotNil(t, after.LastCheckedAt)
	next, err := claims.ClaimScheduledCheck(ctx, after, 2*time.Minute, time.Minute)
	var wait *service.ChannelMonitorScheduledWait
	require.ErrorAs(t, err, &wait)
	require.Positive(t, wait.After)
	require.Nil(t, next, "quick completion or restart must preserve the shared due time")
	_, err = db.ExecContext(ctx, `UPDATE channel_monitor_probe_claims SET next_due_at=clock_timestamp()-interval '1 second' WHERE monitor_id=$1`, m.ID)
	require.NoError(t, err)
	second, err := claims.ClaimScheduledCheck(ctx, after, 2*time.Minute, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, second)
	require.NotEqual(t, first.ID, second.ID)
	require.Equal(t, first.ConfigurationKey, second.ConfigurationKey, "history and internal scope observations cannot change scheduling configuration")
	// A lease that expires inside the finish transaction must roll back both
	// newly inserted history and its freshness update, not just reject later work.
	_, err = db.ExecContext(ctx, `CREATE FUNCTION fixture_expire_v1_claim() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN UPDATE channel_monitor_probe_claims SET lease_expires_at=clock_timestamp()-interval '1 second'
 WHERE monitor_id=NEW.id; RETURN NEW; END $$;
 CREATE TRIGGER fixture_expire_v1 BEFORE UPDATE OF last_checked_at ON channel_monitors
 FOR EACH ROW EXECUTE FUNCTION fixture_expire_v1_claim()`)
	require.NoError(t, err)
	finished, err = claims.FinishScheduledCheck(ctx, m.ID, second, rows)
	require.NoError(t, err)
	require.False(t, finished)
	_, err = db.ExecContext(ctx, `DROP TRIGGER fixture_expire_v1 ON channel_monitors; DROP FUNCTION fixture_expire_v1_claim()`)
	require.NoError(t, err)
	still, err := repo.GetByID(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, after.LastCheckedAt, still.LastCheckedAt)
	rolledBackHistory, err := repo.ListHistory(ctx, m.ID, "", 20)
	require.NoError(t, err)
	require.Len(t, rolledBackHistory, 1)
	require.NoError(t, claims.ReleaseScheduledCheck(ctx, m.ID, first, true))
	renewed, err = claims.RenewScheduledCheck(ctx, m.ID, second, 2*time.Minute)
	require.NoError(t, err)
	require.True(t, renewed, "stale owner must not release successor")
	// A committed model change supersedes a still-live claim; the old response
	// is fenced before its history or freshness transaction can commit.
	after.PrimaryModel = "model-b"
	require.NoError(t, repo.Update(ctx, after))
	after = readV1ClaimSnapshot(t, repo, after)
	renewed, err = claims.RenewScheduledCheck(ctx, m.ID, second, 2*time.Minute)
	require.NoError(t, err)
	require.False(t, renewed)
	third, err := claims.ClaimScheduledCheck(ctx, after, 2*time.Minute, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, third)
	require.NotEqual(t, second.ConfigurationKey, third.ConfigurationKey)
	finished, err = claims.FinishScheduledCheck(ctx, m.ID, second, rows)
	require.NoError(t, err)
	require.False(t, finished)
	require.NoError(t, claims.ReleaseScheduledCheck(ctx, m.ID, second, true))
	renewed, err = claims.RenewScheduledCheck(ctx, m.ID, third, 2*time.Minute)
	require.NoError(t, err)
	require.True(t, renewed)
	// Explicitly expire only this isolated fixture's lease, retaining its
	// future due time: crash recovery must wait for lease expiry, not an hour.
	_, err = db.ExecContext(ctx, `UPDATE channel_monitor_probe_claims SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE monitor_id=$1`, m.ID)
	require.NoError(t, err)
	renewed, err = claims.RenewScheduledCheck(ctx, m.ID, third, 2*time.Minute)
	require.NoError(t, err)
	require.False(t, renewed, "renew must not resurrect an expired claim")
	fourth, err := claims.ClaimScheduledCheck(ctx, after, 2*time.Minute, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, fourth)
	badRows := []*service.ChannelMonitorHistoryRow{{MonitorID: m.ID + 1}}
	finished, err = claims.FinishScheduledCheck(ctx, m.ID, fourth, badRows)
	require.Error(t, err)
	require.False(t, finished)
	require.NoError(t, claims.ReleaseScheduledCheck(ctx, m.ID, fourth, true))
	fifth, err := claims.ClaimScheduledCheck(ctx, after, 2*time.Minute, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, fifth, "cancel/drain permits the original task to recover immediately")
	_, err = db.ExecContext(ctx, `UPDATE channel_monitors SET enabled=false WHERE id=$1`, m.ID)
	require.NoError(t, err)
	finished, err = claims.FinishScheduledCheck(ctx, m.ID, fifth, rows)
	require.NoError(t, err)
	require.False(t, finished)
	history, err := repo.ListHistory(ctx, m.ID, "", 20)
	require.NoError(t, err)
	require.Len(t, history, 1, "only the first valid completion published history")
	require.NoError(t, repo.Delete(ctx, m.ID))
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM channel_monitor_probe_claims WHERE monitor_id=$1`, m.ID).Scan(&count))
	require.Zero(t, count, "monitor deletion must cascade private scheduling state")
}

func TestChannelMonitorV1ClaimMigrationRollbackPreservesData(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	repo := NewChannelMonitorRepository(client, db)
	m := &service.ChannelMonitor{Name: "V1 rollback", Provider: service.MonitorProviderOpenAI, APIMode: service.MonitorAPIModeChatCompletions,
		Endpoint: "https://example.com/v1", APIKey: "fixture-encrypted", PrimaryModel: "model", Enabled: true, IntervalSeconds: 60}
	require.NoError(t, repo.Create(ctx, m))
	m = readV1ClaimSnapshot(t, repo, m)
	require.NoError(t, repo.InsertHistoryBatch(ctx, []*service.ChannelMonitorHistoryRow{{MonitorID: m.ID, Model: "model", Status: service.MonitorStatusOperational, CheckedAt: time.Now()}}))
	before := func() string {
		t.Helper()
		var row string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(m)::text FROM channel_monitors m WHERE id=$1`, m.ID).Scan(&row))
		return row
	}()
	claim, err := repo.(service.ChannelMonitorScheduledRepository).ClaimScheduledCheck(ctx, m, 2*time.Minute, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, claim)
	migration, err := os.ReadFile("../../migrations/351_channel_monitor_probe_claims.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, string(migration))
	require.NoError(t, err, "migration must be idempotent")
	_, err = db.ExecContext(ctx, v1ScheduleRevisionRollbackSQL)
	require.NoError(t, err, "drain and remove 352 triggers before reverting 351")
	_, err = db.ExecContext(ctx, `DROP TABLE channel_monitor_probe_claims`)
	require.NoError(t, err, "rollback applies only inside this task's disposable private database")
	var after string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT row_to_json(m)::text FROM channel_monitors m WHERE id=$1`, m.ID).Scan(&after))
	require.Equal(t, before, after)
	rows, err := repo.ListHistory(ctx, m.ID, "", 20)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	_, err = db.ExecContext(ctx, string(migration))
	require.NoError(t, err)
	revisionMigration, err := os.ReadFile("../../migrations/352_channel_monitor_schedule_revision.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, string(revisionMigration))
	require.NoError(t, err)
	_, err = repo.(service.ChannelMonitorScheduledRepository).ClaimScheduledCheck(ctx, m, 2*time.Minute, time.Minute)
	require.NoError(t, err)
}

func TestChannelMonitorV1TemplateApplyInvalidatesOnlyOwnedClaims(t *testing.T) {
	db, client := newBillingAuditDatabase(t)
	ctx := t.Context()
	templates := NewChannelMonitorRequestTemplateRepository(client, db)
	tpl := &service.ChannelMonitorRequestTemplate{Name: "V1 claim template", Provider: service.MonitorProviderOpenAI,
		APIMode: service.MonitorAPIModeChatCompletions, BodyOverrideMode: service.MonitorBodyOverrideModeOff,
		ExtraHeaders: map[string]string{"X-Fixture": "template"}}
	require.NoError(t, templates.Create(ctx, tpl))
	repo := NewChannelMonitorRepository(client, db)
	claims := repo.(service.ChannelMonitorScheduledRepository)
	linked := &service.ChannelMonitor{Name: "V1 linked", Provider: service.MonitorProviderOpenAI,
		APIMode: service.MonitorAPIModeChatCompletions, Endpoint: "https://example.com/v1", APIKey: "fixture-encrypted",
		PrimaryModel: "model", Enabled: true, IntervalSeconds: 60, TemplateID: &tpl.ID}
	require.NoError(t, repo.Create(ctx, linked))
	unrelated := *linked
	unrelated.ID, unrelated.TemplateID, unrelated.Name = 0, nil, "V1 unrelated"
	require.NoError(t, repo.Create(ctx, &unrelated))
	linked = readV1ClaimSnapshot(t, repo, linked)
	unrelated = *readV1ClaimSnapshot(t, repo, &unrelated)
	linkedClaim, err := claims.ClaimScheduledCheck(ctx, linked, 2*time.Minute, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, linkedClaim)
	unrelatedClaim, err := claims.ClaimScheduledCheck(ctx, &unrelated, 2*time.Minute, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, unrelatedClaim)
	ids := []int64{linked.ID, unrelated.ID}
	count, err := templates.ApplyToMonitors(ctx, tpl.ID, ids)
	require.NoError(t, err)
	require.EqualValues(t, 1, count)
	owned, err := claims.RenewScheduledCheck(ctx, linked.ID, linkedClaim, 2*time.Minute)
	require.NoError(t, err)
	require.False(t, owned)
	owned, err = claims.RenewScheduledCheck(ctx, unrelated.ID, unrelatedClaim, 2*time.Minute)
	require.NoError(t, err)
	require.True(t, owned, "unrelated selected IDs must not lose their scheduling ownership")
	linked, err = repo.GetByID(ctx, linked.ID)
	require.NoError(t, err)
	newClaim, err := claims.ClaimScheduledCheck(ctx, linked, 2*time.Minute, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, newClaim)
	_, err = templates.ApplyToMonitors(ctx, tpl.ID, ids)
	require.NoError(t, err)
	owned, err = claims.RenewScheduledCheck(ctx, linked.ID, newClaim, 2*time.Minute)
	require.NoError(t, err)
	require.False(t, owned, "re-applying identical template values is a new original schedule generation")
}
