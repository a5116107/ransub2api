//go:build unit

package service

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type asyncVideoRecoveryStub struct {
	HTTPUpstream
	started chan struct{}
	stopped chan struct{}
	once    sync.Once
	calls   atomic.Int32
	local   bool
}

func (r *asyncVideoRecoveryStub) IsLocalAsyncVideoKeyReleaseRecovery() bool { return r.local }

func (r *asyncVideoRecoveryStub) RecoverAsyncVideoKeyReleases(ctx context.Context, limit int) (int, error) {
	if limit != asyncVideoKeyReleaseRecoveryBatch {
		return 0, ErrUpstreamKeyPolicy
	}
	r.calls.Add(1)
	r.once.Do(func() { close(r.started) })
	<-ctx.Done()
	close(r.stopped)
	return 0, ctx.Err()
}

func TestAsyncVideoKeyRecoveryLifecycle(t *testing.T) {
	recovery := &asyncVideoRecoveryStub{started: make(chan struct{}), stopped: make(chan struct{})}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: recovery}
	svc.StartAsyncVideoKeyReleaseRecovery()
	svc.StartAsyncVideoKeyReleaseRecovery()
	t.Cleanup(svc.StopAsyncVideoKeyReleaseRecovery)
	select {
	case <-recovery.started:
	case <-time.After(time.Second):
		t.Fatal("recovery worker did not start")
	}
	require.EqualValues(t, 1, recovery.calls.Load())
	done := make(chan struct{})
	go func() { svc.StopAsyncVideoKeyReleaseRecovery(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("recovery worker did not stop")
	}
	select {
	case <-recovery.stopped:
	default:
		t.Fatal("the active recovery round was not cancelled")
	}
	svc.StartAsyncVideoKeyReleaseRecovery()
	svc.StopAsyncVideoKeyReleaseRecovery()
	require.EqualValues(t, 1, recovery.calls.Load(), "a stopped worker cannot restart")
}

func TestAsyncVideoKeyRecoveryRespectsGatewayRoleAndFactory(t *testing.T) {
	for _, tc := range []struct {
		name        string
		gatewayOnly bool
		local       bool
		shouldStart bool
	}{
		{"full", false, false, true},
		{"gateway without local authority", true, false, false},
		{"gateway with local authority", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recovery := &asyncVideoRecoveryStub{started: make(chan struct{}), stopped: make(chan struct{}), local: tc.local}
			cfg := &config.Config{}
			if tc.gatewayOnly {
				cfg.Runtime.Role = config.RuntimeRoleGateway
			}
			svc := ProvideOpenAIGatewayService(OpenAIGatewayDependencies{Config: cfg, Upstream: recovery})
			t.Cleanup(svc.StopOpenAICodexTicketHarvester)
			t.Cleanup(svc.StopAsyncVideoKeyReleaseRecovery)
			if !tc.shouldStart {
				require.Nil(t, svc.asyncVideoRecoveryDone)
				require.Zero(t, recovery.calls.Load())
				return
			}
			select {
			case <-recovery.started:
			case <-time.After(time.Second):
				t.Fatal("production factory did not start recovery")
			}
		})
	}
}
