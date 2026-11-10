//go:build integration

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/regional"
	"github.com/Wei-Shaw/sub2api/internal/regional/control"
	"github.com/Wei-Shaw/sub2api/internal/regional/ledger"
	"github.com/Wei-Shaw/sub2api/internal/regional/slots"
	"github.com/Wei-Shaw/sub2api/internal/regional/video"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRegionalVideoFundsRuntimeRevalidatesSalePolicy(t *testing.T) {
	for _, mode := range []string{"primary", "regional"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			user, group, key, account := regionalVideoFinancialSubject(t, "grok")
			price := ledger.Price{Model: "sale-policy-" + uuid.NewString(), Video: &ledger.VideoPrice{VideoRequest: ledger.VideoRequest{Provider: "grok", UpstreamModel: "grok-imagine-video", Resolution: "720p", DurationSeconds: 6}, Basis: "second", UnitNano: 20_000_000, SaleNumerator: 1, SaleDenominator: 1}}
			fundsConfig := RegionalFundsRuntimeConfig{Mode: mode, Region: "us", Prices: []ledger.Price{price}, Targets: []RegionalFundsTarget{{UserID: user, APIKeyID: key.ID, AccountID: account.ID, Amount: 1_000_000_000}}, MaximumOutbox: 100}
			raw, err := json.Marshal(fundsConfig)
			require.NoError(t, err)
			path := filepath.Join(t.TempDir(), "funds.json")
			require.NoError(t, os.WriteFile(path, raw, 0600))
			epoch := "video-sale-" + uuid.NewString()
			capacity := &regionalConcurrencyCache{runtimeConfig: &RegionalConcurrencyRuntimeConfig{Mode: mode, Region: "us", Epoch: epoch,
				Scope: control.Scope{Region: "us", CertificateSHA256: strings.Repeat("a", 64), UserIDs: []int64{user}, APIKeyIDs: []int64{key.ID}, AccountIDs: []int64{account.ID}, MaximumGrant: 1_000_000_000, MaximumSlots: 1, MaximumLifetime: time.Minute}},
				primary: &slots.Primary{DB: integrationDB, GrantGuard: func(context.Context, *sql.Tx) error { return nil }, Limit: func(context.Context, *sql.Tx, slots.Resource) (int, error) { return 1, nil }}, brokerClient: &control.Client{}}
			_, err = integrationDB.ExecContext(ctx, "INSERT INTO regional_funds_imports(epoch) VALUES($1)", epoch)
			require.NoError(t, err)
			t.Cleanup(func() { _, _ = integrationDB.Exec("DELETE FROM regional_funds_imports WHERE epoch=$1", epoch) })
			cfg := &config.Config{Runtime: config.RuntimeConfig{RegionalFundsFile: path}}
			meter, err := ProvideRegionalMetering(integrationDB, cfg, capacity, nil)
			require.NoError(t, err)
			t.Cleanup(meter.Stop)
			requests, ok := meter.Ledger.(ledger.VideoTaskLedger)
			require.True(t, ok, "exercise the production ledger wiring, not a test-only validator")
			grant := regionalLedgerLease(t, "sale-policy-grant-"+uuid.NewString(), user, 1_000_000_000)
			grant.APIKeyID, grant.AccountID = key.ID, account.ID
			if mode == "regional" {
				primary, _, _ := NewRegionalFundsAuthorities(integrationDB, func(context.Context, *sql.Tx) error { return nil })
				require.NoError(t, primary.Issue(ctx, grant))
				local, ok := meter.Ledger.(*ledger.Local)
				require.True(t, ok)
				require.NoError(t, local.Install(ctx, grant))
			}
			reset := func(t *testing.T) {
				t.Helper()
				_, err := integrationDB.ExecContext(ctx, "UPDATE groups SET rate_multiplier=1,video_rate_independent=false,video_rate_multiplier=1,peak_rate_enabled=false,peak_rate_multiplier=1,status='active' WHERE id=$1", group.ID)
				require.NoError(t, err)
				_, err = integrationDB.ExecContext(ctx, "UPDATE accounts SET group_rate_multiplier=1,rate_multiplier=1,schedulable=true WHERE id=$1", account.ID)
				require.NoError(t, err)
				_, err = integrationDB.ExecContext(ctx, "DELETE FROM user_group_rate_multipliers WHERE user_id=$1 AND group_id=$2", user, group.ID)
				require.NoError(t, err)
			}
			reservation := func(t *testing.T) ledger.Reservation {
				t.Helper()
				maximum, err := price.VideoMaximum(price.Video.VideoRequest)
				require.NoError(t, err)
				r := regionalLedgerReservation(t, grant, "sale-policy-"+uuid.NewString(), maximum)
				r.APIKeyID, r.AccountID, r.Model, r.PricingVersion = key.ID, account.ID, price.Model, price.Version()
				r.Video = &price.Video.VideoRequest
				r.VideoOrigin = &ledger.VideoOrigin{Member: "video:" + uuid.NewString(), Owner: video.Owner{UserID: user, APIKeyID: key.ID, GroupID: group.ID, AccountID: account.ID, Provider: "grok", RouteFingerprint: service.AccountRouteFingerprint(account)}}
				if mode == "primary" {
					r.LeaseID, r.RegionID = "", ""
				}
				return r
			}
			for _, scenario := range []struct {
				name, query string
				account     bool
				allowed     bool
			}{
				{name: "unit", query: "UPDATE groups SET rate_multiplier=1 WHERE id=$1", allowed: true},
				{name: "group_rate", query: "UPDATE groups SET rate_multiplier=0.8 WHERE id=$1"},
				{name: "video_rate", query: "UPDATE groups SET video_rate_independent=true,video_rate_multiplier=0.8 WHERE id=$1"},
				{name: "free_video", query: "UPDATE groups SET video_rate_independent=true,video_rate_multiplier=0 WHERE id=$1"},
				{name: "nan_video", query: "UPDATE groups SET video_rate_independent=true,video_rate_multiplier='NaN'::numeric WHERE id=$1"},
				{name: "disabled_video_rate", query: "UPDATE groups SET video_rate_independent=false,video_rate_multiplier=0.8 WHERE id=$1", allowed: true},
				{name: "peak_rate", query: "UPDATE groups SET peak_rate_enabled=true,peak_rate_multiplier=0.8 WHERE id=$1"},
				{name: "user_override", query: "INSERT INTO user_group_rate_multipliers(user_id,group_id,rate_multiplier) VALUES($2,$1,0.8)"},
				{name: "account_rate", query: "UPDATE accounts SET rate_multiplier=0.8 WHERE id=$1", account: true},
				{name: "account_sale_rate", query: "UPDATE accounts SET group_rate_multiplier=0.8 WHERE id=$1", account: true},
				{name: "paused_account", query: "UPDATE accounts SET schedulable=false WHERE id=$1", account: true},
				{name: "paused_group", query: "UPDATE groups SET status='inactive' WHERE id=$1"},
			} {
				change := func(t *testing.T) {
					t.Helper()
					args := []any{group.ID}
					if scenario.account {
						args[0] = account.ID
					}
					if scenario.name == "user_override" {
						args = append(args, user)
					}
					_, err := integrationDB.ExecContext(ctx, scenario.query, args...)
					require.NoError(t, err)
				}
				for _, boundary := range []string{"reserve", "start"} {
					t.Run(scenario.name+"/"+boundary, func(t *testing.T) {
						reset(t)
						t.Cleanup(func() { reset(t) })
						r := reservation(t)
						if boundary == "start" {
							fresh, err := requests.Reserve(ctx, r)
							require.NoError(t, err)
							require.True(t, fresh)
							t.Cleanup(func() { require.NoError(t, requests.RejectVideoTask(ctx, r.RequestID)) })
						}
						change(t)
						if boundary == "reserve" {
							var balanceBefore string
							var heldBefore int64
							if !scenario.allowed {
								require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT balance::text FROM users WHERE id=$1", user).Scan(&balanceBefore))
								if mode == "regional" {
									require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT reserved FROM regional_local_leases WHERE lease_id=$1", grant.LeaseID).Scan(&heldBefore))
								}
							}
							fresh, err := requests.Reserve(ctx, r)
							if scenario.allowed {
								require.NoError(t, err)
								require.True(t, fresh)
								require.NoError(t, requests.CancelBeforeStart(ctx, r.RequestID))
							} else {
								if err == nil {
									require.NoError(t, requests.RejectVideoTask(ctx, r.RequestID))
								}
								require.Error(t, err, "a stale auth snapshot must not authorize a changed SQL sale policy")
								require.False(t, fresh)
								var identities int
								require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT count(*) FROM regional_video_financial_bindings WHERE request_id=$1", r.RequestID).Scan(&identities))
								require.Zero(t, identities, "rejected admission must roll back financial identity and funds together")
								var balanceAfter string
								require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT balance::text FROM users WHERE id=$1", user).Scan(&balanceAfter))
								require.Equal(t, balanceBefore, balanceAfter)
								if mode == "regional" {
									var heldAfter int64
									require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT reserved FROM regional_local_leases WHERE lease_id=$1", grant.LeaseID).Scan(&heldAfter))
									require.Equal(t, heldBefore, heldAfter)
								} else {
									var allocations int
									require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT count(*) FROM regional_budget_allocations WHERE allocation_id=$1", "request:"+r.RequestID).Scan(&allocations))
									require.Zero(t, allocations)
								}
							}
							return
						}
						err := requests.Start(ctx, r.RequestID)
						if scenario.allowed {
							require.NoError(t, err)
							return
						}
						require.ErrorIs(t, err, ledger.ErrStartRejected, "a policy change after Reserve must provably stop Start before provider work")
						table := "regional_primary_requests"
						if mode == "regional" {
							table = "regional_local_requests"
						}
						var state string
						require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT state FROM "+table+" WHERE request_id=$1", r.RequestID).Scan(&state))
						require.Equal(t, "reserved", state)
						reset(t)
						require.NoError(t, requests.Start(ctx, r.RequestID), "restoring the authoritative policy permits the original reservation without a new identity")
					})
				}
			}
		})
	}
}

func TestRegionalFundsBrokerPreservesPendingPayload(t *testing.T) {
	ctx := context.Background()
	user := regionalLedgerUser(t)
	t.Cleanup(func() {
		_, _ = integrationDB.Exec("DELETE FROM regional_broker_operations WHERE operation='quota/issue' AND (payload->>'user_id')::bigint=$1", user)
	})
	broker := &control.Broker{DB: integrationDB, Quota: &ledger.Local{DB: integrationDB, Region: "us"}}
	target := RegionalFundsTarget{UserID: user, APIKeyID: 1, AccountID: 2, Amount: 1000000000, AccountAmount: 2000000000}
	require.NoError(t, ensureRegionalFunds(ctx, integrationDB, broker, "us", target, time.Minute))
	var firstID string
	var firstRaw []byte
	require.NoError(t, integrationDB.QueryRow("SELECT operation_id,payload FROM regional_broker_operations WHERE operation='quota/issue' AND (payload->>'user_id')::bigint=$1", user).Scan(&firstID, &firstRaw))
	// New worker and changed desired amount must replay old pending operation.
	broker = &control.Broker{DB: integrationDB, Quota: &ledger.Local{DB: integrationDB, Region: "us"}}
	target.Amount *= 2
	target.AccountAmount *= 3
	require.NoError(t, ensureRegionalFunds(ctx, integrationDB, broker, "us", target, time.Minute))
	var count int
	var secondID string
	var secondRaw []byte
	require.NoError(t, integrationDB.QueryRow("SELECT COUNT(*) FROM regional_broker_operations WHERE operation='quota/issue' AND (payload->>'user_id')::bigint=$1", user).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, integrationDB.QueryRow("SELECT operation_id,payload FROM regional_broker_operations WHERE operation='quota/issue' AND (payload->>'user_id')::bigint=$1", user).Scan(&secondID, &secondRaw))
	require.Equal(t, firstID, secondID)
	require.JSONEq(t, string(firstRaw), string(secondRaw))
	require.True(t, json.Valid(firstRaw))
	var queued regional.Lease
	require.NoError(t, json.Unmarshal(firstRaw, &queued))
	require.EqualValues(t, 1_000_000_000, queued.Granted)
	require.EqualValues(t, 2_000_000_000, queued.AccountGranted, "restart must not reprice an already queued independent account grant")
}

func TestRegionalFundsImportsLegacyUsageOnce(t *testing.T) {
	ctx := context.Background()
	user := regionalLedgerUser(t)
	epoch := fmt.Sprintf("funds-%d", user)
	_, err := integrationDB.Exec("INSERT INTO user_platform_quotas(user_id,platform,daily_limit_usd,daily_usage_usd,weekly_usage_usd,monthly_usage_usd) VALUES($1,'openai',10,1,2,3)", user)
	require.NoError(t, err)
	key := userPlatformQuotaCacheKey(user, "openai")
	require.NoError(t, integrationRedis.HSet(ctx, key, "daily_usage", "4.00000000", "weekly_usage", "5.00000000", "monthly_usage", "6.00000000").Err())
	t.Cleanup(func() {
		_, _ = integrationDB.Exec("DELETE FROM regional_funds_imports WHERE epoch=$1", epoch)
		_, _ = integrationDB.Exec("DELETE FROM regional_funds_members WHERE epoch=$1", epoch)
		_ = integrationRedis.Del(ctx, key).Err()
	})
	capacity := &regionalConcurrencyCache{runtimeConfig: &RegionalConcurrencyRuntimeConfig{Epoch: epoch, PrimaryInstances: []string{"one"}}, primary: &slots.Primary{DB: integrationDB, GrantGuard: func(context.Context, *sql.Tx) error { return nil }}}
	require.Error(t, importRegionalFundsUsage(ctx, integrationDB, integrationRedis, capacity))
	_, err = integrationDB.Exec("INSERT INTO regional_funds_members(epoch,instance_id) VALUES($1,'one')", epoch)
	require.NoError(t, err)
	require.NoError(t, importRegionalFundsUsage(ctx, integrationDB, integrationRedis, capacity))
	_, err = integrationDB.Exec("UPDATE user_platform_quotas SET daily_usage_usd=7 WHERE user_id=$1", user)
	require.NoError(t, err)
	require.NoError(t, importRegionalFundsUsage(ctx, integrationDB, integrationRedis, capacity))
	var actual string
	require.NoError(t, integrationDB.QueryRow("SELECT daily_usage_usd::text FROM user_platform_quotas WHERE user_id=$1", user).Scan(&actual))
	require.Equal(t, "7.0000000000", actual)
}

type fundsLostClose struct {
	primary *ledger.Primary
	lost    bool
}

func (r *fundsLostClose) IssueSlots(context.Context, slots.Grant) error {
	return fmt.Errorf("unexpected slots")
}
func (r *fundsLostClose) RenewSlots(context.Context, slots.Grant) error {
	return fmt.Errorf("unexpected slots")
}
func (r *fundsLostClose) IssueQuota(ctx context.Context, l regional.Lease) error {
	return r.primary.Issue(ctx, l)
}
func (r *fundsLostClose) CloseQuota(ctx context.Context, receipt ledger.DrainReceipt) error {
	if err := r.primary.Close(ctx, "us", receipt); err != nil {
		return err
	}
	if !r.lost {
		r.lost = true
		return fmt.Errorf("lost close acknowledgment")
	}
	return nil
}
func TestRegionalFundsCloseRestartAndUnknownHold(t *testing.T) {
	ctx := context.Background()
	user := regionalLedgerUser(t)
	p := &ledger.Primary{DB: integrationDB}
	local := &ledger.Local{DB: integrationDB, Region: "us"}
	lease := regionalLedgerLease(t, fmt.Sprintf("funds-close-%d", user), user, 1_000_000_000)
	require.NoError(t, p.Issue(ctx, lease))
	require.NoError(t, local.Install(ctx, lease))
	request := regionalLedgerReservation(t, lease, fmt.Sprintf("funds-unknown-%d", user), lease.Granted)
	_, err := local.Reserve(ctx, request)
	require.NoError(t, err)
	require.NoError(t, local.Start(ctx, request.RequestID))
	remote := &fundsLostClose{primary: p}
	broker := &control.Broker{DB: integrationDB, Remote: remote, Quota: local}
	t.Cleanup(func() {
		_, _ = integrationDB.Exec("DELETE FROM regional_broker_operations WHERE operation_id=$1", "close:"+lease.LeaseID)
	})
	require.NoError(t, local.Drain(ctx, lease.LeaseID))
	require.NoError(t, drainRegionalFunds(ctx, integrationDB, broker, local))
	var count int
	require.NoError(t, integrationDB.QueryRow("SELECT COUNT(*) FROM regional_broker_operations WHERE operation_id=$1", "close:"+lease.LeaseID).Scan(&count))
	require.Zero(t, count)
	e := regionalLedgerEvent(request, 100_000_000)
	require.NoError(t, local.Complete(ctx, e))
	_, err = p.Settle(ctx, "us", e, func(context.Context, *sql.Tx, ledger.Event) error { return nil })
	require.NoError(t, err)
	require.NoError(t, local.Acknowledge(ctx, e))
	// Close committed locally, process dies before enqueue: background reconstructs.
	_, err = local.Close(ctx, lease.LeaseID)
	require.NoError(t, err)
	require.NoError(t, drainRegionalFunds(ctx, integrationDB, broker, local))
	require.Error(t, broker.Replay(ctx, "close:"+lease.LeaseID))
	broker = &control.Broker{DB: integrationDB, Remote: remote, Quota: local}
	require.NoError(t, broker.Replay(ctx, "close:"+lease.LeaseID))
	var balance string
	require.NoError(t, integrationDB.QueryRow("SELECT balance::text FROM users WHERE id=$1", user).Scan(&balance))
	require.Equal(t, "9.90000000", balance)
	var state string
	require.NoError(t, integrationDB.QueryRow("SELECT state FROM regional_broker_operations WHERE operation_id=$1", "close:"+lease.LeaseID).Scan(&state))
	require.Equal(t, "installed", state)
}
