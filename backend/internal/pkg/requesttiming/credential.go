package requesttiming

import (
	"context"
	"encoding/hex"
)

// UpstreamCredentialObservation identifies transport credentials, independently
// of account/route identity. It proves neither a vendor group nor a cache domain.
// Only a unique verified digest is retained; raw credentials never enter timing.
type UpstreamCredentialObservation struct {
	Status      string `json:"status"`
	Source      string `json:"source"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

// RecordUpstreamCredentials receives the existing final-credential extractor's
// digests at transport handoff. A Client.Do can contain redirects/fallbacks: if
// their identities disagree, the whole attempt becomes unattributable. This
// observation does not establish that bytes reached a supplier.
func RecordUpstreamCredentials(ctx context.Context, fingerprints []string) {
	if ctx == nil {
		return
	}
	tr, _ := ctx.Value(attemptKey{}).(*Trace)
	if tr == nil {
		return
	}
	observation := UpstreamCredentialObservation{Status: "no_credential"}
	if len(fingerprints) > 8 {
		observation.Status = "unverified"
	} else {
		for _, fingerprint := range fingerprints {
			if len(fingerprint) != 64 {
				observation.Status = "unverified"
				observation.Fingerprint = ""
				break
			}
			digest, err := hex.DecodeString(fingerprint)
			if err != nil || len(digest) != 32 {
				observation.Status = "unverified"
				observation.Fingerprint = ""
				break
			}
			canonical := hex.EncodeToString(digest)
			if observation.Fingerprint != "" && observation.Fingerprint != canonical {
				observation.Status = "ambiguous"
			}
			observation.Fingerprint = canonical
			if observation.Status != "ambiguous" {
				observation.Status = "observed"
			}
		}
		if observation.Status != "observed" {
			observation.Fingerprint = ""
		}
	}
	tr.update(func(a *Attempt) {
		if a.EndMS != nil {
			return
		}
		switch a.Kind {
		case "http":
			observation.Source = "http_transport_handoff"
		case "ws_turn":
			observation.Source = "websocket_handshake_credentials"
		default:
			return
		}
		if a.UpstreamCredential == nil {
			a.UpstreamCredential = &observation
		} else if *a.UpstreamCredential != observation {
			a.UpstreamCredential = &UpstreamCredentialObservation{Status: "changed_credentials", Source: observation.Source}
		}
	})
}
