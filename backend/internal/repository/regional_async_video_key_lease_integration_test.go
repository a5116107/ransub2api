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
	"github.com/Wei-Shaw/sub2api/internal/regional/slots"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestRegionalAsyncVideoKeyLeaseHandoffAndRecovery(t *testing.T) {
	t.Setenv("SUB2API_REGIONAL_TEST_DSN", integrationPostgresDSN)
	rdb := regionalKeyRedis(t)
	cfg := upstreamKeyTestConfig(1, 1)
	primary, _, _ := regionalKeyFixture(t, cfg, rdb, 0)
	upstream := liveKeyTestUpstream(t, cfg, rdb, primary)
	require.ErrorIs(t, upstream.ReleaseAsyncVideoKeyLeaseByMember(t.Context(), "invalid-member"), service.ErrUpstreamKeyPolicy)
	owner := asyncVideoLeaseOwner()
	handoff := service.NewAsyncVideoKeyHandoff(owner)
	request := asyncVideoLeaseRequest(t, handoff)
	_, finish, err := upstream.BeginUpstreamKeyRequest(request)
	require.NoError(t, err)
	require.NotNil(t, handoff.Lease())
	require.Equal(t, "regional:primary", handoff.Lease().Authority)
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

	restartedCache, err := NewRegionalConcurrencyCache(primary.ConcurrencyCache, primary.primary, nil)
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
}

func TestRegionalAsyncVideoAcquireWaitsForReleaseFence(t *testing.T) {
	t.Setenv("SUB2API_REGIONAL_TEST_DSN", integrationPostgresDSN)
	rdb := regionalKeyRedis(t)
	cfg := upstreamKeyTestConfig(1, 1)
	primary, _, _ := regionalKeyFixture(t, cfg, rdb, 0)
	upstream := liveKeyTestUpstream(t, cfg, rdb, primary)
	handoff := service.NewAsyncVideoKeyHandoff(asyncVideoLeaseOwner())
	fingerprint := config.UpstreamKeyFingerprint("supplier-fixture")
	resource, limited, err := primary.upstreamKeyResource(t.Context(), upstream.upstreamKeyLimiter.policy.Revision, upstream.upstreamKeyLimiter.digest, fingerprint)
	require.NoError(t, err)
	require.True(t, limited)
	lease := &service.PersistentUpstreamKeyLease{Authority: "regional:primary", Member: handoff.Member(),
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
	count, err := primary.primary.Count(t.Context(), resource)
	require.NoError(t, err)
	require.Zero(t, count, "no delayed acquisition may survive release")
}

func TestRegionalAsyncVideoRecoveryPreservesUnknownAcrossInstances(t *testing.T) {
	t.Setenv("SUB2API_REGIONAL_TEST_DSN", integrationPostgresDSN)
	rdb := regionalKeyRedis(t)
	cfg := upstreamKeyTestConfig(1, 2)
	primary, _, _ := regionalKeyFixture(t, cfg, rdb, 0)
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
}

func TestRegionalAsyncVideoRecoveryRollsBackPartialRelease(t *testing.T) {
	t.Setenv("SUB2API_REGIONAL_TEST_DSN", integrationPostgresDSN)
	rdb := regionalKeyRedis(t)
	cfg := upstreamKeyTestConfig(1, 1)
	second := config.UpstreamKeyFingerprint("second-fixture")
	cfg.Gateway.UpstreamKeyConcurrency.Limits[second] = 1
	primary, _, _ := regionalKeyFixture(t, cfg, rdb, 0)
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
}

func TestRegionalAsyncVideoRecoveryRejectsMismatchedResources(t *testing.T) {
	t.Setenv("SUB2API_REGIONAL_TEST_DSN", integrationPostgresDSN)
	rdb := regionalKeyRedis(t)
	cfg := upstreamKeyTestConfig(1, 1)
	cfg.Gateway.UpstreamKeyConcurrency.Limits[config.UpstreamKeyFingerprint("second-fixture")] = 1
	primary, _, _ := regionalKeyFixture(t, cfg, rdb, 0)
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
}

func TestRegionalAsyncVideoLocalAuthorityRemainsClosed(t *testing.T) {
	t.Setenv("SUB2API_REGIONAL_TEST_DSN", integrationPostgresDSN)
	rdb := regionalKeyRedis(t)
	cfg := upstreamKeyTestConfig(1, 2)
	_, local, _ := regionalKeyFixture(t, cfg, rdb, 1)
	upstream := liveKeyTestUpstream(t, cfg, rdb, local)
	handoff := service.NewAsyncVideoKeyHandoff(asyncVideoLeaseOwner())
	_, _, err := upstream.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, handoff))
	require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy)
	_, err = upstream.AcquirePersistentUpstreamKeySlots(t.Context(), asyncVideoLeaseRequest(t, handoff))
	require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy)
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
	t.Setenv("SUB2API_REGIONAL_TEST_DSN", integrationPostgresDSN)
	rdb := regionalKeyRedis(t)
	cfg := upstreamKeyTestConfig(1, 2)
	primary, _, _ := regionalKeyFixture(t, cfg, rdb, 0)
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

func TestRegionalAsyncVideoSeedanceActualLifecycle(t *testing.T) {
	t.Setenv("SUB2API_REGIONAL_TEST_DSN", integrationPostgresDSN)
	gin.SetMode(gin.TestMode)
	rdb := regionalKeyRedis(t)
	cfg := upstreamKeyTestConfig(1, 1)
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	primary, _, _ := regionalKeyFixture(t, cfg, rdb, 0)
	upstream := liveKeyTestUpstream(t, cfg, rdb, primary)
	var creates, polls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodPost {
			creates.Add(1)
			_, _ = io.WriteString(w, `{"id":"regional-video"}`)
			return
		}
		polls.Add(1)
		_, _ = io.WriteString(w, `{"status":"succeeded","usage":{"completion_tokens":1}}`)
	}))
	t.Cleanup(provider.Close)
	svc := service.NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil)
	t.Cleanup(svc.StopOpenAICodexTicketHarvester)
	account := &service.Account{ID: 7, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "supplier-fixture", "base_url": provider.URL, "openai_capabilities": []string{"seedance"}}}
	owner := service.AsyncVideoKeyOwner{UserID: 10, APIKeyID: 20, GroupID: 30, AccountID: account.ID,
		Provider: "seedance", RouteFingerprint: service.AccountRouteFingerprint(account)}
	ctx := service.WithAsyncVideoKeyOwner(t.Context(), owner)
	makeContext := func(method string) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(method, "/fixture", strings.NewReader("{}")).WithContext(ctx)
		return c
	}
	created, err := svc.ForwardSeedance(ctx, makeContext(http.MethodPost), account, service.SeedanceEndpointCreate, "", []byte(`{"model":"video","content":[{"type":"text","text":"waves"}]}`))
	require.NoError(t, err)
	require.EqualValues(t, 1, creates.Load())
	_, _, err = upstream.AcquireUpstreamKeySlots(ctx, []string{config.UpstreamKeyFingerprint("supplier-fixture")})
	require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)
	status, err := svc.ForwardSeedance(ctx, makeContext(http.MethodGet), account, service.SeedanceEndpointStatus, created.ResponseID, nil)
	require.NoError(t, err)
	require.True(t, status.AsyncVideoTerminal)
	require.EqualValues(t, 1, polls.Load())
	require.NoError(t, svc.ReleaseAsyncVideoTaskLease(ctx, created.ResponseID))
	_, release, err := upstream.AcquireUpstreamKeySlots(ctx, []string{config.UpstreamKeyFingerprint("supplier-fixture")})
	require.NoError(t, err)
	require.NotNil(t, release)
	release()
}
