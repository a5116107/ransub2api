package service

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/gin-gonic/gin"
)

const maxSSETimingBytes = 16 * 1024 * 1024

func newOpenAIWSTurnTiming(account *Account, start time.Time, fingerprints []string) *requesttiming.WSTurnCapture {
	proxyID := int64(0)
	if account.ProxyID != nil {
		proxyID = *account.ProxyID
	}
	capture := requesttiming.NewWSTurn(start, account.ID, proxyID, AccountRouteFingerprint(account))
	capture.RecordUpstreamCredentials(fingerprints)
	return capture
}

func finishOpenAIWSTurnTiming(capture *requesttiming.WSTurnCapture, ctx context.Context, result *OpenAIForwardResult, err error) {
	if capture == nil {
		return
	}
	disconnected := result != nil && result.ClientDisconnect
	if result != nil {
		result.requestTiming = capture.Collector
	}
	capture.Finish(err, ctx != nil && ctx.Err() != nil, disconnected)
}

func startAccountTimingAttempt(request *http.Request, account *Account) (*http.Request, *requesttiming.Trace) {
	proxyID := int64(0)
	if account.ProxyID != nil {
		proxyID = *account.ProxyID
	}
	request, trace := requesttiming.StartAttempt(request, account.ID, proxyID)
	if trace != nil {
		requesttiming.RecordAccountRoute(request.Context(), AccountRouteFingerprint(account))
	}
	return request, trace
}

func responseTimingContext(c *gin.Context, response *http.Response) context.Context {
	var ctx context.Context
	if c != nil && c.Request != nil {
		ctx = c.Request.Context()
	} else if response != nil && response.Request != nil {
		ctx = response.Request.Context()
	}
	if ctx == nil {
		return nil // Standalone parsers without a request retain no timing state.
	}
	return requesttiming.ResponseContext(ctx, response)
}

// sseTimingObserver observes original parser input without altering forwarded
// lines. All supported wire protocols share this bounded event observer.
type sseTimingObserver struct {
	ctx                context.Context
	protocol           string
	timingEventType    string
	timingEventPending bool
	timingData         []byte
	timingDiscard      bool
}

func (s *sseTimingObserver) failTimingCapture() {
	if s.ctx != nil {
		requesttiming.Stage(s.ctx, "typed_content_truncated")
	}
}

func (s *sseTimingObserver) resetTimingEvent() {
	if s.protocol != "responses" && len(s.timingData) > 0 {
		requesttiming.Stage(s.ctx, "typed_content_truncated")
	}
	s.timingEventType = ""
	s.timingEventPending = false
	s.timingData = nil
	s.timingDiscard = false
}

func (s *sseTimingObserver) observeTimingLine(line string) {
	if s.ctx == nil || requesttiming.From(s.ctx) == nil {
		return
	}
	if s.protocol == "gemini" {
		requesttiming.Stage(s.ctx, "typed_gemini_content")
	}
	if s.protocol == "chat_completions" {
		requesttiming.Stage(s.ctx, "typed_chat_completions_content")
	}
	if line == "" || line == "event" {
		s.resetTimingEvent()
		return
	}
	if eventType, ok := extractOpenAISSEEventLine(line); ok {
		s.resetTimingEvent()
		s.timingEventType = eventType
		s.timingEventPending = s.protocol == "messages" &&
			(strings.HasPrefix(eventType, "message_") || strings.HasPrefix(eventType, "content_block_") || eventType == "message" || eventType == "error")
		return
	}
	data, ok := extractOpenAISSEDataLine(line)
	if !ok || s.timingDiscard {
		return
	}
	if s.protocol != "responses" && strings.TrimSpace(data) == "[DONE]" {
		s.resetTimingEvent()
		return
	}
	if len(s.timingData) > 0 {
		if len(data) >= maxSSETimingBytes-len(s.timingData) {
			s.timingData = nil
			s.timingDiscard = true
			requesttiming.Stage(s.ctx, "typed_content_truncated")
			return
		}
		s.timingData = append(s.timingData, '\n')
	} else if len(data) > maxSSETimingBytes {
		s.timingDiscard = true
		requesttiming.Stage(s.ctx, "typed_content_truncated")
		return
	}
	s.timingData = append(s.timingData, data...)
	if !json.Valid(s.timingData) {
		return
	}
	s.timingEventPending = false
	requesttiming.Stage(s.ctx, "sse_parsed_first")
	if s.protocol == "chat_completions" {
		requesttiming.ChatContent(s.ctx, s.timingData, true)
		s.timingData = nil
		return
	}
	if s.protocol == "gemini" {
		requesttiming.GeminiContent(s.ctx, s.timingData, true)
		s.timingData = nil
		return
	}
	eventType := effectiveOpenAISSEEventType(s.timingData, s.timingEventType)
	if (s.protocol == "messages" && strings.HasPrefix(eventType, "response.")) ||
		(s.protocol == "responses" && (strings.HasPrefix(eventType, "message_") || strings.HasPrefix(eventType, "content_block_"))) {
		requesttiming.Stage(s.ctx, "typed_content_truncated")
		s.timingData = nil
		return
	}
	requesttiming.Content(s.ctx, s.timingData, eventType)
	if s.protocol == "responses" && openAIStreamDataStartsVisibleOutput(string(s.timingData), eventType) {
		requesttiming.Stage(s.ctx, "sse_visible_first")
	}
	s.timingData = nil
}

// Match the native forwarding outcome without changing error handling or
// billing. The defer caller passes its final named return values.
func recordForwardTimingOutcome(ctx context.Context, c *gin.Context, disconnected bool, err error) {
	outcome := "success"
	if err != nil || (c != nil && c.Writer != nil && c.Writer.Status() >= http.StatusBadRequest) {
		outcome = "failed"
	}
	if disconnected {
		outcome = "client_disconnected"
	}
	requesttiming.Outcome(ctx, outcome, disconnected)
}

func timingTerminal(event string) string {
	switch event {
	case "response.completed", "response.done":
		return "completed"
	case "response.failed", "error":
		return "failed"
	case "response.incomplete":
		return "incomplete"
	}
	return ""
}

// Optional repository capability avoids changing billing's existing contract.
func (s *UsageService) RequestTimings(ctx context.Context, id int64) ([]json.RawMessage, error) {
	if _, err := s.GetByID(ctx, id); err != nil {
		return nil, err
	}
	if repo, ok := s.usageRepo.(interface {
		RequestTimings(context.Context, int64) ([]json.RawMessage, error)
	}); ok {
		return repo.RequestTimings(ctx, id)
	}
	return []json.RawMessage{}, nil
}
