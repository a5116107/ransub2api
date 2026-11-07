package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
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
	return controller.BindAsyncVideoKeyLease(ctx, handoff, strings.TrimSpace(taskID))
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
	controller, ok := s.httpUpstream.(AsyncVideoKeyLeaseController)
	if !ok {
		return nil
	}
	return controller.ReleaseAsyncVideoKeyLeaseByTask(ctx, strings.TrimSpace(taskID))
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
