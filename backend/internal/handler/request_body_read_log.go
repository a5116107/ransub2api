package handler

import (
	"net/http"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"go.uber.org/zap"
)

// logRequestBodyReadFailure uses the same payload-free classification and
// progress as admission middleware. Never log the raw error or request body.
func logRequestBodyReadFailure(reqLog *zap.Logger, req *http.Request, err error) pkghttputil.RequestBodyReadFailure {
	failure := pkghttputil.DescribeRequestBodyReadFailure(req, err)
	if reqLog != nil && err != nil && failure.Kind != pkghttputil.RequestBodyReadErrorMaxBytes {
		reqLog.Warn("read request body failed", failure.LogFields()...)
	}
	return failure
}

// requestBodyReadFailure preserves the existing protocol-shaped response while
// sharing its classification with logging and middleware.
func requestBodyReadFailure(reqLog *zap.Logger, req *http.Request, err error) (int, string) {
	failure := logRequestBodyReadFailure(reqLog, req, err)
	return failure.Status, failure.ClientMessage()
}
