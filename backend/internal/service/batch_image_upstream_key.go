package service

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/google/uuid"
)

type BatchImageUpstreamKeyAuthority interface {
	PersistentUpstreamKeyConcurrency
	UpstreamKeyRequestAdmission
}

type batchImageKeyScopeKey struct{}
type batchImageKeyCreationKey struct{}

type batchImageKeyScope struct {
	registry *BatchImageProviderRegistry
	repo     BatchImageRepository
	job      *BatchImageJob
	member   string
}

func batchImageSharedKeyConfigured(cfg *config.Config) bool {
	return cfg != nil && (cfg.Gateway.UpstreamKeyConcurrency.Configured() ||
		cfg.Runtime.RegionalConcurrencyFile != "" || cfg.Runtime.RegionalSchedulerFile != "")
}

func (r *BatchImageProviderRegistry) PrepareRequest(ctx context.Context, provider BatchImageProvider, job *BatchImageJob, account *Account, lookup func(context.Context, int64) (*Account, error), repo BatchImageRepository) (context.Context, error) {
	ctx, err := prepareBatchImageProviderRequest(ctx, provider, job, account, lookup)
	if err != nil {
		return ctx, err
	}
	if r == nil || (r.KeyAuthority == nil && (batchImageSharedKeyConfigured(r.config) || job.UpstreamKeyLease != nil)) {
		return ctx, ErrBatchImageSharedKeyUnavailable
	}
	if job.UpstreamKeyLease != nil && !job.UpstreamKeyLease.MatchesBatch(job.BatchID) {
		return ctx, ErrBatchImageSharedKeyUnavailable
	}
	member := "batch:" + job.BatchID + ":" + uuid.NewString()
	if job.UpstreamKeyLease != nil {
		member = job.UpstreamKeyLease.Member
	}
	return context.WithValue(ctx, batchImageKeyScopeKey{}, &batchImageKeyScope{registry: r, repo: repo, job: job, member: member}), nil
}

// The opaque scope is installed only after the persisted route is verified.
// It cannot be supplied by a downstream request header or payload.
func BatchImageKeyRequestMember(req *http.Request) string {
	if req == nil {
		return ""
	}
	scope, _ := req.Context().Value(batchImageKeyScopeKey{}).(*batchImageKeyScope)
	if scope == nil || scope.job == nil || !IsValidBatchImageID(scope.job.BatchID) {
		return ""
	}
	return scope.member
}

func BatchImageKeyPolicyMatches(req *http.Request, revision int64, digest string) bool {
	if req == nil {
		return true
	}
	scope, _ := req.Context().Value(batchImageKeyScopeKey{}).(*batchImageKeyScope)
	if scope == nil || scope.registry.config == nil {
		return true
	}
	policy := scope.registry.config.Gateway.UpstreamKeyConcurrency
	_, expected, err := policy.EncodeLimits()
	return err == nil && policy.Revision == revision && digest == expected
}

func BatchImageKeyPolicyRequired(req *http.Request) bool {
	if req == nil {
		return false
	}
	scope, _ := req.Context().Value(batchImageKeyScopeKey{}).(*batchImageKeyScope)
	return scope != nil && (batchImageSharedKeyConfigured(scope.registry.config) || scope.job.UpstreamKeyLease != nil)
}

func BatchImageKeyCreationMember(req *http.Request) string {
	member := BatchImageKeyRequestMember(req)
	if member == "" || req.URL == nil || req.Method != http.MethodPost || req.Context().Value(batchImageKeyCreationKey{}) != true {
		return ""
	}
	scope, ok := req.Context().Value(batchImageKeyScopeKey{}).(*batchImageKeyScope)
	if !ok || scope == nil {
		return ""
	}
	if scope.job.Status != BatchImageJobStatusUploading || strings.TrimSpace(batchImageDerefString(scope.job.ProviderJobName)) != "" {
		return ""
	}
	if scope.job.Provider == BatchImageProviderGeminiAPI && strings.Contains(req.URL.Path, "/models/") && strings.HasSuffix(req.URL.Path, ":batchGenerateContent") {
		return member
	}
	if scope.job.Provider == BatchImageProviderVertex && strings.HasSuffix(req.URL.Path, "/batchPredictionJobs") {
		return member
	}
	return ""
}

func (s *batchImageKeyScope) begin(req *http.Request) (*http.Request, func(*http.Response, error), error) {
	noop := func(*http.Response, error) {}
	authority := s.registry.KeyAuthority
	if authority == nil {
		return req, noop, nil
	}
	if BatchImageKeyCreationMember(req) != "" {
		// A persisted member means a previous creation may already have run.
		// A caller must query that task, never recreate it with the same slot.
		if s.job.UpstreamKeyLease != nil || s.repo == nil {
			return req, nil, ErrBatchImageSharedKeyUnavailable.WithCause(ErrUpstreamKeyPolicy)
		}
		lease, err := authority.AcquirePersistentUpstreamKeySlots(req.Context(), req)
		if err != nil {
			return req, nil, ErrBatchImageSharedKeyUnavailable.WithCause(err)
		}
		if lease != nil {
			if !lease.MatchesBatch(s.job.BatchID) || lease.Member != s.member {
				return req, nil, ErrBatchImageSharedKeyUnavailable.WithCause(ErrUpstreamKeyPolicy)
			}
			if err := s.repo.BindBatchImageUpstreamKeyLease(req.Context(), s.job.BatchID, s.job.RouteFingerprint, lease); err != nil {
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(req.Context()), 3*time.Second)
				_ = authority.ReleasePersistentUpstreamKeySlots(cleanupCtx, lease)
				cancel()
				return req, nil, ErrBatchImageSharedKeyUnavailable.WithCause(ErrUpstreamKeyPolicy)
			}
			s.job.UpstreamKeyLease = lease
		}
	}
	if s.job.UpstreamKeyLease != nil && s.job.UpstreamKeyReleasedAt == nil {
		req = req.WithContext(WithPersistentUpstreamKeyLease(req.Context(), s.job.UpstreamKeyLease))
	}
	bound, finish, err := authority.BeginUpstreamKeyRequest(req)
	if err != nil {
		return req, nil, ErrBatchImageSharedKeyUnavailable.WithCause(err)
	}
	return bound, finish, nil
}

// Confirmation is persisted before release so a restarted worker can retry an
// acknowledged terminal result without allocating or recreating any member.
func (r *BatchImageProviderRegistry) releaseUpstreamKey(ctx context.Context, repo BatchImageRepository, job *BatchImageJob, confirmed bool) error {
	if job == nil || job.UpstreamKeyLease == nil || job.UpstreamKeyReleasedAt != nil {
		return nil
	}
	if r == nil || r.KeyAuthority == nil || repo == nil || !job.UpstreamKeyLease.MatchesBatch(job.BatchID) {
		return ErrBatchImageSharedKeyUnavailable
	}
	if confirmed && job.UpstreamKeyTerminalAt == nil {
		if err := repo.ConfirmBatchImageUpstreamKeyTerminal(ctx, job.BatchID, job.UpstreamKeyLease); err != nil {
			return err
		}
		now := time.Now()
		job.UpstreamKeyTerminalAt = &now
	}
	if job.UpstreamKeyTerminalAt == nil {
		return ErrBatchImageSharedKeyUnavailable
	}
	if err := r.KeyAuthority.ReleasePersistentUpstreamKeySlots(ctx, job.UpstreamKeyLease); err != nil {
		return err
	}
	if err := repo.MarkBatchImageUpstreamKeyReleased(ctx, job.BatchID, job.UpstreamKeyLease); err != nil {
		return err
	}
	now := time.Now()
	job.UpstreamKeyReleasedAt = &now
	return nil
}

func batchImageProviderSupportsPersistentKey(provider BatchImageProvider) bool {
	switch p := provider.(type) {
	case *GeminiAPIBatchImageProvider:
		_, ok := p.client.(*GeminiBatchHTTPClient)
		return ok
	case *VertexBatchImageProvider:
		_, clientOK := p.client.(*VertexBatchHTTPClient)
		_, storeOK := p.objectStore.(*VertexGCSObjectStore)
		return clientOK && storeOK
	default:
		return false
	}
}

func isBatchImageConfirmedTerminal(status *BatchProviderStatus) bool {
	if status == nil || !status.Done {
		return false
	}
	switch status.InternalState {
	case BatchProviderStateSucceeded, BatchProviderStateFailed, BatchProviderStateCancelled, BatchProviderStateExpired:
		return true
	default:
		return false
	}
}
