//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

func TestProcurementPolicyVersionedCommands(t *testing.T) {
	valid := json.RawMessage(`{"version":1,"revision":0,"min_rate":"0.02000000","max_rate":"0.03"}`)
	created, expected, err := procurementPolicyUpdate(valid, nil)
	require.NoError(t, err)
	require.EqualValues(t, 0, *expected)
	require.EqualValues(t, 1, created.Revision)
	require.Equal(t, "0.02", *created.MinRate)
	kept, expected, err := procurementPolicyUpdate(nil, created)
	require.NoError(t, err)
	require.Nil(t, expected)
	*kept.MinRate = "0"
	require.Equal(t, "0.02", *created.MinRate)
	_, _, err = procurementPolicyUpdate(valid, created)
	require.ErrorIs(t, err, ErrProcurementPolicyConflict)
	for _, invalid := range []string{
		`null`, `{}`, `{"version":1,"revision":0}`, `{"version":1,"revision":null,"min_rate":null,"max_rate":null}`,
		`{"version":1,"revision":0,"min_rate":"0.04","max_rate":"0.03"}`,
		`{"version":1,"revision":0,"min_rate":0.02,"max_rate":"0.03"}`,
		`{"version":1,"revision":0,"min_rate":null,"max_rate":null,"version":1}`,
		`{"Version":1,"revision":0,"min_rate":null,"max_rate":null}`,
		`{"version":2,"revision":0,"min_rate":null,"max_rate":null}`,
	} {
		_, _, err := procurementPolicyUpdate(json.RawMessage(invalid), nil)
		require.ErrorIs(t, err, ErrProcurementPolicyInvalid, invalid)
	}
	cleared, _, err := procurementPolicyUpdate(json.RawMessage(`{"version":1,"revision":1,"min_rate":null,"max_rate":null}`), created)
	require.NoError(t, err)
	require.Nil(t, cleared.MinRate)
	require.Nil(t, cleared.MaxRate)
	require.EqualValues(t, 2, cleared.Revision)
}

func TestProcurementPolicyApprovedSourcesGateAndPreview(t *testing.T) {
	for _, tc := range []struct {
		name, ids, reason string
	}{
		{"original_pool", "null", ""},
		{"explicit_empty", "[]", "procurement_source"},
		{"approved", "[901]", ""},
		{"other_account", "[902]", "procurement_source"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := profitControlTestGroup(7, 0, 0)
			raw := `{"procurement_policy":{"version":2,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":` + tc.ids + `}}}`
			require.NoError(t, json.Unmarshal([]byte(raw), group))
			account := profitControlTestAccountWithRate(upstreamCostTestOAuthAccount(901), 0.025)
			gate := (&OpenAIGatewayService{}).resolveOpenAIProfitControlGate(profitControlTestCtx(group), &group.ID)
			ctx := context.WithValue(t.Context(), openAIProfitControlGateCtxKey{}, gate)
			veto, reason := openAIProfitControlVetoReason(ctx, account)
			require.Equal(t, tc.reason, reason)
			require.Equal(t, tc.reason != "", veto)
			verdict := previewAccountProfitAdmission(account, true, 1, 1, time.Now(), &Group{ID: 0, ProcurementPolicy: group.ProcurementPolicy, ProcurementCatalog: nil})
			want := ProfitPreviewClassAdmitted
			if veto {
				want = "rejected_procurement_source"
			}
			require.Equal(t, want, verdict.Class)
		})
	}
}

func TestProcurementPolicyAllowedBucketsGate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		codes   []string
		rate    float64
		catalog bool
		reason  string
	}{
		{"missing catalog", []string{"low"}, .025, false, "procurement_catalog_unavailable"},
		{"unrestricted", nil, .025, false, ""},
		{"explicit empty", []string{}, .025, true, "procurement_bucket"},
		{"inclusive lower", []string{"low"}, .02, true, ""},
		{"inside", []string{"low"}, .025, true, ""},
		{"exclusive upper", []string{"low"}, .03, true, "procurement_bucket"},
		{"below bucket", []string{"low"}, .019999999999999997, true, "procurement_bucket"},
		{"different bucket", []string{"next"}, .025, true, "procurement_bucket"},
		{"both buckets", []string{"low", "next"}, .04, true, ""},
		{"missing unrelated code", []string{"low", "removed"}, .025, true, "procurement_catalog_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := profitControlTestGroup(7, 0, 0)
			group.ProcurementPolicy = &ProcurementPolicy{Version: 3, Revision: 1, Sources: &domain.ProcurementSources{}, Buckets: &domain.ProcurementBuckets{Codes: tc.codes}}
			if tc.catalog {
				group.ProcurementCatalog = procurementBucketTestCatalog()
			}
			ctx, _ := (&OpenAIGatewayService{}).WithOpenAIRequestPricingContext(profitControlTestCtx(group), &group.ID)
			account := profitControlTestAccountWithRate(upstreamCostTestOAuthAccount(1), tc.rate)
			veto, reason := openAIProfitControlVetoReason(ctx, account)
			require.Equal(t, tc.reason, reason)
			require.Equal(t, tc.reason != "", veto)
			report := PreviewProfitAdmission([]ProfitPreviewGroupInput{{Group: group, Accounts: []*Account{account}}}, time.Now())[0]
			expected := ProfitPreviewClassAdmitted
			if tc.reason != "" {
				expected = "rejected_" + tc.reason
			}
			require.Equal(t, expected, report.Verdicts[0].Class)
			require.Equal(t, 1.0, group.RateMultiplier)
		})
	}
}

func TestProcurementPolicyBucketsCompleteCommandsAndDowngrade(t *testing.T) {
	raw := `{"version":3,"revision":0,"min_rate":null,"max_rate":null,"sources":{"account_ids":null},"buckets":{"codes":["next","low"]}}`
	created, expected, err := procurementPolicyUpdate(json.RawMessage(raw), nil)
	require.NoError(t, err)
	require.Zero(t, *expected)
	require.Equal(t, []string{"low", "next"}, created.Buckets.Codes)
	kept, expected, err := procurementPolicyUpdate(nil, created)
	require.NoError(t, err)
	require.Nil(t, expected)
	kept.Buckets.Codes[0] = "other"
	require.Equal(t, "low", created.Buckets.Codes[0])
	for _, invalid := range []string{
		strings.Replace(raw, `,"buckets":{"codes":["next","low"]}`, "", 1),
		strings.Replace(raw, `"sources":{"account_ids":null}`, `"sources":null`, 1),
		strings.Replace(raw, `"codes":["next","low"]`, `"codes":null,"codes":null`, 1),
		strings.Replace(raw, `"version":3`, `"version":2`, 1),
		strings.Replace(raw, `"buckets":`, `"buckets":null,"buckets":`, 1),
	} {
		_, _, err := procurementPolicyUpdate(json.RawMessage(invalid), nil)
		require.ErrorIs(t, err, ErrProcurementPolicyInvalid, invalid)
	}
	for _, version := range []string{"1", "2"} {
		legacy := `{"version":` + version + `,"revision":1,"min_rate":null,"max_rate":null`
		if version == "2" {
			legacy += `,"sources":{"account_ids":null}`
		}
		legacy += `}`
		_, _, err = procurementPolicyUpdate(json.RawMessage(legacy), created)
		require.ErrorIs(t, err, ErrProcurementPolicyInvalid)
		_, _, err = procurementPolicyUpdate(json.RawMessage(strings.Replace(legacy, `"revision":1`, `"revision":0`, 1)), created)
		require.ErrorIs(t, err, ErrProcurementPolicyConflict)
	}
}

func TestProcurementPolicySourcesCompleteCommandsAndDowngrade(t *testing.T) {
	command := `{"version":2,"revision":0,"min_rate":null,"max_rate":null,"sources":{"account_ids":[902,901]}}`
	created, expected, err := procurementPolicyUpdate(json.RawMessage(command), nil)
	require.NoError(t, err)
	require.Zero(t, *expected)
	require.Equal(t, []int64{901, 902}, created.Sources.AccountIDs)
	kept, expected, err := procurementPolicyUpdate(nil, created)
	require.NoError(t, err)
	require.Nil(t, expected)
	kept.Sources.AccountIDs[0] = 1
	require.EqualValues(t, 901, created.Sources.AccountIDs[0])
	for _, sources := range []string{`null`, `{}`, `{"account_ids":[0]}`, `{"account_ids":[1,1]}`, `{"account_ids":[9007199254740992]}`, `{"account_ids":[1.1]}`, `{"account_ids":["1"]}`, `{"account_ids":false}`, `{"account_ids":null,"account_ids":null}`, `{"Account_ids":[1]}`, `{"account_ids":[1],"pass":true}`} {
		raw := `{"version":2,"revision":1,"min_rate":null,"max_rate":null,"sources":` + sources + `}`
		_, _, err = procurementPolicyUpdate(json.RawMessage(raw), created)
		require.ErrorIs(t, err, ErrProcurementPolicyInvalid, raw)
	}
	_, _, err = procurementPolicyUpdate(json.RawMessage(`{"version":1,"revision":1,"min_rate":null,"max_rate":null}`), created)
	require.ErrorIs(t, err, ErrProcurementPolicyInvalid, "legacy complete edits must not erase sources")
	_, _, err = procurementPolicyUpdate(json.RawMessage(`{"version":1,"revision":0,"min_rate":null,"max_rate":null}`), created)
	require.ErrorIs(t, err, ErrProcurementPolicyConflict, "a stale range-only draft must reach explicit review instead of failing as a downgrade")
	_, _, err = procurementPolicyUpdate(json.RawMessage(command), created)
	require.ErrorIs(t, err, ErrProcurementPolicyConflict)
	for _, ids := range []string{`null`, `[]`} {
		updated, _, err := procurementPolicyUpdate(json.RawMessage(`{"version":2,"revision":1,"min_rate":null,"max_rate":null,"sources":{"account_ids":`+ids+`}}`), created)
		require.NoError(t, err)
		require.Equal(t, ids == "null", updated.Sources.AccountIDs == nil)
	}
}

func TestProcurementPolicyLatestTerminalBoundary(t *testing.T) {
	group := profitControlTestGroup(7, 0, 0)
	min, max := "0.02", "0.03"
	group.ProcurementPolicy = &ProcurementPolicy{Version: 1, Revision: 1, MinRate: &min, MaxRate: &max}
	gate := (&OpenAIGatewayService{}).resolveOpenAIProfitControlGate(profitControlTestCtx(group), &group.ID)
	gate.procurementGroup = func(context.Context) (*Group, error) { return group, nil }
	ctx := context.WithValue(t.Context(), openAIProfitControlGateCtxKey{}, gate)
	account := profitControlTestAccountWithRate(upstreamCostTestOAuthAccount(1), 0.025)
	veto, _ := openAIProfitControlLatestPolicyVetoReason(ctx, account)
	require.False(t, veto)
	group.ProcurementPolicy = group.ProcurementPolicy.WithRevision(2)
	newMax := "0.025"
	group.ProcurementPolicy.MaxRate = &newMax
	veto, reason := openAIProfitControlLatestPolicyVetoReason(ctx, account)
	require.True(t, veto)
	require.Equal(t, "procurement_range", reason)
	require.Equal(t, "0.03", *gate.procurementPolicy.MaxRate, "concurrent callbacks must not mutate the original gate")
	group.ProcurementPolicy = nil
	veto, _ = openAIProfitControlLatestPolicyVetoReason(ctx, account)
	require.False(t, veto, "authoritative removal is visible without a new account snapshot")
	gate.procurementGroup = func(context.Context) (*Group, error) { return nil, errors.New("fixture unavailable") }
	veto, reason = openAIProfitControlLatestPolicyVetoReason(ctx, account)
	require.True(t, veto)
	require.Equal(t, "procurement_policy_unavailable", reason)
	gate.procurementGroup = nil
	veto, reason = openAIProfitControlLatestPolicyVetoReason(ctx, account)
	require.True(t, veto)
	require.Equal(t, "procurement_policy_unavailable", reason, "an installed policy needs a terminal authority")
}

func TestProcurementPolicyOfflinePreviewMatchesGate(t *testing.T) {
	min, max := "0.02", "0.03"
	policy := &ProcurementPolicy{Version: 1, Revision: 1, MinRate: &min, MaxRate: &max}
	account := profitControlTestAccountWithRate(upstreamCostTestOAuthAccount(1), 0.03)
	verdict := previewAccountProfitAdmission(account, true, 1, 1, time.Now(), &Group{ID: 0, ProcurementPolicy: policy, ProcurementCatalog: nil})
	require.Equal(t, ProfitPreviewClassRejectedProcurement, verdict.Class)
	verdict = previewAccountProfitAdmission(account, false, 1, 1, time.Now(), &Group{ID: 0, ProcurementPolicy: policy, ProcurementCatalog: nil})
	require.Equal(t, ProfitPreviewClassAdmitted, verdict.Class, "disabled original gate preserves compatibility")
	policy.Version = 2
	verdict = previewAccountProfitAdmission(account, true, 1, 1, time.Now(), &Group{ID: 0, ProcurementPolicy: policy, ProcurementCatalog: nil})
	require.Equal(t, ProfitPreviewClassUnknownProcurement, verdict.Class)
}

func TestProcurementRangeExistingProfitGate(t *testing.T) {
	for _, tc := range []struct {
		name string
		rate float64
		veto bool
	}{
		{"below_lower", 0.01999999, true},
		{"inclusive_lower", 0.02, false},
		{"inside", 0.025, false},
		{"below_upper", 0.02999999, false},
		{"exclusive_upper", 0.03, true},
		{"above_upper", 0.04, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := profitControlTestGroup(7, 0, 0)
			require.NoError(t, json.Unmarshal([]byte(`{"procurement_policy":{"version":1,"revision":1,"min_rate":"0.02","max_rate":"0.03"}}`), group))
			gate := (&OpenAIGatewayService{}).resolveOpenAIProfitControlGate(profitControlTestCtx(group), &group.ID)
			require.NotNil(t, gate)
			ctx := context.WithValue(context.Background(), openAIProfitControlGateCtxKey{}, gate)
			account := profitControlTestAccountWithRate(upstreamCostTestOAuthAccount(1), tc.rate)
			veto, reason := openAIProfitControlVetoReason(ctx, account)
			require.Equal(t, tc.veto, veto)
			if tc.veto {
				require.Equal(t, "procurement_range", reason)
			} else {
				require.Empty(t, reason)
			}
			require.Equal(t, 1.0, group.RateMultiplier, "procurement never reprices downstream sales")
		})
	}
}

func TestProcurementPolicyBothSchedulersAndSticky(t *testing.T) {
	for _, engine := range []string{"true", "false"} {
		t.Run(engine, func(t *testing.T) {
			resetOpenAIAdvancedSchedulerSettingCacheForTest()
			defer resetOpenAIAdvancedSchedulerSettingCacheForTest()
			entry := billingApplicabilityEntry("responses_http", "priority", true)
			excluded, covered := billingCandidateAccount(t, 901, entry), billingCandidateAccount(t, 902, entry)
			for _, item := range []struct {
				a    *Account
				rate float64
			}{{excluded, 0.03}, {covered, 0.025}} {
				item.a.RateMultiplier = &item.rate
				at := time.Now().UTC().Add(-time.Second)
				_, err := prepareUpstreamBillingManualApplicability(t.Context(), item.a, billingManualCommand(t, item.a, at, entry), at)
				require.NoError(t, err)
			}
			cache := &stubGatewayCache{sessionBindings: map[string]int64{"procurement-sticky": excluded.ID}}
			concurrency := &upstreamCostTrackingConcurrencyCache{loadMap: map[int64]*AccountLoadInfo{
				excluded.ID: {AccountID: excluded.ID}, covered.ID: {AccountID: covered.ID},
			}}
			svc := &OpenAIGatewayService{cache: cache, accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{*excluded, *covered}},
				rateLimitService: newOpenAIAdvancedSchedulerRateLimitService(engine), concurrencyService: NewConcurrencyService(concurrency)}
			group := profitControlTestGroup(7, 0, 0)
			min, max := "0.02", "0.03"
			group.ProcurementPolicy = &ProcurementPolicy{Version: 1, Revision: 1, MinRate: &min, MaxRate: &max}
			ctx := billingCandidateContext(t, svc, group, `{"model":"gpt-fixture","input":"fixture","service_tier":"priority"}`)
			for _, session := range []string{"", "procurement-sticky"} {
				selected, _, err := svc.SelectAccountWithScheduler(ctx, &group.ID, "", session, "gpt-fixture", nil, OpenAIUpstreamTransportAny, false)
				require.NoError(t, err)
				require.Equal(t, covered.ID, selected.Account.ID)
				if selected.ReleaseFunc != nil {
					selected.ReleaseFunc()
				}
			}
			require.Equal(t, excluded.ID, cache.sessionBindings["procurement-sticky"])
			require.Zero(t, cache.deletedSessions["procurement-sticky"])
			selected, _, err := svc.SelectAccountWithScheduler(ctx, &group.ID, "", "", "gpt-fixture", map[int64]struct{}{covered.ID: {}}, OpenAIUpstreamTransportAny, false)
			require.Nil(t, selected)
			require.ErrorIs(t, err, ErrNoAvailableAccounts, "retry cannot escape the procurement range")
			group.ProcurementPolicy = &ProcurementPolicy{Version: 2, Revision: 2, Sources: &domain.ProcurementSources{AccountIDs: []int64{covered.ID}}}
			ctx = billingCandidateContext(t, svc, group, `{"model":"gpt-fixture","input":"fixture","service_tier":"priority"}`)
			for _, session := range []string{"", "procurement-sticky"} {
				selected, _, err = svc.SelectAccountWithScheduler(ctx, &group.ID, "", session, "gpt-fixture", nil, OpenAIUpstreamTransportAny, false)
				require.NoError(t, err)
				require.Equal(t, covered.ID, selected.Account.ID)
				if selected.ReleaseFunc != nil {
					selected.ReleaseFunc()
				}
			}
			selected, _, err = svc.SelectAccountWithScheduler(ctx, &group.ID, "", "", "gpt-fixture", map[int64]struct{}{covered.ID: {}}, OpenAIUpstreamTransportAny, false)
			require.Nil(t, selected)
			require.ErrorIs(t, err, ErrNoAvailableAccounts, "a retry must not escape the approved source list")
			require.Equal(t, excluded.ID, cache.sessionBindings["procurement-sticky"], "source rejection does not erase the original binding")
			group.ProcurementPolicy = &ProcurementPolicy{Version: 3, Revision: 3, Sources: &domain.ProcurementSources{}, Buckets: &domain.ProcurementBuckets{Codes: []string{"low"}}}
			group.ProcurementCatalog = procurementBucketTestCatalog()
			ctx = billingCandidateContext(t, svc, group, `{"model":"gpt-fixture","input":"fixture","service_tier":"priority"}`)
			for _, session := range []string{"", "procurement-sticky"} {
				selected, _, err = svc.SelectAccountWithScheduler(ctx, &group.ID, "", session, "gpt-fixture", nil, OpenAIUpstreamTransportAny, false)
				require.NoError(t, err)
				require.Equal(t, covered.ID, selected.Account.ID)
				if selected.ReleaseFunc != nil {
					selected.ReleaseFunc()
				}
			}
			selected, _, err = svc.SelectAccountWithScheduler(ctx, &group.ID, "", "procurement-sticky", "gpt-fixture", map[int64]struct{}{covered.ID: {}}, OpenAIUpstreamTransportAny, false)
			require.Nil(t, selected)
			require.ErrorIs(t, err, ErrNoAvailableAccounts)
			require.Equal(t, excluded.ID, cache.sessionBindings["procurement-sticky"])
			require.Zero(t, cache.deletedSessions["procurement-sticky"])

			group.ProcurementPolicy = procurementSourceGroupTestPolicy()
			group.ProcurementSources = procurementSourceGroupTestSnapshot(group.ID, []int64{covered.ID})
			for _, members := range [][]int64{{covered.ID}, {}, {covered.ID}} {
				group.ProcurementSources.AccountIDs = members
				ctx = billingCandidateContext(t, svc, group, `{"model":"gpt-fixture","input":"fixture","service_tier":"priority"}`)
				for _, session := range []string{"", "procurement-sticky"} {
					selected, _, err := svc.SelectAccountWithScheduler(ctx, &group.ID, "", session, "gpt-fixture", nil, OpenAIUpstreamTransportAny, false)
					if len(members) == 0 {
						require.Nil(t, selected)
						require.ErrorIs(t, err, ErrNoAvailableAccounts)
					} else {
						require.NoError(t, err)
						require.Equal(t, covered.ID, selected.Account.ID)
						if selected.ReleaseFunc != nil {
							selected.ReleaseFunc()
						}
					}
				}
			}
			require.Equal(t, excluded.ID, cache.sessionBindings["procurement-sticky"])
			require.Zero(t, cache.deletedSessions["procurement-sticky"])

		})
	}
}

func TestProcurementPolicyPhysicalWSTurnRevisionAndRecovery(t *testing.T) {
	for _, constraint := range []string{"procurement_range", "procurement_source", "procurement_bucket", "procurement_catalog_unavailable", "procurement_quality_unavailable", "procurement_quality_failed", "procurement_availability_unavailable", "procurement_availability_failed", "source_group_removed", "source_group_unavailable"} {
		for _, mode := range []string{OpenAIWSIngressModePassthrough, OpenAIWSIngressModeCtxPool} {
			t.Run(mode+"/"+constraint, func(t *testing.T) {
				var frames atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					conn, err := coderws.Accept(w, request, nil)
					if err != nil {
						return
					}
					defer func() { _ = conn.CloseNow() }()
					for {
						if _, _, err := conn.Read(request.Context()); err != nil {
							return
						}
						frames.Add(1)
						if err := conn.Write(request.Context(), coderws.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_procurement","usage":{"input_tokens":4,"output_tokens":2}}}`)); err != nil {
							return
						}
					}
				}))
				defer upstream.Close()
				entry := billingApplicabilityEntry("responses_ws_v2", "unspecified", true)
				account := billingCandidateAccount(t, 914, entry)
				rate := 0.025
				account.RateMultiplier = &rate
				account.Credentials["base_url"] = upstream.URL
				account.Credentials["api_key"] = "sk-test"
				account.Extra["openai_apikey_responses_websockets_v2_mode"] = mode
				at := time.Now().UTC().Add(-time.Second)
				_, err := prepareUpstreamBillingManualApplicability(t.Context(), account, billingManualCommand(t, account, at, entry), at)
				require.NoError(t, err)
				repo := &billingWSCandidateAccountRepo{}
				repo.account.Store(account)
				svc := newOpenAIGatewayServiceWithSettings(t, nil)
				svc.cfg = passthroughLifecycleConfig()
				defer svc.getOpenAIWSConnPool().Close()
				svc.cache, svc.toolCorrector = &stubGatewayCache{}, NewCodexToolCorrector()
				svc.accountRepo, svc.requireLatestTurnAdmission = repo, true
				group := profitControlTestGroup(7, 0, 0)
				min, max := "0.02", "0.03"
				group.ProcurementPolicy = &ProcurementPolicy{Version: 1, Revision: 1, MinRate: &min, MaxRate: &max}
				var qualityStatus atomic.Value
				qualityStatus.Store("pass")
				if strings.HasPrefix(constraint, "procurement_quality_") {
					group.ProcurementPolicy = procurementAnswerPolicy()
				}
				if strings.HasPrefix(constraint, "procurement_availability_") {
					group.ProcurementPolicy = procurementCurrentAvailabilityPolicy()
				}
				var latest atomic.Pointer[Group]
				latest.Store(group)
				body := `{"type":"response.create","model":"gpt-fixture","input":"fixture","store":true,"stream":false}`
				if strings.HasPrefix(constraint, "procurement_availability_") {
					body = strings.Replace(body, `"stream":false`, `"stream":true`, 1)
				}
				ctx := billingWSCandidateContext(t, svc, group, body, "gpt-fixture", nil)
				gate := ctx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
				gate.procurementGroup = func(context.Context) (*Group, error) { return latest.Load(), nil }
				gate.qualityReader = procurementQualityTestReader(func(context.Context, *Account, AvailabilityRequestScope) (*QualityScopeEvidence, error) {
					now := time.Now()
					until := now.Add(time.Minute)
					return &QualityScopeEvidence{Version: 1, Status: qualityStatus.Load().(string), Reason: "all_rules_passed", EvaluatedAt: now, ValidUntil: &until, RuleCount: 1, ConfirmedRules: 1, Samples: 1, ValidSamples: 1}, nil
				})
				gate.availabilityReader = procurementAvailabilityTestReader(func(context.Context, *Account) ([]*AvailabilityReport, error) {
					report := availabilityAdmissionPass(account.ID, "gpt-fixture")
					report.Models[0].Decision.Scope.Protocol, report.Models[0].Decision.Scope.Stream = "responses_ws_v2", true
					report.Models[0].Status, report.Models[0].Decision.Status = qualityStatus.Load().(string), qualityStatus.Load().(string)
					return []*AvailabilityReport{report}, nil
				})
				settled := make(chan int, 3)
				hooks := &OpenAIWSIngressHooks{AfterTurn: func(turn int, _ *OpenAIForwardResult, _ error) { settled <- turn }}
				server, done := startPassthroughLifecycleServer(t, ctx, svc, account, hooks)
				defer server.Close()
				client := dialPassthroughLifecycleClientWithPayload(t, server, body)
				defer func() { _ = client.CloseNow() }()
				_, err = readPassthroughLifecycleFrame(t, client, 3*time.Second)
				require.NoError(t, err)
				select {
				case <-settled:
				case <-time.After(3 * time.Second):
					t.Fatal("first turn not settled")
				}
				replacement := *group
				replacement.ProcurementPolicy = group.ProcurementPolicy.WithRevision(2)
				newMax := "0.025"
				replacement.ProcurementPolicy.MaxRate = &newMax
				if strings.HasPrefix(constraint, "procurement_quality_") || strings.HasPrefix(constraint, "procurement_availability_") {
					replacement.ProcurementPolicy.MaxRate = nil
					if strings.HasSuffix(constraint, "_failed") {
						qualityStatus.Store("fail")
					} else {
						qualityStatus.Store("unknown")
					}
				}
				if constraint == "procurement_source" {
					replacement.ProcurementPolicy = &ProcurementPolicy{Version: 2, Revision: 2, Sources: &domain.ProcurementSources{AccountIDs: []int64{account.ID + 1}}}
				}

				if strings.HasPrefix(constraint, "procurement_bucket") || constraint == "procurement_catalog_unavailable" {
					replacement.ProcurementPolicy = &ProcurementPolicy{Version: 3, Revision: 2, Sources: &domain.ProcurementSources{}, Buckets: &domain.ProcurementBuckets{Codes: []string{"next"}}}
					if constraint == "procurement_bucket" {
						replacement.ProcurementCatalog = procurementBucketTestCatalog()
					}
				}
				expectedReason := constraint
				if strings.HasPrefix(constraint, "source_group_") {
					replacement.ProcurementPolicy = procurementSourceGroupTestPolicy()
					replacement.ProcurementSources = procurementSourceGroupTestSnapshot(group.ID, []int64{})
					expectedReason = "procurement_source"
					if constraint == "source_group_unavailable" {
						replacement.ProcurementSources = nil
						expectedReason = "procurement_policy_unavailable"
					}
				}
				latest.Store(&replacement)
				require.NoError(t, client.Write(t.Context(), coderws.MessageText, []byte(body)))
				select {
				case err := <-done:
					require.ErrorContains(t, err, expectedReason)
				case <-time.After(3 * time.Second):
					t.Fatal("changed procurement policy did not reject the next physical turn")
				}
				require.EqualValues(t, 1, frames.Load())
				qualityStatus.Store("pass")
				latest.Store(group)
				recoveryServer, recovered := startPassthroughLifecycleServer(t, ctx, svc, account, nil)
				defer recoveryServer.Close()
				recoveryClient := dialPassthroughLifecycleClientWithPayload(t, recoveryServer, body)
				_, err = readPassthroughLifecycleFrame(t, recoveryClient, 3*time.Second)
				require.NoError(t, err)
				require.EqualValues(t, 2, frames.Load())
				_ = recoveryClient.CloseNow()
				select {
				case <-recovered:
				case <-time.After(3 * time.Second):
					t.Fatal("recovered session did not close")
				}
				require.False(t, strings.Contains(group.Name, "reprice"))
			})
		}
	}
}

func TestProcurementPolicyPhysicalHTTPRevisionAndRecovery(t *testing.T) {
	for _, engine := range []string{"openai", "native"} {
		platforms := strictTurnAdmissionPlatformsForTest()
		if engine == "native" {
			platforms = []string{PlatformOpenAI, PlatformAnthropic, PlatformGemini, PlatformGrok, PlatformAntigravity}
		}
		for _, platform := range platforms {
			t.Run(engine+"/"+platform, func(t *testing.T) {
				var requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"fixture":true}`))
				}))
				defer server.Close()
				group := gatewayProfitTestGroup(9, platform)
				min, max := "0.02", "0.03"
				group.ProcurementPolicy = &ProcurementPolicy{Version: 1, Revision: 1, MinRate: &min, MaxRate: &max}
				account := gatewayProfitTestAccount(901, platform, 0.025, group.ID)
				account.Credentials = map[string]any{"api_key": "procurement-fixture", "base_url": server.URL}
				latest := *group
				var readErr error
				gate := &openAIProfitControlGate{groupID: group.ID, platform: platform, threshold: 0.5,
					procurementPolicy: group.ProcurementPolicy.Clone(), procurementGroup: func(context.Context) (*Group, error) { return &latest, readErr }}
				ctx := context.WithValue(t.Context(), openAIProfitControlGateCtxKey{}, gate)
				upstream := availabilityWSHTTPUpstream{client: server.Client()}
				openai := &OpenAIGatewayService{cfg: &config.Config{}, accountRepo: &turnAdmissionRepo{account: &account}, httpUpstream: upstream}
				native := &GatewayService{cfg: &config.Config{}, accountRepo: gatewayProfitAccountRepo{account: &account}, httpUpstream: upstream}
				send := func(reason string) {
					t.Helper()
					before := requests.Load()
					request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-fixture","input":"fixture"}`))
					require.NoError(t, err)
					var response *http.Response
					if engine == "openai" {
						response, err = openai.doOpenAIUpstream(request, "", &account)
					} else {
						response, err = native.doGatewayUpstream(request, "", &account)
					}
					if response != nil {
						require.NoError(t, response.Body.Close())
					}
					if reason == "" {
						require.NoError(t, err)
						require.Equal(t, before+1, requests.Load())
					} else {
						require.ErrorContains(t, err, reason)
						require.Equal(t, before, requests.Load(), "rejected policy must not reach the upstream")
					}
				}
				send("")
				latest.ProcurementPolicy = group.ProcurementPolicy.WithRevision(2)
				newMax := "0.025"
				latest.ProcurementPolicy.MaxRate = &newMax
				send("procurement_range")
				gate.procurementPolicy = nil
				send("procurement_range") // Newly activated policies also constrain an already prepared request.
				readErr = errors.New("fixture unavailable")
				send("procurement_policy_unavailable")
				readErr = nil
				latest.ID = 99
				send("procurement_policy_unavailable")
				latest.ID = group.ID
				latest.ProcurementPolicy.Version = 2
				send("procurement_policy_unavailable")
				latest.ProcurementPolicy = group.ProcurementPolicy.Clone()
				send("")
				latest.ProcurementPolicy = &ProcurementPolicy{Version: 2, Revision: 3, Sources: &domain.ProcurementSources{AccountIDs: []int64{}}}
				send("procurement_source")
				latest.ProcurementPolicy.Sources.AccountIDs = []int64{account.ID}
				send("")
				latest.ProcurementPolicy.Sources.AccountIDs = []int64{account.ID + 1}
				send("procurement_source")
				latest.ProcurementPolicy.Sources.AccountIDs = nil
				send("")

				latest.ProcurementPolicy = &ProcurementPolicy{Version: 3, Revision: 4, Sources: &domain.ProcurementSources{}, Buckets: &domain.ProcurementBuckets{Codes: []string{"low"}}}
				latest.ProcurementCatalog = procurementBucketTestCatalog()
				send("")
				higher := .04
				account.RateMultiplier = &higher
				send("procurement_bucket")
				lower := .025
				account.RateMultiplier = &lower
				send("")
				latest.ProcurementCatalog = procurementBucketTestCatalog().WithRevision(2)
				upper := "0.025"
				latest.ProcurementCatalog.Buckets[0].MaxRate = &upper
				send("procurement_bucket")
				latest.ProcurementCatalog = nil
				send("procurement_catalog_unavailable")
				latest.ProcurementCatalog = procurementBucketTestCatalog()
				send("")
				latest.ProcurementCatalog.Buckets = latest.ProcurementCatalog.Buckets[1:]
				send("procurement_catalog_unavailable")
				latest.ProcurementPolicy = procurementSourceGroupTestPolicy()
				latest.ProcurementSources = procurementSourceGroupTestSnapshot(group.ID, []int64{account.ID})
				send("")
				latest.ProcurementSources.AccountIDs = []int64{}
				send("procurement_source") // Same policy revision, new source membership.
				latest.ProcurementSources = nil
				send("procurement_policy_unavailable")
				latest.ProcurementSources = procurementSourceGroupTestSnapshot(group.ID, []int64{account.ID})
				send("")
				latest.ProcurementPolicy = nil
				send("")
				require.Equal(t, 0.5, gate.threshold, "fresh procurement never reprices an in-flight request")
				require.Equal(t, "0.03", *group.ProcurementPolicy.MaxRate)
			})
		}
	}
}

func TestProcurementPolicyPostSlotAndPreview(t *testing.T) {
	group := gatewayProfitTestGroup(9, PlatformOpenAI)
	min, max := "0.02", "0.03"
	group.ProcurementPolicy = &ProcurementPolicy{Version: 1, Revision: 1, MinRate: &min, MaxRate: &max}
	account := gatewayProfitTestAccount(901, PlatformOpenAI, 0.025, group.ID)
	gate := &openAIProfitControlGate{groupID: group.ID, platform: group.Platform, threshold: 0.5,
		procurementPolicy: group.ProcurementPolicy.Clone(), procurementGroup: func(context.Context) (*Group, error) { return group, nil }}
	ctx := context.WithValue(t.Context(), openAIProfitControlGateCtxKey{}, gate)
	group.ProcurementPolicy = group.ProcurementPolicy.WithRevision(2)
	newMin := "0.026"
	group.ProcurementPolicy.MinRate = &newMin
	_, vetoed, reason := profitControlVetoLatest(ctx, &account, nil)
	require.True(t, vetoed)
	require.Equal(t, "procurement_range", reason)
	reports := PreviewProfitAdmission([]ProfitPreviewGroupInput{{Group: group, Accounts: []*Account{&account}, Models: []string{"gpt-fixture"}}}, time.Now())
	require.Equal(t, ProfitPreviewClassRejectedProcurement, reports[0].Verdicts[0].Class)
	require.Zero(t, reports[0].RemainingByModel["gpt-fixture"])
	newMin = "0.02"
	_, vetoed, reason = profitControlVetoLatest(ctx, &account, nil)
	require.False(t, vetoed, reason)
	group.ProcurementPolicy = &ProcurementPolicy{Version: 2, Revision: 3, Sources: &domain.ProcurementSources{AccountIDs: []int64{}}}
	_, vetoed, reason = profitControlVetoLatest(ctx, &account, nil)
	require.True(t, vetoed)
	require.Equal(t, "procurement_source", reason)
	reports = PreviewProfitAdmission([]ProfitPreviewGroupInput{{Group: group, Accounts: []*Account{&account}}}, time.Now())
	require.Equal(t, ProfitPreviewClassRejectedSource, reports[0].Verdicts[0].Class)
	group.ProcurementPolicy.Sources.AccountIDs = []int64{account.ID}
	_, vetoed, reason = profitControlVetoLatest(ctx, &account, nil)
	require.False(t, vetoed, reason)
	group.ProcurementPolicy = &ProcurementPolicy{Version: 3, Revision: 4, Sources: &domain.ProcurementSources{}, Buckets: &domain.ProcurementBuckets{Codes: []string{"next"}}}
	group.ProcurementCatalog = procurementBucketTestCatalog()
	_, vetoed, reason = profitControlVetoLatest(ctx, &account, nil)
	require.True(t, vetoed)
	require.Equal(t, "procurement_bucket", reason)
	group.ProcurementPolicy.Buckets.Codes = []string{"low"}
	_, vetoed, reason = profitControlVetoLatest(ctx, &account, nil)
	require.False(t, vetoed, reason)
	group.ProcurementPolicy = procurementSourceGroupTestPolicy()
	group.ProcurementSources = procurementSourceGroupTestSnapshot(group.ID, []int64{account.ID})
	_, vetoed, reason = profitControlVetoLatest(ctx, &account, nil)
	require.False(t, vetoed, reason)
	group.ProcurementSources.AccountIDs = []int64{}
	_, vetoed, reason = profitControlVetoLatest(ctx, &account, nil)
	require.True(t, vetoed)
	require.Equal(t, "procurement_source", reason)
	group.ProcurementSources = nil
	_, vetoed, reason = profitControlVetoLatest(ctx, &account, nil)
	require.True(t, vetoed)
	require.Equal(t, "procurement_policy_unavailable", reason)
	group.ProcurementSources = procurementSourceGroupTestSnapshot(group.ID, []int64{account.ID})
	_, vetoed, reason = profitControlVetoLatest(ctx, &account, nil)
	require.False(t, vetoed, reason)
	require.Equal(t, .5, gate.threshold)
}

func TestProcurementPolicyObservationsRemainSeparate(t *testing.T) {
	observer := &openAIProfitControlObserver{}
	observer.recordVeto(9, PlatformOpenAI, 0.5, openAIProfitFilterReasonProcurementRange)
	observer.recordVeto(9, PlatformOpenAI, 0.5, openAIProfitFilterReasonProcurementUnavailable)
	observer.recordVeto(9, PlatformOpenAI, 0.5, openAIProfitFilterReasonProcurementSource)
	observer.recordVeto(9, PlatformOpenAI, .5, openAIProfitFilterReasonProcurementBucket)
	observer.recordVeto(9, PlatformOpenAI, .5, openAIProfitFilterReasonProcurementCatalog)
	observer.recordVeto(9, PlatformOpenAI, .5, "procurement_quality_failed")
	observer.recordVeto(9, PlatformOpenAI, .5, "procurement_quality_unavailable")
	observer.recordVeto(9, PlatformOpenAI, .5, "procurement_availability_failed")
	observer.recordVeto(9, PlatformOpenAI, .5, "procurement_availability_unavailable")
	stats := observer.stats(9, PlatformOpenAI)
	require.EqualValues(t, 1, stats.vetoQuality.Load())
	require.EqualValues(t, 1, stats.vetoQualityUnknown.Load())
	require.EqualValues(t, 1, stats.vetoAvailability.Load())
	require.EqualValues(t, 1, stats.vetoAvailabilityUnknown.Load())
	require.EqualValues(t, 1, stats.vetoBucket.Load())
	require.EqualValues(t, 1, stats.vetoCatalog.Load())
	require.EqualValues(t, 1, stats.vetoProcurement.Load())
	require.EqualValues(t, 1, stats.vetoPolicy.Load())
	require.EqualValues(t, 1, stats.vetoSource.Load())
	require.Zero(t, stats.vetoThreshold.Load(), "procurement rejection is not a margin failure")
	require.Zero(t, stats.vetoScope.Load(), "an interval is not model/fast applicability")
	require.Zero(t, stats.vetoUpstreamRate.Load(), "policy storage failure is not a supplier price observation")
}

func TestProcurementPolicySourceGroupsPreviewAndGate(t *testing.T) {
	group := profitControlTestGroup(7, 0, 0)
	group.ProcurementPolicy = procurementSourceGroupTestPolicy()
	account := profitControlTestAccountWithRate(upstreamCostTestOAuthAccount(901), .025)
	for _, tc := range []struct {
		name   string
		source *domain.ProcurementSourceSnapshot
		want   string
	}{
		{"unknown", nil, "procurement_policy_unavailable"}, {"approved", procurementSourceGroupTestSnapshot(7, []int64{901}), ""}, {"removed", procurementSourceGroupTestSnapshot(7, []int64{}), "procurement_source"}, {"wrong product", procurementSourceGroupTestSnapshot(8, []int64{901}), "procurement_policy_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group.ProcurementSources = tc.source
			gate := (&OpenAIGatewayService{}).resolveOpenAIProfitControlGate(profitControlTestCtx(group), &group.ID)
			ctx := context.WithValue(t.Context(), openAIProfitControlGateCtxKey{}, gate)
			veto, reason := openAIProfitControlVetoReason(ctx, account)
			require.Equal(t, tc.want, reason)
			require.Equal(t, tc.want != "", veto)
			verdict := previewAccountProfitAdmission(account, true, 1, 1, time.Now(), group)
			want := ProfitPreviewClassAdmitted
			if tc.want == "procurement_source" {
				want = ProfitPreviewClassRejectedSource
			}
			if tc.want == "procurement_policy_unavailable" {
				want = ProfitPreviewClassUnknownProcurement
			}
			require.Equal(t, want, verdict.Class)
		})
	}
}
