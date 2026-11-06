//go:build integration

package repository

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type batchKeySQLBarrier struct {
	service.BatchImageRepository
	arrived chan struct{}
	proceed chan struct{}
}

func (r *batchKeySQLBarrier) BindBatchImageUpstreamKeyLease(ctx context.Context, batchID, route string, lease *service.PersistentUpstreamKeyLease) error {
	select {
	case r.arrived <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-r.proceed:
	case <-ctx.Done():
		return ctx.Err()
	}
	return r.BatchImageRepository.BindBatchImageUpstreamKeyLease(ctx, batchID, route, lease)
}

func TestBatchImagePersistentKeySQLRedisCompetitionAndRecovery(t *testing.T) {
	db, _ := newBillingAuditDatabase(t)
	repo := NewBatchImageRepository(db)
	// TestMain owns this disposable Redis container. A separate database keeps
	// this test's quota keys independent of the integration harness's DB 0.
	rdb := redis.NewClient(&redis.Options{Addr: integrationRedis.Options().Addr, DB: 15})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	for _, setting := range []struct{ name, value string }{
		{"appendonly", "yes"}, {"appendfsync", "always"},
		{"no-appendfsync-on-rewrite", "no"}, {"maxmemory-policy", "noeviction"},
	} {
		previous, err := rdb.ConfigGet(t.Context(), setting.name).Result()
		require.NoError(t, err)
		require.Contains(t, previous, setting.name)
		require.NoError(t, rdb.ConfigSet(t.Context(), setting.name, setting.value).Err())
		t.Cleanup(func() {
			require.NoError(t, rdb.ConfigSet(context.Background(), setting.name, previous[setting.name]).Err())
		})
	}
	cfg := upstreamKeyTestConfig(1, 2)
	authority := liveKeyTestUpstream(t, cfg, rdb, nil)
	var creates atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body := `{"name":"batches/owned","state":"JOB_STATE_CANCELLED"}`
		switch {
		case strings.Contains(req.URL.Path, "/upload/"):
			body = `{"file":{"name":"files/input"}}`
		case strings.HasSuffix(req.URL.Path, ":batchGenerateContent"):
			creates.Add(1)
			body = `{"name":"batches/owned","state":"JOB_STATE_RUNNING"}`
		case strings.HasSuffix(req.URL.Path, ":cancel"):
			body = `{}`
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Errorf("write batch provider response: %v", err)
		}
	}))
	defer upstream.Close()
	barrier := &batchKeySQLBarrier{BatchImageRepository: repo, arrived: make(chan struct{}, 2), proceed: make(chan struct{})}
	type result struct {
		registry *service.BatchImageProviderRegistry
		provider *service.GeminiAPIBatchImageProvider
		account  *service.Account
		job      *service.BatchImageJob
		created  *service.BatchProviderJob
		err      error
	}
	results := make(chan result, 2)
	var workers sync.WaitGroup
	for index := 0; index < 2; index++ {
		registry, provider, account, job, ctx := batchKeyHTTPFixture(t, authority, barrier, upstream.URL, upstream.Client())
		if index == 0 {
			_, err := repo.CreateBatchImageJob(t.Context(), service.CreateBatchImageJobParams{BatchID: job.BatchID, UserID: 1001,
				AccountID: job.AccountID, Provider: job.Provider, Model: job.Model, Status: job.Status, RouteFingerprint: job.RouteFingerprint})
			require.NoError(t, err)
		}
		workers.Go(func() {
			created, err := provider.Submit(ctx, job, account, batchKeyInput(job))
			results <- result{registry: registry, provider: provider, account: account, job: job, created: created, err: err}
		})
	}
	for range 2 {
		select {
		case <-barrier.arrived:
		case <-time.After(15 * time.Second):
			close(barrier.proceed)
			workers.Wait()
			t.Fatal("both requests must reach the real SQL binding boundary")
		}
	}
	close(barrier.proceed)
	workers.Wait()
	close(results)
	var winner result
	successes := 0
	for outcome := range results {
		if outcome.err == nil {
			successes++
			winner = outcome
		} else {
			require.ErrorIs(t, outcome.err, service.ErrBatchImageSharedKeyUnavailable)
		}
	}
	require.Equal(t, 1, successes)
	require.EqualValues(t, 1, creates.Load(), "only the SQL owner may send remote creation")
	stored, err := repo.GetBatchImageJobByBatchID(t.Context(), winner.job.BatchID)
	require.NoError(t, err)
	require.Equal(t, winner.job.UpstreamKeyLease, stored.UpstreamKeyLease)
	require.NotNil(t, stored.UpstreamKeyLease)
	key := upstreamKeySlotKeyPrefix + config.UpstreamKeyFingerprint("supplier-fixture")
	require.EqualValues(t, 1, rdb.ZCard(t.Context(), key).Val(), "the loser releases only its own Redis member")
	require.NoError(t, repo.UpdateBatchImageJobProviderSubmit(t.Context(), service.UpdateBatchImageJobProviderSubmitParams{
		BatchID: stored.BatchID, ProviderJobName: winner.created.ProviderJobName, ProviderInputRef: winner.created.ProviderInputRef,
	}))
	stored, err = repo.GetBatchImageJobByBatchID(t.Context(), stored.BatchID)
	require.NoError(t, err)
	ctx, err := winner.registry.PrepareRequest(t.Context(), winner.provider, stored, winner.account,
		func(context.Context, int64) (*service.Account, error) { return winner.account, nil }, repo)
	require.NoError(t, err)
	require.NoError(t, winner.provider.Cancel(ctx, stored, winner.account))
	require.NoError(t, authority.CheckPersistentUpstreamKeySlots(t.Context(), stored.UpstreamKeyLease), "cancel acknowledgement is not terminal")
	status, err := winner.provider.Get(ctx, stored, winner.account)
	require.NoError(t, err)
	require.True(t, status.Done)
	require.Equal(t, service.BatchProviderStateCancelled, status.InternalState)
	require.NoError(t, repo.ConfirmBatchImageUpstreamKeyTerminal(t.Context(), stored.BatchID, stored.UpstreamKeyLease))

	// A fresh controller recovers the SQL-confirmed release without a queue entry.
	restarted := service.NewBatchImageProviderRegistry(winner.provider)
	restarted.KeyAuthority = liveKeyTestUpstream(t, cfg, rdb, nil)
	recovery := &service.BatchImageBillingRecoveryService{Repo: NewBatchImageRepository(db), ProviderRegistry: restarted}
	released, err := recovery.ReleaseConfirmedUpstreamKeyOnce(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, released)
	released, err = recovery.ReleaseConfirmedUpstreamKeyOnce(t.Context())
	require.NoError(t, err)
	require.Zero(t, released)
	require.Zero(t, rdb.ZCard(t.Context(), key).Val())
	stored, err = repo.GetBatchImageJobByBatchID(t.Context(), stored.BatchID)
	require.NoError(t, err)
	require.NotNil(t, stored.UpstreamKeyTerminalAt)
	require.NotNil(t, stored.UpstreamKeyReleasedAt)
}

func TestBatchImagePersistentKeyRegionalHandoff(t *testing.T) {
	t.Setenv("SUB2API_REGIONAL_TEST_DSN", integrationPostgresDSN)
	for _, mode := range []string{"primary", "region"} {
		t.Run(mode, func(t *testing.T) {
			rdb := regionalKeyRedis(t)
			cfg := upstreamKeyTestConfig(1, 2)
			primary, region, _ := regionalKeyFixture(t, cfg, rdb, 1)
			owner := primary
			if mode == "region" {
				owner = region
			}
			authority := liveKeyTestUpstream(t, cfg, rdb, owner)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				body := `{"name":"batches/owned","state":"JOB_STATE_RUNNING"}`
				if strings.Contains(req.URL.Path, "/upload/") {
					body = `{"file":{"name":"files/input"}}`
				}
				if _, err := io.WriteString(w, body); err != nil {
					t.Errorf("write regional batch response: %v", err)
				}
			}))
			defer upstream.Close()
			repo := &batchKeyBindingRepository{}
			registry, provider, account, job, ctx := batchKeyHTTPFixture(t, authority, repo, upstream.URL, upstream.Client())
			created, err := provider.Submit(ctx, job, account, batchKeyInput(job))
			require.NoError(t, err)
			require.NotNil(t, job.UpstreamKeyLease)
			lease := job.UpstreamKeyLease
			require.NoError(t, lease.Validate())
			restartedCache, err := NewRegionalConcurrencyCache(owner.ConcurrencyCache, owner.primary, owner.local)
			require.NoError(t, err)
			restarted := liveKeyTestUpstream(t, cfg, rdb, restartedCache)
			require.NoError(t, restarted.CheckPersistentUpstreamKeySlots(t.Context(), lease))
			_, _, err = restarted.AcquireUpstreamKeySlots(t.Context(), lease.Fingerprints)
			require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)
			newPolicy := upstreamKeyTestConfig(2, 2)
			require.NoError(t, primary.primary.PublishUpstreamPolicy(t.Context(), newPolicy.Gateway.UpstreamKeyConcurrency))
			snapshot, err := primary.primary.ReadUpstreamPolicy(t.Context())
			require.NoError(t, err)
			require.NoError(t, region.local.InstallUpstreamPolicy(t.Context(), snapshot))
			registry.KeyAuthority = restarted
			job.Status, job.ProviderJobName = service.BatchImageJobStatusSubmitted, &created.ProviderJobName
			ctx, err = registry.PrepareRequest(t.Context(), provider, job, account, func(context.Context, int64) (*service.Account, error) { return account, nil }, repo)
			require.NoError(t, err)
			_, err = provider.Get(ctx, job, account)
			require.NoError(t, err, "policy changes must not require another grant to manage admitted work")
			require.NoError(t, provider.Cancel(ctx, job, account))
			require.NoError(t, restarted.CheckPersistentUpstreamKeySlots(t.Context(), lease))
			require.NoError(t, restarted.ReleasePersistentUpstreamKeySlots(t.Context(), lease))
			require.NoError(t, restarted.ReleasePersistentUpstreamKeySlots(t.Context(), lease))
			require.ErrorIs(t, restarted.CheckPersistentUpstreamKeySlots(t.Context(), lease), service.ErrUpstreamKeyLease)
			_, err = provider.Get(ctx, job, account)
			require.ErrorIs(t, err, service.ErrBatchImageSharedKeyUnavailable, "a stale owner must not resurrect the member")
		})
	}
}

func TestBatchImagePersistentKeyDatabaseCompetitionAndAuditRollback(t *testing.T) {
	db, _ := newBillingAuditDatabase(t)
	repo := NewBatchImageRepository(db)
	route := strings.Repeat("b", 64)
	job, err := repo.CreateBatchImageJob(t.Context(), service.CreateBatchImageJobParams{BatchID: "imgbatch_sql_key", UserID: 1001,
		Provider: service.BatchImageProviderGeminiAPI, Model: "image", Status: service.BatchImageJobStatusUploading, RouteFingerprint: route})
	require.NoError(t, err)
	// A failed audit insert must roll back ownership/confirmation/receipt together.
	setAuditFault := func(event string) {
		_, err := db.ExecContext(t.Context(), fmt.Sprintf(`
CREATE OR REPLACE FUNCTION reject_key_audit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN IF NEW.event_type = '%s' THEN RAISE EXCEPTION 'synthetic audit failure'; END IF; RETURN NEW; END $$;
DROP TRIGGER IF EXISTS reject_key_audit ON batch_image_events;
CREATE TRIGGER reject_key_audit BEFORE INSERT ON batch_image_events FOR EACH ROW EXECUTE FUNCTION reject_key_audit();`, event))
		require.NoError(t, err)
	}
	setAuditFault("upstream_key_bound")
	require.Error(t, repo.BindBatchImageUpstreamKeyLease(t.Context(), job.BatchID, route, batchImageKeyTestLease(job.BatchID)))
	stored, err := repo.GetBatchImageJobByBatchID(t.Context(), job.BatchID)
	require.NoError(t, err)
	require.Nil(t, stored.UpstreamKeyLease)
	setAuditFault("none")
	const contenders = 12
	results := make(chan error, contenders)
	var workers sync.WaitGroup
	for range contenders {
		lease := batchImageKeyTestLease(job.BatchID)
		workers.Go(func() { results <- repo.BindBatchImageUpstreamKeyLease(t.Context(), job.BatchID, route, lease) })
	}
	workers.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else {
			require.ErrorIs(t, err, service.ErrBatchImageSharedKeyUnavailable)
		}
	}
	require.Equal(t, 1, successes, "one durable owner across independent database transactions")
	stored, err = repo.GetBatchImageJobByBatchID(t.Context(), job.BatchID)
	require.NoError(t, err)
	lease := stored.UpstreamKeyLease
	require.NotNil(t, lease)
	setAuditFault("upstream_key_terminal_confirmed")
	require.Error(t, repo.ConfirmBatchImageUpstreamKeyTerminal(t.Context(), job.BatchID, lease))
	stored, err = repo.GetBatchImageJobByBatchID(t.Context(), job.BatchID)
	require.NoError(t, err)
	require.Nil(t, stored.UpstreamKeyTerminalAt)
	setAuditFault("none")
	require.NoError(t, repo.ConfirmBatchImageUpstreamKeyTerminal(t.Context(), job.BatchID, lease))
	setAuditFault("upstream_key_released")
	require.Error(t, repo.MarkBatchImageUpstreamKeyReleased(t.Context(), job.BatchID, lease))
	stored, err = repo.GetBatchImageJobByBatchID(t.Context(), job.BatchID)
	require.NoError(t, err)
	require.NotNil(t, stored.UpstreamKeyTerminalAt)
	require.Nil(t, stored.UpstreamKeyReleasedAt)
	pending, err := repo.ListBatchImageJobsPendingUpstreamKeyRelease(t.Context(), contenders)
	require.NoError(t, err)
	require.Len(t, pending, 1, "a fresh worker finds the release even with no queue membership")
	setAuditFault("none")
	require.NoError(t, repo.MarkBatchImageUpstreamKeyReleased(t.Context(), job.BatchID, lease))
	pending, err = repo.ListBatchImageJobsPendingUpstreamKeyRelease(t.Context(), contenders)
	require.NoError(t, err)
	require.Empty(t, pending)
}
