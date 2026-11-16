package requesttiming

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFinalCacheUpstreamAttribution(t *testing.T) {
	credential, route := strings.Repeat("a", 64), strings.Repeat("b", 64)
	physical := Attempt{Kind: "http", Number: 2, Parent: 1, AccountID: 7, ProxyID: 3, RouteFingerprint: route,
		UpstreamCredential: &UpstreamCredentialObservation{Status: "observed", Source: "http_transport_handoff", Fingerprint: credential}}
	original := Snapshot{Version: 1, FinalResponseAttempt: 1, Attempts: []Attempt{{Kind: "egress", Number: 1}, physical,
		{Kind: "egress", Number: 3, AccountID: 99}}}
	got := DeriveCacheUpstream(original)
	require.Equal(t, "observed", got.Status)
	require.Equal(t, credential, got.CredentialFingerprint)
	require.Equal(t, route, got.RouteFingerprint)
	require.EqualValues(t, 7, got.AccountID)
	require.Equal(t, 2, got.AttemptNumber)
	require.Equal(t, "http", got.Transport)
	require.Equal(t, "not_observed", got.UpstreamGroupStatus)
	t.Run("ambiguous_physical_number", func(t *testing.T) {
		s := original
		s.Attempts = append(append([]Attempt(nil), original.Attempts...), physical)
		v := DeriveCacheUpstream(s)
		require.Equal(t, "observed", v.Status, "legacy cache attribution stays unchanged")
		require.Zero(t, v.AttemptNumber, "ambiguous physical metrics must not be joined")
	})
	for _, tc := range []struct {
		name   string
		change func(*Snapshot)
	}{
		{"truncated", func(s *Snapshot) { s.Truncated = true }},
		{"invalid_owner", func(s *Snapshot) { s.FinalResponseAttempt = 9 }},
		{"changed_credentials", func(s *Snapshot) { s.Attempts[1].UpstreamCredential.Status = "changed_credentials" }},
		{"missing_credentials", func(s *Snapshot) { s.Attempts[1].UpstreamCredential = nil }},
		{"invalid_digest", func(s *Snapshot) { s.Attempts[1].UpstreamCredential.Fingerprint = "fixture-secret" }},
		{"zero_digest", func(s *Snapshot) { s.Attempts[1].UpstreamCredential.Fingerprint = strings.Repeat("0", 64) }},
		{"wrong_source", func(s *Snapshot) { s.Attempts[1].UpstreamCredential.Source = "account_config" }},
		{"route_missing", func(s *Snapshot) { s.Attempts[1].RouteFingerprint = "" }},
		{"account_missing", func(s *Snapshot) { s.Attempts[1].AccountID = 0 }},
		{"unsupported_version", func(s *Snapshot) { s.Version = 2 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := original
			s.Attempts = append([]Attempt(nil), original.Attempts...)
			c := *physical.UpstreamCredential
			s.Attempts[1].UpstreamCredential = &c
			tc.change(&s)
			v := DeriveCacheUpstream(s)
			require.NotEqual(t, "observed", v.Status)
			require.Empty(t, v.CredentialFingerprint)
			require.Empty(t, v.RouteFingerprint)
		})
	}
	t.Run("websocket_generation", func(t *testing.T) {
		s := Snapshot{Version: 1, TTFTMode: "ws_turn", Attempts: []Attempt{physical}}
		s.Attempts[0].Kind = "ws_turn"
		c := *physical.UpstreamCredential
		c.Source = "websocket_handshake_credentials"
		s.Attempts[0].UpstreamCredential = &c
		require.Equal(t, "observed", DeriveCacheUpstream(s).Status)
		s.TTFTMode = ""
		require.NotEqual(t, "observed", DeriveCacheUpstream(s).Status)
	})
}
