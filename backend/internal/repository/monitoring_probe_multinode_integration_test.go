//go:build integration

package repository

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	redisclient "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// Connection details travel only over the child stdin, never arguments, logs,
// or committed configuration. Each process owns its DB pool and HTTP client.
type monitoringBudgetNodeCommand struct {
	Mode          string
	DSN           string
	RedisAddress  string
	RedisDB       int
	Site          string
	Certificate   []byte
	Path          string
	Method        string
	Header        http.Header
	Requests      int
	TimeoutMillis int
	Recovery      bool
	Tick          time.Time
	ObserveQueue  bool
}

type monitoringBudgetNodeReply struct {
	Status int    `json:"status"`
	Reason string `json:"reason,omitempty"`
	Scope  string `json:"scope,omitempty"`
	State  string `json:"state,omitempty"`
}

const monitoringBudgetNodeFrame = "monitoring-node-result:"

func writeMonitoringBudgetNodeReply(w io.Writer, reply monitoringBudgetNodeReply) error {
	raw, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s%s\n", monitoringBudgetNodeFrame, raw)
	return err
}

func TestMonitoringProbeBudgetProcessNode(t *testing.T) {
	if os.Getenv("SUB2API_MONITORING_BUDGET_PROCESS") != "1" {
		return
	}
	var command monitoringBudgetNodeCommand
	require.NoError(t, json.NewDecoder(io.LimitReader(os.Stdin, 64<<10)).Decode(&command))
	require.True(t, command.Requests > 0 && command.Requests <= 100)
	db, err := sql.Open("postgres", command.DSN)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(8)
	budget := service.NewMonitoringProbeBudgetService(NewMonitoringProbeBudgetRepository(db))
	if command.Mode == "billing" {
		runMonitoringBillingNode(t, command, db, budget)
		return
	}
	if command.Mode == "v3" || command.Mode == "group" || command.Mode == "scheduled" || command.Mode == "v2" {
		runMonitoringGroupProbeNode(t, command, db, budget)
		return
	}
	if command.Mode == "ollama" || command.Mode == "ollama_lease" {
		runMonitoringOllamaNode(t, command, db)
		return
	}
	if command.Mode == "opencode" || command.Mode == "opencode_lease" {
		runMonitoringOpenCodeNode(t, command, db)
		return
	}
	if command.Mode == "cn" {
		runMonitoringCNNode(t, command, db, budget)
		return
	}
	if command.Mode == "v1" {
		runMonitoringV1Node(t, command, db)
		return
	}
	require.Empty(t, command.Mode)
	client := &http.Client{Timeout: 15 * time.Second}
	defer client.CloseIdleConnections()
	var outputMu sync.Mutex
	var outputErr error
	var wg sync.WaitGroup
	for range command.Requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := t.Context()
			if command.TimeoutMillis > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Duration(command.TimeoutMillis)*time.Millisecond)
				defer cancel()
			}
			ctx = budget.Context(ctx, command.Site, command.Recovery)
			reply := monitoringBudgetNodeReply{}
			req, requestErr := http.NewRequestWithContext(ctx, command.Method, command.Site+command.Path, nil)
			if requestErr == nil {
				req.Header = command.Header.Clone()
				var response *http.Response
				response, requestErr = service.MonitoringProbeHTTPClient(ctx, client).Do(req)
				if response != nil {
					reply.Status = response.StatusCode
					_, readErr := io.Copy(io.Discard, response.Body)
					requestErr = errors.Join(requestErr, readErr, response.Body.Close())
				}
			}
			if requestErr != nil {
				var denial *service.MonitoringProbeBudgetDenial
				switch {
				case errors.As(requestErr, &denial):
					reply.Reason, reply.Scope = denial.Reason, denial.Scope
				case service.IsMonitoringProbeBudgetFailure(requestErr):
					reply.Reason = "budget_unavailable"
				default:
					reply.Reason = "transport_error"
				}
			}
			outputMu.Lock()
			if err := writeMonitoringBudgetNodeReply(os.Stdout, reply); err != nil && outputErr == nil {
				outputErr = err
			}
			outputMu.Unlock()
		}()
	}
	wg.Wait()
	require.NoError(t, outputErr)
}

func runMonitoringBillingNode(t *testing.T, command monitoringBudgetNodeCommand, db *sql.DB, budget *service.MonitoringProbeBudgetService) {
	t.Helper()
	require.Equal(t, 1, command.Requests)
	require.NotEmpty(t, command.RedisAddress)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	defer func() { _ = client.Close() }()
	rdb := redisclient.NewClient(&redisclient.Options{Addr: command.RedisAddress, DB: command.RedisDB})
	defer func() { _ = rdb.Close() }()
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true}}}
	upstream := NewHTTPUpstream(cfg).(*httpUpstreamService)
	defer func() {
		upstream.mu.Lock()
		defer upstream.mu.Unlock()
		for _, entry := range upstream.clients {
			entry.client.CloseIdleConnections()
		}
	}()
	accounts := newAccountRepositoryWithSQL(client, db, nil)
	tester := service.NewAccountTestService(accounts, nil, nil, nil, nil, upstream, cfg, nil)
	runner := service.NewUpstreamBillingProbeService(accounts, tester, service.NewSettingService(NewSettingRepository(client), cfg))
	runner.SetMonitoringBudget(budget)
	runner.SetLeaderLock(NewLeaderLockCache(rdb), db)
	defer runner.Stop()
	require.NoError(t, runner.RunDue(t.Context()))
	require.NoError(t, writeMonitoringBudgetNodeReply(os.Stdout, monitoringBudgetNodeReply{Status: http.StatusOK}))
}

func runMonitoringCNNode(t *testing.T, command monitoringBudgetNodeCommand, db *sql.DB, budget *service.MonitoringProbeBudgetService) {
	t.Helper()
	require.Equal(t, 1, command.Requests)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	defer func() { _ = client.Close() }()
	rdb := redisclient.NewClient(&redisclient.Options{Addr: command.RedisAddress, DB: command.RedisDB})
	defer func() { _ = rdb.Close() }()
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true}}}
	cfg.Gateway.CNProviders.BalanceCheckEnabled = true
	upstream := NewHTTPUpstream(cfg)
	native := pr612NativeTransport(t, upstream)
	defer func() {
		native.mu.Lock()
		defer native.mu.Unlock()
		for _, entry := range native.clients {
			entry.client.CloseIdleConnections()
		}
	}()
	accounts := NewAccountRepository(client, db, nil)
	runner := service.NewCNProviderBalanceCheckService(accounts, service.NewCNProviderBalanceService(accounts, nil, upstream, cfg), nil, cfg, time.Second)
	runner.SetMonitoringBudget(budget)
	runner.SetLeaderLock(NewLeaderLockCache(rdb))
	defer runner.Stop()
	runner.Start()
	select {
	case <-time.After(3200 * time.Millisecond):
	case <-t.Context().Done():
		t.Fatal("CN process context expired")
	}
	runner.Stop()
	require.NoError(t, writeMonitoringBudgetNodeReply(os.Stdout, monitoringBudgetNodeReply{Status: http.StatusOK}))
}

func runMonitoringV1Node(t *testing.T, command monitoringBudgetNodeCommand, db *sql.DB) {
	t.Helper()
	require.True(t, command.TimeoutMillis > 0 && command.TimeoutMillis <= 60000)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	defer func() { _ = client.Close() }()
	cfg := &config.Config{}
	settings := service.NewSettingService(NewSettingRepository(client), cfg)
	cert, err := x509.ParseCertificate(command.Certificate)
	require.NoError(t, err)
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "example.com", MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(command.Site, "https://"))
		}}
	defer transport.CloseIdleConnections()
	// V1 meters generation and HEAD itself; its pinned fixture must not pass
	// through HTTPUpstream's second instrumentation or invent HEAD credentials.
	t.Cleanup(service.SetChannelMonitorHTTPClientForTesting(&http.Client{Transport: transport, Timeout: 15 * time.Second}))
	monitors := service.ProvideChannelMonitorService(NewChannelMonitorRepository(client, db), ollamaBudgetFixtureEncryptor{}, settings, cfg, nil, NewMonitoringProbeBudgetRepository(db))
	runner := service.NewChannelMonitorRunner(monitors, settings)
	monitors.SetScheduler(runner)
	defer runner.Stop()
	runner.Start()
	select {
	case <-time.After(time.Duration(command.TimeoutMillis) * time.Millisecond):
	case <-t.Context().Done():
	}
	runner.Stop()
	require.NoError(t, writeMonitoringBudgetNodeReply(os.Stdout, monitoringBudgetNodeReply{Status: http.StatusOK}))
}

func runMonitoringOpenCodeNode(t *testing.T, command monitoringBudgetNodeCommand, db *sql.DB) {
	t.Helper()
	require.Equal(t, 1, command.Requests)
	endpoint, err := url.Parse(command.Site)
	require.NoError(t, err)
	require.Equal(t, "https", endpoint.Scheme)
	require.NotEmpty(t, endpoint.Host)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	defer func() { _ = client.Close() }()
	rdb := redisclient.NewClient(&redisclient.Options{Addr: command.RedisAddress, DB: command.RedisDB})
	defer func() { _ = rdb.Close() }()
	cfg := &config.Config{Runtime: config.RuntimeConfig{Role: config.RuntimeRoleGateway}}
	accounts := service.AccountRepository(newAccountRepositoryWithSQL(client, db, nil))
	if command.Mode == "opencode_lease" {
		accounts = &monitoringOpenCodeLeaseAccounts{accountRepository: newAccountRepositoryWithSQL(client, db, nil)}
	}
	runner := service.ProvideOpenCodeGoUsageService(accounts, monitoringBudgetPinnedUpstream(t, endpoint.Host, command.Certificate),
		service.NewSettingService(NewSettingRepository(client), cfg), NewLeaderLockCache(rdb), db, cfg, NewMonitoringProbeBudgetRepository(db))
	defer runner.Stop()
	runErr := runner.RunDue(t.Context())
	status := http.StatusOK
	if command.Mode == "opencode_lease" {
		require.ErrorContains(t, runErr, "leader lock lost")
		status = http.StatusServiceUnavailable
	} else {
		require.NoError(t, runErr)
	}
	require.NoError(t, writeMonitoringBudgetNodeReply(os.Stdout, monitoringBudgetNodeReply{Status: status}))
}

type monitoringOpenCodeLeaseAccounts struct{ *accountRepository }

func (r *monitoringOpenCodeLeaseAccounts) ListOpenCodeGoUsageGroupAccounts(ctx context.Context, anchors []*service.Account) ([]service.Account, error) {
	select {
	case <-time.After(19 * time.Second):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return r.accountRepository.ListOpenCodeGoUsageGroupAccounts(ctx, anchors)
}

func runMonitoringOllamaNode(t *testing.T, command monitoringBudgetNodeCommand, db *sql.DB) {
	t.Helper()
	require.Equal(t, 1, command.Requests)
	require.NotEmpty(t, command.RedisAddress)
	endpoint, err := url.Parse(command.Site)
	require.NoError(t, err)
	require.Equal(t, "https", endpoint.Scheme)
	require.NotEmpty(t, endpoint.Host)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	defer func() { _ = client.Close() }()
	rdb := redisclient.NewClient(&redisclient.Options{Addr: command.RedisAddress, DB: command.RedisDB})
	defer func() { _ = rdb.Close() }()
	cfg := &config.Config{Runtime: config.RuntimeConfig{Role: config.RuntimeRoleGateway}}
	cfg.Totp.EncryptionKeyConfigured = true
	upstream := monitoringBudgetPinnedUpstream(t, endpoint.Host, command.Certificate)
	accounts := service.AccountRepository(newAccountRepositoryWithSQL(client, db, nil))
	if command.Mode == "ollama_lease" {
		accounts = &monitoringOllamaLeaseAccounts{accountRepository: newAccountRepositoryWithSQL(client, db, nil)}
	}
	runner := service.ProvideOllamaCloudUsageService(accounts, upstream,
		service.NewSettingService(NewSettingRepository(client), cfg), ollamaBudgetFixtureEncryptor{}, cfg,
		NewLeaderLockCache(rdb), db, NewMonitoringProbeBudgetRepository(db))
	defer runner.Stop()
	runErr := runner.RunDue(t.Context())
	scan := runner.MonitoringRuntime().LastScan
	require.NotNil(t, scan)
	status := http.StatusOK
	if command.Mode == "ollama_lease" {
		require.Error(t, runErr)
		require.Equal(t, "coordination_unavailable", scan.State)
		require.Nil(t, scan.Candidates)
		status = http.StatusServiceUnavailable
	} else {
		require.NoError(t, runErr)
	}
	require.NoError(t, writeMonitoringBudgetNodeReply(os.Stdout, monitoringBudgetNodeReply{Status: status, Reason: scan.State}))
}

// Storage latency keeps the native four-request wave in flight at the real
// first renewal. All actual reads/writes still use the original SQL repository.
type monitoringOllamaLeaseAccounts struct{ *accountRepository }

func (r *monitoringOllamaLeaseAccounts) ListOllamaCloudUsageGroupAccounts(ctx context.Context, anchors []*service.Account) ([]service.Account, error) {
	select {
	case <-time.After(19 * time.Second):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return r.accountRepository.ListOllamaCloudUsageGroupAccounts(ctx, anchors)
}

type monitoringScheduledNodePlans struct {
	service.ScheduledTestPlanRepository
	service.QualitySharedBatchRepository
	finalizations int
	done          chan struct{}
	once          sync.Once
}

func (r *monitoringScheduledNodePlans) FinalizeQualityBatches(ctx context.Context) error {
	if err := r.QualitySharedBatchRepository.FinalizeQualityBatches(ctx); err != nil {
		return err
	}
	r.finalizations++
	if r.finalizations == 2 {
		r.once.Do(func() { close(r.done) })
	}
	return nil
}

func (r *monitoringScheduledNodePlans) ListDue(ctx context.Context, now time.Time) ([]*service.ScheduledTestPlan, error) {
	plans, err := r.ScheduledTestPlanRepository.ListDue(ctx, now)
	if err == nil && len(plans) == 0 {
		r.once.Do(func() { close(r.done) })
	}
	return plans, err
}

func runMonitoringGroupProbeNode(t *testing.T, command monitoringBudgetNodeCommand, db *sql.DB, budget *service.MonitoringProbeBudgetService) {
	t.Helper()
	require.Equal(t, 1, command.Requests)
	require.NotEmpty(t, command.RedisAddress)
	require.False(t, command.Tick.IsZero())
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	defer func() { _ = client.Close() }()
	rdb := redisclient.NewClient(&redisclient.Options{Addr: command.RedisAddress, DB: command.RedisDB})
	defer func() { _ = rdb.Close() }()
	cfg := &config.Config{Timezone: "UTC", Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true}}}
	cfg.Default.RateMultiplier = 1
	settings := service.NewSettingService(NewSettingRepository(client), cfg)
	upstream := NewHTTPUpstream(cfg)
	native := pr612NativeTransport(t, upstream)
	defer func() {
		native.mu.Lock()
		defer native.mu.Unlock()
		for _, entry := range native.clients {
			entry.client.CloseIdleConnections()
		}
	}()
	accounts := NewAccountRepository(client, db, nil)
	concurrency := service.NewConcurrencyService(NewConcurrencyCache(rdb, 1, 60))
	gateway := service.NewOpenAIGatewayService(accounts, nil, nil, nil, nil, nil, nil, NewGatewayCache(rdb), cfg, nil, concurrency,
		service.NewBillingService(cfg, nil), nil, nil, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
	defer gateway.StopOpenAICodexTicketHarvester()
	defer gateway.CloseOpenAIWSPool()
	tester := service.NewAccountTestService(accounts, nil, nil, nil, nil, upstream, cfg, nil)
	tester.SetMonitoringBudget(budget)
	groups := service.NewPelicanGroupTestService(NewPelicanGroupTestRepository(db), NewGroupRepository(client, db), nil, gateway, concurrency, tester, nil, nil)
	var queueState string
	if command.Mode == "group" {
		groups.RunDue(t.Context(), command.Tick)
	} else if command.Mode == "scheduled" || command.Mode == "v2" {
		original := NewScheduledTestPlanRepository(db)
		batches, ok := original.(service.QualitySharedBatchRepository)
		require.True(t, ok)
		plans := &monitoringScheduledNodePlans{ScheduledTestPlanRepository: original, QualitySharedBatchRepository: batches, done: make(chan struct{})}
		monitor := service.ProvideChannelMonitorV2Service(NewChannelMonitorV2Repository(db), settings, groups)
		budget.SetGroupProbeQueues(monitor, nil)
		runtime := &config.Config{Timezone: cfg.Timezone, Runtime: config.RuntimeConfig{Role: config.RuntimeRoleGateway}}
		runner := service.ProvideScheduledTestRunnerService(plans,
			service.NewScheduledTestService(NewScheduledTestPlanRepository(db), NewScheduledTestResultRepository(db)), tester, nil,
			runtime, &service.QualityJudgeService{}, nil, groups, monitor)
		defer runner.Stop()
		runner.Start()
		select {
		case <-plans.done:
		case <-time.After(80 * time.Second):
			t.Fatal("original scheduled cron did not finish its first real batch")
		}
		if command.Mode == "v2" {
			// An empty account inventory finishes before the parallel candy batch.
			// Read the existing runtime projection; do not stop its parent early.
			require.Eventually(t, func() bool {
				summary, err := budget.Summary(t.Context())
				return err == nil && summary != nil && summary.Coverage != nil && summary.Coverage.GroupProbeQueues != nil &&
					summary.Coverage.GroupProbeQueues.V2 != nil && (summary.Coverage.GroupProbeQueues.V2.State == "coordination_unavailable" ||
					summary.Coverage.GroupProbeQueues.V2.Cycle != nil && summary.Coverage.GroupProbeQueues.V2.Cycle.FinishedAt != nil)
			}, 20*time.Second, 20*time.Millisecond, "original candy batch must settle before its cron parent stops")
			if command.ObserveQueue {
				summary, err := budget.Summary(t.Context())
				require.NoError(t, err)
				queueState = summary.Coverage.GroupProbeQueues.V2.State
			}
		}
		runner.Stop()
	} else {
		prober := service.NewChannelMonitorV3Prober(NewChannelMonitorV3Repository(db), groups, settings)
		defer prober.Stop()
		prober.RunDue(t.Context(), command.Tick)
		if command.ObserveQueue {
			queueState = prober.MonitoringRuntime().State
		}
	}
	require.NoError(t, writeMonitoringBudgetNodeReply(os.Stdout, monitoringBudgetNodeReply{Status: http.StatusOK, State: queueState}))
}

type monitoringBudgetNode struct {
	pid  int
	wait func()
}

func startMonitoringBudgetNode(t *testing.T, command monitoringBudgetNodeCommand, replies chan<- monitoringBudgetNodeReply) monitoringBudgetNode {
	t.Helper()
	input, err := json.Marshal(command)
	require.NoError(t, err)
	executable, err := os.Executable()
	require.NoError(t, err)
	lifetime := 30 * time.Second
	if command.Mode == "ollama_lease" || command.Mode == "opencode_lease" {
		lifetime = 70 * time.Second // Original two-minute lease renews at forty seconds.
	}
	if command.Mode == "scheduled" || command.Mode == "v2" {
		lifetime = 95 * time.Second // A real minute boundary plus the original ten-second delay.
	}
	if command.Mode == "v1" {
		lifetime = time.Duration(command.TimeoutMillis)*time.Millisecond + 20*time.Second
	}
	ctx, cancel := context.WithTimeout(t.Context(), lifetime)
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestMonitoringProbeBudgetProcessNode$")
	cmd.Env = append(os.Environ(), "SUB2API_MONITORING_BUDGET_PROCESS=1")
	cmd.Stdin = bytes.NewReader(input)
	// Do not surface connection or upstream details from a failing child.
	cmd.Stderr = io.Discard
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	done := make(chan struct{})
	var runErr, decodeErr error
	var diagnostics []string
	go func() {
		defer close(done)
		reader := bufio.NewScanner(stdout)
		// Keep only source locations and known failure labels, never raw child
		// logs, connection strings, credentials or upstream response bodies.
		location := regexp.MustCompile(`[a-zA-Z0-9_]+_test\.go:\d+`)
		reader.Buffer(make([]byte, 4096), 1<<20)
		received := 0
		for reader.Scan() {
			line := reader.Text()
			if !strings.HasPrefix(line, monitoringBudgetNodeFrame) {
				if found := location.FindString(line); found != "" && len(diagnostics) < 12 {
					diagnostics = append(diagnostics, found)
				}
				if strings.Contains(line, "WARNING: DATA RACE") {
					diagnostics = append(diagnostics, "race_detector")
				}
				continue // Framework logs are not control messages or diagnostics.
			}
			var reply monitoringBudgetNodeReply
			if decodeErr = json.Unmarshal([]byte(strings.TrimPrefix(line, monitoringBudgetNodeFrame)), &reply); decodeErr != nil {
				break
			}
			received++
			replies <- reply
		}
		if decodeErr == nil {
			decodeErr = reader.Err()
		}
		if received != command.Requests && decodeErr == nil {
			decodeErr = io.ErrUnexpectedEOF
		}
		_, _ = io.Copy(io.Discard, stdout)
		runErr = cmd.Wait()
		if decodeErr != nil && received < command.Requests {
			replies <- monitoringBudgetNodeReply{Reason: "node_output_incomplete"}
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return monitoringBudgetNode{pid: cmd.Process.Pid, wait: func() {
		t.Helper()
		<-done
		require.NoError(t, runErr, "isolated monitoring node exited unsuccessfully; sanitized locations: %v", diagnostics)
		require.NoError(t, decodeErr, "isolated monitoring node returned incomplete results")
	}}
}

func monitoringBudgetNodeDSN(t *testing.T, db *sql.DB) string {
	t.Helper()
	u, err := url.Parse(integrationPostgresDSN)
	require.NoError(t, err)
	var name string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT current_database()`).Scan(&name))
	u.Path = "/" + name
	return u.String()
}

func receiveMonitoringBudgetReply(t *testing.T, replies <-chan monitoringBudgetNodeReply) monitoringBudgetNodeReply {
	t.Helper()
	select {
	case reply := <-replies:
		return reply
	case <-time.After(20 * time.Second):
		t.Fatal("isolated monitoring node did not return its bounded request result")
		return monitoringBudgetNodeReply{}
	}
}

func TestMonitoringProbeBudgetIndependentProcessesConcurrency(t *testing.T) {
	for _, scope := range []string{"global", "site", "key", "reserve"} {
		t.Run(scope, func(t *testing.T) {
			db, _ := newBillingAuditDatabase(t)
			repo := NewMonitoringProbeBudgetRepository(db)
			policy := monitoringBudgetPolicy()
			switch scope {
			case "global":
				policy.Concurrency = 4
			case "site":
				policy.SiteConcurrency = 4
			case "key":
				policy.KeyConcurrency = 4
			case "reserve":
				policy.Concurrency, policy.RecoveryReservePercent = 5, 20
			}
			_, err := repo.SaveSettings(t.Context(), policy)
			require.NoError(t, err)
			gate := make(chan struct{})
			release := sync.OnceFunc(func() { close(gate) })
			var calls, generations, active, maximum atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method == http.MethodPost {
					generations.Add(1)
				}
				current := active.Add(1)
				defer active.Add(-1)
				for previous := maximum.Load(); current > previous && !maximum.CompareAndSwap(previous, current); previous = maximum.Load() {
				}
				select {
				case <-gate:
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}`)
				case <-r.Context().Done():
				}
			}))
			t.Cleanup(server.Close)
			t.Cleanup(release)
			replies := make(chan monitoringBudgetNodeReply, 100)
			command := monitoringBudgetNodeCommand{DSN: monitoringBudgetNodeDSN(t, db), Site: server.URL,
				Path: "/v1/responses", Method: http.MethodPost, Requests: 50,
				Header: http.Header{"Authorization": []string{"Bearer fixture-shared-key"}}}
			first := startMonitoringBudgetNode(t, command, replies)
			command.Header = http.Header{"X-Api-Key": []string{"fixture-shared-key"}}
			command.Method, command.Path = http.MethodGet, "/balance"
			second := startMonitoringBudgetNode(t, command, replies)
			require.NotEqual(t, first.pid, second.pid)
			denial := monitoringBudgetNodeReply{Reason: "concurrency_limit", Scope: scope}
			if scope == "reserve" {
				denial.Reason, denial.Scope = "recovery_reserve", "global"
			}
			for range 96 {
				reply := receiveMonitoringBudgetReply(t, replies)
				require.Equal(t, denial, reply)
			}
			require.EqualValues(t, 4, calls.Load())
			require.EqualValues(t, 4, maximum.Load())
			command.Requests, command.Recovery = 1, true
			recovery := startMonitoringBudgetNode(t, command, replies)
			expected := int64(4)
			if scope == "reserve" {
				expected = 5
				require.Eventually(t, func() bool { return calls.Load() == expected }, 5*time.Second, 5*time.Millisecond)
				denial.Reason = "concurrency_limit"
				hardCap := startMonitoringBudgetNode(t, command, replies)
				require.Equal(t, denial, receiveMonitoringBudgetReply(t, replies))
				hardCap.wait()
			} else {
				require.Equal(t, denial, receiveMonitoringBudgetReply(t, replies), "recovery must not bypass any hard limit")
				recovery.wait()
			}
			summary, err := repo.Summary(t.Context())
			require.NoError(t, err)
			require.EqualValues(t, expected, summary.ActiveRequests)
			require.EqualValues(t, 4, summary.ActiveRegular)
			require.EqualValues(t, generations.Load(), summary.UsagePending)
			require.EqualValues(t, expected, summary.DayUsed)
			require.EqualValues(t, 4, summary.DayRegular)
			release()
			for range expected {
				require.Equal(t, monitoringBudgetNodeReply{Status: http.StatusOK}, receiveMonitoringBudgetReply(t, replies))
			}
			first.wait()
			second.wait()
			recovery.wait()
			summary, err = repo.Summary(t.Context())
			require.NoError(t, err)
			require.Zero(t, summary.ActiveRequests)
			require.Zero(t, summary.UsagePending)
			require.Zero(t, summary.UsageUnknown)
			require.EqualValues(t, 7*generations.Load(), summary.DayTokens)
			require.EqualValues(t, generations.Load(), summary.UsageReported)
			require.EqualValues(t, expected, calls.Load(), "denied work must not reach the upstream")
			require.EqualValues(t, expected, maximum.Load())
		})
	}
}

func TestMonitoringProbeBudgetIndependentProcessesTokenAndRecovery(t *testing.T) {
	db, _ := newBillingAuditDatabase(t)
	repo := NewMonitoringProbeBudgetRepository(db)
	policy := monitoringBudgetPolicy()
	policy.TokensPerDay = 7
	_, err := repo.SaveSettings(t.Context(), policy)
	require.NoError(t, err)
	var calls atomic.Int64
	entered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/v1/cancel/responses" {
			entered <- struct{}{}
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}`)
	}))
	t.Cleanup(server.Close)
	command := monitoringBudgetNodeCommand{DSN: monitoringBudgetNodeDSN(t, db), Site: server.URL,
		Path: "/v1/responses", Method: http.MethodPost, Requests: 1,
		Header: http.Header{"Authorization": []string{"Bearer fixture-shared-key"}}}
	replies := make(chan monitoringBudgetNodeReply, 1)
	run := func() monitoringBudgetNodeReply {
		t.Helper()
		node := startMonitoringBudgetNode(t, command, replies)
		reply := receiveMonitoringBudgetReply(t, replies)
		node.wait()
		return reply
	}
	require.Equal(t, monitoringBudgetNodeReply{Status: http.StatusOK}, run())
	command.Header = http.Header{"X-Api-Key": []string{"fixture-shared-key"}}
	require.Equal(t, monitoringBudgetNodeReply{Reason: "token_limit", Scope: "global_day"}, run())
	require.EqualValues(t, 1, calls.Load())
	command.Method, command.Path = http.MethodGet, "/balance"
	require.Equal(t, monitoringBudgetNodeReply{Status: http.StatusOK}, run(), "management calls remain available after token exhaustion")
	policy, err = repo.GetSettings(t.Context())
	require.NoError(t, err)
	policy.Enabled = false
	policy, err = repo.SaveSettings(t.Context(), policy)
	require.NoError(t, err)
	command.Method, command.Path = http.MethodPost, "/v1/responses"
	require.Equal(t, monitoringBudgetNodeReply{Status: http.StatusOK}, run(), "observation-only mode must keep actual accounting")
	policy.Enabled = true
	policy, err = repo.SaveSettings(t.Context(), policy)
	require.NoError(t, err)
	require.Equal(t, monitoringBudgetNodeReply{Reason: "token_limit", Scope: "global_day"}, run(), "new processes must not reset completed usage")
	policy.TokensPerDay = 0
	_, err = repo.SaveSettings(t.Context(), policy)
	require.NoError(t, err)
	command.Path, command.TimeoutMillis = "/v1/cancel/responses", 2000
	node := startMonitoringBudgetNode(t, command, replies)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("cancelable physical request did not reach the upstream")
	}
	require.Equal(t, monitoringBudgetNodeReply{Reason: "transport_error"}, receiveMonitoringBudgetReply(t, replies))
	node.wait()
	command.Path, command.TimeoutMillis = "/v1/responses", 0
	require.Equal(t, monitoringBudgetNodeReply{Status: http.StatusOK}, run(), "a different process can use the released slot")
	summary, err := repo.Summary(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 5, calls.Load())
	require.EqualValues(t, 5, summary.DayUsed)
	require.EqualValues(t, 21, summary.DayTokens)
	require.EqualValues(t, 3, summary.UsageReported)
	require.EqualValues(t, 1, summary.UsageUnknown, "canceled generation without usage is not free")
	require.Zero(t, summary.ActiveRequests)
	require.Zero(t, summary.UsagePending)
	var rows int64
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM usage_logs`).Scan(&rows))
	require.Zero(t, rows, "monitoring accounting must not create downstream bills")
}
