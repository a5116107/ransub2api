package service

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

func (s *OpenAIGatewayService) SetPluginManager(manager *PluginManager) {
	s.pluginManager = manager
}

// upstreamBillingRequestTargetConfirmed keeps an account-level declaration
// within the API base actually used by the billing probe. A different protocol
// base is not evidence of the same price, even on the same host.
func upstreamBillingRequestTargetConfirmed(account *Account, request *http.Request) bool {
	if account == nil || request == nil || request.URL == nil || request.Method != http.MethodPost {
		return false
	}
	actual := request.URL
	if actual.User != nil || actual.Opaque != "" || actual.Fragment != "" || actual.ForceQuery ||
		(request.Host != "" && !strings.EqualFold(request.Host, actual.Host)) {
		return false
	}
	base := upstreamBillingProbeBaseURL(account)
	if base == "" && account.Platform == PlatformOpenAI {
		base = "https://api.openai.com"
	}
	// Derive the namespace from the exact probe builder, rather than guessing
	// that protocol-specific builders preserve the same tenant/version root.
	probe, err := url.Parse(buildOpenAIEndpointURL(base, "/v1/sub2api/billing"))
	if err != nil || (probe.Scheme != "http" && probe.Scheme != "https") || probe.Host == "" || probe.User != nil ||
		!strings.EqualFold(actual.Scheme, probe.Scheme) || !strings.EqualFold(actual.Host, probe.Host) || actual.RawQuery != probe.RawQuery {
		return false
	}
	root := strings.TrimSuffix(probe.EscapedPath(), "/sub2api/billing")
	switch actual.EscapedPath() {
	case root + "/chat/completions", root + "/responses", root + "/responses/compact", root + "/messages":
		return true
	default:
		return false
	}
}

// Only non-secret routing selectors are retained with an established socket.
type openAIWSBillingHandshake struct {
	target            string
	headers           http.Header
	redirectsDisabled bool
}

func newOpenAIWSBillingHandshake(ctx context.Context, target string, headers http.Header) *openAIWSBillingHandshake {
	b := &openAIWSBillingHandshake{target: target, headers: make(http.Header), redirectsDisabled: HTTPUpstreamRedirectsDisabled(ctx)}
	for name, values := range headers {
		if strings.EqualFold(name, APIKeyGroupHeader) || strings.EqualFold(name, "Host") {
			b.headers[name] = append([]string(nil), values...)
		}
	}
	return b
}

func checkOpenAIWSBillingHandshake(ctx context.Context, account *Account, binding *openAIWSBillingHandshake) error {
	target, headers := "", http.Header(nil)
	if binding != nil {
		target, headers = binding.target, binding.headers
	}
	_, err := guardOpenAIWSBillingHandshake(ctx, account, target, headers)
	if err != nil {
		return err
	}
	gate, _ := ctx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
	if gate != nil && account != nil && account.Type == AccountTypeAPIKey && upstreamBillingRateSyncEnabled(account) &&
		(binding == nil || !binding.redirectsDisabled) {
		return denyOpenAITurn(openAIProfitFilterReasonUpstreamRateUnavailable)
	}
	return nil
}

// guardUpstreamBillingRequest ties automatically synced rates to the actual
// prepared request. Callers retain their existing authoritative account checks.
func guardUpstreamBillingRequest(ctx context.Context, account *Account, request *http.Request) (context.Context, error) {
	if account == nil || account.Type != AccountTypeAPIKey || !upstreamBillingRateSyncEnabled(account) {
		return ctx, nil
	}
	if !upstreamBillingRequestTargetConfirmed(account, request) {
		return ctx, denyOpenAITurn(openAIProfitFilterReasonUpstreamRateUnavailable)
	}
	expected, expectedErr := upstreamBillingRequestGroup(account)
	actual, actualErr := ParseAPIKeyRequestGroup(request.Header)
	if expectedErr != nil || actualErr != nil || expected != actual {
		return ctx, denyOpenAITurn("account_binding_changed")
	}
	return WithHTTPUpstreamRedirectsDisabled(ctx), nil
}

// guardOpenAIWSBillingHandshake applies the HTTP billing namespace and pin
// checks to the actual WS handshake. It returns a redirect-fenced context only
// for requests governed by the automatic-rate profit gate.
func guardOpenAIWSBillingHandshake(ctx context.Context, account *Account, target string, headers http.Header) (context.Context, error) {
	gate, _ := ctx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
	if gate == nil || account == nil || account.Type != AccountTypeAPIKey || !upstreamBillingRateSyncEnabled(account) {
		return ctx, nil
	}
	if gate.configUnavailable {
		return ctx, denyOpenAITurn(openAIProfitFilterReasonGroupConfigUnavailable)
	}
	u, err := url.Parse(target)
	if err != nil {
		return ctx, denyOpenAITurn(openAIProfitFilterReasonUpstreamRateUnavailable)
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	default:
		return ctx, denyOpenAITurn(openAIProfitFilterReasonUpstreamRateUnavailable)
	}
	request := &http.Request{Method: http.MethodPost, URL: u, Host: headers.Get("Host"), Header: headers}
	return guardUpstreamBillingRequest(ctx, account, request)
}

// guardOpenAIProfitRequest shares the authoritative final account and billing
// checks with adapters that send outside the ordinary OpenAI transport.
func (s *OpenAIGatewayService) guardOpenAIProfitRequest(request *http.Request, account *Account) (*http.Request, error) {
	if gate, _ := request.Context().Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate); gate != nil {
		if gate.configUnavailable {
			return nil, denyOpenAITurn(openAIProfitFilterReasonGroupConfigUnavailable)
		}
		// Some protocol adapters build their request after initial admission.
		// Re-read here as well so a prepared request cannot cross a rate/key or
		// group-membership change before reaching any egress implementation.
		latest, err := s.latestOpenAITurnAccountForGroup(request.Context(), account, gate.groupID, true)
		if err != nil {
			return nil, err
		}
		if account.AuthConnectionID != "" && account.IsExcelBPSEnabled() {
			if account.excelBPSParentRoute == nil || *account.excelBPSParentRoute != excelBPSParentRouteFingerprint(latest) {
				return nil, denyOpenAITurn("account_binding_changed")
			}
			if vetoed, reason := openAIProfitControlVetoReason(request.Context(), latest); vetoed {
				return nil, denyOpenAITurn(reason)
			}
		} else if err := checkOpenAITurnBindingAndRate(request.Context(), account, latest); err != nil {
			return nil, err
		}
		guardedCtx, err := guardUpstreamBillingRequest(request.Context(), latest, request)
		if err != nil {
			return nil, err
		}
		request = request.WithContext(guardedCtx)
	}
	return request, nil
}

// doOpenAIUpstream 只在 OpenAI OAuth 能力绑定已启用时把真实请求交给插件。
// 插件返回标准 http.Response，响应解析、错误映射、SSE 和计费仍由现有核心链处理。
func (s *OpenAIGatewayService) doOpenAIUpstream(request *http.Request, proxyURL string, account *Account) (result *http.Response, resultErr error) {
	request = bindOpenAIAgentPublicKey(request, s.accountRepo, account)
	if err := s.guardOpenAIResponseContinuation(request.Context(), account); err != nil {
		return nil, err
	}
	var err error
	request, err = s.guardOpenAIProfitRequest(request, account)
	if err != nil {
		return nil, err
	}
	if s.excelOAuthReauth != nil {
		if err := s.excelOAuthReauth.validateAuthConnectionPolicy(request.Context(), account); err != nil {
			return nil, err
		}
	}
	if account != nil && account.AuthConnectionID != "" {
		if s.excelOAuthReauth == nil {
			return nil, fmt.Errorf("authorization connection service unavailable")
		}
		if err := s.excelOAuthReauth.ValidateAuthConnectionSend(request.Context(), account); err != nil {
			return nil, err
		}
	}
	var phaseGuard *openAIHTTPPhaseGuard
	request, phaseGuard = s.guardOpenAIHTTPRequest(request, account)
	request, finishHeaderTimeout := guardOpenAIResponseHeaderTimeout(request, account)
	defer func() {
		if result != nil && result.Request == nil {
			result.Request = request
		}
		result, resultErr = phaseGuard.finish(result, resultErr)
		resultErr = finishHeaderTimeout(result, resultErr)
	}()
	if phaseGuard != nil && phaseGuard.ctx.Err() != nil {
		return nil, context.Cause(phaseGuard.ctx)
	}
	request, safetyInfo := s.applySafetyPromptToRequest(request, account)
	defer func() {
		if result != nil {
			result = s.wrapSafetyPromptResponse(result, safetyInfo)
		}
	}()
	if account.IsSIWCEnabled() {
		if request.URL == nil || request.URL.Scheme != "https" || request.URL.Host != "api.openai.com" || request.URL.Path != "/v1/responses" {
			return nil, fmt.Errorf("SIWC request must use the official Responses endpoint")
		}
		// Compatibility adapters can add internal headers after building a request.
		// Enforce SIWC headers at the final send boundary as well.
		authorization := request.Header.Get("Authorization")
		applySIWCRequestHeaders(request)
		request.Host = ""
		if err := s.priorityLatencyBeforeSend(request.Context(), account); err != nil {
			return nil, err
		}
		if err := s.fenceAuthConnectionEgress(request.Context(), request, account); err != nil {
			return nil, err
		}
		if err := s.admitAvailabilityRequest(request.Context(), request, account); err != nil {
			return nil, err
		}
		observeAttempt := s.observePriorityAttempt(request, account)
		request, observeTransport := s.rateLimitService.beginAvailabilityTransport(request, account)
		resp, err := s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
		observeTransport(resp, err)
		observeAttempt(resp, err)
		s.rateLimitService.observeQualityResponse(request.Context(), account, resp, err)
		if err == nil && resp != nil && resp.StatusCode == http.StatusUnauthorized && s.excelOAuthReauth != nil {
			var cache OpenAITokenCache
			if s.openAITokenProvider != nil {
				cache = s.openAITokenProvider.tokenCache
			}
			s.excelOAuthReauth.rejectSIWCAccessToken(request.Context(), account, strings.TrimPrefix(authorization, "Bearer "), cache)
		}
		return resp, err
	}
	request = s.stickBoundCodexTicketRequest(request, account)
	boundTicket := s.codexTicketRequestBound(request, account)
	observationTicket := s.boundCodexTicket(request, account)
	observationHeaders := request.Header.Clone()
	releaseChat := func() {}
	if boundTicket {
		releaseChat = s.holdCodexTicketChat(account)
	}
	proxyURL, release, err := s.pinCodexTicketEgress(request, account, proxyURL)
	if err != nil {
		releaseChat()
		return nil, err
	}
	defer func() {
		observe := resultErr == nil && result != nil && result.StatusCode == http.StatusOK
		result, resultErr = attachCodexTicketEgressRelease(result, resultErr, func() {
			release()
			releaseChat()
			if observe {
				s.observeScheduledAstraCandy(request.Context(), observationHeaders, account, observationTicket)
			}
		})
	}()
	defer func() {
		if resultErr == nil {
			// Fast admission may have installed the ticket inside the upstream
			// adapter. Capture the binding before observation can revoke it.
			observedRequest := request
			if result != nil && result.Request != nil && s.UnifiedAstraTicketsEnabled(request.Context()) && s.codexTicketRequestBound(result.Request, account) {
				// Timing/fallback layers clone request headers before the routing
				// adapter installs a newly acquired bundle. Observe the sent copy.
				observedRequest = result.Request
			}
			if ticket := s.boundCodexTicket(observedRequest, account); ticket != nil {
				observationTicket = ticket
				observationHeaders = observedRequest.Header.Clone()
			}
			responseBound := boundTicket || s.codexTicketRequestBound(observedRequest, account)
			s.observeCodexTicketResponse(observedRequest, result, account)
			if responseBound && codexResponseMismatches(observedRequest, result, account) && s.settingService.CodexTicketStrictResponse(request.Context()) {
				if result.Body != nil {
					_ = result.Body.Close()
				}
				result = nil
				resultErr = ErrCodexTicketResponseRejected
			}
		}
	}()
	// Keep ticket observation/strict validation around the final response,
	// while every egress attempt retains its own plugin routing and trace.
	return s.doUpstreamWithProxyFallback(request.Context(), request, account, proxyURL)
}

// doOpenAIAccountTestUpstream applies the same stored ticket and transport lifecycle
// as production forwarding. Use the final payload model, after mapping/normalization;
// a self-harvested ticket must not depend on membership in the gateway-reuse target list.
func (s *AccountTestService) doOpenAIAccountTestUpstream(
	request *http.Request,
	proxyURL string,
	account *Account,
	useTLSFallback bool,
	body []byte,
) (*http.Response, error) {
	request = bindOpenAIAgentPublicKey(request, s.accountRepo, account)
	gateway := s.openaiGatewayService
	model := extractOpenAICodexTicketModel(body)
	if gateway != nil && !IsAstraSourceAcquisition(request.Context()) && request.URL != nil && request.URL.String() == chatgptCodexAPIURL &&
		isOpenAICodexTicketAccount(account, model) && gateway.openAICodexTicketEnabledContext(request.Context()) &&
		gateway.openAICodexTicketGatedModel(model) {
		if err := gateway.applyOpenAICodexTicket(request.Context(), account, model, request.Header, "sse"); err != nil {
			return nil, err
		}
		// This restores the complete cookie/egress bundle, holds it until body close,
		// observes replacements and enforces the configured strict response policy.
		request = request.WithContext(context.WithValue(request.Context(), astraCandySkipObservationKey{}, true))
		return gateway.doOpenAIUpstream(request, proxyURL, account)
	}
	if s.pluginManager != nil {
		response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(pluginUpstreamKeyPolicyContext(request.Context(), s.cfg), request, proxyURL, account)
		if handled {
			return response, err
		}
	}
	if useTLSFallback {
		return timedAccountTestUpstream(s.httpUpstream).DoWithTLS(
			request,
			proxyURL,
			account.ID,
			account.Concurrency,
			s.tlsFPProfileService.ResolveTLSProfile(account),
		)
	}
	return timedAccountTestUpstream(s.httpUpstream).Do(request, proxyURL, account.ID, account.Concurrency)
}
