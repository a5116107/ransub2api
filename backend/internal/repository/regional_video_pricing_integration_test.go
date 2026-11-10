//go:build integration

package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/regional"
	"github.com/Wei-Shaw/sub2api/internal/regional/control"
	"github.com/Wei-Shaw/sub2api/internal/regional/ledger"
	"github.com/Wei-Shaw/sub2api/internal/regional/video"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestRegionalVideoPricingReservationsAndSettlement(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		basis    string
		free     bool
		failed   bool
		base     regional.Amount
		charge   regional.Amount
	}{
		{"minimax_request", "minimax-h3", "request", false, false, 20_000_000, 2_000_000},
		{"minimax_second", "minimax-h3", "second", false, false, 100_000_000, 10_000_000},
		{"grok_second", "grok", "second", false, false, 420_000_000, 63_000_000},
		{"seedance_tokens", "seedance", "completion_tokens", false, false, 345_660_000, 62_218_800},
		{"free_sale", "minimax-h3", "request", true, false, 20_000_000, 0},
		{"failed", "minimax-h3", "request", false, true, 0, 0},
	} {
		for _, mode := range []string{"primary", "regional"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				ctx := t.Context()
				user, group, key, account := regionalVideoFinancialSubject(t, tc.provider)
				// Daily/weekly counters are active only when their limits are set.
				_, err := integrationDB.ExecContext(ctx, `UPDATE accounts SET extra=jsonb_build_object('quota_daily_limit',100,'quota_weekly_limit',100) WHERE id=$1`, account.ID)
				require.NoError(t, err)
				price := ledger.Price{Model: "video-funds-" + uuid.NewString(), Video: &ledger.VideoPrice{
					VideoRequest: ledger.VideoRequest{Provider: tc.provider, UpstreamModel: "MiniMax-H3", Resolution: "768p", DurationSeconds: 5},
					Basis:        tc.basis, UnitNano: 20_000_000, SaleNumerator: 1, SaleDenominator: 10,
				}}
				var tokens int64
				switch tc.provider {
				case "grok":
					price.Video.VideoRequest = ledger.VideoRequest{Provider: "grok", UpstreamModel: "grok-imagine-video", Resolution: "720p", DurationSeconds: 6}
					price.Video.UnitNano, price.Video.SaleNumerator, price.Video.SaleDenominator = 70_000_000, 3, 20
				case "seedance":
					price.Video.VideoRequest = ledger.VideoRequest{Provider: "seedance", UpstreamModel: "doubao-seedance-2-0"}
					price.Video.UnitNano, price.Video.SaleNumerator, price.Video.SaleDenominator = 28_000_000_000, 9, 50
					price.Video.MaxCompletionTokens, tokens = 1_000_000, 12_345
				}
				if tc.free {
					price.Video.SaleNumerator, price.Video.SaleDenominator = 0, 1
				}
				require.NoError(t, ledger.InstallPrice(ctx, integrationDB, price))
				require.NoError(t, ledger.InstallPrice(ctx, integrationDB, price))
				_, err = integrationDB.ExecContext(ctx, "UPDATE regional_prices SET price='{}' WHERE version=$1", price.Version())
				require.Error(t, err, "published video prices must remain immutable")
				maximum, err := price.VideoMaximum(price.Video.VideoRequest)
				require.NoError(t, err)
				primary, holds, verify := NewRegionalFundsAuthorities(integrationDB, func(context.Context, *sql.Tx) error { return nil })
				lease := regionalLedgerLease(t, "video-grant-"+uuid.NewString(), user, 8_000_000_000)
				lease.APIKeyID, lease.AccountID = key.ID, account.ID
				lease.VideoPriceVersions = []string{price.Version()}
				lease.GroupID = group.ID
				// Seedance's conservative base ceiling exceeds its sale ceiling.
				lease.AccountGranted = 40_000_000_000
				r := regionalLedgerReservation(t, lease, "video-request-"+uuid.NewString(), maximum)
				r.APIKeyID, r.AccountID, r.Model, r.PricingVersion = key.ID, account.ID, price.Model, price.Version()
				r.Video = &price.Video.VideoRequest
				r.VideoOrigin = &ledger.VideoOrigin{Member: "video:" + uuid.NewString(), Owner: video.Owner{UserID: user, APIKeyID: key.ID, GroupID: group.ID, AccountID: account.ID, Provider: tc.provider, RouteFingerprint: service.AccountRouteFingerprint(account)}}
				var requests ledger.VideoTaskLedger = holds
				var local *ledger.Local
				if mode == "regional" {
					require.NoError(t, primary.Issue(ctx, lease))
					local = &ledger.Local{DB: integrationDB, Region: lease.RegionID}
					require.NoError(t, local.Install(ctx, lease))
					requests = local
				} else {
					r.LeaseID, r.RegionID = "", ""
				}
				for _, mutate := range []func(*ledger.VideoOrigin){
					func(o *ledger.VideoOrigin) { o.Member = "video:invalid" },
					func(o *ledger.VideoOrigin) { o.Owner.UserID++ },
					func(o *ledger.VideoOrigin) { o.Owner.APIKeyID++ },
					func(o *ledger.VideoOrigin) { o.Owner.AccountID++ },
					func(o *ledger.VideoOrigin) { o.Owner.Provider = "other" },
					func(o *ledger.VideoOrigin) { o.Owner.RouteFingerprint = [32]byte{} },
				} {
					bad := r
					origin := *r.VideoOrigin
					mutate(&origin)
					bad.VideoOrigin = &origin
					_, err := requests.Reserve(ctx, bad)
					require.ErrorIs(t, err, ledger.ErrConflict)
				}
				missingOrigin := r
				missingOrigin.VideoOrigin = nil
				_, err = requests.Reserve(ctx, missingOrigin)
				require.ErrorIs(t, err, ledger.ErrConflict, "historical video records cannot infer an origin from current routing")
				bad := r
				bad.RequestID = "video-underhold-" + uuid.NewString()
				if maximum > 0 {
					bad.Maximum -= 10
				} else {
					bad.Maximum = 10
				}
				_, err = requests.Reserve(ctx, bad)
				require.ErrorIs(t, err, ledger.ErrConflict, "the entire immutable ceiling must be reserved")
				bad = r
				bad.RequestID, bad.Maximum, bad.PricingVersion = "video-missing-"+uuid.NewString(), 0, "uninstalled-price"
				_, err = requests.Reserve(ctx, bad)
				require.Error(t, err, "missing prices must not imply a free task")
				cancelBeforeSend := r
				cancelBeforeSend.RequestID = "video-cancel-" + uuid.NewString()
				cancelOrigin := *r.VideoOrigin
				cancelOrigin.Member = "video:" + uuid.NewString()
				cancelBeforeSend.VideoOrigin = &cancelOrigin
				fresh, err := requests.Reserve(ctx, cancelBeforeSend)
				require.NoError(t, err)
				require.True(t, fresh)
				require.NoError(t, requests.CancelBeforeStart(ctx, cancelBeforeSend.RequestID))
				require.NoError(t, requests.CancelBeforeStart(ctx, cancelBeforeSend.RequestID))
				require.ErrorIs(t, requests.Start(ctx, cancelBeforeSend.RequestID), ledger.ErrAlreadyDispatched)
				require.ErrorIs(t, requests.BindVideoTask(ctx, cancelBeforeSend.RequestID, "unsent-task"), ledger.ErrAlreadyDispatched)
				cancelBeforeSend.RequestID = "video-reused-member-" + uuid.NewString()
				_, err = requests.Reserve(ctx, cancelBeforeSend)
				require.ErrorIs(t, err, ledger.ErrConflict, "cancellation must not recycle a submission identity")
				fresh, err = requests.Reserve(ctx, r)
				require.NoError(t, err)
				require.True(t, fresh)
				// Restore a fresh ledger instance before starting; no Redis snapshot
				// or caller-supplied price is needed to recover the financial hold.
				if local != nil {
					local = &ledger.Local{DB: integrationDB, Region: lease.RegionID}
					requests = local
				} else {
					_, holds, _ = NewRegionalFundsAuthorities(integrationDB, func(context.Context, *sql.Tx) error { return nil })
					requests = holds
				}
				fresh, err = requests.Reserve(ctx, r)
				require.NoError(t, err)
				require.False(t, fresh)
				require.NoError(t, requests.Start(ctx, r.RequestID))
				require.ErrorIs(t, requests.Start(ctx, r.RequestID), ledger.ErrAlreadyDispatched)
				require.ErrorIs(t, requests.CancelBeforeStart(ctx, r.RequestID), ledger.ErrAlreadyDispatched)
				e := regionalLedgerEvent(r, tc.charge)
				e.InputTokens, e.OutputTokens = 0, tokens
				e.Video = &ledger.VideoUsage{VideoRequest: price.Video.VideoRequest, TaskID: "provider-task-" + uuid.NewString(), State: "completed", CompletionTokens: tokens}
				e.VideoOrigin = r.VideoOrigin
				principal := r.VideoOrigin.Owner
				principal.AccountID, principal.RouteFingerprint = 0, [32]byte{}
				if tc.failed {
					e.Video.State = "failed"
				}
				require.ErrorIs(t, requests.Complete(ctx, e), ledger.ErrConflict, "terminal evidence must not assign an unbound task")
				_, err = requests.ResolveVideoTaskOwner(ctx, principal, e.Video.TaskID)
				require.ErrorIs(t, err, sql.ErrNoRows, "unknown task identity must not be guessed from its reservation")
				for _, invalidTask := range []string{"", " task ", "task\r\n", "task\x00id"} {
					require.ErrorIs(t, requests.BindVideoTask(ctx, r.RequestID, invalidTask), ledger.ErrConflict)
				}
				canceledBind, cancelBind := context.WithCancel(ctx)
				cancelBind()
				require.Error(t, requests.BindVideoTask(canceledBind, r.RequestID, e.Video.TaskID))
				require.NoError(t, requests.BindVideoTask(ctx, r.RequestID, e.Video.TaskID))
				resolvedOwner, err := requests.ResolveVideoTaskOwner(ctx, principal, e.Video.TaskID)
				require.NoError(t, err)
				require.Equal(t, r.VideoOrigin.Owner, *resolvedOwner)
				for _, mutate := range []func(*video.Owner){
					func(o *video.Owner) { o.UserID++ },
					func(o *video.Owner) { o.APIKeyID++ },
					func(o *video.Owner) { o.GroupID++ },
					func(o *video.Owner) {
						o.Provider = "seedance"
						if o.Provider == principal.Provider {
							o.Provider = "grok"
						}
					},
				} {
					wrongPrincipal := principal
					mutate(&wrongPrincipal)
					_, err := requests.ResolveVideoTaskOwner(ctx, wrongPrincipal, e.Video.TaskID)
					require.ErrorIs(t, err, sql.ErrNoRows, "task identity must remain scoped to its authenticated tenant and provider")
				}
				if mode == "regional" {
					_, err := (&ledger.Local{DB: integrationDB, Region: "another-region"}).ResolveVideoTaskOwner(ctx, principal, e.Video.TaskID)
					require.ErrorIs(t, err, ledger.ErrConflict, "sharing a database must not transfer task ownership to another region")
				}
				_, err = requests.ResolveVideoTaskOwner(ctx, r.VideoOrigin.Owner, e.Video.TaskID)
				require.ErrorIs(t, err, ledger.ErrConflict, "the resolver accepts an authenticated principal, never a caller-selected route")
				require.NoError(t, requests.BindVideoTask(ctx, r.RequestID, e.Video.TaskID))
				require.ErrorIs(t, requests.BindVideoTask(ctx, r.RequestID, "other-task"), ledger.ErrConflict)
				recovered, err := requests.LoadVideoTask(ctx, r.VideoOrigin.Owner, e.Video.TaskID)
				require.NoError(t, err)
				require.Equal(t, r, recovered.Reservation)
				require.Equal(t, "started", recovered.State)
				require.Nil(t, recovered.Event)
				wrongOwner := r.VideoOrigin.Owner
				wrongOwner.RouteFingerprint[0] ^= 1
				_, err = requests.LoadVideoTask(ctx, wrongOwner, e.Video.TaskID)
				require.ErrorIs(t, err, ledger.ErrConflict)
				wrongOwner = r.VideoOrigin.Owner
				wrongOwner.UserID++
				_, err = requests.LoadVideoTask(ctx, wrongOwner, e.Video.TaskID)
				require.ErrorIs(t, err, sql.ErrNoRows)
				if local != nil {
					_, err = (&ledger.Local{DB: integrationDB, Region: "eu"}).LoadVideoTask(ctx, r.VideoOrigin.Owner, e.Video.TaskID)
					require.ErrorIs(t, err, ledger.ErrConflict)
				}
				for _, mutate := range []func(*ledger.Event){
					func(e *ledger.Event) { e.Usage.Charge += 10 },
					func(e *ledger.Event) { e.Video.DurationSeconds++ },
					func(e *ledger.Event) { e.Video.State = "queued" },
					func(e *ledger.Event) { e.InputTokens = 1 },
					func(e *ledger.Event) { e.Video = nil },
					func(e *ledger.Event) { e.Video.TaskID = "unbound-task" },
					func(e *ledger.Event) { e.VideoOrigin = nil },
				} {
					badEvent := e
					copyUsage := *e.Video
					badEvent.Video = &copyUsage
					mutate(&badEvent)
					require.Error(t, requests.Complete(ctx, badEvent))
				}
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				require.Error(t, requests.Complete(cancelled, e))
				if local != nil {
					require.NoError(t, local.Drain(ctx, lease.LeaseID))
					_, err = local.Close(ctx, lease.LeaseID)
					require.ErrorContains(t, err, "unresolved", "an interrupted or free in-flight task still blocks closure")
				}
				// Simulate failure after canonical usage and all budget updates.
				// The primary transaction must roll everything back, then retry.
				if local == nil {
					settle := holds.SettleScope
					holds.SettleScope = func(ctx context.Context, tx *sql.Tx, event ledger.Event) error {
						if err := settle(ctx, tx, event); err != nil {
							return err
						}
						return errors.New("fixture settlement acknowledgement aborted")
					}
					require.ErrorContains(t, requests.Complete(ctx, e), "aborted")
					holds.SettleScope = settle
				}
				var count int
				require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT count(*) FROM usage_logs WHERE request_id=$1", r.RequestID).Scan(&count))
				require.Zero(t, count)
				var wg sync.WaitGroup
				errorsCh := make(chan error, 4)
				for range 4 {
					wg.Go(func() { errorsCh <- requests.Complete(ctx, e) })
				}
				wg.Wait()
				close(errorsCh)
				for replayErr := range errorsCh {
					require.NoError(t, replayErr)
				}
				recovered, err = requests.LoadVideoTask(ctx, r.VideoOrigin.Owner, e.Video.TaskID)
				require.NoError(t, err)
				require.Equal(t, &e, recovered.Event, "completed recovery retains the exact immutable event")
				resolvedOwner, err = requests.ResolveVideoTaskOwner(ctx, principal, e.Video.TaskID)
				require.NoError(t, err)
				require.Equal(t, r.VideoOrigin.Owner, *resolvedOwner)
				require.NoError(t, requests.Complete(ctx, *recovered.Event))
				if local != nil {
					local = &ledger.Local{DB: integrationDB, Region: lease.RegionID}
					pending, err := local.Pending(ctx, 100)
					require.NoError(t, err)
					require.Contains(t, pending, e, "video identity survives restart in the durable outbox")
					require.NoError(t, local.Drain(ctx, lease.LeaseID))
					_, err = local.Close(ctx, lease.LeaseID)
					require.ErrorContains(t, err, "unresolved", "even a zero-price task needs settlement acknowledgement")
					badEvent := e
					badEvent.Usage.Charge += 10
					_, err = primary.Settle(ctx, lease.RegionID, badEvent, verify)
					require.Error(t, err, "primary independently recomputes video charges")
					_, err = primary.Settle(ctx, lease.RegionID, e, func(ctx context.Context, tx *sql.Tx, event ledger.Event) error {
						if err := verify(ctx, tx, event); err != nil {
							return err
						}
						return errors.New("fixture primary reconciliation aborted")
					})
					require.ErrorContains(t, err, "aborted")
					require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT count(*) FROM usage_logs WHERE request_id=$1", r.RequestID).Scan(&count))
					require.Zero(t, count, "failed primary reconciliation must roll back usage and money together")
					type settlement struct {
						applied bool
						err     error
					}
					settlements := make(chan settlement, 4)
					for range 4 {
						wg.Go(func() {
							applied, err := primary.Settle(ctx, lease.RegionID, e, verify)
							settlements <- settlement{applied, err}
						})
					}
					wg.Wait()
					close(settlements)
					appliedCount := 0
					for result := range settlements {
						require.NoError(t, result.err)
						if result.applied {
							appliedCount++
						}
					}
					require.Equal(t, 1, appliedCount)
					applied, err := (&ledger.Primary{DB: integrationDB}).Settle(ctx, lease.RegionID, e, verify)
					require.NoError(t, err)
					require.False(t, applied, "lost acknowledgement must not double-charge")
					require.NoError(t, local.Acknowledge(ctx, e))
					receipt, err := local.Close(ctx, lease.LeaseID)
					require.NoError(t, err)
					require.NoError(t, primary.Close(ctx, lease.RegionID, receipt))
					resolvedOwner, err = local.ResolveVideoTaskOwner(ctx, principal, e.Video.TaskID)
					require.NoError(t, err, "settlement acknowledgement and closed funds must preserve permanent query ownership")
					require.Equal(t, r.VideoOrigin.Owner, *resolvedOwner)
				}
				actualUSD, err := ledger.USD(tc.charge)
				require.NoError(t, err)
				baseUSD, err := ledger.USD(tc.base)
				require.NoError(t, err)
				var correct bool
				require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT count(*)=1 AND bool_and(total_cost=$2::numeric AND actual_cost=$3::numeric AND group_id=$4 AND output_tokens=$5) FROM usage_logs WHERE request_id=$1`, r.RequestID, baseUSD, actualUSD, group.ID, tokens).Scan(&correct))
				require.True(t, correct, "canonical usage must retain base tariff, actual sale and original attribution")
				wantMode, wantCount := "video", 1
				if tc.provider == "seedance" {
					wantMode, wantCount = "token", 0
				} else if tc.failed {
					wantCount = 0
				}
				wantResolution := sql.NullString{String: price.Video.Resolution, Valid: price.Video.Resolution != ""}
				wantDuration := sql.NullInt64{Int64: price.Video.DurationSeconds, Valid: price.Video.DurationSeconds > 0}
				require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT billing_mode=$2 AND video_count=$3 AND video_resolution IS NOT DISTINCT FROM $4::text AND video_duration_seconds IS NOT DISTINCT FROM $5::integer AND rate_multiplier=($6::numeric/$7::numeric) FROM usage_logs WHERE request_id=$1`, r.RequestID, wantMode, wantCount, wantResolution, wantDuration, price.Video.SaleNumerator, price.Video.SaleDenominator).Scan(&correct))
				require.True(t, correct, "text metadata updates must not overwrite video or token billing mode")
				require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT u.balance=10-$2::numeric AND k.quota_used=$2::numeric AND k.usage_5h=$2::numeric FROM users u JOIN api_keys k ON k.user_id=u.id WHERE u.id=$1 AND k.id=$3`, user, actualUSD, key.ID).Scan(&correct))
				require.True(t, correct, "one charge must reconcile balance and key budgets atomically")
				require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT (extra->>'quota_used')::numeric=$2::numeric AND (extra->>'quota_daily_used')::numeric=$2::numeric AND (extra->>'quota_weekly_used')::numeric=$2::numeric FROM accounts WHERE id=$1`, account.ID, baseUSD).Scan(&correct))
				require.True(t, correct, "account budgets consume base cost even when terminal token usage is below its ceiling")
				if mode == "primary" {
					duplicate := r
					duplicate.RequestID = "video-duplicate-" + uuid.NewString()
					duplicateOrigin := *r.VideoOrigin
					duplicateOrigin.Member = "video:" + uuid.NewString()
					duplicate.VideoOrigin = &duplicateOrigin
					fresh, err := requests.Reserve(ctx, duplicate)
					require.NoError(t, err)
					require.True(t, fresh)
					require.NoError(t, requests.Start(ctx, duplicate.RequestID))
					require.ErrorIs(t, requests.BindVideoTask(ctx, duplicate.RequestID, e.Video.TaskID), ledger.ErrConflict, "completed tasks retain a permanent financial identity")
					duplicateEvent := e
					duplicateEvent.Usage.EventID = duplicate.RequestID
					duplicateEvent.VideoOrigin = duplicate.VideoOrigin
					require.ErrorIs(t, requests.Complete(ctx, duplicateEvent), ledger.ErrConflict, "one provider task must not settle under a second financial request")
				}
			})
		}
	}
}

func TestRegionalVideoAccountBudgetUsesBaseCost(t *testing.T) {
	for _, mode := range []string{"primary", "regional"} {
		for _, sale := range []int64{1, 0, 3} {
			t.Run(fmt.Sprintf("%s/sale_%d", mode, sale), func(t *testing.T) {
				ctx := t.Context()
				user, group, key, account := regionalVideoFinancialSubject(t, "minimax-h3")
				_, err := integrationDB.ExecContext(ctx, `UPDATE accounts SET extra=jsonb_build_object('quota_limit',0.03,'quota_daily_limit',0.03,'quota_weekly_limit',0.03) WHERE id=$1`, account.ID)
				require.NoError(t, err)
				denominator := int64(10)
				if sale != 1 {
					denominator = 1
				}
				price := ledger.Price{Model: "account-cost-" + uuid.NewString(), Video: &ledger.VideoPrice{
					VideoRequest: ledger.VideoRequest{Provider: "minimax-h3", UpstreamModel: "MiniMax-H3", Resolution: "768p", DurationSeconds: 5},
					Basis:        "request", UnitNano: 20_000_000, SaleNumerator: sale, SaleDenominator: denominator}}
				require.NoError(t, ledger.InstallPrice(ctx, integrationDB, price))
				maximum, err := price.VideoMaximum(price.Video.VideoRequest)
				require.NoError(t, err)
				primary, holds, verify := NewRegionalFundsAuthorities(integrationDB, func(context.Context, *sql.Tx) error { return nil })
				grant := regionalLedgerLease(t, "account-cost-"+uuid.NewString(), user, 30_000_000)
				grant.APIKeyID, grant.AccountID = key.ID, account.ID
				grant.VideoPriceVersions = []string{price.Version()}
				grant.GroupID = group.ID
				if sale == 3 {
					grant.Granted, grant.AccountGranted = 120_000_000, 20_000_000
				}
				requests := ledger.VideoTaskLedger(holds)
				var local *ledger.Local
				if mode == "regional" {
					require.NoError(t, primary.Issue(ctx, grant))
					local = &ledger.Local{DB: integrationDB, Region: grant.RegionID}
					require.NoError(t, local.Install(ctx, grant))
					requests = local
				}
				reservation := func() ledger.Reservation {
					r := regionalLedgerReservation(t, grant, "account-request-"+uuid.NewString(), maximum)
					r.APIKeyID, r.AccountID, r.Model, r.PricingVersion = key.ID, account.ID, price.Model, price.Version()
					r.Video = &price.Video.VideoRequest
					r.VideoOrigin = &ledger.VideoOrigin{Member: "video:" + uuid.NewString(), Owner: video.Owner{UserID: user, APIKeyID: key.ID, GroupID: group.ID, AccountID: account.ID, Provider: "minimax-h3", RouteFingerprint: service.AccountRouteFingerprint(account)}}
					if mode == "primary" {
						r.LeaseID, r.RegionID = "", ""
					}
					return r
				}
				r := reservation()
				refund := reservation()
				fresh, err := requests.Reserve(ctx, refund)
				require.NoError(t, err)
				require.True(t, fresh)
				require.NoError(t, requests.CancelBeforeStart(ctx, refund.RequestID))
				require.NoError(t, requests.CancelBeforeStart(ctx, refund.RequestID), "refund is idempotent for both ceilings")
				rejected := reservation()
				fresh, err = requests.Reserve(ctx, rejected)
				require.NoError(t, err)
				require.True(t, fresh)
				require.NoError(t, requests.Start(ctx, rejected.RequestID))
				require.NoError(t, requests.RejectVideoTask(ctx, rejected.RequestID))
				require.NoError(t, requests.RejectVideoTask(ctx, rejected.RequestID), "definite pre-bind rejection returns both ceilings")
				type attempt struct {
					reservation ledger.Reservation
					fresh       bool
					err         error
				}
				results := make(chan attempt, 8)
				var contenders sync.WaitGroup
				for range cap(results) {
					candidate := reservation()
					contenders.Go(func() {
						fresh, err := requests.Reserve(ctx, candidate)
						results <- attempt{candidate, fresh, err}
					})
				}
				contenders.Wait()
				close(results)
				winners := 0
				for result := range results {
					if result.err == nil {
						require.True(t, result.fresh)
						r = result.reservation
						winners++
					} else {
						require.ErrorIs(t, result.err, ledger.ErrExhausted)
						require.False(t, result.fresh)
					}
				}
				require.Equal(t, 1, winners, "concurrent discounted, marked-up or free requests share one account ceiling")
				// Unknown started work must keep the entire account ceiling occupied.
				require.NoError(t, requests.Start(ctx, r.RequestID))
				require.ErrorIs(t, requests.CancelBeforeStart(ctx, r.RequestID), ledger.ErrAlreadyDispatched)
				second := reservation()
				fresh, err = requests.Reserve(ctx, second)
				if fresh && err == nil {
					require.NoError(t, requests.CancelBeforeStart(ctx, second.RequestID))
				}
				require.ErrorIs(t, err, ledger.ErrExhausted, "discounted or free sale must still reserve the full account base cost")
				require.False(t, fresh)
				task := service.MiniMaxH3TaskKey("account-task-" + uuid.NewString())
				require.NoError(t, requests.BindVideoTask(ctx, r.RequestID, task))
				event := regionalLedgerEvent(r, maximum)
				event.InputTokens, event.OutputTokens = 0, 0
				event.Video = &ledger.VideoUsage{VideoRequest: *r.Video, TaskID: task, State: "completed"}
				event.VideoOrigin = r.VideoOrigin
				require.NoError(t, requests.Complete(ctx, event))
				if local != nil {
					if sale == 3 {
						broker := &control.Broker{DB: integrationDB, Quota: local}
						require.NoError(t, drainRegionalFunds(ctx, integrationDB, broker, local))
						var state string
						require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT state FROM regional_local_leases WHERE lease_id=$1`, grant.LeaseID).Scan(&state))
						require.Equal(t, "draining", state, "exhausted account grant drains even with sale funds remaining; unacknowledged usage still blocks closure")
					}
					_, err = primary.Settle(ctx, grant.RegionID, event, verify)
					require.NoError(t, err)
					require.NoError(t, local.Acknowledge(ctx, event))
				}
				require.NoError(t, requests.Complete(ctx, event), "replaying completion must not double-charge either budget")
				var correctAccount, correctKey bool
				require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT (extra->>'quota_used')::numeric=0.02 AND (extra->>'quota_daily_used')::numeric=0.02 AND (extra->>'quota_weekly_used')::numeric=0.02 FROM accounts WHERE id=$1`, account.ID).Scan(&correctAccount))
				amount, err := ledger.USD(maximum)
				require.NoError(t, err)
				require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT quota_used=$2::numeric FROM api_keys WHERE id=$1`, key.ID, amount).Scan(&correctKey))
				require.True(t, correctAccount, "account counters consume base cost, independently of the sale price")
				require.True(t, correctKey, "downstream counters consume only the agreed sale price")
				var correctBalance bool
				if local != nil {
					require.NoError(t, local.Drain(ctx, grant.LeaseID))
					receipt, err := local.Close(ctx, grant.LeaseID)
					require.NoError(t, err)
					require.NoError(t, primary.Close(ctx, grant.RegionID, receipt))
					require.NoError(t, primary.Close(ctx, grant.RegionID, receipt))
					require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT account_reserved=0 AND account_spent=20000000 AND reserved=0 AND spent=$2 FROM regional_local_leases WHERE lease_id=$1`, grant.LeaseID, int64(maximum)).Scan(&correctBalance))
					require.True(t, correctBalance)
				}
				require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT balance=10-$2::numeric FROM users WHERE id=$1`, user, amount).Scan(&correctBalance))
				require.True(t, correctBalance, "refunds never exchange account quota into downstream balance")
				fresh, err = requests.Reserve(ctx, reservation())
				require.ErrorIs(t, err, ledger.ErrExhausted, "settled base cost remains consumed after restart or closure")
				require.False(t, fresh)
			})
		}
	}
}

func TestRegionalVideoAccountBudgetGrantIsolation(t *testing.T) {
	ctx := t.Context()
	user, group, key, account := regionalVideoFinancialSubject(t, "minimax-h3")
	_, err := integrationDB.ExecContext(ctx, `UPDATE accounts SET extra=jsonb_build_object('quota_limit',0.03) WHERE id=$1`, account.ID)
	require.NoError(t, err)
	price := ledger.Price{Model: "account-grant-" + uuid.NewString(), Video: &ledger.VideoPrice{
		VideoRequest: ledger.VideoRequest{Provider: "minimax-h3", UpstreamModel: "MiniMax-H3", Resolution: "768p", DurationSeconds: 5},
		Basis:        "request", UnitNano: 20_000_000, SaleNumerator: 1, SaleDenominator: 10}}
	require.NoError(t, ledger.InstallPrice(ctx, integrationDB, price))
	primary, holds, _ := NewRegionalFundsAuthorities(integrationDB, func(context.Context, *sql.Tx) error { return nil })
	grant := regionalLedgerLease(t, "account-grant-"+uuid.NewString(), user, 4_000_000)
	grant.APIKeyID, grant.AccountID, grant.AccountGranted = key.ID, account.ID, 20_000_000
	require.NoError(t, primary.Issue(ctx, grant))
	require.NoError(t, primary.Issue(ctx, grant), "replaying the grant must not reserve either budget twice")
	changed := grant
	changed.AccountGranted += 10
	require.ErrorIs(t, primary.Issue(ctx, changed), ledger.ErrConflict, "account budget belongs to immutable grant identity")
	second := grant
	second.LeaseID = "account-other-" + uuid.NewString()
	second.RegionID = "eu"
	require.ErrorIs(t, primary.Issue(ctx, second), ledger.ErrExhausted, "regions compete on account cost, not sale funds")
	r := regionalLedgerReservation(t, grant, "account-main-"+uuid.NewString(), 2_000_000)
	r.LeaseID, r.RegionID = "", ""
	r.APIKeyID, r.AccountID, r.Model, r.PricingVersion = key.ID, account.ID, price.Model, price.Version()
	r.Video = &price.Video.VideoRequest
	r.VideoOrigin = &ledger.VideoOrigin{Member: "video:" + uuid.NewString(), Owner: video.Owner{UserID: user, APIKeyID: key.ID, GroupID: group.ID, AccountID: account.ID, Provider: "minimax-h3", RouteFingerprint: service.AccountRouteFingerprint(account)}}
	_, err = holds.Reserve(ctx, r)
	require.ErrorIs(t, err, ledger.ErrExhausted, "primary requests cannot reuse account budget granted to a region")
	require.NoError(t, primary.Close(ctx, grant.RegionID, ledger.DrainReceipt{LeaseID: grant.LeaseID, Region: grant.RegionID}))
	fresh, err := holds.Reserve(ctx, r)
	require.NoError(t, err, "authenticated unused-grant closure returns both independent budgets")
	require.True(t, fresh)
	require.NoError(t, holds.Start(ctx, r.RequestID))
	require.NoError(t, holds.RejectVideoTask(ctx, r.RequestID))
	require.NoError(t, holds.RejectVideoTask(ctx, r.RequestID))
	require.NoError(t, primary.Issue(ctx, second), "definite provider rejection refunds the account ceiling as well")
}

func TestRegionalAccountCostMigrationRequiresDrain(t *testing.T) {
	ctx := t.Context()
	raw, err := migrations.FS.ReadFile("335_regional_account_cost_budgets.sql")
	require.NoError(t, err)
	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	// A transaction-owned schema exercises the actual migration on old columns;
	// no production or suite-wide ledger rows are changed.
	name := "account_migration_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = tx.ExecContext(ctx, `CREATE SCHEMA `+pq.QuoteIdentifier(name))
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `SET LOCAL search_path TO `+pq.QuoteIdentifier(name))
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `
CREATE TABLE regional_quota_leases (lease_id TEXT, granted BIGINT, state TEXT);
CREATE TABLE regional_local_leases (lease_id TEXT, granted BIGINT, reserved BIGINT, spent BIGINT, state TEXT);
CREATE TABLE regional_local_requests (request_id TEXT, maximum BIGINT, state TEXT);
CREATE TABLE regional_primary_requests (request_id TEXT, state TEXT);
CREATE TABLE regional_budget_allocations (allocation_id TEXT, maximum BIGINT, spent BIGINT, state TEXT);
CREATE TABLE regional_usage_outbox (event_id TEXT, acknowledged BOOLEAN);`)
	require.NoError(t, err)
	for _, pending := range []string{
		`INSERT INTO regional_quota_leases VALUES ('old',100,'open')`,
		`INSERT INTO regional_local_leases VALUES ('old',100,0,0,'draining')`,
		`INSERT INTO regional_primary_requests VALUES ('old','started')`,
		`INSERT INTO regional_local_requests VALUES ('old',0,'reserved')`,
		`INSERT INTO regional_budget_allocations VALUES ('old',0,0,'held')`,
		`INSERT INTO regional_usage_outbox VALUES ('old',FALSE)`,
	} {
		_, err = tx.ExecContext(ctx, `SAVEPOINT before_upgrade`)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, pending)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, string(raw))
		require.ErrorContains(t, err, "drain regional funds", pending)
		_, err = tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT before_upgrade`)
		require.NoError(t, err)
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO regional_quota_leases VALUES ('old',100,'closed');
INSERT INTO regional_local_leases VALUES ('old',100,0,40,'closed');
INSERT INTO regional_local_requests VALUES ('old',100,'completed');
INSERT INTO regional_primary_requests VALUES ('old','completed');
INSERT INTO regional_budget_allocations VALUES ('old',100,40,'closed');
INSERT INTO regional_usage_outbox VALUES ('old',TRUE);`)
	require.NoError(t, err)
	for range 2 {
		_, err = tx.ExecContext(ctx, string(raw))
		require.NoError(t, err, "a drained upgrade is repeatable without changing historical funds")
	}
	var copied bool
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT account_granted=100 AND account_reserved=0 AND account_spent=40 FROM regional_local_leases WHERE lease_id='old'`).Scan(&copied))
	require.True(t, copied)
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT account_maximum=100 AND account_spent=40 FROM regional_budget_allocations WHERE allocation_id='old'`).Scan(&copied))
	require.True(t, copied)
	for _, invalid := range []string{
		`INSERT INTO regional_local_leases (lease_id,granted,reserved,spent,state) VALUES ('legacy-writer',100,0,0,'open')`,
		`INSERT INTO regional_local_requests (request_id,maximum,state) VALUES ('legacy-writer',100,'reserved')`,
		`INSERT INTO regional_budget_allocations (allocation_id,maximum,spent,state) VALUES ('legacy-writer',100,0,'held')`,
		`UPDATE regional_local_leases SET account_reserved=70 WHERE lease_id='old'`,
		`UPDATE regional_local_leases SET account_spent=-10 WHERE lease_id='old'`,
		`UPDATE regional_budget_allocations SET account_spent=110 WHERE allocation_id='old'`,
		`UPDATE regional_local_requests SET account_maximum=1 WHERE request_id='old'`,
	} {
		_, err = tx.ExecContext(ctx, `SAVEPOINT invalid_budget`)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, invalid)
		require.Error(t, err, invalid)
		_, err = tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT invalid_budget`)
		require.NoError(t, err)
	}
}

func TestRegionalVideoPriceMigrationRequiresDrain(t *testing.T) {
	ctx := t.Context()
	raw, err := migrations.FS.ReadFile("336_regional_video_price_authorizations.sql")
	require.NoError(t, err)
	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	name := "video_price_migration_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = tx.ExecContext(ctx, `CREATE SCHEMA `+pq.QuoteIdentifier(name))
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `SET LOCAL search_path TO `+pq.QuoteIdentifier(name))
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `
CREATE TABLE regional_quota_leases (lease_id TEXT, state TEXT);
CREATE TABLE regional_local_leases (lease_id TEXT, state TEXT);
CREATE TABLE regional_local_requests (request_id TEXT, state TEXT);
CREATE TABLE regional_primary_requests (request_id TEXT, state TEXT);
CREATE TABLE regional_budget_allocations (allocation_id TEXT, state TEXT);
CREATE TABLE regional_usage_outbox (event_id TEXT, acknowledged BOOLEAN);`)
	require.NoError(t, err)
	for _, pending := range []string{
		`INSERT INTO regional_quota_leases VALUES ('old','open')`,
		`INSERT INTO regional_local_leases VALUES ('old','draining')`,
		`INSERT INTO regional_primary_requests VALUES ('old','started')`,
		`INSERT INTO regional_local_requests VALUES ('old','reserved')`,
		`INSERT INTO regional_budget_allocations VALUES ('old','held')`,
		`INSERT INTO regional_usage_outbox VALUES ('old',FALSE)`,
	} {
		_, err = tx.ExecContext(ctx, `SAVEPOINT before_upgrade`)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, pending)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, string(raw))
		require.ErrorContains(t, err, "drain regional funds", pending)
		_, err = tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT before_upgrade`)
		require.NoError(t, err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO regional_quota_leases VALUES ('old','closed')`)
	require.NoError(t, err)
	for range 2 {
		_, err = tx.ExecContext(ctx, string(raw))
		require.NoError(t, err)
	}
	var retained bool
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT group_id=0 AND video_price_versions='[]'::jsonb FROM regional_quota_leases WHERE lease_id='old'`).Scan(&retained))
	require.True(t, retained, "historical closed rows do not invent authorized video prices")
	for _, invalid := range []string{
		`INSERT INTO regional_quota_leases(lease_id,state) VALUES ('old-writer','open')`,
		`UPDATE regional_quota_leases SET video_price_versions='{}' WHERE lease_id='old'`,
		`UPDATE regional_quota_leases SET video_price_versions='["unscoped"]' WHERE lease_id='old'`,
		`UPDATE regional_quota_leases SET group_id=-1 WHERE lease_id='old'`,
	} {
		_, err = tx.ExecContext(ctx, `SAVEPOINT invalid_price_grant`)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, invalid)
		require.Error(t, err, invalid)
		_, err = tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT invalid_price_grant`)
		require.NoError(t, err)
	}
}

func TestRegionalVideoPriceAuthorizationAcrossDatabases(t *testing.T) {
	ctx := t.Context()
	user, group, key, account := regionalVideoFinancialSubject(t, "grok")
	_, err := integrationDB.ExecContext(ctx, `UPDATE groups SET rate_multiplier=0.12 WHERE id=$1`, group.ID)
	require.NoError(t, err)
	name := "video_price_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = integrationDB.ExecContext(ctx, `CREATE DATABASE `+pq.QuoteIdentifier(name))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(context.Background(), `DROP DATABASE `+pq.QuoteIdentifier(name))
		require.NoError(t, err)
	})
	address, err := url.Parse(integrationPostgresDSN)
	require.NoError(t, err)
	address.Path = "/" + name
	localDB, err := sql.Open("postgres", address.String())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, localDB.Close()) })
	require.NoError(t, ApplyMigrations(ctx, localDB))
	// A separate SQL database models the existing replicated identity/config
	// authority. No regional request reads the main database to choose its price.
	for _, row := range []struct {
		table string
		id    int64
	}{{"users", user}, {"groups", group.ID}, {"accounts", account.ID}, {"api_keys", key.ID}} {
		var raw []byte
		require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT row_to_json(t) FROM "+row.table+" t WHERE id=$1", row.id).Scan(&raw))
		_, err := localDB.ExecContext(ctx, "INSERT INTO "+row.table+" SELECT * FROM json_populate_record(NULL::"+row.table+",$1::json)", string(raw))
		require.NoError(t, err)
	}
	_, err = localDB.ExecContext(ctx, `INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, account.ID, group.ID)
	require.NoError(t, err)
	template := ledger.Price{Model: "two-db-" + uuid.NewString(), Video: &ledger.VideoPrice{VideoRequest: ledger.VideoRequest{Provider: "grok", UpstreamModel: "grok-imagine-video", Resolution: "720p", DurationSeconds: 6}, Basis: "second", UnitNano: 20_000_000, SaleNumerator: 1, SaleDenominator: 10}}
	require.NoError(t, ledger.InstallPrice(ctx, integrationDB, template))
	require.NoError(t, ledger.InstallPrice(ctx, localDB, template))
	price, err := ledger.DeriveVideoPrice(template, big.NewRat(12, 100))
	require.NoError(t, err)
	require.NoError(t, ledger.InstallPrice(ctx, localDB, price))
	var exists bool
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM regional_prices WHERE version=$1)`, price.Version()).Scan(&exists))
	require.False(t, exists, "a local installation alone must not create primary authority")
	primary, holds, verify := NewRegionalFundsAuthorities(integrationDB, func(context.Context, *sql.Tx) error { return nil }, template)
	grant := regionalLedgerLease(t, "two-db-"+uuid.NewString(), user, 1_000_000_000)
	grant.APIKeyID, grant.AccountID, grant.GroupID = key.ID, account.ID, group.ID
	grant.VideoPriceVersions = []string{price.Version()}
	for _, mutate := range []func(*regional.Lease){
		func(l *regional.Lease) { l.VideoPriceVersions = nil },
		func(l *regional.Lease) { l.VideoPriceVersions = []string{strings.Repeat("f", 64)} },
		func(l *regional.Lease) { l.GroupID++ },
	} {
		bad := grant
		mutate(&bad)
		require.ErrorIs(t, primary.Issue(ctx, bad), ledger.ErrConflict)
		var balance string
		require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT balance::text FROM users WHERE id=$1`, user).Scan(&balance))
		require.Equal(t, "10.00000000", balance, "failed price approval must refund the entire grant transaction")
	}
	require.NoError(t, primary.Issue(ctx, grant))
	require.NoError(t, primary.Issue(ctx, grant))
	changed := grant
	changed.VideoPriceVersions = []string{strings.Repeat("f", 64)}
	require.ErrorIs(t, primary.Issue(ctx, changed), ledger.ErrConflict, "retries must retain exact authorizations")
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM regional_prices WHERE version=$1)`, price.Version()).Scan(&exists))
	require.True(t, exists, "the primary installs its own approved derivation during issuance")
	local := &ledger.Local{DB: localDB, Region: grant.RegionID, ValidateAdmission: VerifyRegionalSalePolicy}
	require.NoError(t, local.Install(ctx, grant))
	r := regionalLedgerReservation(t, grant, "two-db-request-"+uuid.NewString(), 0)
	r.APIKeyID, r.AccountID, r.Model, r.PricingVersion = key.ID, account.ID, price.Model, price.Version()
	r.Maximum, err = price.VideoMaximum(price.Video.VideoRequest)
	require.NoError(t, err)
	r.Video = &price.Video.VideoRequest
	r.VideoOrigin = &ledger.VideoOrigin{Member: "video:" + uuid.NewString(), Owner: video.Owner{UserID: user, APIKeyID: key.ID, GroupID: group.ID, AccountID: account.ID, Provider: "grok", RouteFingerprint: service.AccountRouteFingerprint(account)}}
	for _, mutate := range []func(*ledger.Reservation){
		func(r *ledger.Reservation) { r.PricingVersion = template.Version() },
		func(r *ledger.Reservation) { origin := *r.VideoOrigin; origin.Owner.GroupID++; r.VideoOrigin = &origin },
	} {
		bad := r
		mutate(&bad)
		_, err := local.Reserve(ctx, bad)
		require.ErrorIs(t, err, ledger.ErrConflict, "installed but unauthorized prices/groups must stop before dispatch")
	}
	text := ledger.Price{Model: "text-" + uuid.NewString(), Input: 1_000_000_000, Output: 2_000_000_000, MaxInput: 100, MaxOutput: 100}
	for _, db := range []*sql.DB{integrationDB, localDB} {
		require.NoError(t, ledger.InstallPrice(ctx, db, text))
	}
	textHold := r
	textHold.RequestID, textHold.Model, textHold.PricingVersion, textHold.Maximum = "text-"+uuid.NewString(), text.Model, text.Version(), 1000
	textHold.Video, textHold.VideoOrigin = nil, nil
	_, err = local.Reserve(ctx, textHold)
	require.ErrorIs(t, err, ledger.ErrConflict, "non-unit video authorization must not admit unit-priced text")
	textHold.LeaseID, textHold.RegionID = "", ""
	_, err = holds.Reserve(ctx, textHold)
	require.ErrorIs(t, err, ledger.ErrConflict, "main text has the same authoritative policy fence")
	fresh, err := local.Reserve(ctx, r)
	require.NoError(t, err)
	require.True(t, fresh)
	_, err = localDB.ExecContext(ctx, `UPDATE groups SET rate_multiplier=0.15 WHERE id=$1`, group.ID)
	require.NoError(t, err)
	require.ErrorIs(t, local.Start(ctx, r.RequestID), ledger.ErrStartRejected)
	_, err = localDB.ExecContext(ctx, `UPDATE groups SET rate_multiplier=0.12 WHERE id=$1`, group.ID)
	require.NoError(t, err)
	require.NoError(t, local.Start(ctx, r.RequestID), "the original tariff is valid again after policy restoration")
	const taskID = "two-db-task"
	require.NoError(t, local.BindVideoTask(ctx, r.RequestID, taskID))
	_, err = integrationDB.ExecContext(ctx, `UPDATE groups SET rate_multiplier=9 WHERE id=$1`, group.ID)
	require.NoError(t, err)
	usage := ledger.VideoUsage{VideoRequest: *r.Video, TaskID: taskID, State: "completed"}
	_, charge, err := price.VideoCosts(usage)
	require.NoError(t, err)
	event := regionalLedgerEvent(r, charge)
	event.InputTokens, event.OutputTokens, event.Video, event.VideoOrigin = 0, 0, &usage, r.VideoOrigin
	require.NoError(t, local.Complete(ctx, event))
	forged := event
	forged.PricingVersion = template.Version()
	_, err = primary.Settle(ctx, grant.RegionID, forged, verify)
	require.ErrorIs(t, err, ledger.ErrConflict)
	applied, err := primary.Settle(ctx, grant.RegionID, event, verify)
	require.NoError(t, err, "reconciliation uses the issued contract, never today's rate or the regional price table")
	require.True(t, applied)
	require.NoError(t, local.Acknowledge(ctx, event))
	require.NoError(t, local.Drain(ctx, grant.LeaseID))
	receipt, err := local.Close(ctx, grant.LeaseID)
	require.NoError(t, err)
	require.NoError(t, primary.Close(ctx, grant.RegionID, receipt))
}

func regionalVideoFinancialSubject(t *testing.T, provider string) (int64, *service.Group, *service.APIKey, *service.Account) {
	t.Helper()
	user := regionalLedgerUser(t)
	client := testEntClient(t)
	platform := service.PlatformOpenAI
	switch provider {
	case "minimax-h3":
		platform = service.PlatformMiniMax
	case "grok":
		platform = service.PlatformGrok
	}
	group := mustCreateGroup(t, client, &service.Group{Name: "video-funds-" + uuid.NewString(), Platform: platform, RateMultiplier: 1})
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user, GroupID: &group.ID, Key: "sk-fixture-" + uuid.NewString(), Name: "video-funds"})
	account := mustCreateAccount(t, client, &service.Account{Name: "video-funds-" + uuid.NewString(), Type: service.AccountTypeAPIKey, Platform: platform, Status: "active", Schedulable: true})
	_, err := integrationDB.ExecContext(t.Context(), "INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)", account.ID, group.ID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.Exec("DELETE FROM usage_logs WHERE user_id=$1", user)
		_, _ = integrationDB.Exec("DELETE FROM regional_primary_requests WHERE user_id=$1", user)
		_, _ = integrationDB.Exec("DELETE FROM regional_budget_allocations WHERE user_id=$1", user)
	})
	return user, group, key, account
}
