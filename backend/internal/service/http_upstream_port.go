package service

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/google/uuid"
)

var ErrUpstreamKeyCapacity = errors.New("upstream key concurrency capacity unavailable")
var ErrUpstreamKeyPolicy = errors.New("upstream key concurrency policy unavailable or revision conflict")
var ErrUpstreamKeyLease = errors.New("upstream key concurrency lease lost")

// UpstreamKeyAdmissionReason identifies a local refusal before network send.
const UpstreamKeyAdmissionReason GatewayFailureReason = "upstream_key_admission_unavailable"

// IsUpstreamKeyAdmissionFailure keeps pre-send local capacity refusals out of account-health observations,
// including when transport adapters wrap the original or classified error.
// Lease loss after a send deliberately does not match this predicate.
func IsUpstreamKeyAdmissionFailure(err error) bool {
	if errors.Is(err, ErrUpstreamKeyCapacity) || errors.Is(err, ErrUpstreamKeyPolicy) {
		return true
	}
	var failover *UpstreamFailoverError
	return errors.As(err, &failover) && failover.Reason == UpstreamKeyAdmissionReason
}

// UpstreamKeyConcurrency is the optional shared authority used by native WS
// turns. Fingerprints come from the final handshake, including pooled reuse.
// Duplicate fingerprints consume one slot; the caller's slice is not modified.
// A nil release means no configured credential consumed capacity.
type UpstreamKeyConcurrency interface {
	AcquireUpstreamKeySlots(context.Context, []string) (context.Context, func(), error)
}

// Media and provider batches outlive their creation response. Their exact
// members stay reserved until upstream termination is explicitly confirmed.
// Only credential fingerprints are persisted, never authentication headers.
type PersistentUpstreamKeyResource struct {
	Fingerprint string `json:"fingerprint"`
	ResourceID  int64  `json:"resource_id,omitempty"`
}

type PersistentUpstreamKeyLease struct {
	Authority    string                          `json:"authority"`
	Member       string                          `json:"member"`
	Fingerprints []string                        `json:"fingerprints"`
	Resources    []PersistentUpstreamKeyResource `json:"resources"`
}

func (l *PersistentUpstreamKeyLease) Validate() error {
	if l == nil || (l.Authority != "redis" && l.Authority != "regional:primary" && !strings.HasPrefix(l.Authority, "regional:local:")) || len(l.Member) > 128 || len(l.Resources) == 0 {
		return ErrUpstreamKeyPolicy
	}
	batchID, attempt, hasAttempt := strings.Cut(strings.TrimPrefix(l.Member, "batch:"), ":")
	liveMember := strings.HasPrefix(l.Member, "live:") && len(l.Member) > len("live:")
	batchMember := strings.HasPrefix(l.Member, "batch:") && hasAttempt && IsValidBatchImageID(batchID) && uuid.Validate(attempt) == nil
	if !liveMember && !batchMember {
		return ErrUpstreamKeyPolicy
	}
	seen := make(map[string]bool, len(l.Fingerprints))
	for _, fingerprint := range l.Fingerprints {
		decoded, err := hex.DecodeString(fingerprint)
		if err != nil || len(decoded) != 32 || strings.ToLower(fingerprint) != fingerprint || seen[fingerprint] {
			return ErrUpstreamKeyPolicy
		}
		seen[fingerprint] = true
	}
	for _, resource := range l.Resources {
		if !seen[resource.Fingerprint] || resource.ResourceID < 0 || (l.Authority == "redis") != (resource.ResourceID == 0) {
			return ErrUpstreamKeyPolicy
		}
		delete(seen, resource.Fingerprint)
	}
	return nil
}

func (l *PersistentUpstreamKeyLease) MatchesBatch(batchID string) bool {
	return l != nil && l.Validate() == nil && IsValidBatchImageID(batchID) && strings.HasPrefix(l.Member, "batch:"+batchID+":")
}

// The HTTP transport, Live controller and batch worker share this port. Rechecking never
// creates a missing member; release is idempotent and survives process handoff.
type PersistentUpstreamKeyConcurrency interface {
	AcquirePersistentUpstreamKeySlots(context.Context, *http.Request) (*PersistentUpstreamKeyLease, error)
	CheckPersistentUpstreamKeySlots(context.Context, *PersistentUpstreamKeyLease) error
	ReleasePersistentUpstreamKeySlots(context.Context, *PersistentUpstreamKeyLease) error
}

// UpstreamKeyRequestAdmission reuses the HTTP transport's exact admission and
// response-body lifecycle with provider-specific HTTP clients.
type UpstreamKeyRequestAdmission interface {
	BeginUpstreamKeyRequest(*http.Request) (*http.Request, func(*http.Response, error), error)
}

type persistentUpstreamKeyLeaseContextKey struct{}

func WithPersistentUpstreamKeyLease(ctx context.Context, lease *PersistentUpstreamKeyLease) context.Context {
	return context.WithValue(ctx, persistentUpstreamKeyLeaseContextKey{}, lease)
}

func PersistentUpstreamKeyLeaseFromContext(ctx context.Context) *PersistentUpstreamKeyLease {
	lease, _ := ctx.Value(persistentUpstreamKeyLeaseContextKey{}).(*PersistentUpstreamKeyLease)
	return lease
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
