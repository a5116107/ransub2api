//go:build integration

package repository

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/regional/control"
	"github.com/Wei-Shaw/sub2api/internal/regional/video"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRegionalAsyncVideoDirectoryAuthenticatedRoundTrip(t *testing.T) {
	t.Setenv("SUB2API_REGIONAL_TEST_DSN", integrationPostgresDSN)
	rdb := regionalKeyRedis(t)
	cfg := upstreamKeyTestConfig(1, 8)
	cfg.Gateway.OpenAIWS.StickySessionTTLSeconds = 48 * 60 * 60
	primary, local, grant := regionalKeyFixture(t, cfg, rdb, 4)
	primary.primary.GrantGuard = func(context.Context, *sql.Tx) error { return nil }
	roots, serverCert, clientCert, otherClient := regionalControlTLS(t)
	owner := asyncVideoLeaseOwner()
	scope := control.Scope{Region: local.local.Region, CertificateSHA256: regionalControlPin(clientCert),
		UserIDs: []int64{owner.UserID}, APIKeyIDs: []int64{owner.APIKeyID}, AccountIDs: []int64{owner.AccountID},
		MaximumGrant: 1_000_000_000, MaximumSlots: 8, MaximumLifetime: time.Minute}
	scope.UpstreamKeyIDs = []int64{grant.Resource.ID}
	identityConfig := func(identity tls.Certificate) RegionalConcurrencyRuntimeConfig {
		t.Helper()
		dir := t.TempDir()
		key, err := x509.MarshalPKCS8PrivateKey(identity.PrivateKey)
		require.NoError(t, err)
		files := map[string][]byte{
			"ca.pem":   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: identity.Certificate[1]}),
			"cert.pem": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: identity.Certificate[0]}),
			"key.pem":  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}),
		}
		for name, raw := range files {
			require.NoError(t, os.WriteFile(filepath.Join(dir, name), raw, 0600))
		}
		return RegionalConcurrencyRuntimeConfig{CAFile: filepath.Join(dir, "ca.pem"), CertFile: filepath.Join(dir, "cert.pem"), KeyFile: filepath.Join(dir, "key.pem")}
	}
	primaryConfig := identityConfig(serverCert)
	primaryConfig.Mode, primaryConfig.Listen, primaryConfig.Scope = "primary", "127.0.0.1:0", scope
	primary.runtimeConfig = &primaryConfig
	require.NoError(t, primary.prepareControl())
	endpoint := "https://" + primary.controlListener.Addr().String()
	localConfig := identityConfig(clientCert)
	localConfig.Mode, localConfig.Endpoint, localConfig.Region = "regional", endpoint, local.local.Region
	localConfig.ServerPin, localConfig.LifetimeSeconds = regionalControlPin(serverCert), 60
	localConfig.Targets = []RegionalSlotTarget{{Resource: grant.Resource, Capacity: grant.Slots}}
	local.runtimeConfig = &localConfig
	require.NoError(t, local.prepareControl())
	t.Cleanup(local.brokerClient.Close)
	client := local.brokerClient
	runtimeCtx, stopRuntime := context.WithCancel(t.Context())
	runtimeDone := make(chan struct{})
	go func() { defer close(runtimeDone); primary.RunRegionalRuntime(runtimeCtx) }()
	t.Cleanup(func() {
		stopRuntime()
		select {
		case <-runtimeDone:
		case <-time.After(12 * time.Second):
			t.Error("regional runtime failed to stop")
		}
	})
	primaryUpstream := liveKeyTestUpstream(t, cfg, rdb, primary)
	localUpstream := liveKeyTestUpstream(t, cfg, rdb, local)
	bind := func(upstream *httpUpstreamService, owner service.AsyncVideoKeyOwner, id string) *service.AsyncVideoKeyHandoff {
		t.Helper()
		handoff := service.NewAsyncVideoKeyHandoff(owner)
		_, finish, err := upstream.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, handoff))
		require.NoError(t, err)
		require.NoError(t, upstream.BindAsyncVideoKeyLease(t.Context(), handoff, id))
		finish(nil, nil)
		return handoff
	}
	primaryTask := bind(primaryUpstream, owner, "primary-only")
	localTask := bind(localUpstream, owner, "local-only")
	principal := owner
	principal.AccountID, principal.RouteFingerprint = 0, [32]byte{}
	for _, tc := range []struct {
		upstream *httpUpstreamService
		task     string
	}{{primaryUpstream, "primary-only"}, {localUpstream, "local-only"}} {
		_, err := tc.upstream.ResolveAsyncVideoKeyOwner(t.Context(), principal, tc.task)
		require.ErrorIs(t, err, video.ErrUnavailable, "bootstrap cannot guess directory completeness")
	}
	// Exercise the production regional worker, not just the sync method. Its
	// existing authenticated slot/policy cycle must also install the directory.
	localCtx, stopLocal := context.WithCancel(t.Context())
	localDone := make(chan struct{})
	go func() { defer close(localDone); local.RunRegionalRuntime(localCtx) }()
	t.Cleanup(func() {
		stopLocal()
		select {
		case <-localDone:
		case <-time.After(12 * time.Second):
			t.Error("regional worker failed to stop")
		}
	})
	require.Eventually(t, func() bool {
		resolved, err := localUpstream.ResolveAsyncVideoKeyOwner(t.Context(), principal, "local-only")
		return err == nil && resolved != nil && *resolved == owner
	}, 10*time.Second, 20*time.Millisecond, "the background worker must install a complete directory")
	stopLocal()
	select {
	case <-localDone:
	case <-time.After(12 * time.Second):
		t.Fatal("regional worker failed to stop")
	}
	for _, tc := range []struct {
		upstream     *httpUpstreamService
		own, foreign string
	}{{primaryUpstream, "primary-only", "local-only"}, {localUpstream, "local-only", "primary-only"}} {
		resolved, err := tc.upstream.ResolveAsyncVideoKeyOwner(t.Context(), principal, tc.own)
		require.NoError(t, err)
		require.Equal(t, owner, *resolved)
		_, err = tc.upstream.ResolveAsyncVideoKeyOwner(t.Context(), principal, tc.foreign)
		require.ErrorIs(t, err, video.ErrTaskNotLocal)
		_, err = tc.upstream.LoadAsyncVideoKeyLeaseByTask(t.Context(), owner, tc.foreign)
		require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy, "directory metadata never installs a remote lease")
		_, err = tc.upstream.ResolveAsyncVideoKeyOwner(t.Context(), principal, "not-yet-exported")
		require.ErrorIs(t, err, video.ErrUnavailable, "absence must not re-enable sticky fallback")
		wrong := principal
		wrong.APIKeyID++
		_, err = tc.upstream.ResolveAsyncVideoKeyOwner(t.Context(), wrong, tc.own)
		require.ErrorIs(t, err, video.ErrUnavailable)
	}
	// A receiver restart keeps both the installed directory and its revision floor.
	restartedCache, err := NewRegionalConcurrencyCache(local.ConcurrencyCache, nil, local.local)
	require.NoError(t, err)
	restarted, ok := restartedCache.(*regionalConcurrencyCache)
	require.True(t, ok)
	restarted.runtimeConfig = local.runtimeConfig
	restartedUpstream := liveKeyTestUpstream(t, cfg, rdb, restarted)
	_, err = restartedUpstream.ResolveAsyncVideoKeyOwner(t.Context(), principal, "primary-only")
	require.ErrorIs(t, err, video.ErrTaskNotLocal)
	// Failed authentication cannot publish even a newer revision.
	report, err := local.exportAsyncVideoDirectory(t.Context())
	require.NoError(t, err)
	wrongPeer, err := control.NewClient(endpoint, roots, otherClient, regionalControlPin(serverCert))
	require.NoError(t, err)
	t.Cleanup(wrongPeer.Close)
	_, err = wrongPeer.SyncVideoDirectory(t.Context(), report)
	require.Error(t, err)
	foreignReport := report
	foreignReport.Authority = video.LocalAuthority("eu")
	_, err = client.SyncVideoDirectory(t.Context(), foreignReport)
	require.Error(t, err)
	outOfScope := report
	outOfScope.Entries = append([]video.Entry(nil), report.Entries...)
	outOfScope.Entries[0].Owner.APIKeyID++
	_, err = client.SyncVideoDirectory(t.Context(), outOfScope)
	require.Error(t, err)
	// Primary tasks outside the peer's immutable assignment are not disclosed.
	other := owner
	other.UserID++
	otherTask := bind(primaryUpstream, other, "other-tenant")
	directory, err := client.SyncVideoDirectory(t.Context(), report)
	require.NoError(t, err)
	require.NoError(t, directory.Validate(local.local.Region))
	encoded, err := json.Marshal(directory)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "primary-only", "raw provider task IDs must not cross the control channel")
	require.NotContains(t, string(encoded), "supplier-fixture", "credential data must not cross the directory channel")
	for _, snapshot := range directory.Snapshots {
		for _, entry := range snapshot.Entries {
			require.Equal(t, owner.UserID, entry.Owner.UserID)
		}
	}
	// A second authority cannot silently win an identical tenant/task key.
	conflict := bind(primaryUpstream, owner, "local-only")
	require.NoError(t, local.refreshAsyncVideoDirectory(t.Context()))
	for _, upstream := range []*httpUpstreamService{primaryUpstream, localUpstream} {
		_, err := upstream.ResolveAsyncVideoKeyOwner(t.Context(), principal, "local-only")
		require.ErrorIs(t, err, video.ErrConflict)
	}
	require.NoError(t, primaryUpstream.ReleaseAsyncVideoKeyLeaseByMember(t.Context(), conflict.Member()))
	require.NoError(t, local.refreshAsyncVideoDirectory(t.Context()))
	_, err = localUpstream.ResolveAsyncVideoKeyOwner(t.Context(), principal, "local-only")
	require.NoError(t, err, "the full replacement removes terminal conflicts")
	prepared := httptest.NewRequest(http.MethodGet, "https://upstream.example/v1/videos/local-only", nil)
	prepared.Header.Set("Authorization", "Bearer supplier-fixture")
	prepared = service.WithAsyncVideoKeyLookup(prepared.WithContext(service.WithAsyncVideoKeyOwner(t.Context(), owner)), localTask.Lease(), "local-only")
	_, finish, err := localUpstream.BeginUpstreamKeyRequest(prepared)
	require.NoError(t, err)
	finish(nil, nil)
	// A stale directory stops lookups, not exact terminal cleanup of local slots.
	_, err = local.database().ExecContext(t.Context(), `UPDATE regional_async_video_directory_snapshots SET payload=jsonb_set(jsonb_set(payload,'{issued_at}',to_jsonb(clock_timestamp()-interval '1 minute')),'{expires_at}',to_jsonb(clock_timestamp()-interval '31 seconds'))`)
	require.NoError(t, err)
	_, err = restartedUpstream.ResolveAsyncVideoKeyOwner(t.Context(), principal, "local-only")
	require.ErrorIs(t, err, video.ErrUnavailable)
	_, err = localUpstream.LoadAsyncVideoKeyLeaseByTask(t.Context(), owner, "local-only")
	require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy)
	_, finish, err = localUpstream.BeginUpstreamKeyRequest(prepared)
	require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy, "prepared lookups must recheck the directory at final HTTP admission")
	require.Nil(t, finish, "rejected admission must not return a success lifecycle")
	require.NoError(t, localUpstream.ReleaseAsyncVideoKeyLeaseByTask(t.Context(), owner, "local-only"))
	require.NoError(t, primaryUpstream.ReleaseAsyncVideoKeyLeaseByMember(t.Context(), primaryTask.Member()))
	require.NoError(t, primaryUpstream.ReleaseAsyncVideoKeyLeaseByMember(t.Context(), otherTask.Member()))
	require.NoError(t, local.refreshAsyncVideoDirectory(t.Context()))
	resolved, err := localUpstream.ResolveAsyncVideoKeyOwner(t.Context(), principal, "local-only")
	require.NoError(t, err)
	require.Equal(t, owner, *resolved, "completed video content must retain its local account after generation slots are released")
	completedLease, err := localUpstream.LoadAsyncVideoKeyLeaseByTask(t.Context(), owner, "local-only")
	require.NoError(t, err)
	require.Nil(t, completedLease, "a completed receipt must never become a live generation lease")
	_, _, err = localUpstream.BeginUpstreamKeyRequest(prepared)
	require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy, "an old prepared generation lease cannot be revived by a completed receipt")
	completedRequest := httptest.NewRequest(http.MethodGet, "https://upstream.example/v1/videos/local-only", nil)
	completedRequest.Header.Set("Authorization", "Bearer supplier-fixture")
	completedRequest = service.WithAsyncVideoKeyLookup(completedRequest.WithContext(service.WithAsyncVideoKeyOwner(t.Context(), owner)), nil, "local-only")
	_, finish, err = localUpstream.BeginUpstreamKeyRequest(completedRequest)
	require.NoError(t, err)
	finish(nil, nil)
	for _, mutation := range []string{"method", "target", "owner", "credentials"} {
		changed := completedRequest.Clone(completedRequest.Context())
		switch mutation {
		case "method":
			changed.Method = http.MethodPost
		case "target":
			changed.URL.Path += "/different"
		case "owner":
			wrong := owner
			wrong.AccountID++
			changed = changed.WithContext(service.WithAsyncVideoKeyOwner(changed.Context(), wrong))
		case "credentials":
			changed.Header.Set("Authorization", "Bearer other-fixture")
		}
		_, _, err := localUpstream.BeginUpstreamKeyRequest(changed)
		require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy, mutation)
	}
	var retention float64
	require.NoError(t, local.database().QueryRowContext(t.Context(), `SELECT extract(epoch from expires_at-clock_timestamp()) FROM regional_async_video_completed_tasks WHERE authority=$1 AND task_key=$2`, local.persistentUpstreamKeyAuthority(), video.TaskKey(owner, "local-only")).Scan(&retention))
	require.Greater(t, retention, float64(47*60*60), "completed receipts cover the longer existing sticky retention")
	reused := service.NewAsyncVideoKeyHandoff(owner)
	_, finish, err = localUpstream.BeginUpstreamKeyRequest(asyncVideoLeaseRequest(t, reused))
	require.NoError(t, err)
	require.ErrorIs(t, localUpstream.BindAsyncVideoKeyLease(t.Context(), reused, "local-only"), service.ErrUpstreamKeyPolicy, "a reused task ID must not overwrite retained ownership")
	finish(nil, nil)
	_, err = local.database().ExecContext(t.Context(), `UPDATE regional_async_video_completed_tasks SET expires_at=clock_timestamp()-interval '1 second'`)
	require.NoError(t, err)
	_, err = localUpstream.ResolveAsyncVideoKeyOwner(t.Context(), principal, "local-only")
	require.ErrorIs(t, err, video.ErrUnavailable)
	require.NoError(t, local.pruneCompletedAsyncVideoTasks(t.Context()))
	var receipts int
	require.NoError(t, local.database().QueryRowContext(t.Context(), `SELECT count(*) FROM regional_async_video_completed_tasks`).Scan(&receipts))
	require.Zero(t, receipts, "expired local receipts must be reclaimed without modifying active leases")
	require.NoError(t, localUpstream.CheckPersistentUpstreamKeySlots(t.Context(), reused.Lease()), "receipt cleanup must not reclaim an unrelated unconfirmed generation")
	require.NoError(t, localUpstream.ReleaseAsyncVideoKeyLeaseByMember(t.Context(), reused.Member()))
	stopRuntime()
	select {
	case <-runtimeDone:
	case <-time.After(12 * time.Second):
		t.Fatal("regional runtime failed to stop")
	}
	require.Error(t, local.refreshAsyncVideoDirectory(t.Context()), "control failure must not invent a refreshed view")
	// No local record or slot was recreated from metadata or after a restart.
	var count int
	require.NoError(t, local.database().QueryRowContext(t.Context(), `SELECT count(*) FROM regional_async_video_key_leases`).Scan(&count))
	require.Zero(t, count)
}

func TestRegionalAsyncVideoDirectoryRevisionAndTransactionRecovery(t *testing.T) {
	t.Setenv("SUB2API_REGIONAL_TEST_DSN", integrationPostgresDSN)
	db := regionalKeyTestDB(t)
	var now time.Time
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT clock_timestamp()`).Scan(&now))
	base := video.Snapshot{SchemaVersion: video.SchemaVersion, Authority: video.LocalAuthority("us"), Revision: 10, IssuedAt: now, ExpiresAt: now.Add(video.Lifetime), Entries: []video.Entry{}}
	install := func(snapshot video.Snapshot, commit bool) error {
		tx, err := db.BeginTx(t.Context(), nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := installAsyncVideoDirectorySnapshot(t.Context(), tx, video.PrimaryAuthority, snapshot); err != nil {
			return err
		}
		if commit {
			return tx.Commit()
		}
		return nil
	}
	require.NoError(t, install(base, true))
	require.NoError(t, install(base, true), "identical retries are idempotent")
	for _, tc := range []struct {
		name string
		edit func(*video.Snapshot)
	}{
		{"rollback", func(s *video.Snapshot) { s.Revision-- }},
		{"same revision deadline extension", func(s *video.Snapshot) {
			s.IssuedAt = s.IssuedAt.Add(time.Microsecond)
			s.ExpiresAt = s.ExpiresAt.Add(time.Microsecond)
		}},
		{"new revision clock rollback", func(s *video.Snapshot) { s.Revision++; s.IssuedAt = s.IssuedAt.Add(-time.Microsecond) }},
		{"expired", func(s *video.Snapshot) {
			s.Revision++
			s.IssuedAt = s.IssuedAt.Add(-time.Minute)
			s.ExpiresAt = s.IssuedAt.Add(video.Lifetime)
		}},
		{"future", func(s *video.Snapshot) {
			s.Revision++
			s.IssuedAt = s.IssuedAt.Add(time.Hour)
			s.ExpiresAt = s.IssuedAt.Add(video.Lifetime)
		}},
		{"partial", func(s *video.Snapshot) { s.Revision++; s.Entries = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) { s := base; tc.edit(&s); require.Error(t, install(s, true)) })
	}
	next := base
	next.Revision++
	require.NoError(t, install(next, false))
	var revision int64
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT revision FROM regional_async_video_directory_snapshots`).Scan(&revision))
	require.Equal(t, base.Revision, revision, "an interrupted transaction must not move the revision floor")
	var wg sync.WaitGroup
	results := make(chan error, 12)
	for range 12 {
		wg.Go(func() { results <- install(next, true) })
	}
	wg.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	require.ErrorIs(t, install(base, true), video.ErrConflict)
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM regional_async_video_directory_snapshots`).Scan(&revision))
	require.EqualValues(t, 1, revision, "concurrent retries leave one durable source")
}

func TestRegionalAsyncVideoDirectoryMigrationAndBounds(t *testing.T) {
	t.Setenv("SUB2API_REGIONAL_TEST_DSN", integrationPostgresDSN)
	rdb := regionalKeyRedis(t)
	cfg := upstreamKeyTestConfig(1, 1)
	primary, _, _ := regionalKeyFixture(t, cfg, rdb, 0)
	for range 2 {
		raw, err := migrations.FS.ReadFile("332_regional_async_video_directory.sql")
		require.NoError(t, err)
		_, err = primary.database().ExecContext(t.Context(), string(raw))
		require.NoError(t, err)
	}
	upstream := liveKeyTestUpstream(t, cfg, rdb, primary)
	owner := asyncVideoLeaseOwner()
	for i := 0; i <= video.MaxEntries; i++ {
		handoff := service.NewAsyncVideoKeyHandoff(owner)
		handoff.SetLease(&service.PersistentUpstreamKeyLease{Authority: video.PrimaryAuthority, Member: handoff.Member(), Fingerprints: []string{strings.Repeat("a", 64)}, Resources: []service.PersistentUpstreamKeyResource{{Fingerprint: strings.Repeat("a", 64), ResourceID: 1}}})
		require.NoError(t, upstream.PersistAsyncVideoKeyLease(t.Context(), handoff))
		require.NoError(t, upstream.BindAsyncVideoKeyLease(t.Context(), handoff, uuid.NewString()))
	}
	_, err := primary.exportAsyncVideoDirectory(t.Context())
	require.Error(t, err, "an oversized source is rejected, never silently truncated")
	var revisions int
	require.NoError(t, primary.database().QueryRowContext(t.Context(), `SELECT count(*) FROM regional_async_video_directory_revisions`).Scan(&revisions))
	require.Zero(t, revisions, "a failed export cannot publish a revision")
	// Rolling back the application leaves all actual task/lease data intact.
	_, err = primary.database().ExecContext(t.Context(), `DROP TABLE regional_async_video_directory_snapshots,regional_async_video_directory_revisions,regional_async_video_completed_tasks`)
	require.NoError(t, err)
	var tasks int
	require.NoError(t, primary.database().QueryRowContext(t.Context(), `SELECT count(*) FROM regional_async_video_key_leases`).Scan(&tasks))
	require.Equal(t, video.MaxEntries+1, tasks)
}
