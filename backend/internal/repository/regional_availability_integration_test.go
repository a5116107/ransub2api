//go:build integration

package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/regional"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRegionalAvailabilitySQLSnapshotFaultAndRecovery(t *testing.T) {
	for _, platform := range []string{service.PlatformOpenAI, service.PlatformAnthropic, service.PlatformGemini} {
		t.Run(platform, func(t *testing.T) {
			scope := service.AvailabilityRequestScope{Version: 2, Protocol: "responses_http", Model: "target", Capability: "text", ServiceTier: "unspecified", ReasoningEffort: "unspecified"}
			switch platform {
			case service.PlatformAnthropic:
				scope.Protocol, scope.ThinkingMode, scope.ThinkingBudget = "messages_http", "enabled", 2048
				scope.ProtocolOptions = strings.Repeat("a", 64)
			case service.PlatformGemini:
				scope.Protocol, scope.Stream = "gemini_http", true
				scope.ProtocolOptions = strings.Repeat("b", 64)
			}
			testRegionalAvailabilitySQLSnapshotFaultAndRecovery(t, platform, scope)
		})
	}
}

type geminiAvailabilitySQLUpstream struct {
	client *http.Client
	body   []byte
}

func (u *geminiAvailabilitySQLUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	var err error
	u.body, err = io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(u.body))
	return u.client.Do(req)
}

func (u *geminiAvailabilitySQLUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

func TestAvailabilityGeminiSemanticFaultSQLRecovery(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		fault         bool
	}{
		{"service", `{"error":{"code":503,"status":"UNAVAILABLE"}}`, true},
		{"quota", `{"error":{"code":429}}`, true},
		{"authentication", `{"error":{"code":401}}`, true},
		{"empty", `{}`, true},
		{"policy", `{"promptFeedback":{"blockReason":"SAFETY"}}`, false},
		{"unknown", `{"error":{"message":"fixture"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			var sends atomic.Int32
			var fail atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sends.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				if fail.Load() {
					_, _ = fmt.Fprintf(w, "data: %s\n\n", tc.payload)
					return
				}
				_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"21\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":1}}\n\n")
			}))
			t.Cleanup(server.Close)
			credentials, err := json.Marshal(map[string]string{"api_key": "fixture-key", "base_url": server.URL})
			require.NoError(t, err)
			var accountID int64
			require.NoError(t, integrationDB.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,credentials,status,schedulable)
 VALUES('gemini-semantic','gemini','apikey',$1,'active',true) RETURNING id`, string(credentials)).Scan(&accountID))
			t.Cleanup(func() {
				_, err := integrationDB.ExecContext(context.Background(), "DELETE FROM accounts WHERE id=$1", accountID)
				require.NoError(t, err)
			})
			accounts := NewAccountRepository(integrationEntClient, integrationDB, nil)
			plans := &scheduledTestPlanRepository{db: integrationDB}
			cfg := &config.Config{}
			cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			upstream := &geminiAvailabilitySQLUpstream{client: server.Client()}
			limiter := service.ProvideRateLimitService(accounts, nil, cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil, plans)
			gateway := service.NewGeminiMessagesCompatService(accounts, nil, nil, nil, nil, limiter, upstream, nil, cfg)
			tester := service.NewAccountTestService(accounts, nil, nil, nil, nil, upstream, cfg, nil)
			probeCfg := &service.PelicanTestConfig{QuestionKind: "candy", Prompt: "Return 21", ParallelCount: 1, Availability: &service.AvailabilityPolicy{ValidForSeconds: 120}}
			next := time.Now().UTC().Add(-time.Minute)
			plan, err := plans.Create(ctx, &service.ScheduledTestPlan{AccountID: accountID, ModelID: "gemini-3.7-flash", Enabled: true, CronExpression: "0 * * * *", MaxResults: 10, NextRunAt: &next, PelicanConfig: probeCfg})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, plans.Delete(context.Background(), plan.ID)) })
			probeAndPublish := func() []byte {
				t.Helper()
				plan, err = plans.GetByID(ctx, plan.ID)
				require.NoError(t, err)
				now := time.Now().UTC().Truncate(time.Microsecond)
				until := now.Add(time.Minute)
				plan.TriggerSource = "upstream_5xx"
				claimed, err := plans.ClaimPelican(ctx, plan, now, until, *plan.NextRunAt)
				require.NoError(t, err)
				require.True(t, claimed)
				result, err := tester.RunPelicanBackground(ctx, accountID, plan.ModelID, probeCfg)
				require.NoError(t, err)
				probe := result.PelicanConfig.AvailabilityResult
				require.NotNil(t, probe)
				require.Equal(t, "pass", probe.Status, result.ErrorMessage)
				require.NotNil(t, probe.Scope)
				require.Equal(t, "gemini_http", probe.Scope.Protocol)
				require.Eventually(t, func() bool {
					var dbNow time.Time
					return integrationDB.QueryRowContext(ctx, "SELECT clock_timestamp()").Scan(&dbNow) == nil && !dbNow.Before(probe.FinishedAt)
				}, 2*time.Second, 10*time.Millisecond)
				action, err := plans.ApplyAvailabilityOutcomes(ctx, plan, until, []service.AvailabilityModelOutcome{{ModelID: plan.ModelID, Probe: probe}})
				require.NoError(t, err)
				require.Equal(t, "observed", action)
				require.NoError(t, plans.FinishPelican(ctx, plan.ID, until, now))
				return bytes.Clone(upstream.body)
			}
			body := probeAndPublish()
			forward := func() error {
				t.Helper()
				account, err := accounts.GetByID(ctx, accountID)
				require.NoError(t, err)
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-3.7-flash:streamGenerateContent", bytes.NewReader(body)).WithContext(ctx)
				_, err = gateway.ForwardNative(ctx, c, account, plan.ModelID, "streamGenerateContent", true, body)
				return err
			}
			require.NoError(t, forward(), "the real probe must authorize the matching generation route")
			fail.Store(true)
			require.NoError(t, forward(), "native forwarding must preserve the in-band response")
			report, err := plans.ReadAvailability(ctx, plan.ID)
			require.NoError(t, err)
			before := sends.Load()
			if tc.fault {
				require.Equal(t, "unknown", report.Models[0].Status)
				require.True(t, service.IsOpenAITurnAdmissionError(forward()))
				require.Equal(t, before, sends.Load(), "an invalidated pass must stop the next physical send")
			} else {
				require.Equal(t, "pass", report.Models[0].Status)
				require.NoError(t, forward())
				require.Equal(t, before+1, sends.Load())
			}
			fail.Store(false)
			body = probeAndPublish()
			require.NoError(t, forward(), "a fresh successful probe must restore generation")
		})
	}
}

func testRegionalAvailabilitySQLSnapshotFaultAndRecovery(t *testing.T, platform string, scope service.AvailabilityRequestScope) {
	t.Helper()
	ctx := t.Context()
	var accountID int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,credentials,status,schedulable)
 VALUES('regional-availability',$1,'apikey','{"api_key":"fixture-before","base_url":"https://fixture.invalid"}','active',true) RETURNING id`, platform).Scan(&accountID))
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
		var now time.Time
		err := integrationDB.QueryRowContext(ctx, "UPDATE scheduled_test_plans SET next_run_at=clock_timestamp()-interval '1 second' WHERE id=$1 RETURNING clock_timestamp()", plan.ID).Scan(&now)
		require.NoError(t, err)
		plan, err = plans.GetByID(ctx, plan.ID)
		require.NoError(t, err)
		until := now.Add(time.Minute)
		claimed, err := plans.ClaimPelican(ctx, plan, now, until, now.Add(time.Hour))
		require.NoError(t, err)
		require.True(t, claimed)
		finished := now.Add(-time.Millisecond)
		action, err := plans.ApplyAvailabilityOutcomes(ctx, plan, until, []service.AvailabilityModelOutcome{{ModelID: "target",
			Probe: &service.AvailabilityProbeResult{Version: 1, Status: "pass", Reason: "completed_response", AttemptCount: 1,
				StartedAt: finished.Add(-time.Second), FinishedAt: finished,
				Scope: &scope}}})
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
			payload, err := exporter.ExportRegionalScheduler(ctx, []service.SchedulerBucket{{Platform: platform, Mode: service.SchedulerModeSingle}}, []int64{accountID})
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
	require.Equal(t, &scope, reports[0].Models[0].Decision.Scope, "the full protocol scope must survive SQL and regional transport")
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
	require.Equal(t, &scope, reports[0].Models[0].Decision.Scope)
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
