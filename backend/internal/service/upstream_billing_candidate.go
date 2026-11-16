package service

import (
	"context"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

type upstreamBillingCandidateKey struct{}

type upstreamBillingCandidate struct {
	scope       *AvailabilityRequestScope
	fastPolicy  *OpenAIFastPolicySettings
	policyKnown bool
}

// WithUpstreamBillingCandidateRequest records prompt-free ingress metadata
// after channel mapping. Only native HTTP Responses passthrough is predictable
// here; conversions and WebSocket turns keep their original final-send checks.
// Unknown metadata/policy defers scope-specific filtering, never qualifies
// supply. Declaration expiry, validity and binding still apply to every route.
func (s *OpenAIGatewayService) WithUpstreamBillingCandidateRequest(ctx context.Context, request *http.Request, body []byte) context.Context {
	candidate := upstreamBillingCandidate{}
	group, _ := ctx.Value(ctxkey.Group).(*Group)
	_, suppressed := ctx.Value(openAIProfitControlSuppressCtxKey{}).(struct{})
	// Composite members install their own gate during selection. Capture their
	// descriptor now, but leave confirmed-disabled ordinary groups untouched.
	mayFilter := gatewayProfitControlGateActive(ctx) || IsGroupContextValid(group) && group.Platform == PlatformComposite
	if !suppressed && mayFilter && request != nil && !isOpenAIResponsesLiteHeader(request.Header.Get(responsesLiteHeader)) {
		candidate.scope = availabilityScopeForRequest(request, body)
		if candidate.scope != nil && candidate.scope.Protocol != "responses_http" {
			candidate.scope = nil
		}
	}
	if candidate.scope != nil {
		candidate.policyKnown = s == nil || s.settingService == nil
		if !candidate.policyKnown {
			candidate.fastPolicy = openAIFastPolicySettingsFromContext(ctx)
			candidate.policyKnown = candidate.fastPolicy != nil
		}
		if !candidate.policyKnown {
			settings, err := s.settingService.GetOpenAIFastPolicySettings(ctx)
			if err == nil && settings != nil {
				candidate.fastPolicy, candidate.policyKnown = settings, true
				// Selection and forwarding share the same request policy snapshot.
				ctx = withOpenAIFastPolicyContext(ctx, settings)
			}
		}
	}
	// Also overwrite a previous descriptor when the request shape is unknown.
	return context.WithValue(ctx, upstreamBillingCandidateKey{}, candidate)
}

func upstreamBillingCandidateVetoReason(ctx context.Context, account *Account, at time.Time) string {
	if !upstreamBillingHasApplicability(account) {
		return "" // Legacy compatibility is not strict qualification.
	}
	candidate, _ := ctx.Value(upstreamBillingCandidateKey{}).(upstreamBillingCandidate)
	var scope *AvailabilityRequestScope
	if candidate.scope != nil && candidate.policyKnown && account.IsOpenAIApiKey() &&
		account.IsOpenAIPassthroughEnabled() && !account.IsCopilotSDKEnabled() &&
		!account.HasManagedOpenAIConnections() && !account.IsShadow() &&
		!account.routesByModel() && !account.IsAdaptiveAPIProtocol() && !account.UsesNativeCNResponses() &&
		controlledMode(ctx) == nil && resolveUpstreamProtocol(account, APIProtocolResponses, "", nil) == APIProtocolResponses {
		// Passthrough preserves the channel-mapped body model. Applying the
		// account mapping here would invent a model this path never sends.
		request, body := billingApplicabilityScopeRequest(*candidate.scope)
		body, err := filterOpenAIResponsesNoneReasoningEffortForAccount(account, body)
		if err != nil {
			return ""
		}
		body, err = applyOpenAIFastPolicyBody(ctx, account, candidate.scope.Model, body, func(tier string) (string, string) {
			return evaluateOpenAIFastPolicyWithSettings(candidate.fastPolicy, openAIFastPolicyUserID(ctx), account, candidate.scope.Model, tier)
		})
		if err != nil {
			// Preserve the original policy 403 instead of reporting an empty pool.
			return ""
		}
		scope = availabilityScopeForRequest(request, body)
	}
	status, reason := upstreamBillingScopeVerdictAt(account, scope, at)
	if scope != nil || status != "unknown" && status != "not_declared" {
		// Invalid, expired or binding-changed declarations are unusable for
		// every scope, including routes whose final shape is not predictable.
		return reason
	}
	return ""
}
