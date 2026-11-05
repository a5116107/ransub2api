package slots

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRegionalUpstreamKeyResourceIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, kind, fingerprint string
		id                      int64
		valid                   bool
	}{
		{"upstream", "upstream_key", strings.Repeat("a", 64), 1, true},
		{"other_credential", "upstream_key", strings.Repeat("b", 64), 1, true},
		{"missing_fingerprint", "upstream_key", "", 1, false},
		{"raw_credential", "upstream_key", "supplier-fixture", 1, false},
		{"uppercase", "upstream_key", strings.Repeat("A", 64), 1, false},
		{"missing_registry_id", "upstream_key", strings.Repeat("a", 64), 0, false},
		{"downstream", "api_key", "", 1, true},
		{"account", "account", "", 1, true},
		{"user", "user", "", 1, true},
		{"downstream_with_upstream_identity", "api_key", strings.Repeat("a", 64), 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{"kind": tc.kind, "resource_id": tc.id, "fingerprint": tc.fingerprint})
			require.NoError(t, err)
			var resource Resource
			require.NoError(t, json.Unmarshal(raw, &resource))
			if tc.valid {
				require.NoError(t, resource.Validate())
				if tc.kind == "upstream_key" {
					encoded, err := json.Marshal(resource)
					require.NoError(t, err)
					require.Contains(t, string(encoded), tc.fingerprint)
				}
			} else {
				require.Error(t, resource.Validate())
			}
		})
	}
}
