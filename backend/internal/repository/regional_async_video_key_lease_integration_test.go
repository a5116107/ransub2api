//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/Wei-Shaw/sub2api/internal/regional/slots"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func regionalAsyncVideoAuthority(t *testing.T, cfg *config.Config, rdb *redis.Client, mode string) *regionalConcurrencyCache {
	t.Helper()
	primary, local, _ := regionalKeyFixture(t, cfg, rdb, 0)
	if mode == "primary" {
		return primary
	}
	snapshot, err := primary.primary.ReadUpstreamPolicy(t.Context())
	require.NoError(t, err)
	require.NoError(t, local.local.InstallUpstreamPolicy(t.Context(), snapshot))
	for fingerprint, id := range snapshot.Resources {
		grant := slots.Grant{LeaseID: uuid.NewString(), Region: local.local.Region,
			Resource: slots.Resource{Kind: "upstream_key", ID: id, Fingerprint: fingerprint},
			Slots:    snapshot.Policy.Limits[fingerprint], Revision: 1,
			IssuedAt: snapshot.IssuedAt.Truncate(time.Microsecond), ExpiresAt: snapshot.IssuedAt.Add(time.Minute).Truncate(time.Microsecond)}
		require.NoError(t, primary.primary.Issue(t.Context(), grant))
		require.NoError(t, local.local.Install(t.Context(), grant))
	}
	return local
}

func TestRegionalAsyncVideoKeyLeaseHandoffAndRecovery(t *testing.T) {
	for _, mode := range []string{"primary", "regional"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("SUB2API_REGIONAL_TEST_DSN", integrationPostgresDSN)
			rdb := regionalKeyRedis(t)
			cfg := upstreamKeyTestConfig(1, 1)
			primary := regionalAsyncVideoAuthority(t, cfg, rdb, mode)
			upstream := liveKeyTestUpstream(t, cfg, rdb, primary)
			require.ErrorIs(t, upstream.ReleaseAsyncVideoKeyLeaseByMember(t.Context(), "invalid-member"), service.ErrUpstreamKeyPolicy)
			owner := asyncVideoLeaseOwner()
			handoff := service.NewAsyncVideoKeyHandoff(owner)
			request := asyncVideoLeaseRequest(t, handoff)
			_, finish, err := upstream.BeginUpstreamKeyRequest(request)
			require.NoError(t, err)
			require.NotNil(t, handoff.Lease())
			expectedAuthority := "regional:primary"
			if mode == "regional" {
				expectedAuthority = "regional:local:us"
			}
			require.Equal(t, expectedAuthority, handoff.Lease().Authority)
			require.NoError(t, upstream.BindAsyncVideoKeyLease(t.Context(), handoff, "regional-task"))
			finish(nil, nil)

			resolved, err := upstream.ResolveAsyncVideoKeyOwner(t.Context(), service.AsyncVideoKeyOwner{
				UserID: owner.UserID, APIKeyID: owner.APIKeyID, GroupID: owner.GroupID, Provider: owner.Provider,
			}, "regional-task")
			require.NoError(t, err)
			require.Equal(t, owner, *resolved)

			competitor := service.NewAsyncVideoKeyHandoff(owner)
			_, err = upstream.AcquirePersistentUpstreamKeySlots(t.Context(), asyncVideoLeaseRequest(t, competitor))
			require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)

			restartedCache, err := NewRegionalConcurrencyCache(primary.ConcurrencyCache, primary.primary, primary.local)
			require.NoError(t, err)
			restarted := liveKeyTestUpstream(t, cfg, rdb, restartedCache)
			require.NoError(t, restarted.ReleaseAsyncVideoKeyLeaseByTask(t.Context(), owner, "regional-task"))
			require.Eventually(t, func() bool {
				loaded, loadErr := restarted.LoadAsyncVideoKeyLeaseByTask(context.Background(), owner, "regional-task")
				return loadErr == nil && loaded == nil
			}, time.Second, 10*time.Millisecond, "a missing task record is an idempotent lookup")

			competitor = service.NewAsyncVideoKeyHandoff(owner)
			lease, err := upstream.AcquirePersistentUpstreamKeySlots(t.Context(), asyncVideoLeaseRequest(t, competitor))
			require.NoError(t, err)
			require.NotNil(t, lease)
			require.NoError(t, upstream.ReleaseAsyncVideoKeyLease(t.Context(), competitor))

			unknown := service.NewAsyncVideoKeyHandoff(owner)
			_, finish, err = upstream.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, unknown))
			require.NoError(t, err)
			finish(nil, errors.New("submission interrupted"))
			require.NoError(t, restarted.BindAsyncVideoKeyLeaseByMember(t.Context(), unknown.Member(), "recovered-task"))
			wrongOwner := owner
			wrongOwner.AccountID++
			_, err = restarted.LoadAsyncVideoKeyLeaseByTask(t.Context(), wrongOwner, "recovered-task")
			require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy)
			require.NoError(t, restarted.ReleaseAsyncVideoKeyLeaseByTask(t.Context(), owner, "recovered-task"))

			astra := &astraRoutingUpstream{delegate: restarted}
			astraHandoff := service.NewAsyncVideoKeyHandoff(owner)
			_, finish, err = astra.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, astraHandoff))
			require.NoError(t, err)
			require.NoError(t, astra.BindAsyncVideoKeyLease(t.Context(), astraHandoff, "astra-task"))
			finish(nil, nil)
			require.NoError(t, astra.ReleaseAsyncVideoKeyLeaseByTask(t.Context(), owner, "astra-task"))
		})
	}
}

func TestRegionalAsyncVideoAcquireWaitsForReleaseFence(t *testing.T) {
	for _, mode := range []string{"primary", "regional"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("SUB2API_REGIONAL_TEST_DSN", integrationPostgresDSN)
			rdb := regionalKeyRedis(t)
			cfg := upstreamKeyTestConfig(1, 1)
			primary := regionalAsyncVideoAuthority(t, cfg, rdb, mode)
			upstream := liveKeyTestUpstream(t, cfg, rdb, primary)
			handoff := service.NewAsyncVideoKeyHandoff(asyncVideoLeaseOwner())
			fingerprint := config.UpstreamKeyFingerprint("supplier-fixture")
			resource, limited, err := primary.upstreamKeyResource(t.Context(), upstream.upstreamKeyLimiter.policy.Revision, upstream.upstreamKeyLimiter.digest, fingerprint)
			require.NoError(t, err)
			require.True(t, limited)
			lease := &service.PersistentUpstreamKeyLease{Authority: primary.persistentUpstreamKeyAuthority(), Member: handoff.Member(),
				Fingerprints: []string{fingerprint}, Resources: []service.PersistentUpstreamKeyResource{{Fingerprint: fingerprint, ResourceID: resource.ID}}}
			handoff.SetLease(lease)
			require.NoError(t, upstream.PersistAsyncVideoKeyLease(t.Context(), handoff))

			tx, err := primary.database().BeginTx(t.Context(), nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = tx.Rollback() })
			var member string
			require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT member FROM regional_async_video_key_leases WHERE member=$1 FOR UPDATE`, handoff.Member()).Scan(&member))
			result := make(chan error, 1)
			go func() {
				_, _, takeErr := upstream.upstreamKeyLimiter.take(t.Context(), fingerprint, handoff.Member(), "video")
				result <- takeErr
			}()
			select {
			case err := <-result:
				t.Fatalf("acquisition must wait for the task row lock, got %v", err)
			case <-time.After(150 * time.Millisecond):
			}
			_, err = tx.ExecContext(t.Context(), `UPDATE regional_async_video_key_leases SET fenced=true,release_due_at=clock_timestamp() WHERE member=$1`, member)
			require.NoError(t, err)
			require.NoError(t, tx.Commit())
			select {
			case err := <-result:
				require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy)
			case <-time.After(3 * time.Second):
				t.Fatal("acquisition did not finish after the fence committed")
			}
			require.NoError(t, upstream.ReleaseAsyncVideoKeyLeaseByMember(t.Context(), member))
			count, err := primary.count(t.Context(), resource.Kind, resource.ID)
			require.NoError(t, err)
			require.Zero(t, count, "no delayed acquisition may survive release")
		})
	}
}

func TestRegionalAsyncVideoRecoveryPreservesUnknownAcrossInstances(t *testing.T) {
	for _, mode := range []string{"primary", "regional"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("SUB2API_REGIONAL_TEST_DSN", integrationPostgresDSN)
			rdb := regionalKeyRedis(t)
			cfg := upstreamKeyTestConfig(1, 2)
			primary := regionalAsyncVideoAuthority(t, cfg, rdb, mode)
			upstream := liveKeyTestUpstream(t, cfg, rdb, primary)
			completed := service.NewAsyncVideoKeyHandoff(asyncVideoLeaseOwner())
			_, finish, err := upstream.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, completed))
			require.NoError(t, err)
			require.NoError(t, upstream.BindAsyncVideoKeyLease(t.Context(), completed, "completed-task"))
			finish(nil, nil)
			unknown := service.NewAsyncVideoKeyHandoff(asyncVideoLeaseOwner())
			_, finish, err = upstream.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, unknown))
			require.NoError(t, err)
			finish(nil, errors.New("provider result unknown"))
			_, err = primary.database().ExecContext(t.Context(), `UPDATE regional_async_video_key_leases SET fenced=true,release_due_at=clock_timestamp() WHERE member=$1`, completed.Member())
			require.NoError(t, err)

			restarted := liveKeyTestUpstream(t, cfg, rdb, primary)
			workers := []*httpUpstreamService{upstream, restarted}
			start := make(chan struct{})
			type recoveryResult struct {
				count int
				err   error
			}
			results := make(chan recoveryResult, len(workers))
			var group sync.WaitGroup
			for _, worker := range workers {
				group.Add(1)
				go func() {
					defer group.Done()
					<-start
					count, recoveryErr := worker.RecoverAsyncVideoKeyReleases(t.Context(), 4)
					results <- recoveryResult{count: count, err: recoveryErr}
				}()
			}
			close(start)
			group.Wait()
			close(results)
			total := 0
			for result := range results {
				require.NoError(t, result.err)
				total += result.count
			}
			require.Equal(t, 1, total, "only the instance that deleted the record counts the recovery")
			require.ErrorIs(t, upstream.CheckPersistentUpstreamKeySlots(t.Context(), completed.Lease()), service.ErrUpstreamKeyLease)
			require.NoError(t, upstream.CheckPersistentUpstreamKeySlots(t.Context(), unknown.Lease()), "unknown submission must remain held")
			var state string
			require.NoError(t, primary.database().QueryRowContext(t.Context(), `SELECT state FROM regional_async_video_key_leases WHERE member=$1`, unknown.Member()).Scan(&state))
			require.Equal(t, asyncVideoLeaseStateUnknown, state)
			require.NoError(t, restarted.ReleaseAsyncVideoKeyLeaseByMember(t.Context(), unknown.Member()))
		})
	}
}

func TestRegionalAsyncVideoRecoveryRollsBackPartialRelease(t *testing.T) {
	for _, mode := range []string{"primary", "regional"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("SUB2API_REGIONAL_TEST_DSN", integrationPostgresDSN)
			rdb := regionalKeyRedis(t)
			cfg := upstreamKeyTestConfig(1, 1)
			second := config.UpstreamKeyFingerprint("second-fixture")
			cfg.Gateway.UpstreamKeyConcurrency.Limits[second] = 1
			primary := regionalAsyncVideoAuthority(t, cfg, rdb, mode)
			upstream := liveKeyTestUpstream(t, cfg, rdb, primary)
			handoff := service.NewAsyncVideoKeyHandoff(asyncVideoLeaseOwner())
			request := asyncVideoLeaseRequest(t, handoff)
			request.Header.Set("X-Api-Key", "second-fixture")
			_, finish, err := upstream.BeginUpstreamKeyRequest(request)
			require.NoError(t, err)
			require.Len(t, handoff.Lease().Resources, 2)
			finish(nil, nil)
			_, err = primary.database().ExecContext(t.Context(), `UPDATE regional_async_video_key_leases SET fenced=true,release_due_at=clock_timestamp() WHERE member=$1`, handoff.Member())
			require.NoError(t, err)
			resources := append([]service.PersistentUpstreamKeyResource(nil), handoff.Lease().Resources...)
			sort.Slice(resources, func(i, j int) bool { return resources[i].ResourceID < resources[j].ResourceID })
			lock, err := primary.database().BeginTx(t.Context(), nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = lock.Rollback() })
			var lockedID int64
			require.NoError(t, lock.QueryRowContext(t.Context(), `SELECT resource_id FROM regional_slot_budgets WHERE kind='upstream_key' AND resource_id=$1 FOR UPDATE`, resources[1].ResourceID).Scan(&lockedID))
			short, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
			defer cancel()
			_, err = upstream.RecoverAsyncVideoKeyReleases(short, 4)
			require.Error(t, err, "a stalled release must not partially commit")
			require.NoError(t, upstream.CheckPersistentUpstreamKeySlots(t.Context(), handoff.Lease()), "both resources must remain held after rollback")
			require.NoError(t, lock.Rollback())
			recovered, err := upstream.RecoverAsyncVideoKeyReleases(t.Context(), 4)
			require.NoError(t, err)
			require.Equal(t, 1, recovered)
			require.ErrorIs(t, upstream.CheckPersistentUpstreamKeySlots(t.Context(), handoff.Lease()), service.ErrUpstreamKeyLease)
		})
	}
}

func TestRegionalAsyncVideoRecoveryRejectsMismatchedResources(t *testing.T) {
	for _, mode := range []string{"primary", "regional"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("SUB2API_REGIONAL_TEST_DSN", integrationPostgresDSN)
			rdb := regionalKeyRedis(t)
			cfg := upstreamKeyTestConfig(1, 1)
			cfg.Gateway.UpstreamKeyConcurrency.Limits[config.UpstreamKeyFingerprint("second-fixture")] = 1
			primary := regionalAsyncVideoAuthority(t, cfg, rdb, mode)
			upstream := liveKeyTestUpstream(t, cfg, rdb, primary)
			handoff := service.NewAsyncVideoKeyHandoff(asyncVideoLeaseOwner())
			request := asyncVideoLeaseRequest(t, handoff)
			request.Header.Set("X-Api-Key", "second-fixture")
			_, finish, err := upstream.BeginUpstreamKeyRequest(request)
			require.NoError(t, err)
			finish(nil, nil)
			original := handoff.Lease()
			require.Len(t, original.Resources, 2)
			damaged := *original
			damaged.Resources = append([]service.PersistentUpstreamKeyResource(nil), original.Resources...)
			damaged.Resources[0].ResourceID, damaged.Resources[1].ResourceID = damaged.Resources[1].ResourceID, damaged.Resources[0].ResourceID
			encoded, err := json.Marshal(damaged)
			require.NoError(t, err)
			_, err = primary.database().ExecContext(t.Context(), `UPDATE regional_async_video_key_leases SET lease=$2::jsonb,fenced=true,release_due_at=clock_timestamp() WHERE member=$1`, handoff.Member(), string(encoded))
			require.NoError(t, err)

			recovered, err := upstream.RecoverAsyncVideoKeyReleases(t.Context(), 4)
			require.ErrorIs(t, err, slots.ErrConflict)
			require.Zero(t, recovered)
			require.NoError(t, upstream.CheckPersistentUpstreamKeySlots(t.Context(), original), "a malformed record must preserve every original slot")
			var deferred bool
			require.NoError(t, primary.database().QueryRowContext(t.Context(), `SELECT release_due_at>clock_timestamp() FROM regional_async_video_key_leases WHERE member=$1`, handoff.Member()).Scan(&deferred))
			require.True(t, deferred, "a malformed record must back off")
			recovered, err = upstream.RecoverAsyncVideoKeyReleases(t.Context(), 4)
			require.NoError(t, err)
			require.Zero(t, recovered)
		})
	}
}

func TestRegionalAsyncVideoLocalAuthorityLifecycle(t *testing.T) {
	t.Setenv("SUB2API_REGIONAL_TEST_DSN", integrationPostgresDSN)
	rdb := regionalKeyRedis(t)
	cfg := upstreamKeyTestConfig(1, 2)
	primary, local, grant := regionalKeyFixture(t, cfg, rdb, 1)
	upstream := liveKeyTestUpstream(t, cfg, rdb, local)
	require.True(t, upstream.SupportsAsyncVideoKeyGuard())
	require.Nil(t, local.primary, "the request replica has no primary authority")
	require.Nil(t, local.brokerClient, "request-time ownership must work without a control client")
	owner := asyncVideoLeaseOwner()
	handoff := service.NewAsyncVideoKeyHandoff(owner)
	_, finish, err := upstream.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, handoff))
	require.NoError(t, err)
	require.Equal(t, "regional:local:us", handoff.Lease().Authority)
	finish(nil, io.ErrUnexpectedEOF)
	var state string
	require.NoError(t, local.database().QueryRowContext(t.Context(), `SELECT state FROM regional_async_video_key_leases WHERE member=$1`, handoff.Member()).Scan(&state))
	require.Equal(t, asyncVideoLeaseStateUnknown, state)
	// The outstanding regional grant and a primary request consume the same global budget.
	primaryUpstream := liveKeyTestUpstream(t, cfg, rdb, primary)
	_, release, err := primaryUpstream.AcquireUpstreamKeySlots(t.Context(), handoff.Lease().Fingerprints)
	require.NoError(t, err)
	t.Cleanup(release)
	_, _, err = primaryUpstream.AcquireUpstreamKeySlots(t.Context(), handoff.Lease().Fingerprints)
	require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)
	_, _, err = upstream.AcquireUpstreamKeySlots(t.Context(), handoff.Lease().Fingerprints)
	require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)
	restartedCache, err := NewRegionalConcurrencyCache(local.ConcurrencyCache, nil, local.local)
	require.NoError(t, err)
	restarted := liveKeyTestUpstream(t, cfg, rdb, restartedCache)
	require.NoError(t, restarted.CheckPersistentUpstreamKeySlots(t.Context(), handoff.Lease()))
	// Only explicit provider reconciliation supplies the recovered task ID.
	require.NoError(t, restarted.BindAsyncVideoKeyLeaseByMember(t.Context(), handoff.Member(), "local-task"))
	lease, err := restarted.LoadAsyncVideoKeyLeaseByTask(t.Context(), owner, "local-task")
	require.NoError(t, err)
	require.NotNil(t, lease)
	foreignCache, err := NewRegionalConcurrencyCache(local.ConcurrencyCache, nil, &slots.Local{DB: local.database(), Region: "eu"})
	require.NoError(t, err)
	foreign := liveKeyTestUpstream(t, cfg, rdb, foreignCache)
	_, err = foreign.LoadAsyncVideoKeyLeaseByTask(t.Context(), owner, "local-task")
	require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy, "even a shared SQL database must not reassign regional authority")
	expiredGrant := grant
	expiredGrant.IssuedAt, expiredGrant.ExpiresAt = grant.IssuedAt.Add(-time.Hour), grant.ExpiresAt.Add(-time.Hour)
	raw, err := json.Marshal(expiredGrant)
	require.NoError(t, err)
	_, err = local.database().ExecContext(t.Context(), `UPDATE regional_local_slot_grants SET grant_payload=$2 WHERE lease_id=$1`, grant.LeaseID, string(raw))
	require.NoError(t, err)
	var policyJSON []byte
	require.NoError(t, local.database().QueryRowContext(t.Context(), `SELECT payload FROM regional_upstream_key_policy_cache WHERE region_id=$1`, local.region()).Scan(&policyJSON))
	var expiredPolicy slots.UpstreamPolicy
	require.NoError(t, json.Unmarshal(policyJSON, &expiredPolicy))
	expiredPolicy.IssuedAt, expiredPolicy.ExpiresAt = expiredPolicy.IssuedAt.Add(-time.Hour), expiredPolicy.ExpiresAt.Add(-time.Hour)
	policyJSON, err = json.Marshal(expiredPolicy)
	require.NoError(t, err)
	_, err = local.database().ExecContext(t.Context(), `UPDATE regional_upstream_key_policy_cache SET issued_at=$1,expires_at=$2,payload=$3`, expiredPolicy.IssuedAt, expiredPolicy.ExpiresAt, string(policyJSON))
	require.NoError(t, err)
	require.NoError(t, restarted.CheckPersistentUpstreamKeySlots(t.Context(), lease), "expiry cannot reclaim a remotely running task")
	lookup := httptest.NewRequest(http.MethodGet, "https://upstream.example/v1/videos/local-task", nil)
	lookup.Header.Set("Authorization", "Bearer supplier-fixture")
	lookup = service.WithAsyncVideoKeyLookup(lookup.WithContext(service.WithAsyncVideoKeyOwner(t.Context(), owner)), lease)
	_, finishLookup, err := restarted.BeginUpstreamKeyRequest(lookup)
	require.NoError(t, err, "an existing owner can observe completion after grant/policy expiry")
	finishLookup(nil, nil)
	_, _, err = restarted.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, service.NewAsyncVideoKeyHandoff(owner)))
	require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy, "stale evidence cannot admit a new submission")
	_, err = local.database().ExecContext(t.Context(), `UPDATE regional_async_video_key_leases SET fenced=true,release_due_at=clock_timestamp() WHERE member=$1`, handoff.Member())
	require.NoError(t, err)
	recovered, err := foreign.RecoverAsyncVideoKeyReleases(t.Context(), 4)
	require.NoError(t, err)
	require.Zero(t, recovered, "a recovery worker cannot claim another region's release")
	require.NoError(t, restarted.CheckPersistentUpstreamKeySlots(t.Context(), lease))
	recovered, err = restarted.RecoverAsyncVideoKeyReleases(t.Context(), 4)
	require.NoError(t, err)
	require.Equal(t, 1, recovered)
	held, err := local.local.Count(t.Context(), grant.Resource)
	require.NoError(t, err)
	require.Zero(t, held)
	raw, err = json.Marshal(grant)
	require.NoError(t, err)
	_, err = local.database().ExecContext(t.Context(), `UPDATE regional_local_slot_grants SET grant_payload=$2 WHERE lease_id=$1`, grant.LeaseID, string(raw))
	require.NoError(t, err)
	snapshot, err := primary.primary.ReadUpstreamPolicy(t.Context())
	require.NoError(t, err)
	require.NoError(t, local.local.InstallUpstreamPolicy(t.Context(), snapshot))
	_, releaseRestored, err := restarted.AcquireUpstreamKeySlots(t.Context(), handoff.Lease().Fingerprints)
	require.NoError(t, err, "a valid refreshed policy restores released regional capacity")
	releaseRestored()
}

func TestRegionalAsyncVideoLocalFactoryRecoversFencedRelease(t *testing.T) {
	t.Setenv("SUB2API_REGIONAL_TEST_DSN", integrationPostgresDSN)
	for _, decorated := range []bool{false, true} {
		name := "direct"
		if decorated {
			name = "astra"
		}
		t.Run(name, func(t *testing.T) {
			rdb := regionalKeyRedis(t)
			cfg := upstreamKeyTestConfig(1, 2)
			cfg.Runtime.Role = config.RuntimeRoleGateway
			authority := regionalAsyncVideoAuthority(t, cfg, rdb, "regional")
			upstream := liveKeyTestUpstream(t, cfg, rdb, authority)
			completed := service.NewAsyncVideoKeyHandoff(asyncVideoLeaseOwner())
			_, finish, err := upstream.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, completed))
			require.NoError(t, err)
			require.NoError(t, upstream.BindAsyncVideoKeyLease(t.Context(), completed, "completed-task"))
			finish(nil, nil)
			unknown := service.NewAsyncVideoKeyHandoff(asyncVideoLeaseOwner())
			_, finish, err = upstream.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, unknown))
			require.NoError(t, err)
			finish(nil, io.ErrUnexpectedEOF)
			lock, err := authority.database().BeginTx(t.Context(), nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = lock.Rollback() })
			var id int64
			require.NoError(t, lock.QueryRowContext(t.Context(), `SELECT resource_id FROM regional_slot_budgets WHERE kind='upstream_key' AND resource_id=$1 FOR UPDATE`, completed.Lease().Resources[0].ResourceID).Scan(&id))
			short, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			err = upstream.ReleaseAsyncVideoKeyLeaseByTask(short, completed.Owner(), "completed-task")
			cancel()
			require.Error(t, err, "a blocked slot release must preserve its fence for recovery")
			var fenced bool
			require.NoError(t, authority.database().QueryRowContext(t.Context(), `SELECT fenced FROM regional_async_video_key_leases WHERE member=$1`, completed.Member()).Scan(&fenced))
			require.True(t, fenced)
			require.NoError(t, upstream.CheckPersistentUpstreamKeySlots(t.Context(), completed.Lease()))
			require.NoError(t, lock.Rollback())
			var transport service.HTTPUpstream = upstream
			if decorated {
				transport = &astraRoutingUpstream{delegate: upstream}
			}
			svc := service.ProvideOpenAIGatewayService(service.OpenAIGatewayDependencies{Config: cfg, Upstream: transport})
			t.Cleanup(svc.StopOpenAICodexTicketHarvester)
			t.Cleanup(svc.StopAsyncVideoKeyReleaseRecovery)
			require.Eventually(t, func() bool {
				lease, loadErr := upstream.LoadAsyncVideoKeyLeaseByTask(t.Context(), completed.Owner(), "completed-task")
				return loadErr == nil && lease == nil
			}, 3*time.Second, 10*time.Millisecond, "the real gateway factory must recover its own regional release")
			require.NoError(t, upstream.CheckPersistentUpstreamKeySlots(t.Context(), unknown.Lease()), "the worker must not reclaim an unfenced unknown submission")
			require.NoError(t, upstream.ReleaseAsyncVideoKeyLeaseByMember(t.Context(), unknown.Member()))
		})
	}
}

func TestRegionalAsyncVideoUnrestrictedRequiresFreshPolicy(t *testing.T) {
	t.Setenv("SUB2API_REGIONAL_TEST_DSN", integrationPostgresDSN)
	rdb := regionalKeyRedis(t)
	cfg := upstreamKeyTestConfig(1, 1)
	primary, _, _ := regionalKeyFixture(t, cfg, rdb, 0)
	upstream := liveKeyTestUpstream(t, cfg, rdb, primary)
	handoff := service.NewAsyncVideoKeyHandoff(asyncVideoLeaseOwner())
	request := asyncVideoLeaseRequest(t, handoff)
	request.Header.Set("Authorization", "Bearer unrestricted-fixture")
	_, finish, err := upstream.BeginUpstreamKeyRequest(request)
	require.NoError(t, err)
	require.Nil(t, handoff.Lease(), "a current unlisted credential has no persistent member")
	finish(nil, nil)

	ownerless := httptest.NewRequest(http.MethodPost, "https://upstream.example/create", strings.NewReader("{}"))
	ownerless.Header.Set("Authorization", "Bearer unrestricted-fixture")
	ownerless = ownerless.WithContext(service.WithAsyncVideoKeyGuard(ownerless.Context(), true))
	_, _, err = upstream.BeginUpstreamKeyRequest(ownerless)
	require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy, "regional previews need a durable owner")

	newer := upstreamKeyTestConfig(2, 1)
	newer.Gateway.UpstreamKeyConcurrency.Limits[config.UpstreamKeyFingerprint("unrestricted-fixture")] = 1
	require.NoError(t, primary.primary.PublishUpstreamPolicy(t.Context(), newer.Gateway.UpstreamKeyConcurrency))
	handoff = service.NewAsyncVideoKeyHandoff(asyncVideoLeaseOwner())
	request = asyncVideoLeaseRequest(t, handoff)
	request.Header.Set("Authorization", "Bearer unrestricted-fixture")
	_, _, err = upstream.BeginUpstreamKeyRequest(request)
	require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy, "a stale process must not treat a newly limited key as unrestricted")
}

func TestRegionalAsyncVideoTaskCollisionDoesNotReplaceOwner(t *testing.T) {
	for _, mode := range []string{"primary", "regional"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("SUB2API_REGIONAL_TEST_DSN", integrationPostgresDSN)
			rdb := regionalKeyRedis(t)
			cfg := upstreamKeyTestConfig(1, 2)
			primary := regionalAsyncVideoAuthority(t, cfg, rdb, mode)
			upstream := liveKeyTestUpstream(t, cfg, rdb, primary)
			owner := asyncVideoLeaseOwner()
			first := service.NewAsyncVideoKeyHandoff(owner)
			_, finish, err := upstream.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, first))
			require.NoError(t, err)
			require.NoError(t, upstream.BindAsyncVideoKeyLease(t.Context(), first, "duplicate-task"))
			finish(nil, nil)
			other := owner
			other.AccountID++
			other.RouteFingerprint[0]++
			second := service.NewAsyncVideoKeyHandoff(other)
			_, finish, err = upstream.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, second))
			require.NoError(t, err)
			require.ErrorIs(t, upstream.BindAsyncVideoKeyLease(t.Context(), second, "duplicate-task"), service.ErrUpstreamKeyPolicy)
			finish(nil, nil)
			principal := owner
			principal.AccountID, principal.RouteFingerprint = 0, [32]byte{}
			resolved, err := upstream.ResolveAsyncVideoKeyOwner(t.Context(), principal, "duplicate-task")
			require.NoError(t, err)
			require.Equal(t, owner, *resolved)
			require.NoError(t, upstream.ReleaseAsyncVideoKeyLeaseByMember(t.Context(), second.Member()))
			require.NoError(t, upstream.CheckPersistentUpstreamKeySlots(t.Context(), first.Lease()))
			require.NoError(t, upstream.ReleaseAsyncVideoKeyLeaseByTask(t.Context(), owner, "duplicate-task"))
		})
	}
}

func TestRegionalAsyncVideoMigrationRollbackAndRepeat(t *testing.T) {
	ctx := t.Context()
	name := "async_video_migration_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	raw, err := migrations.FS.ReadFile("331_regional_async_video_key_lease.sql")
	require.NoError(t, err)
	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.ExecContext(ctx, `CREATE SCHEMA `+pq.QuoteIdentifier(name))
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `SET LOCAL search_path TO `+pq.QuoteIdentifier(name))
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, string(raw))
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, string(raw))
	require.NoError(t, err, "the additive migration must be repeatable")
	var exists bool
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, name+".regional_async_video_key_leases").Scan(&exists))
	require.True(t, exists)
	require.NoError(t, tx.Rollback())
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT to_regnamespace($1) IS NOT NULL`, name).Scan(&exists))
	require.False(t, exists, "rolling back the isolated migration transaction must leave no schema")
}

func TestRegionalAsyncVideoActualLifecycle(t *testing.T) {
	t.Setenv("SUB2API_REGIONAL_TEST_DSN", integrationPostgresDSN)
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{"primary", "regional"} {
		for _, decorated := range []bool{false, true} {
			for _, tc := range []struct {
				name, platform, body, terminal string
				create, status                 service.GrokMediaEndpoint
			}{
				{"seedance", service.PlatformOpenAI, `{"model":"video","content":[{"type":"text","text":"waves"}]}`, "succeeded", service.SeedanceEndpointCreate, service.SeedanceEndpointStatus},
				{"grok", service.PlatformGrok, `{"model":"grok-imagine-video","prompt":"waves"}`, "done", service.GrokMediaEndpointVideosGenerations, service.GrokMediaEndpointVideoStatus},
				{"minimax", service.PlatformMiniMax, `{"model":"MiniMax-H3","prompt":"waves","seconds":"1"}`, "completed", service.MiniMaxH3EndpointCreate, service.MiniMaxH3EndpointStatus},
			} {
				route := "direct"
				if decorated {
					route = "astra"
				}
				t.Run(mode+"/"+route+"/"+tc.name, func(t *testing.T) {
					rdb := regionalKeyRedis(t)
					cfg := upstreamKeyTestConfig(1, 1)
					cfg.Security.URLAllowlist.AllowInsecureHTTP = true
					if mode == "regional" {
						cfg.Runtime.Role = config.RuntimeRoleGateway
					}
					authority := regionalAsyncVideoAuthority(t, cfg, rdb, mode)
					upstream := liveKeyTestUpstream(t, cfg, rdb, authority)
					var transport service.HTTPUpstream = upstream
					if decorated {
						transport = &astraRoutingUpstream{delegate: upstream}
					}
					var creates, polls atomic.Int32
					var malformed atomic.Bool
					provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						if request.Method == http.MethodPost {
							creates.Add(1)
							_, _ = io.WriteString(w, `{"id":"regional-video"}`)
							return
						}
						polls.Add(1)
						if malformed.Load() {
							_, _ = io.WriteString(w, `{"id":"another-task","status":"`+tc.terminal+`"}`)
							return
						}
						_, _ = io.WriteString(w, `{"id":"regional-video","status":"`+tc.terminal+`","video":{"url":"https://vidgen.x.ai/result.mp4","duration":6},"metadata":{"url":"https://video.example/result.mp4"},"usage":{"completion_tokens":1}}`)
					}))
					t.Cleanup(provider.Close)
					svc := service.ProvideOpenAIGatewayService(service.OpenAIGatewayDependencies{Config: cfg, Upstream: transport})
					t.Cleanup(svc.StopOpenAICodexTicketHarvester)
					t.Cleanup(svc.StopAsyncVideoKeyReleaseRecovery)
					account := &service.Account{ID: 7, Platform: tc.platform, Type: service.AccountTypeAPIKey, Concurrency: 1,
						Credentials: map[string]any{"api_key": "supplier-fixture", "base_url": provider.URL, "openai_capabilities": []string{"seedance"}}}
					owner := service.AsyncVideoKeyOwner{UserID: 10, APIKeyID: 20, GroupID: 30, AccountID: account.ID, Provider: tc.create.AsyncVideoProvider(), RouteFingerprint: service.AccountRouteFingerprint(account)}
					ctx := service.WithAsyncVideoKeyOwner(t.Context(), owner)
					forward := func(endpoint service.GrokMediaEndpoint, task string, body []byte) (*service.OpenAIForwardResult, error) {
						method := http.MethodGet
						if endpoint.IsAsyncVideoCreate() {
							method = http.MethodPost
						}
						c, _ := gin.CreateTestContext(httptest.NewRecorder())
						c.Request = httptest.NewRequest(method, "/fixture", strings.NewReader(string(body))).WithContext(ctx)
						switch tc.platform {
						case service.PlatformOpenAI:
							return svc.ForwardSeedance(ctx, c, account, endpoint, task, body)
						case service.PlatformMiniMax:
							return svc.ForwardMiniMaxH3(ctx, c, account, endpoint, task, body)
						default:
							return svc.ForwardGrokMedia(ctx, c, account, endpoint, task, body, "application/json")
						}
					}
					created, err := forward(tc.create, "", []byte(tc.body))
					require.NoError(t, err)
					require.EqualValues(t, 1, creates.Load())
					fingerprint := []string{config.UpstreamKeyFingerprint("supplier-fixture")}
					_, _, err = upstream.AcquireUpstreamKeySlots(ctx, fingerprint)
					require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)
					malformed.Store(true)
					invalid, err := forward(tc.status, created.ResponseID, nil)
					require.Error(t, err)
					require.Nil(t, invalid)
					_, _, err = upstream.AcquireUpstreamKeySlots(ctx, fingerprint)
					require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)
					malformed.Store(false)
					status, err := forward(tc.status, created.ResponseID, nil)
					require.NoError(t, err)
					require.True(t, status.AsyncVideoTerminal)
					require.EqualValues(t, 2, polls.Load())
					require.NoError(t, svc.ReleaseAsyncVideoTaskLease(ctx, created.ResponseID))
					_, release, err := upstream.AcquireUpstreamKeySlots(ctx, fingerprint)
					require.NoError(t, err)
					require.NotNil(t, release)
					release()
				})
			}
		}
	}
}
