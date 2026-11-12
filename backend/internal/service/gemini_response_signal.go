package service

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// geminiResponseSignalKind 区分 Gemini 2xx 响应事件里携带的带内信号。
// 这些信号都承载在 HTTP 2xx 之上，客户端字节原样透传；确定的上游故障同时失效可用证据。
// 数值越大优先级越高：同一响应出现多个信号时只登记优先级最高的一个。
type geminiResponseSignalKind int

const (
	geminiSignalNone geminiResponseSignalKind = iota
	// geminiSignalPromptBlocked：promptFeedback.blockReason 有值，提示词整体被拦，candidates 为空。
	geminiSignalPromptBlocked
	// geminiSignalContentFilter：首候选 finishReason 属于内容过滤类枚举。
	geminiSignalContentFilter
	// geminiSignalAbnormalStop：上游 2xx 但响应为空或 SSE 缺少完整终态。
	geminiSignalAbnormalStop
	// geminiSignalError：事件本身是 Google 错误信封 {"error":{"code","message","status"}}，
	// 与 Gemini SDK 客户端对流式 chunk 的错误判定相同。
	geminiSignalError
)

// geminiResponseSignal 是一次带内信号的归一化描述。
type geminiResponseSignal struct {
	Kind geminiResponseSignalKind
	// Reason 是 finishReason / blockReason 枚举值，或 Google 错误信封的 status；作为 ops 错误 code。
	Reason  string
	Message string
	// Status 是该信号的语义 HTTP 状态：内容策略类固定 400，错误信封取 error.code，异常响应 502。
	Status int
	// FaultStatus 只接受无歧义的上游故障；ops 的兜底状态不能作为失效依据。
	FaultStatus int
	// Detail 是错误信封原文（已截断），仅 geminiSignalError 填写。
	Detail string
}

const (
	geminiSignalEmptyStreamReason      = "EMPTY_STREAM"
	geminiSignalEmptyResponseReason    = "EMPTY_RESPONSE"
	geminiSignalIncompleteStreamReason = "INCOMPLETE_STREAM"
	// geminiSSEFallbackBodyLimit 是非 data 行兜底内容的收集上限，超过即放弃整段判定。
	geminiSSEFallbackBodyLimit = 1 << 20
)

// geminiSignalProbeKeys 是三类信号各自必然出现的字段名；事件不含任何一个即可跳过 JSON 解析。
var geminiSignalProbeKeys = [][]byte{[]byte(`"error"`), []byte(`"promptFeedback"`), []byte(`"finishReason"`)}

func geminiPayloadMayCarrySignal(payload []byte) bool {
	for _, key := range geminiSignalProbeKeys {
		if bytes.Contains(payload, key) {
			return true
		}
	}
	return false
}

// detectGeminiResponseSignal 检查一个 Gemini 响应事件是否携带带内信号；Code Assist 的
// {"response":{...}} 包装体先解包。判定顺序：错误信封 > promptFeedback.blockReason > 首候选 finishReason。
// finishReason 只有内容过滤类枚举登记为信号；STOP、MAX_TOKENS、OTHER、MALFORMED_FUNCTION_CALL、
// UNEXPECTED_TOOL_CALL、TOO_MANY_TOOL_CALLS 等其余枚举都是模型侧的生成结果，不做上游归因，一律不登记。
func detectGeminiResponseSignal(payload []byte) (geminiResponseSignal, bool) {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || !geminiPayloadMayCarrySignal(payload) || !gjson.ValidBytes(payload) {
		return geminiResponseSignal{}, false
	}
	faultStatus := geminiResponseFaultStatus(payload)
	if inner := gjson.GetBytes(payload, "response"); inner.Exists() && inner.IsObject() {
		payload = []byte(inner.Raw)
	}

	if errObj := gjson.GetBytes(payload, "error"); errObj.Exists() && errObj.IsObject() {
		googleStatus := strings.ToUpper(strings.TrimSpace(errObj.Get("status").String()))
		status := int(errObj.Get("code").Int())
		if status < 400 || status > 599 {
			status = geminiGoogleStatusToHTTPStatus(googleStatus)
		}
		message := strings.TrimSpace(errObj.Get("message").String())
		if message == "" {
			message = "Gemini upstream returned an error event"
		}
		reason := googleStatus
		if reason == "" {
			reason = "UPSTREAM_ERROR"
		}
		return geminiResponseSignal{
			Kind:        geminiSignalError,
			Reason:      reason,
			Message:     message,
			Status:      status,
			FaultStatus: faultStatus,
			Detail:      truncateString(string(payload), 2048),
		}, true
	}

	blockReason := strings.ToUpper(strings.TrimSpace(gjson.GetBytes(payload, "promptFeedback.blockReason").String()))
	switch blockReason {
	case "", "BLOCKED_REASON_UNSPECIFIED":
	default:
		return geminiResponseSignal{
			Kind:    geminiSignalPromptBlocked,
			Reason:  blockReason,
			Message: fmt.Sprintf("Gemini content policy block (blockReason=%s): prompt was blocked before generation", blockReason),
			Status:  http.StatusBadRequest,
		}, true
	}

	candidate := geminiPrimaryCandidate(payload)
	finishReason := strings.ToUpper(strings.TrimSpace(candidate.Get("finishReason").String()))
	if !isGeminiContentFilterFinishReason(finishReason) {
		return geminiResponseSignal{}, false
	}
	detail := strings.TrimSpace(candidate.Get("finishMessage").String())
	if detail == "" {
		detail = geminiFinishReasonText(finishReason)
	}
	detail = truncateString(detail, 512)
	return geminiResponseSignal{
		Kind:    geminiSignalContentFilter,
		Reason:  finishReason,
		Message: fmt.Sprintf("Gemini content policy stop (finishReason=%s): %s", finishReason, detail),
		Status:  http.StatusBadRequest,
	}, true
}

// Logging remains lenient for compatibility. Scheduling requires a numeric
// integer or a known Google status, and rejects conflicting duplicate fields.
func geminiResponseFaultStatus(payload []byte) int {
	fields, valid := availabilityJSONObject(gjson.ParseBytes(payload))
	if !valid {
		return 0
	}
	if inner := fields["response"]; inner.IsObject() {
		fields, valid = availabilityJSONObject(inner)
		if !valid {
			return 0
		}
	}
	fields, valid = availabilityJSONObject(fields["error"])
	if !valid {
		return 0
	}
	status := 0
	if code := fields["code"]; code.Exists() {
		if code.Type != gjson.Number {
			return 0
		}
		var err error
		status, err = strconv.Atoi(code.Raw)
		if err != nil || status < 400 || status > 599 {
			return 0
		}
	} else if name := fields["status"]; name.Type == gjson.String {
		switch strings.ToUpper(strings.TrimSpace(name.String())) {
		case "UNAUTHENTICATED", "RESOURCE_EXHAUSTED", "INTERNAL", "UNKNOWN", "DATA_LOSS", "UNAVAILABLE", "DEADLINE_EXCEEDED":
			status = geminiGoogleStatusToHTTPStatus(strings.ToUpper(strings.TrimSpace(name.String())))
		}
	}
	if status == http.StatusUnauthorized || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500 {
		return status
	}
	return 0
}

// One observer belongs to one upstream response. A later 5xx still triggers
// quality protection after a 401/429, while repeated chunks cannot multiply it.
type geminiResponseSignalObserver struct {
	ctx                   context.Context
	limiter               *RateLimitService
	account               *Account
	best                  geminiResponseSignal
	availabilityObserved  bool
	qualityObserved       bool
	sawSSEData            bool
	sawSSEResponseContent bool
	sawSSECandidate       bool
	sawSSETerminal        bool
	sseFallback           geminiSSEFallbackBody
}

func newGeminiResponseSignalObserver(ctx context.Context, account *Account, response *http.Response, limiter *RateLimitService) *geminiResponseSignalObserver {
	if ctx == nil {
		ctx = context.Background()
	}
	if response != nil {
		if body, ok := response.Body.(*availabilityFaultBody); ok {
			ctx = body.ctx
			if account == nil {
				// Independent Gemini-wire gateways consume the same immutable,
				// transport-bound identity instead of guessing it from headers.
				account = &body.account
			}
		} else if ctx.Value(qualityProbeContextKey{}) == nil && response.Request != nil {
			ctx = availabilityFaultResponseContext(response.Request.Context(), response)
		}
	}
	return &geminiResponseSignalObserver{ctx: ctx, account: account, limiter: limiter}
}

func (o *geminiResponseSignalObserver) observe(sig geminiResponseSignal) {
	if sig.Kind > o.best.Kind {
		o.best = sig
	}
	if sig.Kind == geminiSignalAbnormalStop {
		if !o.availabilityObserved {
			o.limiter.observeAvailabilityIncomplete(o.ctx, o.account, false)
			o.availabilityObserved = true
		}
		return
	}
	if sig.FaultStatus == 0 {
		return
	}
	if sig.FaultStatus >= 500 {
		if o.qualityObserved {
			return
		}
		o.qualityObserved = true
	} else if o.availabilityObserved {
		return
	}
	o.limiter.observeQualityStatus(o.ctx, o.account, sig.FaultStatus)
	o.availabilityObserved = true
}

func (o *geminiResponseSignalObserver) observePayload(payload []byte) {
	if sig, ok := detectGeminiResponseSignal(payload); ok {
		o.observe(sig)
	}
	if !gjson.ValidBytes(payload) {
		return
	}
	root := gjson.ParseBytes(payload)
	fields, valid := availabilityJSONObject(root)
	if !valid {
		return
	}
	if response, exists := fields["response"]; exists {
		if fields["candidates"].Exists() {
			return
		}
		_, valid = availabilityJSONObject(response)
		if !valid {
			return
		}
		root = response
	}
	// Only the primary candidate can complete the stream being served. Unknown
	// JSON, usage tails and a secondary candidate's STOP are not that evidence.
	fields, valid = availabilityJSONObject(geminiPrimaryCandidate([]byte(root.Raw)))
	if !valid || fields["index"].Exists() && fields["index"].Raw != "0" {
		return
	}
	o.sawSSECandidate = true
	if finish := fields["finishReason"]; finish.Type == gjson.String {
		reason := strings.TrimSpace(finish.String())
		if reason != "" && reason != "FINISH_REASON_UNSPECIFIED" {
			o.sawSSETerminal = true
		}
	}
}

func (o *geminiResponseSignalObserver) observeIncompleteSSE(clientDisconnected bool) {
	if clientDisconnected || o.ctx.Err() != nil || o.best.Kind != geminiSignalNone || o.sawSSETerminal {
		return
	}
	o.observe(geminiResponseSignal{Kind: geminiSignalAbnormalStop,
		Reason: geminiSignalIncompleteStreamReason, Message: "Gemini upstream stream ended before completion", Status: http.StatusBadGateway})
}

func (o *geminiResponseSignalObserver) finishSSE(clientDisconnected bool) {
	if clientDisconnected || o.ctx.Err() != nil {
		return
	}
	o.finalizeSSESignal(o.sawSSEData, o.sawSSEResponseContent, &o.sseFallback)
}

func (s *GeminiMessagesCompatService) observeGeminiJSONSignal(c *gin.Context, response *http.Response, account *Account, upstreamRequestID string, body []byte) {
	observed := newGeminiResponseSignalObserver(responseTimingContext(c, response), account, response, s.rateLimitService)
	if sig, ok := detectGeminiResponseSignalInBody(body); ok {
		observed.observe(sig)
		s.markGeminiResponseSignal(c, account, sig, false, upstreamRequestID)
	} else if isGeminiEmptyResponseBody(body) {
		observed.observe(geminiResponseSignal{Kind: geminiSignalAbnormalStop})
		s.markGeminiEmptyResponse(c, account, false, upstreamRequestID)
	}
}

// geminiPrimaryCandidate 返回首候选：index 缺省或为 0 的候选。多候选流里其它 index 的候选不参与判定。
func geminiPrimaryCandidate(payload []byte) gjson.Result {
	candidates := gjson.GetBytes(payload, "candidates")
	if !candidates.IsArray() {
		return gjson.Result{}
	}
	var primary gjson.Result
	candidates.ForEach(func(_, cand gjson.Result) bool {
		if index := cand.Get("index"); index.Exists() && index.Int() != 0 {
			return true
		}
		primary = cand
		return false
	})
	return primary
}

// detectGeminiResponseSignalInBody 对整段响应体判定：JSON 数组逐元素判定并取优先级最高者，对象直接判定。
func detectGeminiResponseSignalInBody(body []byte) (geminiResponseSignal, bool) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 || !geminiPayloadMayCarrySignal(body) || !gjson.ValidBytes(body) {
		return geminiResponseSignal{}, false
	}
	parsed := gjson.ParseBytes(body)
	if !parsed.IsArray() {
		return detectGeminiResponseSignal(body)
	}
	var best geminiResponseSignal
	faultStatus := 0
	parsed.ForEach(func(_, item gjson.Result) bool {
		if sig, ok := detectGeminiResponseSignal([]byte(item.Raw)); ok {
			faultStatus = max(faultStatus, sig.FaultStatus)
			if sig.Kind > best.Kind {
				best = sig
			}
		}
		return true
	})
	best.FaultStatus = faultStatus
	return best, best.Kind != geminiSignalNone
}

// isGeminiEmptyResponseBody 判定 2xx 响应体是否没有任何响应内容：空白、空对象、candidates 为空数组
// 且没有 promptFeedback，或数组为空 / 全部元素都满足前述条件；Code Assist 的 response 包装先解包。
func isGeminiEmptyResponseBody(body []byte) bool {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return true
	}
	if !gjson.ValidBytes(body) {
		return false
	}
	parsed := gjson.ParseBytes(body)
	if parsed.IsArray() {
		empty := true
		parsed.ForEach(func(_, item gjson.Result) bool {
			empty = isGeminiEmptyResponseObject(item)
			return empty
		})
		return empty
	}
	return isGeminiEmptyResponseObject(parsed)
}

func isGeminiEmptyResponseObject(item gjson.Result) bool {
	if !item.IsObject() {
		return false
	}
	if inner := item.Get("response"); inner.Exists() && inner.IsObject() {
		item = inner
	}
	candidates := item.Get("candidates")
	if candidates.IsArray() && len(candidates.Array()) == 0 && !item.Get("promptFeedback").Exists() {
		return true
	}
	empty := true
	item.ForEach(func(key, _ gjson.Result) bool {
		switch key.String() {
		case "usageMetadata", "modelVersion", "responseId":
			return true
		default:
			empty = false
			return false
		}
	})
	return empty
}

// isGeminiContentFilterFinishReason 的集合与 gemini-cli 遥测的 CONTENT_FILTER 归类一致，并纳入同语义的图片枚举。
func isGeminiContentFilterFinishReason(reason string) bool {
	switch reason {
	case "SAFETY", "RECITATION", "LANGUAGE", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII",
		"IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "IMAGE_RECITATION":
		return true
	default:
		return false
	}
}

// geminiFinishReasonText 是 finishMessage 缺失时的兜底描述；列出的枚举沿用 gemini-cli 的
// 用户提示文案，其余枚举用通用兜底。
func geminiFinishReasonText(reason string) string {
	switch reason {
	case "SAFETY":
		return "Response stopped due to safety reasons."
	case "RECITATION":
		return "Response stopped due to recitation policy."
	case "LANGUAGE":
		return "Response stopped due to unsupported language."
	case "BLOCKLIST":
		return "Response stopped due to forbidden terms."
	case "PROHIBITED_CONTENT":
		return "Response stopped due to prohibited content."
	case "SPII":
		return "Response stopped due to sensitive personally identifiable information."
	case "IMAGE_SAFETY":
		return "Response stopped due to image safety violations."
	case "IMAGE_PROHIBITED_CONTENT":
		return "Response stopped due to prohibited image content."
	default:
		return "Response stopped."
	}
}

// geminiGoogleStatusToHTTPStatus 把 google.rpc.Code 名称映射为 HTTP 状态；未知取 502。
func geminiGoogleStatusToHTTPStatus(status string) int {
	switch status {
	case "INVALID_ARGUMENT", "FAILED_PRECONDITION", "OUT_OF_RANGE":
		return http.StatusBadRequest
	case "UNAUTHENTICATED":
		return http.StatusUnauthorized
	case "PERMISSION_DENIED":
		return http.StatusForbidden
	case "NOT_FOUND":
		return http.StatusNotFound
	case "RESOURCE_EXHAUSTED":
		return http.StatusTooManyRequests
	case "INTERNAL", "UNKNOWN", "DATA_LOSS":
		return http.StatusInternalServerError
	case "UNAVAILABLE":
		return http.StatusServiceUnavailable
	case "DEADLINE_EXCEEDED":
		return http.StatusGatewayTimeout
	default:
		return http.StatusBadGateway
	}
}

// geminiSignalOpsErrorType 把语义状态映射为 ops 已知的错误类型（见 handler.isKnownOpsErrorType）。
func geminiSignalOpsErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		return "upstream_error"
	}
}

// geminiSSEFallbackBody 收集 SSE 读取过程中既非 data 行也非注释的非空内容（上游忽略 alt=sse
// 直接回 JSON 的形态），超过上限后停止收集并标记截断。
type geminiSSEFallbackBody struct {
	buf       bytes.Buffer
	truncated bool
}

func (b *geminiSSEFallbackBody) AddLine(line string) {
	if b == nil || b.truncated {
		return
	}
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, ":") {
		return
	}
	if b.buf.Len()+len(trimmed)+1 > geminiSSEFallbackBodyLimit {
		b.truncated = true
		return
	}
	_, _ = b.buf.WriteString(trimmed)
	_ = b.buf.WriteByte('\n')
}

func (b *geminiSSEFallbackBody) Bytes() []byte {
	if b == nil {
		return nil
	}
	return b.buf.Bytes()
}

func (b *geminiSSEFallbackBody) Truncated() bool {
	return b != nil && b.truncated
}

// markGeminiResponseSignal 把带内信号登记到 ops 上下文。wire 状态保持 2xx，客户端字节不变，
// 用量与计费照常走 ForwardResult。stream 是客户端请求的流式标记。
//   - 内容策略类（promptFeedback.blockReason / 内容过滤 finishReason）是请求级结果：
//     只记请求级带内错误，不归因上游账号，不计入 SLA。
//   - 错误信封与空响应按上游失败登记：写上游错误上下文与尝试事件，并按语义状态计入 SLA。
func (s *GeminiMessagesCompatService) markGeminiResponseSignal(c *gin.Context, account *Account, sig geminiResponseSignal, stream bool, upstreamRequestID string) {
	if c == nil || sig.Kind == geminiSignalNone {
		return
	}
	message := sanitizeUpstreamErrorMessage(strings.TrimSpace(sig.Message))
	switch sig.Kind {
	case geminiSignalPromptBlocked, geminiSignalContentFilter:
		MarkOpsStreamErrorValue(c, OpsStreamError{
			ErrType:        "invalid_request_error",
			Code:           sig.Reason,
			Message:        message,
			IntendedStatus: sig.Status,
			RequestScoped:  true,
			NonStream:      !stream,
		})
		return
	}

	detail := ""
	if sig.Detail != "" && s != nil && s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		detail = truncateString(sig.Detail, maxBytes)
	}
	setOpsUpstreamError(c, sig.Status, message, detail)
	kind := "http_error"
	if stream {
		kind = "stream_failed"
	}
	event := OpsUpstreamErrorEvent{
		ProxyID:            opsUpstreamProxyID(account),
		ProxyName:          opsUpstreamProxyName(account),
		Platform:           PlatformGemini,
		UpstreamStatusCode: sig.Status,
		UpstreamRequestID:  strings.TrimSpace(upstreamRequestID),
		Kind:               kind,
		Message:            message,
		Detail:             detail,
	}
	if account != nil {
		event.Platform = account.Platform
		event.AccountID = account.ID
		event.AccountName = account.Name
	}
	appendOpsUpstreamError(c, event)
	MarkOpsStreamErrorValue(c, OpsStreamError{
		ErrType:         geminiSignalOpsErrorType(sig.Status),
		Code:            sig.Reason,
		Message:         message,
		IntendedStatus:  sig.Status,
		CountTowardsSLA: true,
		NonStream:       !stream,
	})
}

// markGeminiEmptyResponse 登记"上游 2xx 但没有任何响应内容"的结果：流式指整段 body 没有 data 事件
// 也没有其它非空内容，非流式指空体、空对象或 candidates 为空数组。原响应不重放。
func (s *GeminiMessagesCompatService) markGeminiEmptyResponse(c *gin.Context, account *Account, stream bool, upstreamRequestID string) {
	reason := geminiSignalEmptyResponseReason
	message := "Gemini upstream returned an empty response body"
	if stream {
		reason = geminiSignalEmptyStreamReason
		message = "Gemini upstream returned an empty stream body"
	}
	s.markGeminiResponseSignal(c, account, geminiResponseSignal{
		Kind:    geminiSignalAbnormalStop,
		Reason:  reason,
		Message: message,
		Status:  http.StatusBadGateway,
	}, stream, upstreamRequestID)
}

// finalizeGeminiSSESignal 在上游 SSE 读完后做最终登记：已识别主候选缺终态时先记录未完成，
// 然后登记流中优先级最高的信号；
// 只有空 data 事件同样登记空响应；没有 data 时，兜底内容超限则放弃判定，为空登记空响应，是非 SSE 的 JSON 则对整段
// 判定一次，整段没有信号但没有任何响应内容同样登记空响应。
func (s *GeminiMessagesCompatService) finalizeGeminiSSESignal(c *gin.Context, account *Account, stream bool, upstreamRequestID string, observed *geminiResponseSignalObserver, sawDataEvent, sawResponseContent bool, fallback *geminiSSEFallbackBody) {
	sig := observed.finalizeSSESignal(sawDataEvent, sawResponseContent, fallback)
	if sig.Kind == geminiSignalAbnormalStop && sig.Reason == "" {
		s.markGeminiEmptyResponse(c, account, stream, upstreamRequestID)
		return
	}
	s.markGeminiResponseSignal(c, account, sig, stream, upstreamRequestID)
}

// Both gateways use the same EOF classification. An explicit non-fault signal
// or unknown response must not be promoted to an empty-response fault.
func (o *geminiResponseSignalObserver) finalizeSSESignal(sawDataEvent, sawResponseContent bool, fallback *geminiSSEFallbackBody) geminiResponseSignal {
	if o.sawSSECandidate {
		o.observeIncompleteSSE(false)
	}
	if o.best.Kind != geminiSignalNone || sawResponseContent || fallback.Truncated() {
		return o.best
	}
	body := fallback.Bytes()
	if sawDataEvent || len(bytes.TrimSpace(body)) == 0 {
		o.observe(geminiResponseSignal{Kind: geminiSignalAbnormalStop})
		return o.best
	}
	if sig, ok := detectGeminiResponseSignalInBody(body); ok {
		o.observe(sig)
		return o.best
	}
	if isGeminiEmptyResponseBody(body) {
		o.observe(geminiResponseSignal{Kind: geminiSignalAbnormalStop})
	}
	return o.best
}
