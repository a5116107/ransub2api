package requesttiming

import (
	"context"
	"encoding/json"
	"math"
	"strings"
)

const maxTextStreamChunks = 1_000_000

// TextStreamTiming is a constant-size aggregate of parsed Responses text
// deltas, not tokens or network packets. No generated content is retained.
type TextStreamTiming struct {
	Chunks        int     `json:"chunks"`
	FirstMS       float64 `json:"first_ms"`
	LastMS        float64 `json:"last_ms"`
	MaxGapMS      float64 `json:"max_gap_ms"`
	Terminal      string  `json:"terminal,omitempty"`
	TerminalMS    float64 `json:"terminal_ms,omitempty"`
	Truncated     bool    `json:"truncated,omitempty"`
	Invalid       bool    `json:"invalid,omitempty"`
	NonTextOutput bool    `json:"non_text_output,omitempty"`
}

type responseContentItem struct {
	Type    string `json:"type"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func (item responseContentItem) visibleText() bool {
	if item.Type == "message" {
		for _, part := range item.Content {
			if part.Type == "output_text" && part.Text != "" {
				return true
			}
		}
	}
	return false
}

// Content records typed protocol observations without retaining generated text.
// The legacy first_semantic/first_visible meanings remain unchanged.
func Content(ctx context.Context, data []byte, eventTypes ...string) {
	c := From(ctx)
	if c == nil {
		return
	}
	var event struct {
		Type     string              `json:"type"`
		Delta    string              `json:"delta"`
		Text     string              `json:"text"`
		Item     responseContentItem `json:"item"`
		Response struct {
			Output []responseContentItem `json:"output"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &event) != nil {
		return
	}
	if event.Type == "" && len(eventTypes) > 0 {
		event.Type = strings.TrimSpace(eventTypes[0])
	}
	reasoning := false
	switch event.Type {
	case "response.output_item.added", "response.output_item.done":
		reasoning = event.Item.Type == "reasoning"
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		reasoning = event.Delta != ""
	case "response.reasoning_text.done", "response.reasoning_summary_text.done":
		reasoning = event.Text != ""
	case "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		reasoning = true
	}
	textDelta := event.Type == "response.output_text.delta" && event.Delta != ""
	nonTextOutput := ((event.Type == "response.output_item.added" || event.Type == "response.output_item.done") &&
		event.Item.Type != "message" && event.Item.Type != "reasoning") ||
		(strings.HasPrefix(event.Type, "response.") && strings.HasSuffix(event.Type, ".delta") &&
			event.Type != "response.output_text.delta" && event.Type != "response.reasoning_text.delta" && event.Type != "response.reasoning_summary_text.delta")
	visibleText := textDelta || (event.Type == "response.output_text.done" && event.Text != "")
	if (event.Type == "response.output_item.added" || event.Type == "response.output_item.done") && event.Item.Type == "message" {
		visibleText = event.Item.visibleText()
	}
	terminal := ""
	switch event.Type {
	case "response.completed", "response.done":
		terminal = "completed"
		for _, item := range event.Response.Output {
			reasoning = reasoning || item.Type == "reasoning"
			visibleText = visibleText || item.visibleText()
		}
	case "response.failed":
		terminal = "failed"
	case "response.incomplete":
		terminal = "incomplete"
	}
	tr, _ := ctx.Value(attemptKey{}).(*Trace)
	if tr == nil || tr.c != c || (!reasoning && !visibleText && !nonTextOutput && terminal == "") {
		return
	}
	var cache *CacheUsage
	var output *OutputUsage
	if terminal != "" {
		cache, output = readTerminalUsage(data)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished {
		return
	}
	if stream := c.data.Attempts[tr.index].TextStream; stream != nil && stream.Terminal != "" {
		return
	}
	now := c.offset(c.now())
	if a := &c.data.Attempts[tr.index]; a.Kind == "http" && terminal != "" {
		a.CacheUsage = cache
		a.OutputUsage = output
	}
	for index := tr.index; index >= 0; {
		a := &c.data.Attempts[index]
		if reasoning {
			if _, exists := a.Events["first_reasoning"]; !exists {
				a.Events["first_reasoning"] = now
			}
		}
		if visibleText {
			if _, exists := a.Events["first_visible_text"]; !exists {
				a.Events["first_visible_text"] = now
			}
		}
		// Parents retain first-event diagnostics, but never a second copy of
		// physical content counts. Each parser invokes Content once per event.
		if a.Kind == "http" && (textDelta || nonTextOutput || terminal != "") {
			if a.TextStream == nil {
				a.TextStream = &TextStreamTiming{}
			}
			stream := a.TextStream
			if stream.Terminal == "" {
				stream.NonTextOutput = stream.NonTextOutput || nonTextOutput
				if textDelta && !stream.Invalid {
					switch {
					case !finite(now) || now < a.StartMS || (stream.Chunks > 0 && now < stream.LastMS):
						stream.Invalid = true
					case stream.Chunks >= maxTextStreamChunks:
						stream.Truncated = true
					default:
						if stream.Chunks == 0 {
							stream.FirstMS = now
						} else {
							stream.MaxGapMS = math.Max(stream.MaxGapMS, now-stream.LastMS)
						}
						stream.LastMS = now
						stream.Chunks++
					}
				}
				stream.Terminal = terminal
				if terminal != "" {
					stream.TerminalMS = now
				}
			}
		}
		index = a.Parent - 1
	}
}

// TextStreamObservation is parser-side diagnostic evidence. A partial maximum
// is only a lower bound; it is never used as a healthy scheduling sample.
type TextStreamObservation struct {
	Chunks     *int     `json:"chunks"`
	GapSamples *int     `json:"gap_samples"`
	SpanMS     *float64 `json:"span_ms"`
	MaxGapMS   *float64 `json:"max_gap_ms"`
	TailWaitMS *float64 `json:"tail_wait_ms"`
	Status     string   `json:"status"`
	Source     string   `json:"source"`
	Scope      string   `json:"scope"`
}

type SocketEvidence struct {
	MinRTTMS        *float64 `json:"min_rtt_ms"`
	Retransmissions *uint64  `json:"retransmissions"`
	Status          string   `json:"status"`
	Source          string   `json:"source"`
	Scope           string   `json:"scope"`
}
type ContentObservation struct {
	OutputRate                   OutputRateObservation `json:"output_rate"`
	Cache                        CacheObservation      `json:"cache"`
	TextStream                   TextStreamObservation `json:"text_stream"`
	ReasoningFirstMS             *float64              `json:"reasoning_first_ms"`
	VisibleTextFirstMS           *float64              `json:"visible_text_first_ms"`
	ResponseByteFirstMS          *float64              `json:"response_byte_first_ms"`
	Status                       string                `json:"status"`
	Source                       string                `json:"source"`
	Scope                        string                `json:"scope"`
	ReasoningStreamingCapability string                `json:"reasoning_streaming_capability"`
	CapabilitySource             string                `json:"capability_source"`
	ClientSocket                 SocketEvidence        `json:"client_socket"`
	UpstreamSocket               SocketEvidence        `json:"upstream_socket"`
}

func UnknownContentObservation() ContentObservation {
	return ContentObservation{Status: "not_collected", Source: "none", Scope: "final_physical_http_attempt_offsets_from_application_entry", ReasoningStreamingCapability: "unknown", CapabilitySource: "none",
		OutputRate:     unknownOutputRate(),
		Cache:          unknownCacheObservation(),
		TextStream:     TextStreamObservation{Status: "not_collected", Source: "none", Scope: "final_physical_http_responses_text_deltas_at_parser"},
		ClientSocket:   SocketEvidence{Status: "not_collected_or_reverse_proxy_obscured", Source: "none", Scope: "client_to_server; application_direct_peer_may_be_reverse_proxy"},
		UpstreamSocket: SocketEvidence{Status: "not_collected", Source: "none", Scope: "server_to_upstream"}}
}
func DeriveContentObservation(s Snapshot) ContentObservation {
	out := UnknownContentObservation()
	if s.Truncated || len(s.Attempts) == 0 {
		return out
	}
	a := s.Attempts[len(s.Attempts)-1]
	if a.Kind != "http" {
		return out
	}
	out.Status = "partial"
	out.Source = "httptrace_and_typed_sse"
	out.Cache = deriveCacheObservation(s, a)
	if a.ProxyID > 0 {
		out.UpstreamSocket.Status = "proxy_obscured"
		out.UpstreamSocket.Scope = "server_to_configured_proxy; upstream_origin_socket_not_observed"
	}
	_, incompleteCapture := a.Events["typed_content_truncated"]
	if incompleteCapture {
		out.TextStream.Status = "incomplete_capture"
		out.TextStream.Source = "typed_responses_sse"
	}
	read := func(key string) *float64 {
		if key != "first_byte" && incompleteCapture {
			return nil
		}
		v, ok := a.Events[key]
		if a.EndMS == nil || !finite(*a.EndMS) {
			return nil
		}
		upper := *a.EndMS
		if key != "first_byte" && finite(s.TotalMS) && s.TotalMS > upper {
			upper = s.TotalMS
		}
		if !ok || !finite(v) || !finite(a.StartMS) || a.StartMS < 0 || v < a.StartMS || v > upper {
			return nil
		}
		return &v
	}
	out.ReasoningFirstMS = read("first_reasoning")
	out.VisibleTextFirstMS = read("first_visible_text")
	out.ResponseByteFirstMS = read("first_byte")
	if stream := a.TextStream; !incompleteCapture && stream != nil && stream.Chunks > 0 {
		out.TextStream.Source = "typed_responses_sse"
		out.TextStream.Status = "invalid_boundary"
		upper := s.TotalMS
		if a.EndMS != nil && finite(*a.EndMS) {
			upper = math.Max(upper, *a.EndMS)
		}
		span := stream.LastMS - stream.FirstMS
		terminalValid := stream.Terminal == "" && stream.TerminalMS == 0
		if stream.Terminal == "completed" || stream.Terminal == "failed" || stream.Terminal == "incomplete" {
			terminalValid = finite(stream.TerminalMS) && stream.TerminalMS >= stream.LastMS && stream.TerminalMS <= upper
		}
		if !stream.Invalid && stream.Chunks <= maxTextStreamChunks && a.EndMS != nil && finite(*a.EndMS) &&
			finite(upper) && finite(a.StartMS) && a.StartMS >= 0 && *a.EndMS >= a.StartMS &&
			finite(stream.FirstMS) && stream.FirstMS >= a.StartMS &&
			finite(stream.LastMS) && stream.LastMS >= stream.FirstMS && stream.LastMS <= upper &&
			finite(stream.MaxGapMS) && stream.MaxGapMS >= 0 && stream.MaxGapMS <= span && terminalValid &&
			((stream.Chunks == 1 && span == 0 && stream.MaxGapMS == 0) ||
				(stream.Chunks > 1 && stream.MaxGapMS+1e-9 >= span/float64(stream.Chunks-1))) {
			chunks, gaps, maxGap := stream.Chunks, stream.Chunks-1, stream.MaxGapMS
			out.TextStream.Chunks, out.TextStream.GapSamples = &chunks, &gaps
			if gaps > 0 {
				out.TextStream.SpanMS, out.TextStream.MaxGapMS = &span, &maxGap
			}
			if stream.Terminal != "" && !stream.Truncated {
				tail := stream.TerminalMS - stream.LastMS
				out.TextStream.TailWaitMS = &tail
			}
			out.TextStream.Status = "partial"
			if !stream.Truncated && stream.Terminal == "completed" && a.Terminal == "completed" && a.Error == "" &&
				a.Status >= 200 && a.Status < 300 && s.Status >= 200 && s.Status < 300 && !s.Canceled && !s.ClientDisconnect {
				out.TextStream.Status = "observed"
			}
		}
	}
	if out.ReasoningFirstMS != nil || out.VisibleTextFirstMS != nil || out.ResponseByteFirstMS != nil {
		out.Status = "observed"
	}
	out.OutputRate = deriveOutputRate(s, a, out.TextStream)
	return out
}

func NormalizeObservationEffort(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) > 32 {
		return ""
	}
	return value
}
