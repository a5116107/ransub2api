package domain

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProcurementSourceMappingCommandKeepsExternalGroupIdentityOpaque(t *testing.T) {
	var command ProcurementSourceMappingCommand
	err := json.Unmarshal([]byte(`{"version":1,"site_code":"site-a","site_name":"Site A","platform":"openai","external_group_id":"42","external_group_name":"Low tier","local_group_id":42,"account_id":7,"confirm":true}`), &command)
	require.NoError(t, err)
	require.Equal(t, "42", command.ExternalGroupID)
	require.EqualValues(t, 42, command.LocalGroupID)
}

func TestProcurementSourceMappingCommandRejectsAmbiguousOrInvalidClaims(t *testing.T) {
	base := `{"version":1,"site_code":"site-a","site_name":"Site A","platform":"openai","external_group_id":"group-1","local_group_id":42,"account_id":7,"confirm":true}`
	tests := []struct {
		name string
		raw  string
	}{
		{name: "numeric external ids are strings", raw: `{"version":1,"site_code":"site-a","site_name":"Site A","platform":"openai","external_group_id":42,"local_group_id":42,"account_id":7,"confirm":true}`},
		{name: "unknown platform", raw: strings.ReplaceAll(base, `"openai"`, `"unknown-provider"`)},
		{name: "whitespace external id", raw: strings.ReplaceAll(base, `"group-1"`, `" group-1 "`)},
		{name: "zero local group", raw: strings.ReplaceAll(base, `"local_group_id":42`, `"local_group_id":0`)},
		{name: "duplicate field", raw: strings.ReplaceAll(base, `"confirm":true`, `"confirm":true,"confirm":true`)},
		{name: "unknown field", raw: strings.ReplaceAll(base, `"confirm":true`, `"confirm":true,"verified":true`)},
		{name: "confirmation required", raw: strings.ReplaceAll(base, `"confirm":true`, `"confirm":false`)},
		{name: "null optional name", raw: strings.ReplaceAll(base, `"confirm":true`, `"confirm":true,"external_group_name":null`)},
		{name: "unsafe local id", raw: strings.ReplaceAll(base, `"local_group_id":42`, `"local_group_id":9007199254740992`)},
		{name: "create cannot carry stale revision", raw: strings.ReplaceAll(base, `"confirm":true`, `"confirm":true,"expected_revision":1`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var command ProcurementSourceMappingCommand
			require.Error(t, json.Unmarshal([]byte(tt.raw), &command))
		})
	}
}

func TestProcurementSourceMappingStateCommandsRequireRevisionAndConfirmation(t *testing.T) {
	var enabled ProcurementSourceMappingEnableCommand
	require.Error(t, json.Unmarshal([]byte(`{"version":1,"expected_revision":0,"enabled":false,"confirm":true}`), &enabled))
	require.Error(t, json.Unmarshal([]byte(`{"version":1,"expected_revision":2,"enabled":false,"confirm":false}`), &enabled))
	require.Error(t, json.Unmarshal([]byte(`{"version":1,"expected_revision":2,"enabled":null,"confirm":true}`), &enabled))
	require.NoError(t, json.Unmarshal([]byte(`{"version":1,"expected_revision":2,"enabled":false,"confirm":true}`), &enabled))

	var deleted ProcurementSourceMappingDeleteCommand
	require.Error(t, json.Unmarshal([]byte(`{"version":1,"expected_revision":2,"confirm":true,"force":true}`), &deleted))
	require.NoError(t, json.Unmarshal([]byte(`{"version":1,"expected_revision":2,"confirm":true}`), &deleted))
	var rename ProcurementSupplierSiteRenameCommand
	require.Error(t, json.Unmarshal([]byte(`{"version":1,"expected_revision":2,"name":" ","confirm":true}`), &rename))
	require.Error(t, json.Unmarshal([]byte(`{"version":1,"expected_revision":9007199254740991,"name":"Supplier","confirm":true}`), &rename))
	require.NoError(t, json.Unmarshal([]byte(`{"version":1,"expected_revision":2,"name":"Supplier","confirm":true}`), &rename))
}
