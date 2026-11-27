package middleware

import (
	"context"
	"regexp"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const requestIDHeader = "X-Request-ID"

var incomingCloudflareRay = regexp.MustCompile(`^[0-9a-fA-F]{16}(?:-[A-Z]{3})?$`)

func requestLogPath(path string) string {
	if strings.HasPrefix(path, "/api/bps-images/") {
		return "/api/bps-images/[redacted]"
	}
	return path
}

// RequestLogger 在请求入口注入 request-scoped logger。
func RequestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request == nil {
			c.Next()
			return
		}

		requestID, validRequestID := normalizeCorrelationID(c.GetHeader(requestIDHeader))
		if !validRequestID {
			requestID = uuid.NewString()
		}
		c.Header(requestIDHeader, requestID)

		ctx := context.WithValue(c.Request.Context(), ctxkey.RequestID, requestID)
		clientRequestID, _ := ctx.Value(ctxkey.ClientRequestID).(string)
		clientRequestID, _ = normalizeCorrelationID(clientRequestID)

		fields := []zap.Field{
			zap.String("component", "http"),
			zap.String("request_id", requestID),
			zap.String("client_request_id", strings.TrimSpace(clientRequestID)),
			zap.String("path", requestLogPath(c.Request.URL.Path)),
			zap.String("method", c.Request.Method),
		}
		// Incoming identifiers are untrusted correlation hints, never the
		// internal client/usage identity or evidence of a trusted proxy.
		if incoming := strings.TrimSpace(c.GetHeader(clientRequestIDHeader)); len(incoming) == 36 {
			if id, err := uuid.Parse(incoming); err == nil {
				fields = append(fields, zap.String("incoming_client_request_id", id.String()))
			}
		}
		if ray := strings.TrimSpace(c.GetHeader("Cf-Ray")); len(ray) <= 20 && incomingCloudflareRay.MatchString(ray) {
			fields = append(fields, zap.String("incoming_cf_ray", ray))
		}
		requestLogger := logger.With(fields...)

		ctx = logger.IntoContext(ctx, requestLogger)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
