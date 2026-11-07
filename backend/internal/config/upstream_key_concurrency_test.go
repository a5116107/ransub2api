package config

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpstreamKeyConcurrencyValidation(t *testing.T) {
	fingerprint := UpstreamKeyFingerprint("supplier-fixture")
	require.Equal(t, fingerprint, UpstreamKeyFingerprint("  supplier-fixture\n"))
	for _, tc := range []struct {
		name   string
		policy UpstreamKeyConcurrencyConfig
		valid  bool
	}{
		{"disabled", UpstreamKeyConcurrencyConfig{}, true},
		{"configured", UpstreamKeyConcurrencyConfig{Revision: 1, Limits: map[string]int{fingerprint: 4}}, true},
		{"versioned_removal", UpstreamKeyConcurrencyConfig{Revision: 2}, true},
		{"no_revision", UpstreamKeyConcurrencyConfig{Limits: map[string]int{fingerprint: 4}}, false},
		{"negative_revision", UpstreamKeyConcurrencyConfig{Revision: -1}, false},
		{"imprecise_revision", UpstreamKeyConcurrencyConfig{Revision: 9007199254740992}, false},
		{"raw_key", UpstreamKeyConcurrencyConfig{Revision: 1, Limits: map[string]int{"supplier-fixture": 4}}, false},
		{"uppercase", UpstreamKeyConcurrencyConfig{Revision: 1, Limits: map[string]int{strings.ToUpper(fingerprint): 4}}, false},
		{"zero_limit", UpstreamKeyConcurrencyConfig{Revision: 1, Limits: map[string]int{fingerprint: 0}}, false},
		{"negative_limit", UpstreamKeyConcurrencyConfig{Revision: 1, Limits: map[string]int{fingerprint: -1}}, false},
		{"oversized_limit", UpstreamKeyConcurrencyConfig{Revision: 1, Limits: map[string]int{fingerprint: 1000001}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.valid {
				require.NoError(t, tc.policy.Validate())
			} else {
				require.Error(t, tc.policy.Validate())
			}
		})
	}
}

func TestUpstreamBasicCredentialFingerprints(t *testing.T) {
	fingerprint := func(user, password string) string {
		return fmt.Sprintf("%x", sha256.Sum256([]byte("\x00http-basic\x00"+user+":"+password)))
	}
	encoded := func(value string) string { return base64.StdEncoding.EncodeToString([]byte(value)) }
	for _, tc := range []struct {
		name, header, user, password string
		valid                        bool
	}{
		{"ordinary", "Basic " + encoded("monitor:shared-fixture"), "monitor", "shared-fixture", true},
		{"case insensitive scheme", "bAsIc " + encoded("monitor:shared-fixture"), "monitor", "shared-fixture", true},
		{"wire header whitespace", " \tBasic " + encoded("monitor:shared-fixture") + " \t", "monitor", "shared-fixture", true},
		{"empty username", "Basic " + encoded(":password"), "", "password", true},
		{"empty password", "Basic " + encoded("user:"), "user", "", true},
		{"password colon", "Basic " + encoded("user:pass:word"), "user", "pass:word", true},
		{"credential spaces", "Basic " + encoded(" user:password "), " user", "password ", true},
		{"unicode", "Basic " + encoded("\u7528\u6236:\u5bc6\u78bc"), "\u7528\u6236", "\u5bc6\u78bc", true},
		{"invalid base64", "Basic !!!", "", "", false},
		{"missing colon", "Basic " + encoded("user"), "", "", false},
		{"missing token", "Basic ", "", "", false},
		{"unknown scheme", "Custom " + encoded("monitor:shared-fixture"), "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://fixture.invalid", nil)
			req.Header["authorization"] = []string{tc.header}
			got := UpstreamCredentialFingerprints(req)
			//nolint:staticcheck // Verify extraction does not rewrite raw header casing or values.
			require.Equal(t, []string{tc.header}, req.Header["authorization"])
			if tc.valid {
				require.Equal(t, []string{fingerprint(tc.user, tc.password)}, got)
			} else {
				require.Empty(t, got)
			}
		})
	}
	req := httptest.NewRequest(http.MethodPost, "http://fixture.invalid", nil)
	req.Header["authorization"] = []string{"Basic " + encoded("monitor:shared-fixture")}
	req.Header["AUTHORIZATION"] = []string{"bAsIc " + encoded("monitor:shared-fixture")}
	req.Header["Authorization"] = []string{"Bearer monitor:shared-fixture"}
	require.ElementsMatch(t, []string{fingerprint("monitor", "shared-fixture"), UpstreamKeyFingerprint("monitor:shared-fixture")}, UpstreamCredentialFingerprints(req))
	require.NotEqual(t, fingerprint("user", "password"), fingerprint("user", "password "))
	require.NotEqual(t, fingerprint("user", "password"), fingerprint("other", "password"))
	require.Equal(t, fingerprint("monitor", "shared-fixture"), UpstreamBasicAuthFingerprint("monitor", "shared-fixture"))
	require.Equal(t, fingerprint("user", "password "), UpstreamBasicAuthFingerprint("user", "password "))
	require.Empty(t, UpstreamBasicAuthFingerprint("user:name", "password"))
}

func TestUpstreamKeyConcurrencyRequiresMatchingRegionalAuthority(t *testing.T) {
	for _, regional := range []string{"redis", "concurrency", "scheduler", "both"} {
		t.Run(regional, func(t *testing.T) {
			resetViperWithJWTSecret(t)
			cfg, err := Load()
			require.NoError(t, err)
			cfg.Gateway.UpstreamKeyConcurrency = UpstreamKeyConcurrencyConfig{Revision: 1, Limits: map[string]int{UpstreamKeyFingerprint("supplier-fixture"): 2}}
			if regional == "concurrency" || regional == "both" {
				cfg.Runtime.RegionalConcurrencyFile = "fixture.json"
			}
			if regional == "scheduler" || regional == "both" {
				cfg.Runtime.RegionalSchedulerFile = "fixture.json"
			}
			if regional == "scheduler" {
				require.EqualError(t, cfg.Validate(), "gateway.upstream_key_concurrency requires regional concurrency authority when using a regional scheduler")
			} else {
				require.NoError(t, cfg.Validate())
			}
		})
	}
}

func TestUpstreamCredentialFingerprintsRawHeaderCasing(t *testing.T) {
	for _, name := range []string{"authorization", "aUtHoRiZaTiOn", "x-api-key", "api-key", "x-goog-api-key"} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://fixture.invalid/v1", nil)
			value := "supplier-fixture"
			if strings.EqualFold(name, "authorization") {
				value = "Bearer " + value
			}
			req.Header[name] = []string{value}
			require.Equal(t, []string{UpstreamKeyFingerprint("supplier-fixture")}, UpstreamCredentialFingerprints(req))
		})
	}
	req := httptest.NewRequest(http.MethodPost, "http://fixture.invalid/v1?api_key=supplier-fixture", nil)
	req.Header["authorization"] = []string{"Bearer supplier-fixture"}
	req.Header["Authorization"] = []string{"Token supplier-fixture"}
	req.Header["x-api-key"] = []string{"supplier-fixture"}
	require.Equal(t, []string{UpstreamKeyFingerprint("supplier-fixture")}, UpstreamCredentialFingerprints(req))
	req.Header["AUTHORIZATION"] = []string{"Bearer second-fixture"}
	req.Header["X-Api-Key"] = []string{"third-fixture"}
	for range 10 {
		require.Equal(t, []string{UpstreamKeyFingerprint("second-fixture"), UpstreamKeyFingerprint("supplier-fixture"), UpstreamKeyFingerprint("third-fixture")}, UpstreamCredentialFingerprints(req))
	}
	//nolint:staticcheck // Verify the deliberately non-canonical on-wire key is unchanged.
	require.Equal(t, []string{"Bearer supplier-fixture"}, req.Header["authorization"], "extraction must not rewrite the wire")
}

func TestUpstreamSigV4CredentialScopeBoundaries(t *testing.T) {
	const id = "AKIDEXAMPLE12345"
	const valid = "AWS4-HMAC-SHA256 Credential=" + id + "/20261107/us-east-1/bedrock/aws4_request, SignedHeaders=host;x-amz-date, Signature=fixture"
	for _, tc := range []struct {
		name, value string
		want        bool
	}{
		{"valid", valid, true},
		{"lowercase scheme", strings.Replace(valid, "AWS4-HMAC-SHA256", "aws4-hmac-sha256", 1), true},
		{"missing credential", "AWS4-HMAC-SHA256 SignedHeaders=host, Signature=fixture", false},
		{"missing scope", "AWS4-HMAC-SHA256 Credential=" + id, false},
		{"wrong terminator", strings.Replace(valid, "aws4_request", "wrong", 1), false},
		{"bad date", strings.Replace(valid, "20261107", "2026a107", 1), false},
		{"bad access id", strings.Replace(valid, id, "bad id", 1), false},
		{"duplicate", valid + ", Credential=" + id + "/20261107/us-east-1/bedrock/aws4_request", false},
		{"empty duplicate", "AWS4-HMAC-SHA256 Credential=, Credential=" + id + "/20261107/us-east-1/bedrock/aws4_request", false},
		{"v4a unsupported", strings.Replace(valid, "AWS4-HMAC-SHA256", "AWS4-ECDSA-P256-SHA256", 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://fixture.invalid", nil)
			req.Header["authorization"] = []string{tc.value}
			got := UpstreamCredentialFingerprints(req)
			if tc.want {
				require.Equal(t, []string{UpstreamAWSAccessKeyFingerprint(id)}, got)
			} else {
				require.Empty(t, got)
			}
		})
	}
	req := httptest.NewRequest(http.MethodPost, "http://fixture.invalid?key="+id, nil)
	req.Header["authorization"] = []string{valid}
	req.Header["Authorization"] = []string{"Bearer " + id}
	req.Header["x-api-key"] = []string{id}
	require.ElementsMatch(t, []string{UpstreamAWSAccessKeyFingerprint(id), UpstreamKeyFingerprint(id)}, UpstreamCredentialFingerprints(req))
}
