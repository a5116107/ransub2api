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
	"github.com/tidwall/sjson"
)

type upstreamBillingCandidateKey struct{}

type upstreamBillingCandidate struct {
	scope       *AvailabilityRequestScope
	fastPolicy  *OpenAIFastPolicySettings
	policyKnown bool
	chatHasText bool
	gateway     *OpenAIGatewayService
}

func upstreamBillingCandidateMayFilter(ctx context.Context) bool {
	group, _ := ctx.Value(ctxkey.Group).(*Group)
	_, suppressed := ctx.Value(openAIProfitControlSuppressCtxKey{}).(struct{})
	// Composite members install their own gate during selection.
	return !suppressed && (gatewayProfitControlGateActive(ctx) || IsGroupContextValid(group) && group.Platform == PlatformComposite)
}

// WithUpstreamBillingCandidateRequest records prompt-free ingress metadata
// after channel mapping. Predictable native HTTP Responses and fixed Responses
// to Chat text conversions share the original wire transformers. All routes
// keep their original final-send checks.
// Unknown metadata/policy defers scope-specific filtering, never qualifies
// supply. Declaration expiry, validity and binding still apply to every route.
func (s *OpenAIGatewayService) WithUpstreamBillingCandidateRequest(ctx context.Context, request *http.Request, body []byte) context.Context {
	candidate := upstreamBillingCandidate{}
	if upstreamBillingCandidateMayFilter(ctx) && request != nil && !isOpenAIResponsesLiteHeader(request.Header.Get(responsesLiteHeader)) {
		candidate.scope = availabilityScopeForRequest(request, body)
		if candidate.scope != nil && candidate.scope.Protocol != "responses_http" {
			candidate.scope = nil
		}
	}
	if candidate.scope != nil {
		input := gjson.GetBytes(body, "input")
		candidate.chatHasText = input.Type == gjson.String || input.IsArray() && input.Get("#").Int() > 0 ||
			strings.TrimSpace(gjson.GetBytes(body, "instructions").String()) != ""
	}
	return s.withUpstreamBillingCandidatePolicy(ctx, candidate)
}

// WithUpstreamBillingWSCandidateRequest describes only first-frame selection.
// Reasoning policy uses the client model before the handler's channel mapping.
// Original payload size determines bridge dispatch; no payload or hooks survive
// in the descriptor. Dynamic transports and unsupported shapes defer to the
// actual send guard, without granting strict qualification.
func (s *OpenAIGatewayService) WithUpstreamBillingWSCandidateRequest(ctx context.Context, request *http.Request, body []byte, forwardModel string, hooks *OpenAIWSIngressHooks) context.Context {
	candidate := upstreamBillingCandidate{}
	if upstreamBillingCandidateMayFilter(ctx) && s != nil && s.cfg != nil &&
		s.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled && !s.cfg.Gateway.OpenAIWS.SmartProtocol.Enabled &&
		request != nil && request.URL != nil && request.Method == http.MethodGet &&
		(strings.EqualFold(request.Header.Get("Upgrade"), "websocket") || request.URL.Scheme == "ws" || request.URL.Scheme == "wss") &&
		!isOpenAIResponsesLiteHeader(request.Header.Get(responsesLiteHeader)) && !isOpenAIResponsesLiteWebSocketPayload(body) &&
		!s.shouldBridgeOpenAIWSPassthroughFirstMessage(nil, body) {
		// Server-side upgrade requests normally have an http(s) URL. Keep the
		// original path, query and header validation when describing WS ingress.
		wsRequest := *request
		wsURL := *request.URL
		wsURL.Scheme = "wss"
		wsRequest.URL = &wsURL
		if scope := availabilityScopeForRequest(&wsRequest, body); scope != nil && scope.Protocol == "responses_ws_v2" {
			if normalized, err := applyOpenAIWSReasoningEffortPolicy(body, hooks, hooks.forcedReasoningEffort()); err == nil {
				if forwardModel = strings.TrimSpace(forwardModel); forwardModel != "" {
					normalized = s.ReplaceModelInBody(normalized, forwardModel)
				}
				candidate.scope = availabilityScopeForRequest(&wsRequest, normalized)
			}
		}
	}
	return s.withUpstreamBillingCandidatePolicy(ctx, candidate)
}

func (s *OpenAIGatewayService) withUpstreamBillingCandidatePolicy(ctx context.Context, candidate upstreamBillingCandidate) context.Context {
	if candidate.scope != nil {
		candidate.gateway = s
		// Parsed strings can be slices of the complete input JSON. Detach the
		// supported Responses fields before a long-lived context retains them.
		candidate.scope.Model = strings.Clone(candidate.scope.Model)
		candidate.scope.ServiceTier = strings.Clone(candidate.scope.ServiceTier)
		candidate.scope.ReasoningEffort = strings.Clone(candidate.scope.ReasoningEffort)
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

// WithoutUpstreamBillingCandidateRequest ends the selection-only descriptor's
// lifetime before connection hooks, turn admission and physical WS sends. Those
// checks must use the current frame, never the first frame's predicted scope.
func WithoutUpstreamBillingCandidateRequest(ctx context.Context) context.Context {
	if _, ok := ctx.Value(upstreamBillingCandidateKey{}).(upstreamBillingCandidate); !ok {
		return ctx
	}
	return context.WithValue(ctx, upstreamBillingCandidateKey{}, upstreamBillingCandidate{})
}

func (s *OpenAIGatewayService) upstreamBillingWSCandidateScope(ctx context.Context, account *Account, scope AvailabilityRequestScope) *AvailabilityRequestScope {
	if s == nil || s.cfg == nil ||
		account.ResolveOpenAIResponsesWebSocketV2Mode(s.cfg.Gateway.OpenAIWS.IngressModeDefault) != OpenAIWSIngressModePassthrough ||
		s.getOpenAIWSProtocolResolver().Resolve(account).Transport != OpenAIUpstreamTransportResponsesWebsocketV2 {
		return nil
	}
	request, body := billingApplicabilityScopeRequest(scope)
	// Passthrough sends the already channel-mapped model, without applying
	// ordinary account model_mapping or the non-passthrough none filter.
	model := openAIWSPassthroughPolicyModelForFrame(account, body)
	body, _, err := normalizeOpenAIResponsesWebSocketCompatibilityBodyWithConfig(s.cfg, body, account, false)
	if err != nil {
		return nil
	}
	body, blocked, err := s.applyOpenAIFastPolicyToWSResponseCreate(ctx, account, model, body)
	if err != nil || blocked != nil {
		return nil // Preserve the original policy error and close semantics.
	}
	return availabilityScopeForRequest(request, body)
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
		if candidate.scope.Protocol == "responses_ws_v2" {
			scope = candidate.gateway.upstreamBillingWSCandidateScope(ctx, account, *candidate.scope)
		} else {
			protocol := resolveUpstreamProtocol(account, APIProtocolResponses, "", nil)
			nativeHTTP := protocol == APIProtocolResponses &&
				candidate.gateway.getOpenAIWSProtocolResolver().Resolve(account).Transport == OpenAIUpstreamTransportHTTPSSE
			nativeModel := candidate.scope.Model
			if nativeHTTP && !account.IsOpenAIPassthroughEnabled() {
				_, nativeModel = resolveOpenAIForwardMappedModels(account, nativeModel, false)
				nativeHTTP = !isOpenAIImageGenerationModel(nativeModel)
			}
			predictable := protocol == APIProtocolResponses && (account.IsOpenAIPassthroughEnabled() || nativeHTTP) ||
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
				} else if !account.IsOpenAIPassthroughEnabled() {
					model = nativeModel
					body = ReplaceModelInBody(body, model)
					effort := gjson.GetBytes(body, "reasoning.effort").String()
					if normalized := normalizeOpenAIResponsesMinimalEffort(effort); normalized != effort {
						body, err = sjson.SetBytes(body, "reasoning.effort", normalized)
					}
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
	}
	status, reason := upstreamBillingScopeVerdictAt(account, scope, at)
	if scope != nil || status != "unknown" && status != "not_declared" {
		// Invalid, expired or binding-changed declarations are unusable for
		// every scope, including routes whose final shape is not predictable.
		return reason
	}
	return ""
}
