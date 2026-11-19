package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// UpstreamCacheProbe never accepts a downstream key, prompt, URL or credential.
// The existing administrator middleware owns access; observers are not allowed.
func (h *AccountHandler) UpstreamCacheProbe(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 8193))
	if err != nil || len(body) > 8192 || !cacheProbeJSONFields(body) {
		response.BadRequest(c, "Invalid upstream cache probe settings")
		return
	}
	var input service.UpstreamCacheProbeInput
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || input.Validate() != nil {
		response.BadRequest(c, "Invalid upstream cache probe settings")
		return
	}
	if h.accountTestService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Upstream cache probe service unavailable")
		return
	}
	_ = h.accountTestService.TestUpstreamCacheProbe(c, id, input)
}

// Token decoding retains duplicates/case before map/struct decoding discards
// them. Keep the small allowlist here at the untrusted HTTP boundary.
func cacheProbeJSONFields(body []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return false
	}
	allowed := map[string]bool{"model": true, "intervals_ms": true, "prefix_blocks": true, "timeout_seconds": true, "reasoning_effort": true, "service_tier": true}
	seen := make(map[string]bool)
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok || !allowed[name] || seen[name] {
			return false
		}
		seen[name] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil || bytes.Equal(value, []byte("null")) {
			return false
		}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return false
	}
	_, err = decoder.Token()
	return err == io.EOF
}
