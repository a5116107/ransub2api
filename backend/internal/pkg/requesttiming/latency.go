package requesttiming

import "math"

// Measurement distinguishes a measured zero from an unavailable boundary.
type Measurement struct {
	MS     *float64 `json:"ms"`
	Status string   `json:"status"`
}

type AttemptLatency struct {
	Number             int         `json:"number"`
	Transition         string      `json:"transition"`
	Upload             Measurement `json:"body_upload"`
	SemanticWait       Measurement `json:"post_upload_first_content_wait"`
	VisibleWait        Measurement `json:"post_upload_first_visible_wait"`
	FirstByteWait      Measurement `json:"post_upload_first_byte_wait"`
	FirstEventWait     Measurement `json:"post_upload_first_event_wait"`
	FirstReasoningWait Measurement `json:"post_upload_first_reasoning_wait"`
	FirstTextWait      Measurement `json:"post_upload_first_text_wait"`
	ResponseWait       Measurement `json:"post_upload_response_wait"`
	RequestBytes       int64       `json:"request_bytes"`
	Failed             bool        `json:"failed"`
}

// WSTurnLatency deliberately has no HTTP upload, socket-byte or SSE boundary.
// Its origin is the independent generation capture just before the send attempt.
type WSTurnLatency struct {
	Number         int         `json:"number"`
	Source         string      `json:"source"`
	FirstEvent     Measurement `json:"first_event"`
	FirstReasoning Measurement `json:"first_reasoning"`
	FirstText      Measurement `json:"first_text"`
}

type LatencySummary struct {
	WSTurn                 *WSTurnLatency            `json:"ws_turn,omitempty"`
	CacheMigration         CacheMigrationObservation `json:"cache_migration"`
	Version                int                       `json:"version"`
	Status                 string                    `json:"status"`
	Attempts               []AttemptLatency          `json:"attempts"`
	AccountSwitches        int                       `json:"account_switches"`
	SameAccountRetries     int                       `json:"same_account_retries"`
	UnknownTransitions     int                       `json:"unknown_transitions"`
	LargeBody              bool                      `json:"large_body"`
	ExtraUploadComponentMS *float64                  `json:"extra_upload_component_ms"`
	ExtraWaitComponentMS   *float64                  `json:"extra_wait_component_ms"`
	CostStatus             string                    `json:"cost_status"`
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func attemptInterval(a Attempt, start, end string) Measurement {
	s, sok := a.Events[start]
	e, eok := a.Events[end]
	if !sok || !eok {
		return Measurement{Status: "not_collected"}
	}
	if !finite(s) || !finite(e) || !finite(a.StartMS) || a.StartMS < 0 || s < a.StartMS || e < s ||
		a.EndMS == nil || !finite(*a.EndMS) || e > *a.EndMS {
		return Measurement{Status: "invalid_boundary"}
	}
	v := e - s
	return Measurement{MS: &v, Status: "measured"}
}

func deriveWSTurnLatency(s Snapshot) *WSTurnLatency {
	if s.Version != 1 || s.TTFTMode != "ws_turn" || s.Status != 0 || len(s.Attempts) != 1 ||
		(s.FinalResponseAttempt != 0 && s.FinalResponseAttempt != 1) {
		return nil
	}
	a := s.Attempts[0]
	if a.Kind != "ws_turn" || a.Number != 1 || a.Parent != 0 || a.Status != 0 {
		return nil
	}
	read := func(event string) Measurement {
		if _, gap := a.Events["typed_content_truncated"]; s.Truncated || gap {
			return Measurement{Status: "incomplete_capture"}
		}
		if event != "first_upstream_event" && a.TextStream != nil && (a.TextStream.Invalid || a.TextStream.Truncated) {
			return Measurement{Status: "invalid_boundary"}
		}
		value, ok := a.Events[event]
		if !ok {
			return Measurement{Status: "not_collected"}
		}
		if !finite(s.TotalMS) || !finite(a.StartMS) || a.StartMS < 0 || !finite(value) || value < a.StartMS ||
			a.EndMS == nil || !finite(*a.EndMS) || *a.EndMS > s.TotalMS || value > *a.EndMS {
			return Measurement{Status: "invalid_boundary"}
		}
		ms := value - a.StartMS
		return Measurement{MS: &ms, Status: "measured"}
	}
	return &WSTurnLatency{Number: a.Number, Source: "physical_ws_turn_generation_start",
		FirstEvent: read("first_upstream_event"), FirstReasoning: read("first_reasoning"), FirstText: read("first_visible_text")}
}

// DeriveLatency keeps HTTP attempts and a physical WS generation independent,
// never borrowing the request-wide event map or enclosing egress parents.
// HTTP component sums are not elapsed wall time.
// It also supports the #599 request_written name and the wrote_request alias.
func DeriveLatency(s Snapshot) LatencySummary {
	out := LatencySummary{Version: 1, Status: "measured", Attempts: []AttemptLatency{}, CostStatus: "not_applicable", LargeBody: s.BodyBytes > 1<<20}
	out.WSTurn = deriveWSTurnLatency(s)
	var previous *Attempt
	var upload, wait float64
	costComplete, overlap := true, false
	parents := make(map[int]bool)
	for _, a := range s.Attempts {
		if a.Kind == "http" && a.Parent > 0 {
			parents[a.Parent] = true
		}
	}
	unobservedEgress := false
	for _, a := range s.Attempts {
		if a.Kind == "egress" && !parents[a.Number] {
			unobservedEgress = true
		}
	}
	for i := range s.Attempts {
		a := &s.Attempts[i]
		if a.Kind != "http" {
			continue
		}
		if a.RequestBytes > 1<<20 {
			out.LargeBody = true
		}
		written := "request_written"
		if _, ok := a.Events[written]; !ok {
			written = "wrote_request"
		}
		item := AttemptLatency{Number: a.Number, Transition: "initial", RequestBytes: a.RequestBytes, Failed: a.Error != "" || a.Status >= 400 || a.Terminal == "failed" || a.Terminal == "incomplete"}
		item.Upload = attemptInterval(*a, "headers_written", written)
		if _, failed := a.Events["request_write_failed"]; failed {
			item.Upload = Measurement{Status: "write_failed"}
		}
		// Network EOF can precede parsing buffered SSE data. Parser timestamps
		// may extend to request completion, but never borrow the next physical
		// request's lifetime. Network first-byte evidence stays bounded by EOF.
		parsed := *a
		if a.EndMS != nil && finite(*a.EndMS) && finite(s.TotalMS) && s.TotalMS > *a.EndMS {
			end := s.TotalMS
			for _, next := range s.Attempts[i+1:] {
				if next.Kind == "http" {
					if !finite(next.StartMS) || next.StartMS < 0 {
						end = *a.EndMS
						break
					}
					end = min(end, next.StartMS)
				}
			}
			if end > *a.EndMS {
				parsed.EndMS = &end
			}
		}
		item.SemanticWait = attemptInterval(parsed, written, "first_semantic")
		item.VisibleWait = attemptInterval(parsed, written, "first_visible")
		item.FirstByteWait = attemptInterval(*a, written, "first_byte")
		item.FirstEventWait = attemptInterval(parsed, written, "first_sse_event_flush")
		item.FirstReasoningWait = attemptInterval(parsed, written, "first_reasoning")
		item.FirstTextWait = attemptInterval(parsed, written, "first_visible_text")
		item.ResponseWait = attemptInterval(*a, written, "response_headers")
		if _, incomplete := a.Events["typed_content_truncated"]; incomplete {
			item.FirstEventWait = Measurement{Status: "incomplete_capture"}
			item.FirstReasoningWait = Measurement{Status: "incomplete_capture"}
			item.FirstTextWait = Measurement{Status: "incomplete_capture"}
		}
		if item.Upload.Status != "measured" {
			item.SemanticWait = Measurement{Status: item.Upload.Status}
			item.VisibleWait = Measurement{Status: item.Upload.Status}
			item.ResponseWait = Measurement{Status: item.Upload.Status}
			item.FirstByteWait = Measurement{Status: item.Upload.Status}
			item.FirstEventWait = Measurement{Status: item.Upload.Status}
			item.FirstReasoningWait = Measurement{Status: item.Upload.Status}
			item.FirstTextWait = Measurement{Status: item.Upload.Status}
		}
		if previous != nil {
			switch {
			case previous.AccountID <= 0 || a.AccountID <= 0:
				item.Transition = "unknown"
				out.UnknownTransitions++
			case previous.AccountID == a.AccountID:
				item.Transition = "same_account_retry"
				out.SameAccountRetries++
			default:
				item.Transition = "account_switch"
				out.AccountSwitches++
			}
			if previous.EndMS == nil || !finite(*previous.EndMS) || !finite(a.StartMS) {
				costComplete = false
			} else if a.StartMS < *previous.EndMS {
				overlap = true
			}
			if item.Upload.MS == nil {
				costComplete = false
			} else {
				upload += *item.Upload.MS
			}
			// Failed attempts have no first content: response-header wait remains
			// a distinct, measurable network/upstream response component.
			w := item.SemanticWait.MS
			if w == nil && a.Status >= 400 {
				w = item.ResponseWait.MS
			}
			if w == nil {
				costComplete = false
			} else {
				wait += *w
			}
		}
		out.Attempts = append(out.Attempts, item)
		previous = a
	}
	if len(out.Attempts) == 0 {
		out.Status = "not_collected"
	}
	if s.Truncated || unobservedEgress || overlap || out.UnknownTransitions > 0 {
		out.Status = "partial"
	}
	if out.LargeBody && len(out.Attempts) > 1 {
		switch {
		case overlap:
			out.CostStatus = "overlap"
		case !costComplete || s.Truncated || unobservedEgress || out.UnknownTransitions > 0:
			out.CostStatus = "partial"
		default:
			out.CostStatus = "measured_components"
			out.ExtraUploadComponentMS, out.ExtraWaitComponentMS = &upload, &wait
		}
	}
	out.CacheMigration = deriveCacheMigration(s, out.Status)
	return out
}
