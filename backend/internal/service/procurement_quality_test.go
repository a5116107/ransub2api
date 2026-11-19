package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

type procurementQualityTestReader func(context.Context, *Account, AvailabilityRequestScope) (*QualityScopeEvidence, error)

func (f procurementQualityTestReader) ReadQualityScopeEvidence(ctx context.Context, account *Account, scope AvailabilityRequestScope) (*QualityScopeEvidence, error) {
	return f(ctx, account, scope)
}

func procurementAnswerPolicy() *ProcurementPolicy {
	return &ProcurementPolicy{Version: 4, Revision: 1, Sources: &domain.ProcurementSources{}, Buckets: &domain.ProcurementBuckets{}, Quality: "current_answer"}
}

func TestProcurementQualityRequiresCurrentExactEvidence(t *testing.T) {
	for _, scenario := range []string{"pass", "incorrect", "expired", "unknown", "not-configured", "missing", "read-error", "coverage", "future-version", "missing-deadline", "inconsistent-read", "clock-offset", "off"} {
		t.Run(scenario, func(t *testing.T) {
			account, scope, rule, at := qualityScopeFixture()
			evidence := AssessQualityScopeEvidence(account, scope, []QualityEvidenceRule{rule}, at, false)
			gate := &openAIProfitControlGate{procurementPolicy: procurementAnswerPolicy()}
			want := "procurement_quality_unavailable"
			switch scenario {
			case "pass", "clock-offset", "off":
				want = ""
			case "incorrect":
				evidence.Status = "fail"
				want = "procurement_quality_failed"
			case "expired":
				expired := at.Add(-time.Second)
				evidence.ValidUntil = &expired
			case "unknown", "not-configured":
				evidence.Status = scenario
			case "missing":
				evidence = nil
			case "coverage":
				evidence.ValidSamples = 0
			case "future-version":
				evidence.Version++
			case "missing-deadline":
				evidence.ValidUntil = nil
			case "inconsistent-read":
				evidence.EvaluatedAt = time.Time{}
			}
			if scenario == "clock-offset" {
				evidence.EvaluatedAt = at.Add(time.Hour)
				until := evidence.EvaluatedAt.Add(time.Minute)
				evidence.ValidUntil = &until
			}
			calls := 0
			gate.qualityReader = procurementQualityTestReader(func(ctx context.Context, a *Account, s AvailabilityRequestScope) (*QualityScopeEvidence, error) {
				calls++
				require.Equal(t, account.ID, a.ID)
				require.Equal(t, scope, s)
				_, bounded := ctx.Deadline()
				require.True(t, bounded)
				if scenario == "read-error" {
					return nil, errors.New("fixture read error")
				}
				return evidence, nil
			})
			if scenario == "off" {
				gate.procurementPolicy.Quality = "not_required"
			}
			require.Equal(t, want, procurementQualityScopeVetoReason(t.Context(), gate, account, &scope))
			require.Equal(t, scenario != "off", calls > 0)
		})
	}
}

func TestProcurementQualityCandidateCacheNeverAuthorizesActualSend(t *testing.T) {
	account, scope, rule, at := qualityScopeFixture()
	evidence := AssessQualityScopeEvidence(account, scope, []QualityEvidenceRule{rule}, at, false)
	gate := &openAIProfitControlGate{procurementPolicy: procurementAnswerPolicy(), qualityCache: &sync.Map{}}
	calls := 0
	gate.qualityReader = procurementQualityTestReader(func(context.Context, *Account, AvailabilityRequestScope) (*QualityScopeEvidence, error) {
		calls++
		if calls == 1 {
			return evidence, nil
		}
		copy := *evidence
		copy.Status = "unknown"
		copy.Reason = "rule_unconfirmed"
		return &copy, nil
	})
	require.Empty(t, procurementQualityScopeVetoReason(t.Context(), gate, account, &scope))
	require.Empty(t, procurementQualityScopeVetoReason(t.Context(), gate, account, &scope))
	require.Equal(t, 1, calls)
	ctx := context.WithValue(t.Context(), openAIProfitControlGateCtxKey{}, gate)
	ctx = context.WithValue(ctx, procurementQualityActualScopeKey{}, func() *AvailabilityRequestScope { return &scope })
	require.Equal(t, "procurement_quality_unavailable", procurementQualityCandidateVetoReason(ctx, account))
	require.Equal(t, 2, calls, "actual generation must load the latest registered fault/rule")
	ctx = context.WithValue(ctx, procurementQualityActualScopeKey{}, func() *AvailabilityRequestScope { return nil })
	require.Equal(t, "procurement_quality_unavailable", procurementQualityCandidateVetoReason(ctx, account), "unknown wire shape must not borrow a candidate scope")
}

func TestProcurementQualityActualHTTPAndWSTurnScope(t *testing.T) {
	account := billingCandidateAccount(t, 901, billingApplicabilityEntry("responses_http", "priority", true))
	group := profitControlTestGroup(7, 0, 0)
	group.ProcurementPolicy = procurementAnswerPolicy()
	gate := &openAIProfitControlGate{groupID: group.ID, platform: group.Platform, threshold: 1, procurementPolicy: group.ProcurementPolicy.Clone(), procurementGroup: func(context.Context) (*Group, error) { return group, nil }}
	status := "pass"
	var scopes []AvailabilityRequestScope
	gate.qualityReader = procurementQualityTestReader(func(ctx context.Context, a *Account, scope AvailabilityRequestScope) (*QualityScopeEvidence, error) {
		scopes = append(scopes, scope)
		at := time.Now()
		until := at.Add(time.Minute)
		return &QualityScopeEvidence{Version: 1, Status: status, Reason: "all_rules_passed", EvaluatedAt: at, ValidUntil: &until, RuleCount: 1, ConfirmedRules: 1, Samples: 1, ValidSamples: 1}, nil
	})
	ctx := context.WithValue(t.Context(), openAIProfitControlGateCtxKey{}, gate)
	body := `{"model":"gpt-fixture","input":"fixture","stream":true,"service_tier":"priority"}`
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, account.GetOpenAIBaseURL()+"/v1/responses", strings.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+account.GetCredential("api_key"))
	physical := withProcurementQualityHTTPRequest(request, account).Context()
	blocked, reason := openAIProfitControlLatestPolicyVetoReason(physical, account)
	require.False(t, blocked, reason)
	require.Equal(t, "responses_http", scopes[0].Protocol)
	status = "unknown"
	blocked, reason = openAIProfitControlLatestPolicyVetoReason(physical, account)
	require.True(t, blocked)
	require.Equal(t, "procurement_quality_unavailable", reason)
	status = "pass"
	blocked, reason = openAIProfitControlLatestPolicyVetoReason(physical, account)
	require.False(t, blocked, reason)
	// A real WS turn is parsed from the established handshake, not its HTTP ingress.
	account.Extra = map[string]any{}
	target := strings.Replace(account.GetOpenAIBaseURL(), "https://", "wss://", 1) + "/v1/responses"
	binding := (&OpenAIGatewayService{}).bindOpenAIWSHandshake(account, "gpt-fixture", http.Header{"Authorization": []string{"Bearer " + account.GetCredential("api_key")}}, target)
	require.NotNil(t, binding.availability.route, "the physical handshake must belong to the selected account")
	ws := binding.availability.scope(account, []byte(`{"type":"response.create","model":"gpt-fixture","input":"fixture","stream":true}`))
	require.NotNil(t, ws)
	wsCtx := context.WithValue(ctx, procurementQualityActualScopeKey{}, func() *AvailabilityRequestScope { return ws })
	require.NoError(t, (&OpenAIGatewayService{}).checkOpenAIWSBinding(wsCtx, account, "gpt-fixture", binding, []byte(`{"type":"response.create","model":"gpt-fixture","input":"fixture","stream":true}`)))
	require.Equal(t, "responses_ws_v2", scopes[len(scopes)-1].Protocol)
	status = "unknown"
	require.Error(t, (&OpenAIGatewayService{}).checkOpenAIWSBinding(wsCtx, account, "gpt-fixture", binding, []byte(`{"type":"response.create","model":"gpt-fixture","input":"fixture","stream":true}`)))
	group.ProcurementPolicy.Quality = "not_required"
	require.NoError(t, (&OpenAIGatewayService{}).checkOpenAIWSBinding(wsCtx, account, "gpt-fixture", binding, nil))
}

func TestProcurementQualityBothSchedulersKeepHealthyBinding(t *testing.T) {
	for _, engine := range []string{"true", "false"} {
		t.Run(engine, func(t *testing.T) {
			resetOpenAIAdvancedSchedulerSettingCacheForTest()
			defer resetOpenAIAdvancedSchedulerSettingCacheForTest()
			entry := billingApplicabilityEntry("responses_http", "priority", true)
			first, second := billingCandidateAccount(t, 901, entry), billingCandidateAccount(t, 902, entry)
			cache := &stubGatewayCache{sessionBindings: map[string]int64{"quality-sticky": first.ID}}
			concurrency := &upstreamCostTrackingConcurrencyCache{loadMap: map[int64]*AccountLoadInfo{first.ID: {AccountID: first.ID}, second.ID: {AccountID: second.ID}}}
			svc := &OpenAIGatewayService{cache: cache, accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{*first, *second}}, rateLimitService: newOpenAIAdvancedSchedulerRateLimitService(engine), concurrencyService: NewConcurrencyService(concurrency)}
			group := profitControlTestGroup(7, 0, 0)
			group.ProcurementPolicy = procurementAnswerPolicy()
			for _, state := range []string{"first-fails", "first-recovers"} {
				ctx := billingCandidateContext(t, svc, group, `{"model":"gpt-fixture","input":"fixture","service_tier":"priority"}`)
				gate := ctx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
				gate.qualityReader = procurementQualityTestReader(func(ctx context.Context, a *Account, scope AvailabilityRequestScope) (*QualityScopeEvidence, error) {
					at := time.Now()
					until := at.Add(time.Minute)
					status := "pass"
					if state == "first-fails" && a.ID == first.ID {
						status = "fail"
					}
					return &QualityScopeEvidence{Version: 1, Status: status, Reason: "all_rules_passed", EvaluatedAt: at, ValidUntil: &until, RuleCount: 1, ConfirmedRules: 1, Samples: 1, ValidSamples: 1}, nil
				})
				for _, session := range []string{"", "quality-sticky"} {
					selected, _, err := svc.SelectAccountWithScheduler(ctx, &group.ID, "", session, "gpt-fixture", nil, OpenAIUpstreamTransportAny, false)
					require.NoError(t, err)
					require.NotNil(t, selected)
					if state == "first-fails" {
						require.Equal(t, second.ID, selected.Account.ID)
					}
					if state == "first-recovers" {
						require.Contains(t, []int64{first.ID, second.ID}, selected.Account.ID)
					}
					if selected.ReleaseFunc != nil {
						selected.ReleaseFunc()
					}
				}
				require.Equal(t, first.ID, cache.sessionBindings["quality-sticky"])
				require.Zero(t, cache.deletedSessions["quality-sticky"])
			}
		})
	}
}

func TestProcurementQualityHTTPWireRejectsBeforeTransport(t *testing.T) {
	sends := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != `{"model":"gpt-fixture","input":"fixture","service_tier":"priority"}` {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		sends++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	account := billingCandidateAccount(t, 901, billingApplicabilityEntry("responses_http", "priority", true))
	account.Extra = map[string]any{}
	account.Credentials["base_url"] = server.URL
	svc := &OpenAIGatewayService{cfg: &config.Config{}, accountRepo: &turnAdmissionRepo{account: account}}
	group := profitControlTestGroup(7, 0, 0)
	group.ProcurementPolicy = procurementAnswerPolicy()
	gate := &openAIProfitControlGate{groupID: group.ID, platform: group.Platform, threshold: 1, procurementPolicy: group.ProcurementPolicy, procurementGroup: func(context.Context) (*Group, error) { return group, nil }}
	status := "unknown"
	gate.qualityReader = procurementQualityTestReader(func(context.Context, *Account, AvailabilityRequestScope) (*QualityScopeEvidence, error) {
		at := time.Now()
		until := at.Add(time.Minute)
		return &QualityScopeEvidence{Version: 1, Status: status, Reason: "all_rules_passed", EvaluatedAt: at, ValidUntil: &until, RuleCount: 1, ConfirmedRules: 1, Samples: 1, ValidSamples: 1}, nil
	})
	ctx := context.WithValue(t.Context(), openAIProfitControlGateCtxKey{}, gate)
	for _, next := range []string{"unknown", "pass", "unknown", "pass"} {
		status = next
		for _, engine := range []string{"openai", "native"} {
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-fixture","input":"fixture","service_tier":"priority"}`))
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer "+account.GetCredential("api_key"))
			var guarded *http.Request
			var guardErr error
			if engine == "native" {
				guarded, guardErr = guardGatewayUpstreamRequest(request, account, &gatewayFinalAdmissionRepo{latest: account}, svc.cfg)
			} else {
				guarded, guardErr = svc.guardOpenAIProfitRequest(request, account)
			}
			if guardErr != nil {
				var denied *OpenAITurnAdmissionError
				require.ErrorAs(t, guardErr, &denied)
				require.Equal(t, "procurement_quality_unavailable", denied.Reason)
				require.Equal(t, "unknown", next)
				continue
			}
			response, err := server.Client().Do(guarded)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, response.StatusCode)
			require.NoError(t, response.Body.Close())
		}
	}
	require.Equal(t, 4, sends)
}

type procurementQualityGroupAuthority struct {
	GroupRepository
	QualityScopeEvidenceReader
}

func TestProcurementQualityHTTPDefersBodyReadUntilRequired(t *testing.T) {
	account := billingCandidateAccount(t, 901, billingApplicabilityEntry("responses_http", "priority", true))
	account.Extra = map[string]any{}
	group := profitControlTestGroup(7, 0, 0)
	group.ProcurementPolicy = procurementAnswerPolicy()
	group.ProcurementPolicy.Quality = "not_required"
	gate := &openAIProfitControlGate{groupID: group.ID, platform: group.Platform, threshold: 1, procurementPolicy: group.ProcurementPolicy.Clone(), procurementGroup: func(context.Context) (*Group, error) { return group, nil }}
	gate.qualityReader = procurementQualityTestReader(func(context.Context, *Account, AvailabilityRequestScope) (*QualityScopeEvidence, error) {
		at := time.Now()
		until := at.Add(time.Minute)
		return &QualityScopeEvidence{Version: 1, Status: "pass", Reason: "all_rules_passed", EvaluatedAt: at, ValidUntil: &until, RuleCount: 1, ConfirmedRules: 1, Samples: 1, ValidSamples: 1}, nil
	})
	ctx := context.WithValue(t.Context(), openAIProfitControlGateCtxKey{}, gate)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, account.GetOpenAIBaseURL()+"/v1/responses", strings.NewReader(`{"model":"gpt-fixture","input":"fixture"}`))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+account.GetCredential("api_key"))
	getBody := request.GetBody
	reads := 0
	request.GetBody = func() (io.ReadCloser, error) { reads++; return getBody() }
	prepared := withProcurementQualityHTTPRequest(request, account)
	physical := prepared.Context()
	require.Zero(t, reads, "installing a request scope must not copy its body")
	blocked, reason := openAIProfitControlLatestPolicyVetoReason(physical, account)
	require.False(t, blocked, reason)
	require.Zero(t, reads, "disabled quality gates must not inspect the body")
	group.ProcurementPolicy.Quality = "current_answer"
	blocked, reason = openAIProfitControlLatestPolicyVetoReason(physical, account)
	require.False(t, blocked, reason)
	require.Equal(t, 1, reads, "a newly enabled quality gate still validates the actual request")
	actual, err := io.ReadAll(prepared.Body)
	require.NoError(t, err)
	require.Equal(t, `{"model":"gpt-fixture","input":"fixture"}`, string(actual), "lazy inspection must restore the physical outbound request")
	require.NoError(t, prepared.Body.Close())
}

func TestProcurementQualityPostSlotAndRegionalAuthority(t *testing.T) {
	account := billingCandidateAccount(t, 901, billingApplicabilityEntry("responses_http", "priority", true))
	group := profitControlTestGroup(7, 0, 0)
	group.ProcurementPolicy = procurementAnswerPolicy()
	ctx := billingCandidateContext(t, &OpenAIGatewayService{}, group, `{"model":"gpt-fixture","input":"fixture","service_tier":"priority"}`)
	gate := ctx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
	gate.procurementGroup = func(context.Context) (*Group, error) { return group, nil }
	status, calls := "pass", 0
	reader := procurementQualityTestReader(func(context.Context, *Account, AvailabilityRequestScope) (*QualityScopeEvidence, error) {
		calls++
		at := time.Now()
		until := at.Add(time.Minute)
		return &QualityScopeEvidence{Version: 1, Status: status, Reason: "all_rules_passed", EvaluatedAt: at, ValidUntil: &until, RuleCount: 1, ConfirmedRules: 1, Samples: 1, ValidSamples: 1}, nil
	})
	gate.qualityReader, gate.qualityCache = reader, &sync.Map{}
	for _, state := range []string{"pass", "unknown", "fail", "pass"} {
		status = state
		before := calls
		_, vetoed, reason := profitControlVetoLatest(ctx, account, nil)
		require.Equal(t, state != "pass", vetoed, reason)
		require.Equal(t, before+1, calls, "post-slot checks must not reuse earlier candidate evidence")
	}
	repo := &procurementQualityGroupAuthority{QualityScopeEvidenceReader: reader}
	snapshot := &SchedulerSnapshotService{groupRepo: repo, localSnapshotReader: &LocalSchedulerSnapshotStore{}}
	require.Nil(t, procurementQualityReader(snapshot, repo), "a regional node must not borrow the primary authority")
	snapshot.localSnapshotReader = nil
	require.Same(t, repo, procurementQualityReader(snapshot, nil))
	require.Same(t, repo, procurementQualityReader(nil, repo))
}
