package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/stretchr/testify/require"
)

func TestProbeOpenAIAPIKeyResponsesMonitoringBudgetUsage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		usage string
		want  MonitoringProbeTokenUsage
	}{
		{name: "actual_including_cache", usage: `,"usage":{"input_tokens":4,"input_tokens_details":{"cached_tokens":3},"output_tokens":3,"total_tokens":7}`, want: MonitoringProbeTokenUsage{Tokens: 7, Reported: true}},
		{name: "explicit_zero", usage: `,"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}`, want: MonitoringProbeTokenUsage{Reported: true}},
		{name: "missing"},
		{name: "partial", usage: `,"usage":{"input_tokens":4}`, want: MonitoringProbeTokenUsage{Tokens: 4}},
		{name: "invalid", usage: `,"usage":{"input_tokens":-1,"output_tokens":3,"total_tokens":2}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := newResponsesProbeAccount(4400)
			account.Credentials["base_url"] = "https://compat-upstream.example/tenant/v1"
			updates := make(chan map[string]any, 1)
			accounts := &snapshotUpdateAccountRepo{stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{account}}, updateExtraCalls: updates}
			budget := &monitoringBudgetRepoStub{}
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{"status":"completed","output":[{"type":"function_call","name":"probe_ping"}]` + tc.usage + `}`))}}
			svc := NewAccountTestService(accounts, nil, nil, nil, nil, monitoringBudgetHTTP{base: upstream}, &config.Config{}, nil)
			svc.SetMonitoringBudget(NewMonitoringProbeBudgetService(budget))

			require.Nil(t, svc.ProbeOpenAIAPIKeyResponsesSupport(t.Context(), account.ID))

			require.Len(t, budget.requests, 1)
			require.Equal(t, monitoringProbeHash("https://compat-upstream.example/tenant"), budget.requests[0].SiteHash)
			require.Equal(t, []string{config.UpstreamKeyFingerprint("sk-test")}, budget.requests[0].KeyHashes)
			require.True(t, budget.requests[0].MeterTokens)
			require.False(t, budget.requests[0].Recovery)
			require.Equal(t, 1, budget.released)
			require.Equal(t, []MonitoringProbeTokenUsage{tc.want}, budget.usages)
			require.Equal(t, true, (<-updates)[openai_compat.ExtraKeyResponsesSupported])
			require.True(t, HTTPUpstreamRedirectsDisabled(upstream.lastReq.Context()))
		})
	}
}

func TestProbeOpenAIAPIKeyResponsesMonitoringBudgetDefersWithoutVerdict(t *testing.T) {
	for _, cause := range []error{
		&MonitoringProbeBudgetDenial{Reason: "request_limit", Scope: "key_minute", RetryAt: time.Now().Add(time.Minute)},
		&MonitoringProbeBudgetDenial{Reason: "token_limit", Scope: "global_day", RetryAt: time.Now().Add(time.Hour)},
		errors.New("fixture database failure"),
	} {
		t.Run(cause.Error(), func(t *testing.T) {
			account := newResponsesProbeAccount(4401)
			account.Extra = map[string]any{openai_compat.ExtraKeyResponsesSupported: false, "unrelated": "preserved"}
			updates := make(chan map[string]any, 1)
			accounts := &snapshotUpdateAccountRepo{stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{account}}, updateExtraCalls: updates}
			budget := &monitoringBudgetRepoStub{denyAt: 1, err: cause}
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(`{"output":[{"type":"function_call"}]}`))}}
			svc := NewAccountTestService(accounts, nil, nil, nil, nil, monitoringBudgetHTTP{base: upstream}, &config.Config{}, nil)
			svc.SetMonitoringBudget(NewMonitoringProbeBudgetService(budget))

			deferral := svc.ProbeOpenAIAPIKeyResponsesSupport(t.Context(), account.ID)

			require.NotNil(t, deferral)
			wantReason := "budget_unavailable"
			var denial *MonitoringProbeBudgetDenial
			if errors.As(cause, &denial) {
				wantReason = denial.Reason
			}
			require.Equal(t, wantReason, deferral.Reason)
			require.Len(t, budget.requests, 1)
			require.Nil(t, upstream.lastReq, "denied probes must never leave the physical boundary")
			require.Empty(t, updates, "a local deferral must not replace the saved routing capability")
			require.Zero(t, budget.released, "no lease was acquired")
		})
	}
}

func TestProbeOpenAIAPIKeyResponsesMonitoringBudgetSettlesBeforeVerdict(t *testing.T) {
	account := newResponsesProbeAccount(4402)
	updates := make(chan map[string]any, 1)
	accounts := &snapshotUpdateAccountRepo{stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{account}}, updateExtraCalls: updates}
	budget := &monitoringBudgetRepoStub{releaseErr: errors.New("fixture settlement failure")}
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"status":"completed","output":[{"type":"function_call"}],"usage":{"input_tokens":4,"output_tokens":3,"total_tokens":7}}`))}}
	svc := NewAccountTestService(accounts, nil, nil, nil, nil, monitoringBudgetHTTP{base: upstream}, &config.Config{}, nil)
	svc.SetMonitoringBudget(NewMonitoringProbeBudgetService(budget))

	deferral := svc.ProbeOpenAIAPIKeyResponsesSupport(t.Context(), account.ID)

	require.NotNil(t, deferral)
	require.Equal(t, "budget_unavailable", deferral.Reason)
	require.Empty(t, updates, "successful-looking output cannot be published before its budget settlement")
	require.Equal(t, 1, budget.released)
	require.Equal(t, []MonitoringProbeTokenUsage{{Tokens: 7, Reported: true}}, budget.usages)
}

type responsesProbeClosingBody struct {
	io.Reader
	close func() error
}

func (b responsesProbeClosingBody) Close() error { return b.close() }

func TestProbeOpenAIAPIKeyResponsesMonitoringBudgetReadAndCloseFailures(t *testing.T) {
	for _, mode := range []string{"transport", "read", "close", "local_lease_on_close"} {
		t.Run(mode, func(t *testing.T) {
			account := newResponsesProbeAccount(4403)
			updates := make(chan map[string]any, 1)
			accounts := &snapshotUpdateAccountRepo{stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{account}}, updateExtraCalls: updates}
			budget := &monitoringBudgetRepoStub{}
			upstream := &httpUpstreamRecorder{}
			body := `{"status":"completed","output":[{"type":"function_call"}],"usage":{"input_tokens":4,"output_tokens":3,"total_tokens":7}}`
			upstream.resp = &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
			wantUsage := MonitoringProbeTokenUsage{Tokens: 7, Reported: true}
			switch mode {
			case "transport":
				upstream.resp, upstream.err = nil, io.ErrUnexpectedEOF
				wantUsage = MonitoringProbeTokenUsage{}
			case "read":
				upstream.resp.Body = passthroughErrReadCloser{err: io.ErrUnexpectedEOF}
				wantUsage = MonitoringProbeTokenUsage{}
			case "close":
				upstream.resp.Body = responsesProbeClosingBody{Reader: strings.NewReader(body), close: func() error { return io.ErrClosedPipe }}
			case "local_lease_on_close":
				upstream.resp.Body = responsesProbeClosingBody{Reader: strings.NewReader(body), close: func() error {
					recordMonitoringProbeLocalError(upstream.lastReq.Context(), ErrMonitoringProbeBudgetLeaseLost)
					return nil
				}}
			}
			svc := NewAccountTestService(accounts, nil, nil, nil, nil, monitoringBudgetHTTP{base: upstream}, &config.Config{}, nil)
			svc.SetMonitoringBudget(NewMonitoringProbeBudgetService(budget))

			deferral := svc.ProbeOpenAIAPIKeyResponsesSupport(t.Context(), account.ID)

			if mode == "local_lease_on_close" {
				require.NotNil(t, deferral)
				require.Equal(t, "budget_unavailable", deferral.Reason)
			} else {
				require.Nil(t, deferral, "a supplier/network error is not a typed local deferral")
			}
			require.Empty(t, updates, "neither incomplete transport nor late local failure can change a capability")
			require.Equal(t, 1, budget.released)
			require.Equal(t, []MonitoringProbeTokenUsage{wantUsage}, budget.usages)
		})
	}
}

func TestProbeOpenAIAPIKeyResponsesMonitoringBudgetDoesNotClassifySupplierText(t *testing.T) {
	account := newResponsesProbeAccount(4404)
	updates := make(chan map[string]any, 1)
	accounts := &snapshotUpdateAccountRepo{stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{account}}, updateExtraCalls: updates}
	budget := &monitoringBudgetRepoStub{}
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(`{"error":{"message":"monitoring probe deferred: token_limit"}}`))}}
	svc := NewAccountTestService(accounts, nil, nil, nil, nil, monitoringBudgetHTTP{base: upstream}, &config.Config{}, nil)
	svc.SetMonitoringBudget(NewMonitoringProbeBudgetService(budget))

	require.Nil(t, svc.ProbeOpenAIAPIKeyResponsesSupport(t.Context(), account.ID))

	require.Len(t, budget.requests, 1)
	require.Equal(t, 1, budget.released)
	require.Equal(t, true, (<-updates)[openai_compat.ExtraKeyResponsesSupported], "retain the existing non-2xx capability policy, not a fabricated local budget error")
}

func TestProbeOpenAIAPIKeyResponsesSupportUsesCodexProbeHeaders(t *testing.T) {
	updateCalls := make(chan map[string]any, 1)
	account := Account{
		ID:          96,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://compat-upstream.example/v1",
		},
	}
	repo := &snapshotUpdateAccountRepo{
		stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{account}},
		updateExtraCalls:      updateCalls,
	}
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"output":[{"type":"function_call","name":"probe_ping"}]}`)),
	}}
	svc := &AccountTestService{
		accountRepo:  repo,
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}

	svc.ProbeOpenAIAPIKeyResponsesSupport(context.Background(), account.ID)

	require.NotNil(t, upstream.lastReq)
	require.Equal(t, "https://compat-upstream.example/v1/responses", upstream.lastReq.URL.String())
	requireOpenAICodexProbeHeaders(t, upstream.lastReq.Header)
	updates := <-updateCalls
	require.Equal(t, true, updates[openai_compat.ExtraKeyResponsesSupported])
}

func TestProbeOpenAIAPIKeyResponsesSupportCNProviders(t *testing.T) {
	tests := []struct {
		name        string
		id          int64
		platform    string
		protocol    string
		wantSupport bool
		wantMode    string
	}{
		{name: "deepseek adaptive supports responses", id: 201, platform: PlatformDeepseek, protocol: APIProtocolAdaptive, wantSupport: true, wantMode: string(openai_compat.ResponsesSupportModeForceResponses)},
		{name: "deepseek chat clears forced responses", id: 202, platform: PlatformDeepseek, protocol: APIProtocolChatCompletions, wantSupport: false, wantMode: string(openai_compat.ResponsesSupportModeAuto)},
		{name: "kimi adaptive supports responses", id: 203, platform: PlatformKimi, protocol: APIProtocolAdaptive, wantSupport: true, wantMode: string(openai_compat.ResponsesSupportModeForceResponses)},
		{name: "kimi responses protocol supports responses", id: 205, platform: PlatformKimi, protocol: APIProtocolResponses, wantSupport: true, wantMode: string(openai_compat.ResponsesSupportModeForceResponses)},
		{name: "zhipu adaptive falls back to chat", id: 204, platform: PlatformZhipu, protocol: APIProtocolAdaptive, wantSupport: false, wantMode: string(openai_compat.ResponsesSupportModeAuto)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			updateCalls := make(chan map[string]any, 1)
			account := Account{
				ID: tc.id, Platform: tc.platform, Type: AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "sk-test", "api_protocol": tc.protocol},
				Extra: map[string]any{
					openai_compat.ExtraKeyResponsesMode: string(openai_compat.ResponsesSupportModeForceResponses),
				},
			}
			repo := &snapshotUpdateAccountRepo{
				stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{account}},
				updateExtraCalls:      updateCalls,
			}
			svc := &AccountTestService{accountRepo: repo}

			svc.ProbeOpenAIAPIKeyResponsesSupport(context.Background(), account.ID)

			updates := <-updateCalls
			require.Equal(t, tc.wantSupport, updates[openai_compat.ExtraKeyResponsesSupported])
			require.Equal(t, tc.wantMode, updates[openai_compat.ExtraKeyResponsesMode])
		})
	}
}

func TestProbeOpenAIAPIKeyResponsesSupportSkipsForcedModes(t *testing.T) {
	cases := []struct {
		name      string
		mode      any
		wantProbe bool
	}{
		{name: "force_responses skips probe", mode: string(openai_compat.ResponsesSupportModeForceResponses), wantProbe: false},
		{name: "force_chat_completions skips probe", mode: string(openai_compat.ResponsesSupportModeForceChatCompletions), wantProbe: false},
		{name: "auto probes", mode: string(openai_compat.ResponsesSupportModeAuto), wantProbe: true},
		{name: "invalid mode falls back to auto and probes", mode: "bogus", wantProbe: true},
		{name: "non-string mode falls back to auto and probes", mode: nil, wantProbe: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			updateCalls := make(chan map[string]any, 1)
			account := Account{
				ID:          301,
				Platform:    PlatformOpenAI,
				Type:        AccountTypeAPIKey,
				Concurrency: 1,
				Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://compat-upstream.example/v1"},
				Extra:       map[string]any{openai_compat.ExtraKeyResponsesMode: tc.mode},
			}
			repo := &snapshotUpdateAccountRepo{
				stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{account}},
				updateExtraCalls:      updateCalls,
			}
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"output":[{"type":"function_call","name":"probe_ping"}]}`)),
			}}
			svc := &AccountTestService{
				accountRepo:  repo,
				httpUpstream: upstream,
				cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
			}

			svc.ProbeOpenAIAPIKeyResponsesSupport(context.Background(), account.ID)

			if !tc.wantProbe {
				require.Nil(t, upstream.lastReq)
				require.Empty(t, updateCalls)
				return
			}
			require.NotNil(t, upstream.lastReq)
			require.Equal(t, true, (<-updateCalls)[openai_compat.ExtraKeyResponsesSupported])
		})
	}
}

func TestDecideResponsesProbeSupport(t *testing.T) {
	fnCall := []byte(`{"output":[{"type":"reasoning"},{"type":"function_call","name":"probe_ping"}]}`)
	reasoningOnly := []byte(`{"output":[{"type":"reasoning"}]}`)

	cases := []struct {
		name   string
		status int
		body   []byte
		want   bool
	}{
		// Endpoint clearly absent on third-party OpenAI-compatible upstreams.
		{"404 endpoint absent", 404, fnCall, false},
		{"405 method not allowed", 405, fnCall, false},
		// 2xx: tool capability is judged by presence of a function_call output item.
		{"200 with function_call", 200, fnCall, true},
		// Volcengine Ark coding/v3 × kimi-k2.6: reasoning only, no function_call.
		{"200 reasoning only", 200, reasoningOnly, false},
		{"200 invalid json", 200, []byte("not-json"), false},
		{"200 no output field", 200, []byte(`{"status":"completed"}`), false},
		// Non-2xx (other than 404/405): endpoint exists, capability undecidable -> conservative true.
		{"400 conservative true", 400, reasoningOnly, true},
		{"401 conservative true", 401, nil, true},
		{"500 conservative true", 500, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, decideResponsesProbeSupport(tc.status, tc.body))
		})
	}
}

func TestResponsesProbeBodyHasFunctionCall(t *testing.T) {
	require.True(t, responsesProbeBodyHasFunctionCall([]byte(`{"output":[{"type":"function_call"}]}`)))
	require.True(t, responsesProbeBodyHasFunctionCall([]byte(`{"output":[{"type":"reasoning"},{"type":"function_call"}]}`)))
	require.False(t, responsesProbeBodyHasFunctionCall([]byte(`{"output":[{"type":"reasoning"}]}`)))
	require.False(t, responsesProbeBodyHasFunctionCall([]byte(`{"output":[]}`)))
	require.False(t, responsesProbeBodyHasFunctionCall([]byte(`{}`)))
	require.False(t, responsesProbeBodyHasFunctionCall([]byte(`garbage`)))
}

func TestSelectResponsesProbeModel(t *testing.T) {
	// No model_mapping -> fall back to DefaultTestModel (OpenAI official APIKey).
	require.Equal(t, openai.DefaultTestModel, selectResponsesProbeModel(&Account{}))

	// model_mapping values are upstream models; pick first by sort for reproducibility.
	acct := &Account{Credentials: map[string]any{
		"model_mapping": map[string]any{
			"client-b": "zeta-model",
			"client-a": "alpha-model",
		},
	}}
	require.Equal(t, "alpha-model", selectResponsesProbeModel(acct))

	// Wildcard / blank upstream values are skipped.
	acctWild := &Account{Credentials: map[string]any{
		"model_mapping": map[string]any{
			"a": "*",
			"b": "  ",
			"c": "real-model",
		},
	}}
	require.Equal(t, "real-model", selectResponsesProbeModel(acctWild))

	// Only wildcard mappings -> DefaultTestModel.
	acctAllWild := &Account{Credentials: map[string]any{
		"model_mapping": map[string]any{"a": "gpt-*"},
	}}
	require.Equal(t, openai.DefaultTestModel, selectResponsesProbeModel(acctAllWild))
}
