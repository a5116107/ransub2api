package service

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/regional/video"
	"github.com/google/uuid"
)

var ErrUpstreamKeyCapacity = errors.New("upstream key concurrency capacity unavailable")
var ErrUpstreamKeyPolicy = errors.New("upstream key concurrency policy unavailable or revision conflict")
var ErrUpstreamKeyLease = errors.New("upstream key concurrency lease lost")

func upstreamKeyAdmissionRequired(cfg *config.Config) bool {
	return cfg != nil && (cfg.Gateway.UpstreamKeyConcurrency.Configured() ||
		cfg.Runtime.RegionalConcurrencyFile != "" || cfg.Runtime.RegionalSchedulerFile != "")
}

type pluginUpstreamKeyPolicyContextKey struct{}

func pluginUpstreamKeyPolicyContext(ctx context.Context, cfg *config.Config) context.Context {
	if !upstreamKeyAdmissionRequired(cfg) {
		return ctx
	}
	return context.WithValue(ctx, pluginUpstreamKeyPolicyContextKey{}, true)
}

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
	videoMember := strings.HasPrefix(l.Member, "video:") && uuid.Validate(strings.TrimPrefix(l.Member, "video:")) == nil
	if !liveMember && !batchMember && !videoMember {
		return ErrUpstreamKeyPolicy
	}
	seen := make(map[string]bool, len(l.Fingerprints))
	for _, fingerprint := range l.Fingerprints {
		if fingerprint == config.UnverifiedUpstreamAgentCredential {
			return ErrUpstreamKeyPolicy
		}
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

// AsyncVideoKeyOwner identifies the authenticated task owner and the
// selected account. A task identifier alone is not globally unique.
type AsyncVideoKeyOwner = video.Owner

type asyncVideoKeyOwnerContextKey struct{}

func WithAsyncVideoKeyOwner(ctx context.Context, owner AsyncVideoKeyOwner) context.Context {
	return context.WithValue(ctx, asyncVideoKeyOwnerContextKey{}, owner)
}

func AsyncVideoKeyOwnerFromContext(ctx context.Context) (AsyncVideoKeyOwner, bool) {
	if ctx == nil {
		return AsyncVideoKeyOwner{}, false
	}
	owner, ok := ctx.Value(asyncVideoKeyOwnerContextKey{}).(AsyncVideoKeyOwner)
	return owner, ok && owner.Valid()
}

// AsyncVideoKeyHandoff is the in-process handle for a video creation member.
// The member is created before the upstream POST and the lease is attached by
// the final HTTP admission layer. The handle deliberately contains no
// authentication material; the durable record stores owner and route identity
// with the lease's fingerprints and slot resource IDs.
type AsyncVideoKeyHandoff struct {
	mu      sync.RWMutex
	member  string
	lease   *PersistentUpstreamKeyLease
	owner   AsyncVideoKeyOwner
	started bool
}

func NewAsyncVideoKeyHandoff(owner AsyncVideoKeyOwner) *AsyncVideoKeyHandoff {
	return &AsyncVideoKeyHandoff{member: "video:" + uuid.NewString(), owner: owner}
}

func WithAsyncVideoKeyHandoff(ctx context.Context, handoff *AsyncVideoKeyHandoff) context.Context {
	if handoff == nil {
		return ctx
	}
	return context.WithValue(ctx, asyncVideoKeyHandoffContextKey{}, handoff)
}

func AsyncVideoKeyHandoffFromContext(ctx context.Context) *AsyncVideoKeyHandoff {
	if ctx == nil {
		return nil
	}
	handoff, _ := ctx.Value(asyncVideoKeyHandoffContextKey{}).(*AsyncVideoKeyHandoff)
	return handoff
}

func AsyncVideoKeyHandoffFromRequest(req *http.Request) *AsyncVideoKeyHandoff {
	if req == nil {
		return nil
	}
	return AsyncVideoKeyHandoffFromContext(req.Context())
}

func AsyncVideoKeyHandoffMember(req *http.Request) string {
	handoff := AsyncVideoKeyHandoffFromRequest(req)
	if handoff == nil {
		return ""
	}
	return handoff.Member()
}

func (h *AsyncVideoKeyHandoff) Member() string {
	if h == nil {
		return ""
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.member
}

func (h *AsyncVideoKeyHandoff) Owner() AsyncVideoKeyOwner {
	if h == nil {
		return AsyncVideoKeyOwner{}
	}
	return h.owner
}

func (h *AsyncVideoKeyHandoff) Lease() *PersistentUpstreamKeyLease {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.lease
}

func (h *AsyncVideoKeyHandoff) SetLease(lease *PersistentUpstreamKeyLease) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.lease = lease
	h.mu.Unlock()
}

func (h *AsyncVideoKeyHandoff) TryStart() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.started {
		return false
	}
	h.started = true
	return true
}

type asyncVideoKeyHandoffContextKey struct{}

// AsyncVideoKeyLeaseController persists the provisional member before the
// POST, binds it to the provider task ID after an acknowledged response, and
// releases it only after a terminal task observation. The HTTP transport owns
// the implementation so direct providers, Astra routing and future instances
// use the same upstream-key authority.
type AsyncVideoKeyLeaseController interface {
	PersistAsyncVideoKeyLease(context.Context, *AsyncVideoKeyHandoff) error
	BindAsyncVideoKeyLease(context.Context, *AsyncVideoKeyHandoff, string) error
	BindAsyncVideoKeyLeaseByMember(context.Context, string, string) error
	MarkAsyncVideoKeySubmissionUnknown(context.Context, *AsyncVideoKeyHandoff) error
	ReleaseAsyncVideoKeyLease(context.Context, *AsyncVideoKeyHandoff) error
	ResolveAsyncVideoKeyOwner(context.Context, AsyncVideoKeyOwner, string) (*AsyncVideoKeyOwner, error)
	LoadAsyncVideoKeyLeaseByTask(context.Context, AsyncVideoKeyOwner, string) (*PersistentUpstreamKeyLease, error)
	ReleaseAsyncVideoKeyLeaseByTask(context.Context, AsyncVideoKeyOwner, string) error
	ReleaseAsyncVideoKeyLeaseByMember(context.Context, string) error
}

// AsyncVideoKeyReleaseRecovery retries only releases that were already fenced.
type AsyncVideoKeyReleaseRecovery interface {
	RecoverAsyncVideoKeyReleases(context.Context, int) (int, error)
	// A gateway replica may recover only its own regional SQL authority.
	IsLocalAsyncVideoKeyReleaseRecovery() bool
}

type asyncVideoKeyLookupContextKey struct{}

type asyncVideoKeyLookup struct {
	Member      string
	TaskID      string
	Credentials []string
	Method      string
	URL         string
	Host        string
}

// WithAsyncVideoKeyLookup binds a task and optional active lease to one status
// request. A nil lease never proves completion; regional SQL must confirm it.
func WithAsyncVideoKeyLookup(req *http.Request, lease *PersistentUpstreamKeyLease, taskID string) *http.Request {
	if req == nil || req.URL == nil || strings.TrimSpace(taskID) == "" {
		return req
	}
	member := ""
	if lease != nil {
		member = lease.Member
	}
	ctx := WithPersistentUpstreamKeyLease(req.Context(), lease)
	credentials := config.UpstreamCredentialFingerprints(req)
	slices.Sort(credentials)
	ctx = context.WithValue(ctx, asyncVideoKeyLookupContextKey{}, asyncVideoKeyLookup{
		Member: member, TaskID: strings.TrimSpace(taskID), Credentials: credentials, Method: req.Method, URL: req.URL.String(), Host: req.Host,
	})
	return req.WithContext(ctx)
}

func AsyncVideoKeyLookupMatches(req *http.Request, lease *PersistentUpstreamKeyLease) bool {
	if _, ok := AsyncVideoKeyLookupTaskID(req); !ok || lease == nil {
		return false
	}
	binding, ok := req.Context().Value(asyncVideoKeyLookupContextKey{}).(asyncVideoKeyLookup)
	return ok && binding.Member == lease.Member
}

func AsyncVideoKeyLookupTaskID(req *http.Request) (string, bool) {
	if req == nil || req.URL == nil {
		return "", false
	}
	binding, ok := req.Context().Value(asyncVideoKeyLookupContextKey{}).(asyncVideoKeyLookup)
	if !ok {
		return "", false
	}
	credentials := config.UpstreamCredentialFingerprints(req)
	slices.Sort(credentials)
	return binding.TaskID, binding.TaskID != "" && (req.Method == http.MethodGet || req.Method == http.MethodDelete) && binding.Method == req.Method && binding.URL == req.URL.String() && binding.Host == req.Host && slices.Equal(binding.Credentials, credentials)
}

// UpstreamKeyRequestAdmission reuses the HTTP transport's exact admission and
// response-body lifecycle with provider-specific HTTP clients.
type UpstreamKeyRequestAdmission interface {
	BeginUpstreamKeyRequest(*http.Request) (*http.Request, func(*http.Response, error), error)
}

// AsyncVideoKeyGuard rejects restricted asynchronous video creates at the
// final HTTP admission boundary until their remote lifetime has durable ownership.
type AsyncVideoKeyGuard interface {
	SupportsAsyncVideoKeyGuard() bool
}

type asyncVideoKeyGuardContextKey struct{}

type asyncVideoKeyGuardState struct{ required bool }

func WithAsyncVideoKeyGuard(ctx context.Context, required bool) context.Context {
	return context.WithValue(ctx, asyncVideoKeyGuardContextKey{}, asyncVideoKeyGuardState{required: required})
}

func AsyncVideoKeyGuardState(req *http.Request) (active, required bool) {
	if req == nil {
		return false, false
	}
	state, ok := req.Context().Value(asyncVideoKeyGuardContextKey{}).(asyncVideoKeyGuardState)
	return ok, state.required
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
