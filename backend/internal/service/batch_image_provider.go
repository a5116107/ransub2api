package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

type BatchImageProvider interface {
	Name() string
	RouteFingerprint(account *Account) string
	SupportsAccount(account *Account) bool
	Submit(ctx context.Context, job *BatchImageJob, account *Account, input BatchImageInput) (*BatchProviderJob, error)
	Get(ctx context.Context, job *BatchImageJob, account *Account) (*BatchProviderStatus, error)
	Cancel(ctx context.Context, job *BatchImageJob, account *Account) error
	OpenResult(ctx context.Context, job *BatchImageJob, account *Account) (io.ReadCloser, string, error)
	Cleanup(ctx context.Context, job *BatchImageJob, account *Account, target CleanupTarget) error
}

// Only providers that can prove creation never started (or was explicitly
// rejected) may authorize releasing the submission hold on an error.
var errBatchImageSubmitRejected = errors.New("batch image creation was not accepted")

type batchImageRequestGuardKey struct{}

func batchImageRouteDigest(parts ...string) string {
	encoded, _ := json.Marshal(parts)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

// Bind the persisted creation route before any provider operation. The real
// HTTP clients repeat this check after uploads, token waits and pagination.
func prepareBatchImageProviderRequest(ctx context.Context, provider BatchImageProvider, job *BatchImageJob, account *Account, lookup func(context.Context, int64) (*Account, error)) (context.Context, error) {
	if provider == nil || job == nil || account == nil || lookup == nil ||
		job.AccountID == nil || *job.AccountID <= 0 || account.ID != *job.AccountID ||
		job.Provider != provider.Name() || job.RouteFingerprint == "" ||
		job.RouteFingerprint != provider.RouteFingerprint(account) {
		return ctx, ErrBatchImageRouteUnavailable
	}
	accountID, fingerprint := account.ID, job.RouteFingerprint
	guard := func(requestCtx context.Context) error {
		readCtx, cancel := context.WithTimeout(requestCtx, 3*time.Second)
		defer cancel()
		current, err := lookup(readCtx, accountID)
		if err != nil || current == nil || current.ID != accountID ||
			fingerprint != provider.RouteFingerprint(current) {
			return ErrBatchImageRouteUnavailable
		}
		return nil
	}
	if err := guard(ctx); err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, batchImageRequestGuardKey{}, guard), nil
}

func doBatchImageHTTPRequest(client *http.Client, req *http.Request) (*http.Response, error) {
	if guard, ok := req.Context().Value(batchImageRequestGuardKey{}).(func(context.Context) error); ok {
		if err := guard(req.Context()); err != nil {
			return nil, err
		}
		// A redirect has no creation-route proof. Copy the shared client rather
		// than changing redirect behavior for unrelated users of its transport.
		guardedClient := *client
		guardedClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		return guardedClient.Do(req)
	}
	return client.Do(req)
}

func batchImageCreateError(original, mapped error) error {
	if errors.Is(original, ErrBatchImageRouteUnavailable) {
		return errors.Join(mapped, errBatchImageSubmitRejected)
	}
	var geminiErr *GeminiAPIError
	var vertexErr *VertexAPIError
	status := 0
	if errors.As(original, &geminiErr) {
		status = geminiErr.StatusCode
	} else if errors.As(original, &vertexErr) {
		status = vertexErr.StatusCode
	}
	switch status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
		http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusRequestEntityTooLarge,
		http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity, http.StatusTooManyRequests:
		return errors.Join(mapped, errBatchImageSubmitRejected)
	default:
		// Timeouts, conflicts, transport errors, malformed success and 5xx can
		// arrive after creation. They do not establish that nothing is running.
		return mapped
	}
}

type BatchImageProviderRegistry struct {
	providers map[string]BatchImageProvider
}

func NewBatchImageProviderRegistry(providers ...BatchImageProvider) *BatchImageProviderRegistry {
	r := &BatchImageProviderRegistry{providers: make(map[string]BatchImageProvider, len(providers))}
	for _, provider := range providers {
		if provider == nil || strings.TrimSpace(provider.Name()) == "" {
			continue
		}
		r.providers[provider.Name()] = provider
	}
	return r
}

func NewDefaultBatchImageProviderRegistry() *BatchImageProviderRegistry {
	return NewBatchImageProviderRegistry(
		NewGeminiAPIBatchImageProvider(nil),
		NewVertexBatchImageProvider(VertexBatchImageProviderOptions{}, nil, nil, nil),
	)
}

func NewBatchImageProviderRegistryFromConfig(cfg *config.Config) *BatchImageProviderRegistry {
	return NewBatchImageProviderRegistry(
		NewGeminiAPIBatchImageProvider(nil),
		NewVertexBatchImageProviderFromConfig(cfg, nil, nil, nil),
	)
}

func (r *BatchImageProviderRegistry) Get(provider string) (BatchImageProvider, bool) {
	if r == nil {
		return nil, false
	}
	p, ok := r.providers[provider]
	return p, ok
}

func (r *BatchImageProviderRegistry) MustGet(provider string) (BatchImageProvider, error) {
	p, ok := r.Get(provider)
	if !ok {
		return nil, ErrBatchImageInvalidProvider
	}
	return p, nil
}

type BatchImageInput struct {
	BatchID     string
	Model       string
	DisplayName string
	Items       []BatchImageInputItem

	ResponseMimeType string
	AspectRatio      string
	ImageSize        string

	Metadata map[string]string
}

type BatchImageInputItem struct {
	CustomID string
	Prompt   string

	ReferenceImages []BatchImageReference
}

type BatchImageReference struct {
	ID       string
	Type     string
	MimeType string
	Data     []byte
	FileURI  string
}

type BatchProviderJob struct {
	ProviderJobName   string
	ProviderInputRef  string
	ProviderOutputRef string
	RawState          string
}

type BatchProviderInternalState string

const (
	BatchProviderStateQueued    BatchProviderInternalState = "queued"
	BatchProviderStateRunning   BatchProviderInternalState = "running"
	BatchProviderStateSucceeded BatchProviderInternalState = "succeeded"
	BatchProviderStateFailed    BatchProviderInternalState = "failed"
	BatchProviderStateCancelled BatchProviderInternalState = "cancelled"
	BatchProviderStateExpired   BatchProviderInternalState = "expired"
)

type BatchProviderStatus struct {
	RawState string

	InternalState BatchProviderInternalState
	Done          bool

	ProviderOutputRef string

	ErrorCode    string
	ErrorMessage string

	SuggestedRequeueAfter time.Duration
}

type CleanupTarget string

const (
	CleanupTargetInput  CleanupTarget = "input"
	CleanupTargetOutput CleanupTarget = "output"
	CleanupTargetAll    CleanupTarget = "all"
)

var (
	ErrBatchImageProviderUnsupportedAccount      = infraerrors.New(http.StatusBadRequest, "BATCH_IMAGE_PROVIDER_UNSUPPORTED_ACCOUNT", "batch image provider does not support this account")
	ErrBatchImageProviderMissingAPIKey           = infraerrors.New(http.StatusBadRequest, "BATCH_IMAGE_PROVIDER_MISSING_API_KEY", "batch image provider account is missing api key")
	ErrBatchImageProviderMissingServiceAccount   = infraerrors.New(http.StatusBadRequest, "BATCH_IMAGE_PROVIDER_MISSING_SERVICE_ACCOUNT", "batch image provider account is missing service account credentials")
	ErrBatchImageProviderMissingJobName          = infraerrors.New(http.StatusBadRequest, "BATCH_IMAGE_PROVIDER_MISSING_JOB_NAME", "batch image provider job name is missing")
	ErrBatchImageProviderMissingResultRef        = infraerrors.New(http.StatusBadRequest, "BATCH_IMAGE_PROVIDER_MISSING_RESULT_REF", "batch image provider result reference is missing")
	ErrBatchImageProviderInlineResultUnsupported = infraerrors.New(http.StatusBadRequest, "GEMINI_INLINE_BATCH_RESULT_UNSUPPORTED", "Gemini inline batch result is not supported")
	ErrBatchImageProviderInvalidInput            = infraerrors.New(http.StatusBadRequest, "BATCH_IMAGE_PROVIDER_INVALID_INPUT", "invalid batch image provider input")
	ErrBatchImageProviderUnsafeCleanupPath       = infraerrors.New(http.StatusBadRequest, "VERTEX_UNSAFE_CLEANUP_PATH", "unsafe batch image cleanup path")
	ErrUnsupportedCleanupTarget                  = infraerrors.New(http.StatusBadRequest, "BATCH_IMAGE_PROVIDER_UNSUPPORTED_CLEANUP_TARGET", "unsupported batch image cleanup target")
)

func batchImageProviderJobName(job *BatchImageJob) string {
	if job == nil || job.ProviderJobName == nil {
		return ""
	}
	return strings.TrimSpace(*job.ProviderJobName)
}

func batchImageProviderInputRef(job *BatchImageJob) string {
	if job == nil || job.ProviderInputRef == nil {
		return ""
	}
	return strings.TrimSpace(*job.ProviderInputRef)
}

func batchImageProviderOutputRef(job *BatchImageJob) string {
	if job == nil || job.ProviderOutputRef == nil {
		return ""
	}
	return strings.TrimSpace(*job.ProviderOutputRef)
}

func batchImageProviderAPIKey(account *Account) string {
	if account == nil {
		return ""
	}
	return strings.TrimSpace(account.GetCredential("api_key"))
}

func batchImageProviderInputError(format string, args ...any) error {
	return ErrBatchImageProviderInvalidInput.WithCause(fmt.Errorf(format, args...))
}
