package requesttiming

import "encoding/hex"

// CacheMigrationObservation bounds attribution for the final response of one
// request. Non-read input includes cache creation. Neither the range nor a
// client session identifier proves a reusable prefix or predicts the next turn.
type CacheMigrationObservation struct {
	Status                  string `json:"status"`
	Source                  string `json:"source"`
	Scope                   string `json:"scope"`
	Method                  string `json:"method"`
	CacheStatus             string `json:"cache_status"`
	RouteSwitches           int    `json:"route_switches"`
	UnknownTransitions      int    `json:"unknown_transitions"`
	InputTokens             *int64 `json:"input_tokens"`
	ReadTokens              *int64 `json:"read_tokens"`
	NonReadInputTokens      *int64 `json:"non_read_input_tokens"`
	ExtraColdInputMinTokens *int64 `json:"extra_cold_input_min_tokens"`
	ExtraColdInputMaxTokens *int64 `json:"extra_cold_input_max_tokens"`
}

func deriveCacheMigration(s Snapshot, latencyStatus string) CacheMigrationObservation {
	out := CacheMigrationObservation{Status: "not_collected", Source: "none", CacheStatus: "not_collected",
		Scope: "final_physical_http_input_after_request_route_transitions", Method: "zero_to_final_non_read_input_not_causal"}
	if s.Version != 1 || len(s.Attempts) == 0 {
		return out
	}
	if s.TTFTMode == "ws_turn" {
		out.Status = "unsupported_transport"
		return out
	}
	last := s.Attempts[len(s.Attempts)-1]
	if last.Kind != "http" {
		return out
	}
	if s.Truncated || latencyStatus != "measured" {
		out.Status = "partial"
		return out
	}
	cache := deriveCacheObservation(s, last)
	out.Source, out.CacheStatus = cache.Source, cache.Status
	if cache.Status != "observed" || cache.InputTokens == nil || cache.ReadTokens == nil {
		out.Status = "cache_usage_unavailable"
		return out
	}
	input, read := *cache.InputTokens, *cache.ReadTokens
	nonRead := input - read
	out.InputTokens, out.ReadTokens, out.NonReadInputTokens = &input, &read, &nonRead
	var previous *Attempt
	previousKnown, unknownRoute := false, false
	for i := range s.Attempts {
		a := &s.Attempts[i]
		if a.Kind == "ws_turn" {
			out.Status = "unsupported_transport"
			return out
		}
		if a.Kind != "http" {
			continue
		}
		if a.Number <= 0 || !finite(a.StartMS) || a.StartMS < 0 || a.EndMS == nil || !finite(*a.EndMS) ||
			*a.EndMS < a.StartMS || !finite(s.TotalMS) || *a.EndMS > s.TotalMS ||
			(previous != nil && (a.Number <= previous.Number || a.StartMS < *previous.EndMS)) {
			out.Status = "invalid_boundary"
			return out
		}
		digest, err := hex.DecodeString(a.RouteFingerprint)
		known := a.AccountID > 0 && a.ProxyID >= 0 && err == nil && len(digest) == 32 &&
			[32]byte(digest) != [32]byte{} && hex.EncodeToString(digest) == a.RouteFingerprint
		unknownRoute = unknownRoute || !known
		if previous != nil {
			switch {
			case !previousKnown || !known:
				out.UnknownTransitions++
			case previous.AccountID != a.AccountID || previous.ProxyID != a.ProxyID || previous.RouteFingerprint != a.RouteFingerprint:
				out.RouteSwitches++
			}
		}
		previous, previousKnown = a, known
	}
	if unknownRoute {
		out.Status = "unknown_route"
		return out
	}
	if out.RouteSwitches == 0 {
		out.Status = "no_observed_route_change"
		return out
	}
	// No same-prefix counterfactual is observed: any amount between zero and the
	// final non-read input could be attributable to the observed route change.
	zero := int64(0)
	out.ExtraColdInputMinTokens, out.ExtraColdInputMaxTokens = &zero, &nonRead
	out.Status = "bounded_attribution_unknown"
	return out
}
