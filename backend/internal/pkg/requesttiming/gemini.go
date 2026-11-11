package requesttiming

import (
	"context"
	"encoding/json"
	"strings"
)

// GeminiContent observes a single original Gemini response chunk or JSON
// response. Code Assist envelopes are unwrapped here; converted downstream
// events and aggregated responses must not be observed a second time.
// Text chunks are protocol events, never token counts or network packets.
func GeminiContent(ctx context.Context, data []byte, streaming bool) {
	if From(ctx) == nil {
		return
	}
	Stage(ctx, "typed_gemini_content")
	type geminiEvent struct {
		Response   json.RawMessage `json:"response"`
		Error      json.RawMessage `json:"error"`
		Type       string          `json:"type"`
		Candidates []struct {
			Index   *int `json:"index"`
			Content struct {
				Parts []map[string]json.RawMessage `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		PromptFeedback struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback"`
	}
	var event geminiEvent
	if len(data) > 16*1024*1024 || json.Unmarshal(data, &event) != nil {
		Stage(ctx, "typed_content_truncated")
		return
	}
	if len(event.Response) > 0 && string(event.Response) != "null" {
		// Only one envelope layer is part of this wire protocol. Avoid
		// recursion and do not let an inner response hide an outer error.
		outerError := event.Error
		inner := event.Response
		event = geminiEvent{}
		if json.Unmarshal(inner, &event) != nil || len(event.Response) > 0 {
			Stage(ctx, "typed_content_truncated")
			return
		}
		if len(outerError) > 0 && string(outerError) != "null" {
			event.Error = outerError
		}
	}
	if event.Type != "" {
		Stage(ctx, "typed_content_truncated")
		return
	}
	reasoning, visible, nonText := false, false, false
	terminal := ""
	for _, candidate := range event.Candidates {
		if candidate.Index != nil && *candidate.Index != 0 {
			continue
		}
		for _, part := range candidate.Content.Parts {
			var text string
			var thought bool
			if (len(part["text"]) > 0 && json.Unmarshal(part["text"], &text) != nil) ||
				(len(part["thought"]) > 0 && json.Unmarshal(part["thought"], &thought) != nil) {
				Stage(ctx, "typed_content_truncated")
				return
			}
			reasoning = reasoning || (thought && text != "")
			visible = visible || (!thought && text != "")
			for name := range part {
				if name != "text" && name != "thought" && name != "thoughtSignature" {
					nonText = true
				}
			}
		}
		switch strings.ToUpper(strings.TrimSpace(candidate.FinishReason)) {
		case "":
		case "STOP":
			terminal = "completed"
		default:
			terminal = "incomplete"
		}
		break // Non-primary candidates never complete or time the primary output.
	}
	switch strings.ToUpper(strings.TrimSpace(event.PromptFeedback.BlockReason)) {
	case "", "BLOCKED_REASON_UNSPECIFIED":
	default:
		terminal = "incomplete"
	}
	if len(event.Error) > 0 && string(event.Error) != "null" {
		terminal = "failed"
	}
	recordContent(ctx, "gemini", reasoning, visible, streaming && visible, nonText, terminal, nil, nil)
}
