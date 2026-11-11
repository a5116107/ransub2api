package requesttiming

import (
	"bytes"
	"context"
	"encoding/json"
)

// ChatContent observes original Chat Completions wire data, before billing
// normalization or conversion into Responses/Messages. Only primary-choice
// text deltas contribute chunks; buffered JSON never invents a text stream.
func ChatContent(ctx context.Context, data []byte, streaming bool) {
	if From(ctx) == nil {
		return
	}
	Stage(ctx, "typed_chat_completions_content")
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return // A transport sentinel does not prove a completed primary choice.
	}
	if len(data) > 16*1024*1024 {
		Stage(ctx, "typed_content_truncated")
		return
	}
	event, ok := cacheObject(data)
	if !ok {
		Stage(ctx, "typed_content_truncated")
		return
	}
	for key, expected := range map[string]string{"object": "chat.completion", "type": ""} {
		if raw, present := event[key]; present {
			var value string
			if key == "object" && streaming {
				expected = "chat.completion.chunk"
			}
			if json.Unmarshal(raw, &value) != nil || value != expected {
				Stage(ctx, "typed_content_truncated")
				return
			}
		}
	}
	if raw := event["error"]; len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		recordContent(ctx, "chat_completions", false, false, false, false, "failed", nil, nil)
		return
	}
	var choices []json.RawMessage
	if raw, present := event["choices"]; !present || json.Unmarshal(raw, &choices) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		Stage(ctx, "typed_content_truncated")
		return
	}
	if len(choices) > 4096 {
		Stage(ctx, "typed_content_truncated")
		return
	}
	var primary map[string]json.RawMessage
	for _, raw := range choices {
		choice, valid := cacheObject(raw)
		index := 0
		if v, present := choice["index"]; present {
			valid = valid && !bytes.Equal(bytes.TrimSpace(v), []byte("null")) && json.Unmarshal(v, &index) == nil && index >= 0
		} else if len(choices) > 1 {
			valid = false
		}
		if !valid || (index == 0 && primary != nil) {
			Stage(ctx, "typed_content_truncated")
			return
		}
		if index == 0 {
			primary = choice
		} else {
			Stage(ctx, "typed_output_multiple_candidates")
		}
	}
	reasoning, visible, nonText := false, false, false
	terminal := ""
	if primary != nil {
		contentKey, otherKey := "message", "delta"
		if streaming {
			contentKey, otherKey = otherKey, contentKey
		}
		if _, present := primary[otherKey]; present {
			Stage(ctx, "typed_content_truncated")
			return
		}
		if raw, present := primary[contentKey]; present {
			content, valid := cacheObject(raw)
			if !valid {
				Stage(ctx, "typed_content_truncated")
				return
			}
			for _, key := range []string{"content", "reasoning_content", "reasoning"} {
				if value, present := content[key]; present && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
					var text string
					if json.Unmarshal(value, &text) != nil {
						Stage(ctx, "typed_content_truncated")
						return
					}
					if key == "content" {
						visible = text != ""
					} else {
						reasoning = reasoning || text != ""
					}
				}
			}
			for _, key := range []string{"tool_calls", "function_call", "audio", "refusal", "images"} {
				if raw := content[key]; len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) &&
					!bytes.Equal(bytes.TrimSpace(raw), []byte(`""`)) && !bytes.Equal(bytes.TrimSpace(raw), []byte("[]")) {
					nonText = true
				}
			}
		}
		if raw := primary["finish_reason"]; len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			var finish string
			if json.Unmarshal(raw, &finish) != nil {
				Stage(ctx, "typed_content_truncated")
				return
			}
			switch finish {
			case "":
			case "stop", "tool_calls", "function_call":
				terminal = "completed"
			default:
				terminal = "incomplete"
			}
		}
	}
	recordContent(ctx, "chat_completions", reasoning, visible, streaming && visible, nonText, terminal,
		readCacheUsage(event["usage"], "chat_completions"), readOutputUsage(event["usage"], "chat_completions"))
}
