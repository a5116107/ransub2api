package video

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTaskKeyPreservesV2IndexAndPrincipalIsolation(t *testing.T) {
	owner := Owner{UserID: 9007199254740993, APIKeyID: 11, GroupID: 13, AccountID: 17, Provider: "grok", RouteFingerprint: [32]byte{1}}
	key := TaskKey(owner, "task-001")
	require.Equal(t, "concurrency:upstream_key:async_video:task:v2:32ec39a657f452a8eca76c3f29341d35dbe7a66eb5decbc84160843ff0ba863f", key)
	changedRoute := owner
	changedRoute.AccountID++
	changedRoute.RouteFingerprint[0]++
	require.Equal(t, key, TaskKey(changedRoute, "task-001"), "another account must conflict with the original task index")
	for _, change := range []func(*Owner){
		func(o *Owner) { o.UserID-- },
		func(o *Owner) { o.APIKeyID++ },
		func(o *Owner) { o.GroupID++ },
		func(o *Owner) { o.Provider = "seedance" },
	} {
		other := owner
		change(&other)
		require.NotEqual(t, key, TaskKey(other, "task-001"))
	}
	require.Empty(t, TaskKey(Owner{}, "task-001"))
}
