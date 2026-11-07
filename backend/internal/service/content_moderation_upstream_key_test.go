//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

type moderationKeyAdmissionTestStub struct {
	err        error
	loseOnRead bool
	attempts   atomic.Int32
}

func (s *moderationKeyAdmissionTestStub) BeginUpstreamKeyRequest(req *http.Request) (*http.Request, func(*http.Response, error), error) {
	s.attempts.Add(1)
	if s.err != nil {
		return req, nil, s.err
	}
	ctx, cancel := context.WithCancelCause(req.Context())
	return req.WithContext(ctx), func(resp *http.Response, _ error) {
		if s.loseOnRead && resp != nil && resp.Body != nil {
			resp.Body = &moderationLeaseLossBody{ReadCloser: resp.Body, cancel: cancel}
		}
	}, nil
}

type moderationLeaseLossBody struct {
	io.ReadCloser
	cancel context.CancelCauseFunc
}

func (b *moderationLeaseLossBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.cancel(ErrUpstreamKeyLease)
	return n, err
}

func (b *moderationLeaseLossBody) Close() error {
	b.cancel(ErrUpstreamKeyLease)
	return b.ReadCloser.Close()
}

func TestContentModerationLocalKeyFailureDoesNotChangeSupplierHealth(t *testing.T) {
	for _, engine := range []string{"openai", "typesafe"} {
		for _, tc := range []struct {
			name   string
			err    error
			lost   bool
			status int
		}{
			{"capacity", ErrUpstreamKeyCapacity, false, http.StatusOK},
			{"policy", ErrUpstreamKeyPolicy, false, http.StatusOK},
			{"lease", ErrUpstreamKeyLease, true, http.StatusOK},
			{"lease-error-response", ErrUpstreamKeyLease, true, http.StatusServiceUnavailable},
		} {
			t.Run(engine+"-"+tc.name, func(t *testing.T) {
				var sends atomic.Int32
				answers := map[string]any{}
				for _, category := range ContentModerationCategories("typesafe") {
					answers[category] = map[string]any{"type": "noul", "noul": 0.1}
				}
				body := `{"model":"fixture","results":[{"flagged":false,"categories":{},"category_scores":{}}]}`
				if engine == "typesafe" {
					raw, err := json.Marshal(map[string]any{"model": "fixture", "answers": answers})
					require.NoError(t, err)
					body = string(raw)
				}
				client := &http.Client{Transport: typeSafeURLRoundTripper(func(*http.Request) (*http.Response, error) {
					sends.Add(1)
					return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
				})}
				stub := &moderationKeyAdmissionTestStub{loseOnRead: tc.lost}
				if !tc.lost {
					stub.err = tc.err
				}
				svc := &ContentModerationService{httpClient: client, upstreamKeyAdmission: stub, requireUpstreamKeyAdmission: true}
				cfg := defaultContentModerationConfig()
				cfg.Engine, cfg.BaseURL, cfg.APIKeys, cfg.RetryCount = engine, "https://fixture.invalid", []string{"fixture-key"}, 0
				if tc.lost {
					cfg.RetryCount = 2
				}
				result, err := svc.callModeration(t.Context(), cfg, "fixture", true)
				require.Nil(t, result)
				require.ErrorIs(t, err, tc.err)
				require.EqualValues(t, 1, stub.attempts.Load(), "lease loss must not replay a sent audit")
				if tc.lost {
					require.EqualValues(t, 1, sends.Load())
				} else {
					require.Zero(t, sends.Load())
				}
				status := svc.apiKeyStatuses(cfg.APIKeys, engine)[0]
				require.Equal(t, "unknown", status.Status)
				require.False(t, status.LastTested)
				require.Zero(t, status.FailureCount)
				require.Zero(t, status.SuccessCount)
				require.Empty(t, status.LastError)
				load := svc.preBlockAPIKeyLoads(cfg.APIKeys, engine)[0]
				require.Zero(t, load.Active)
				require.Zero(t, load.Total)
				require.Zero(t, load.Errors)
			})
		}
	}
}

func TestContentModerationLocalKeyFailurePreservesHealthyKeyAndFailOpen(t *testing.T) {
	for _, engine := range []string{"openai", "typesafe"} {
		t.Run(engine, func(t *testing.T) {
			repo := &contentModerationTestRepo{}
			svc := &ContentModerationService{repo: repo, httpClient: http.DefaultClient,
				upstreamKeyAdmission: &moderationKeyAdmissionTestStub{err: ErrUpstreamKeyCapacity}, requireUpstreamKeyAdmission: true}
			cfg := defaultContentModerationConfig()
			cfg.Engine, cfg.BaseURL, cfg.APIKeys, cfg.RetryCount = engine, "https://fixture.invalid", []string{"fixture-key"}, 0
			cfg.Mode, cfg.RecordNonHits = ContentModerationModePreBlock, true
			svc.markAPIKeySuccess("fixture-key", 23, http.StatusOK, engine)
			before := svc.apiKeyStatuses(cfg.APIKeys, engine)[0]
			decision := svc.checkSync(t.Context(), ContentModerationCheckInput{UserID: 1}, cfg, ContentModerationInput{Text: "fixture"}, "hash", nil, true)
			require.True(t, decision.Allowed, "local admission keeps the existing audit failure-open behavior")
			require.Equal(t, before, svc.apiKeyStatuses(cfg.APIKeys, engine)[0], "local capacity is not an upstream observation")
			load := svc.preBlockAPIKeyLoads(cfg.APIKeys, engine)[0]
			require.Zero(t, load.Active)
			require.Zero(t, load.Total)
			require.Zero(t, load.Errors)
			require.Len(t, repo.logs, 1)
			require.Equal(t, ContentModerationActionError, repo.logs[0].Action)
		})
	}
}

type moderationSelectiveAdmission struct{}

func (moderationSelectiveAdmission) BeginUpstreamKeyRequest(req *http.Request) (*http.Request, func(*http.Response, error), error) {
	if req.Header.Get("Authorization") == "Bearer full-key" {
		return req, nil, ErrUpstreamKeyCapacity
	}
	return req, func(*http.Response, error) {}, nil
}

func TestContentModerationLocalKeyCapacityRotatesWithoutHealthFailure(t *testing.T) {
	for _, engine := range []string{"openai", "typesafe"} {
		t.Run(engine, func(t *testing.T) {
			answers := map[string]any{}
			for _, category := range ContentModerationCategories("typesafe") {
				answers[category] = map[string]any{"type": "noul", "noul": 0.1}
			}
			body := `{"model":"fixture","results":[{"flagged":false,"categories":{},"category_scores":{}}]}`
			if engine == "typesafe" {
				raw, err := json.Marshal(map[string]any{"model": "fixture", "answers": answers})
				require.NoError(t, err)
				body = string(raw)
			}
			var sends atomic.Int32
			client := &http.Client{Transport: typeSafeURLRoundTripper(func(req *http.Request) (*http.Response, error) {
				sends.Add(1)
				require.Equal(t, "Bearer available-key", req.Header.Get("Authorization"))
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			svc := &ContentModerationService{httpClient: client, upstreamKeyAdmission: moderationSelectiveAdmission{}, requireUpstreamKeyAdmission: true}
			cfg := defaultContentModerationConfig()
			cfg.Engine, cfg.BaseURL, cfg.APIKeys, cfg.RetryCount = engine, "https://fixture.invalid", []string{"full-key", "available-key"}, 1
			result, err := svc.callModeration(t.Context(), cfg, "fixture", true)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.EqualValues(t, 1, sends.Load())
			statuses := svc.apiKeyStatuses(cfg.APIKeys, engine)
			require.Equal(t, "unknown", statuses[0].Status)
			require.False(t, statuses[0].LastTested)
			require.EqualValues(t, 1, statuses[1].SuccessCount)
			loads := svc.preBlockAPIKeyLoads(cfg.APIKeys, engine)
			require.Zero(t, loads[0].Total)
			require.EqualValues(t, 1, loads[1].Total)
			require.EqualValues(t, 1, loads[1].Success)
		})
	}
}

func TestContentModerationSharedKeyMissingAuthorityAndRedirect(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Add(1)
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(destination.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, destination.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(origin.Close)
	svc := &ContentModerationService{httpClient: origin.Client(), requireUpstreamKeyAdmission: true}
	_, err := svc.moderationHTTPClient(t.Context(), nil)
	require.ErrorIs(t, err, ErrUpstreamKeyPolicy)
	svc.upstreamKeyAdmission = &moderationKeyAdmissionTestStub{}
	client, err := svc.moderationHTTPClient(t.Context(), nil)
	require.NoError(t, err)
	response, err := client.Get(origin.URL)
	require.NoError(t, err)
	require.Equal(t, http.StatusTemporaryRedirect, response.StatusCode)
	require.NoError(t, response.Body.Close())
	require.Zero(t, redirected.Load())
	svc.requireUpstreamKeyAdmission = false
	client, err = svc.moderationHTTPClient(t.Context(), nil)
	require.NoError(t, err)
	response, err = client.Get(origin.URL)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NoError(t, response.Body.Close())
	require.EqualValues(t, 1, redirected.Load())
}
