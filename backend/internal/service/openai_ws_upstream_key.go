package service

import (
	"context"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

func (s *OpenAIGatewayService) openAIWSKeyAuthority() (UpstreamKeyConcurrency, error) {
	return sharedWSKeyAuthority(s.cfg, s.httpUpstream)
}

func sharedWSKeyAuthority(cfg *config.Config, upstream HTTPUpstream) (UpstreamKeyConcurrency, error) {
	if !upstreamKeyAdmissionRequired(cfg) {
		return nil, nil
	}
	authority, ok := upstream.(UpstreamKeyConcurrency)
	if !ok {
		return nil, ErrUpstreamKeyPolicy
	}
	return authority, nil
}

// The active generation owns capacity, not its idle socket. A lost lease closes
// upstream before releasing the exact member; completion detaches cancellation
// before releasing so a healthy connection remains reusable.
type openAIWSKeyTurn struct {
	authority    UpstreamKeyConcurrency
	fingerprints []string
	abort        func(error)
	mu           sync.Mutex
	release      func()
}

func (t *openAIWSKeyTurn) begin(ctx context.Context) (context.Context, error) {
	if t.authority == nil {
		return ctx, nil
	}
	for _, fingerprint := range t.fingerprints {
		if fingerprint == config.UnverifiedUpstreamAgentCredential {
			return nil, upstreamKeyAdmissionFailover(ErrUpstreamKeyPolicy)
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.release != nil {
		return nil, ErrUpstreamKeyPolicy
	}
	keyCtx, release, err := t.authority.AcquireUpstreamKeySlots(ctx, t.fingerprints)
	if err != nil {
		if failover := upstreamKeyAdmissionFailover(err); failover != nil {
			return nil, failover
		}
		return nil, err
	}
	if release == nil {
		return ctx, nil
	}
	closed := make(chan struct{})
	stop := context.AfterFunc(keyCtx, func() {
		defer close(closed)
		t.abort(context.Cause(keyCtx))
	})
	t.release = func() {
		if !stop() {
			<-closed
		}
		release()
	}
	return keyCtx, nil
}

func (t *openAIWSKeyTurn) finish() {
	t.mu.Lock()
	release := t.release
	t.release = nil
	t.mu.Unlock()
	if release != nil {
		release()
	}
}
