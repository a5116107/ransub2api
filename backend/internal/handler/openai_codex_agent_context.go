package handler

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Codex Desktop injects page and goal context as named agent function_call_output
// items without a corresponding model call. Preserve their original trust boundary
// before ordinary tool-output validation, including when replaying tool history.
// Unrecognized outputs and items carrying a call identity remain untouched.
func normalizeCodexAgentContext(body []byte) ([]byte, bool) {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, false
	}
	hasContext := false
	input.ForEach(func(_, item gjson.Result) bool {
		hasContext = isCodexAgentContext(item)
		return !hasContext
	})
	if !hasContext || !hasUniqueJSONMembers(body) {
		return body, false
	}
	var items []json.RawMessage
	changed := false
	input.ForEach(func(_, item gjson.Result) bool {
		if !isCodexAgentContext(item) {
			items = append(items, json.RawMessage(item.Raw))
			return true
		}
		text := "<external_" + item.Get("name").String() + ">\n" + item.Get("output").String() + "\n</external_" + item.Get("name").String() + ">"
		if item.Get("name").String() == "goal_objective" {
			// An agent-created objective is data, not a new user instruction.
			// JSON quoting also escapes closing tags inside its original text.
			context, err := json.Marshal(map[string]string{"namespace": "agent", "name": "goal_objective", "output": item.Get("output").String()})
			if err != nil {
				items = append(items, json.RawMessage(item.Raw))
				return true
			}
			text = "<untrusted_codex_agent_context>\n" + string(context) + "\n</untrusted_codex_agent_context>"
		}
		message, err := json.Marshal(map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_text",
				"text": text,
			}},
		})
		if err != nil {
			items = append(items, json.RawMessage(item.Raw))
			return true
		}
		items = append(items, message)
		changed = true
		return true
	})
	if !changed {
		return body, false
	}
	// Raw input items and the untouched request fields keep their exact numbers.
	normalized, err := sjson.SetBytes(body, "input", items)
	if err != nil {
		return body, false
	}
	return normalized, true
}

func isCodexAgentContext(item gjson.Result) bool {
	if item.Get("type").String() != "function_call_output" || item.Get("namespace").String() != "agent" {
		return false
	}
	for _, field := range []string{"call_id", "tool_call_id"} {
		id := item.Get(field)
		if id.Exists() && (id.Type != gjson.String || strings.TrimSpace(id.String()) != "") {
			return false
		}
	}
	output := item.Get("output")
	if output.Type != gjson.String {
		return false
	}
	name := item.Get("name").String()
	if name == "goal_objective" {
		valid := strings.TrimSpace(output.String()) != ""
		item.ForEach(func(key, _ gjson.Result) bool {
			switch key.String() {
			case "type", "namespace", "name", "output", "id", "call_id", "tool_call_id", "internal_chat_message_metadata_passthrough":
			default:
				valid = false
			}
			return valid
		})
		return valid
	}
	if name == "codex_apps_open_page" {
		return validCodexPageSnapshot(output.String())
	}
	const prefix = "codex_apps_selected_page_part_"
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	part, total, ok := strings.Cut(strings.TrimPrefix(name, prefix), "_of_")
	if !ok {
		return false
	}
	partNumber, partErr := strconv.Atoi(part)
	totalNumber, totalErr := strconv.Atoi(total)
	if partErr != nil || totalErr != nil || partNumber < 1 || partNumber > totalNumber ||
		strconv.Itoa(partNumber) != part || strconv.Itoa(totalNumber) != total {
		return false
	}
	// Snapshot parts may split a JSON document, so validate the matching envelope
	// rather than requiring each part to be independently valid JSON.
	header, snapshot, hasNewline := strings.Cut(output.String(), "\n")
	wantHeader := "Companion Page snapshot part " + part + " of " + total + ":"
	return hasNewline && strings.TrimSuffix(header, "\r") == wantHeader && strings.TrimSpace(snapshot) != "" &&
		(totalNumber > 1 || validCodexPageSnapshot(snapshot))
}

func validCodexPageSnapshot(text string) bool {
	if !hasUniqueJSONMembers([]byte(text)) {
		return false
	}
	var snapshot map[string]json.RawMessage
	if json.Unmarshal([]byte(text), &snapshot) != nil {
		return false
	}
	id, exists := snapshot["page_id"]
	if !exists {
		return false
	}
	if bytes.Equal(bytes.TrimSpace(id), []byte("null")) {
		return true
	}
	var value string
	return json.Unmarshal(id, &value) == nil && strings.TrimSpace(value) != ""
}
