package service

import (
	"context"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
)

// AvailabilityProbeResult records one completed answer attempt before grading.
// It is evidence for a future latest-state decision, not scheduling permission.
// The allowlist excludes trace IDs, credentials, routes and upstream error text.
type AvailabilityProbeResult struct {
	Version        int                       `json:"version"`
	Status         string                    `json:"status"`
	Reason         string                    `json:"reason"`
	HTTPStatus     int                       `json:"http_status,omitempty"`
	SemanticStatus int                       `json:"semantic_status,omitempty"`
	AttemptCount   int                       `json:"attempt_count"`
	StartedAt      time.Time                 `json:"started_at"`
	FinishedAt     time.Time                 `json:"finished_at"`
	Scope          *AvailabilityRequestScope `json:"scope,omitempty"`
}

type availabilityProbeSnapshotKey struct{}

type availabilityProbeObservation struct {
	snapshot       *requesttiming.Snapshot
	semanticStatus int
	scope          *AvailabilityRequestScope
	scopeAttempt   int
}

func observeAvailabilityProbeFailure(ctx context.Context, payload []byte, message string) {
	if observation, ok := ctx.Value(availabilityProbeSnapshotKey{}).(*availabilityProbeObservation); ok {
		observation.semanticStatus = openAIStreamFailedEventSemanticStatus(payload, message)
	}
}

func availabilityProbeResult(started, finished time.Time, observation *availabilityProbeObservation, complete, captureLimited bool, ctxErr error) *AvailabilityProbeResult {
	result := &AvailabilityProbeResult{Version: 1, Status: "inconclusive", Reason: "not_observed", StartedAt: started, FinishedAt: finished}
	if observation == nil || observation.snapshot == nil {
		return result
	}
	snapshot := observation.snapshot
	var last *requesttiming.Attempt
	for i := range snapshot.Attempts {
		attempt := &snapshot.Attempts[i]
		if attempt.Kind == "egress" {
			result.AttemptCount++
			last = attempt
		}
	}
	if snapshot.Truncated {
		result.Reason = "truncated_evidence"
		return result
	}
	if last == nil {
		return result
	}
	if observation.scopeAttempt == last.Number && observation.scope != nil {
		scope := *observation.scope
		result.Scope = &scope
	}
	result.HTTPStatus = last.Status
	status := last.Status
	if observation.semanticStatus != 0 {
		result.SemanticStatus = observation.semanticStatus
		status = observation.semanticStatus
	}
	if complete && snapshot.Outcome == "success" && (status >= 300 || last.Error != "") {
		result.Reason = "conflicting_evidence"
		return result
	}
	switch {
	case status >= 500 && status <= 599:
		result.Status, result.Reason = "fail", "upstream_error"
		return result
	case status == 401:
		result.Status, result.Reason = "fail", "authentication_rejected"
		return result
	case status == 408:
		result.Status, result.Reason = "fail", "upstream_timeout"
		return result
	case status == 429:
		result.Status, result.Reason = "fail", "rate_limited"
		return result
	case status >= 300:
		result.Reason = "request_rejected"
		return result
	}
	if captureLimited {
		result.Reason = "capture_limited"
		return result
	}
	if errors.Is(ctxErr, context.Canceled) || snapshot.Canceled || snapshot.ClientDisconnect || last.Error == "canceled" {
		result.Reason = "canceled"
		return result
	}
	// An egress error can be a local queue/admission failure. Network activity
	// or an actual response must support a transport failure conclusion.
	physical := last.Status > 0
	for _, attempt := range snapshot.Attempts {
		if attempt.Number != last.Number && (attempt.Kind != "http" || attempt.Parent != last.Number) {
			continue
		}
		for _, event := range []string{"connection_start", "request_written", "response_headers"} {
			if _, ok := attempt.Events[event]; ok {
				physical = true
			}
		}
	}
	if physical && (last.Error == "timeout" || last.Error == "transport_error") {
		result.Status, result.Reason = "fail", "transport_error"
		if last.Error == "timeout" {
			result.Reason = "upstream_timeout"
		}
		return result
	}
	result.Reason = "response_not_confirmed"
	if last.Status >= 200 && last.Status < 300 && last.Error == "" && snapshot.Outcome == "success" && complete {
		result.Status, result.Reason = "pass", "completed_response"
	}
	return result
}
