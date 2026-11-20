//go:build unit

package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestProcurementManagementContracts(t *testing.T) {
	for _, raw := range []string{
		`null`, `{}`, `{"version":1,"revision":0,"mode":"manual"}`, `{"version":1,"revision":1,"mode":"automatic"}`,
		`{"version":1,"revision":1,"mode":"review","future":true}`, `{"version":1,"revision":1,"mode":"review","mode":"review"}`,
		`{"version":1,"revision":9007199254740992,"mode":"review"}`, `{"version":1,"revision":1.1,"mode":"manual"}`,
	} {
		var saved domain.ProcurementManagement
		require.Error(t, json.Unmarshal([]byte(raw), &saved), raw)
	}
	for _, raw := range []string{`{"version":1,"revision":1,"mode":"manual"}`, `{"version":1,"revision":9007199254740991,"mode":"review"}`} {
		var saved domain.ProcurementManagement
		require.NoError(t, json.Unmarshal([]byte(raw), &saved))
	}
	manual := `{"version":1,"mode":"manual","expected_revision":0}`
	_, err := ParseProcurementManagementCommand([]byte(manual))
	require.NoError(t, err)
	for _, raw := range []string{strings.Replace(manual, `"manual"`, `"automatic"`, 1), strings.Replace(manual, `"expected_revision":0`, `"expected_revision":null`, 1), strings.Replace(manual, `"expected_revision":0`, `"expected_revision":0,"expected_revision":0`, 1), strings.TrimSuffix(manual, "}") + `,"account_ids":[]}`, strings.Replace(manual, `"expected_revision":0`, `"expected_revision":9007199254740991`, 1)} {
		_, err := ParseProcurementManagementCommand([]byte(raw))
		require.ErrorIs(t, err, ErrProcurementManagementInvalid, raw)
	}
	for _, raw := range []string{`{}`, `{"account_ids":null}`, `{"account_ids":[]}`, `{"account_ids":[1,1]}`, `{"account_ids":[0]}`, `{"account_ids":[9007199254740992]}`, `{"account_ids":[1],"account_ids":[1]}`, `{"account_ids":[1],"future":true}`} {
		_, err := ParseProcurementManagementPreview([]byte(raw))
		require.ErrorIs(t, err, ErrProcurementManagementInvalid, raw)
	}
	ids, err := ParseProcurementManagementPreview([]byte(`{"account_ids":[3,1]}`))
	require.NoError(t, err)
	require.Equal(t, []int64{1, 3}, ids)
	good := `{"version":1,"mode":"review","expected_revision":0,"account_ids":[3,1],"observed_at":"2026-11-20T01:02:03Z","fingerprint":"` + strings.Repeat("a", 64) + `"}`
	_, err = ParseProcurementManagementCommand([]byte(good))
	require.NoError(t, err)
	for _, raw := range []string{strings.Replace(good, `[3,1]`, `[1,1]`, 1), strings.Replace(good, strings.Repeat("a", 64), strings.Repeat("g", 64), 1), strings.Replace(good, `"observed_at":"2026-11-20T01:02:03Z"`, `"observed_at":null`, 1), strings.TrimSuffix(good, "}") + `,"future":true}`} {
		_, err := ParseProcurementManagementCommand([]byte(raw))
		require.ErrorIs(t, err, ErrProcurementManagementInvalid, raw)
	}
}
