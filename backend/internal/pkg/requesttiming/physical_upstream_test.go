package requesttiming

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/util/transportdiag"
	"github.com/stretchr/testify/require"
)

func TestPhysicalUpstreamRawCacheIsPerAttemptNotDownstreamDelivery(t *testing.T) {
	s := physicalUpstreamFixture()
	s.Attempts[0].CacheUsage = &CacheUsage{Status: "observed", InputTokens: 100, ReadTokens: 99}
	s.Attempts[1].CacheUsage = &CacheUsage{Status: "observed", InputTokens: 200, ReadTokens: 40}
	s.Status, s.Outcome, s.Terminal = 502, "failed", "failed"
	s.DownstreamError, s.ClientDisconnect, s.Canceled = true, true, true
	for i, a := range DerivePhysicalUpstream(s).Attempts {
		var report struct {
			Cache struct {
				Version int `json:"version"`
				CacheObservation
			} `json:"raw_cache"`
		}
		raw, err := json.Marshal(a)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &report))
		require.Equal(t, 1, report.Cache.Version)
		require.Equal(t, "physical_http_responses_terminal_usage", report.Cache.Scope)
		if i == 0 {
			require.Equal(t, "partial", report.Cache.Status)
			require.Nil(t, report.Cache.InputTokens, "failed attempts never become cache misses or borrow final usage")
		} else {
			require.Equal(t, "observed", report.Cache.Status, "upstream completion and downstream delivery are different observations")
			require.EqualValues(t, 200, *report.Cache.InputTokens)
			require.EqualValues(t, 40, *report.Cache.ReadTokens)
			require.InDelta(t, 0.2, *report.Cache.ReadRatio, 0.000001)
		}
	}
}

func TestPhysicalUpstreamRawCacheBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Snapshot, *Attempt)
		status string
	}{
		{"hit", func(*Snapshot, *Attempt) {}, "observed"},
		{"explicit_zero", func(_ *Snapshot, a *Attempt) { a.CacheUsage.ReadTokens = 0 }, "observed"},
		{"missing", func(_ *Snapshot, a *Attempt) { a.CacheUsage = nil }, "not_collected"},
		{"missing_counter", func(_ *Snapshot, a *Attempt) { a.CacheUsage.Status = "not_reported" }, "not_reported"},
		{"invalid_counter", func(_ *Snapshot, a *Attempt) { a.CacheUsage.ReadTokens = 101 }, "invalid_counters"},
		{"unsafe_input", func(_ *Snapshot, a *Attempt) { a.CacheUsage.InputTokens = 1 << 53 }, "invalid_counters"},
		{"empty", func(_ *Snapshot, a *Attempt) { a.CacheUsage.InputTokens, a.CacheUsage.ReadTokens = 0, 0 }, "empty_input"},
		{"failed", func(_ *Snapshot, a *Attempt) { a.Terminal = "failed" }, "partial"},
		{"timeout", func(_ *Snapshot, a *Attempt) { a.Error = "timeout" }, "partial"},
		{"interrupted", func(_ *Snapshot, a *Attempt) { a.Error = "canceled" }, "partial"},
		{"unknown", func(_ *Snapshot, a *Attempt) { a.Terminal = "" }, "partial"},
		{"no_end", func(_ *Snapshot, a *Attempt) { a.EndMS = nil }, "partial"},
		{"snapshot_gap", func(s *Snapshot, _ *Attempt) { s.Truncated = true }, "partial"},
		{"parser_gap", func(_ *Snapshot, a *Attempt) { a.Events = map[string]float64{"typed_content_truncated": 1} }, "incomplete_capture"},
		{"foreign_protocol", func(_ *Snapshot, a *Attempt) { a.CacheUsage.Protocol = "gemini" }, "invalid_protocol"},
		{"ws_turn", func(s *Snapshot, a *Attempt) {
			s.Attempts, s.TTFTMode, s.Status = s.Attempts[1:], "ws_turn", 0
			a.Number, a.Kind, a.Status, a.UpstreamCredential.Source = 1, "ws_turn", 0, "websocket_handshake_credentials"
		}, "observed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := physicalUpstreamFixture()
			s.Attempts[1].CacheUsage = &CacheUsage{Status: "observed", InputTokens: 100, ReadTokens: 90}
			tc.change(&s, &s.Attempts[1])
			var report struct {
				Cache struct {
					Version int
					CacheObservation
				} `json:"raw_cache"`
			}
			attempts := DerivePhysicalUpstream(s).Attempts
			raw, err := json.Marshal(attempts[len(attempts)-1])
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &report))
			require.Equal(t, 1, report.Cache.Version)
			require.Equal(t, tc.status, report.Cache.Status)
			if tc.status != "observed" {
				require.Nil(t, report.Cache.ReadTokens)
				require.Nil(t, report.Cache.Hit)
			}
			if tc.name == "explicit_zero" {
				require.False(t, *report.Cache.Hit)
				require.Zero(t, *report.Cache.ReadRatio)
			}
			if tc.name == "ws_turn" {
				require.Equal(t, "physical_websocket_turn_responses_terminal_usage", report.Cache.Scope)
			}
		})
	}
}

func physicalUpstreamFixture() Snapshot {
	end := 100.0
	a := Attempt{Kind: "http", Number: 1, AccountID: 7, EndMS: &end, Status: 502,
		Route: transportdiag.Route{Endpoint: "responses"}, RouteFingerprint: strings.Repeat("a", 64),
		UpstreamCredential: &UpstreamCredentialObservation{Status: "observed", Source: "http_transport_handoff", Fingerprint: strings.Repeat("b", 64)}}
	b := a
	b.Number, b.AccountID, b.Status, b.Terminal = 2, 8, 200, "completed"
	return Snapshot{Version: 1, TotalMS: 100, Status: 200, Terminal: "completed", Outcome: "success", Attempts: []Attempt{a, b}}
}

func TestPhysicalUpstreamOutputKeepsAttemptScopeAndDeliverySeparate(t *testing.T) {
	for _, mode := range []string{"complete", "downstream_failed", "failed", "single", "zero_gap", "missing", "truncated", "cross_next_send", "ws"} {
		t.Run(mode, func(t *testing.T) {
			s := physicalUpstreamFixture()
			a := &s.Attempts[1]
			a.Events = map[string]float64{"first_visible_text": 10}
			a.TextStream = &TextStreamTiming{Chunks: 3, FirstMS: 10, LastMS: 310, MaxGapMS: 200, Terminal: "completed", TerminalMS: 410}
			a.OutputUsage = &OutputUsage{Status: "observed", ContentStatus: "text_and_reasoning", OutputTokens: 120, ReasoningTokens: 20}
			end := 450.0
			a.EndMS, s.TotalMS, s.Terminal = &end, 500, "completed"
			switch mode {
			case "downstream_failed":
				s.DownstreamError, s.Status, s.Outcome, s.Terminal = true, 502, "failed", "failed"
			case "failed":
				a.Terminal, a.TextStream.Terminal = "failed", "failed"
			case "single":
				a.TextStream.Chunks, a.TextStream.LastMS, a.TextStream.MaxGapMS = 1, 10, 0
			case "zero_gap":
				a.TextStream.Chunks, a.TextStream.LastMS, a.TextStream.MaxGapMS = 2, 10, 0
			case "missing":
				a.TextStream = nil
			case "truncated":
				a.TextStream.Truncated = true
			case "cross_next_send":
				s.Attempts = append(s.Attempts, Attempt{Number: 3, Kind: "http", StartMS: 300})
			case "ws":
				s.Attempts, s.TTFTMode, s.Status = s.Attempts[1:], "ws_turn", 0
				a.Number, a.Kind, a.Status, a.UpstreamCredential.Source = 1, "ws_turn", 0, "websocket_handshake_credentials"
			}
			var projected struct {
				Output struct {
					Version int
					Stream  TextStreamObservation `json:"text_stream"`
					Rate    OutputRateObservation `json:"rate"`
				} `json:"output"`
			}
			physical := DerivePhysicalUpstream(s)
			index := len(physical.Attempts) - 1
			if mode == "cross_next_send" {
				index = 1
			}
			raw, err := json.Marshal(physical.Attempts[index])
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &projected))
			require.Equal(t, 1, projected.Output.Version)
			if mode == "complete" || mode == "downstream_failed" || mode == "ws" {
				require.Equal(t, "observed", projected.Output.Stream.Status)
				require.Equal(t, 200.0, *projected.Output.Stream.MaxGapMS)
				require.Equal(t, 100.0, *projected.Output.Stream.TailWaitMS)
				require.Equal(t, "estimated", projected.Output.Rate.Status)
				require.Equal(t, 250.0, *projected.Output.Rate.TokensPerSecond)
			} else if mode == "failed" || mode == "truncated" {
				require.Equal(t, "partial", projected.Output.Stream.Status)
				require.Equal(t, 200.0, *projected.Output.Stream.MaxGapMS)
				require.Nil(t, projected.Output.Rate.TokensPerSecond)
			} else if mode == "single" {
				require.Nil(t, projected.Output.Stream.MaxGapMS)
			} else if mode == "zero_gap" {
				require.Equal(t, 0.0, *projected.Output.Stream.MaxGapMS)
			} else {
				require.Nil(t, projected.Output.Stream.MaxGapMS)
				require.Nil(t, projected.Output.Rate.TokensPerSecond)
			}
		})
	}
}

func TestPhysicalUpstreamKeepsFailuresBeforeSuccess(t *testing.T) {
	s := physicalUpstreamFixture()
	out := DerivePhysicalUpstream(s)
	require.True(t, out.Complete)
	require.Len(t, out.Attempts, 2)
	require.Equal(t, "failed", out.Attempts[0].Outcome)
	require.Equal(t, "succeeded", out.Attempts[1].Outcome)
	for i, a := range out.Attempts {
		require.True(t, a.IdentityKnown)
		require.Equal(t, s.Attempts[i].AccountID, a.AccountID)
		require.Equal(t, "responses", a.Protocol)
	}
}

func TestPhysicalUpstreamOutcomeBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Snapshot, *Attempt)
		want   string
	}{
		{"success", func(*Snapshot, *Attempt) {}, "succeeded"},
		{"header200_is_not_complete", func(_ *Snapshot, a *Attempt) { a.Terminal = "" }, "unknown"},
		{"stream_failure", func(_ *Snapshot, a *Attempt) { a.Terminal = "failed" }, "failed"},
		{"incomplete", func(_ *Snapshot, a *Attempt) { a.Terminal = "incomplete" }, "failed"},
		{"timeout", func(_ *Snapshot, a *Attempt) { a.Error = "timeout" }, "failed"},
		{"transport", func(_ *Snapshot, a *Attempt) { a.Error = "transport_error" }, "failed"},
		{"cancel", func(_ *Snapshot, a *Attempt) { a.Error = "canceled" }, "interrupted"},
		{"cleanup", func(_ *Snapshot, a *Attempt) { a.CleanupCanceled = true }, "succeeded"},
		{"capture_gap", func(s *Snapshot, _ *Attempt) { s.Truncated = true }, "unknown"},
		{"parser_gap", func(_ *Snapshot, a *Attempt) { a.Events = map[string]float64{"typed_content_truncated": 1} }, "unknown"},
		{"invalid_content", func(_ *Snapshot, a *Attempt) { a.TextStream = &TextStreamTiming{Invalid: true} }, "unknown"},
		{"truncated_content", func(_ *Snapshot, a *Attempt) { a.TextStream = &TextStreamTiming{Truncated: true} }, "unknown"},
		{"no_end", func(_ *Snapshot, a *Attempt) { a.EndMS = nil }, "unknown"},
		{"failure_without_timing", func(_ *Snapshot, a *Attempt) { a.EndMS = nil; a.Status = 503 }, "failed"},
		{"bad_interval", func(_ *Snapshot, a *Attempt) { a.StartMS = 101 }, "unknown"},
		{"nonfinite", func(s *Snapshot, _ *Attempt) { s.TotalMS = math.Inf(1) }, "unknown"},
		{"future_error", func(_ *Snapshot, a *Attempt) { a.Error = "future" }, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := physicalUpstreamFixture()
			tc.change(&s, &s.Attempts[1])
			require.Equal(t, tc.want, DerivePhysicalUpstream(s).Attempts[1].Outcome)
		})
	}
}

func TestPhysicalUpstreamExcludesParentsHandshakesAndUnsent(t *testing.T) {
	s := physicalUpstreamFixture()
	s.Attempts[0].Number, s.Attempts[1].Number = 2, 4
	s.Attempts[0].Parent, s.Attempts[1].Parent = 1, 3
	s.Attempts = []Attempt{{Number: 1, Kind: "egress"}, s.Attempts[0], {Number: 3, Kind: "egress"}, s.Attempts[1]}
	require.Len(t, DerivePhysicalUpstream(s).Attempts, 2)
	// A failed WebSocket handshake is still not a generation.
	s.Attempts[0].Route.Upstream = "websocket"
	require.Len(t, DerivePhysicalUpstream(s).Attempts, 1)
	s.Attempts[3].UpstreamCredential = nil
	out := DerivePhysicalUpstream(s)
	require.Empty(t, out.Attempts)
	require.False(t, out.Complete)
	s.Attempts = []Attempt{{Kind: "egress", Number: 1, Route: transportdiag.Route{Upstream: "websocket"}},
		{Kind: "egress", Number: 2, Parent: 1}, {Kind: "http", Number: 3, Parent: 2, Status: 403}}
	require.Empty(t, DerivePhysicalUpstream(s).Attempts, "failed nested handshakes do not become model attempts")
}

func TestPhysicalUpstreamUnknownAndAmbiguousIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Snapshot)
		want   int
	}{
		{"future", func(s *Snapshot) { s.Version = 9 }, 0},
		{"too_many", func(s *Snapshot) { s.Attempts = make([]Attempt, 33) }, 0},
		{"duplicate", func(s *Snapshot) { s.Attempts[1].Number = 1 }, 0},
		{"invalid_number", func(s *Snapshot) { s.Attempts[1].Number = 0 }, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := physicalUpstreamFixture()
			tc.change(&s)
			out := DerivePhysicalUpstream(s)
			require.False(t, out.Complete)
			require.Len(t, out.Attempts, tc.want)
		})
	}
	s := physicalUpstreamFixture()
	s.Attempts[0].RouteFingerprint = "unverified"
	out := DerivePhysicalUpstream(s)
	require.False(t, out.Attempts[0].IdentityKnown)
	require.Empty(t, out.Attempts[0].CredentialFingerprint)
	require.Empty(t, out.Attempts[0].RouteFingerprint)
	require.Equal(t, "failed", out.Attempts[0].Outcome, "missing attribution must not erase observed failures")
}

func TestPhysicalUpstreamIndependentWSTurn(t *testing.T) {
	s := physicalUpstreamFixture()
	s.Attempts = s.Attempts[1:]
	s.TTFTMode = "ws_turn"
	s.Status = 0
	a := &s.Attempts[0]
	a.Kind = "ws_turn"
	a.Number = 1
	a.Status = 0
	credential := *a.UpstreamCredential
	credential.Source = "websocket_handshake_credentials"
	a.UpstreamCredential = &credential
	out := DerivePhysicalUpstream(s)
	require.True(t, out.Complete)
	require.Len(t, out.Attempts, 1)
	require.True(t, out.Attempts[0].IdentityKnown)
	require.Equal(t, "succeeded", out.Attempts[0].Outcome)
	a.Error = "timeout"
	require.Equal(t, "failed", DerivePhysicalUpstream(s).Attempts[0].Outcome)
}
