package service

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// Native and Antigravity answer tests require a complete Gemini answer. The
// manual connection probe can use a one-token budget; that cutoff never proves
// answer availability, and an SSE sentinel or thought text never completes it.
func readGeminiTestStream(ctx context.Context, body io.Reader, observe func(string), emit func(TestEvent)) error {
	_, answerTest := pelicanTestOptionsFromContext(ctx)
	requireComplete := answerTest || ctx.Value(availabilityProbeSnapshotKey{}) != nil
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	var data strings.Builder
	output := false
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			if data.Len() > 0 {
				if err := data.WriteByte('\n'); err != nil {
					return err
				}
			}
			if _, err := data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")); err != nil {
				return err
			}
			if data.Len() > 4<<20 {
				return errors.New("gemini SSE event exceeds 4 MiB capture limit")
			}
			continue
		}
		if line != "" || data.Len() == 0 {
			continue
		}
		raw := data.String()
		data.Reset()
		if observe != nil {
			observe(raw)
		}
		if !gjson.Valid(raw) {
			return errors.New("gemini stream ended without a valid completion event")
		}
		fields, ok := availabilityJSONObject(gjson.Parse(raw))
		if !ok {
			return errors.New("invalid Gemini response object")
		}
		if err := geminiTestResponseError(ctx, fields); err != nil {
			return err
		}
		if response, exists := fields["response"]; exists {
			if fields["candidates"].Exists() {
				return errors.New("conflicting Gemini response envelopes")
			}
			fields, ok = availabilityJSONObject(response)
			if !ok {
				return errors.New("invalid Gemini response envelope")
			}
			if err := geminiTestResponseError(ctx, fields); err != nil {
				return err
			}
		}
		if !fields["candidates"].Exists() {
			continue // Usage and metadata events do not complete an answer.
		}
		candidates := fields["candidates"]
		if !candidates.IsArray() || len(candidates.Array()) != 1 {
			return errors.New("unexpected Gemini response candidates")
		}
		candidate, ok := availabilityJSONObject(candidates.Array()[0])
		if !ok || candidate["index"].Exists() && candidate["index"].Raw != "0" {
			return errors.New("invalid Gemini response candidate")
		}
		if content := candidate["content"]; content.Exists() {
			contentFields, ok := availabilityJSONObject(content)
			if !ok || !contentFields["parts"].IsArray() {
				return errors.New("invalid Gemini response content")
			}
			for _, part := range contentFields["parts"].Array() {
				partFields, ok := availabilityJSONObject(part)
				if !ok {
					return errors.New("invalid Gemini response part")
				}
				if thought := partFields["thought"]; thought.Exists() {
					if thought.Type != gjson.True && thought.Type != gjson.False {
						return errors.New("invalid Gemini thought marker")
					}
					if thought.Bool() {
						continue
					}
				}
				if partFields["functionCall"].Exists() || partFields["functionResponse"].Exists() {
					return errors.New("gemini returned a tool operation instead of an answer")
				}
				if text := partFields["text"]; text.Exists() {
					if text.Type != gjson.String {
						return errors.New("invalid Gemini response text")
					}
					if text.String() != "" {
						output = output || strings.TrimSpace(text.String()) != ""
						accountTestContentContext(ctx, text.String())
						emit(TestEvent{Type: "content", Text: text.String()})
					}
				}
				if inline := partFields["inlineData"]; inline.Exists() {
					mime, data := inline.Get("mimeType"), inline.Get("data")
					if !inline.IsObject() || mime.Type != gjson.String || data.Type != gjson.String || !strings.HasPrefix(strings.ToLower(mime.String()), "image/") || data.String() == "" {
						return errors.New("invalid Gemini inline image")
					}
					// An image reply must not publish a text-only observation.
					if observation, ok := ctx.Value(availabilityProbeSnapshotKey{}).(*availabilityProbeObservation); ok {
						observation.scope = nil
					}
					output = true
					emit(TestEvent{Type: "image", MimeType: mime.String(), ImageURL: "data:" + mime.String() + ";base64," + data.String()})
				}
			}
		}
		if finish := candidate["finishReason"]; finish.Exists() {
			if finish.Type != gjson.String {
				return errors.New("invalid Gemini completion reason")
			}
			switch finish.String() {
			case "STOP":
				if !output {
					return errors.New(pelicanErrEmptyOutput)
				}
				return ctx.Err()
			case "MAX_TOKENS":
				if !requireComplete && output {
					return ctx.Err()
				}
				return errors.New(pelicanErrMaxTokens)
			case "":
			default:
				return fmt.Errorf("gemini answer did not finish: %s", finish.String())
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("gemini stream read error: %w", err)
	}
	return errors.New("stream ended before Gemini completion")
}

func geminiTestResponseError(ctx context.Context, fields map[string]gjson.Result) error {
	if failure, exists := fields["error"]; exists && failure.Type != gjson.Null {
		message := failure.Get("message").String()
		if message == "" {
			message = "Gemini upstream error"
		}
		// Google places its HTTP-equivalent status inside an HTTP 200 SSE
		// error event. Retain it independently from model grading.
		if observation, ok := ctx.Value(availabilityProbeSnapshotKey{}).(*availabilityProbeObservation); ok {
			observation.semanticStatus = 502
			code := failure.Get("code")
			if status, err := strconv.Atoi(code.Raw); code.Type == gjson.Number && err == nil && status >= 300 && status <= 599 {
				observation.semanticStatus = status
			}
		}
		return errors.New(message)
	}
	if fields["promptFeedback"].Get("blockReason").String() != "" {
		return errors.New("gemini blocked the probe prompt")
	}
	return nil
}
