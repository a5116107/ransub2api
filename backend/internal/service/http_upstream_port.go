package service

import (
	"context"
	"errors"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

var ErrUpstreamKeyCapacity = errors.New("upstream key concurrency capacity unavailable")
var ErrUpstreamKeyPolicy = errors.New("upstream key concurrency policy unavailable or revision conflict")
var ErrUpstreamKeyLease = errors.New("upstream key concurrency lease lost")

// UpstreamKeyAdmissionReason identifies a local refusal before network send.
const UpstreamKeyAdmissionReason GatewayFailureReason = "upstream_key_admission_unavailable"

// Keep pre-send local capacity refusals out of account-health observations,
// including when transport adapters wrap the original or classified error.
// Lease loss after a send deliberately does not match this predicate.
func isUpstreamKeyAdmissionFailure(err error) bool {
	if errors.Is(err, ErrUpstreamKeyCapacity) || errors.Is(err, ErrUpstreamKeyPolicy) {
		return true
	}
	var failover *UpstreamFailoverError
	return errors.As(err, &failover) && failover.Reason == UpstreamKeyAdmissionReason
}

// UpstreamKeyConcurrency is the optional shared authority used by native WS
// turns. Fingerprints come from the final handshake, including pooled reuse.
// A nil release means no configured credential consumed capacity.
type UpstreamKeyConcurrency interface {
	AcquireUpstreamKeySlots(context.Context, []string) (context.Context, func(), error)
}

// Local admission failures are not supplier/network health failures. They use
// the existing bounded account failover without changing account health.
func upstreamKeyAdmissionFailover(err error) *UpstreamFailoverError {
	if !errors.Is(err, ErrUpstreamKeyCapacity) && !errors.Is(err, ErrUpstreamKeyPolicy) {
		return nil
	}
	return &UpstreamFailoverError{StatusCode: http.StatusServiceUnavailable, Reason: UpstreamKeyAdmissionReason, ResponseBody: []byte(`{"error":{"type":"upstream_key_admission_unavailable","message":"No shared upstream Key capacity is available"}}`)}
}

// HTTPUpstream 上游 HTTP 请求接口
// 用于向上游 API（Claude、OpenAI、Gemini 等）发送请求
type HTTPUpstream interface {
	// Do 执行 HTTP 请求（不启用 TLS 指纹）
	Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error)

	// DoWithTLS 执行带 TLS 指纹伪装的 HTTP 请求
	//
	// profile 参数:
	//   - nil: 不启用 TLS 指纹，行为与 Do 方法相同
	//   - non-nil: 使用指定的 Profile 进行 TLS 指纹伪装
	//
	// Profile 由调用方通过 TLSFingerprintProfileService 解析后传入，
	// 支持按账号绑定的数据库 profile 或内置默认 profile。
	DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error)
}
