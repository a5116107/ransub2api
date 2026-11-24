package service

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type cnTurnFaultRepo struct {
	*fakeCNCheckRepo
	fail      string
	finishes  int
	finishCtx error
}

func (r *cnTurnFaultRepo) SyncCNMonitoringTurns(ctx context.Context, targets []CNMonitoringTurn, complete bool) (*CNMonitoringQueue, error) {
	if r.fail == "read" {
		return nil, errors.New("fixture storage read failed")
	}
	return r.fakeCNCheckRepo.SyncCNMonitoringTurns(ctx, targets, complete)
}

func (r *cnTurnFaultRepo) ClaimCNMonitoringTurn(ctx context.Context, turn CNMonitoringTurn) (CNMonitoringTurn, bool, error) {
	if r.fail == "claim" {
		return turn, false, errors.New("fixture storage claim failed")
	}
	if r.fail == "stale_claim" {
		return turn, false, nil
	}
	return r.fakeCNCheckRepo.ClaimCNMonitoringTurn(ctx, turn)
}

func (r *cnTurnFaultRepo) FinishCNMonitoringTurn(ctx context.Context, turn CNMonitoringTurn, round int64) (bool, error) {
	r.finishes++
	r.finishCtx = ctx.Err()
	if r.fail == "finish" {
		return false, errors.New("fixture storage write failed")
	}
	return r.fakeCNCheckRepo.FinishCNMonitoringTurn(ctx, turn, round)
}

func TestCNMonitoringTurnSchedulingFailuresStopAndRecover(t *testing.T) {
	for _, phase := range []string{"missing", "read", "claim", "stale_claim", "finish"} {
		t.Run(phase, func(t *testing.T) {
			repo := &cnTurnFaultRepo{fakeCNCheckRepo: cnQueueAccounts(t, 12), fail: phase}
			var calls atomic.Int64
			cfg := &config.Config{}
			cfg.Gateway.CNProviders.BalanceCheckEnabled = true
			svc := NewCNProviderBalanceCheckService(repo, &CNProviderBalanceService{}, nil, cfg, time.Minute)
			svc.quotaService = cnQueueQuotaProber{query: func(context.Context, int64) (*CNProviderQuotaProbeResult, error) {
				calls.Add(1)
				return &CNProviderQuotaProbeResult{Success: true}, nil
			}}
			if phase == "missing" {
				svc.accountRepo = struct{ AccountRepository }{repo}
			}
			svc.runOnce(t.Context())
			if phase == "finish" {
				require.EqualValues(t, cnQuotaProbeConcurrency, calls.Load(), "in-flight workers drain, but failed scheduling writes stop the next wave")
				require.Equal(t, cnQuotaProbeConcurrency, repo.finishes)
			} else {
				require.Zero(t, calls.Load(), "missing/read/claim failure cannot silently fall back to local scheduling")
			}
			if phase != "stale_claim" {
				require.Equal(t, "coordination_unavailable", svc.MonitoringRuntime().State)
			}
			cfg.Gateway.CNProviders.BalanceCheckEnabled = false
			require.Equal(t, "disabled", svc.MonitoringRuntime().State, "explicit administrator configuration still wins")
			cfg.Gateway.CNProviders.BalanceCheckEnabled = true
			repo.fail, svc.accountRepo = "", repo
			before := calls.Load()
			svc.runOnce(t.Context())
			require.EqualValues(t, 12, calls.Load()-before)
			require.Equal(t, "not_started", svc.MonitoringRuntime().State, "external RunDue does not pretend a ticker was started")
		})
	}
}

func TestCNMonitoringTurnCanceledPhysicalAttemptRetainsConsumedAge(t *testing.T) {
	repo := &cnTurnFaultRepo{fakeCNCheckRepo: cnQueueAccounts(t, 1)}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	svc := NewCNProviderBalanceCheckService(repo, &CNProviderBalanceService{}, nil, &config.Config{}, time.Minute)
	svc.SetMonitoringBudget(NewMonitoringProbeBudgetService(&monitoringBudgetRepoStub{}))
	svc.quotaService = cnQueueQuotaProber{query: func(ctx context.Context, id int64) (*CNProviderQuotaProbeResult, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://fixture.invalid/usage", nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer cn-private-fixture")
		req, finish, err := BeginMonitoringProbeRequest(req)
		if err != nil {
			return nil, err
		}
		cancel()
		return nil, FinishMonitoringProbeResponse(req.Context(), nil, context.Canceled, finish)
	}}
	svc.runOnce(ctx)
	require.Equal(t, 1, repo.finishes, "an admitted canceled call consumed scheduling capacity, not passing evidence")
	require.NoError(t, repo.finishCtx, "persist only scheduling age with a bounded independent cleanup context")
	require.Greater(t, repo.turns[1].Served, int64(1))
}

func TestCNMonitoringTurnIdentityIgnoresObservationButTracksRoutes(t *testing.T) {
	account := &Account{ID: 1, Platform: PlatformKimi, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "private-fixture", "account_mode": AccountModeCoding},
		Extra:       map[string]any{}, Proxy: &Proxy{ID: 2, Host: "fixture", Port: 443}}
	target := accountProbeTarget{account: account}
	original, err := cnMonitoringTurn(target)
	require.NoError(t, err)
	account.Extra["balance_updated_at"], account.Schedulable = time.Now(), false
	account.Proxy.Name, account.Proxy.UpdatedAt = "new display label", time.Now()
	unchanged, err := cnMonitoringTurn(target)
	require.NoError(t, err)
	require.Equal(t, original, unchanged)
	account.Proxy.Host = "changed-route"
	changed, err := cnMonitoringTurn(target)
	require.NoError(t, err)
	require.NotEqual(t, original.IdentityHash, changed.IdentityHash)
	account.Credentials["api_key"] = "replacement-fixture"
	credential, err := cnMonitoringTurn(target)
	require.NoError(t, err)
	require.NotEqual(t, changed.IdentityHash, credential.IdentityHash)
}
