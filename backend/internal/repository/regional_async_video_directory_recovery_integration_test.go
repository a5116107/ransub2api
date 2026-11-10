//go:build integration

package repository

import (
	"github.com/Wei-Shaw/sub2api/internal/regional/video"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/regional/control"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestRegionalAsyncVideoDirectoryCompletedReleaseRollback(t *testing.T) {
	t.Setenv("SUB2API_REGIONAL_TEST_DSN", integrationPostgresDSN)
	for _, mode := range []string{"primary", "regional"} {
		t.Run(mode, func(t *testing.T) {
			rdb := regionalKeyRedis(t)
			cfg := upstreamKeyTestConfig(1, 1)
			authority := regionalAsyncVideoAuthority(t, cfg, rdb, mode)
			if mode == "primary" {
				authority.runtimeConfig = &RegionalConcurrencyRuntimeConfig{Listen: "fixture", Scope: control.Scope{Region: "us"}}
			} else {
				authority.runtimeConfig = &RegionalConcurrencyRuntimeConfig{Endpoint: "fixture", Region: authority.local.Region}
			}
			upstream := liveKeyTestUpstream(t, cfg, rdb, authority)
			owner := asyncVideoLeaseOwner()
			handoff := service.NewAsyncVideoKeyHandoff(owner)
			_, finish, err := upstream.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, handoff))
			require.NoError(t, err)
			require.NoError(t, upstream.BindAsyncVideoKeyLease(t.Context(), handoff, "completed-rollback"))
			finish(nil, nil)
			_, err = authority.database().ExecContext(t.Context(), `CREATE FUNCTION reject_video_receipt() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture receipt unavailable'; END $$; CREATE TRIGGER reject_video_receipt BEFORE INSERT ON regional_async_video_completed_tasks FOR EACH ROW EXECUTE FUNCTION reject_video_receipt()`)
			require.NoError(t, err)
			require.Error(t, upstream.ReleaseAsyncVideoKeyLeaseByTask(t.Context(), owner, "completed-rollback"))
			require.NoError(t, upstream.CheckPersistentUpstreamKeySlots(t.Context(), handoff.Lease()), "receipt failure must roll back every slot release")
			var active, receipts int
			require.NoError(t, authority.database().QueryRowContext(t.Context(), `SELECT count(*) FROM regional_async_video_key_leases WHERE member=$1 AND fenced`, handoff.Member()).Scan(&active))
			require.Equal(t, 1, active)
			require.NoError(t, authority.database().QueryRowContext(t.Context(), `SELECT count(*) FROM regional_async_video_completed_tasks`).Scan(&receipts))
			require.Zero(t, receipts)
			_, err = authority.database().ExecContext(t.Context(), `DROP TRIGGER reject_video_receipt ON regional_async_video_completed_tasks; DROP FUNCTION reject_video_receipt()`)
			require.NoError(t, err)
			recovered, err := upstream.RecoverAsyncVideoKeyReleases(t.Context(), 8)
			require.NoError(t, err)
			require.Equal(t, 1, recovered)
			require.ErrorIs(t, upstream.CheckPersistentUpstreamKeySlots(t.Context(), handoff.Lease()), service.ErrUpstreamKeyLease)
			require.NoError(t, authority.database().QueryRowContext(t.Context(), `SELECT count(*) FROM regional_async_video_key_leases WHERE member=$1`, handoff.Member()).Scan(&active))
			require.Zero(t, active, "the original active task record is deleted after confirmed release")
			var raw string
			require.NoError(t, authority.database().QueryRowContext(t.Context(), `SELECT row_to_json(t)::text FROM regional_async_video_completed_tasks t WHERE authority=$1 AND task_key=$2`, authority.persistentUpstreamKeyAuthority(), video.TaskKey(owner, "completed-rollback")).Scan(&raw))
			require.NotContains(t, raw, "supplier-fixture")
			require.NotContains(t, raw, config.UpstreamKeyFingerprint("supplier-fixture"))
			require.NotContains(t, raw, `"lease"`)
			require.NoError(t, upstream.ReleaseAsyncVideoKeyLeaseByTask(t.Context(), owner, "completed-rollback"))
		})
	}
}
