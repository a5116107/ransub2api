package service

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type openAICompatSessionResponseBinding struct {
	RouteFingerprint     [32]byte
	ResponseID           string
	TurnState            string
	ContinuationDisabled bool
	ExpiresAt            time.Time
}

func openAICompatContinuationEnabled(account *Account, model string) bool {
	if account == nil || account.Type != AccountTypeAPIKey {
		return false
	}
	return shouldAutoInjectPromptCacheKeyForCompat(model)
}

func trimAnthropicCompatResponsesInputToLatestTurn(req *apicompat.ResponsesRequest) {
	if req == nil || len(req.Input) == 0 {
		return
	}

	var items []apicompat.ResponsesInputItem
	if err := json.Unmarshal(req.Input, &items); err != nil || len(items) == 0 {
		return
	}

	start := latestAnthropicCompatResponsesInputTurnStart(items)
	trimmed := append([]apicompat.ResponsesInputItem(nil), items[start:]...)
	if len(trimmed) == len(items) {
		return
	}
	if input, err := json.Marshal(trimmed); err == nil {
		req.Input = input
	}
}

func latestAnthropicCompatResponsesInputTurnStart(items []apicompat.ResponsesInputItem) int {
	if len(items) == 0 {
		return 0
	}

	start := len(items) - 1
	last := items[start]
	switch {
	case last.Type == "function_call_output":
		for start > 0 && items[start-1].Type == "function_call_output" {
			start--
		}
	case last.Type == "message" && last.Role == "user":
		for start > 0 && items[start-1].Type == "function_call_output" {
			start--
		}
	default:
		return start
	}

	return expandAnthropicCompatResponsesInputToolCallStart(items, start)
}

func expandAnthropicCompatResponsesInputToolCallStart(items []apicompat.ResponsesInputItem, start int) int {
	if start < 0 || start >= len(items) {
		return start
	}

	needed := make(map[string]struct{})
	for i := start; i < len(items); i++ {
		if items[i].Type != "function_call_output" {
			continue
		}
		callID := strings.TrimSpace(items[i].CallID)
		if callID != "" {
			needed[callID] = struct{}{}
		}
	}
	if len(needed) == 0 {
		return start
	}

	expandedStart := start
	for i := start - 1; i >= 0 && len(needed) > 0; i-- {
		if items[i].Type != "function_call" {
			continue
		}
		callID := strings.TrimSpace(items[i].CallID)
		if _, ok := needed[callID]; !ok {
			continue
		}
		delete(needed, callID)
		expandedStart = i
	}
	return expandedStart
}

func isOpenAICompatPreviousResponseNotFound(statusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if statusCode != http.StatusBadRequest && statusCode != http.StatusNotFound {
		return false
	}
	check := func(s string) bool {
		lower := strings.ToLower(strings.TrimSpace(s))
		return strings.Contains(lower, "previous_response_not_found") ||
			lower == "previous_response_id is not available for this user" ||
			(strings.Contains(lower, "previous response") && strings.Contains(lower, "not found")) ||
			(strings.Contains(lower, "unsupported parameter") && strings.Contains(lower, "previous_response_id"))
	}
	if check(upstreamMsg) || check(string(upstreamBody)) {
		return true
	}
	return check(gjson.GetBytes(upstreamBody, "error.code").String()) ||
		check(gjson.GetBytes(upstreamBody, "error.message").String())
}

func isOpenAICompatPreviousResponseUnsupported(statusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if statusCode != http.StatusBadRequest {
		return false
	}
	check := func(s string) bool {
		lower := strings.ToLower(strings.TrimSpace(s))
		if !strings.Contains(lower, "previous_response_id") {
			return false
		}
		return strings.Contains(lower, "unsupported parameter") ||
			strings.Contains(lower, "only supported on responses websocket") ||
			strings.Contains(lower, "not supported") ||
			strings.Contains(lower, "is not available for this user") ||
			strings.Contains(lower, "requires an openai api-key account for http requests")
	}
	if check(upstreamMsg) || check(string(upstreamBody)) {
		return true
	}
	return check(gjson.GetBytes(upstreamBody, "error.code").String()) ||
		check(gjson.GetBytes(upstreamBody, "error.message").String())
}

func openAICompatSessionResponseKey(c *gin.Context, account *Account, promptCacheKey string) string {
	key := strings.TrimSpace(promptCacheKey)
	if account == nil || key == "" {
		return ""
	}
	apiKeyID := int64(0)
	if c != nil {
		apiKeyID = getAPIKeyIDFromContext(c)
	}
	return strings.Join([]string{
		strconv.FormatInt(account.ID, 10),
		strconv.FormatInt(apiKeyID, 10),
		key,
	}, "\x00")
}

// loadOpenAICompatSessionResponseBinding is shared by readers and writers so a
// new observation cannot revive expired state or state from a replaced route.
func (s *OpenAIGatewayService) loadOpenAICompatSessionResponseBinding(key string, account *Account) (openAICompatSessionResponseBinding, bool) {
	if s == nil || key == "" || account == nil {
		return openAICompatSessionResponseBinding{}, false
	}
	raw, ok := s.openaiCompatSessionResponses.Load(key)
	if !ok {
		return openAICompatSessionResponseBinding{}, false
	}
	binding, ok := raw.(openAICompatSessionResponseBinding)
	if !ok {
		s.openaiCompatSessionResponses.Delete(key)
		return openAICompatSessionResponseBinding{}, false
	}
	if !time.Now().Before(binding.ExpiresAt) || binding.RouteFingerprint != AccountRouteFingerprint(account) {
		// Do not remove a newer observation stored after this load.
		s.openaiCompatSessionResponses.CompareAndDelete(key, binding)
		return openAICompatSessionResponseBinding{}, false
	}
	return binding, true
}

func (s *OpenAIGatewayService) getOpenAICompatSessionResponseID(_ context.Context, c *gin.Context, account *Account, promptCacheKey string) string {
	binding, ok := s.loadOpenAICompatSessionResponseBinding(openAICompatSessionResponseKey(c, account, promptCacheKey), account)
	if !ok || binding.ContinuationDisabled {
		return ""
	}
	return strings.TrimSpace(binding.ResponseID)
}

func (s *OpenAIGatewayService) bindOpenAICompatSessionResponseID(_ context.Context, c *gin.Context, account *Account, promptCacheKey, responseID string) {
	if s == nil {
		return
	}
	key := openAICompatSessionResponseKey(c, account, promptCacheKey)
	id := strings.TrimSpace(responseID)
	if key == "" || id == "" {
		return
	}
	binding, _ := s.loadOpenAICompatSessionResponseBinding(key, account)
	binding.RouteFingerprint = AccountRouteFingerprint(account)
	binding.ExpiresAt = time.Now().Add(s.openAIWSResponseStickyTTL())
	if binding.ContinuationDisabled {
		binding.ResponseID = ""
	} else {
		binding.ResponseID = id
	}
	s.openaiCompatSessionResponses.Store(key, binding)
}

func (s *OpenAIGatewayService) deleteOpenAICompatSessionResponseID(_ context.Context, c *gin.Context, account *Account, promptCacheKey string) {
	key := openAICompatSessionResponseKey(c, account, promptCacheKey)
	binding, ok := s.loadOpenAICompatSessionResponseBinding(key, account)
	if !ok {
		return
	}
	existing := binding
	binding.ResponseID = ""
	if strings.TrimSpace(binding.TurnState) == "" && !binding.ContinuationDisabled {
		s.openaiCompatSessionResponses.CompareAndDelete(key, existing)
		return
	}
	binding.ExpiresAt = time.Now().Add(s.openAIWSResponseStickyTTL())
	s.openaiCompatSessionResponses.CompareAndSwap(key, existing, binding)
}

func (s *OpenAIGatewayService) disableOpenAICompatSessionContinuation(_ context.Context, c *gin.Context, account *Account, promptCacheKey string) {
	if s == nil {
		return
	}
	key := openAICompatSessionResponseKey(c, account, promptCacheKey)
	if key == "" {
		return
	}
	binding, _ := s.loadOpenAICompatSessionResponseBinding(key, account)
	binding.RouteFingerprint = AccountRouteFingerprint(account)
	binding.ResponseID = ""
	binding.ContinuationDisabled = true
	binding.ExpiresAt = time.Now().Add(s.openAIWSResponseStickyTTL())
	s.openaiCompatSessionResponses.Store(key, binding)
}

func (s *OpenAIGatewayService) isOpenAICompatSessionContinuationDisabled(_ context.Context, c *gin.Context, account *Account, promptCacheKey string) bool {
	binding, ok := s.loadOpenAICompatSessionResponseBinding(openAICompatSessionResponseKey(c, account, promptCacheKey), account)
	return ok && binding.ContinuationDisabled
}

func (s *OpenAIGatewayService) getOpenAICompatSessionTurnState(_ context.Context, c *gin.Context, account *Account, promptCacheKey string) string {
	binding, ok := s.loadOpenAICompatSessionResponseBinding(openAICompatSessionResponseKey(c, account, promptCacheKey), account)
	if !ok {
		return ""
	}
	return strings.TrimSpace(binding.TurnState)
}

func (s *OpenAIGatewayService) bindOpenAICompatSessionTurnState(_ context.Context, c *gin.Context, account *Account, promptCacheKey, turnState string) {
	if s == nil {
		return
	}
	key := openAICompatSessionResponseKey(c, account, promptCacheKey)
	state := strings.TrimSpace(turnState)
	if key == "" || state == "" {
		return
	}
	binding, _ := s.loadOpenAICompatSessionResponseBinding(key, account)
	binding.RouteFingerprint = AccountRouteFingerprint(account)
	binding.TurnState = state
	binding.ExpiresAt = time.Now().Add(s.openAIWSResponseStickyTTL())
	s.openaiCompatSessionResponses.Store(key, binding)
}
