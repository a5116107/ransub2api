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
	scope                 *AvailabilityRequestScope
	fastPolicy            *OpenAIFastPolicySettings
	policyKnown           bool
	chatHasText           bool
	chatHasAssistant      bool
	messagesDispatchModel string
	wsPassthroughBridge   bool
	wsSmartRequest        *openAISmartProtocolRequest
	gateway               *OpenAIGatewayService
}

func upstreamBillingCandidateMayFilter(ctx context.Context) bool {
	group, _ := ctx.Value(ctxkey.Group).(*Group)
	_, suppressed := ctx.Value(openAIProfitControlSuppressCtxKey{}).(struct{})
	// Composite members install their own gate during selection.
	return !suppressed && (gatewayProfitControlGateActive(ctx) || IsGroupContextValid(group) && group.Platform == PlatformComposite)
}

// WithUpstreamBillingCandidateRequest records prompt-free ingress metadata
// after channel mapping. Predictable native HTTP Responses/Chat and fixed
// text conversions share the original wire transformers and final-send checks.
// Messages accepts only its explicit dispatch fallback, not the scheduling
// context's routing model, which can differ from the forwarded body.
// Unknown metadata/policy defers scope-specific filtering, never qualifies
// supply. Declaration expiry, validity and binding still apply to every route.
func (s *OpenAIGatewayService) WithUpstreamBillingCandidateRequest(ctx context.Context, request *http.Request, body []byte, messagesDispatchModel ...string) context.Context {
	candidate := upstreamBillingCandidate{}
	if upstreamBillingCandidateMayFilter(ctx) && request != nil && !isOpenAIResponsesLiteHeader(request.Header.Get(responsesLiteHeader)) {
		candidate.scope = availabilityScopeForRequest(request, body)
		if candidate.scope != nil && candidate.scope.Protocol != "responses_http" && candidate.scope.Protocol != "chat_http" && candidate.scope.Protocol != "messages_http" {
			candidate.scope = nil
		}
	}
	if candidate.scope != nil {
		input := gjson.GetBytes(body, "input")
		candidate.chatHasText = input.Type == gjson.String || input.IsArray() && input.Get("#").Int() > 0 ||
			strings.TrimSpace(gjson.GetBytes(body, "instructions").String()) != ""
		conversation := input
		if candidate.scope.Protocol == "chat_http" || candidate.scope.Protocol == "messages_http" {
			conversation = gjson.GetBytes(body, "messages")
		}
		if candidate.scope.Protocol == "messages_http" && len(messagesDispatchModel) > 0 {
			candidate.messagesDispatchModel = strings.Clone(strings.TrimSpace(messagesDispatchModel[0]))
		}
		conversation.ForEach(func(_, message gjson.Result) bool {
			candidate.chatHasAssistant = message.Get("role").String() == "assistant"
			return !candidate.chatHasAssistant
		})
	}
	return s.withUpstreamBillingCandidatePolicy(ctx, candidate)
}

// WithUpstreamBillingWSCandidateRequest describes only first-frame selection.
// Reasoning policy uses the client model before the handler's channel mapping.
// No payload or hooks survive in the descriptor. Passthrough bridge dispatch
// uses original payload size, captured only as a routing boolean. Fixed HTTP
// bridge and native modes share their original wire transformers and smart
// dispatcher. Pooled normalized-size decisions retain the final-send guard.
func (s *OpenAIGatewayService) WithUpstreamBillingWSCandidateRequest(ctx context.Context, request *http.Request, body []byte, forwardModel string, hooks *OpenAIWSIngressHooks) context.Context {
	candidate := upstreamBillingCandidate{}
	if upstreamBillingCandidateMayFilter(ctx) && s != nil && s.cfg != nil &&
		s.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled &&
		request != nil && request.URL != nil && request.Method == http.MethodGet &&
		(strings.EqualFold(request.Header.Get("Upgrade"), "websocket") || request.URL.Scheme == "ws" || request.URL.Scheme == "wss") &&
		!isOpenAIResponsesLiteHeader(request.Header.Get(responsesLiteHeader)) && !isOpenAIResponsesLiteWebSocketPayload(body) {
		// Server-side upgrade requests normally have an http(s) URL. Keep the
		// original path, query and header validation when describing WS ingress.
		wsRequest := *request
		wsURL := *request.URL
		wsURL.Scheme = "wss"
		wsRequest.URL = &wsURL
		if scope := availabilityScopeForRequest(&wsRequest, body); scope != nil && scope.Protocol == "responses_ws_v2" {
			candidate.wsPassthroughBridge = s.shouldBridgeOpenAIWSPassthroughFirstMessage(nil, body)
			if s.cfg.Gateway.OpenAIWS.SmartProtocol.Enabled {
				smartRequest := parseOpenAISmartProtocolRequest(body)
				candidate.wsSmartRequest = &smartRequest
			}
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
		// supported scope fields before a long-lived context retains them.
		candidate.scope.Model = strings.Clone(candidate.scope.Model)
		candidate.scope.ServiceTier = strings.Clone(candidate.scope.ServiceTier)
		candidate.scope.ReasoningEffort = strings.Clone(candidate.scope.ReasoningEffort)
		candidate.scope.ThinkingMode = strings.Clone(candidate.scope.ThinkingMode)
		candidate.scope.ProtocolOptions = strings.Clone(candidate.scope.ProtocolOptions)
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

func (s *OpenAIGatewayService) upstreamBillingWSCandidateScope(ctx context.Context, account *Account, scope AvailabilityRequestScope, passthroughBridge bool, smartRequest *openAISmartProtocolRequest) *AvailabilityRequestScope {
	if s == nil || s.cfg == nil {
		return nil
	}
	mode := account.ResolveOpenAIResponsesWebSocketV2Mode(s.cfg.Gateway.OpenAIWS.IngressModeDefault)
	httpBridge := mode == OpenAIWSIngressModeHTTPBridge
	// Passthrough validates native WS transport before its first-frame bridge
	// override. Explicit HTTP bridge mode has no such native prerequisite.
	if !httpBridge && s.getOpenAIWSProtocolResolver().Resolve(account).Transport != OpenAIUpstreamTransportResponsesWebsocketV2 {
		return nil
	}
	smartHTTP := false
	if s.cfg.Gateway.OpenAIWS.SmartProtocol.Enabled && !httpBridge && !(mode == OpenAIWSIngressModePassthrough && passthroughBridge) {
		if smartRequest == nil {
			return nil
		}
		// Reuse the dispatch algorithm with original routing scalars and current
		// account/history. Policy-rewritten scope is only for the final wire.
		decision := s.decideOpenAIWSUpstreamProtocolForRequest(account, *smartRequest)
		smartHTTP = decision.Mode != "shadow" && decision.Protocol == "http"
	}
	switch mode {
	case OpenAIWSIngressModePassthrough:
		httpBridge = passthroughBridge || smartHTTP
	case OpenAIWSIngressModeHTTPBridge:
	case OpenAIWSIngressModeCtxPool, OpenAIWSIngressModeShared, OpenAIWSIngressModeDedicated:
		// Native dispatch compares the fully normalized payload size, not the
		// original frame or this prompt-free skeleton. Never guess that size.
		if s.openAIWSHTTPBridgeEnabled() && !smartHTTP {
			return nil
		}
		httpBridge = smartHTTP
	default:
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
	if mode != OpenAIWSIngressModePassthrough || httpBridge {
		// Native ingress applies channel mapping first, then account mapping
		// after compatibility. It does not use HTTP's minimal/none rewrites.
		model = normalizeOpenAIModelForUpstream(account, account.GetMappedModel(scope.Model))
		if isOpenAIImageGenerationModel(model) {
			return nil
		}
		body = ReplaceModelInBody(body, model)
	}
	body, blocked, err := s.applyOpenAIFastPolicyToWSResponseCreate(ctx, account, model, body)
	if err != nil || blocked != nil {
		return nil // Preserve the original policy error and close semantics.
	}
	if httpBridge {
		body, err = prepareOpenAIWSHTTPBridgeSessionBody(account, body, false)
		if err != nil {
			return nil
		}
		request.Method, request.URL.Scheme = http.MethodPost, "https"
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
			scope = candidate.gateway.upstreamBillingWSCandidateScope(ctx, account, *candidate.scope, candidate.wsPassthroughBridge, candidate.wsSmartRequest)
		} else if candidate.scope.Protocol == "messages_http" {
			if resolveUpstreamProtocol(account, APIProtocolAnthropic, "", nil) == APIProtocolChatCompletions && candidate.gateway != nil {
				// Reuse the direct Messages converter with detached options and
				// synthetic text. No prompt, metadata or header is retained here.
				anthropic := apicompat.AnthropicRequest{
					Model: candidate.scope.Model, Stream: candidate.scope.Stream,
					Messages: []apicompat.AnthropicMessage{{Role: "user", Content: json.RawMessage(`""`)}},
				}
				if candidate.scope.ThinkingMode != "" {
					anthropic.Thinking = &apicompat.AnthropicThinking{Type: candidate.scope.ThinkingMode, BudgetTokens: int(candidate.scope.ThinkingBudget)}
				}
				if candidate.scope.ReasoningEffort != "unspecified" {
					anthropic.OutputConfig = &apicompat.AnthropicOutputConfig{Effort: candidate.scope.ReasoningEffort}
				}
				applyOpenAICompatModelNormalization(&anthropic)
				chat, err := apicompat.AnthropicToChatCompletionsRequestWithMinOutputTokens(&anthropic, configuredMinOutputTokens(candidate.gateway.cfg))
				if err == nil {
					model := normalizeOpenAIModelForUpstream(account, resolveOpenAIForwardModel(account, anthropic.Model, candidate.messagesDispatchModel))
					if !isOpenAIImageGenerationModel(model) {
						chat.Model = model
						chat.ReasoningEffort = openAICompatAnthropicReasoningEffort(&anthropic, model, chat.ReasoningEffort)
						body, err := json.Marshal(chat)
						if err == nil {
							if normalized, changed := NormalizeGLMOpenAIReasoningEffort(body, model); changed {
								body = normalized
							}
							body, _, err = applyOpenAIReasoningEffortPolicyForGLMFromContext(ctx, body, model)
							if err == nil {
								// The existing direct bridge deliberately skips Fast policy
								// and drops Messages service_tier, including group forcing.
								request, _ := billingApplicabilityScopeRequest(AvailabilityRequestScope{Protocol: "chat_http", ServiceTier: "unspecified", ReasoningEffort: "unspecified"})
								scope = availabilityScopeForRequest(request, body)
							}
						}
					}
				}
			}
		} else if candidate.scope.Protocol == "chat_http" {
			protocol := resolveUpstreamProtocol(account, APIProtocolChatCompletions, "", nil)
			if protocol == APIProtocolChatCompletions || protocol == APIProtocolResponses && candidate.gateway != nil {
				request, body := billingApplicabilityScopeRequest(*candidate.scope)
				model := normalizeOpenAIModelForUpstream(account, resolveOpenAIForwardModel(account, candidate.scope.Model, ""))
				if !isOpenAIImageGenerationModel(model) {
					body = ReplaceModelInBody(body, model)
					if protocol == APIProtocolResponses {
						var chat apicompat.ChatCompletionsRequest
						if json.Unmarshal(body, &chat) != nil {
							return upstreamBillingCandidateScopeVetoReason(account, nil, at)
						}
						// The converter sees only detached scope metadata and synthetic
						// text, never the actual conversation or encrypted history.
						chat.Messages = []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`""`)}}
						responses, err := chatCompletionsToResponses(candidate.gateway.cfg, &chat)
						if err != nil {
							return upstreamBillingCandidateScopeVetoReason(account, nil, at)
						}
						normalizeResponsesRequestServiceTier(responses)
						body, err = json.Marshal(responses)
						if err != nil {
							return upstreamBillingCandidateScopeVetoReason(account, nil, at)
						}
						request.URL.Path = "/v1/responses"
					} else if normalized, changed := NormalizeGLMOpenAIReasoningEffort(body, model); changed {
						body = normalized
					}
					var err error
					body, err = applyOpenAIFastPolicyBody(ctx, account, model, body, func(tier string) (string, string) {
						return evaluateOpenAIFastPolicyWithSettings(candidate.fastPolicy, openAIFastPolicyUserID(ctx), account, model, tier)
					})
					if err == nil {
						scope = availabilityScopeForRequest(request, body)
					}
				}
			}
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
					return upstreamBillingCandidateScopeVetoReason(account, nil, at)
				}
				if protocol == APIProtocolChatCompletions {
					var responses apicompat.ResponsesRequest
					if json.Unmarshal(body, &responses) != nil {
						return upstreamBillingCandidateScopeVetoReason(account, nil, at)
					}
					// Only the existence of validated text was retained, never its
					// contents. The same converter carries scope options to Chat.
					responses.Input = json.RawMessage(`""`)
					chat, convertErr := apicompat.ResponsesToChatCompletionsRequest(&responses)
					if convertErr != nil {
						return upstreamBillingCandidateScopeVetoReason(account, nil, at)
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
					return upstreamBillingCandidateScopeVetoReason(account, nil, at)
				}
				body, err = applyOpenAIFastPolicyBody(ctx, account, model, body, func(tier string) (string, string) {
					return evaluateOpenAIFastPolicyWithSettings(candidate.fastPolicy, openAIFastPolicyUserID(ctx), account, model, tier)
				})
				if err != nil {
					// Valid declarations retain the original policy error. A failed
					// prediction never bypasses expiry or binding validity checks.
					return upstreamBillingCandidateScopeVetoReason(account, nil, at)
				}
				scope = availabilityScopeForRequest(request, body)
			}
		}
	}
	if scope != nil && scope.Protocol == "chat_http" && candidate.chatHasAssistant {
		_, body := billingApplicabilityScopeRequest(*scope)
		if requiresDeepSeekChatReasoning(account, body) {
			// The actual CC pipeline adds reasoning_content to assistant history.
			// Do not attest that hidden-history wire shape with a plain-text scope.
			scope = nil
		}
	}
	return upstreamBillingCandidateScopeVetoReason(account, scope, at)
}

func upstreamBillingCandidateScopeVetoReason(account *Account, scope *AvailabilityRequestScope, at time.Time) string {
	status, reason := upstreamBillingScopeVerdictAt(account, scope, at)
	if scope != nil || status != "unknown" && status != "not_declared" {
		// Invalid, expired or binding-changed declarations are unusable for
		// every scope, including routes whose final shape is not predictable.
		return reason
	}
	return ""
}
