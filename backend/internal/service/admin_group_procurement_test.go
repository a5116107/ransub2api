//go:build unit

package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProcurementPolicyAdminCreateEditAndLegacyOmission(t *testing.T) {
	repo := &groupRepoStubForAdmin{}
	svc := &adminServiceImpl{groupRepo: repo}
	created, err := svc.CreateGroup(t.Context(), &CreateGroupInput{
		Name: "procurement", Platform: PlatformOpenAI, RateMultiplier: 1,
		ProcurementPolicy: json.RawMessage(`{"version":1,"revision":0,"min_rate":"0.02","max_rate":"0.03"}`),
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, created.ProcurementPolicy.Revision)
	repo.getByID = created
	_, err = svc.UpdateGroup(t.Context(), created.ID, &UpdateGroupInput{Name: "renamed"})
	require.NoError(t, err)
	require.Nil(t, repo.updated.ProcurementPolicyExpectedRevision)
	require.Equal(t, "0.03", *repo.updated.ProcurementPolicy.MaxRate)
	_, err = svc.UpdateGroup(t.Context(), created.ID, &UpdateGroupInput{
		ProcurementPolicy: json.RawMessage(`{"version":1,"revision":0,"min_rate":null,"max_rate":null}`),
	})
	require.ErrorIs(t, err, ErrProcurementPolicyConflict)
	_, err = svc.UpdateGroup(t.Context(), created.ID, &UpdateGroupInput{
		ProcurementPolicy: json.RawMessage(`{"version":1,"revision":1,"min_rate":null,"max_rate":null}`),
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, *repo.updated.ProcurementPolicyExpectedRevision)
	require.EqualValues(t, 2, repo.updated.ProcurementPolicy.Revision)
	require.Equal(t, 1.0, repo.updated.RateMultiplier)
}
