package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// Scope the final Google wire request, including Code Assist/Antigravity
// envelopes. A text observation never attests to tools, media or cached objects.
func availabilityGeminiScope(request *http.Request, payload []byte) *AvailabilityRequestScope {
	if request.Method != http.MethodPost || request.URL.RawPath != "" {
		return nil
	}
	path, action, ok := strings.Cut(request.URL.Path, ":")
	if !ok || action != "generateContent" && action != "streamGenerateContent" {
		return nil
	}
	stream := action == "streamGenerateContent"
	if stream && request.URL.RawQuery != "alt=sse" || !stream && request.URL.RawQuery != "" {
		return nil
	}
	fields, valid := availabilityJSONObject(gjson.ParseBytes(payload))
	if !valid {
		return nil
	}
	options := map[string]any{"path": path, "query": request.URL.RawQuery}
	scope := &AvailabilityRequestScope{Version: availabilityRequestScopeVersion, Protocol: "gemini_http", Stream: stream, Capability: "text", ServiceTier: "unspecified", ReasoningEffort: "unspecified"}
	if strings.HasSuffix(path, "/v1internal") {
		scope.Protocol = "gemini_internal_http"
		if fields["model"].Type != gjson.String || fields["project"].Type != gjson.String || fields["project"].String() == "" {
			return nil
		}
		scope.Model = fields["model"].String()
		for name, value := range fields {
			switch name {
			case "model", "request":
			case "project", "userAgent", "requestType":
				if value.Type != gjson.String || len(value.String()) > 4096 {
					return nil
				}
				options[name] = value.String()
			case "requestId":
				if value.Type != gjson.String {
					return nil
				}
			default:
				return nil
			}
		}
		fields, valid = availabilityJSONObject(fields["request"])
		if !valid {
			return nil
		}
	} else {
		prefix, model, ok := strings.Cut(path, "/models/")
		if !ok || strings.Contains(model, "/") {
			return nil
		}
		scope.Model, options["path"] = model, prefix+"/models/"
	}
	if scope.Model == "" || len(scope.Model) > 200 {
		return nil
	}
	for _, r := range scope.Model {
		if r <= ' ' || r > '~' {
			return nil
		}
	}
	if !fields["contents"].IsArray() || len(fields["contents"].Array()) == 0 {
		return nil
	}
	for _, content := range fields["contents"].Array() {
		if !availabilityGeminiTextContent(content, false) {
			return nil
		}
	}
	for name, value := range fields {
		switch name {
		case "contents":
		case "systemInstruction":
			if !availabilityGeminiTextContent(value, true) {
				return nil
			}
		case "sessionId":
			if value.Type != gjson.String {
				return nil
			}
		case "tools":
			if !value.IsArray() || len(value.Array()) != 0 {
				return nil
			}
		case "toolConfig":
			config, ok := availabilityJSONObject(value)
			calling, valid := availabilityJSONObject(config["functionCallingConfig"])
			mode, known := availabilityOption(calling["mode"], "AUTO", "NONE", "VALIDATED")
			if !ok || len(config) != 1 || !valid || len(calling) != 1 || !known || mode == "unspecified" {
				return nil
			}
			options[name] = value.Value()
		case "generationConfig":
			config, ok := availabilityJSONObject(value)
			if !ok {
				return nil
			}
			for option, setting := range config {
				switch option {
				case "temperature", "topP", "topK", "maxOutputTokens", "seed", "stopSequences", "presencePenalty", "frequencyPenalty":
				case "candidateCount":
					if setting.Type != gjson.Number || setting.Raw != "1" {
						return nil
					}
					options[option] = setting.Value()
				case "responseModalities":
					if !setting.IsArray() || len(setting.Array()) != 1 || setting.Array()[0].Type != gjson.String || setting.Array()[0].String() != "TEXT" {
						return nil
					}
					options[option] = setting.Value()
				case "responseMimeType":
					if setting.Type != gjson.String || setting.String() != "text/plain" {
						return nil
					}
					options[option] = setting.Value()
				case "thinkingConfig":
					if !availabilityGeminiThinkingScope(setting, scope) {
						return nil
					}
					options[option] = setting.Value()
				default:
					return nil
				}
			}
		default:
			return nil
		}
	}
	for _, name := range []string{"X-Goog-User-Project", "X-Goog-Request-Params", "X-Goog-Api-Version"} {
		var values []string
		for key, value := range request.Header {
			if strings.EqualFold(key, name) {
				values = append(values, value...)
			}
		}
		if len(values) > 1 || len(values) == 1 && len(values[0]) > 4096 {
			return nil
		}
		if len(values) == 1 {
			options[name] = values[0]
		}
	}
	raw, err := json.Marshal(options)
	if err != nil {
		return nil
	}
	sum := sha256.Sum256(raw)
	scope.ProtocolOptions = hex.EncodeToString(sum[:])
	return scope
}

func availabilityGeminiTextContent(value gjson.Result, system bool) bool {
	fields, ok := availabilityJSONObject(value)
	if !ok || !fields["parts"].IsArray() || len(fields["parts"].Array()) == 0 {
		return false
	}
	for name, item := range fields {
		switch name {
		case "parts":
		case "role":
			if item.Type != gjson.String || item.String() != "user" && item.String() != "model" && (!system || item.String() != "system") {
				return false
			}
		default:
			return false
		}
	}
	for _, part := range fields["parts"].Array() {
		text, ok := availabilityJSONObject(part)
		if !ok || len(text) != 1 || text["text"].Type != gjson.String {
			return false
		}
	}
	return true
}

func availabilityGeminiThinkingScope(value gjson.Result, scope *AvailabilityRequestScope) bool {
	fields, ok := availabilityJSONObject(value)
	if !ok {
		return false
	}
	for name, item := range fields {
		switch name {
		case "thinkingLevel":
			level, valid := availabilityOption(item, "minimal", "low", "medium", "high")
			if !valid || level == "unspecified" || fields["thinkingBudget"].Exists() {
				return false
			}
			scope.ThinkingMode, scope.ReasoningEffort = "level", level
		case "thinkingBudget":
			budget, err := strconv.ParseInt(item.Raw, 10, 64)
			if err != nil || budget < -1 || fields["thinkingLevel"].Exists() {
				return false
			}
			scope.ThinkingMode, scope.ThinkingBudget = "budget", budget
		case "includeThoughts":
			if item.Type != gjson.True && item.Type != gjson.False {
				return false
			}
		default:
			return false
		}
	}
	return true
}
