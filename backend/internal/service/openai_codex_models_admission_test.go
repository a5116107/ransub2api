package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestCodexModelsOAuthDirectClientSharesConfiguredKeyAdmission(t *testing.T) {
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sends.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"models":[]}`)
	}))
	t.Cleanup(server.Close)
	admission := &clientAdmissionFixture{}
	admission.blocked.Store(true)
	cfg := &config.Config{}
	cfg.Gateway.UpstreamKeyConcurrency.Revision = 1
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: admission}
	request := openAIModelsRequest{
		url:                server.URL,
		headers:            http.Header{"Authorization": {"Bearer manifest-fixture"}},
		credentialAccount:  &Account{ID: 901, Platform: PlatformOpenAI, Type: AccountTypeOAuth},
		accountID:          901,
		accountConcurrency: 1,
	}
	manifest, err := svc.fetchOpenAIModelsUpstream(context.Background(), request, "")
	require.Nil(t, manifest)
	require.True(t, IsUpstreamKeyAdmissionFailure(err))
	require.Zero(t, sends.Load(), "a full shared Key must block direct OAuth manifest traffic")
	admission.blocked.Store(false)
	manifest, err = svc.fetchOpenAIModelsUpstream(context.Background(), request, "")
	require.NoError(t, err)
	require.NotNil(t, manifest)
	require.EqualValues(t, 1, sends.Load())
	require.EqualValues(t, 1, admission.starts.Load())
	require.EqualValues(t, 1, admission.finish.Load())
}
