package service

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
)

const maxSSETimingBytes = 16 * 1024 * 1024

// sseTimingObserver observes original parser input without altering forwarded
// lines. It is shared by Responses and Messages and retains at most one event.
type sseTimingObserver struct {
	ctx             context.Context
	protocol        string
	timingEventType string
	timingData      []byte
	timingDiscard   bool
}

func (s *sseTimingObserver) resetTimingEvent() {
	if s.protocol == "messages" && len(s.timingData) > 0 {
		requesttiming.Stage(s.ctx, "typed_content_truncated")
	}
	s.timingEventType = ""
	s.timingData = nil
	s.timingDiscard = false
}

func (s *sseTimingObserver) observeTimingLine(line string) {
	if s.ctx == nil || requesttiming.From(s.ctx) == nil {
		return
	}
	if line == "" || line == "event" {
		s.resetTimingEvent()
		return
	}
	if eventType, ok := extractOpenAISSEEventLine(line); ok {
		s.resetTimingEvent()
		s.timingEventType = eventType
		return
	}
	data, ok := extractOpenAISSEDataLine(line)
	if !ok || s.timingDiscard {
		return
	}
	if s.protocol == "messages" && strings.TrimSpace(data) == "[DONE]" {
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
	requesttiming.Stage(s.ctx, "sse_parsed_first")
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
