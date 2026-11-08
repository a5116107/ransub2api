package service

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// asyncVideoKeyController returns the transport-owned durable handoff. A
// custom test transport without a shared-key lease remains a valid legacy
// path; once it attaches a lease, the controller is mandatory.
func (s *OpenAIGatewayService) asyncVideoKeyController(handoff *AsyncVideoKeyHandoff) (AsyncVideoKeyLeaseController, bool) {
	if handoff == nil || handoff.Lease() == nil || s == nil || s.httpUpstream == nil {
		return nil, false
	}
	controller, ok := s.httpUpstream.(AsyncVideoKeyLeaseController)
	return controller, ok
}

func (s *OpenAIGatewayService) bindAsyncVideoKeyLease(ctx context.Context, req *http.Request, taskID string) error {
	handoff := AsyncVideoKeyHandoffFromRequest(req)
	if handoff == nil || handoff.Lease() == nil {
		return nil
	}
	controller, ok := s.asyncVideoKeyController(handoff)
	if !ok {
		return ErrUpstreamKeyPolicy
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	taskID = strings.TrimSpace(taskID)
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		err = controller.BindAsyncVideoKeyLease(ctx, handoff, taskID)
		if err == nil || errors.Is(err, ErrUpstreamKeyPolicy) || errors.Is(err, ErrUpstreamKeyLease) || ctx.Err() != nil {
			return err
		}
	}
	return err
}

func (s *OpenAIGatewayService) markAsyncVideoKeySubmissionUnknown(ctx context.Context, req *http.Request) error {
	handoff := AsyncVideoKeyHandoffFromRequest(req)
	if handoff == nil || handoff.Lease() == nil {
		return nil
	}
	controller, ok := s.asyncVideoKeyController(handoff)
	if !ok {
		return ErrUpstreamKeyPolicy
	}
	return controller.MarkAsyncVideoKeySubmissionUnknown(ctx, handoff)
}

func (s *OpenAIGatewayService) releaseAsyncVideoKeyLease(ctx context.Context, req *http.Request) error {
	handoff := AsyncVideoKeyHandoffFromRequest(req)
	if handoff == nil || handoff.Lease() == nil {
		return nil
	}
	controller, ok := s.asyncVideoKeyController(handoff)
	if !ok {
		return ErrUpstreamKeyPolicy
	}
	return controller.ReleaseAsyncVideoKeyLease(ctx, handoff)
}

// ReleaseAsyncVideoTaskLease is used by status/content/delete handlers after
// the provider reports a terminal task state. It is safe across instances and
// idempotent when the task was already released or never had a shared lease.
func (s *OpenAIGatewayService) ReleaseAsyncVideoTaskLease(ctx context.Context, taskID string) error {
	if s == nil || s.httpUpstream == nil || strings.TrimSpace(taskID) == "" {
		return nil
	}
	owner, ok := AsyncVideoKeyOwnerFromContext(ctx)
	if !ok {
		return ErrUpstreamKeyPolicy
	}
	controller, ok := s.httpUpstream.(AsyncVideoKeyLeaseController)
	if !ok {
		return nil
	}
	return controller.ReleaseAsyncVideoKeyLeaseByTask(ctx, owner, strings.TrimSpace(taskID))
}

// prepareAsyncVideoKeyLookup allows only the authenticated task owner to use
// its existing persistent member for a read/cancel request. The transport
// checks the final target and credentials again before the physical send.
func (s *OpenAIGatewayService) prepareAsyncVideoKeyLookup(req *http.Request, account *Account, endpoint GrokMediaEndpoint, taskID string) (*http.Request, error) {
	if s == nil || s.httpUpstream == nil || req == nil || strings.TrimSpace(taskID) == "" {
		return req, nil
	}
	owner, ok := AsyncVideoKeyOwnerFromContext(req.Context())
	if !ok {
		return req, nil
	}
	if account == nil || owner.AccountID != account.ID || owner.Provider != endpoint.AsyncVideoProvider() || owner.RouteFingerprint != AccountRouteFingerprint(account) {
		return nil, ErrUpstreamKeyPolicy
	}
	controller, ok := s.httpUpstream.(AsyncVideoKeyLeaseController)
	if !ok {
		return req, nil
	}
	lease, err := controller.LoadAsyncVideoKeyLeaseByTask(req.Context(), owner, strings.TrimSpace(taskID))
	if err != nil {
		return nil, err
	}
	if lease == nil {
		return req, nil
	}
	bound := WithAsyncVideoKeyLookup(req, lease)
	return bound.WithContext(WithHTTPUpstreamRedirectsDisabled(bound.Context())), nil
}

// BindAsyncVideoTaskLeaseByMember completes recovery when a process lost the
// response after the provider accepted a task and only the provisional member
// survived. The reconciler supplies the provider task ID after an out-of-band
// status lookup; no new upstream submission is made.
func (s *OpenAIGatewayService) BindAsyncVideoTaskLeaseByMember(ctx context.Context, member, taskID string) error {
	if s == nil || s.httpUpstream == nil {
		return nil
	}
	controller, ok := s.httpUpstream.(AsyncVideoKeyLeaseController)
	if !ok {
		return nil
	}
	return controller.BindAsyncVideoKeyLeaseByMember(ctx, strings.TrimSpace(member), strings.TrimSpace(taskID))
}

// ReleaseAsyncVideoTaskLeaseByMember closes an unknown submission after an
// operator/provider reconciliation proves that no remote task exists.
func (s *OpenAIGatewayService) ReleaseAsyncVideoTaskLeaseByMember(ctx context.Context, member string) error {
	if s == nil || s.httpUpstream == nil {
		return nil
	}
	controller, ok := s.httpUpstream.(AsyncVideoKeyLeaseController)
	if !ok {
		return nil
	}
	return controller.ReleaseAsyncVideoKeyLeaseByMember(ctx, strings.TrimSpace(member))
}

const asyncVideoKeyReleaseRecoveryInterval = 30 * time.Second
const asyncVideoKeyReleaseRecoveryTimeout = 10 * time.Second
const asyncVideoKeyReleaseRecoveryBatch = 64

// StartAsyncVideoKeyReleaseRecovery retries only releases fenced by a prior
// confirmed terminal observation or explicit reconciliation.
func (s *OpenAIGatewayService) StartAsyncVideoKeyReleaseRecovery() {
	if s == nil || !s.cfg.RunsBackgroundJobs() {
		return
	}
	recovery, ok := s.httpUpstream.(AsyncVideoKeyReleaseRecovery)
	if !ok {
		return
	}
	s.asyncVideoRecoveryMu.Lock()
	defer s.asyncVideoRecoveryMu.Unlock()
	if s.asyncVideoRecoveryStopped || s.asyncVideoRecoveryDone != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.asyncVideoRecoveryCancel, s.asyncVideoRecoveryDone = cancel, done
	go func() {
		defer close(done)
		timer := time.NewTimer(0)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				round, stop := context.WithTimeout(ctx, asyncVideoKeyReleaseRecoveryTimeout)
				_, err := recovery.RecoverAsyncVideoKeyReleases(round, asyncVideoKeyReleaseRecoveryBatch)
				stop()
				if err != nil && ctx.Err() == nil {
					slog.Warn("async_video_key_release_recovery_deferred")
				}
				timer.Reset(asyncVideoKeyReleaseRecoveryInterval)
			}
		}
	}()
}

func (s *OpenAIGatewayService) StopAsyncVideoKeyReleaseRecovery() {
	if s == nil {
		return
	}
	s.asyncVideoRecoveryMu.Lock()
	s.asyncVideoRecoveryStopped = true
	cancel, done := s.asyncVideoRecoveryCancel, s.asyncVideoRecoveryDone
	s.asyncVideoRecoveryMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func asyncVideoExplicitRejection(err error) bool {
	if err == nil || IsUpstreamKeyAdmissionFailure(err) {
		return true
	}
	var failover *UpstreamFailoverError
	if !errors.As(err, &failover) {
		return false
	}
	switch failover.StatusCode {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden,
		http.StatusMethodNotAllowed, http.StatusUnprocessableEntity, http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}

// finalizeAsyncVideoKeyError preserves a provisional member for transport or
// server outcomes whose remote task state is unknown. Explicit rejections can
// safely release it and retain the existing bounded failover behavior.
func (s *OpenAIGatewayService) finalizeAsyncVideoKeyError(ctx context.Context, req *http.Request, err error) error {
	if asyncVideoExplicitRejection(err) {
		return s.releaseAsyncVideoKeyLease(ctx, req)
	}
	return s.markAsyncVideoKeySubmissionUnknown(ctx, req)
}
