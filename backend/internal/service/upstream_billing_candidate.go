package service

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/tidwall/gjson"
)

type upstreamBillingCandidateKey struct{}

type upstreamBillingCandidate struct {
	scope       *AvailabilityRequestScope
	fastPolicy  *OpenAIFastPolicySettings
	policyKnown bool
	chatHasText bool
}

// WithUpstreamBillingCandidateRequest records prompt-free ingress metadata
// after channel mapping. Native HTTP Responses passthrough and fixed Responses
// to Chat text conversions share the original wire transformers. Other routes
// and WebSocket turns keep their original final-send checks.
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
		input := gjson.GetBytes(body, "input")
		candidate.chatHasText = input.Type == gjson.String || input.IsArray() && input.Get("#").Int() > 0 ||
			strings.TrimSpace(gjson.GetBytes(body, "instructions").String()) != ""
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
		!account.IsCopilotSDKEnabled() &&
		!account.HasManagedOpenAIConnections() && !account.IsShadow() &&
		!account.routesByModel() && !account.IsAdaptiveAPIProtocol() && !account.UsesNativeCNResponses() &&
		controlledMode(ctx) == nil {
		protocol := resolveUpstreamProtocol(account, APIProtocolResponses, "", nil)
		predictable := protocol == APIProtocolResponses && account.IsOpenAIPassthroughEnabled() ||
			protocol == APIProtocolChatCompletions && candidate.chatHasText
		if predictable {
			request, body := billingApplicabilityScopeRequest(*candidate.scope)
			model := candidate.scope.Model
			// Forward applies this before choosing native or converted output;
			// compatible non-passthrough drops none, passthrough preserves it.
			body, err := filterOpenAIResponsesNoneReasoningEffortForAccount(account, body)
			if err != nil {
				return ""
			}
			if protocol == APIProtocolChatCompletions {
				var responses apicompat.ResponsesRequest
				if json.Unmarshal(body, &responses) != nil {
					return ""
				}
				// Only the existence of validated text was retained, never its
				// contents. The same converter carries scope options to Chat.
				responses.Input = json.RawMessage(`""`)
				chat, convertErr := apicompat.ResponsesToChatCompletionsRequest(&responses)
				if convertErr != nil {
					return ""
				}
				model = normalizeOpenAIModelForUpstream(account, resolveOpenAIForwardModel(account, model, ""))
				chat.Model = model
				body, err = json.Marshal(chat)
				request.URL.Path = "/v1/chat/completions"
			}
			if err != nil {
				return ""
			}
			body, err = applyOpenAIFastPolicyBody(ctx, account, model, body, func(tier string) (string, string) {
				return evaluateOpenAIFastPolicyWithSettings(candidate.fastPolicy, openAIFastPolicyUserID(ctx), account, model, tier)
			})
			if err != nil {
				// Preserve the original policy 403 instead of reporting an empty pool.
				return ""
			}
			scope = availabilityScopeForRequest(request, body)
		}
	}
	status, reason := upstreamBillingScopeVerdictAt(account, scope, at)
	if scope != nil || status != "unknown" && status != "not_declared" {
		// Invalid, expired or binding-changed declarations are unusable for
		// every scope, including routes whose final shape is not predictable.
		return reason
	}
	return ""
}
