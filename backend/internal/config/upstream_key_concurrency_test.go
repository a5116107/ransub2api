package config

import (
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
