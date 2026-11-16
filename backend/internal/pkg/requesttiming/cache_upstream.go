package requesttiming

import "encoding/hex"

// CacheUpstreamObservation is a bounded index projection of the same final
// physical attempt as RawCache. It is not a supplier group or cache-domain proof.
// Credential identity stays admin-only; missing/ambiguous scope never borrows the
// current account configuration or an earlier retry's credential.
type CacheUpstreamObservation struct {
	Version               int    `json:"version"`
	Status                string `json:"status"`
	CredentialFingerprint string `json:"credential_fingerprint,omitempty"`
	CredentialSource      string `json:"credential_source,omitempty"`
	RouteFingerprint      string `json:"route_fingerprint,omitempty"`
	AccountID             int64  `json:"account_id,omitempty"`
	AttemptNumber         int    `json:"attempt_number,omitempty"`
	ProxyID               int64  `json:"proxy_id"`
	Transport             string `json:"transport,omitempty"`
	UpstreamGroupStatus   string `json:"upstream_group_status"`
}

func DeriveCacheUpstream(s Snapshot) CacheUpstreamObservation {
	out := CacheUpstreamObservation{Version: 1, Status: "not_collected", UpstreamGroupStatus: "not_observed"}
	if s.Version != 1 || s.Truncated || len(s.Attempts) == 0 {
		return out
	}
	a, ok := finalContentAttempt(s)
	if !ok || (a.Kind != "http" && (a.Kind != "ws_turn" || s.TTFTMode != "ws_turn")) {
		return out
	}
	credential := a.UpstreamCredential
	if credential == nil || credential.Status != "observed" {
		out.Status = "credential_unconfirmed"
		return out
	}
	expectedSource := "http_transport_handoff"
	if a.Kind == "ws_turn" {
		expectedSource = "websocket_handshake_credentials"
	}
	if credential.Source != expectedSource {
		out.Status = "credential_unconfirmed"
		return out
	}
	for _, value := range []string{credential.Fingerprint, a.RouteFingerprint} {
		digest, err := hex.DecodeString(value)
		if err != nil || len(digest) != 32 || hex.EncodeToString(digest) != value || [32]byte(digest) == [32]byte{} {
			out.Status = "identity_unconfirmed"
			return out
		}
	}
	if a.AccountID <= 0 || a.ProxyID < 0 {
		out.Status = "route_unconfirmed"
		return out
	}
	out.Status = "observed"
	out.CredentialFingerprint, out.CredentialSource = credential.Fingerprint, credential.Source
	out.RouteFingerprint, out.AccountID, out.ProxyID, out.Transport = a.RouteFingerprint, a.AccountID, a.ProxyID, a.Kind
	if a.Number > 0 && a.Number <= 32 {
		matches := 0
		for _, candidate := range s.Attempts {
			if candidate.Number == a.Number {
				matches++
			}
		}
		if matches == 1 {
			out.AttemptNumber = a.Number
		}
	}
	return out
}
