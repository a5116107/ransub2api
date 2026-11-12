//go:build integration && codexcli

package repository_test

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/server/routes"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// Opt in with -tags 'unit,integration,codexcli' and SUB2API_TEST_CODEX_BIN set
// to an installed native CLI. No CLI is downloaded and no provider is contacted.
type priorityCLIProcessInput struct {
	DSN, RedisAddr, Prefix string
	GroupID                int64
	KeyID                  int64
}

type priorityCLIProcess struct {
	URL     string
	PID     int
	RedisID int64
	stop    func()
}

func startPriorityCLIProcess(t *testing.T, f *repository.PrioritySessionFixture) *priorityCLIProcess {
	t.Helper()
	binary, err := os.Executable()
	require.NoError(t, err)
	// testing cancels t.Context before Cleanup; graceful child shutdown must
	// still run then, before this explicit process owner cancels the command.
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, binary, "-test.run=^TestPrioritySchedulingCLIProcessNode$")
	cmd.Env = append(os.Environ(), "SUB2API_PRIORITY_PROCESS=1")
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Start())
	ready := make(chan priorityCLIProcess, 1)
	readDone := make(chan struct{})
	var childOutput strings.Builder
	go func() {
		defer close(readDone)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			_, _ = childOutput.WriteString(scanner.Text() + "\n")
			if data, found := strings.CutPrefix(scanner.Text(), "PRIORITY_CLI_NODE="); found {
				var node priorityCLIProcess
				if json.Unmarshal([]byte(data), &node) == nil {
					ready <- node
				}
			}
		}
	}()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_, _ = io.WriteString(stdin, "\"stop\"\n")
			_ = stdin.Close()
			select {
			case err := <-done:
				<-readDone
				require.NotContains(t, childOutput.String(), "panic_recovered", "a completed CLI response must not hide a recording panic")
				if t.Failed() {
					t.Logf("gateway diagnostic output: %s\n%s", childOutput.String(), stderr.String())
				}
				require.NoError(t, err, "gateway child must stop cleanly: %s", stderr.String())
			case <-time.After(15 * time.Second):
				cancel()
				if err := <-done; err != nil {
					t.Logf("terminated overdue gateway child: %v", err)
				}
				<-readDone
				t.Error("gateway child exceeded its graceful shutdown deadline")
			}
			cancel()
		})
	}
	t.Cleanup(stop)
	input := priorityCLIProcessInput{DSN: f.DSN, RedisAddr: f.Clients[0].Options().Addr, Prefix: f.Prefix, GroupID: f.Group.ID, KeyID: f.Key.ID}
	require.NoError(t, json.NewEncoder(stdin).Encode(input))
	select {
	case node := <-ready:
		require.NotEqual(t, os.Getpid(), node.PID)
		node.stop = stop
		return &node
	case <-readDone:
		t.Fatalf("gateway child exited before readiness: %s", childOutput.String())
	case <-time.After(20 * time.Second):
		t.Fatal("gateway child did not become ready")
	}
	return nil
}

// This is a test-only child entrypoint, never an application service or a
// parallel scheduler. The parent sends only its own disposable connections.
func TestPrioritySchedulingCLIProcessNode(t *testing.T) {
	if os.Getenv("SUB2API_PRIORITY_PROCESS") != "1" {
		return
	}
	logger.InitBootstrap()
	decoder := json.NewDecoder(os.Stdin)
	var input priorityCLIProcessInput
	require.NoError(t, decoder.Decode(&input))
	db, err := sql.Open("postgres", input.DSN)
	require.NoError(t, err)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	rdb := repository.NewNamespacedRedisClient(&redis.Options{Addr: input.RedisAddr}, input.Prefix)
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	redisID, err := rdb.ClientID(t.Context()).Result()
	require.NoError(t, err)
	accounts := repository.NewAccountRepository(client, db, nil)
	groups := repository.NewGroupRepository(client, db)
	group, err := groups.GetByID(t.Context(), input.GroupID)
	require.NoError(t, err)
	keys := repository.NewAPIKeyRepository(client, db)
	key, err := keys.GetByID(t.Context(), input.KeyID)
	require.NoError(t, err)
	cfg := priorityCLIConfig()
	// Authentication selects only this fixture's funded SQL user and API Key.
	// Keep standard mode: simple mode intentionally collapses groups to zero.
	settings := service.NewSettingService(repository.NewSettingRepository(client), cfg)
	cache := repository.NewSchedulerCache(rdb)
	slots := repository.NewConcurrencyCache(rdb, 15, 60)
	concurrency := service.NewConcurrencyService(slots)
	gateway := service.ProvideOpenAIGatewayService(service.OpenAIGatewayDependencies{
		Accounts: accounts, Config: cfg, Settings: settings, Cache: repository.NewGatewayCache(rdb), Concurrency: concurrency,
		Scheduler: service.NewSchedulerSnapshotService(cache, nil, accounts, groups, cfg), Upstream: repository.ProvideHTTPUpstream(cfg, nil, rdb, slots),
		Billing: service.NewBillingService(cfg, nil), UsageLogs: repository.NewUsageLogRepository(client, db),
		UsageBilling: repository.NewUsageBillingRepository(client, db), Users: repository.NewUserRepository(client, db), Deferred: &service.DeferredService{},
	})
	t.Cleanup(gateway.StopOpenAICodexTicketHarvester)
	t.Cleanup(gateway.StopAsyncVideoKeyReleaseRecovery)
	t.Cleanup(gateway.CloseOpenAIWSPool)
	t.Cleanup(func() { require.NoError(t, gateway.CloseExcelBPSImages()) })
	billingCache := repository.NewBillingCache(rdb)
	require.NoError(t, billingCache.SetUserBalance(t.Context(), key.UserID, key.User.Balance))
	billing := service.NewBillingCacheService(billingCache, repository.NewUserRepository(client, db), nil, keys, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	h := &handler.Handlers{
		Gateway: &handler.GatewayHandler{},
		OpenAIGateway: handler.NewOpenAIGatewayHandler(gateway, concurrency, billing,
			service.NewAPIKeyService(keys, repository.NewUserRepository(client, db), groups, nil, nil, nil, cfg), nil, nil, nil, nil, cfg),
		AsyncImage: handler.NewAsyncImageHandler(nil, nil),
	}
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	key.Group = group
	routes.RegisterGatewayRoutes(router, h, func(c *gin.Context) {
		if c.GetHeader("Authorization") != "Bearer "+key.Key {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		c.Set(string(middleware.ContextKeyAPIKey), key)
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: key.UserID, Concurrency: 8})
		c.Next()
	}, nil, nil, nil, settings, nil, cfg)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	data, err := json.Marshal(priorityCLIProcess{URL: server.URL, PID: os.Getpid(), RedisID: redisID})
	require.NoError(t, err)
	fmt.Println("PRIORITY_CLI_NODE=" + string(data))
	var stop string
	require.NoError(t, decoder.Decode(&stop))
	require.Equal(t, "stop", stop)
}

type priorityCLIResult struct {
	Thread, Output string
	Err            error
}

type priorityCLISession struct {
	binary, home, workspace, key string
}

func newPriorityCLISession(t *testing.T, binary, key string) priorityCLISession {
	t.Helper()
	home, workspace := t.TempDir(), t.TempDir()
	configuration := `model = "gpt-5.4"
model_provider = "fixture"
[model_providers.fixture]
name = "Local scheduling fixture"
wire_api = "responses"
env_key = "SUB2API_AFFINITY_KEY"
requires_openai_auth = false
request_max_retries = 0
stream_max_retries = 0
`
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(configuration), 0o600))
	return priorityCLISession{binary: binary, home: home, workspace: workspace, key: key}
}

func (s priorityCLISession) invoke(ctx context.Context, node, thread, prompt string) priorityCLIResult {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	args := []string{"exec", "--json", "--skip-git-repo-check", "--color", "never", "-s", "read-only", "-C", s.workspace,
		"--disable", "shell_snapshot", "--disable", "shell_tool", "--disable", "code_mode_host",
		"-c", fmt.Sprintf("model_providers.fixture.base_url=%q", node+"/v1")}
	if thread != "" {
		args = append(args, "resume", thread)
	}
	args = append(args, prompt)
	cmd := exec.CommandContext(ctx, s.binary, args...)
	cmd.WaitDelay = 3 * time.Second
	// Do not inherit live API credentials, proxies, MCP config or user sessions.
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "SystemRoot=" + os.Getenv("SystemRoot"), "WINDIR=" + os.Getenv("WINDIR"),
		"TEMP=" + s.home, "TMP=" + s.home, "TMPDIR=" + s.home, "HOME=" + s.home, "USERPROFILE=" + s.home,
		"CODEX_HOME=" + s.home, "SUB2API_AFFINITY_KEY=" + s.key, "NO_PROXY=127.0.0.1,localhost",
	}
	// No model tool invocation is part of this wire fixture. Disable shell
	// snapshots/tools and their host so helpers do not outlive a CLI turn;
	// WaitDelay also bounds inherited output handles after cancellation.
	output, err := priorityCLIRun(cmd)
	result := priorityCLIResult{Thread: thread, Output: string(output), Err: err}
	for _, line := range bytes.Split(output, []byte{'\n'}) {
		var event struct {
			Type   string
			Thread string `json:"thread_id"`
		}
		if json.Unmarshal(line, &event) == nil && event.Type == "thread.started" {
			result.Thread = event.Thread
		}
	}
	return result
}

func TestPrioritySessionAffinityCLIProcesses(t *testing.T) {
	requested := os.Getenv("SUB2API_TEST_CODEX_BIN")
	require.NotEmpty(t, requested, "codexcli tag requires an installed native SUB2API_TEST_CODEX_BIN")
	require.True(t, filepath.IsAbs(requested), "the opt-in CLI must have an explicit absolute path")
	binary, err := filepath.EvalSymlinks(requested)
	require.NoError(t, err)
	require.True(t, filepath.IsAbs(binary))
	info, err := os.Stat(binary)
	require.NoError(t, err)
	require.True(t, info.Mode().IsRegular(), "the installed CLI must be a regular executable file")
	// Only a native Codex entrypoint is accepted, never a shell wrapper. Resolve
	// the explicit installed path rather than relying on ambient PATH lookup.
	var versionCommand *exec.Cmd
	switch filepath.Base(binary) {
	case "codex":
		versionCommand = exec.CommandContext(t.Context(), "codex", "--version")
	case "codex.exe":
		versionCommand = exec.CommandContext(t.Context(), "codex.exe", "--version")
	default:
		t.Fatal("SUB2API_TEST_CODEX_BIN must name the installed native codex executable")
	}
	versionCommand.Path, versionCommand.Err = binary, nil
	version, err := priorityCLIRun(versionCommand)
	require.NoError(t, err)
	t.Logf("CLI identity: %s", strings.TrimSpace(string(version)))
	type observation struct {
		account  int
		cacheKey string
		input    string
	}
	observed := make(chan observation, 128)
	var sends atomic.Int32
	var hold atomic.Bool
	held, release := make(chan struct{}, 1), make(chan struct{})
	var releaseOnce sync.Once
	unhold := func() { releaseOnce.Do(func() { close(release) }) }
	defer unhold()
	var upstreams []string
	for account := range 2 {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var request struct {
				Input          json.RawMessage `json:"input"`
				PromptCacheKey string          `json:"prompt_cache_key"`
			}
			if json.NewDecoder(r.Body).Decode(&request) != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			sequence := sends.Add(1)
			observed <- observation{account: account, cacheKey: request.PromptCacheKey, input: string(request.Input)}
			if hold.CompareAndSwap(true, false) {
				held <- struct{}{}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			writePriorityCLIResponse(w, sequence)
		}))
		t.Cleanup(upstream.Close)
		upstreams = append(upstreams, upstream.URL)
	}
	f := repository.NewPrioritySessionFixture(t, upstreams)
	policy := service.DefaultPrioritySchedulingConfig()
	policy.Enabled, policy.PreserveSessionAffinity = true, true
	policy.GroupIDs, policy.Models = []int64{f.Group.ID}, []string{"gpt-5.4"}
	require.NoError(t, service.NewSettingService(f.Settings, nil).SavePrioritySchedulingConfig(t.Context(), policy))
	cache := repository.NewSchedulerCache(f.Clients[0])
	bucket := service.SchedulerBucket{GroupID: f.Group.ID, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	token, err := cache.CaptureBucketWriteToken(t.Context(), bucket)
	require.NoError(t, err)
	require.NoError(t, cache.SetSnapshot(t.Context(), bucket, token, f.Pool))
	nodes := [2]*priorityCLIProcess{startPriorityCLIProcess(t, f), startPriorityCLIProcess(t, f)}
	require.NotEqual(t, nodes[0].PID, nodes[1].PID)
	require.NotEqual(t, nodes[0].RedisID, nodes[1].RedisID)
	t.Cleanup(unhold)
	sessions := [2]priorityCLISession{newPriorityCLISession(t, binary, f.Key.Key), newPriorityCLISession(t, binary, f.Key.Key)}
	var threads [2]string
	var origins [2]observation
	turn := func(session, node, account int, prompt string) {
		t.Helper()
		result := sessions[session].invoke(t.Context(), nodes[node].URL, threads[session], prompt)
		require.NoError(t, result.Err, "actual CLI failed: %s", result.Output)
		require.Contains(t, result.Output, `"type":"turn.completed"`)
		if threads[session] == "" {
			threads[session] = result.Thread
		}
		require.Equal(t, threads[session], result.Thread)
		select {
		case got := <-observed:
			require.Equal(t, account, got.account)
			require.Contains(t, got.input, fmt.Sprintf("origin-%d", session), "a resumed CLI must preserve its first user turn")
			require.Contains(t, got.input, prompt)
			require.NotEmpty(t, got.cacheKey)
			if origins[session].cacheKey == "" {
				origins[session] = got
			} else {
				require.Equal(t, origins[session].cacheKey, got.cacheKey)
				require.Greater(t, len(got.input), len(origins[session].input))
			}
		case <-time.After(time.Second):
			t.Fatal("CLI success had no physical upstream request")
		}
	}
	turn(0, 0, 0, "origin-0")
	require.NoError(t, f.Accounts.SetSchedulable(t.Context(), f.Pool[0].ID, false))
	turn(1, 1, 1, "origin-1")
	require.NoError(t, f.Accounts.SetSchedulable(t.Context(), f.Pool[0].ID, true))
	require.NotEqual(t, threads[0], threads[1])
	require.NotEqual(t, origins[0].cacheKey, origins[1].cacheKey)
	for round := range 12 {
		if round == 6 {
			oldPID := nodes[0].PID
			nodes[0].stop()
			nodes[0] = startPriorityCLIProcess(t, f)
			require.NotEqual(t, oldPID, nodes[0].PID)
		}
		for session := range sessions {
			turn(session, (round+session)%2, session, fmt.Sprintf("continue-%d-%d", session, round))
		}
		if round == 5 || round == 11 {
			t.Logf("both CLI sessions completed continuation round %d", round+1)
		}
	}
	// A real held request on A owns the shared Key. B must send nothing, then
	// resume the same CLI session after release rather than creating a new one.
	hold.Store(true)
	alphaDone := make(chan priorityCLIResult, 1)
	go func() { alphaDone <- sessions[0].invoke(t.Context(), nodes[0].URL, threads[0], "hold-0") }()
	select {
	case <-held:
	case <-time.After(45 * time.Second):
		t.Fatal("held CLI request never reached the provider")
	}
	before := sends.Load()
	blocked := sessions[1].invoke(t.Context(), nodes[1].URL, threads[1], "blocked-1")
	require.Error(t, blocked.Err, "full shared Key must not be reported as CLI success: %s", blocked.Output)
	// The first account fails shared-Key admission (503); its fallback is the
	// other session's occupied account (429). Neither may physically send.
	require.Contains(t, blocked.Output, "429", "the exhausted local fallback must remain an explicit admission failure")
	require.Equal(t, threads[1], blocked.Thread)
	require.Equal(t, before, sends.Load(), "the other gateway must reject before any physical send")
	unhold()
	alpha := <-alphaDone
	require.NoError(t, alpha.Err, "%s", alpha.Output)
	require.Contains(t, alpha.Output, `"type":"turn.completed"`)
	got := <-observed
	require.Equal(t, 0, got.account)
	require.Contains(t, got.input, "hold-0")
	turn(1, 1, 1, "recovered-1")
	require.NoError(t, f.Accounts.SetSchedulable(t.Context(), f.Pool[0].ID, false))
	turn(0, 1, 1, "manual-stop-0")
	require.NoError(t, f.Accounts.SetSchedulable(t.Context(), f.Pool[0].ID, true))
	turn(0, 0, 1, "healthy-migration-retained-0")
	require.EqualValues(t, 30, sends.Load())
	require.Empty(t, observed, "no hidden duplicate or automatic replay is expected")
	db, err := sql.Open("postgres", f.DSN)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	var count int
	var charge, balance float64
	require.Eventually(t, func() bool {
		err := db.QueryRowContext(t.Context(), `SELECT COUNT(*),COALESCE(SUM(actual_cost),0) FROM usage_logs WHERE api_key_id=$1`, f.Key.ID).Scan(&count, &charge)
		return err == nil && count == 30
	}, 5*time.Second, 10*time.Millisecond, "each physical completion must persist exactly once; refusal is not a zero-cost success")
	require.Positive(t, charge)
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT balance FROM users WHERE id=$1`, f.Key.UserID).Scan(&balance))
	require.InDelta(t, 100-charge, balance, 1e-8)
	for _, account := range f.Pool {
		count, err := repository.NewConcurrencyCache(f.Clients[0], 15, 60).GetAccountConcurrency(t.Context(), account.ID)
		require.NoError(t, err)
		require.Zero(t, count)
	}
	transport := repository.ProvideHTTPUpstream(priorityCLIConfig(), nil, f.Clients[0], nil)
	keyAuthority, ok := transport.(service.UpstreamKeyConcurrency)
	require.True(t, ok)
	_, releaseKey, err := keyAuthority.AcquireUpstreamKeySlots(t.Context(), []string{config.UpstreamKeyFingerprint("supplier-fixture")})
	require.NoError(t, err, "completed CLI requests must not leak a shared-Key member")
	require.NotNil(t, releaseKey)
	releaseKey()
	t.Log("2 real CLI sessions, 30 physical completions, 3 gateway PIDs: cross-node resume, restart, shared-Key refusal/recovery, hard stop and retained migration")
}

func priorityCLIConfig() *config.Config {
	cfg := &config.Config{RunMode: config.RunModeStandard}
	cfg.Gateway.UpstreamKeyConcurrency = config.UpstreamKeyConcurrencyConfig{Revision: 1, Limits: map[string]int{config.UpstreamKeyFingerprint("supplier-fixture"): 1}}
	cfg.Gateway.MaxBodySize, cfg.Gateway.TextMaxBodySize = 4<<20, 4<<20
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.AllowPrivateHosts = true
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	return cfg
}

func writePriorityCLIResponse(w http.ResponseWriter, sequence int32) {
	w.Header().Set("Content-Type", "text/event-stream")
	item := map[string]any{"id": fmt.Sprintf("msg_cli_%d", sequence), "type": "message", "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": "ACK", "annotations": []any{}}}}
	response := map[string]any{"id": fmt.Sprintf("resp_cli_%d", sequence), "object": "response", "model": "gpt-5.4", "status": "completed",
		"output": []any{item}, "usage": map[string]any{"input_tokens": 100, "output_tokens": 1, "total_tokens": 101}}
	for _, event := range []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": response["id"], "status": "in_progress", "output": []any{}}},
		{"type": "response.output_item.added", "output_index": 0, "item": item},
		{"type": "response.output_text.delta", "item_id": item["id"], "output_index": 0, "content_index": 0, "delta": "ACK"},
		{"type": "response.output_item.done", "output_index": 0, "item": item},
		{"type": "response.completed", "response": response},
	} {
		data, err := json.Marshal(event)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
	}
}
