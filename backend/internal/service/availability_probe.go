package service

import (
	"context"
	"errors"
	"net/http"
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

// An internal-only scheduled probe target; never accepted from request JSON or
// stored in the public result history.
type availabilityProbeConnectionKey struct{}

type availabilityProbeConnection struct {
	accountID    int64
	connectionID string
}

func withAvailabilityProbeConnection(ctx context.Context, accountID int64, connectionID string) context.Context {
	return context.WithValue(ctx, availabilityProbeConnectionKey{}, availabilityProbeConnection{accountID: accountID, connectionID: connectionID})
}

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

// Reuse the request's existing collector for each WS attempt, including pooled
// connections. A handshake alone never publishes a passing capability: the
// actual bound socket must accept the exact business frame and finish its answer.
func beginAvailabilityWSProbe(ctx context.Context, account *Account, target string) (context.Context, func(*openAIWSConn, any), func(error)) {
	observation, _ := ctx.Value(availabilityProbeSnapshotKey{}).(*availabilityProbeObservation)
	if observation == nil {
		return ctx, func(*openAIWSConn, any) {}, func(error) {}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return ctx, func(*openAIWSConn, any) {}, func(error) {}
	}
	request, trace := requesttiming.StartAttempt(request, account.ID, 0)
	observation.scope, observation.scopeAttempt, observation.semanticStatus = nil, trace.Number(), 0
	ctx = request.Context()
	sent := false
	return ctx, func(conn *openAIWSConn, payload any) {
			if conn == nil {
				return
			}
			wire, ok := conn.ws.(*coderOpenAIWSClientConn)
			if !ok {
				return // An opaque adapter's WriteJSON is not physical evidence.
			}
			sent = true
			requesttiming.Stage(ctx, "request_written")
			if trace.Number() == 0 || conn.turnBinding == nil || conn.turnBinding.availability == nil {
				return
			}
			binding := conn.turnBinding.availability
			if binding.requestHash == ([32]byte{}) || wire.availabilityHandshakeHash != binding.requestHash {
				return
			}
			observation.scope = binding.scope(account, payload)
		}, func(err error) {
			if sent {
				trace.Response(&http.Response{StatusCode: http.StatusSwitchingProtocols}, err)
				return
			}
			var dial *openAIWSDialError
			if errors.As(err, &dial) && (dial.networkAttempted || dial.StatusCode > 0) {
				if dial.networkAttempted {
					requesttiming.Stage(ctx, "connection_start")
				}
				trace.Response(&http.Response{StatusCode: dial.StatusCode}, dial.Err)
				return
			}
			trace.Response(nil, nil) // Local admission/queue failures are not network evidence.
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
	responseOK := last.Status >= 200 && last.Status < 300
	if last.Status == http.StatusSwitchingProtocols && result.Scope != nil && result.Scope.Protocol == "responses_ws_v2" {
		responseOK = true
	}
	if responseOK && last.Error == "" && snapshot.Outcome == "success" && complete {
		result.Status, result.Reason = "pass", "completed_response"
	}
	return result
}
