package service

import (
	"context"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"slices"
	"strings"
	"time"
)

type observationEffortKey struct{}

func WithObservationEffort(ctx context.Context, effort string) context.Context {
	return context.WithValue(ctx, observationEffortKey{}, requesttiming.NormalizeObservationEffort(effort))
}
func observationEffort(ctx context.Context) (string, bool) {
	value, ok := ctx.Value(observationEffortKey{}).(string)
	return value, ok
}
func WithObservationRequestEffort(ctx context.Context, body []byte) context.Context {
	value := ""
	if effort := CanonicalRequestedReasoningEffort(body); effort != nil {
		value = *effort
	}
	return WithObservationEffort(ctx, value)
}

// Observation identity is captured before channel routing. It must never drive
// upstream model support, account mapping, pricing, or response ownership.
type priorityObservationContextKey struct{}
type priorityObservationIdentity struct {
	model, endpoint string
	groupID         int64
	hasGroup        bool
}

func WithPrioritySchedulingObservation(ctx context.Context, group *int64, publicModel, endpoint string) context.Context {
	identity := priorityObservationIdentity{model: strings.TrimSpace(publicModel), endpoint: strings.TrimSpace(endpoint)}
	if group != nil {
		identity.groupID, identity.hasGroup = *group, true
	}
	return context.WithValue(ctx, priorityObservationContextKey{}, identity)
}

func priorityHistoryModel(req OpenAIAccountScheduleRequest) string {
	if model := strings.TrimSpace(req.HistoryModel); model != "" {
		return model
	}
	return strings.TrimSpace(req.RequestedModel)
}

// Public model scopes work before routing; retain legacy routing-model scopes
// as a configuration alias only. Each history/attempt key remains exact.
func priorityApplies(c PrioritySchedulingConfig, req OpenAIAccountScheduleRequest) bool {
	return c.applies(req.GroupID, priorityHistoryModel(req)) || c.applies(req.GroupID, strings.TrimSpace(req.RequestedModel))
}

func priorityObservationFromContext(ctx context.Context, fallback string) priorityObservationIdentity {
	identity, _ := ctx.Value(priorityObservationContextKey{}).(priorityObservationIdentity)
	if identity.model == "" {
		identity.model = strings.TrimSpace(fallback)
		if public, ok := RequestedPublicModelFromContext(ctx); ok {
			identity.model = strings.TrimSpace(public)
		}
	}
	return identity
}

// Only immutable routing identity is retained by diagnostic snapshots. Managed
// grants are distinct from their parent and from sibling grants, including when
// their projected routing settings happen to produce the same fingerprint.
type priorityObservationRoute struct {
	fingerprint [32]byte
	connection  string
	managed     bool
}

func priorityAccountObservationRoute(account *Account) priorityObservationRoute {
	if account == nil {
		return priorityObservationRoute{}
	}
	return priorityObservationRoute{fingerprint: AccountRouteFingerprint(account), connection: account.AuthConnectionID,
		managed: account.AuthConnectionID != "" || account.HasManagedOpenAIConnections()}
}

func priorityCandidateObservationRoute(item openAIAccountCandidateScore) priorityObservationRoute {
	if item.observationRoute != nil {
		return *item.observationRoute
	}
	return priorityAccountObservationRoute(item.account)
}

func (r priorityObservationRoute) historyFingerprint() string {
	// Persisted HTTP attempts do not prove which managed grant was used. Parent
	// routing settings must never pool otherwise indistinguishable sibling grants.
	if r.managed || r.fingerprint == [32]byte{} {
		return ""
	}
	return fmt.Sprintf("%x", r.fingerprint)
}

func priorityRequestAttemptKey(req OpenAIAccountScheduleRequest, account *Account) priorityAttemptKey {
	key := priorityAttemptKey{model: priorityHistoryModel(req), endpoint: priorityErrorEndpoint(req), effort: req.ObservationEffort,
		route: priorityAccountObservationRoute(account)}
	if account != nil {
		key.accountID = account.ID
	}
	if req.GroupID != nil {
		key.groupID, key.hasGroup = *req.GroupID, true
	}
	return key
}

// A miss schedules a bounded asynchronous read, so sessions consisting only of
// sticky requests also acquire evidence. Missing evidence never means slow.
func (s *defaultOpenAIAccountScheduler) shouldYieldPriorityLatencySticky(req OpenAIAccountScheduleRequest, account *Account) bool {
	if s == nil || s.service == nil || account == nil || req.DisableStickyEscape || req.PreserveStickyBinding || req.EncryptedReasoning || req.GuardianParentAccountID > 0 || strings.TrimSpace(req.PreviousResponseID) != "" || req.Platform != PlatformOpenAI || !req.UseUpstreamTokenCost || req.RequiredImageCapability != "" {
		return false
	}
	if s.service.preservesPrioritySession(req) {
		return false
	}
	cfg := s.service.prioritySchedulingRuntimeConfig()
	if !priorityApplies(cfg, req) {
		return false
	}
	pool := []openAIAccountCandidateScore{{account: account}}
	history := s.service.priorityHistory(req, cfg, pool)
	signal, known := history.signals[account.ID]
	if !known {
		return false
	}
	if req.priorityLatencyPool != nil && req.priorityLatencyPool.fallbackActive() && s.service.priorityLatencyAdmission(req, account) == nil {
		return false
	}
	score := scorePriorityCandidate(cfg, pool[0], signal, time.Now())
	return slices.Contains(score.Reasons, "latency_above_target")
}

// Preserve affinity while migration benefit and cache-rebuild cost are unknown.
// This is not an ownership or eligibility grant: all hard gates and the existing
// bounded concurrency wait remain authoritative.
func (s *OpenAIGatewayService) preservesPrioritySession(req OpenAIAccountScheduleRequest) bool {
	if s == nil || req.Platform != PlatformOpenAI || !req.UseUpstreamTokenCost || req.RequiredImageCapability != "" {
		return false
	}
	cfg := s.prioritySchedulingRuntimeConfig()
	return cfg.PreserveSessionAffinity && priorityApplies(cfg, req)
}
