package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMonitoringProbeBudgetTokenLimitsValidation(t *testing.T) {
	for _, field := range []string{"tokens_per_minute", "tokens_per_day", "site_tokens_per_minute", "key_tokens_per_minute"} {
		for _, value := range []int64{-1, 0, 1, 1000000000000, 1000000000001} {
			settings := DefaultMonitoringProbeBudgetSettings()
			raw, err := json.Marshal(map[string]int64{field: value})
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &settings))
			if value < 0 || value > 1000000000000 {
				require.Error(t, settings.Validate(), "%s=%d", field, value)
			} else {
				require.NoError(t, settings.Validate(), "%s=%d", field, value)
			}
		}
	}
}

func TestMonitoringProbeBudgetActualUsageFormats(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		stream     bool
		tokens     int64
		reported   bool
	}{
		{"chat_cached_included", `{"usage":{"prompt_tokens":8,"completion_tokens":2,"total_tokens":10,"prompt_tokens_details":{"cached_tokens":6}}}`, false, 10, true},
		{"responses", `{"type":"response.completed","response":{"usage":{"input_tokens":8,"output_tokens":2,"total_tokens":10}}}`, false, 10, true},
		{"anthropic_cached_additive", `{"type":"message","stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2,"cache_read_input_tokens":5,"cache_creation_input_tokens":4}}`, false, 14, true},
		{"gemini_thoughts_in_total", `{"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"thoughtsTokenCount":5,"totalTokenCount":10}}`, false, 10, true},
		{"gemini_envelope", `{"response":{"usageMetadata":{"totalTokenCount":10}}}`, false, 10, true},
		{"gemini_partial", `{"usageMetadata":{"promptTokenCount":5}}`, false, 5, false},
		{"gemini_array", `[{"usageMetadata":{"totalTokenCount":3}},{"usageMetadata":{"totalTokenCount":10}}]`, false, 10, true},
		{"explicit_zero", `{"usage":{"input_tokens":0,"output_tokens":0}}`, false, 0, true},
		{"missing", `{"output":"hello"}`, false, 0, false},
		{"null", `{"usage":null}`, false, 0, false},
		{"partial", `{"usage":{"input_tokens":5}}`, false, 5, false},
		{"inconsistent_total", `{"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":3}}`, false, 7, false},
		{"negative", `{"usage":{"total_tokens":-1}}`, false, 0, false},
		{"string", `{"usage":{"total_tokens":"5"}}`, false, 0, false},
		{"fraction", `{"usage":{"total_tokens":5.5}}`, false, 0, false},
		{"exponent", `{"usage":{"total_tokens":5e2}}`, false, 0, false},
		{"overflow", `{"usage":{"total_tokens":9223372036854775808}}`, false, 0, false},
		{"unsafe_integer", `{"usage":{"total_tokens":9007199254740992}}`, false, 0, false},
		{"duplicate_counter", `{"usage":{"total_tokens":5,"total_tokens":7}}`, false, 0, false},
		{"duplicate_usage", `{"usage":{"total_tokens":5},"usage":{"total_tokens":7}}`, false, 0, false},
		{"ambiguous_alias", `{"usage":{"input_tokens":5,"prompt_tokens":5,"output_tokens":2}}`, false, 0, false},
		{"trailing_document", `{"usage":{"total_tokens":5}}{}`, false, 0, false},
		{"chat_sse_usage_tail", "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":2}}\r\n\r\ndata: [DONE]\r\n\r\n", true, 10, true},
		{"responses_sse_duplicate", "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":8,\"output_tokens\":2}}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":8,\"output_tokens\":2}}}\n\n", true, 10, true},
		{"messages_split", "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":0,\"cache_read_input_tokens\":5,\"cache_creation_input_tokens\":4}}}\n\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2}}\n\ndata: {\"type\":\"message_stop\"}\n\n", true, 14, true},
		{"messages_truncated", "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n", true, 3, false},
		{"incomplete_frame", "data: {\"usage\":{\"total_tokens\":5}}", true, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, chunk := range []int{1, 7, len(tc.body)} {
				meter := &monitoringProbeTokenCollector{}
				meter.setStream(tc.stream)
				for start := 0; start < len(tc.body); start += chunk {
					end := start + chunk
					if end > len(tc.body) {
						end = len(tc.body)
					}
					meter.feed([]byte(tc.body[start:end]), false)
				}
				meter.feed(nil, true)
				require.Equal(t, MonitoringProbeTokenUsage{Tokens: tc.tokens, Reported: tc.reported}, meter.snapshot(), "chunk=%d", chunk)
			}
		})
	}
}

func TestMonitoringProbeBudgetActualUsageLifecycle(t *testing.T) {
	for _, mode := range []string{"json", "decoder_close", "partial_close", "management", "ws", "handshake", "settlement_failure", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			repo := &monitoringBudgetRepoStub{}
			if mode == "settlement_failure" {
				repo.releaseErr = errors.New("fixture SQL failed")
			}
			original := monitoringBudgetRequest(t, repo)
			original.Method, original.URL.Path = http.MethodPost, "/tenant/v1/responses"
			if mode == "management" {
				original.URL.Path = "/tenant/api/status"
			}
			var ctx context.Context
			var finish func()
			var err error
			if mode == "ws" || mode == "handshake" {
				ctx, finish, err = beginMonitoringProbeWSRequest(original.Context(), "wss://example.com/tenant/responses", []string{"fixture-key"}, mode == "ws")
			} else {
				var req *http.Request
				req, finish, err = BeginMonitoringProbeRequest(original)
				ctx = req.Context()
			}
			require.NoError(t, err)
			payload := `{"type":"response.completed","response":{"usage":{"input_tokens":4,"output_tokens":3}}}`
			if mode == "ws" || mode == "handshake" {
				observeMonitoringProbeWSUsage(ctx, []byte(payload))
				require.NoError(t, finishMonitoringProbeWSRequest(ctx, finish))
			} else {
				if mode == "oversized" {
					payload = strings.Repeat(" ", monitoringProbeUsageMaxBytes+1) + payload
				}
				response := &http.Response{Body: io.NopCloser(strings.NewReader(payload))}
				require.NoError(t, FinishMonitoringProbeResponse(ctx, response, nil, finish))
				if mode == "decoder_close" {
					var document map[string]any
					err = json.NewDecoder(response.Body).Decode(&document)
				} else if mode == "partial_close" {
					_, err = response.Body.Read(make([]byte, 5))
				} else {
					_, err = io.ReadAll(response.Body)
				}
				require.NoError(t, err)
				if mode == "settlement_failure" {
					require.ErrorIs(t, response.Body.Close(), ErrMonitoringProbeBudgetUnavailable)
					require.ErrorIs(t, monitoringProbeLocalError(original.Context()), ErrMonitoringProbeBudgetUnavailable)
				} else {
					require.NoError(t, response.Body.Close())
				}
			}
			finish()
			require.Equal(t, 1, repo.released)
			require.Len(t, repo.usages, 1)
			expected := MonitoringProbeTokenUsage{Tokens: 7, Reported: true}
			if mode == "partial_close" || mode == "management" || mode == "handshake" || mode == "oversized" {
				expected = MonitoringProbeTokenUsage{}
			}
			require.Equal(t, expected, repo.usages[0])
			require.NoError(t, original.Context().Err())
		})
	}
}

func TestMonitoringProbeBudgetNativeGenerationPaths(t *testing.T) {
	for _, tc := range []struct {
		name, path, method string
		generation         bool
	}{
		{"vertex_stream", "/tenant/v1/projects/project/locations/global/publishers/anthropic/models/claude:streamRawPredict", http.MethodPost, true},
		{"vertex_json", "/tenant/v1/projects/project/locations/global/publishers/anthropic/models/claude:rawPredict", http.MethodPost, true},
		{"bedrock_json", "/tenant/model/anthropic.claude:0/invoke", http.MethodPost, true},
		{"bedrock_stream", "/tenant/model/anthropic.claude:0/invoke-with-response-stream", http.MethodPost, true},
		{"bedrock_arn", "/tenant/model/arn:aws:bedrock:us-east-1:123456789012:inference-profile/fixture/invoke", http.MethodPost, true},
		{"jev", "/tenant/v1/systemone", http.MethodPost, true},
		{"vertex_count", "/tenant/v1/projects/project/locations/global/publishers/anthropic/models/claude:countTokens", http.MethodPost, false},
		{"bedrock_count", "/tenant/model/anthropic.claude:0/count-tokens", http.MethodPost, false},
		{"inventory", "/tenant/v1/models", http.MethodGet, false},
		{"management_invoke", "/tenant/api/invoke", http.MethodPost, false},
		{"generation_get", "/tenant/model/anthropic.claude:0/invoke", http.MethodGet, false},
		{"jev_get", "/tenant/v1/systemone", http.MethodGet, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &monitoringBudgetRepoStub{}
			original := monitoringBudgetRequest(t, repo)
			original.Method, original.URL.Path = tc.method, tc.path
			req, finish, err := BeginMonitoringProbeRequest(original)
			require.NoError(t, err)
			t.Cleanup(finish)
			require.Len(t, repo.requests, 1)
			require.Equal(t, tc.generation, repo.requests[0].MeterTokens)
			response := &http.Response{Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"type":"message","stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2,"cache_read_input_tokens":5,"cache_creation_input_tokens":4}}`))}
			require.NoError(t, FinishMonitoringProbeResponse(req.Context(), response, nil, finish))
			_, err = io.Copy(io.Discard, response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			require.Len(t, repo.usages, 1)
			want := MonitoringProbeTokenUsage{}
			if tc.generation {
				want = MonitoringProbeTokenUsage{Tokens: 14, Reported: true}
			}
			require.Equal(t, want, repo.usages[0])
		})
	}
}

func TestMonitoringProbeBudgetImageJSONFiltering(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       MonitoringProbeTokenUsage
	}{
		{"escaped_key", `{"data":[{"b64_\u006ason":"image"}],"usage":{"total_tokens":7}}`, MonitoringProbeTokenUsage{Tokens: 7, Reported: true}},
		{"string_escapes", `{"data":[{"b64_json":"\u0061\/\\\\\""}],"usage":{"total_tokens":7}}`, MonitoringProbeTokenUsage{Tokens: 7, Reported: true}},
		{"whitespace", "{\"data\":[{\"b64_json\" \t:\r\n\"image\"}],\"usage\":{\"total_tokens\":7}}", MonitoringProbeTokenUsage{Tokens: 7, Reported: true}},
		{"array_string_is_not_key", `{"data":["b64_json","anything"],"usage":{"total_tokens":7}}`, MonitoringProbeTokenUsage{Tokens: 7, Reported: true}},
		{"usage_string_is_invalid", `{"b64_json":"image","usage":{"total_tokens":"7"}}`, MonitoringProbeTokenUsage{}},
		{"fake_embedded_usage", `{"b64_json":"\"usage\":{\"total_tokens\":7}"}`, MonitoringProbeTokenUsage{}},
		{"invalid_escape", `{"b64_json":"image\q","usage":{"total_tokens":7}}`, MonitoringProbeTokenUsage{}},
		{"invalid_unicode", `{"b64_json":"image\u00xz","usage":{"total_tokens":7}}`, MonitoringProbeTokenUsage{}},
		{"truncated_unicode", `{"b64_json":"image\u0","usage":{"total_tokens":7}}`, MonitoringProbeTokenUsage{}},
		{"control_byte", "{\"b64_json\":\"image\n\",\"usage\":{\"total_tokens\":7}}", MonitoringProbeTokenUsage{}},
		{"truncated_value", `{"usage":{"total_tokens":7},"b64_json":"image`, MonitoringProbeTokenUsage{}},
		{"trailing_document", `{"b64_json":"image","usage":{"total_tokens":7}}{}`, MonitoringProbeTokenUsage{}},
		{"bounded_metadata", `{"extra":"` + strings.Repeat("x", monitoringProbeUsageMaxBytes) + `","b64_json":"image","usage":{"total_tokens":7}}`, MonitoringProbeTokenUsage{}},
		{"bounded_escapes", `{"b64_json":"` + strings.Repeat(`\u0061`, monitoringProbeUsageMaxBytes/6+1) + `","usage":{"total_tokens":7}}`, MonitoringProbeTokenUsage{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, size := range []int{1, 7, len(tc.body)} {
				meter := &monitoringProbeTokenCollector{imageJSON: &monitoringProbeImageJSONFilter{}}
				peak := 0
				for start := 0; start < len(tc.body); start += size {
					end := start + size
					if end > len(tc.body) {
						end = len(tc.body)
					}
					meter.feed([]byte(tc.body[start:end]), false)
					if len(meter.buffer) > peak {
						peak = len(meter.buffer)
					}
				}
				require.LessOrEqual(t, peak, monitoringProbeUsageMaxBytes)
				meter.feed(nil, true)
				require.Equal(t, tc.want, meter.snapshot(), "chunk=%d", size)
			}
		})
	}
}

func TestMonitoringProbeBudgetImageJSONGrammar(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	alphabet := []byte("abc012{}[],:\"\\\r\n\t\x00\x1f\xff")
	for n := 0; n < 2048; n++ {
		content := make([]byte, rng.IntN(80))
		for i := range content {
			content[i] = alphabet[rng.IntN(len(alphabet))]
		}
		body := []byte(`{"data":[{"b64_json":"` + string(content) + `"}],"usage":{"total_tokens":7}}`)
		for _, size := range []int{1, 7, len(body)} {
			filter := &monitoringProbeImageJSONFilter{}
			var compact []byte
			for start := 0; start < len(body); start += size {
				end := start + size
				if end > len(body) {
					end = len(body)
				}
				var overflow bool
				compact, overflow = filter.append(compact, body[start:end])
				require.False(t, overflow)
			}
			require.Equal(t, json.Valid(body), json.Valid(compact), "case=%d chunk=%d raw=%q filtered=%q", n, size, body, compact)
		}
	}
}
