//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type scheduledMonitorRepoStub struct {
	quotaModeRepoStub
	claimErr error
	renew    func(context.Context) (bool, error)
	claimN   atomic.Int32
	renewN   atomic.Int32
	finishN  atomic.Int32
	releaseN atomic.Int32
	retry    atomic.Bool
	finished bool
}

func (r *scheduledMonitorRepoStub) ClaimScheduledCheck(context.Context, *ChannelMonitor, time.Duration, time.Duration) (*ChannelMonitorScheduledClaim, error) {
	r.claimN.Add(1)
	if r.claimErr != nil {
		return nil, r.claimErr
	}
	return &ChannelMonitorScheduledClaim{ID: "fixture-claim", ConfigurationKey: "fixture-config", NextDelay: time.Minute}, nil
}

func (r *scheduledMonitorRepoStub) RenewScheduledCheck(ctx context.Context, _ int64, _ *ChannelMonitorScheduledClaim, _ time.Duration) (bool, error) {
	r.renewN.Add(1)
	if r.renew != nil {
		return r.renew(ctx)
	}
	return true, nil
}

func (r *scheduledMonitorRepoStub) FinishScheduledCheck(_ context.Context, _ int64, _ *ChannelMonitorScheduledClaim, rows []*ChannelMonitorHistoryRow) (bool, error) {
	r.finishN.Add(1)
	if r.finished {
		r.history = append(r.history, rows...)
	}
	return r.finished, nil
}

func (r *scheduledMonitorRepoStub) ReleaseScheduledCheck(_ context.Context, _ int64, _ *ChannelMonitorScheduledClaim, retry bool) error {
	r.releaseN.Add(1)
	r.retry.Store(retry)
	return nil
}

func scheduledMonitorFixture(t *testing.T, repo *scheduledMonitorRepoStub, call func(*http.Request) (*http.Response, error)) *ChannelMonitorService {
	t.Helper()
	repo.monitor = &ChannelMonitor{ID: 1, Enabled: true, IntervalSeconds: 60, Provider: MonitorProviderOpenAI,
		Endpoint: "https://fixture.example/v1", APIKey: "OLD:fixture", PrimaryModel: "model-a",
		BodyOverrideMode: MonitorBodyOverrideModeReplace,
		BodyOverride:     map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hello"}}}}
	oldModel, oldPing := monitorHTTPClient, monitorPingHTTPClient
	t.Cleanup(func() { monitorHTTPClient, monitorPingHTTPClient = oldModel, oldPing })
	monitorHTTPClient = &http.Client{Transport: monitorBudgetRoundTripper(call)}
	monitorPingHTTPClient = &http.Client{Transport: monitorBudgetRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	svc := NewChannelMonitorService(repo, &duplicateChannelMonitorEncryptor{})
	svc.SetRuntimeReader(channelMonitorRuntimeStub{rt: ChannelMonitorRuntime{Enabled: true, Mode: ChannelMonitorModeV1}})
	svc.requireScheduledClaims = true
	return svc
}

func TestChannelMonitorV1ScheduledLeaseLossAndDrain(t *testing.T) {
	for _, mode := range []string{"false", "error", "deadline", "late_ack", "renewed"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				repo := &scheduledMonitorRepoStub{finished: true}
				unblock := make(chan struct{})
				release := sync.OnceFunc(func() { close(unblock) })
				defer release()
				repo.renew = func(ctx context.Context) (bool, error) {
					switch mode {
					case "false":
						return false, nil
					case "error":
						return false, errors.New("fixture storage unavailable")
					case "deadline":
						<-ctx.Done()
						return false, ctx.Err()
					case "late_ack":
						<-unblock
					}
					return true, nil
				}
				var calls atomic.Int32
				svc := scheduledMonitorFixture(t, repo, func(req *http.Request) (*http.Response, error) {
					calls.Add(1)
					select {
					case <-time.After(100 * time.Second):
						return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"ok"}}]}`))}, nil
					case <-req.Context().Done():
						return nil, req.Context().Err()
					}
				})
				done := make(chan error, 1)
				go func() { _, err := svc.RunCheck(withAutomaticMonitoringProbe(t.Context()), 1); done <- err }()
				synctest.Wait()
				time.Sleep(81 * time.Second)
				synctest.Wait()
				release()
				err := <-done
				if mode == "renewed" {
					require.NoError(t, err)
					require.Len(t, repo.history, 1)
					require.GreaterOrEqual(t, repo.renewN.Load(), int32(2))
					require.False(t, repo.retry.Load())
				} else {
					require.ErrorIs(t, err, errChannelMonitorScheduledClaimLost)
					require.Empty(t, repo.history)
					require.Zero(t, repo.finishN.Load())
					require.EqualValues(t, 1, repo.renewN.Load())
					require.True(t, repo.retry.Load())
				}
				require.EqualValues(t, 1, calls.Load())
				require.EqualValues(t, 1, repo.releaseN.Load())
				n := repo.renewN.Load()
				time.Sleep(3 * time.Minute)
				synctest.Wait()
				require.Equal(t, n, repo.renewN.Load(), "renewal must drain with the original run")
			})
		})
	}
}

func TestChannelMonitorV1ScheduledAdmissionAndManualIsolation(t *testing.T) {
	for _, mode := range []string{"wait", "store_error", "missing_repo", "disabled", "invalid_interval", "manual", "stale_finish"} {
		t.Run(mode, func(t *testing.T) {
			repo := &scheduledMonitorRepoStub{finished: true}
			var calls atomic.Int32
			svc := scheduledMonitorFixture(t, repo, func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"ok"}}]}`))}, nil
			})
			ctx := withAutomaticMonitoringProbe(t.Context())
			switch mode {
			case "wait":
				repo.claimErr = &ChannelMonitorScheduledWait{After: time.Second}
			case "store_error":
				repo.claimErr = errors.New("fixture coordination unavailable")
			case "missing_repo":
				svc.repo = &repo.quotaModeRepoStub
			case "disabled":
				repo.monitor.Enabled = false
			case "invalid_interval":
				repo.monitor.IntervalSeconds = 0
			case "manual":
				ctx = t.Context()
			case "stale_finish":
				repo.finished = false
			}
			_, err := svc.RunCheck(ctx, 1)
			if mode == "manual" {
				require.NoError(t, err)
				require.EqualValues(t, 1, calls.Load())
				require.Zero(t, repo.claimN.Load())
				require.Len(t, repo.history, 1)
			} else if mode == "stale_finish" {
				require.ErrorIs(t, err, errChannelMonitorScheduledClaimLost)
				require.EqualValues(t, 1, calls.Load())
				require.Empty(t, repo.history)
			} else {
				if mode == "wait" || mode == "disabled" {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
				require.Zero(t, calls.Load())
				require.Empty(t, repo.history)
				require.Empty(t, repo.markedIDs)
			}
		})
	}
	// The formal provider must fail closed when its production repo lacks
	// scheduling support, while manual diagnostics retain their old contract.
	svc := ProvideChannelMonitorService(&quotaModeRepoStub{}, nil, nil, &config.Config{}, nil, nil)
	require.True(t, svc.requireScheduledClaims)
}

func TestChannelMonitorV1RunnerUsesDatabaseDueHint(t *testing.T) {
	var calls atomic.Int32
	svc := &stubMonitorSvc{enabled: []*ChannelMonitor{{ID: 1, Enabled: true, IntervalSeconds: 60}}}
	svc.run = func(ctx context.Context, _ int64) ([]*CheckResult, error) {
		require.True(t, ctx.Value(automaticMonitoringProbeKey{}).(bool))
		calls.Add(1)
		// A short returned due hint makes this discriminating without waiting
		// a real minute. Pond's process-global Stop pool is not synctest-owned.
		time.Sleep(5 * time.Millisecond)
		recordChannelMonitorSchedule(ctx, 25*time.Millisecond)
		time.Sleep(10 * time.Millisecond)
		return nil, nil
	}
	runner := newRunnerForTest(svc)
	defer runner.Stop()
	runner.Start()
	require.Eventually(t, func() bool { return calls.Load() >= 2 }, time.Second, 5*time.Millisecond,
		"the original timer must honor storage's due hint instead of its sixty-second fallback")
}
