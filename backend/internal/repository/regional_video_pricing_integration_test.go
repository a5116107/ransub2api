//go:build integration

package repository

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/regional"
	"github.com/Wei-Shaw/sub2api/internal/regional/ledger"
	"github.com/Wei-Shaw/sub2api/internal/regional/video"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
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
				_, err := integrationDB.ExecContext(ctx, "UPDATE regional_prices SET price='{}' WHERE version=$1", price.Version())
				require.Error(t, err, "published video prices must remain immutable")
				maximum, err := price.VideoMaximum(price.Video.VideoRequest)
				require.NoError(t, err)
				primary, holds, verify := NewRegionalFundsAuthorities(integrationDB, func(context.Context, *sql.Tx) error { return nil })
				lease := regionalLedgerLease(t, "video-grant-"+uuid.NewString(), user, 8_000_000_000)
				lease.APIKeyID, lease.AccountID = key.ID, account.ID
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
				if tc.failed {
					e.Video.State = "failed"
				}
				require.ErrorIs(t, requests.Complete(ctx, e), ledger.ErrConflict, "terminal evidence must not assign an unbound task")
				for _, invalidTask := range []string{"", " task ", "task\r\n", "task\x00id"} {
					require.ErrorIs(t, requests.BindVideoTask(ctx, r.RequestID, invalidTask), ledger.ErrConflict)
				}
				canceledBind, cancelBind := context.WithCancel(ctx)
				cancelBind()
				require.Error(t, requests.BindVideoTask(canceledBind, r.RequestID, e.Video.TaskID))
				require.NoError(t, requests.BindVideoTask(ctx, r.RequestID, e.Video.TaskID))
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
