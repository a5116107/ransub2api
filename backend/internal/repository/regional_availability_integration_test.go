//go:build integration

package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/regional"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestRegionalAvailabilitySQLSnapshotFaultAndRecovery(t *testing.T) {
	ctx := t.Context()
	var accountID int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,credentials,status,schedulable)
 VALUES('regional-availability','openai','apikey','{"api_key":"fixture-before","base_url":"https://fixture.invalid"}','active',true) RETURNING id`).Scan(&accountID))
	regionID := fmt.Sprintf("availability-%d", time.Now().UnixNano())
	plans := &scheduledTestPlanRepository{db: integrationDB}
	next := time.Now().Add(-time.Second)
	plan, err := plans.Create(ctx, &service.ScheduledTestPlan{AccountID: accountID, ModelID: "target", Enabled: true,
		CronExpression: "0 * * * *", MaxResults: 10, NextRunAt: &next,
		PelicanConfig: &service.PelicanTestConfig{ParallelCount: 1, Availability: &service.AvailabilityPolicy{ValidForSeconds: 60}}})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(context.Background(), "DELETE FROM regional_scheduler_streams WHERE region_id=$1", regionID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(context.Background(), "DELETE FROM scheduled_test_plans WHERE id=$1", plan.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(context.Background(), "DELETE FROM accounts WHERE id=$1", accountID)
		require.NoError(t, err)
	})
	observePass := func() {
		t.Helper()
		_, err := integrationDB.ExecContext(ctx, "UPDATE scheduled_test_plans SET next_run_at=clock_timestamp()-interval '1 second' WHERE id=$1", plan.ID)
		require.NoError(t, err)
		plan, err = plans.GetByID(ctx, plan.ID)
		require.NoError(t, err)
		now := time.Now().UTC().Truncate(time.Microsecond)
		until := now.Add(time.Minute)
		claimed, err := plans.ClaimPelican(ctx, plan, now, until, now.Add(time.Hour))
		require.NoError(t, err)
		require.True(t, claimed)
		finished := now.Add(-time.Millisecond)
		action, err := plans.ApplyAvailabilityOutcomes(ctx, plan, until, []service.AvailabilityModelOutcome{{ModelID: "target",
			Probe: &service.AvailabilityProbeResult{Version: 1, Status: "pass", Reason: "completed_response", AttemptCount: 1,
				StartedAt: finished.Add(-time.Second), FinishedAt: finished,
				Scope: &service.AvailabilityRequestScope{Version: 1, Protocol: "responses_http", Model: "target", Capability: "text", ServiceTier: "unspecified", ReasoningEffort: "unspecified"}}}})
		require.NoError(t, err)
		require.Equal(t, "observed", action)
		require.NoError(t, plans.FinishPelican(ctx, plan.ID, until, now))
	}
	observePass()
	codec, err := regional.NewCodec(map[string][]byte{"test": bytes.Repeat([]byte{8}, 32)})
	require.NoError(t, err)
	policy := regional.Config{RegionID: regionID, MaxSnapshotAgeSeconds: 60}
	validator := service.RegionalSchedulerValidator([]int64{accountID})
	source, err := regional.OpenReplica(filepath.Join(t.TempDir(), "source"), policy, regional.SnapshotScheduler, codec, validator, true)
	require.NoError(t, err)
	statePath := filepath.Join(t.TempDir(), "target")
	target, err := regional.OpenReplica(statePath, policy, regional.SnapshotScheduler, codec, validator, true)
	require.NoError(t, err)
	server := httptest.NewTLSServer(source.Handler())
	t.Cleanup(server.Close)
	authority := NewRegionalSchedulerAuthority(integrationDB)
	_, err = authority.Restore(ctx, regionID, true)
	require.NoError(t, err)
	publish := func(change func()) []byte {
		t.Helper()
		wire, err := authority.Publish(ctx, regionID, func(ctx context.Context, revision uint64, accounts service.AccountRepository, groups service.GroupRepository) ([]byte, error) {
			// Establish the account snapshot before a different transaction
			// changes both credentials and the availability fault counter.
			_, err := accounts.GetByID(ctx, accountID)
			if err != nil {
				return nil, err
			}
			if change != nil {
				change()
			}
			exporter := service.NewSchedulerSnapshotService(nil, nil, accounts, groups, nil)
			payload, err := exporter.ExportRegionalScheduler(ctx, []service.SchedulerBucket{{Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}}, []int64{accountID})
			if err != nil {
				return nil, err
			}
			sum := sha256.Sum256(payload)
			now := time.Now()
			return codec.Seal("test", regional.Snapshot{SchemaVersion: regional.SchemaVersion, RegionID: regionID, Kind: regional.SnapshotScheduler,
				Revision: revision, IssuedAt: now, ExpiresAt: now.Add(time.Minute), Payload: payload, PayloadSHA256: hex.EncodeToString(sum[:])})
		})
		require.NoError(t, err)
		require.NoError(t, source.Accept(ctx, wire))
		require.NoError(t, target.Pull(ctx, server.URL, server.Client().Transport))
		return wire
	}
	oldWire := publish(func() {
		_, err := integrationDB.ExecContext(ctx, `UPDATE accounts SET credentials='{"api_key":"fixture-after","base_url":"https://fixture.invalid"}' WHERE id=$1`, accountID)
		require.NoError(t, err)
		require.NoError(t, (&accountRepository{sql: integrationDB}).ApplyAvailabilityFault(ctx, accountID))
	})
	reader, err := service.NewRegionalSchedulerReader(target)
	require.NoError(t, err)
	read := func() (*service.Account, []*service.AvailabilityReport) {
		t.Helper()
		accounts, err := reader.GetLocalAccounts(ctx, []int64{accountID})
		require.NoError(t, err)
		require.Contains(t, accounts, accountID)
		reports, err := reader.ReadAccountAvailability(ctx, accounts[accountID])
		require.NoError(t, err)
		require.Len(t, reports, 1)
		require.Len(t, reports[0].Models, 1)
		return accounts[accountID], reports
	}
	oldAccount, reports := read()
	require.Equal(t, "fixture-before", oldAccount.Credentials["api_key"])
	require.Equal(t, "pass", reports[0].Models[0].Status, "rules and credentials must share the publication transaction")
	publish(nil)
	_, err = reader.ReadAccountAvailability(ctx, oldAccount)
	require.ErrorIs(t, err, service.ErrAvailabilityRouteChanged)
	current, reports := read()
	require.Equal(t, "fixture-after", current.Credentials["api_key"])
	require.Equal(t, "unknown", reports[0].Models[0].Status)
	observePass()
	publish(nil)
	_, reports = read()
	require.Equal(t, "pass", reports[0].Models[0].Status)
	_, err = integrationDB.ExecContext(ctx, "UPDATE scheduled_test_plans SET enabled=false WHERE id=$1", plan.ID)
	require.NoError(t, err)
	publish(nil)
	_, reports = read()
	require.False(t, reports[0].Enabled)
	require.Equal(t, "unknown", reports[0].Models[0].Status)
	require.Error(t, target.Accept(ctx, oldWire), "an old pass cannot undo a newer paused-rule revision")
	// Startup restores the same revocation, without contacting primary SQL.
	restored, err := regional.OpenReplica(statePath, policy, regional.SnapshotScheduler, codec, validator, false)
	require.NoError(t, err)
	reader, err = service.NewRegionalSchedulerReader(restored)
	require.NoError(t, err)
	_, reports = read()
	require.False(t, reports[0].Enabled)
	// Failure after export must not consume a revision or refresh its lifetime.
	before, err := authority.Restore(ctx, regionID, false)
	require.NoError(t, err)
	_, err = authority.Publish(ctx, regionID, func(context.Context, uint64, service.AccountRepository, service.GroupRepository) ([]byte, error) {
		return nil, errors.New("availability export failed")
	})
	require.Error(t, err)
	after, err := authority.Restore(ctx, regionID, false)
	require.NoError(t, err)
	require.Equal(t, before, after)
}
