package repository

import (
	"context"
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

// The actual repository CAS/transaction is exercised by the PostgreSQL tests.
// This fixture coordinates two independent HTTP submissions at that boundary.
type batchKeyBindingRepository struct {
	service.BatchImageRepository
	mu      sync.Mutex
	lease   *service.PersistentUpstreamKeyLease
	arrived chan struct{}
	proceed chan struct{}
}

func (r *batchKeyBindingRepository) BindBatchImageUpstreamKeyLease(ctx context.Context, batchID, _ string, lease *service.PersistentUpstreamKeyLease) error {
	if r.arrived != nil {
		r.arrived <- struct{}{}
		select {
		case <-r.proceed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lease != nil || !lease.MatchesBatch(batchID) {
		return service.ErrBatchImageSharedKeyUnavailable
	}
	r.lease = lease
	return nil
}

func batchKeyHTTPFixture(t *testing.T, authority service.BatchImageUpstreamKeyAuthority, repo service.BatchImageRepository, endpoint string, client *http.Client) (*service.BatchImageProviderRegistry, *service.GeminiAPIBatchImageProvider, *service.Account, *service.BatchImageJob, context.Context) {
	t.Helper()
	provider := service.NewGeminiAPIBatchImageProvider(service.NewGeminiBatchHTTPClient(endpoint, client))
	registry := service.NewBatchImageProviderRegistry(provider)
	registry.KeyAuthority = authority
	account := &service.Account{ID: 10, Platform: service.PlatformGemini, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"api_key": "supplier-fixture"}}
	job := &service.BatchImageJob{BatchID: "imgbatch_persistent", Provider: provider.Name(), Model: "gemini-2.5-flash-image", AccountID: &account.ID,
		Status: service.BatchImageJobStatusUploading, RouteFingerprint: provider.RouteFingerprint(account)}
	ctx, err := registry.PrepareRequest(t.Context(), provider, job, account, func(context.Context, int64) (*service.Account, error) { return account, nil }, repo)
	require.NoError(t, err)
	return registry, provider, account, job, ctx
}

func batchKeyInput(job *service.BatchImageJob) service.BatchImageInput {
	return service.BatchImageInput{BatchID: job.BatchID, Model: job.Model, Items: []service.BatchImageInputItem{{CustomID: "one", Prompt: "fixture"}}, ImageSize: "1K"}
}

func TestUpstreamKeyBatchPersistsAndSharesCapacity(t *testing.T) {
	server, rdb := liveKeyRedis(t, nil)
	testUpstreamKeyBatchPersistence(t, rdb, func() { server.FastForward(2 * upstreamKeyLeaseTTL) })
}

func testUpstreamKeyBatchPersistence(t *testing.T, rdb *redis.Client, advance func()) {
	t.Helper()
	cfg := upstreamKeyTestConfig(1, 1)
	authority := liveKeyTestUpstream(t, cfg, rdb, nil)
	var sent atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		sent.Add(1)
		body := `{"name":"batches/owned","state":"JOB_STATE_RUNNING"}`
		if strings.Contains(req.URL.Path, "/upload/") {
			body = `{"file":{"name":"files/input"}}`
		} else if strings.HasSuffix(req.URL.Path, ":cancel") {
			body = `{}`
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Errorf("write batch response: %v", err)
		}
	}))
	defer upstream.Close()
	repo := &batchKeyBindingRepository{}
	registry, provider, account, job, ctx := batchKeyHTTPFixture(t, authority, repo, upstream.URL, upstream.Client())
	created, err := provider.Submit(ctx, job, account, batchKeyInput(job))
	require.NoError(t, err)
	require.NotNil(t, repo.lease)
	require.True(t, repo.lease.MatchesBatch(job.BatchID))
	key := upstreamKeySlotKeyPrefix + config.UpstreamKeyFingerprint("supplier-fixture")
	require.EqualValues(t, 1, rdb.ZCard(t.Context(), key).Val())
	require.Equal(t, -time.Nanosecond, rdb.TTL(t.Context(), key).Val(), "closing the creation response must not expire remote work")
	advance()
	restarted := liveKeyTestUpstream(t, cfg, rdb, nil)
	require.NoError(t, restarted.CheckPersistentUpstreamKeySlots(t.Context(), repo.lease))
	_, _, err = restarted.AcquireUpstreamKeySlots(t.Context(), repo.lease.Fingerprints)
	require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity, "HTTP and native WS share the batch budget")
	registry.KeyAuthority = restarted
	job.ProviderJobName = &created.ProviderJobName
	job.Status = service.BatchImageJobStatusSubmitted
	ctx, err = registry.PrepareRequest(t.Context(), provider, job, account, func(context.Context, int64) (*service.Account, error) { return account, nil }, repo)
	require.NoError(t, err)
	status, err := provider.Get(ctx, job, account)
	require.NoError(t, err, "polling uses existing ownership without requiring a second free slot")
	require.False(t, status.Done)
	require.NoError(t, provider.Cancel(ctx, job, account))
	require.NoError(t, restarted.CheckPersistentUpstreamKeySlots(t.Context(), repo.lease), "a cancel acknowledgement is not termination")
	require.EqualValues(t, 4, sent.Load())
	require.NoError(t, restarted.ReleasePersistentUpstreamKeySlots(t.Context(), repo.lease))
	_, err = provider.Get(ctx, job, account)
	require.ErrorIs(t, err, service.ErrBatchImageSharedKeyUnavailable)
	require.EqualValues(t, 4, sent.Load(), "lost ownership fails before HTTP and never recreates the member")
	require.Zero(t, rdb.ZCard(t.Context(), key).Val())
}

func TestUpstreamKeyBatchBindingLoserDoesNotReleaseWinner(t *testing.T) {
	_, rdb := liveKeyRedis(t, nil)
	authority := liveKeyTestUpstream(t, upstreamKeyTestConfig(1, 2), rdb, nil)
	var creates atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body := `{"file":{"name":"files/input"}}`
		if strings.HasSuffix(req.URL.Path, ":batchGenerateContent") {
			creates.Add(1)
			body = `{"name":"batches/owned"}`
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Errorf("write batch response: %v", err)
		}
	}))
	defer upstream.Close()
	repo := &batchKeyBindingRepository{arrived: make(chan struct{}, 2), proceed: make(chan struct{})}
	defer func() {
		if repo.lease != nil {
			require.NoError(t, authority.ReleasePersistentUpstreamKeySlots(context.Background(), repo.lease))
		}
	}()
	errors := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		_, provider, account, job, ctx := batchKeyHTTPFixture(t, authority, repo, upstream.URL, upstream.Client())
		workers.Go(func() { _, err := provider.Submit(ctx, job, account, batchKeyInput(job)); errors <- err })
	}
	for range 2 {
		select {
		case <-repo.arrived:
		case <-time.After(5 * time.Second):
			close(repo.proceed)
			workers.Wait()
			t.Fatal("both submitters did not reach the binding boundary")
		}
	}
	close(repo.proceed)
	workers.Wait()
	close(errors)
	successes := 0
	for err := range errors {
		if err == nil {
			successes++
		} else {
			require.ErrorIs(t, err, service.ErrBatchImageSharedKeyUnavailable)
		}
	}
	require.Equal(t, 1, successes)
	require.EqualValues(t, 1, creates.Load(), "only the database binding winner may create remote work")
	require.NoError(t, authority.CheckPersistentUpstreamKeySlots(t.Context(), repo.lease))
	require.EqualValues(t, 1, rdb.ZCard(t.Context(), upstreamKeySlotKeyPrefix+repo.lease.Resources[0].Fingerprint).Val())
}

func TestUpstreamKeyBatchRejectsUntrustedOrUnconfirmedOwnership(t *testing.T) {
	for _, volatile := range []bool{false, true} {
		metadata := map[string]string{}
		if volatile {
			metadata["appendonly"] = "no"
		}
		_, rdb := liveKeyRedis(t, metadata)
		authority := liveKeyTestUpstream(t, upstreamKeyTestConfig(1, 1), rdb, nil)
		req := httptest.NewRequest(http.MethodPost, "http://fixture/v1/models/model:batchGenerateContent", nil)
		req.Header.Set("X-Goog-Api-Key", "supplier-fixture")
		_, err := authority.AcquirePersistentUpstreamKeySlots(t.Context(), req)
		require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy, "an untrusted request has no internal batch creation proof")
		if volatile {
			_, _, _, job, ctx := batchKeyHTTPFixture(t, authority, &batchKeyBindingRepository{}, "http://127.0.0.1:1", http.DefaultClient)
			// Call the actual creation client directly so there is no preceding upload.
			client := service.NewGeminiBatchHTTPClient("http://127.0.0.1:1", http.DefaultClient)
			_, err := client.CreateBatch(ctx, "supplier-fixture", job.Model, "files/input", "fixture")
			require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy)
			require.Nil(t, job.UpstreamKeyLease)
		}
	}
}
