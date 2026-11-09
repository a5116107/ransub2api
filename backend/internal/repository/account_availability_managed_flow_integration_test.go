//go:build integration

package repository_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/qualityqueue"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// The request URL and headers remain untouched; only the TCP destination is
// redirected to this test's own TLS server. No external provider is contacted.
type managedAvailabilityLoopback struct{ client *http.Client }

func (u *managedAvailabilityLoopback) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.client.Do(req)
}

func TestAvailabilityManagedScheduledWorkerRotatesRealGrants(t *testing.T) {
	f := newConnectionHTTPFixture(t)
	ctx := t.Context()
	first := f.seedHistoricalAPIKey(t, "first", "synthetic-first")
	second := f.seedHistoricalAPIKey(t, "second", "synthetic-second")
	connectionResponse(t, f.request(http.MethodPatch, "/"+first.ID, fmt.Sprintf(`{"action":"select","revision":%d}`, first.Revision)))
	wireTokens := make(chan string, 8)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wireTokens <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"21\"}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixture\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n"))
	}))
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "example.com", MinVersion: tls.VersionTLS12}}
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	t.Cleanup(transport.CloseIdleConnections)
	upstream := &managedAvailabilityLoopback{client: &http.Client{Transport: transport, Timeout: 5 * time.Second}}
	cfg := &config.Config{}
	cfg.Default.RateMultiplier = 1
	plans := repository.NewScheduledTestPlanRepository(f.db)
	gateway := service.NewOpenAIGatewayService(f.accounts, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, service.NewBillingService(cfg, nil), nil, nil, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil, service.WithOpenAIAuthConnections(f.svc), service.WithOpenAIAvailabilityReader(plans))
	t.Cleanup(gateway.StopOpenAICodexTicketHarvester)
	t.Cleanup(gateway.CloseOpenAIWSPool)
	tester := service.NewAccountTestService(f.accounts, nil, nil, nil, nil, upstream, cfg, nil)
	tester.SetOpenAIGatewayService(gateway)
	probeCfg := &service.PelicanTestConfig{QuestionKind: "candy", Prompt: "Return 21", ReasoningEffort: "high", ParallelCount: 1, Availability: &service.AvailabilityPolicy{ValidForSeconds: 120}}
	next := time.Now().Add(time.Hour)
	plan, err := plans.Create(ctx, &service.ScheduledTestPlan{AccountID: f.accountID, ModelID: "gpt-5.4", CronExpression: "0 * * * *", Enabled: true, MaxResults: 10, NextRunAt: &next, PelicanConfig: probeCfg})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, plans.Delete(context.Background(), plan.ID)) })
	results := repository.NewScheduledTestResultRepository(f.db)
	scheduled := service.NewScheduledTestService(plans, results)
	queue := qualityqueue.NewRedis(repository.Issue594TestRedis())
	t.Cleanup(func() { require.NoError(t, queue.Close()) })
	runner := service.ProvideScheduledTestRunnerService(plans, scheduled, tester, nil, cfg, &service.QualityJudgeService{}, repository.Issue594TestRedis(), nil, &service.ChannelMonitorV2Service{})
	t.Cleanup(runner.Stop)
	for round := 1; round <= 2; round++ {
		require.NoError(t, queue.Enqueue(ctx, f.accountID, time.Now()))
		require.Eventually(t, func() bool {
			var count int
			err := f.db.QueryRowContext(ctx, `SELECT count(*) FROM scheduled_test_results WHERE plan_id=$1`, plan.ID).Scan(&count)
			_, queued := queue.Score(ctx, f.accountID)
			return err == nil && count >= round && errors.Is(queued, redis.Nil)
		}, 20*time.Second, 100*time.Millisecond, "the existing worker must claim, test, persist and acknowledge each signal")
	}
	got := []string{<-wireTokens, <-wireTokens}
	require.ElementsMatch(t, []string{"Bearer synthetic-first", "Bearer synthetic-second"}, got, "the worker must probe both grants without changing the preferred connection")
	var count int
	require.NoError(t, f.db.QueryRowContext(ctx, `SELECT count(*) FROM account_availability_states s CROSS JOIN LATERAL jsonb_object_keys(s.state->'connections') WHERE s.plan_id=$1 AND s.model_id=$2`, plan.ID, plan.ModelID).Scan(&count))
	require.Equal(t, 2, count)
	var internalState string
	require.NoError(t, f.db.QueryRowContext(ctx, `SELECT state::text FROM account_availability_states WHERE plan_id=$1 AND model_id=$2`, plan.ID, plan.ModelID).Scan(&internalState))
	for _, value := range []string{first.ID, second.ID, "synthetic-first", "synthetic-second"} {
		require.NotContains(t, internalState, value, "the cursor and decisions must not persist raw grants or credentials")
	}
	currentPlan, err := plans.GetByID(ctx, plan.ID)
	require.NoError(t, err)
	require.NotNil(t, currentPlan.NextRunAt)
	require.WithinDuration(t, next, *currentPlan.NextRunAt, time.Millisecond, "successful rotation must not increase the configured cron frequency")
	for _, connection := range []*service.OpenAIAuthConnection{first, second} {
		connectionResponse(t, f.request(http.MethodPatch, "/"+connection.ID, fmt.Sprintf(`{"action":"select","revision":%d}`, connection.Revision)))
		account, getErr := f.accounts.GetByID(ctx, f.accountID)
		require.NoError(t, getErr)
		body := `{"model":"gpt-5.4","input":"different prompt","stream":true,"reasoning":{"effort":"high"}}`
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		_, forwardErr := gateway.Forward(ctx, c, account, []byte(body))
		require.NoError(t, forwardErr, "each automatically observed grant must pass the actual gateway admission")
	}
}

func TestAvailabilityManagedScheduledWorkerCoversBudgetedGrants(t *testing.T) {
	f := newConnectionHTTPFixture(t)
	ctx := t.Context()
	first := f.seedHistoricalAPIKey(t, "first", "synthetic-first")
	second := f.seedHistoricalAPIKey(t, "second", "synthetic-second")
	third := f.seedHistoricalAPIKey(t, "third", "synthetic-third")
	connectionResponse(t, f.request(http.MethodPatch, "/"+first.ID, fmt.Sprintf(`{"action":"select","revision":%d}`, first.Revision)))
	wireTokens := make(chan string, 8)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wireTokens <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"21\"}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixture\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n"))
	}))
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "example.com", MinVersion: tls.VersionTLS12}}
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	t.Cleanup(transport.CloseIdleConnections)
	upstream := &managedAvailabilityLoopback{client: &http.Client{Transport: transport, Timeout: 5 * time.Second}}
	cfg := &config.Config{Timezone: "UTC"}
	cfg.Default.RateMultiplier = 1
	plans := repository.NewScheduledTestPlanRepository(f.db)
	gateway := service.NewOpenAIGatewayService(f.accounts, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, service.NewBillingService(cfg, nil), nil, nil, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil, service.WithOpenAIAuthConnections(f.svc), service.WithOpenAIAvailabilityReader(plans))
	t.Cleanup(gateway.StopOpenAICodexTicketHarvester)
	t.Cleanup(gateway.CloseOpenAIWSPool)
	tester := service.NewAccountTestService(f.accounts, nil, nil, nil, nil, upstream, cfg, nil)
	tester.SetOpenAIGatewayService(gateway)
	probeCfg := &service.PelicanTestConfig{QuestionKind: "candy", Prompt: "Return 21", ReasoningEffort: "high", ParallelCount: 1,
		Availability: &service.AvailabilityPolicy{ValidForSeconds: 30, MaxExtraSamples: 2},
		Quality:      &service.QualityPolicy{Action: service.QualityActionObserveOnly, ExpectedAnswer: "21"}}
	next := time.Now().UTC().Add(-time.Minute)
	plan, err := plans.Create(ctx, &service.ScheduledTestPlan{AccountID: f.accountID, ModelID: "gpt-5.4", CronExpression: "* * * * *", Enabled: true, MaxResults: 3, NextRunAt: &next, PelicanConfig: probeCfg})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, plans.Delete(context.Background(), plan.ID)) })
	results := repository.NewScheduledTestResultRepository(f.db)
	scheduled := service.NewScheduledTestService(plans, results)
	runner := service.NewScheduledTestRunnerService(plans, scheduled, tester, nil, cfg)
	runner.Start()
	t.Cleanup(runner.Stop)
	require.Eventually(t, func() bool {
		var count, extra int
		err := f.db.QueryRowContext(ctx, `SELECT count(*) FROM account_availability_states s CROSS JOIN LATERAL jsonb_object_keys(s.state->'connections') WHERE s.plan_id=$1 AND s.model_id=$2`, plan.ID, plan.ModelID).Scan(&count)
		if err != nil || count != 3 {
			return false
		}
		err = f.db.QueryRowContext(ctx, `SELECT count(*) FROM scheduled_test_results WHERE plan_id=$1 AND pelican_config->>'trigger_source'='availability_coverage'`, plan.ID).Scan(&extra)
		return err == nil && extra == 2
	}, 85*time.Second, 100*time.Millisecond, "the actual scheduled worker must publish each budgeted connection")
	require.Len(t, wireTokens, 3)
	require.ElementsMatch(t, []string{"Bearer synthetic-first", "Bearer synthetic-second", "Bearer synthetic-third"}, []string{<-wireTokens, <-wireTokens, <-wireTokens})
	var extra int
	require.NoError(t, f.db.QueryRowContext(ctx, `SELECT count(*) FROM scheduled_test_results WHERE plan_id=$1 AND pelican_config->>'trigger_source'='availability_coverage'`, plan.ID).Scan(&extra))
	require.Equal(t, 2, extra)
	var hasQuality int
	require.NoError(t, f.db.QueryRowContext(ctx, `SELECT count(*) FROM scheduled_test_results WHERE plan_id=$1 AND pelican_config->>'trigger_source'='availability_coverage' AND (pelican_config ? 'quality' OR quality_round_id <> '' OR quality_action <> '' OR quality_judgment IS NOT NULL)`, plan.ID).Scan(&hasQuality))
	require.Zero(t, hasQuality, "extra availability samples must not be quality evidence")
	history, err := results.ListQualityHistory(ctx, 0, 100)
	require.NoError(t, err)
	qualityRounds := 0
	for _, item := range history {
		if item.PlanID == plan.ID {
			qualityRounds++
			require.Equal(t, "scheduled", item.PelicanConfig.TriggerSource)
		}
	}
	require.Equal(t, 1, qualityRounds, "only the base scheduled sample belongs to quality history")
	var lastRun, scheduledFinished time.Time
	require.NoError(t, f.db.QueryRowContext(ctx, `SELECT last_run_at FROM scheduled_test_plans WHERE id=$1`, plan.ID).Scan(&lastRun))
	require.NoError(t, f.db.QueryRowContext(ctx, `SELECT finished_at FROM scheduled_test_results WHERE plan_id=$1 AND pelican_config->>'trigger_source'='scheduled' ORDER BY id LIMIT 1`, plan.ID).Scan(&scheduledFinished))
	require.WithinDuration(t, scheduledFinished, lastRun, time.Second, "availability-only rounds must not overwrite the quality schedule's last run")
	current, err := plans.GetByID(ctx, plan.ID)
	require.NoError(t, err)
	require.NotNil(t, current.NextRunAt)
	require.True(t, current.NextRunAt.After(time.Now()), "extra rounds must retain the normal next cron time")
	for _, connection := range []*service.OpenAIAuthConnection{first, second, third} {
		connectionResponse(t, f.request(http.MethodPatch, "/"+connection.ID, fmt.Sprintf(`{"action":"select","revision":%d}`, connection.Revision)))
		account, getErr := f.accounts.GetByID(ctx, f.accountID)
		require.NoError(t, getErr)
		body := `{"model":"gpt-5.4","input":"fixture","stream":true,"reasoning":{"effort":"high"}}`
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		_, forwardErr := gateway.Forward(ctx, c, account, []byte(body))
		require.NoError(t, forwardErr, "each actually probed connection must pass the gateway's final admission")
	}
}

func (u *managedAvailabilityLoopback) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, id, concurrency)
}

func TestAvailabilityManagedBPSIncompleteKeepsHealthyGrantWithPostgres(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	f := newConnectionHTTPFixture(t)
	ctx := t.Context()
	seed := func(label, token string) *service.OpenAIAuthConnection {
		t.Helper()
		connection := &service.OpenAIAuthConnection{ID: uuid.NewString(), AccountID: f.accountID, Kind: "bps", Label: label, Status: "active", RefreshState: "idle", Revision: 1,
			ClientID: openai.ExcelClientID, Subject: label, Scopes: []string{}, Capabilities: []string{"responses"}}
		raw, err := json.Marshal(map[string]any{"_connection_id": connection.ID, "_account_id": strconv.FormatInt(connection.AccountID, 10), "_connection_kind": connection.Kind,
			"_connection_subject": connection.Subject, "client_id": connection.ClientID, "access_token": token, "chatgpt_account_id": "synthetic-workspace", "expires_at": strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)})
		require.NoError(t, err)
		connection.CredentialsCiphertext, err = f.encryptor.Encrypt(string(raw))
		require.NoError(t, err)
		require.NoError(t, f.repo.CreateAuthConnection(ctx, connection))
		return connection
	}
	first := seed("first", "synthetic-first")
	second := seed("second", "synthetic-second")
	selectConnection := func(connection *service.OpenAIAuthConnection) {
		t.Helper()
		connectionResponse(t, f.request(http.MethodPatch, "/"+connection.ID, fmt.Sprintf(`{"action":"select","revision":%d}`, connection.Revision)))
	}
	selectConnection(first)

	var sends atomic.Int32
	var failFirst, badRequest atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		if r.URL.Path != "/basispoints/api/responses" || (r.Header.Get("Authorization") != "Bearer synthetic-first" && r.Header.Get("Authorization") != "Bearer synthetic-second") {
			badRequest.Store(true)
			http.Error(w, "unexpected fixture request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"21\"}\n\n"))
		if !failFirst.Load() || r.Header.Get("Authorization") != "Bearer synthetic-first" {
			_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixture\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n"))
		}
	}))
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "example.com", MinVersion: tls.VersionTLS12}}
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	t.Cleanup(transport.CloseIdleConnections)
	upstream := &managedAvailabilityLoopback{client: &http.Client{Transport: transport, Timeout: 5 * time.Second}}
	cfg := &config.Config{}
	cfg.Default.RateMultiplier = 1
	plans := repository.NewScheduledTestPlanRepository(f.db)
	limiter := service.ProvideRateLimitService(f.accounts, nil, cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	gateway := service.NewOpenAIGatewayService(f.accounts, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, service.NewBillingService(cfg, nil), limiter, nil, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil, service.WithOpenAIAuthConnections(f.svc), service.WithOpenAIAvailabilityReader(plans))
	t.Cleanup(gateway.StopOpenAICodexTicketHarvester)
	t.Cleanup(gateway.CloseOpenAIWSPool)
	t.Cleanup(func() { require.NoError(t, gateway.CloseExcelBPSImages()) })
	tester := service.NewAccountTestService(f.accounts, nil, nil, nil, nil, upstream, cfg, nil)
	tester.SetOpenAIGatewayService(gateway)
	probeCfg := &service.PelicanTestConfig{QuestionKind: "candy", Prompt: "Return 21", ReasoningEffort: "high", ParallelCount: 1, Availability: &service.AvailabilityPolicy{ValidForSeconds: 120}}
	next := time.Now().UTC().Add(-time.Minute)
	plan, err := plans.Create(ctx, &service.ScheduledTestPlan{AccountID: f.accountID, ModelID: "gpt-5.4", CronExpression: "0 * * * *", Enabled: true, MaxResults: 10, NextRunAt: &next, PelicanConfig: probeCfg})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, plans.Delete(context.Background(), plan.ID)) })
	probeAndPublish := func() {
		t.Helper()
		var err error
		plan, err = plans.GetByID(ctx, plan.ID)
		require.NoError(t, err)
		now := time.Now().UTC().Truncate(time.Microsecond)
		until := now.Add(time.Minute)
		plan.TriggerSource = "upstream_5xx"
		claimed, err := plans.ClaimPelican(ctx, plan, now, until, *plan.NextRunAt)
		require.NoError(t, err)
		require.True(t, claimed)
		result, err := tester.RunPelicanBackground(ctx, f.accountID, plan.ModelID, probeCfg)
		require.NoError(t, err)
		require.Equal(t, "success", result.Status, result.ErrorMessage)
		probe := result.PelicanConfig.AvailabilityResult
		require.Equal(t, "pass", probe.Status)
		require.NotNil(t, probe.Scope)
		require.NotEmpty(t, probe.Scope.ConnectionScope)
		require.Eventually(t, func() bool {
			var dbNow time.Time
			return f.db.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&dbNow) == nil && !dbNow.Before(probe.FinishedAt)
		}, 2*time.Second, 10*time.Millisecond)
		action, err := plans.ApplyAvailabilityOutcomes(ctx, plan, until, []service.AvailabilityModelOutcome{{ModelID: plan.ModelID, Probe: probe}})
		require.NoError(t, err)
		require.Equal(t, "observed", action)
		require.NoError(t, plans.FinishPelican(ctx, plan.ID, until, time.Now()))
	}
	forward := func() error {
		t.Helper()
		account, err := f.accounts.GetByID(ctx, f.accountID)
		require.NoError(t, err)
		body := `{"model":"gpt-5.4","input":"fixture","stream":true,"reasoning":{"effort":"high"}}`
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		_, err = gateway.Forward(ctx, c, account, []byte(body))
		return err
	}

	probeAndPublish()
	selectConnection(second)
	probeAndPublish()
	require.NoError(t, forward(), "the peer must have its own admitted evidence")
	selectConnection(first)
	failFirst.Store(true)
	before := sends.Load()
	require.Error(t, forward(), "the first connection's incomplete real response must fail")
	require.Equal(t, before+1, sends.Load())
	selectConnection(second)
	require.NoError(t, forward(), "the healthy peer must retain its own persisted pass")
	selectConnection(first)
	before = sends.Load()
	require.Error(t, forward(), "the failed connection must be stopped before another send")
	require.Equal(t, before, sends.Load())
	failFirst.Store(false)
	probeAndPublish()
	require.NoError(t, forward(), "a fresh probe must restore the failed connection")
	require.False(t, badRequest.Load())
}

func TestAvailabilityManagedProbeToGatewayWithPostgres(t *testing.T) {
	f := newConnectionHTTPFixture(t)
	ctx := t.Context()
	first := f.seedHistoricalAPIKey(t, "first", "synthetic-first")
	second := f.seedHistoricalAPIKey(t, "second", "synthetic-second")
	selectConnection := func(id string, revision int64) {
		connectionResponse(t, f.request(http.MethodPatch, "/"+id, fmt.Sprintf(`{"action":"select","revision":%d}`, revision)))
	}
	selectConnection(first.ID, first.Revision)

	var sends atomic.Int32
	var rejectFirst atomic.Bool
	var businessFailure atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		require.Equal(t, "/v1/responses", r.URL.Path)
		require.Contains(t, []string{"Bearer synthetic-first", "Bearer synthetic-second", "Bearer synthetic-rotated"}, r.Header.Get("Authorization"))
		if status := businessFailure.Load(); status != 0 && r.Header.Get("Authorization") != "Bearer synthetic-second" {
			if status == -1 {
				w.Header().Set("Content-Length", "9999")
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"))
			} else {
				w.WriteHeader(int(status))
				_, _ = w.Write([]byte(`{"error":"fixture-business-failure"}`))
			}
			return
		}
		if rejectFirst.Load() && r.Header.Get("Authorization") == "Bearer synthetic-first" {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"fixture-unavailable"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"21\"}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixture\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n"))
	}))
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "example.com", MinVersion: tls.VersionTLS12}}
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	t.Cleanup(transport.CloseIdleConnections)
	upstream := &managedAvailabilityLoopback{client: &http.Client{Transport: transport, Timeout: 5 * time.Second}}
	cfg := &config.Config{}
	cfg.Default.RateMultiplier = 1
	plans := repository.NewScheduledTestPlanRepository(f.db)
	limiter := service.ProvideRateLimitService(f.accounts, nil, cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	gateway := service.NewOpenAIGatewayService(f.accounts, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, service.NewBillingService(cfg, nil), limiter, nil, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil, service.WithOpenAIAuthConnections(f.svc), service.WithOpenAIAvailabilityReader(plans))
	t.Cleanup(gateway.StopOpenAICodexTicketHarvester)
	t.Cleanup(gateway.CloseOpenAIWSPool)
	tester := service.NewAccountTestService(f.accounts, nil, nil, nil, nil, upstream, cfg, nil)
	tester.SetOpenAIGatewayService(gateway)
	probeCfg := &service.PelicanTestConfig{QuestionKind: "candy", Prompt: "Return 21", ReasoningEffort: "high", ParallelCount: 1,
		Availability: &service.AvailabilityPolicy{ValidForSeconds: 120}}
	next := time.Now().UTC().Add(-time.Minute)
	plan, err := plans.Create(ctx, &service.ScheduledTestPlan{AccountID: f.accountID, ModelID: "gpt-5.4", CronExpression: "0 * * * *", Enabled: true,
		MaxResults: 10, NextRunAt: &next, PelicanConfig: probeCfg})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, plans.Delete(context.Background(), plan.ID)) })
	claim := func() time.Time {
		var getErr error
		plan, getErr = plans.GetByID(ctx, plan.ID)
		require.NoError(t, getErr)
		now := time.Now().UTC().Truncate(time.Microsecond)
		until := now.Add(time.Minute)
		plan.TriggerSource = "upstream_5xx"
		claimed, claimErr := plans.ClaimPelican(ctx, plan, now, until, *plan.NextRunAt)
		require.NoError(t, claimErr)
		require.True(t, claimed)
		return until
	}
	probeAndPublish := func(want string) {
		until := claim()
		result, probeErr := tester.RunPelicanBackground(ctx, f.accountID, plan.ModelID, probeCfg)
		require.NoError(t, probeErr)
		if want == "pass" {
			require.Equal(t, "success", result.Status, result.ErrorMessage)
		} else {
			require.Equal(t, "failed", result.Status)
		}
		probe := result.PelicanConfig.AvailabilityResult
		require.Equal(t, want, probe.Status)
		require.NotNil(t, probe.Scope)
		require.NotEmpty(t, probe.Scope.ConnectionScope)
		// Windows and Docker have independent wall clocks: let the database
		// catch up before publishing, without changing the observed timestamp.
		require.Eventually(t, func() bool {
			var dbNow time.Time
			return f.db.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&dbNow) == nil && !dbNow.Before(probe.FinishedAt)
		}, 2*time.Second, 10*time.Millisecond)
		action, writeErr := plans.ApplyAvailabilityOutcomes(ctx, plan, until, []service.AvailabilityModelOutcome{{ModelID: plan.ModelID, Probe: probe}})
		require.NoError(t, writeErr)
		require.Equal(t, "observed", action)
		require.NoError(t, plans.FinishPelican(ctx, plan.ID, until, time.Now()))
	}
	forward := func() error {
		account, getErr := f.accounts.GetByID(ctx, f.accountID)
		require.NoError(t, getErr)
		body := `{"model":"gpt-5.4","input":"different prompt","stream":true,"reasoning":{"effort":"high"}}`
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		_, forwardErr := gateway.Forward(ctx, c, account, []byte(body))
		return forwardErr
	}

	// A pre-upgrade account-level pass has no managed-connection evidence.
	until := claim()
	finished := time.Now().UTC().Add(-time.Second)
	legacy := &service.AvailabilityProbeResult{Version: 1, Status: "pass", Reason: "completed_response", StartedAt: finished.Add(-time.Second), FinishedAt: finished,
		Scope: &service.AvailabilityRequestScope{Version: 1, Protocol: "responses_http", Stream: true, Model: plan.ModelID, ServiceTier: "unspecified", ReasoningEffort: "high", Capability: "text"}}
	_, err = plans.ApplyAvailabilityOutcomes(ctx, plan, until, []service.AvailabilityModelOutcome{{ModelID: plan.ModelID, Probe: legacy}})
	require.NoError(t, err)
	require.NoError(t, plans.FinishPelican(ctx, plan.ID, until, time.Now()))
	require.Error(t, forward(), "a parent-only pass must not authorize a managed grant")
	require.Zero(t, sends.Load())

	probeAndPublish("pass")
	require.EqualValues(t, 1, sends.Load())
	report, err := plans.ReadAvailability(ctx, plan.ID)
	require.NoError(t, err)
	require.Equal(t, "unknown", report.Models[0].Status, "an unselected plan-level report must not claim all connections are healthy")
	require.Equal(t, "connection_not_selected", report.Models[0].Reason)
	publicJSON, err := json.Marshal(report)
	require.NoError(t, err)
	require.NotContains(t, string(publicJSON), "connection_scope")
	require.NotContains(t, string(publicJSON), "synthetic-first")
	until = claim()
	_, err = plans.ApplyAvailabilityOutcomes(ctx, plan, until, []service.AvailabilityModelOutcome{{ModelID: plan.ModelID,
		Probe: &service.AvailabilityProbeResult{Version: 1, Status: "inconclusive", Reason: "not_observed"}}})
	require.NoError(t, err)
	require.NoError(t, plans.FinishPelican(ctx, plan.ID, until, time.Now()))
	require.NoError(t, forward(), "the observed connection and scope must be admitted")
	require.EqualValues(t, 2, sends.Load())
	selectConnection(second.ID, second.Revision)
	require.Error(t, forward(), "a peer connection cannot borrow the first connection's pass")
	require.EqualValues(t, 2, sends.Load())
	probeAndPublish("pass")
	require.NoError(t, forward(), "the second connection may use its own freshly observed pass")
	require.EqualValues(t, 4, sends.Load())
	selectConnection(first.ID, first.Revision)
	require.NoError(t, forward(), "a peer's later pass must not overwrite the original connection's unexpired evidence")
	require.EqualValues(t, 5, sends.Load())
	businessFailure.Store(http.StatusRequestTimeout)
	require.Error(t, forward(), "the real first-connection failure must reach the caller")
	failedSends := sends.Load()
	require.Greater(t, failedSends, int32(5))
	selectConnection(second.ID, second.Revision)
	require.NoError(t, forward(), "a confirmed business failure on the first grant must not invalidate a healthy peer")
	require.EqualValues(t, failedSends+1, sends.Load())
	selectConnection(first.ID, first.Revision)
	businessFailure.Store(0)
	probeAndPublish("pass")
	require.NoError(t, forward(), "a fresh probe must restore the failed grant")
	businessFailure.Store(-1)
	beforeBrokenBody := sends.Load()
	require.Error(t, forward(), "a truncated real response body must fail")
	require.Greater(t, sends.Load(), beforeBrokenBody)
	selectConnection(second.ID, second.Revision)
	require.NoError(t, forward(), "a body read fault on one grant must preserve a healthy peer")
	selectConnection(first.ID, first.Revision)
	businessFailure.Store(0)
	probeAndPublish("pass")
	require.NoError(t, forward(), "the body-failed grant must recover with a fresh probe")
	base := sends.Load()
	rejectFirst.Store(true)
	probeAndPublish("fail")
	require.Error(t, forward(), "an observed failure must stop its own connection")
	require.EqualValues(t, base+1, sends.Load())
	selectConnection(second.ID, second.Revision)
	require.NoError(t, forward(), "a peer with valid evidence stays available after the first connection fails")
	require.EqualValues(t, base+2, sends.Load())
	selectConnection(first.ID, first.Revision)
	rejectFirst.Store(false)
	probeAndPublish("pass")
	require.NoError(t, forward(), "a fresh successful probe recovers only the failed connection")
	require.EqualValues(t, base+4, sends.Load())
	rotated := connectionResponse(t, f.request(http.MethodPut, "/"+first.ID+"/api-key", fmt.Sprintf(`{"revision":%d,"api_key":"synthetic-rotated"}`, first.Revision)))
	require.Greater(t, rotated.Revision, first.Revision)
	require.Error(t, forward(), "a rotated connection must not reuse pre-rotation evidence")
	require.EqualValues(t, base+4, sends.Load())
	probeAndPublish("pass")
	require.NoError(t, forward(), "a completed fresh probe restores the rotated connection")
	require.EqualValues(t, base+6, sends.Load())
	selectConnection(second.ID, second.Revision)
	require.NoError(t, forward(), "rotating and probing one connection must retain a peer's unexpired evidence")
	require.EqualValues(t, base+7, sends.Load())
	selectConnection(first.ID, rotated.Revision)
	businessFailure.Store(http.StatusServiceUnavailable)
	before503 := sends.Load()
	require.Error(t, forward(), "a real 503 must be observed before failover")
	require.Greater(t, sends.Load(), before503)
	selectConnection(second.ID, second.Revision)
	require.NoError(t, forward(), "the native 5xx observer must retain a healthy peer's evidence")
}
