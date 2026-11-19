package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type UpstreamCacheProbeInput struct {
	Model           string `json:"model"`
	IntervalsMS     []int  `json:"intervals_ms"`
	PrefixBlocks    int    `json:"prefix_blocks"`
	TimeoutSeconds  int    `json:"timeout_seconds"`
	ReasoningEffort string `json:"reasoning_effort"`
	ServiceTier     string `json:"service_tier"`
}

func (p UpstreamCacheProbeInput) Validate() error {
	if strings.TrimSpace(p.Model) == "" || len(p.Model) > 200 || strings.ContainsAny(p.Model, "\r\n\x00") ||
		len(p.IntervalsMS) < 1 || len(p.IntervalsMS) > 5 || p.PrefixBlocks < 16 || p.PrefixBlocks > 128 ||
		p.TimeoutSeconds < 10 || p.TimeoutSeconds > 120 ||
		p.ReasoningEffort == "ultra" || p.ReasoningEffort != "" && normalizePelicanReasoningEffort(p.ReasoningEffort) != p.ReasoningEffort ||
		p.ServiceTier != "" && p.ServiceTier != "default" && p.ServiceTier != "priority" {
		return infraerrors.BadRequest("INVALID_UPSTREAM_CACHE_PROBE", "invalid upstream cache probe settings")
	}
	for _, interval := range p.IntervalsMS {
		if interval < 0 || interval > 10000 {
			return infraerrors.BadRequest("INVALID_UPSTREAM_CACHE_PROBE", "invalid upstream cache probe interval")
		}
	}
	return nil
}

// This is a manual observation, not a capability grant. No prompt, credential,
// route digest or provider response text crosses this public allowlist.
type UpstreamCacheProbeRun struct {
	Version            int                          `json:"version"`
	AccountID          int64                        `json:"account_id"`
	Settings           UpstreamCacheProbeInput      `json:"settings"`
	StartedAt          time.Time                    `json:"started_at"`
	FinishedAt         *time.Time                   `json:"finished_at"`
	Status             string                       `json:"status"`
	Verdict            string                       `json:"verdict"`
	Reason             string                       `json:"reason"`
	Source             string                       `json:"source"`
	ColdBaseline       string                       `json:"cold_baseline"`
	RouteContinuity    string                       `json:"route_continuity"`
	UpstreamGroup      string                       `json:"upstream_group"`
	ModelIdentity      string                       `json:"model_identity"`
	CacheDomain        string                       `json:"cache_domain"`
	ValidUntil         *time.Time                   `json:"valid_until"`
	PlannedWarmRounds  int                          `json:"planned_warm_rounds"`
	ObservedWarmRounds int                          `json:"observed_warm_rounds"`
	RequestHitRate     *float64                     `json:"request_hit_rate"`
	TokenHitRate       *float64                     `json:"token_hit_rate"`
	BudgetDeferral     *MonitoringProbeBudgetDenial `json:"budget_deferral,omitempty"`
	Rounds             []UpstreamCacheProbeRound    `json:"rounds"`
}

type UpstreamCacheProbeRound struct {
	Index        int                             `json:"index"`
	IntervalMS   int                             `json:"interval_ms"`
	StartedAt    time.Time                       `json:"started_at"`
	FinishedAt   *time.Time                      `json:"finished_at"`
	Status       string                          `json:"status"`
	Reason       string                          `json:"reason"`
	Scope        *AvailabilityRequestScope       `json:"scope"`
	Cache        *requesttiming.CacheObservation `json:"cache"`
	OutputTokens *int64                          `json:"output_tokens"`
	FirstTextMS  *float64                        `json:"first_text_ms"`
	DurationMS   *float64                        `json:"duration_ms"`
}

type upstreamCacheProbeKey struct{}
type upstreamCacheProbeRuntime struct {
	service       *AccountTestService
	accountID     int64
	model         string
	fingerprint   [32]byte
	expectedScope *AvailabilityRequestScope
	input         UpstreamCacheProbeInput
	physical      atomic.Int32
	reason        string
}

var errUpstreamCacheProbeRoute = errors.New("upstream cache probe route is unsupported or changed")

func (s *AccountTestService) upstreamCacheProbeAccount(account *Account, model string) bool {
	if account == nil || account.ID <= 0 || !account.IsOpenAI() || account.Type != AccountTypeAPIKey ||
		account.ParentAccountID != nil || account.AuthConnectionID != "" || account.HasManagedOpenAIConnections() ||
		account.Status != StatusActive || !account.Schedulable || account.GetCredential("api_key") == "" ||
		account.IsSIWCEnabled() || account.IsCopilotSDKEnabled() || account.IsOpenAIPersonalAccessToken() || account.IsOpenAIPassthroughEnabled() ||
		account.ProxyID != nil && account.Proxy == nil || s.pluginManager != nil && s.pluginManager.ShouldRouteOpenAIOAuth(account) {
		return false
	}
	_, err := modelTraceEnvironmentForAccount(account, model)
	return err == nil && account.IsModelSupported(model)
}

// Check again after the original entry point reloads an account, before it can
// resolve a managed credential or enter a different adapter.
func validateUpstreamCacheProbeAccount(ctx context.Context, account *Account) error {
	runtime, _ := ctx.Value(upstreamCacheProbeKey{}).(*upstreamCacheProbeRuntime)
	if runtime == nil {
		return nil
	}
	if !runtime.service.upstreamCacheProbeAccount(account, runtime.model) || account.ID != runtime.accountID || AccountRouteFingerprint(account) != runtime.fingerprint {
		runtime.reason = "route_changed"
		return errUpstreamCacheProbeRoute
	}
	return nil
}

func validateUpstreamCacheProbeEgress(request *http.Request, account *Account, proxy string) error {
	runtime, _ := request.Context().Value(upstreamCacheProbeKey{}).(*upstreamCacheProbeRuntime)
	if runtime == nil {
		return nil
	}
	if err := validateUpstreamCacheProbeAccount(request.Context(), account); err != nil {
		return err
	}
	expectedProxy := ""
	if account.Proxy != nil {
		expectedProxy = account.Proxy.URL()
	}
	if proxy != expectedProxy {
		runtime.reason = "scope_or_identity_unconfirmed"
		return errUpstreamCacheProbeRoute
	}
	return nil
}

// Both native HTTP payload builders use this same bounded probe option. All
// normal account/Pelican tests retain their original payloads.
func applyUpstreamCacheProbePayload(ctx context.Context, payload map[string]any, chat bool) {
	runtime, _ := ctx.Value(upstreamCacheProbeKey{}).(*upstreamCacheProbeRuntime)
	if runtime == nil {
		return
	}
	if chat {
		payload["max_completion_tokens"] = 256
		payload["stream_options"] = map[string]any{"include_usage": true}
	} else {
		payload["max_output_tokens"] = 256
	}
	if runtime.input.ServiceTier != "" {
		payload["service_tier"] = runtime.input.ServiceTier
	}
}

// This is called by the existing physical monitoring-budget hook, so a TLS
// fallback or compatibility retry cannot spend another generation unnoticed.
func admitUpstreamCacheProbePhysical(request *http.Request) error {
	runtime, _ := request.Context().Value(upstreamCacheProbeKey{}).(*upstreamCacheProbeRuntime)
	if runtime == nil {
		return nil
	}
	if err := request.Context().Err(); err != nil {
		runtime.reason = "canceled"
		return err
	}
	account, err := runtime.service.accountRepo.GetByID(request.Context(), runtime.accountID)
	if err != nil {
		runtime.reason = "account_unavailable"
		return errUpstreamCacheProbeRoute
	}
	if err = validateUpstreamCacheProbeAccount(request.Context(), account); err != nil {
		return err
	}
	proxy := ""
	if account.Proxy != nil {
		proxy = account.Proxy.URL()
	}
	route := qualityRouteForRequest(account, request, proxy)
	payload, confirmed := availabilityRequestPayload(request)
	scope := availabilityScopeForRequest(request, payload)
	if !confirmed || route == nil || route.Fingerprint != hex.EncodeToString(runtime.fingerprint[:]) || scope == nil ||
		scope.Model != account.GetMappedModel(runtime.model) || !scope.Stream || scope.Capability != "text" ||
		(scope.Protocol != "responses_http" && scope.Protocol != "chat_http") ||
		runtime.expectedScope != nil && *runtime.expectedScope != *scope {
		runtime.reason = "scope_or_identity_unconfirmed"
		return errUpstreamCacheProbeRoute
	}
	if !runtime.physical.CompareAndSwap(0, 1) {
		runtime.reason = "additional_send_blocked"
		return errUpstreamCacheProbeRoute
	}
	return nil
}

// Stream through the same SSE writer/keepalive as the account tests. Its only
// events are safe, structured snapshots; upstream output is never relayed.
func (s *AccountTestService) TestUpstreamCacheProbe(c *gin.Context, accountID int64, input UpstreamCacheProbeInput) error {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-store")
	c.Header("X-Accel-Buffering", "no")
	stop := startAccountTestSSEKeepalive(c, 10*time.Second)
	defer stop()
	_, err := s.RunUpstreamCacheProbe(c.Request.Context(), accountID, input, func(run *UpstreamCacheProbeRun) {
		s.sendEvent(c, TestEvent{Type: "cache_probe", Data: run})
	})
	if err != nil {
		s.sendEvent(c, TestEvent{Type: "error", Code: "UPSTREAM_CACHE_PROBE_UNAVAILABLE"})
	}
	return err
}

func (s *AccountTestService) RunUpstreamCacheProbe(ctx context.Context, accountID int64, input UpstreamCacheProbeInput, changed func(*UpstreamCacheProbeRun)) (*UpstreamCacheProbeRun, error) {
	if err := input.Validate(); err != nil {
		return nil, err
	}
	if s == nil || s.accountRepo == nil || s.monitoringBudget == nil || s.monitoringBudget.repo == nil || s.httpUpstream == nil {
		return nil, ErrMonitoringProbeBudgetUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if account == nil || account.ID != accountID || !s.upstreamCacheProbeAccount(account, strings.TrimSpace(input.Model)) {
		return nil, infraerrors.BadRequest("UPSTREAM_CACHE_PROBE_UNSUPPORTED", "select a schedulable native OpenAI API key and text model")
	}
	input.Model = strings.TrimSpace(input.Model)
	input.IntervalsMS = append([]int(nil), input.IntervalsMS...)
	fingerprint := AccountRouteFingerprint(account)
	marker := make([]byte, 24)
	if _, err = rand.Read(marker); err != nil {
		return nil, err
	}
	prefix := "Cache probe " + hex.EncodeToString(marker) + ". Synthetic public reference notes.\n"
	for block := 1; block <= input.PrefixBlocks; block++ {
		prefix += fmt.Sprintf("Reference %d: The observatory keeps a daily ledger of weather, instrument calibration, and maintenance. Each entry records the local date, temperature, wind direction, cloud cover, and the name of the instrument. Reviewers compare measurements against the previous week, mark uncertain observations for another review, and preserve the original values. A missing measurement is recorded as unknown rather than zero. This synthetic reference contains no personal information.\n", block)
	}
	run := &UpstreamCacheProbeRun{Version: 1, AccountID: accountID, Settings: input, StartedAt: time.Now().UTC(), Status: "running", Verdict: "running", Reason: "collecting",
		Source: "synthetic_upstream_reported_usage", ColdBaseline: "not_observed", RouteContinuity: "not_observed", UpstreamGroup: "not_observed", ModelIdentity: "not_verified", CacheDomain: "not_verified",
		PlannedWarmRounds: len(input.IntervalsMS), Rounds: make([]UpstreamCacheProbeRound, 0, len(input.IntervalsMS)+1)}
	notify := func() {
		if changed != nil {
			changed(run)
		}
	}
	notify()
	var expectedScope *AvailabilityRequestScope
	for index := 0; index <= len(input.IntervalsMS); index++ {
		interval := 0
		if index > 0 {
			interval = input.IntervalsMS[index-1]
		}
		if err := upstreamCacheProbeWait(ctx, time.Duration(interval)*time.Millisecond); err != nil {
			run.Status, run.Reason = "stopped", "canceled"
			break
		}
		round := UpstreamCacheProbeRound{Index: index + 1, IntervalMS: interval, StartedAt: time.Now().UTC(), Status: "running", Reason: "collecting"}
		run.Rounds = append(run.Rounds, round)
		notify()
		probeCtx, cancel := context.WithTimeout(ctx, time.Duration(input.TimeoutSeconds)*time.Second)
		runtime := &upstreamCacheProbeRuntime{service: s, accountID: accountID, model: input.Model, fingerprint: fingerprint, input: input, expectedScope: expectedScope}
		probeCtx = context.WithValue(probeCtx, upstreamCacheProbeKey{}, runtime)
		observation := &availabilityProbeObservation{}
		probeCtx = context.WithValue(probeCtx, availabilityProbeSnapshotKey{}, observation)
		probeCtx = context.WithValue(probeCtx, pelicanTestUsageKey{}, &pelicanTestUsageCollector{})
		probeCtx = withPelicanTestOptions(probeCtx, pelicanTestOptions{observeOnly: true})
		recorder := &pelicanRecorder{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}
		c, _ := gin.CreateTestContext(recorder)
		c.Request = (&http.Request{Header: make(http.Header)}).WithContext(probeCtx)
		prompt := prefix + fmt.Sprintf("\nRound %d. Reply with only CACHE_OK. Do not summarize the notes.", index+1)
		testErr := s.TestPelicanAccountConnection(c, accountID, input.Model, prompt, input.ReasoningEffort)
		finished := time.Now().UTC()
		output, message := parsePelicanOutput(recorder.Body.String())
		probe := availabilityProbeResult(round.StartedAt, finished, observation, testErr == nil && message == "" && strings.TrimSpace(output) != "", recorder.overflow, probeCtx.Err())
		round.FinishedAt, round.Scope = &finished, probe.Scope
		round.Status, round.Reason = "unknown", "usage_not_observed"
		if observation.snapshot != nil {
			snapshot := *observation.snapshot
			content := requesttiming.DeriveContentObservation(snapshot)
			round.Cache = &content.Cache
			round.FirstTextMS = content.VisibleTextFirstMS
			round.DurationMS = &snapshot.TotalMS
			for _, attempt := range snapshot.Attempts {
				if attempt.Kind == "http" && attempt.OutputUsage != nil && attempt.OutputUsage.Status == "observed" && (attempt.OutputUsage.Protocol == "" || attempt.OutputUsage.Final) {
					value := attempt.OutputUsage.OutputTokens
					round.OutputTokens = &value
				}
			}
		}
		denial := monitoringProbeDeferral(testErr, finished)
		if denial == nil {
			denial = monitoringProbeDeferral(monitoringProbeLocalError(c.Request.Context()), finished)
		}
		switch {
		case ctx.Err() != nil:
			round.Status, round.Reason = "stopped", "canceled"
		case errors.Is(probeCtx.Err(), context.DeadlineExceeded):
			round.Status, round.Reason = "failed", "timeout"
		case runtime.reason != "":
			round.Status, round.Reason = "stopped", runtime.reason
		case denial != nil:
			round.Status, round.Reason = "deferred", "budget_deferred"
			run.BudgetDeferral = denial
		case testErr != nil || probe.Status != "pass":
			round.Status, round.Reason = "failed", probe.Reason
		case probe.AttemptCount != 1 || runtime.physical.Load() != 1 || probe.route == nil || probe.route.Fingerprint != hex.EncodeToString(fingerprint[:]) || round.Scope == nil:
			round.Reason = "scope_or_identity_unconfirmed"
		case expectedScope != nil && *expectedScope != *round.Scope:
			round.Reason = "scope_changed"
		case round.Cache == nil || round.Cache.Status != "observed" || round.Cache.ReadRatio == nil:
			round.Reason = "usage_not_observed"
		default:
			round.Status, round.Reason = "completed", "usage_observed"
			run.RouteContinuity = "same_configured_route"
			if expectedScope == nil {
				copy := *round.Scope
				expectedScope = &copy
			}
		}
		cancel()
		run.Rounds[index] = round
		if index > 0 && round.Status == "completed" {
			run.ObservedWarmRounds++
		}
		notify()
		if round.Status != "completed" {
			run.Status, run.Reason = round.Status, round.Reason
			break
		}
	}
	finished := time.Now().UTC()
	run.FinishedAt, run.Verdict = &finished, "insufficient"
	if len(run.Rounds) > 0 && run.Rounds[0].Status == "completed" && run.Rounds[0].Cache.ReadTokens != nil {
		run.ColdBaseline = "zero_read_observed"
		if *run.Rounds[0].Cache.ReadTokens != 0 {
			run.ColdBaseline = "cold_not_confirmed"
		}
	}
	hits := 0
	run.ObservedWarmRounds = 0
	var inputTokens, readTokens int64
	for index, round := range run.Rounds {
		if index == 0 {
			continue
		}
		if round.Status == "completed" {
			run.ObservedWarmRounds++
			inputTokens += *round.Cache.InputTokens
			readTokens += *round.Cache.ReadTokens
			if *round.Cache.Hit {
				hits++
			}
		}
	}
	if run.Status == "running" && len(run.Rounds) == len(input.IntervalsMS)+1 && run.ObservedWarmRounds == len(input.IntervalsMS) {
		run.Status, run.Reason, run.Verdict = "completed", "completed_observations", "no_hit_observed"
		if hits > 0 {
			run.Verdict = "hit_observed"
		}
		requestRate, tokenRate := float64(hits)/float64(run.ObservedWarmRounds), float64(readTokens)/float64(inputTokens)
		run.RequestHitRate, run.TokenHitRate = &requestRate, &tokenRate
	}
	notify()
	return run, nil
}

func upstreamCacheProbeWait(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if delay == 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func observeUpstreamCacheProbeEvent(ctx context.Context, data []byte, chat bool) {
	if runtime, _ := ctx.Value(upstreamCacheProbeKey{}).(*upstreamCacheProbeRuntime); runtime != nil {
		if chat {
			requesttiming.ChatContent(ctx, data, true)
		} else {
			if requesttiming.Content(ctx, data) {
				requesttiming.Output(ctx, false, false, timingTerminal(gjson.GetBytes(data, "type").String()))
			}
		}
	}
}
