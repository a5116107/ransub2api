//go:build integration

package repository_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// The request URL and headers remain untouched; only the TCP destination is
// redirected to this test's own TLS server. No external provider is contacted.
type managedAvailabilityLoopback struct{ client *http.Client }

func (u *managedAvailabilityLoopback) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.client.Do(req)
}
func (u *managedAvailabilityLoopback) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, id, concurrency)
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
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		require.Equal(t, "/v1/responses", r.URL.Path)
		require.Contains(t, []string{"Bearer synthetic-first", "Bearer synthetic-second", "Bearer synthetic-rotated"}, r.Header.Get("Authorization"))
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
	probeCfg := &service.PelicanTestConfig{QuestionKind: "candy", Prompt: "Return 21", ReasoningEffort: "high", ParallelCount: 1,
		Availability: &service.AvailabilityPolicy{ValidForSeconds: 120}}
	next := time.Now().UTC().Add(-time.Minute)
	plan, err := plans.Create(ctx, &service.ScheduledTestPlan{AccountID: f.accountID, ModelID: "gpt-5.4", CronExpression: "0 * * * *", Enabled: true,
		MaxResults: 10, NextRunAt: &next, PelicanConfig: probeCfg})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, plans.Delete(context.Background(), plan.ID)) })
	claim := func() time.Time {
		now := time.Now().UTC().Truncate(time.Microsecond)
		until := now.Add(time.Minute)
		plan.TriggerSource = "upstream_5xx"
		claimed, claimErr := plans.ClaimPelican(ctx, plan, now, until, *plan.NextRunAt)
		require.NoError(t, claimErr)
		require.True(t, claimed)
		return until
	}
	probeAndPublish := func() {
		until := claim()
		result, probeErr := tester.RunPelicanBackground(ctx, f.accountID, plan.ModelID, probeCfg)
		require.NoError(t, probeErr)
		require.Equal(t, "success", result.Status, result.ErrorMessage)
		probe := result.PelicanConfig.AvailabilityResult
		require.Equal(t, "pass", probe.Status)
		require.NotNil(t, probe.Scope)
		require.NotEmpty(t, probe.Scope.ConnectionScope)
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

	probeAndPublish()
	require.EqualValues(t, 1, sends.Load())
	report, err := plans.ReadAvailability(ctx, plan.ID)
	require.NoError(t, err)
	require.NotEmpty(t, report.Models[0].Decision.Scope.ConnectionScope, "the repository must restore the private scope for admission")
	publicJSON, err := json.Marshal(report)
	require.NoError(t, err)
	require.NotContains(t, string(publicJSON), "connection_scope")
	require.NotContains(t, string(publicJSON), report.Models[0].Decision.Scope.ConnectionScope)
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
	selectConnection(first.ID, first.Revision)
	require.NoError(t, forward(), "the original connection can resume before its evidence expires")
	require.EqualValues(t, 3, sends.Load())
	rotated := connectionResponse(t, f.request(http.MethodPut, "/"+first.ID+"/api-key", fmt.Sprintf(`{"revision":%d,"api_key":"synthetic-rotated"}`, first.Revision)))
	require.Greater(t, rotated.Revision, first.Revision)
	require.Error(t, forward(), "a rotated connection must not reuse pre-rotation evidence")
	require.EqualValues(t, 3, sends.Load())
	probeAndPublish()
	require.NoError(t, forward(), "a completed fresh probe restores the rotated connection")
	require.EqualValues(t, 5, sends.Load())
}
