package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
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

// The async admin caller uses Background; storage must therefore inherit a
// service-owned deadline, not only the HTTP request's nested timeout.
type responsesProbeLifecycleRepo struct {
	AccountRepository
	load func(context.Context, int64) (*Account, error)
	save func(context.Context, int64, map[string]any) error
}

func (r responsesProbeLifecycleRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	if r.load != nil {
		return r.load(ctx, id)
	}
	return r.AccountRepository.GetByID(ctx, id)
}
func (r responsesProbeLifecycleRepo) UpdateExtra(ctx context.Context, id int64, extra map[string]any) error {
	if r.save != nil {
		return r.save(ctx, id, extra)
	}
	return r.AccountRepository.UpdateExtra(ctx, id, extra)
}

func TestProbeOpenAIAPIKeyResponsesLifecycleStorageIsBounded(t *testing.T) {
	for _, tc := range []struct {
		name          string
		callerTimeout time.Duration
		wantDuration  time.Duration
	}{
		{name: "background", wantDuration: 30 * time.Second},
		{name: "earlier_caller", callerTimeout: 7 * time.Second, wantDuration: 7 * time.Second},
		{name: "later_caller", callerTimeout: time.Minute, wantDuration: 30 * time.Second},
	} {
		for _, phase := range []string{"load", "save", "native_save"} {
			t.Run(tc.name+"/"+phase, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					account := newResponsesProbeAccount(4600)
					if phase == "native_save" {
						account.Platform = PlatformDeepseek
						account.Credentials["api_protocol"] = APIProtocolAdaptive
					}
					base := &snapshotUpdateAccountRepo{stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{account}}, updateExtraCalls: make(chan map[string]any, 1)}
					repo := responsesProbeLifecycleRepo{AccountRepository: base}
					deadlineObserved := make(chan bool, 1)
					wait := func(ctx context.Context) error {
						_, ok := ctx.Deadline()
						deadlineObserved <- ok
						if !ok {
							return errors.New("fixture: no lifecycle deadline")
						}
						<-ctx.Done()
						return ctx.Err()
					}
					if phase == "load" {
						repo.load = func(ctx context.Context, _ int64) (*Account, error) { return nil, wait(ctx) }
					} else {
						repo.save = func(ctx context.Context, _ int64, _ map[string]any) error { return wait(ctx) }
					}
					upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"status":"completed","output":[{"type":"function_call"}]}`))}}
					svc := NewAccountTestService(repo, nil, nil, nil, nil, upstream, &config.Config{}, nil)
					ctx := context.Background()
					if tc.callerTimeout > 0 {
						var cancel context.CancelFunc
						ctx, cancel = context.WithTimeout(ctx, tc.callerTimeout)
						defer cancel()
					}
					started := time.Now()
					require.Nil(t, svc.ProbeOpenAIAPIKeyResponsesSupport(ctx, account.ID))
					require.True(t, <-deadlineObserved, "%s storage received an unbounded Background context", phase)
					require.Equal(t, tc.wantDuration, time.Since(started), "storage uses the whole lifecycle allowance or the earlier caller deadline, not the HTTP-only timeout")
					require.Empty(t, base.updateExtraCalls)
					if phase != "save" {
						require.Nil(t, upstream.lastReq)
					}
				})
			})
		}
	}
}

func TestProbeOpenAIAPIKeyResponsesCancellationDuringLoadDoesNotPublish(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformDeepseek} {
		t.Run(platform, func(t *testing.T) {
			account := newResponsesProbeAccount(4603)
			account.Platform = platform
			account.Credentials["api_protocol"] = APIProtocolAdaptive
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			saves := 0
			repo := responsesProbeLifecycleRepo{
				load: func(context.Context, int64) (*Account, error) {
					cancel()
					return &account, nil // A late database success must not start work.
				},
				save: func(context.Context, int64, map[string]any) error { saves++; return nil },
			}
			upstream := &httpUpstreamRecorder{}
			svc := NewAccountTestService(repo, nil, nil, nil, nil, upstream, &config.Config{}, nil)
			require.Nil(t, svc.ProbeOpenAIAPIKeyResponsesSupport(ctx, account.ID))
			require.Nil(t, upstream.lastReq)
			require.Zero(t, saves, "neither an HTTP result nor fixed native protocol state may publish after cancellation")
		})
	}
}

func TestProbeOpenAIAPIKeyResponsesHTTPTimeoutDoesNotPublishLateSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		account := newResponsesProbeAccount(4604)
		updates := make(chan map[string]any, 1)
		repo := &snapshotUpdateAccountRepo{stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{account}}, updateExtraCalls: updates}
		upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: responsesProbeClosingBody{
			Reader: strings.NewReader(`{"status":"completed","output":[{"type":"function_call"}],"usage":{"total_tokens":7}}`),
			close: func() error {
				time.Sleep(16 * time.Second)
				return nil
			},
		}}}
		budget := &monitoringBudgetRepoStub{}
		svc := NewAccountTestService(repo, nil, nil, nil, nil, monitoringBudgetHTTP{base: upstream}, &config.Config{}, nil)
		svc.SetMonitoringBudget(NewMonitoringProbeBudgetService(budget))
		require.Nil(t, svc.ProbeOpenAIAPIKeyResponsesSupport(context.Background(), account.ID))
		require.Empty(t, updates, "a completed body is not publishable after the original 15s HTTP deadline")
		require.Equal(t, 1, budget.released)
		require.Equal(t, []MonitoringProbeTokenUsage{{Tokens: 7, Reported: true}}, budget.usages)
	})
}

func TestProbeOpenAIAPIKeyResponsesCancelledBeforeLoadDoesNoWork(t *testing.T) {
	account := newResponsesProbeAccount(4601)
	loads := 0
	repo := responsesProbeLifecycleRepo{load: func(context.Context, int64) (*Account, error) { loads++; return &account, nil }}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	svc := NewAccountTestService(repo, nil, nil, nil, nil, &httpUpstreamRecorder{}, &config.Config{}, nil)
	svc.ProbeOpenAIAPIKeyResponsesSupport(ctx, account.ID)
	require.Zero(t, loads, "an already-cancelled automatic event must not start loading or writing")
}

func TestProbeOpenAIAPIKeyResponsesCancellationOnCloseDoesNotPublish(t *testing.T) {
	account := newResponsesProbeAccount(4602)
	updates := make(chan map[string]any, 1)
	repo := &snapshotUpdateAccountRepo{stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{account}}, updateExtraCalls: updates}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	body := `{"status":"completed","output":[{"type":"function_call"}],"usage":{"input_tokens":4,"output_tokens":3,"total_tokens":7}}`
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: make(http.Header), Body: responsesProbeClosingBody{Reader: strings.NewReader(body), close: func() error { cancel(); return nil }}}}
	budget := &monitoringBudgetRepoStub{}
	svc := NewAccountTestService(repo, nil, nil, nil, nil, monitoringBudgetHTTP{base: upstream}, &config.Config{}, nil)
	svc.SetMonitoringBudget(NewMonitoringProbeBudgetService(budget))
	require.Nil(t, svc.ProbeOpenAIAPIKeyResponsesSupport(ctx, account.ID), "caller cancellation is not a fabricated supplier or budget failure")
	require.Empty(t, updates, "a late success after cancellation must not publish routing capability")
	require.Equal(t, 1, budget.released)
	require.Equal(t, []MonitoringProbeTokenUsage{{Tokens: 7, Reported: true}}, budget.usages, "the admitted call still settles actual returned usage")
}

func TestResponsesCapabilityRuntimeConcurrentEventsAndCancellation(t *testing.T) {
	const events = 16
	entered := make(chan struct{}, events)
	repo := responsesProbeLifecycleRepo{load: func(ctx context.Context, _ int64) (*Account, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	svc := NewAccountTestService(repo, nil, nil, nil, nil, &httpUpstreamRecorder{}, &config.Config{}, nil)
	initial := svc.ResponsesCapabilityRuntime()
	require.Equal(t, "idle", initial.State)
	require.NotEmpty(t, initial.InstanceID)
	require.Nil(t, initial.LastStartedAt)
	require.Nil(t, initial.LastFinishedAt)
	require.Zero(t, initial.StartedEvents)
	require.Equal(t, int64(30), initial.LifecycleTimeoutSeconds)
	require.Equal(t, int64(15), initial.HTTPTimeoutSeconds)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var wg sync.WaitGroup
	for range events {
		wg.Go(func() { svc.ProbeOpenAIAPIKeyResponsesSupport(ctx, 4700) })
	}
	for range events {
		<-entered
	}
	active := svc.ResponsesCapabilityRuntime()
	require.Equal(t, "active", active.State)
	require.EqualValues(t, events, active.StartedEvents)
	require.EqualValues(t, events, active.ActiveEvents)
	require.Zero(t, active.FinishedEvents)
	require.NotNil(t, active.LastStartedAt)
	require.Nil(t, active.LastFinishedAt)
	started := *active.LastStartedAt
	*active.LastStartedAt = time.Time{}
	require.Equal(t, started, *svc.ResponsesCapabilityRuntime().LastStartedAt, "a caller must not rewrite stored event time")
	readerDone := make(chan struct{})
	wg.Go(func() {
		defer close(readerDone)
		for range 1000 {
			view := svc.ResponsesCapabilityRuntime()
			if view.StartedEvents != view.FinishedEvents+view.ActiveEvents {
				t.Error("runtime counters must be one locked snapshot")
			}
		}
	})
	cancel()
	wg.Wait()
	<-readerDone
	after := svc.ResponsesCapabilityRuntime()
	require.Equal(t, "idle", after.State)
	require.Equal(t, initial.InstanceID, after.InstanceID)
	require.EqualValues(t, events, after.StartedEvents)
	require.EqualValues(t, events, after.FinishedEvents, "finished counts exited events, not successful upstream probes")
	require.Zero(t, after.ActiveEvents)
	require.NotNil(t, after.LastFinishedAt)
	finished := *after.LastFinishedAt
	*after.LastFinishedAt = time.Time{}
	require.Equal(t, finished, *svc.ResponsesCapabilityRuntime().LastFinishedAt)
	svc.ProbeOpenAIAPIKeyResponsesSupport(ctx, 4700)
	require.EqualValues(t, events, svc.ResponsesCapabilityRuntime().StartedEvents, "already-cancelled calls do not start work")
	rebuilt := NewAccountTestService(repo, nil, nil, nil, nil, &httpUpstreamRecorder{}, &config.Config{}, nil).ResponsesCapabilityRuntime()
	require.NotEqual(t, initial.InstanceID, rebuilt.InstanceID)
	require.Zero(t, rebuilt.StartedEvents)
	require.Nil(t, rebuilt.LastStartedAt)
}

func TestResponsesCapabilityRuntimeOriginalBudgetProjectionAndProvider(t *testing.T) {
	policy := DefaultMonitoringProbeBudgetSettings()
	coverage := CompileMonitoringProbeCoverage(policy, UpstreamBillingProbeSettings{IntervalMinutes: 30}, nil, true, nil, true, nil, true, nil, true, time.Now())
	repo := &monitoringRuntimeBudgetRepo{coverage: coverage}
	tests := NewAccountTestService(responsesProbeLifecycleRepo{}, nil, nil, nil, nil, &httpUpstreamRecorder{}, &config.Config{}, nil)
	probe := ProvideUpstreamBillingProbeService(nil, tests, nil, nil, nil,
		&config.Config{Runtime: config.RuntimeConfig{Role: config.RuntimeRoleGateway}}, nil, nil, repo, nil, nil, nil, nil)
	t.Cleanup(probe.Stop)
	preview, err := probe.MonitoringBudget().PreviewCoverage(t.Context(), policy)
	require.NoError(t, err)
	summary, err := probe.MonitoringBudget().Summary(t.Context())
	require.NoError(t, err)
	require.Equal(t, tests.ResponsesCapabilityRuntime().InstanceID, preview.ResponsesCapability.InstanceID)
	require.Equal(t, preview.ResponsesCapability.InstanceID, summary.Coverage.ResponsesCapability.InstanceID)
	require.Equal(t, int64(15), preview.ResponsesCapability.HTTPTimeoutSeconds)
	require.Nil(t, repo.coverage.ResponsesCapability, "do not mutate the repository's reused result")
	require.Zero(t, preview.MinimumTotalRequestsPerDay, "event counters are not forecasts of physical calls or tokens")
	require.Contains(t, preview.UnestimatedDemandSources, "responses_capability")
	unknown, err := NewMonitoringProbeBudgetService(repo).PreviewCoverage(t.Context(), policy)
	require.NoError(t, err)
	require.Equal(t, "unavailable", unknown.ResponsesCapability.State)
	repo.err = ErrMonitoringProbeBudgetConflict
	_, err = probe.MonitoringBudget().PreviewCoverage(t.Context(), policy)
	require.ErrorIs(t, err, ErrMonitoringProbeBudgetConflict)
}
