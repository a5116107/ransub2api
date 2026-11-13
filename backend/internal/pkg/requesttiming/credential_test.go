package requesttiming

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUpstreamCredentialEvidenceValidation(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, tc := range []struct {
		name         string
		fingerprints []string
		status       string
		fingerprint  string
	}{
		{"missing", nil, "no_credential", ""},
		{"unique", []string{a}, "observed", a},
		{"duplicate", []string{a, a}, "observed", a},
		{"canonical_case", []string{a, strings.ToUpper(a)}, "observed", a},
		{"multiple", []string{a, b, a}, "ambiguous", ""},
		{"plaintext", []string{"must-not-retain-plaintext"}, "unverified", ""},
		{"invalid_hex", []string{strings.Repeat("z", 64)}, "unverified", ""},
		{"unknown_then_valid", []string{"!unverified-upstream-credential!", a}, "unverified", ""},
		{"valid_then_unknown", []string{a, "!unverified-upstream-credential!"}, "unverified", ""},
		{"bounded", []string{a, a, a, a, a, a, a, a, a}, "unverified", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New(time.Now(), 0)
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(With(t.Context(), c))
			req, _ = StartAttempt(req, 7, 0)
			RecordUpstreamCredentials(req.Context(), tc.fingerprints)
			req, tr := StartTransport(req)
			RecordUpstreamCredentials(req.Context(), tc.fingerprints)
			tr.Response(nil, nil)
			c.Finish(200, false)
			c.WhenFinished(func(s Snapshot) {
				require.Nil(t, s.Attempts[0].UpstreamCredential, "logical attempts must not duplicate physical identity")
				require.Equal(t, &UpstreamCredentialObservation{Status: tc.status, Source: "http_transport_handoff", Fingerprint: tc.fingerprint}, s.Attempts[1].UpstreamCredential)
				raw, err := json.Marshal(s)
				require.NoError(t, err)
				require.NotContains(t, string(raw), "must-not-retain-plaintext")
				require.NotContains(t, string(raw), "!unverified-upstream-credential!")
			})
		})
	}
}

func TestUpstreamCredentialChangesStayUnknown(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, tc := range []struct {
		name   string
		before []string
		after  []string
	}{
		{"redirect_strips", []string{a}, nil},
		{"fallback_replaces", []string{a}, []string{b}},
		{"missing_then_present", nil, []string{a}},
		{"unverified_then_valid", []string{"unknown"}, []string{a}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := NewWSTurn(time.Now(), 7, 0, [32]byte{1})
			w.RecordUpstreamCredentials(tc.before)
			w.RecordUpstreamCredentials(tc.after)
			w.RecordUpstreamCredentials(tc.before)
			w.Finish(nil, false, false)
			w.WhenFinished(func(s Snapshot) {
				require.Equal(t, &UpstreamCredentialObservation{Status: "changed_credentials", Source: "websocket_handshake_credentials"}, s.Attempts[0].UpstreamCredential)
			})
		})
	}
}

func TestUpstreamCredentialAttemptAndSnapshotIsolation(t *testing.T) {
	c := New(time.Now(), 0)
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for i, fp := range []string{a, a, b} {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(With(t.Context(), c))
		req, _ = StartAttempt(req, int64(i+1), 0)
		RecordAccountRoute(req.Context(), sha256.Sum256([]byte{byte(i)}))
		req, tr := StartTransport(req)
		RecordUpstreamCredentials(req.Context(), []string{fp})
		tr.update(func(a *Attempt) {
			reused := true
			a.Reused = &reused
		})
		tr.Response(nil, nil)
		RecordUpstreamCredentials(req.Context(), []string{b})
	}
	c.WhenFinished(func(s Snapshot) {
		require.Equal(t, a, s.Attempts[1].UpstreamCredential.Fingerprint)
		s.Attempts[1].UpstreamCredential.Fingerprint = b
		*s.Attempts[1].EndMS = 999999
		*s.Attempts[1].Reused = false
		s.Attempts[1].Events["observer_mutation"] = 1
	})
	c.WhenFinished(func(s Snapshot) {
		require.Equal(t, a, s.Attempts[1].UpstreamCredential.Fingerprint)
		require.NotEqual(t, 999999.0, *s.Attempts[1].EndMS)
		require.True(t, *s.Attempts[1].Reused)
		require.NotContains(t, s.Attempts[1].Events, "observer_mutation")
	})
	c.Finish(200, false)
	c.WhenFinished(func(s Snapshot) {
		require.Len(t, s.Attempts, 6)
		for i, fp := range []string{a, a, b} {
			require.Nil(t, s.Attempts[i*2].UpstreamCredential)
			require.Equal(t, fp, s.Attempts[i*2+1].UpstreamCredential.Fingerprint)
			digest := sha256.Sum256([]byte{byte(i)})
			require.Equal(t, hex.EncodeToString(digest[:]), s.Attempts[i*2+1].RouteFingerprint)
		}
	})
	var absent *WSTurnCapture
	absent.RecordUpstreamCredentials([]string{a})
	RecordUpstreamCredentials(t.Context(), []string{a})
}

func TestUpstreamCredentialConcurrentObservation(t *testing.T) {
	w := NewWSTurn(time.Now(), 7, 0, [32]byte{1})
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			fp := a
			if i%2 == 0 {
				fp = b
			}
			w.RecordUpstreamCredentials([]string{fp})
		})
	}
	wg.Wait()
	w.Finish(nil, false, false)
	w.RecordUpstreamCredentials([]string{a})
	w.WhenFinished(func(s Snapshot) {
		require.Equal(t, "changed_credentials", s.Attempts[0].UpstreamCredential.Status)
		require.Empty(t, s.Attempts[0].UpstreamCredential.Fingerprint)
	})
}
