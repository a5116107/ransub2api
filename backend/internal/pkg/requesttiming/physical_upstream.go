package requesttiming

import (
	"math"
	"strings"
)

// PhysicalCacheObservation is this generation's canonical report. It does not
// assert downstream delivery, cache retention, or a supplier cache domain.
type PhysicalCacheObservation struct {
	Version int `json:"version"`
	CacheObservation
}

// PhysicalUpstreamObservation is independent of usage/billing. It counts
// transport handoffs, not supplier receipt, socket handshakes or egress parents.
// Missing historical model/group/source evidence is deliberately not inferred.
type PhysicalUpstreamObservation struct {
	Version  int                       `json:"version"`
	Complete bool                      `json:"complete"`
	Attempts []PhysicalUpstreamAttempt `json:"attempts"`
}

// PhysicalOutputObservation describes one generation, independently of whether
// its result was delivered downstream. It never borrows a later retry's usage.
type PhysicalOutputObservation struct {
	Version    int                   `json:"version"`
	TextStream TextStreamObservation `json:"text_stream"`
	Rate       OutputRateObservation `json:"rate"`
}

type PhysicalUpstreamAttempt struct {
	Output                PhysicalOutputObservation   `json:"output"`
	RawCache              PhysicalCacheObservation    `json:"raw_cache"`
	UpstreamRequest       *UpstreamRequestObservation `json:"upstream_request,omitempty"`
	Number                int                         `json:"number"`
	AccountID             int64                       `json:"account_id"`
	Transport             string                      `json:"transport"`
	Protocol              string                      `json:"protocol,omitempty"`
	CredentialFingerprint string                      `json:"credential_fingerprint,omitempty"`
	RouteFingerprint      string                      `json:"route_fingerprint,omitempty"`
	IdentityKnown         bool                        `json:"identity_known"`
	Outcome               string                      `json:"outcome"`
}

func DerivePhysicalUpstream(s Snapshot) PhysicalUpstreamObservation {
	out := PhysicalUpstreamObservation{Version: 1, Complete: s.Version == 1 && !s.Truncated && len(s.Attempts) <= 32,
		Attempts: []PhysicalUpstreamAttempt{}}
	if s.Version != 1 || len(s.Attempts) > 32 {
		return out
	}
	byNumber := make(map[int]Attempt, len(s.Attempts))
	numbers := make(map[int]int, len(s.Attempts))
	for _, a := range s.Attempts {
		byNumber[a.Number] = a
		numbers[a.Number]++
	}
	for _, a := range s.Attempts {
		if a.Number < 1 || a.Number > 32 || numbers[a.Number] != 1 {
			out.Complete = false
			continue
		}
		if a.Kind != "http" && a.Kind != "ws_turn" {
			// An egress without a physical child has no observed handoff. It
			// may be a local rejection, so do not invent an upstream failure.
			if a.Kind == "egress" && a.Route.Upstream != "websocket" {
				child := false
				for _, candidate := range s.Attempts {
					child = child || (candidate.Kind == "http" && candidate.Parent == a.Number)
				}
				out.Complete = out.Complete && child
			}
			continue
		}
		if a.Kind == "http" {
			handshake, validParents := physicalHandshake(a, byNumber, numbers)
			if !validParents {
				out.Complete = false
				continue
			}
			if a.Status == 101 || handshake || a.Route.Endpoint == "other" {
				continue
			}
			if a.UpstreamCredential == nil {
				out.Complete = false
				continue
			}
		} else if s.TTFTMode != "ws_turn" || len(s.Attempts) != 1 || a.Parent != 0 || a.Status != 0 {
			out.Complete = false
			continue
		}
		item := PhysicalUpstreamAttempt{Number: a.Number, AccountID: a.AccountID, Transport: a.Kind, Outcome: physicalUpstreamOutcome(s, a)}
		item.Output = derivePhysicalOutput(s, a, item.Outcome == "succeeded")
		cache := deriveCacheObservation(s, a, item.Outcome == "succeeded")
		cache.Scope = strings.TrimPrefix(cache.Scope, "final_")
		item.RawCache = PhysicalCacheObservation{Version: 1, CacheObservation: cache}
		if observed := a.UpstreamRequest; observed != nil && observed.Version == 1 && observed.Status == "observed" &&
			(a.Kind == "http" && observed.Source == "http_transport_body" || a.Kind == "ws_turn" && observed.Source == "websocket_generation_payload") {
			value := *observed
			item.UpstreamRequest = &value
		}
		switch a.Route.Endpoint {
		case "responses", "responses_compact":
			item.Protocol = "responses"
		case "messages":
			item.Protocol = "messages"
		case "chat_completions":
			item.Protocol = "chat_completions"
		case "gemini_generate_content", "gemini_stream_generate_content":
			item.Protocol = "gemini"
		}
		if a.Kind == "ws_turn" {
			item.Protocol = "responses"
		}
		identity := DeriveCacheUpstream(Snapshot{Version: 1, TTFTMode: s.TTFTMode, Attempts: []Attempt{a}})
		if identity.Status == "observed" {
			item.IdentityKnown = true
			item.CredentialFingerprint, item.RouteFingerprint = identity.CredentialFingerprint, identity.RouteFingerprint
		}
		out.Attempts = append(out.Attempts, item)
	}
	return out
}

func physicalUpstreamOutcome(s Snapshot, a Attempt) string {
	if a.Status >= 400 && a.Status <= 599 || a.Terminal == "failed" || a.Terminal == "incomplete" || a.Error == "timeout" || a.Error == "transport_error" {
		return "failed"
	}
	if a.Error == "canceled" {
		return "interrupted"
	}
	if !finite(s.TotalMS) || s.TotalMS < 0 || !finite(a.StartMS) || a.StartMS < 0 || a.EndMS == nil ||
		!finite(*a.EndMS) || *a.EndMS < a.StartMS || *a.EndMS > s.TotalMS {
		return "unknown"
	}
	if a.Error != "" || s.Truncated {
		return "unknown"
	}
	if _, lost := a.Events["typed_content_truncated"]; lost {
		return "unknown"
	}
	if a.TextStream != nil && (a.TextStream.Invalid || a.TextStream.Truncated) {
		return "unknown"
	}
	if a.Terminal == "completed" && (a.Kind == "ws_turn" || a.Status >= 200 && a.Status < 300) {
		return "succeeded"
	}
	return "unknown"
}

func physicalHandshake(a Attempt, byNumber map[int]Attempt, numbers map[int]int) (bool, bool) {
	previous := a.Number
	for parent := a.Parent; parent != 0; {
		p, exists := byNumber[parent]
		if !exists || parent < 1 || parent >= previous || numbers[parent] != 1 {
			return false, false
		}
		if p.Route.Upstream == "websocket" || p.Kind == "ws_turn" {
			return true, true
		}
		previous, parent = parent, p.Parent
	}
	return false, true
}

// Buffered parsing may finish after a transport body closes, but it must never
// consume the observation window of a subsequent physical request.
func derivePhysicalOutput(s Snapshot, a Attempt, succeeded bool) PhysicalOutputObservation {
	stream := TextStreamObservation{Status: "not_collected", Source: "typed_responses_sse", Scope: "physical_http_responses_text_deltas_at_parser"}
	if a.Kind == "ws_turn" {
		stream.Source, stream.Scope = "typed_responses_ws", "physical_websocket_turn_text_deltas_at_parser"
	} else if a.TextStream != nil && a.TextStream.Protocol != "" && a.TextStream.Protocol != "responses" {
		protocol := a.TextStream.Protocol
		stream.Source = "typed_" + protocol + "_sse"
		stream.Scope = "physical_http_" + protocol + "_text_deltas_at_parser"
		if protocol == "gemini" {
			stream.Source, stream.Scope = "typed_gemini_content", "physical_http_gemini_primary_text_chunks_at_parser"
		}
	}
	upper := s.TotalMS
	valid := finite(upper) && upper >= 0 && finite(a.StartMS) && a.StartMS >= 0 && a.EndMS != nil && finite(*a.EndMS) && *a.EndMS >= a.StartMS && *a.EndMS <= upper
	if a.Kind == "http" {
		for _, next := range s.Attempts {
			if next.Kind != "http" || next.Number <= a.Number {
				continue
			}
			if !finite(next.StartMS) || next.StartMS < a.StartMS {
				valid = false
				break
			}
			upper = math.Min(upper, next.StartMS)
		}
	}
	if valid {
		stream = deriveTextStream(a, stream, upper, succeeded && !s.Truncated)
	} else {
		stream.Status = "invalid_boundary"
	}
	rate := deriveOutputRate(a, stream, succeeded && !s.Truncated)
	rate.Scope = strings.TrimPrefix(rate.Scope, "final_")
	return PhysicalOutputObservation{Version: 1, TextStream: stream, Rate: rate}
}
