//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type monitorKeyAdmissionStub struct {
	blocked  atomic.Bool
	attempts atomic.Int32
	finished atomic.Int32
}

func (s *monitorKeyAdmissionStub) BeginUpstreamKeyRequest(req *http.Request) (*http.Request, func(*http.Response, error), error) {
	s.attempts.Add(1)
	if s.blocked.Load() {
		return req, nil, ErrUpstreamKeyCapacity
	}
	return req, func(*http.Response, error) { s.finished.Add(1) }, nil
}

func TestChannelMonitorSharedKeyRejectionSkipsObservationAndRecovers(t *testing.T) {
	swapMonitorHTTPClient(t)
	var sends atomic.Int32
	handler := &openAICaptureHandler{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		sends.Add(1)
		handler.ServeHTTP(w, req)
	}))
	t.Cleanup(server.Close)
	repo := &quotaModeRepoStub{monitor: &ChannelMonitor{ID: 91, Provider: MonitorProviderOpenAI,
		Endpoint: server.URL, APIKey: "OLD:sk-openai", PrimaryModel: "gpt-test", Enabled: true, IntervalSeconds: 60}}
	svc := newQuotaModeService(repo)
	admission := &monitorKeyAdmissionStub{}
	admission.blocked.Store(true)
	svc.upstreamKeyAdmission = admission
	svc.requireUpstreamKeyAdmission = true
	results, err := svc.RunCheck(context.Background(), 91)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.True(t, results[0].Skipped, "local quota rejection is not an upstream observation")
	require.Zero(t, sends.Load(), "a full shared key must not send the monitor generation")
	require.Empty(t, repo.history, "local capacity must not overwrite the last upstream status")
	require.Empty(t, repo.markedIDs, "a skipped check must not look like a fresh observation")
	admission.blocked.Store(false)
	results, err = svc.RunCheck(context.Background(), 91)
	require.NoError(t, err)
	require.False(t, results[0].Skipped)
	require.Equal(t, MonitorStatusOperational, results[0].Status)
	require.EqualValues(t, 1, sends.Load())
	require.Len(t, repo.history, 1)
	require.Len(t, repo.markedIDs, 1)
	require.EqualValues(t, 2, admission.attempts.Load())
	require.EqualValues(t, 1, admission.finished.Load())
}

func TestChannelMonitorMissingConfiguredAuthoritySkips(t *testing.T) {
	swapMonitorHTTPClient(t)
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { sends.Add(1) }))
	t.Cleanup(server.Close)
	for _, mode := range []string{"static", "regional", "scheduler"} {
		t.Run(mode, func(t *testing.T) {
			repo := &quotaModeRepoStub{monitor: &ChannelMonitor{ID: 92, Provider: MonitorProviderOpenAI,
				Endpoint: server.URL, APIKey: "OLD:sk-openai", PrimaryModel: "gpt-test"}}
			svc := newQuotaModeService(repo)
			cfg := &config.Config{}
			switch mode {
			case "static":
				cfg.Gateway.UpstreamKeyConcurrency.Revision = 1
			case "regional":
				cfg.Runtime.RegionalConcurrencyFile = "fixture"
			case "scheduler":
				cfg.Runtime.RegionalSchedulerFile = "fixture"
			}
			svc.SetUpstreamKeyAuthority(cfg, &httpUpstreamRecorder{})
			results, err := svc.RunCheck(t.Context(), 92)
			require.NoError(t, err)
			require.True(t, results[0].Skipped)
			require.Empty(t, repo.history)
			require.Empty(t, repo.markedIDs)
		})
	}
	require.Zero(t, sends.Load())
}

type selectiveMonitorAdmission struct{}

func (selectiveMonitorAdmission) BeginUpstreamKeyRequest(req *http.Request) (*http.Request, func(*http.Response, error), error) {
	body, err := req.GetBody()
	if err != nil {
		return req, nil, err
	}
	defer func() { _ = body.Close() }()
	var data struct {
		Model string `json:"model"`
	}
	if err = json.NewDecoder(body).Decode(&data); err != nil {
		return req, nil, err
	}
	if data.Model == "blocked-model" {
		return req, nil, ErrUpstreamKeyCapacity
	}
	return req, func(*http.Response, error) {}, nil
}

func TestChannelMonitorPartialSkipOnlyPersistsActualObservation(t *testing.T) {
	h := &openAICaptureHandler{}
	endpoint := setupFakeOpenAI(t, h)
	repo := &quotaModeRepoStub{monitor: &ChannelMonitor{ID: 93, Provider: MonitorProviderOpenAI,
		Endpoint: endpoint, APIKey: "OLD:sk-openai", PrimaryModel: "blocked-model", ExtraModels: []string{"passing-model"}}}
	svc := newQuotaModeService(repo)
	svc.upstreamKeyAdmission, svc.requireUpstreamKeyAdmission = selectiveMonitorAdmission{}, true
	results, err := svc.RunCheck(t.Context(), 93)
	require.NoError(t, err)
	require.Len(t, results, 2)
	require.True(t, results[0].Skipped)
	require.False(t, results[1].Skipped)
	require.Equal(t, MonitorStatusOperational, results[1].Status)
	require.Len(t, repo.history, 1)
	require.Equal(t, "passing-model", repo.history[0].Model)
	require.Len(t, repo.markedIDs, 1)
}

type monitorLeaseLossAdmission struct{ cancels chan context.CancelCauseFunc }

func (s *monitorLeaseLossAdmission) BeginUpstreamKeyRequest(req *http.Request) (*http.Request, func(*http.Response, error), error) {
	ctx, cancel := context.WithCancelCause(req.Context())
	s.cancels <- cancel
	return req.WithContext(ctx), func(*http.Response, error) {}, nil
}

func TestChannelMonitorLostLeaseDoesNotRecordSupplierFailure(t *testing.T) {
	swapMonitorHTTPClient(t)
	admission := &monitorLeaseLossAdmission{cancels: make(chan context.CancelCauseFunc, 1)}
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		sends.Add(1)
		(<-admission.cancels)(ErrUpstreamKeyLease)
		_, _ = io.WriteString(w, "unused")
	}))
	t.Cleanup(server.Close)
	repo := &quotaModeRepoStub{monitor: &ChannelMonitor{ID: 94, Provider: MonitorProviderOpenAI,
		Endpoint: server.URL, APIKey: "OLD:sk-openai", PrimaryModel: "gpt-test"}}
	svc := newQuotaModeService(repo)
	svc.upstreamKeyAdmission, svc.requireUpstreamKeyAdmission = admission, true
	results, err := svc.RunCheck(t.Context(), 94)
	require.NoError(t, err)
	require.True(t, results[0].Skipped)
	require.EqualValues(t, 1, sends.Load(), "lease loss may occur after sending")
	require.Empty(t, repo.history)
	require.Empty(t, repo.markedIDs)
}

func TestChannelMonitorSharedKeyModeDoesNotFollowRedirect(t *testing.T) {
	swapMonitorHTTPClient(t)
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Add(1)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	t.Cleanup(destination.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, destination.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(origin.Close)
	opts := &CheckOptions{keyAdmission: &monitorKeyAdmissionStub{}, requireKeyAdmission: true}
	_, status, err := postRawJSON(t.Context(), origin.URL, []byte(`{}`), map[string]string{"Authorization": "Bearer fixture"}, opts)
	require.NoError(t, err)
	require.Equal(t, http.StatusTemporaryRedirect, status)
	require.Zero(t, redirected.Load())
	_, status, err = postRawJSON(t.Context(), origin.URL, []byte(`{}`), nil, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status, "unconfigured legacy mode retains its redirect behavior")
	require.EqualValues(t, 1, redirected.Load())
}

func TestChannelMonitorHeaderOverridesIgnoreCasing(t *testing.T) {
	opts := &CheckOptions{ExtraHeaders: map[string]string{"authorization": "Bearer replacement"}}
	result := mergeHeaders(map[string]string{"Authorization": "Bearer original"}, opts)
	require.Len(t, result, 1)
	require.Equal(t, "Bearer replacement", result["Authorization"])
}

func TestChannelMonitorUpstreamFailureStillRecordsObservation(t *testing.T) {
	h := &openAICaptureHandler{status: http.StatusServiceUnavailable}
	endpoint := setupFakeOpenAI(t, h)
	repo := &quotaModeRepoStub{monitor: &ChannelMonitor{ID: 95, Provider: MonitorProviderOpenAI,
		Endpoint: endpoint, APIKey: "OLD:sk-openai", PrimaryModel: "gpt-test"}}
	svc := newQuotaModeService(repo)
	svc.upstreamKeyAdmission, svc.requireUpstreamKeyAdmission = &monitorKeyAdmissionStub{}, true
	results, err := svc.RunCheck(t.Context(), 95)
	require.NoError(t, err)
	require.False(t, results[0].Skipped)
	require.Equal(t, MonitorStatusError, results[0].Status)
	require.Len(t, repo.history, 1)
	require.Len(t, repo.markedIDs, 1)
}

type monitorTestTransport func(*http.Request) (*http.Response, error)

func (f monitorTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type monitorBufferedLeaseLossAdmission struct{}

func (monitorBufferedLeaseLossAdmission) BeginUpstreamKeyRequest(req *http.Request) (*http.Request, func(*http.Response, error), error) {
	ctx, cancel := context.WithCancelCause(req.Context())
	return req.WithContext(ctx), func(*http.Response, error) { cancel(ErrUpstreamKeyLease) }, nil
}

func TestChannelMonitorBufferedResponseAfterLeaseLossIsSkipped(t *testing.T) {
	old := monitorHTTPClient
	monitorHTTPClient = &http.Client{Transport: monitorTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"ok"}}]}`))}, nil
	})}
	t.Cleanup(func() { monitorHTTPClient = old })
	opts := &CheckOptions{keyAdmission: monitorBufferedLeaseLossAdmission{}, requireKeyAdmission: true,
		BodyOverrideMode: MonitorBodyOverrideModeReplace,
		BodyOverride:     map[string]any{"messages": []any{map[string]any{"role": "user", "content": "test"}}}}
	result := runCheckForModel(t.Context(), MonitorProviderOpenAI, "https://fixture.invalid", "key", "model", opts)
	require.True(t, result.Skipped, "a completely buffered response after lease loss is not a valid observation")
	require.Nil(t, result.LatencyMs)
}
