package requesttiming

import (
	"math"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/util/transportdiag"
	"github.com/stretchr/testify/require"
)

func physicalUpstreamFixture() Snapshot {
	end := 100.0
	a := Attempt{Kind: "http", Number: 1, AccountID: 7, EndMS: &end, Status: 502,
		Route: transportdiag.Route{Endpoint: "responses"}, RouteFingerprint: strings.Repeat("a", 64),
		UpstreamCredential: &UpstreamCredentialObservation{Status: "observed", Source: "http_transport_handoff", Fingerprint: strings.Repeat("b", 64)}}
	b := a
	b.Number, b.AccountID, b.Status, b.Terminal = 2, 8, 200, "completed"
	return Snapshot{Version: 1, TotalMS: 100, Status: 200, Terminal: "completed", Outcome: "success", Attempts: []Attempt{a, b}}
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
