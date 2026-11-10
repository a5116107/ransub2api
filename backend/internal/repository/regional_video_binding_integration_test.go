//go:build integration

package repository

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/regional/ledger"
	"github.com/Wei-Shaw/sub2api/internal/regional/video"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRegionalVideoFinancialBindingCompetitionAndRecovery(t *testing.T) {
	for _, mode := range []string{"primary", "regional"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			user, group, key, account := regionalVideoFinancialSubject(t, "minimax-h3")
			price := ledger.Price{Model: "binding-" + uuid.NewString(), Video: &ledger.VideoPrice{
				VideoRequest: ledger.VideoRequest{Provider: "minimax-h3", UpstreamModel: "MiniMax-H3", Resolution: "768p", DurationSeconds: 5},
				Basis:        "request", UnitNano: 20_000_000, SaleNumerator: 1, SaleDenominator: 10,
			}}
			require.NoError(t, ledger.InstallPrice(ctx, integrationDB, price))
			primary, holds, verify := NewRegionalFundsAuthorities(integrationDB, func(context.Context, *sql.Tx) error { return nil })
			lease := regionalLedgerLease(t, "binding-grant-"+uuid.NewString(), user, 8_000_000_000)
			lease.APIKeyID, lease.AccountID = key.ID, account.ID
			r := regionalLedgerReservation(t, lease, "binding-request-"+uuid.NewString(), 2_000_000)
			r.APIKeyID, r.AccountID, r.Model, r.PricingVersion = key.ID, account.ID, price.Model, price.Version()
			r.Video = &price.Video.VideoRequest
			r.VideoOrigin = &ledger.VideoOrigin{Member: "video:" + uuid.NewString(), Owner: video.Owner{UserID: user, APIKeyID: key.ID, GroupID: group.ID, AccountID: account.ID, Provider: "minimax-h3", RouteFingerprint: service.AccountRouteFingerprint(account)}}
			var requests ledger.VideoTaskLedger = holds
			if mode == "regional" {
				require.NoError(t, primary.Issue(ctx, lease))
				local := &ledger.Local{DB: integrationDB, Region: lease.RegionID}
				require.NoError(t, local.Install(ctx, lease))
				requests = local
			} else {
				r.LeaseID, r.RegionID = "", ""
				bad := r
				badOrigin := *r.VideoOrigin
				badOrigin.Owner.GroupID++
				bad.VideoOrigin = &badOrigin
				_, err := requests.Reserve(ctx, bad)
				require.ErrorIs(t, err, ledger.ErrConflict, "origin group must match the allocated financial scope")
			}
			other := r
			other.RequestID = "binding-other-" + uuid.NewString()
			otherOrigin := *r.VideoOrigin
			otherOrigin.Member = "video:" + uuid.NewString()
			other.VideoOrigin = &otherOrigin
			for _, hold := range []ledger.Reservation{r, other} {
				fresh, err := requests.Reserve(ctx, hold)
				require.NoError(t, err)
				require.True(t, fresh)
				require.ErrorIs(t, requests.BindVideoTask(ctx, hold.RequestID, "unsent"), ledger.ErrAlreadyDispatched)
				require.NoError(t, requests.Start(ctx, hold.RequestID))
			}
			reusedMember := r
			reusedMember.RequestID = "binding-reused-" + uuid.NewString()
			_, err := requests.Reserve(ctx, reusedMember)
			require.ErrorIs(t, err, ledger.ErrConflict)
			type bindingResult struct {
				hold ledger.Reservation
				err  error
			}
			results := make(chan bindingResult, 2)
			var workers sync.WaitGroup
			// The same raw ID also occurs under the next mode's different user:
			// tenant separation must not turn unrelated tasks into one charge.
			const taskID = "same-provider-task"
			for _, hold := range []ledger.Reservation{r, other} {
				workers.Go(func() { results <- bindingResult{hold, requests.BindVideoTask(ctx, hold.RequestID, taskID)} })
			}
			workers.Wait()
			close(results)
			var winner, loser ledger.Reservation
			won := 0
			for result := range results {
				if result.err == nil {
					winner = result.hold
					won++
				} else {
					require.ErrorIs(t, result.err, ledger.ErrConflict)
					loser = result.hold
				}
			}
			require.Equal(t, 1, won)
			makeEvent := func(hold ledger.Reservation, id, state string) ledger.Event {
				charge := hold.Maximum
				if state != "completed" {
					charge = 0
				}
				e := regionalLedgerEvent(hold, charge)
				e.InputTokens, e.OutputTokens = 0, 0
				e.Video = &ledger.VideoUsage{VideoRequest: *hold.Video, TaskID: id, State: state}
				e.VideoOrigin = hold.VideoOrigin
				return e
			}
			require.ErrorIs(t, requests.Complete(ctx, makeEvent(loser, taskID, "completed")), ledger.ErrConflict)
			require.ErrorIs(t, requests.CancelBeforeStart(ctx, loser.RequestID), ledger.ErrAlreadyDispatched, "a conflicting task response must not refund possibly dispatched work")
			if mode == "regional" {
				requests = &ledger.Local{DB: integrationDB, Region: lease.RegionID}
			} else {
				_, restarted, _ := NewRegionalFundsAuthorities(integrationDB, func(context.Context, *sql.Tx) error { return nil })
				requests = restarted
			}
			record, err := requests.LoadVideoTask(ctx, winner.VideoOrigin.Owner, taskID)
			require.NoError(t, err)
			require.Equal(t, winner, record.Reservation)
			require.Equal(t, "started", record.State)
			for _, statement := range []string{
				`UPDATE regional_video_financial_bindings SET member='video:00000000-0000-0000-0000-000000000000' WHERE request_id=$1`,
				`UPDATE regional_video_financial_bindings SET task_id='replacement' WHERE request_id=$1`,
				`DELETE FROM regional_video_financial_bindings WHERE request_id=$1`,
			} {
				_, err := integrationDB.ExecContext(ctx, statement, winner.RequestID)
				require.Error(t, err, "financial task identities must remain immutable after slot release or application rollback")
			}
			// Recovery can bind a different, verified task to the still-unknown
			// loser; an explicit failed terminal event then releases only its hold.
			require.NoError(t, requests.BindVideoTask(ctx, loser.RequestID, "verified-other-task"))
			events := []ledger.Event{makeEvent(winner, taskID, "completed"), makeEvent(loser, "verified-other-task", "failed")}
			for _, e := range events {
				require.NoError(t, requests.Complete(ctx, e))
				if mode == "regional" {
					_, err := primary.Settle(ctx, lease.RegionID, e, verify)
					require.NoError(t, err)
					localLedger, ok := requests.(*ledger.Local)
					require.True(t, ok)
					require.NoError(t, localLedger.Acknowledge(ctx, e))
				}
			}
			record, err = requests.LoadVideoTask(ctx, winner.VideoOrigin.Owner, taskID)
			require.NoError(t, err)
			require.Equal(t, &events[0], record.Event)
			require.NoError(t, requests.Complete(ctx, *record.Event))
			// Primary requests and every regional grant share one permanent
			// primary namespace, even when event/member/lease/region all change.
			cross := regionalLedgerLease(t, "binding-cross-"+uuid.NewString(), user, 10_000_000)
			cross.RegionID, cross.APIKeyID, cross.AccountID = "eu", key.ID, account.ID
			require.NoError(t, primary.Issue(ctx, cross))
			duplicate := events[0]
			duplicate.Usage.EventID = "binding-replay-" + uuid.NewString()
			duplicate.Usage.LeaseID, duplicate.Usage.RegionID = cross.LeaseID, cross.RegionID
			duplicate.Usage.AdmittedAt = regionalLedgerClock(t)
			duplicate.Usage.CompletedAt = duplicate.Usage.AdmittedAt
			duplicateOrigin := *duplicate.VideoOrigin
			duplicateOrigin.Member = "video:" + uuid.NewString()
			duplicate.VideoOrigin = &duplicateOrigin
			applied, err := primary.Settle(ctx, cross.RegionID, duplicate, verify)
			require.ErrorIs(t, err, ledger.ErrConflict)
			require.False(t, applied)
			var orphaned, usage int
			require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT count(*) FROM regional_video_financial_bindings WHERE request_id=$1`, duplicate.Usage.EventID).Scan(&orphaned))
			require.Zero(t, orphaned, "rejected primary claims roll back with money and canonical usage")
			require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT count(*) FROM usage_logs WHERE user_id=$1`, user).Scan(&usage))
			require.Equal(t, 2, usage, "one completed and one explicitly failed task, with no duplicate financial event")
			// A failed independent scope verification must also roll back the new
			// binding, so a corrected event can be retried after recovery.
			uniqueTask := *duplicate.Video
			uniqueTask.TaskID = "independent-scope-task"
			duplicate.Video = &uniqueTask
			duplicateOrigin.Owner.GroupID++
			_, err = primary.Settle(ctx, cross.RegionID, duplicate, verify)
			require.ErrorIs(t, err, ledger.ErrConflict, "primary independently checks original grant group attribution")
			duplicateOrigin.Owner.GroupID--
			_, err = primary.Settle(ctx, cross.RegionID, duplicate, func(context.Context, *sql.Tx, ledger.Event) error { return errors.New("fixture authority unavailable") })
			require.ErrorContains(t, err, "authority unavailable")
			require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT count(*) FROM regional_video_financial_bindings WHERE request_id=$1`, duplicate.Usage.EventID).Scan(&orphaned))
			require.Zero(t, orphaned)
			_, err = primary.Settle(ctx, cross.RegionID, duplicate, verify)
			require.NoError(t, err)
		})
	}
}
