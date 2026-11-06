//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestBatchImagePersistentKeySubmissionOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		uploadErr bool
		bindErr   bool
		uncertain bool
	}{
		{name: "accepted", status: 200, body: `{"name":"batches/task","state":"JOB_STATE_RUNNING"}`},
		{name: "explicit rejection", status: 400, body: `{"error":{"message":"invalid batch"}}`},
		{name: "request timeout", status: 408, body: `{}`, uncertain: true},
		{name: "conflict", status: 409, body: `{}`, uncertain: true},
		{name: "server failure", status: 503, body: `{}`, uncertain: true},
		{name: "malformed success", status: 200, body: `{`, uncertain: true},
		{name: "missing job name", status: 200, body: `{}`, uncertain: true},
		{name: "upload rejected", status: 429, body: `{}`, uploadErr: true},
		{name: "binding failure", status: 200, body: `{}`, bindErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var creates atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				body, code := `{"file":{"name":"files/input"}}`, http.StatusOK
				if strings.HasSuffix(req.URL.Path, ":batchGenerateContent") {
					creates.Add(1)
					body, code = tc.body, tc.status
				} else if tc.uploadErr {
					body, code = tc.body, tc.status
				}
				w.WriteHeader(code)
				if _, err := io.WriteString(w, body); err != nil {
					t.Errorf("write provider response: %v", err)
				}
			}))
			defer server.Close()
			svc, repo, _, _, _ := newTestBatchImagePublicService(true)
			provider := NewGeminiAPIBatchImageProvider(NewGeminiBatchHTTPClient(server.URL, server.Client()))
			authority := newBatchImageKeyAuthorityStub()
			svc.Config.Gateway.UpstreamKeyConcurrency = config.UpstreamKeyConcurrencyConfig{Revision: 1, Limits: map[string]int{config.UpstreamKeyFingerprint("test-secret"): 1}}
			svc.ProviderRegistry = NewBatchImageProviderRegistry(provider)
			svc.ProviderRegistry.config, svc.ProviderRegistry.KeyAuthority = svc.Config, authority
			if tc.bindErr {
				repo.keyBindErr = errors.New("binding database unavailable")
			}
			result, err := svc.Submit(t.Context(), testBatchImageOwner(), validBatchImageSubmitRequest(), "fixed-request")
			require.Len(t, repo.jobs, 1)
			var job *BatchImageJob
			for _, item := range repo.jobs {
				job = item
			}
			require.NotNil(t, job)
			if tc.uploadErr {
				require.Error(t, err)
				require.Zero(t, creates.Load())
				require.Zero(t, authority.acquires)
				return
			}
			require.Equal(t, 1, authority.acquires)
			if tc.bindErr {
				require.Error(t, err)
				require.Zero(t, creates.Load(), "persist exact ownership before starting generation")
				require.Equal(t, 1, authority.releases)
				return
			}
			require.EqualValues(t, 1, creates.Load())
			require.NotNil(t, job.UpstreamKeyLease)
			require.True(t, job.UpstreamKeyLease.MatchesBatch(job.BatchID))
			if tc.uncertain {
				require.ErrorIs(t, err, ErrBatchImageSubmitUnconfirmed)
				require.Equal(t, BatchImageJobStatusUploading, job.Status)
				require.Nil(t, job.UpstreamKeyTerminalAt)
				require.Zero(t, authority.releases)
				repeated, lookupErr := svc.Submit(t.Context(), testBatchImageOwner(), validBatchImageSubmitRequest(), "fixed-request")
				require.NoError(t, lookupErr)
				require.Equal(t, job.BatchID, repeated.ID)
				require.EqualValues(t, 1, creates.Load(), "unknown creation must not be replayed")
				return
			}
			if tc.status >= 400 {
				require.Error(t, err)
				require.Equal(t, 1, authority.releases)
				require.NotNil(t, job.UpstreamKeyTerminalAt)
				require.NotNil(t, job.UpstreamKeyReleasedAt)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "queued", result.Status)
			require.Nil(t, job.UpstreamKeyTerminalAt)
			require.Zero(t, authority.releases, "creation response close does not stop remote work")
			require.Contains(t, authority.requestMembers, job.UpstreamKeyLease.Member)
		})
	}
}

func TestBatchImagePersistentKeyConfirmedReleaseRecovery(t *testing.T) {
	var remoteState atomic.Value
	remoteState.Store("JOB_STATE_RUNNING")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body := `{"file":{"name":"files/input"}}`
		if strings.HasSuffix(req.URL.Path, ":cancel") {
			body = `{}`
		} else if !strings.Contains(req.URL.Path, "/upload/") {
			state, ok := remoteState.Load().(string)
			if !ok {
				t.Error("batch provider state must be a string")
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			body = `{"name":"batches/task","state":"` + state + `"}`
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Errorf("write provider response: %v", err)
		}
	}))
	defer server.Close()
	svc, repo, _, _, _ := newTestBatchImagePublicService(true)
	provider := NewGeminiAPIBatchImageProvider(NewGeminiBatchHTTPClient(server.URL, server.Client()))
	authority := newBatchImageKeyAuthorityStub()
	svc.ProviderRegistry = NewBatchImageProviderRegistry(provider)
	svc.ProviderRegistry.KeyAuthority = authority
	created, err := svc.Submit(t.Context(), testBatchImageOwner(), validBatchImageSubmitRequest(), "owned-request")
	require.NoError(t, err)
	_, err = svc.Cancel(t.Context(), testBatchImageOwner(), created.ID)
	require.NoError(t, err)
	require.Zero(t, authority.releases, "a cancellation response is not terminal evidence")
	processor := &BatchImageProviderProcessor{Repo: repo, ProviderRegistry: svc.ProviderRegistry,
		AccountResolver: &BatchImageAccountRepositoryResolver{Repo: svc.AccountRepo}, BillingRepo: svc.BillingRepo}
	result, err := processor.Process(t.Context(), created.ID)
	require.NoError(t, err)
	require.False(t, result.Terminal)
	require.Zero(t, authority.releases)
	remoteState.Store("JOB_STATE_CANCELLED")
	repo.keyConfirmErr = errors.New("terminal confirmation database unavailable")
	_, err = processor.Process(t.Context(), created.ID)
	require.Error(t, err)
	require.Nil(t, repo.jobs[created.ID].UpstreamKeyTerminalAt)
	require.Zero(t, authority.releases, "persist terminal evidence before giving capacity back")
	repo.keyConfirmErr = nil
	authority.releaseErr = errors.New("quota authority unavailable")
	_, err = processor.Process(t.Context(), created.ID)
	require.Error(t, err)
	job := repo.jobs[created.ID]
	require.NotNil(t, job.UpstreamKeyTerminalAt)
	require.Nil(t, job.UpstreamKeyReleasedAt)
	authority.releaseErr = nil
	repo.keyReleaseErr = errors.New("release acknowledgement database unavailable")
	_, err = processor.Process(t.Context(), created.ID)
	require.Error(t, err)
	require.Equal(t, 1, authority.releases)
	require.Nil(t, job.UpstreamKeyReleasedAt)
	repo.keyReleaseErr = nil
	result, err = processor.Process(t.Context(), created.ID)
	require.NoError(t, err)
	require.True(t, result.Terminal)
	require.Equal(t, BatchImageJobStatusCancelled, job.Status)
	require.NotNil(t, job.UpstreamKeyReleasedAt)
	require.Equal(t, 1, authority.releases, "retry only releases the same member")
	require.Empty(t, authority.requestMembers[len(authority.requestMembers)-1], "recover release before a new provider request")
}

func TestBatchImagePersistentKeyRecoveryWithoutQueue(t *testing.T) {
	repo := newFakeBatchImageRepository()
	authority := newBatchImageKeyAuthorityStub()
	registry := NewBatchImageProviderRegistry()
	registry.KeyAuthority = authority
	now := time.Now()
	for _, batchID := range []string{"imgbatch_confirmed", "imgbatch_unknown"} {
		fingerprint := config.UpstreamKeyFingerprint("test-secret")
		lease := &PersistentUpstreamKeyLease{Authority: "redis", Member: "batch:" + batchID + ":" + uuid.NewString(),
			Fingerprints: []string{fingerprint}, Resources: []PersistentUpstreamKeyResource{{Fingerprint: fingerprint}}}
		repo.jobs[batchID] = &BatchImageJob{BatchID: batchID, Status: BatchImageJobStatusFailed, UpstreamKeyLease: lease}
		repo.keyBound[batchID] = true
	}
	repo.jobs["imgbatch_confirmed"].UpstreamKeyTerminalAt = &now
	recovery := &BatchImageBillingRecoveryService{Repo: repo, ProviderRegistry: registry}
	authority.releaseErr = errors.New("quota authority offline")
	released, err := recovery.ReleaseConfirmedUpstreamKeyOnce(t.Context())
	require.Error(t, err)
	require.Zero(t, released)
	require.Nil(t, repo.jobs["imgbatch_confirmed"].UpstreamKeyReleasedAt)
	authority.releaseErr = nil
	released, err = recovery.ReleaseConfirmedUpstreamKeyOnce(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, released)
	require.Equal(t, 1, authority.releases)
	require.NotNil(t, repo.jobs["imgbatch_confirmed"].UpstreamKeyReleasedAt)
	require.Nil(t, repo.jobs["imgbatch_unknown"].UpstreamKeyReleasedAt, "local failed state does not prove upstream termination")
	released, err = recovery.ReleaseConfirmedUpstreamKeyOnce(t.Context())
	require.NoError(t, err)
	require.Zero(t, released)
	require.Equal(t, 1, authority.releases)
}

func TestBatchImagePersistentKeyAdmissionRemainsLocal(t *testing.T) {
	for _, err := range []error{ErrBatchImageSharedKeyUnavailable.WithCause(ErrUpstreamKeyCapacity), ErrBatchImageSharedKeyUnavailable.WithCause(ErrUpstreamKeyPolicy)} {
		require.ErrorIs(t, mapGeminiClientError(err), ErrBatchImageSharedKeyUnavailable)
		require.ErrorIs(t, mapVertexClientError(err), ErrBatchImageSharedKeyUnavailable)
		require.ErrorIs(t, batchImageProviderSubmitPublicError(err), ErrBatchImageSharedKeyUnavailable)
		require.ErrorIs(t, batchImageCreateError(err, mapGeminiClientError(err)), errBatchImageSubmitRejected)
	}
}

func TestBatchImagePersistentKeyVertexCreationAndManagement(t *testing.T) {
	const credential = "vertex-batch-key-fixture"
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		sends.Add(1)
		if req.Header.Get("Authorization") != "Bearer "+credential {
			t.Error("Vertex batch and storage requests must use their resolved credential")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body := `{}`
		switch {
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/batchPredictionJobs"):
			body = `{"name":"projects/proj/locations/global/batchPredictionJobs/created","state":"JOB_STATE_RUNNING"}`
		case req.Method == http.MethodGet:
			body = `{"name":"projects/proj/locations/global/batchPredictionJobs/created","state":"JOB_STATE_CANCELLED"}`
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Errorf("write Vertex key fixture: %v", err)
		}
	}))
	defer server.Close()
	provider := newTestVertexProvider(NewVertexBatchHTTPClient(server.URL, server.Client()), NewVertexGCSObjectStore(server.URL, server.Client()))
	provider.tokenCache = &fakeGeminiTokenCache{token: credential}
	account := vertexServiceAccount()
	account.ID = 202
	job := &BatchImageJob{BatchID: "imgbatch_vertex_key", AccountID: &account.ID, Provider: provider.Name(),
		Model: "gemini-2.5-flash-image", Status: BatchImageJobStatusUploading, RouteFingerprint: provider.RouteFingerprint(account)}
	repo := newFakeBatchImageRepository()
	repo.jobs[job.BatchID] = job
	authority := newBatchImageKeyAuthorityStub()
	registry := NewBatchImageProviderRegistry(provider)
	registry.KeyAuthority = authority
	ctx, err := registry.PrepareRequest(t.Context(), provider, job, account, func(context.Context, int64) (*Account, error) { return account, nil }, repo)
	require.NoError(t, err)
	input := validVertexBatchInput()
	input.BatchID, input.Model = job.BatchID, job.Model
	created, err := provider.Submit(ctx, job, account, input)
	require.NoError(t, err)
	require.NotNil(t, job.UpstreamKeyLease)
	require.Equal(t, []string{config.UpstreamKeyFingerprint(credential)}, job.UpstreamKeyLease.Fingerprints)
	require.Equal(t, []string{"", job.UpstreamKeyLease.Member}, authority.requestMembers,
		"GCS upload uses a request slot; generation uses the persisted batch member")
	require.Zero(t, authority.releases)
	job.Status, job.ProviderJobName = BatchImageJobStatusSubmitted, &created.ProviderJobName
	job.ProviderInputRef = &created.ProviderInputRef
	require.NoError(t, provider.Cancel(ctx, job, account))
	require.Zero(t, authority.releases, "a cancel acknowledgement must retain remote generation capacity")
	status, err := provider.Get(ctx, job, account)
	require.NoError(t, err)
	require.True(t, isBatchImageConfirmedTerminal(status))
	require.Equal(t, job.UpstreamKeyLease.Member, authority.requestMembers[len(authority.requestMembers)-1])
	require.NoError(t, registry.releaseUpstreamKey(t.Context(), repo, job, true))
	require.NotNil(t, job.UpstreamKeyTerminalAt)
	require.NotNil(t, job.UpstreamKeyReleasedAt)
	require.Equal(t, 1, authority.releases)
	require.NoError(t, provider.Cleanup(ctx, job, account, CleanupTargetInput))
	require.Empty(t, authority.requestMembers[len(authority.requestMembers)-1], "post-terminal GCS cleanup uses an ordinary request slot")
	require.EqualValues(t, 5, sends.Load())
}

type batchImageKeyAuthorityStub struct {
	acquires       int
	releases       int
	releaseErr     error
	released       map[string]bool
	requestMembers []string
}

func newBatchImageKeyAuthorityStub() *batchImageKeyAuthorityStub {
	return &batchImageKeyAuthorityStub{released: make(map[string]bool)}
}

func (s *batchImageKeyAuthorityStub) AcquirePersistentUpstreamKeySlots(_ context.Context, req *http.Request) (*PersistentUpstreamKeyLease, error) {
	s.acquires++
	fingerprints := config.UpstreamCredentialFingerprints(req)
	lease := &PersistentUpstreamKeyLease{Authority: "redis", Member: BatchImageKeyCreationMember(req), Fingerprints: fingerprints}
	for _, fingerprint := range fingerprints {
		lease.Resources = append(lease.Resources, PersistentUpstreamKeyResource{Fingerprint: fingerprint})
	}
	return lease, nil
}

func (s *batchImageKeyAuthorityStub) CheckPersistentUpstreamKeySlots(_ context.Context, lease *PersistentUpstreamKeyLease) error {
	if lease.Validate() != nil || s.released[lease.Member] {
		return ErrUpstreamKeyLease
	}
	return nil
}

func (s *batchImageKeyAuthorityStub) ReleasePersistentUpstreamKeySlots(_ context.Context, lease *PersistentUpstreamKeyLease) error {
	if s.releaseErr != nil {
		return s.releaseErr
	}
	if !s.released[lease.Member] {
		s.released[lease.Member] = true
		s.releases++
	}
	return nil
}

func (s *batchImageKeyAuthorityStub) BeginUpstreamKeyRequest(req *http.Request) (*http.Request, func(*http.Response, error), error) {
	member := ""
	if lease := PersistentUpstreamKeyLeaseFromContext(req.Context()); lease != nil {
		if err := s.CheckPersistentUpstreamKeySlots(req.Context(), lease); err != nil {
			return req, nil, err
		}
		member = lease.Member
	}
	s.requestMembers = append(s.requestMembers, member)
	return req, func(*http.Response, error) {}, nil
}
