//go:build unit

package service

import (
	"context"
	"encoding/json"
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

type procurementAvailabilityTestReader func(context.Context, *Account) ([]*AvailabilityReport, error)

func (f procurementAvailabilityTestReader) ReadAccountAvailability(ctx context.Context, account *Account) ([]*AvailabilityReport, error) {
	return f(ctx, account)
}

func procurementCurrentAvailabilityPolicy() *ProcurementPolicy {
	return &ProcurementPolicy{Version: 5, Revision: 1, Sources: &domain.ProcurementSources{}, Buckets: &domain.ProcurementBuckets{}, Quality: "not_required", Availability: "current_state"}
}

func TestProcurementAvailabilityCurrentExactStateAndProjection(t *testing.T) {
	for _, scenario := range []string{"pass", "managed-pass", "fail", "stale", "unknown", "unconfigured", "no-rules", "paused", "missing-model", "wrong-account", "nil-report", "missing-decision", "future-decision", "future-version", "missing-clock", "future-clock", "scope", "connection", "multiple-pass", "multiple-fail", "multiple-unknown", "read-error", "clock-offset", "off"} {
		t.Run(scenario, func(t *testing.T) {
			account := billingCandidateAccount(t, 901, billingApplicabilityEntry("responses_http", "unspecified", true))
			report := availabilityAdmissionPass(account.ID, "gpt-fixture")
			scope := *report.Models[0].Decision.Scope
			if scenario == "managed-pass" {
				account.AuthConnectionID, account.AuthConnectionRevision = "selected-fixture", 1
				scope.ConnectionScope = AvailabilityConnectionScope(account)
				report.Models[0].Decision.Scope.ConnectionScope = scope.ConnectionScope
			}
			reports := []*AvailabilityReport{report}
			want := "procurement_availability_unavailable"
			switch scenario {
			case "pass", "managed-pass", "multiple-pass", "clock-offset", "off":
				want = ""
			case "fail", "multiple-fail":
				report.Models[0].Status, report.Models[0].Decision.Status = "fail", "fail"
				want = "procurement_availability_failed"
			case "stale":
				until := report.EvaluatedAt.Add(-time.Second)
				report.Models[0].ValidUntil = &until
			case "unknown":
				report.Models[0].Status = "unknown"
			case "unconfigured":
				report.Configured = false
			case "no-rules":
				reports = nil
			case "paused":
				report.Enabled = false
			case "missing-model":
				report.Models[0].ModelID = "different-model"
			case "wrong-account":
				report.AccountID++
			case "nil-report":
				reports[0] = nil
			case "missing-decision":
				report.Models[0].Decision = nil
			case "future-decision":
				report.Models[0].Decision.FinishedAt = report.EvaluatedAt.Add(time.Hour)
			case "future-version":
				report.Models[0].Decision.Version++
			case "missing-clock":
				report.EvaluatedAt = time.Time{}
			case "future-clock":
				report.EvaluatedAt = report.EvaluatedAt.Add(time.Hour)
			case "scope":
				report.Models[0].Decision.Scope.Stream = !scope.Stream
			case "connection":
				report.Models[0].Decision.Scope.ConnectionScope = "other-connection"
			}
			if strings.HasPrefix(scenario, "multiple-") {
				second := availabilityAdmissionPass(account.ID, scope.Model)
				if scenario == "multiple-unknown" {
					second.Enabled = false
				}
				reports = append(reports, second)
			}
			if scenario == "clock-offset" {
				report.EvaluatedAt = report.EvaluatedAt.Add(-time.Hour)
				state := &report.Models[0]
				state.Decision.StartedAt = state.Decision.StartedAt.Add(-time.Hour)
				state.Decision.FinishedAt = state.Decision.FinishedAt.Add(-time.Hour)
				until := state.ValidUntil.Add(-time.Hour)
				state.ValidUntil = &until
			}
			calls := 0
			gate := &openAIProfitControlGate{procurementPolicy: procurementCurrentAvailabilityPolicy()}
			gate.availabilityReader = procurementAvailabilityTestReader(func(ctx context.Context, a *Account) ([]*AvailabilityReport, error) {
				calls++
				require.Equal(t, account.ID, a.ID)
				_, bounded := ctx.Deadline()
				require.True(t, bounded)
				if scenario == "read-error" {
					return nil, errors.New("fixture unavailable")
				}
				return reports, nil
			})
			if scenario == "off" {
				gate.procurementPolicy.Availability = "not_required"
			}
			// A future authority clock alone does not certify the old decision.
			if scenario == "future-clock" {
				want = "procurement_availability_unavailable"
			}
			require.Equal(t, want, procurementAvailabilityScopeVetoReason(t.Context(), gate, account, &scope))
			require.Equal(t, scenario != "off", calls > 0)
			if scenario != "read-error" {
				at := report.EvaluatedAt
				for _, item := range reports {
					if item != nil && item.EvaluatedAt.After(at) {
						at = item.EvaluatedAt
					}
				}
				evidence := AssessAvailabilityScopeEvidence(account, &scope, reports, at)
				view := ProcurementAvailability(gate.procurementPolicy, evidence, at)
				if want == "" {
					require.Contains(t, []string{"pass", "not_required"}, view.State)
				} else if strings.HasSuffix(want, "_failed") {
					require.Equal(t, "fail", view.State)
				} else {
					require.Equal(t, "unknown", view.State)
				}
			}
		})
	}
}

func TestProcurementAvailabilityCandidateCacheAndPostSlotRefresh(t *testing.T) {
	account := billingCandidateAccount(t, 901, billingApplicabilityEntry("responses_http", "unspecified", true))
	report := availabilityAdmissionPass(account.ID, "gpt-fixture")
	scope := *report.Models[0].Decision.Scope
	group := profitControlTestGroup(7, 0, 0)
	group.ProcurementPolicy = procurementCurrentAvailabilityPolicy()
	calls := 0
	reader := procurementAvailabilityTestReader(func(context.Context, *Account) ([]*AvailabilityReport, error) {
		calls++
		if calls > 1 {
			return nil, nil
		}
		return []*AvailabilityReport{report}, nil
	})
	gate := &openAIProfitControlGate{groupID: group.ID, platform: group.Platform, threshold: 1, procurementPolicy: group.ProcurementPolicy.Clone(), procurementGroup: func(context.Context) (*Group, error) { return group, nil }, availabilityReader: reader, availabilityCache: &sync.Map{}}
	ctx := context.WithValue(t.Context(), openAIProfitControlGateCtxKey{}, gate)
	request, err := http.NewRequest(http.MethodPost, "http://fixture.invalid/v1/responses", nil)
	require.NoError(t, err)
	ctx = (&OpenAIGatewayService{cfg: &config.Config{}}).WithUpstreamBillingCandidateRequest(ctx, request, []byte(`{"model":"gpt-fixture","input":"fixture"}`))
	require.Empty(t, procurementAvailabilityScopeVetoReason(ctx, gate, account, &scope))
	require.Empty(t, procurementAvailabilityScopeVetoReason(ctx, gate, account, &scope))
	require.Equal(t, 1, calls)
	_, vetoed, reason := profitControlVetoLatest(ctx, account, nil)
	require.True(t, vetoed)
	require.Equal(t, "procurement_availability_unavailable", reason)
	require.Equal(t, 2, calls, "post-slot admission must not reuse candidate evidence")
	physical := context.WithValue(ctx, procurementActualScopeKey{}, func() *AvailabilityRequestScope { return nil })
	require.Equal(t, "procurement_availability_unavailable", procurementAvailabilityCandidateVetoReason(physical, account))
	var current ProcurementPolicy
	raw, err := json.Marshal(group.ProcurementPolicy)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &current))
	_, _, err = PrepareProcurementPolicyUpdate([]byte(strings.Replace(string(raw), `"version":5`, `"version":4`, 1)), &current)
	require.ErrorIs(t, err, ErrProcurementPolicyInvalid)
}

func TestProcurementAvailabilityHTTPWireAndLazyScope(t *testing.T) {
	sends := 0
	const body = `{"model":"gpt-fixture","input":"fixture"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actual, err := io.ReadAll(r.Body)
		if err != nil || string(actual) != body {
			w.WriteHeader(400)
			return
		}
		sends++
		w.WriteHeader(200)
	}))
	defer server.Close()
	account := billingCandidateAccount(t, 901, billingApplicabilityEntry("responses_http", "unspecified", true))
	account.Extra, account.Credentials["base_url"] = map[string]any{}, server.URL
	group := profitControlTestGroup(7, 0, 0)
	group.ProcurementPolicy = procurementCurrentAvailabilityPolicy()
	gate := &openAIProfitControlGate{groupID: group.ID, platform: group.Platform, threshold: 1, procurementPolicy: group.ProcurementPolicy.Clone(), procurementGroup: func(context.Context) (*Group, error) { return group, nil }}
	status := "pass"
	gate.availabilityReader = procurementAvailabilityTestReader(func(context.Context, *Account) ([]*AvailabilityReport, error) {
		if status == "unknown" {
			return nil, nil
		}
		report := availabilityAdmissionPass(account.ID, "gpt-fixture")
		report.Models[0].Status, report.Models[0].Decision.Status = status, status
		return []*AvailabilityReport{report}, nil
	})
	ctx := context.WithValue(t.Context(), openAIProfitControlGateCtxKey{}, gate)
	svc := &OpenAIGatewayService{cfg: &config.Config{}, accountRepo: &turnAdmissionRepo{account: account}}
	for _, status = range []string{"pass", "unknown", "fail", "pass"} {
		for _, engine := range []string{"openai", "native"} {
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(body))
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer "+account.GetCredential("api_key"))
			var guarded *http.Request
			if engine == "native" {
				guarded, err = guardGatewayUpstreamRequest(request, account, &gatewayFinalAdmissionRepo{latest: account}, svc.cfg)
			} else {
				guarded, err = svc.guardOpenAIProfitRequest(request, account)
			}
			before := sends
			if status != "pass" {
				require.ErrorContains(t, err, "procurement_availability_")
				require.Equal(t, before, sends)
				continue
			}
			require.NoError(t, err)
			response, sendErr := server.Client().Do(guarded)
			require.NoError(t, sendErr)
			require.Equal(t, http.StatusOK, response.StatusCode)
			require.NoError(t, response.Body.Close())
		}
	}
	require.Equal(t, 4, sends)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+account.GetCredential("api_key"))
	reads, getBody := 0, request.GetBody
	request.GetBody = func() (io.ReadCloser, error) { reads++; return getBody() }
	prepared := withProcurementHTTPRequest(request, account)
	group.ProcurementPolicy.Availability = "not_required"
	vetoed, reason := openAIProfitControlLatestPolicyVetoReason(prepared.Context(), account)
	require.False(t, vetoed, reason)
	require.Zero(t, reads)
	group.ProcurementPolicy.Availability = "current_state"
	vetoed, reason = openAIProfitControlLatestPolicyVetoReason(prepared.Context(), account)
	require.False(t, vetoed, reason)
	require.Equal(t, 1, reads)
	actual, err := io.ReadAll(prepared.Body)
	require.NoError(t, err)
	require.Equal(t, body, string(actual))
	require.NoError(t, prepared.Body.Close())
}

func TestProcurementAvailabilityBothSchedulersKeepHealthyBinding(t *testing.T) {
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
			group.ProcurementPolicy = procurementCurrentAvailabilityPolicy()
			for _, state := range []string{"first-fails", "first-recovers"} {
				ctx := billingCandidateContext(t, svc, group, `{"model":"gpt-fixture","input":"fixture","service_tier":"priority"}`)
				gate := ctx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
				gate.availabilityReader = procurementAvailabilityTestReader(func(ctx context.Context, a *Account) ([]*AvailabilityReport, error) {
					status := "pass"
					if state == "first-fails" && a.ID == first.ID {
						status = "fail"
					}
					report := availabilityAdmissionPass(a.ID, "gpt-fixture")
					report.Models[0].Decision.Scope.ServiceTier = "priority"
					report.Models[0].Status, report.Models[0].Decision.Status = status, status
					return []*AvailabilityReport{report}, nil
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

func TestProcurementAvailabilityKeepsOriginalNativeWireScope(t *testing.T) {
	for _, fixture := range []struct {
		platform, path, body string
		managed              bool
	}{
		{PlatformAnthropic, "/v1/messages", `{"model":"target","messages":[{"role":"user","content":"fixture"}],"max_tokens":5}`, false},
		{PlatformGemini, "/v1beta/models/target:generateContent", `{"contents":[{"role":"user","parts":[{"text":"fixture"}]}]}`, false},
		{PlatformOpenAI, "/v1/responses", `{"model":"target","input":"fixture"}`, false},
		{PlatformOpenAI, "/v1/responses", `{"model":"target","input":"fixture"}`, true},
	} {
		t.Run(fixture.platform, func(t *testing.T) {
			account := gatewayProfitTestAccount(901, fixture.platform, .025, 7)
			account.Credentials = map[string]any{"api_key": "selected-fixture-key"}
			if fixture.managed {
				account.AuthConnectionID, account.AuthConnectionRevision = "selected-fixture", 1
			}
			request, err := http.NewRequest(http.MethodPost, "https://fixture.invalid"+fixture.path, strings.NewReader(fixture.body))
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer "+account.GetCredential("api_key"))
			scope := availabilityScopeForAccount(availabilityScopeForRequest(request, []byte(fixture.body)), request, &account)
			require.NotNil(t, scope)
			group := gatewayProfitTestGroup(7, fixture.platform)
			group.ProcurementPolicy = procurementCurrentAvailabilityPolicy()
			gate := &openAIProfitControlGate{groupID: group.ID, platform: group.Platform, threshold: 1, procurementPolicy: group.ProcurementPolicy.Clone(), procurementGroup: func(context.Context) (*Group, error) { return group, nil }}
			gate.availabilityReader = procurementAvailabilityTestReader(func(context.Context, *Account) ([]*AvailabilityReport, error) {
				report := availabilityAdmissionPass(account.ID, scope.Model)
				report.Models[0].Decision.Scope = scope
				return []*AvailabilityReport{report}, nil
			})
			ctx := context.WithValue(t.Context(), openAIProfitControlGateCtxKey{}, gate)
			physical := withProcurementHTTPRequest(request.WithContext(ctx), &account)
			denied, reason := openAIProfitControlLatestPolicyVetoReason(physical.Context(), &account)
			require.False(t, denied, reason)
			if fixture.managed {
				require.NotEmpty(t, scope.ConnectionScope)
				account.AuthConnectionRevision++
				denied, reason = openAIProfitControlLatestPolicyVetoReason(physical.Context(), &account)
				require.True(t, denied)
				require.Equal(t, "procurement_availability_unavailable", reason)
				account.AuthConnectionRevision--
				denied, reason = openAIProfitControlLatestPolicyVetoReason(physical.Context(), &account)
				require.False(t, denied, reason)
			}
			group.ProcurementPolicy.Quality = "current_answer"
			denied, reason = openAIProfitControlLatestPolicyVetoReason(physical.Context(), &account)
			require.True(t, denied)
			require.Equal(t, "procurement_quality_unavailable", reason, "latest-state evidence must not widen the separate answer-quality authority")
			require.NoError(t, physical.Body.Close())
		})
	}
}

type procurementAvailabilityGroupAuthority struct {
	GroupRepository
	AccountAvailabilityReader
}

func TestProcurementAvailabilityAuthoritySelection(t *testing.T) {
	reader := procurementAvailabilityTestReader(func(context.Context, *Account) ([]*AvailabilityReport, error) {
		t.Fatal("unexpected primary read")
		return nil, nil
	})
	repo := &procurementAvailabilityGroupAuthority{AccountAvailabilityReader: reader}
	snapshot := &SchedulerSnapshotService{groupRepo: repo, localSnapshotReader: &LocalSchedulerSnapshotStore{}}
	require.Nil(t, procurementAvailabilityReader(snapshot, repo), "a candidate-only regional store must not fall back to primary")
	snapshot.localSnapshotReader = nil
	require.Same(t, repo, procurementAvailabilityReader(snapshot, nil))
	require.Same(t, repo, procurementAvailabilityReader(nil, repo))
	gate := (&OpenAIGatewayService{availabilityReader: reader}).resolveOpenAIProfitControlGate(profitControlTestCtx(profitControlTestGroup(7, 0, 0)), func() *int64 { id := int64(7); return &id }())
	require.NotNil(t, gate.availabilityReader, "original installed authority works without a scheduler snapshot")
}
