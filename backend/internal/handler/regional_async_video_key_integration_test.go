//go:build unit && integration

package handler

import (
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/alicebob/miniredis/v2"
	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestRegionalAsyncVideoGrokHandlerLifecycle(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("Docker and the local PostgreSQL image are required")
	}
	container, err := exec.Command(docker, "run", "-d", "--rm", "--pull=never", "-p", "127.0.0.1::5432",
		"-e", "POSTGRES_PASSWORD=fixture", "postgres:18.1-alpine3.23").Output()
	if err != nil {
		t.Skipf("isolated PostgreSQL container did not start: %v", err)
	}
	containerID := strings.TrimSpace(string(container))
	t.Cleanup(func() { _ = exec.Command(docker, "rm", "-f", containerID).Run() })
	port, err := exec.Command(docker, "port", containerID, "5432/tcp").Output()
	require.NoError(t, err)
	db, err := sql.Open("postgres", fmt.Sprintf("postgres://postgres:fixture@%s/postgres?sslmode=disable", strings.TrimSpace(string(port))))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.Eventually(t, func() bool { return db.PingContext(t.Context()) == nil }, 15*time.Second, 100*time.Millisecond)
	for _, name := range []string{"280_regional_slot_leases.sql", "281_regional_admission.sql", "327_regional_upstream_key_slots.sql", "331_regional_async_video_key_lease.sql"} {
		raw, err := migrations.FS.ReadFile(name)
		require.NoError(t, err)
		_, err = db.ExecContext(t.Context(), string(raw))
		require.NoError(t, err)
	}
	redisServer := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })

	var creates, polls, competitors atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/competing" {
			competitors.Add(1)
			_, _ = io.WriteString(w, `{"ok":true}`)
		} else if r.Method == http.MethodPost {
			creates.Add(1)
			_, _ = io.WriteString(w, `{"request_id":"task","status":"pending"}`)
		} else {
			polls.Add(1)
			_, _ = io.WriteString(w, `{"status":"done","video":{"url":"https://vidgen.x.ai/result.mp4","duration":6}}`)
		}
	}))
	t.Cleanup(provider.Close)

	h, slots, bindings, _ := newGrokMediaSlotHandler(t, false, false)
	h.cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	h.cfg.Gateway.UpstreamKeyConcurrency = config.UpstreamKeyConcurrencyConfig{Revision: 1,
		Limits: map[string]int{config.UpstreamKeyFingerprint("regional-fixture"): 1}}
	primary := repository.NewRegionalSlotAuthority(db)
	require.NoError(t, primary.PublishUpstreamPolicy(t.Context(), h.cfg.Gateway.UpstreamKeyConcurrency))
	regional, err := repository.NewRegionalConcurrencyCache(repository.NewConcurrencyCache(rdb, 15, 30), primary, nil)
	require.NoError(t, err)
	transport := repository.ProvideHTTPUpstream(h.cfg, nil, rdb, regional)
	account := service.Account{ID: 1, Platform: service.PlatformGrok, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, Concurrency: 5, GroupIDs: []int64{24},
		Credentials: map[string]any{"api_key": "regional-fixture", "base_url": provider.URL}}
	repo := grokMediaSlotRepo{openAIImagesFailoverAccountRepo: openAIImagesFailoverAccountRepo{accounts: []service.Account{account}}}
	concurrency := service.NewConcurrencyService(slots)
	h.gatewayService = service.NewOpenAIGatewayService(repo, nil, nil, nil, nil, nil, nil, bindings, h.cfg, nil, concurrency, nil, nil, nil, transport, nil, nil, nil, nil, nil, nil, nil, nil)
	t.Cleanup(h.gatewayService.StopOpenAICodexTicketHarvester)

	create, created := grokMediaSlotContext(t.Context(), true)
	h.GrokVideoGeneration(create)
	require.Equal(t, http.StatusOK, created.Code, created.Body.String())
	require.EqualValues(t, 1, creates.Load())
	tryCompeting := func() error {
		req, requestErr := http.NewRequestWithContext(t.Context(), http.MethodPost, provider.URL+"/competing", strings.NewReader("{}"))
		require.NoError(t, requestErr)
		req.Header.Set("Authorization", "Bearer regional-fixture")
		response, sendErr := transport.Do(req, "", 2, 1)
		if response != nil {
			require.NoError(t, response.Body.Close())
		}
		return sendErr
	}
	require.ErrorIs(t, tryCompeting(), service.ErrUpstreamKeyCapacity)
	bindings.owner, bindings.key = 0, ""
	wrong, denied := grokMediaSlotContext(t.Context(), false)
	groupID := int64(24)
	wrong.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 21, UserID: 11, GroupID: &groupID,
		Group: &service.Group{ID: groupID, Platform: service.PlatformGrok, AllowImageGeneration: true}, User: &service.User{ID: 11}})
	wrong.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 11, Concurrency: 5})
	h.GrokVideoStatus(wrong)
	require.Equal(t, http.StatusNotFound, denied.Code, denied.Body.String())
	require.Zero(t, polls.Load())
	status, completed := grokMediaSlotContext(t.Context(), false)
	h.GrokVideoStatus(status)
	require.Equal(t, http.StatusOK, completed.Code, completed.Body.String())
	require.EqualValues(t, 1, polls.Load())
	require.NoError(t, tryCompeting())
	require.EqualValues(t, 1, competitors.Load())
	var records int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM regional_async_video_key_leases`).Scan(&records))
	require.Zero(t, records)
	slots.assertReleased(t)
}
