package repository

import (
	"context"
	"crypto/tls"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/regional/control"
	"github.com/Wei-Shaw/sub2api/internal/regional/slots"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	miniserver "github.com/alicebob/miniredis/v2/server"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// Each connection is confined to a new schema in an explicitly supplied test
// database. Neither the container harness nor an existing application's schema
// is changed. The original migrations are executed without rewriting them.
func regionalKeyTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("SUB2API_REGIONAL_TEST_DSN")
	if dsn == "" {
		t.Skip("SUB2API_REGIONAL_TEST_DSN must point to an isolated PostgreSQL fixture")
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, err := pq.ParseURL(dsn)
		require.NoError(t, err)
		dsn = parsed
	}
	base, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, base.Close()) })
	schema := "regional_key_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = base.ExecContext(t.Context(), `CREATE SCHEMA `+schema)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := base.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		require.NoError(t, err)
	})
	db, err := sql.Open("postgres", dsn+" search_path="+schema)
	require.NoError(t, err)
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(16)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	for _, name := range []string{"280_regional_slot_leases.sql", "281_regional_admission.sql", "327_regional_upstream_key_slots.sql"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "migrations", name))
		require.NoError(t, err)
		tx, err := db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(t.Context(), string(raw))
		if err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		require.NoError(t, tx.Commit())
	}
	// Concurrency checks measure admission on a ready pool, independently of
	// Windows starting new PostgreSQL connection processes under a burst.
	func() {
		var connections []*sql.Conn
		defer func() {
			for _, conn := range connections {
				require.NoError(t, conn.Close())
			}
		}()
		for range 16 {
			conn, err := db.Conn(t.Context())
			require.NoError(t, err)
			connections = append(connections, conn)
		}
	}()
	return db
}

func regionalKeyFixture(t *testing.T, cfg *config.Config, rdb *redis.Client, capacity int) (*regionalConcurrencyCache, *regionalConcurrencyCache, slots.Grant) {
	t.Helper()
	primary := NewRegionalSlotAuthority(regionalKeyTestDB(t))
	require.NoError(t, primary.PublishUpstreamPolicy(t.Context(), cfg.Gateway.UpstreamKeyConcurrency))
	local := &slots.Local{DB: regionalKeyTestDB(t), Region: "us"}
	snapshot, err := primary.ReadUpstreamPolicy(t.Context())
	require.NoError(t, err)
	require.NoError(t, local.InstallUpstreamPolicy(t.Context(), snapshot))
	fingerprint := config.UpstreamKeyFingerprint("supplier-fixture")
	// Grants use the authority's clock, matching the real broker. Container and
	// Windows wall clocks need not agree within the old one-millisecond margin.
	var issuedAt time.Time
	require.NoError(t, primary.DB.QueryRowContext(t.Context(), "SELECT clock_timestamp()").Scan(&issuedAt))
	g := slots.Grant{LeaseID: uuid.NewString(), Region: local.Region, Resource: slots.Resource{Kind: "upstream_key", ID: snapshot.Resources[fingerprint], Fingerprint: fingerprint}, Slots: capacity, Revision: 1, IssuedAt: issuedAt.Add(-time.Millisecond).Truncate(time.Microsecond), ExpiresAt: issuedAt.Add(time.Minute).Truncate(time.Microsecond)}
	if capacity > 0 {
		require.NoError(t, primary.Issue(t.Context(), g))
		require.NoError(t, local.Install(t.Context(), g))
	}
	base := NewConcurrencyCache(rdb, 15, 30)
	p, err := NewRegionalConcurrencyCache(base, primary, nil)
	require.NoError(t, err)
	l, err := NewRegionalConcurrencyCache(base, nil, local)
	require.NoError(t, err)
	primaryCache, ok := p.(*regionalConcurrencyCache)
	require.True(t, ok)
	localCache, ok := l.(*regionalConcurrencyCache)
	require.True(t, ok)
	return primaryCache, localCache, g
}

func regionalKeyRedis(t *testing.T) *redis.Client {
	t.Helper()
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	return rdb
}

func TestRegionalUpstreamKeyCompetingPoolsAndRestart(t *testing.T) {
	rdb := regionalKeyRedis(t)
	cfg := upstreamKeyTestConfig(1, 4)
	p, l, grant := regionalKeyFixture(t, cfg, rdb, 2)
	fingerprint := grant.Resource.Fingerprint
	type acquired struct {
		local   bool
		request string
		cache   *regionalConcurrencyCache
		ok      bool
		err     error
	}
	results := make(chan acquired, 64)
	var wg sync.WaitGroup
	for _, cache := range []*regionalConcurrencyCache{p, l} {
		limiter := newUpstreamKeyLimiter(cfg, rdb, cache)
		for range 32 {
			wg.Go(func() {
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				defer cancel()
				request := uuid.NewString()
				_, result, err := cache.acquireUpstreamKeySlot(ctx, limiter.policy.Revision, limiter.digest, fingerprint, request)
				if err == nil && result == 0 {
					err = service.ErrUpstreamKeyCapacity
				}
				results <- acquired{cache.local != nil, request, cache, result == 1, err}
			})
		}
	}
	wg.Wait()
	close(results)
	counts := map[bool]int{}
	for result := range results {
		if result.ok {
			counts[result.local]++
			t.Cleanup(func() {
				require.NoError(t, result.cache.releaseUpstreamKeySlot(context.Background(), grant.Resource, result.request))
			})
		} else {
			require.ErrorIs(t, result.err, service.ErrUpstreamKeyCapacity)
		}
	}
	require.Equal(t, map[bool]int{false: 2, true: 2}, counts)
	for _, cache := range []*regionalConcurrencyCache{p, l} {
		restarted, err := NewRegionalConcurrencyCache(NewConcurrencyCache(rdb, 15, 30), cache.primary, cache.local)
		require.NoError(t, err)
		upstream := ProvideHTTPUpstream(cfg, nil, rdb, restarted)
		authority, ok := upstream.(service.UpstreamKeyConcurrency)
		require.True(t, ok)
		_, release, err := authority.AcquireUpstreamKeySlots(t.Context(), []string{fingerprint})
		require.Nil(t, release)
		require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity, "restarted transports must retain persisted occupancy")
	}
	require.Zero(t, rdb.DBSize(t.Context()).Val(), "regional occupancy must never spill into Redis")
}

func TestRegionalUpstreamKeyPolicyRevisionExpiryAndRecovery(t *testing.T) {
	rdb := regionalKeyRedis(t)
	cfg := upstreamKeyTestConfig(1, 4)
	p, l, g := regionalKeyFixture(t, cfg, rdb, 2)
	old, err := p.primary.ReadUpstreamPolicy(t.Context())
	require.NoError(t, err)
	acquire := func(cache *regionalConcurrencyCache, policy *config.Config, fingerprint string) (func(), error) {
		t.Helper()
		s := &httpUpstreamService{upstreamKeyLimiter: newUpstreamKeyLimiter(policy, rdb, cache)}
		_, release, err := s.AcquireUpstreamKeySlots(t.Context(), []string{fingerprint})
		return release, err
	}
	release, err := acquire(l, cfg, g.Resource.Fingerprint)
	require.NoError(t, err)
	require.NotNil(t, release)
	t.Cleanup(release)
	var peerRequest string
	require.NoError(t, l.database().QueryRow(`SELECT request_id FROM regional_local_slots WHERE NOT released`).Scan(&peerRequest))
	conflicting := upstreamKeyTestConfig(1, 100)
	require.ErrorIs(t, p.primary.PublishUpstreamPolicy(t.Context(), conflicting.Gateway.UpstreamKeyConcurrency), slots.ErrConflict)
	shrink := upstreamKeyTestConfig(2, 1)
	require.NoError(t, p.primary.PublishUpstreamPolicy(t.Context(), shrink.Gateway.UpstreamKeyConcurrency))
	_, err = acquire(p, cfg, g.Resource.Fingerprint)
	require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy)
	_, err = acquire(p, shrink, g.Resource.Fingerprint)
	require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)
	next := g
	next.Revision++
	next.ExpiresAt = next.ExpiresAt.Add(time.Minute)
	require.ErrorIs(t, p.primary.Renew(t.Context(), next), slots.ErrUnavailable, "reduction cannot renew reserved capacity above the new limit")
	// A missing or stale policy never means unlimited, even for an unlisted key.
	_, err = l.database().Exec(`UPDATE regional_upstream_key_policy_cache SET issued_at=clock_timestamp()-interval '1 minute',expires_at=clock_timestamp()-interval '1 second'`)
	require.NoError(t, err)
	for _, key := range []string{g.Resource.Fingerprint, config.UpstreamKeyFingerprint("unlisted-fixture")} {
		_, err := acquire(l, cfg, key)
		require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy)
	}
	held, err := l.local.Held(t.Context(), g.Resource, peerRequest)
	require.NoError(t, err)
	require.True(t, held, "policy expiry must not release a peer's admitted request")
	snapshot, err := p.primary.ReadUpstreamPolicy(t.Context())
	require.NoError(t, err)
	require.NoError(t, l.local.InstallUpstreamPolicy(t.Context(), snapshot))
	require.ErrorIs(t, l.local.InstallUpstreamPolicy(t.Context(), old), slots.ErrConflict)
	newRelease, err := acquire(l, shrink, g.Resource.Fingerprint)
	if newRelease != nil {
		t.Cleanup(newRelease)
	}
	require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity, "a fresh reduced policy must not admit new work from an overdrawn old grant")
	_, err = acquire(l, cfg, g.Resource.Fingerprint)
	require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy, "an older process cannot reinterpret the new policy")
	// Removing all rules is explicit, versioned evidence, not cache absence.
	removed := &config.Config{}
	removed.Gateway.UpstreamKeyConcurrency.Revision = 3
	require.NoError(t, p.primary.PublishUpstreamPolicy(t.Context(), removed.Gateway.UpstreamKeyConcurrency))
	snapshot, err = p.primary.ReadUpstreamPolicy(t.Context())
	require.NoError(t, err)
	require.NoError(t, l.local.InstallUpstreamPolicy(t.Context(), snapshot))
	for _, cache := range []*regionalConcurrencyCache{p, l} {
		unrestricted, err := acquire(cache, removed, g.Resource.Fingerprint)
		require.NoError(t, err)
		require.Nil(t, unrestricted)
	}
	release()
	count, err := l.local.Count(t.Context(), g.Resource)
	require.NoError(t, err)
	require.Zero(t, count)
	// Re-enabling the rule retains the same registry and reserved budget.
	restored := upstreamKeyTestConfig(4, 4)
	require.NoError(t, p.primary.PublishUpstreamPolicy(t.Context(), restored.Gateway.UpstreamKeyConcurrency))
	snapshot, err = p.primary.ReadUpstreamPolicy(t.Context())
	require.NoError(t, err)
	require.Equal(t, g.Resource.ID, snapshot.Resources[g.Resource.Fingerprint])
	require.NoError(t, l.local.InstallUpstreamPolicy(t.Context(), snapshot))
	resumed, err := acquire(l, restored, g.Resource.Fingerprint)
	require.NoError(t, err)
	require.NotNil(t, resumed)
	resumed()
}

func TestRegionalUpstreamKeyDrainAndIdentityConflicts(t *testing.T) {
	rdb := regionalKeyRedis(t)
	cfg := upstreamKeyTestConfig(1, 2)
	p, l, g := regionalKeyFixture(t, cfg, rdb, 1)
	bad := g
	bad.LeaseID = uuid.NewString()
	bad.Resource.Fingerprint = config.UpstreamKeyFingerprint("different-fixture")
	require.ErrorIs(t, p.primary.Issue(t.Context(), bad), slots.ErrConflict)
	require.ErrorIs(t, l.local.Install(t.Context(), bad), slots.ErrConflict)
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	lease, err := newUpstreamKeyLimiter(cfg, rdb, l).acquire(ctx, g.Resource.Fingerprint, cancel)
	require.NoError(t, err)
	t.Cleanup(lease.release)
	require.NoError(t, p.primary.BeginDrain(t.Context(), g.Region, g.LeaseID))
	require.NoError(t, l.local.Drain(t.Context(), g.LeaseID))
	_, err = l.local.Close(t.Context(), g.LeaseID)
	require.ErrorIs(t, err, slots.ErrUnavailable)
	count, err := p.primary.Count(t.Context(), g.Resource)
	require.NoError(t, err)
	require.Equal(t, 1, count, "draining capacity is still reserved on the authority")
	lease.release()
	lease.release()
	receipt, err := l.local.Close(t.Context(), g.LeaseID)
	require.NoError(t, err)
	require.NoError(t, p.primary.Close(t.Context(), g.Region, receipt))
	require.NoError(t, p.primary.Close(t.Context(), g.Region, receipt))
	require.NoError(t, l.local.Install(t.Context(), g))
	ok, err := l.local.Acquire(t.Context(), g.LeaseID, "stale-token")
	require.False(t, ok)
	require.ErrorIs(t, err, slots.ErrUnavailable)
	count, err = p.primary.Count(t.Context(), g.Resource)
	require.NoError(t, err)
	require.Zero(t, count)
}

func TestRegionalUpstreamKeyHTTPBodyLifetime(t *testing.T) {
	for _, mode := range []string{"primary", "regional"} {
		t.Run(mode, func(t *testing.T) {
			rdb := regionalKeyRedis(t)
			cfg := upstreamKeyTestConfig(1, 1)
			capacity := 0
			if mode == "regional" {
				capacity = 1
			}
			p, l, _ := regionalKeyFixture(t, cfg, rdb, capacity)
			cache := p
			if capacity > 0 {
				cache = l
			}
			var requests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				_, _ = io.WriteString(w, "data: first\n\n")
				if http.NewResponseController(w).Flush() != nil {
					return
				}
				<-r.Context().Done()
			}))
			t.Cleanup(upstream.Close)
			port := ProvideHTTPUpstream(cfg, nil, rdb, cache)
			request := func(ctx context.Context) *http.Request {
				t.Helper()
				r, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL+"/v1/responses", strings.NewReader("{}"))
				require.NoError(t, err)
				r.Header.Set("Authorization", "Bearer supplier-fixture")
				return r
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			response, err := port.Do(request(ctx), "", 1, 50)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, response.Body.Close()) })
			blocked, err := port.Do(request(t.Context()), "", 99, 500)
			require.Nil(t, blocked)
			require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)
			require.EqualValues(t, 1, requests.Load())
			cancel()
			require.Eventually(t, func() bool {
				var held int
				table := "regional_primary_slots"
				if capacity > 0 {
					table = "regional_local_slots"
				}
				err := cache.database().QueryRow(`SELECT count(*) FROM ` + table + ` WHERE NOT released`).Scan(&held)
				return err == nil && held == 0
			}, time.Second, time.Millisecond)
			require.NoError(t, response.Body.Close())
			next, err := port.Do(request(t.Context()), "", 99, 500)
			require.NoError(t, err)
			require.NoError(t, next.Body.Close())
		})
	}
}

func upstreamKeyWSAuthority(t *testing.T, cfg *config.Config, rdb *redis.Client, mode string) (service.ConcurrencyCache, func(context.Context) int64, func(context.Context) error) {
	t.Helper()
	key := "concurrency:upstream_key:slots:" + config.UpstreamKeyFingerprint("supplier-fixture")
	if mode == "redis" {
		return nil, func(ctx context.Context) int64 { return rdb.ZCard(ctx, key).Val() }, func(ctx context.Context) error { return rdb.Del(ctx, key).Err() }
	}
	capacity := 0
	if mode == "regional" {
		capacity = 1
	}
	p, l, g := regionalKeyFixture(t, cfg, rdb, capacity)
	cache, table := p, "regional_primary_slots"
	if capacity > 0 {
		cache, table = l, "regional_local_slots"
	}
	count := func(ctx context.Context) int64 {
		var n int64
		err := cache.database().QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE kind=$1 AND resource_id=$2 AND NOT released`, g.Resource.Kind, g.Resource.ID).Scan(&n)
		if err != nil {
			t.Logf("regional occupancy read failed: %v", err)
			return -1
		}
		return n
	}
	lose := func(ctx context.Context) error {
		var request string
		if err := cache.database().QueryRowContext(ctx, `SELECT request_id FROM `+table+` WHERE kind=$1 AND resource_id=$2 AND NOT released`, g.Resource.Kind, g.Resource.ID).Scan(&request); err != nil {
			return err
		}
		return cache.releaseUpstreamKeySlot(ctx, g.Resource, request)
	}
	return cache, count, lose
}

func TestRegionalUpstreamKeyNativeWebSocketSharedLifetime(t *testing.T) {
	for _, mode := range []string{"primary", "regional"} {
		t.Run(mode, func(t *testing.T) { testUpstreamKeyNativeWebSocketSharedLifetime(t, mode) })
	}
}

func TestRegionalUpstreamKeyWebSocketIngressSharedLifetime(t *testing.T) {
	for _, authority := range []string{"primary", "regional"} {
		for _, mode := range []string{service.OpenAIWSIngressModeHTTPBridge, service.OpenAIWSIngressModeCtxPool, service.OpenAIWSIngressModePassthrough} {
			for _, loss := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/lease_loss=%t", authority, mode, loss), func(t *testing.T) {
					testUpstreamKeyWSIngressLifetime(t, mode, loss, authority)
				})
			}
		}
	}
}

func TestRegionalUpstreamKeyAuthenticatedPolicyAndBroker(t *testing.T) {
	rdb := regionalKeyRedis(t)
	cfg := upstreamKeyTestConfig(1, 2)
	p, l, g := regionalKeyFixture(t, cfg, rdb, 0)
	p.primary.GrantGuard = func(context.Context, *sql.Tx) error { return nil }
	roots, serverCert, clientCert, otherClient := regionalControlTLS(t)
	scope := control.Scope{Region: "us", CertificateSHA256: regionalControlPin(clientCert), AccountIDs: []int64{g.Resource.ID}, APIKeyIDs: []int64{g.Resource.ID}, MaximumGrant: 1_000_000_000, MaximumSlots: 1, MaximumLifetime: time.Minute}
	startServer := func(scope control.Scope) *httptest.Server {
		t.Helper()
		handler, err := control.NewSlotsHandler(scope, p.primary)
		require.NoError(t, err)
		server := httptest.NewUnstartedServer(handler)
		server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots, Certificates: []tls.Certificate{serverCert}}
		server.StartTLS()
		t.Cleanup(server.Close)
		return server
	}
	server := startServer(scope)
	client, err := control.NewClient(server.URL, roots, clientCert, regionalControlPin(serverCert))
	require.NoError(t, err)
	t.Cleanup(client.Close)
	g.Slots = 1
	require.Error(t, client.IssueSlots(t.Context(), g), "an account/downstream assignment cannot authorize upstream credentials")
	snapshot, err := client.UpstreamPolicy(t.Context())
	require.NoError(t, err)
	require.NoError(t, l.local.InstallUpstreamPolicy(t.Context(), snapshot))
	wrong, err := control.NewClient(server.URL, roots, otherClient, regionalControlPin(serverCert))
	require.NoError(t, err)
	t.Cleanup(wrong.Close)
	_, err = wrong.UpstreamPolicy(t.Context())
	require.Error(t, err, "CA membership alone does not authorize policy access")
	scope.UpstreamKeyIDs = []int64{g.Resource.ID}
	authorized := startServer(scope)
	scope.UpstreamKeyIDs[0]++ // The handler retains its immutable assignment.
	assigned, err := control.NewClient(authorized.URL, roots, clientCert, regionalControlPin(serverCert))
	require.NoError(t, err)
	t.Cleanup(assigned.Close)
	broker := &control.Broker{DB: l.database(), Slots: l.local, Remote: assigned}
	for range 2 {
		require.NoError(t, broker.EnsureSlots(t.Context(), g.Resource, 1, time.Minute))
		require.NoError(t, broker.RunOnce(t.Context()))
	}
	count, err := p.primary.Count(t.Context(), g.Resource)
	require.NoError(t, err)
	require.Equal(t, 1, count, "idempotent broker replay cannot duplicate reserved capacity")
	_, release, err := (&httpUpstreamService{upstreamKeyLimiter: newUpstreamKeyLimiter(cfg, rdb, l)}).AcquireUpstreamKeySlots(t.Context(), []string{g.Resource.Fingerprint})
	require.NoError(t, err)
	require.NotNil(t, release)
	release()
}

func TestRegionalUpstreamKeySharedLeaseDedupAndPartialRollback(t *testing.T) {
	for _, mode := range []string{"primary", "regional"} {
		t.Run(mode, func(t *testing.T) {
			rdb := regionalKeyRedis(t)
			cfg := upstreamKeyTestConfig(1, 1)
			first := config.UpstreamKeyFingerprint("supplier-fixture")
			second := config.UpstreamKeyFingerprint("second-fixture")
			cfg.Gateway.UpstreamKeyConcurrency.Limits[second] = 1
			capacity := 0
			if mode == "regional" {
				capacity = 1
			}
			p, l, g := regionalKeyFixture(t, cfg, rdb, capacity)
			cache := p
			if capacity > 0 {
				cache = l
				snapshot, err := p.primary.ReadUpstreamPolicy(t.Context())
				require.NoError(t, err)
				other := g
				other.LeaseID = uuid.NewString()
				other.Resource.Fingerprint = second
				other.Resource.ID = snapshot.Resources[second]
				require.NoError(t, p.primary.Issue(t.Context(), other))
				require.NoError(t, l.local.Install(t.Context(), other))
			}
			s := &httpUpstreamService{upstreamKeyLimiter: newUpstreamKeyLimiter(cfg, rdb, cache)}
			_, peer, err := s.AcquireUpstreamKeySlots(t.Context(), []string{second, second})
			require.NoError(t, err)
			require.NotNil(t, peer)
			t.Cleanup(peer)
			_, release, err := s.AcquireUpstreamKeySlots(t.Context(), []string{first, first, second, second})
			require.Nil(t, release)
			require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)
			// The earlier acquisition was rolled back; the exact peer is retained.
			_, firstRelease, err := s.AcquireUpstreamKeySlots(t.Context(), []string{first, first, first})
			require.NoError(t, err)
			require.NotNil(t, firstRelease)
			firstRelease()
			firstRelease()
			_, release, err = s.AcquireUpstreamKeySlots(t.Context(), []string{second})
			require.Nil(t, release)
			require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)
			peer()
			_, release, err = s.AcquireUpstreamKeySlots(t.Context(), []string{second, first, second})
			require.NoError(t, err)
			require.NotNil(t, release)
			release()
			if mode == "primary" {
				cache.primary.GrantGuard = func(context.Context, *sql.Tx) error { return fmt.Errorf("fixture legacy requests not drained") }
				_, release, err = s.AcquireUpstreamKeySlots(t.Context(), []string{first})
				require.Nil(t, release)
				require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy)
				cache.primary.GrantGuard = nil
			}
			cache.runtimeFailed.Store(true)
			_, release, err = s.AcquireUpstreamKeySlots(t.Context(), []string{first})
			require.Nil(t, release)
			require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy)
			cache.runtimeFailed.Store(false)
			_, release, err = s.AcquireUpstreamKeySlots(t.Context(), []string{first})
			require.NoError(t, err)
			require.NotNil(t, release)
			release()
		})
	}
}

func TestRegionalUpstreamKeyAdditiveMigrationAndNumericNamespace(t *testing.T) {
	db := regionalKeyTestDB(t)
	p := NewRegionalSlotAuthority(db)
	policy := upstreamKeyTestConfig(1, 1).Gateway.UpstreamKeyConcurrency
	require.NoError(t, p.PublishUpstreamPolicy(t.Context(), policy))
	snapshot, err := p.ReadUpstreamPolicy(t.Context())
	require.NoError(t, err)
	fingerprint := config.UpstreamKeyFingerprint("supplier-fixture")
	id := snapshot.Resources[fingerprint]
	_, err = db.Exec(`CREATE TABLE users(id BIGINT PRIMARY KEY, concurrency INTEGER NOT NULL, status TEXT NOT NULL, deleted_at TIMESTAMPTZ)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO users(id,concurrency,status) VALUES($1,1,'active')`, id)
	require.NoError(t, err)
	legacy := slots.Resource{Kind: "user", ID: id}
	ok, err := p.Acquire(t.Context(), legacy, "legacy-user")
	require.NoError(t, err)
	require.True(t, ok)
	resource := slots.Resource{Kind: "upstream_key", ID: id, Fingerprint: fingerprint}
	ok, err = p.Acquire(t.Context(), resource, "upstream-key")
	require.NoError(t, err)
	require.True(t, ok, "equal numeric IDs in different namespaces cannot share or multiply budgets")
	raw, err := os.ReadFile(filepath.Join("..", "..", "migrations", "327_regional_upstream_key_slots.sql"))
	require.NoError(t, err)
	_, err = db.Exec(string(raw))
	require.NoError(t, err, "replaying the additive migration must preserve prior allocations")
	for _, r := range []slots.Resource{legacy, resource} {
		count, err := p.Count(t.Context(), r)
		require.NoError(t, err)
		require.Equal(t, 1, count)
	}
	require.NoError(t, p.Release(t.Context(), legacy, "legacy-user"))
	require.NoError(t, p.Release(t.Context(), resource, "upstream-key"))
}

func TestRegionalUpstreamKeyMissingPolicyNeverMeansUnlimited(t *testing.T) {
	rdb := regionalKeyRedis(t)
	cfg := &config.Config{}
	p, l, _ := regionalKeyFixture(t, cfg, rdb, 0)
	fingerprint := config.UpstreamKeyFingerprint("supplier-fixture")
	for _, cache := range []*regionalConcurrencyCache{p, l} {
		s := &httpUpstreamService{upstreamKeyLimiter: newUpstreamKeyLimiter(cfg, rdb, cache)}
		require.NotNil(t, s.upstreamKeyLimiter)
		_, release, err := s.AcquireUpstreamKeySlots(t.Context(), []string{fingerprint})
		require.NoError(t, err)
		require.Nil(t, release, "confirmed empty policy retains legacy behavior")
	}
	_, err := l.database().Exec(`DELETE FROM regional_upstream_key_policy_cache`)
	require.NoError(t, err)
	s := &httpUpstreamService{upstreamKeyLimiter: newUpstreamKeyLimiter(cfg, rdb, l)}
	_, release, err := s.AcquireUpstreamKeySlots(t.Context(), []string{fingerprint})
	require.Nil(t, release)
	require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy)
	require.NoError(t, p.primary.PublishUpstreamPolicy(t.Context(), upstreamKeyTestConfig(1, 1).Gateway.UpstreamKeyConcurrency))
	primaryPort := &httpUpstreamService{upstreamKeyLimiter: newUpstreamKeyLimiter(cfg, rdb, p)}
	_, release, err = primaryPort.AcquireUpstreamKeySlots(t.Context(), []string{fingerprint})
	require.Nil(t, release)
	require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy, "a process without the configured rules must reject a newer authority")
}

func TestRegionalUpstreamKeyLegacyCutoverIncludesCredentialSlots(t *testing.T) {
	server := miniredis.RunT(t)
	// Only ACL/CLIENT metadata is stubbed. SCAN/ZCARD and SQL are real adapters;
	// the existing integration harness covers actual Redis ACL revocation.
	server.Server().SetPreHook(func(peer *miniserver.Peer, command string, args ...string) bool {
		if command == "ACL" {
			peer.WriteNull()
			return true
		}
		if command == "CLIENT" && len(args) == 1 && strings.EqualFold(args[0], "LIST") {
			peer.WriteBulk("")
			return true
		}
		return false
	})
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	db := regionalKeyTestDB(t)
	cfg := RegionalConcurrencyRuntimeConfig{Epoch: "upstream-cutover", PrimaryInstances: []string{"one"}, LegacyRedisUser: "old"}
	_, err := db.Exec(`INSERT INTO regional_runtime_members(epoch,instance_id,role) VALUES($1,'one','primary')`, cfg.Epoch)
	require.NoError(t, err)
	key := upstreamKeySlotKeyPrefix + config.UpstreamKeyFingerprint("supplier-fixture")
	require.NoError(t, rdb.ZAdd(t.Context(), key, redis.Z{Score: 1, Member: "legacy-inflight"}).Err())
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	guard := regionalLegacyGuard(rdb, cfg)
	require.EqualError(t, guard(t.Context(), tx), "legacy concurrency namespace is not drained")
	require.NoError(t, rdb.Del(t.Context(), key).Err())
	require.NoError(t, guard(t.Context(), tx))
}

func TestRegionalUpstreamKeyActualPolicyDeadlineAndDatabaseRecovery(t *testing.T) {
	rdb := regionalKeyRedis(t)
	cfg := upstreamKeyTestConfig(1, 2)
	p, l, g := regionalKeyFixture(t, cfg, rdb, 1)
	snapshot, err := p.primary.ReadUpstreamPolicy(t.Context())
	require.NoError(t, err)
	snapshot.ExpiresAt = snapshot.IssuedAt.Add(750 * time.Millisecond)
	require.NoError(t, l.local.InstallUpstreamPolicy(t.Context(), snapshot))
	s := &httpUpstreamService{upstreamKeyLimiter: newUpstreamKeyLimiter(cfg, rdb, l)}
	_, release, err := s.AcquireUpstreamKeySlots(t.Context(), []string{g.Resource.Fingerprint})
	require.NoError(t, err)
	require.NotNil(t, release)
	t.Cleanup(release)
	require.Eventually(t, func() bool {
		_, retryRelease, err := s.AcquireUpstreamKeySlots(t.Context(), []string{config.UpstreamKeyFingerprint("unlisted-fixture")})
		if retryRelease != nil {
			retryRelease()
		}
		return err == service.ErrUpstreamKeyPolicy
	}, 2*time.Second, 10*time.Millisecond, "actual authority time must expire the policy for unlisted credentials too")
	release()
	_, err = l.database().Exec(`ALTER TABLE regional_upstream_key_policy_cache RENAME TO policy_cache_offline`)
	require.NoError(t, err)
	_, retryRelease, err := s.AcquireUpstreamKeySlots(t.Context(), []string{g.Resource.Fingerprint})
	require.Nil(t, retryRelease)
	require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy)
	require.Zero(t, rdb.DBSize(t.Context()).Val(), "SQL failure must not obtain a fallback Redis slot")
	_, err = l.database().Exec(`ALTER TABLE policy_cache_offline RENAME TO regional_upstream_key_policy_cache`)
	require.NoError(t, err)
	snapshot, err = p.primary.ReadUpstreamPolicy(t.Context())
	require.NoError(t, err)
	require.NoError(t, l.local.InstallUpstreamPolicy(t.Context(), snapshot))
	_, recovered, err := s.AcquireUpstreamKeySlots(t.Context(), []string{g.Resource.Fingerprint})
	require.NoError(t, err)
	require.NotNil(t, recovered)
	recovered()
}

func TestRegionalUpstreamKeySharedLeaseCompetition(t *testing.T) {
	rdb := regionalKeyRedis(t)
	cfg := upstreamKeyTestConfig(1, 4)
	p, l, grant := regionalKeyFixture(t, cfg, rdb, 2)
	type result struct {
		local bool
		lease *upstreamKeyLease
		err   error
	}
	results := make(chan result, 16)
	var wg sync.WaitGroup
	for _, cache := range []*regionalConcurrencyCache{p, l} {
		limiter := newUpstreamKeyLimiter(cfg, rdb, cache)
		for range 8 {
			wg.Go(func() {
				ctx, cancel := context.WithCancelCause(t.Context())
				t.Cleanup(func() { cancel(nil) })
				lease, err := limiter.acquire(ctx, grant.Resource.Fingerprint, cancel)
				results <- result{cache.local != nil, lease, err}
			})
		}
	}
	wg.Wait()
	close(results)
	counts := map[bool]int{}
	for result := range results {
		if result.lease != nil {
			counts[result.local]++
			t.Cleanup(result.lease.release)
		} else {
			require.ErrorIs(t, result.err, service.ErrUpstreamKeyCapacity)
		}
	}
	require.Equal(t, map[bool]int{false: 2, true: 2}, counts)
}

func TestRegionalUpstreamKeyReductionIncludesOtherRegionReservations(t *testing.T) {
	rdb := regionalKeyRedis(t)
	cfg := upstreamKeyTestConfig(1, 2)
	p, l, g := regionalKeyFixture(t, cfg, rdb, 1)
	other := g
	other.LeaseID = uuid.NewString()
	other.Region = "eu"
	require.NoError(t, p.primary.Issue(t.Context(), other))
	shrink := upstreamKeyTestConfig(2, 1)
	require.NoError(t, p.primary.PublishUpstreamPolicy(t.Context(), shrink.Gateway.UpstreamKeyConcurrency))
	snapshot, err := p.primary.ReadUpstreamPolicy(t.Context())
	require.NoError(t, err)
	require.True(t, snapshot.Overdrawn[g.Resource.Fingerprint], "one region's local grant size does not prove the whole budget fits")
	require.NoError(t, l.local.InstallUpstreamPolicy(t.Context(), snapshot))
	s := &httpUpstreamService{upstreamKeyLimiter: newUpstreamKeyLimiter(shrink, rdb, l)}
	_, release, err := s.AcquireUpstreamKeySlots(t.Context(), []string{g.Resource.Fingerprint})
	if release != nil {
		t.Cleanup(release)
	}
	require.Nil(t, release)
	require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)
	require.NoError(t, p.primary.BeginDrain(t.Context(), g.Region, g.LeaseID))
	require.NoError(t, l.local.Drain(t.Context(), g.LeaseID))
	receipt, err := l.local.Close(t.Context(), g.LeaseID)
	require.NoError(t, err)
	require.NoError(t, p.primary.Close(t.Context(), g.Region, receipt))
	snapshot, err = p.primary.ReadUpstreamPolicy(t.Context())
	require.NoError(t, err)
	require.False(t, snapshot.Overdrawn[g.Resource.Fingerprint])
	count, err := p.primary.Count(t.Context(), g.Resource)
	require.NoError(t, err)
	require.Equal(t, 1, count, "a local drain must preserve the other region's reservation")
}
