package service

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type clientAdmissionFixture struct {
	blocked atomic.Bool
	starts  atomic.Int32
	finish  atomic.Int32
}

func (a *clientAdmissionFixture) Do(*http.Request, string, int64, int) (*http.Response, error) {
	return nil, errors.New("fixture HTTPUpstream path is not used")
}

func (a *clientAdmissionFixture) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return a.Do(req, proxyURL, accountID, accountConcurrency)
}

func (a *clientAdmissionFixture) BeginUpstreamKeyRequest(req *http.Request) (*http.Request, func(*http.Response, error), error) {
	if a.blocked.Load() {
		return req, nil, ErrUpstreamKeyCapacity
	}
	a.starts.Add(1)
	return req, func(*http.Response, error) { a.finish.Add(1) }, nil
}

func TestAdmittedOpenAIClientRequestSharesAdmissionAndRedirectBoundary(t *testing.T) {
	var destination atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		destination.Add(1)
		_, _ = io.WriteString(w, "destination")
	}))
	t.Cleanup(target.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(origin.Close)
	fixture := &clientAdmissionFixture{}
	client := origin.Client()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, origin.URL, nil)
	require.NoError(t, err)
	resp, err := doAdmittedOpenAIClientRequest(client, req, true, fixture)
	require.NoError(t, err)
	require.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
	require.EqualValues(t, 1, fixture.starts.Load())
	require.EqualValues(t, 1, fixture.finish.Load())
	require.Zero(t, destination.Load(), "an admitted request cannot follow a redirect to a different credential target")
}

func TestAdmittedOpenAIClientRequestRejectsMissingAuthorityBeforeSend(t *testing.T) {
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { sends.Add(1) }))
	t.Cleanup(server.Close)
	client := server.Client()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	resp, err := doAdmittedOpenAIClientRequest(client, req, true, nil)
	require.Nil(t, resp)
	require.ErrorIs(t, err, ErrUpstreamKeyPolicy)
	require.Zero(t, sends.Load())
}

func TestAdmittedOpenAIClientRequestKeepsLegacyDisabledPolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	t.Cleanup(server.Close)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	resp, err := doAdmittedOpenAIClientRequest(server.Client(), req, false, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
}
