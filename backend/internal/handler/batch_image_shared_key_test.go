//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestBatchImageSharedKeyAdmissionHTTPResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/images/batches", strings.NewReader(`{"model":"gemini-2.5-flash-image","provider":"gemini_api","items":[{"prompt":"fixture"}]}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 1, UserID: 2})
	repo := &batchImageAdmissionRepo{}
	provider := &batchImageAdmissionProvider{}
	svc := &service.BatchImagePublicService{
		Repo:             repo,
		AccountRepo:      &batchImageAdmissionAccountRepo{},
		ProviderRegistry: service.NewBatchImageProviderRegistry(provider),
		Config: &config.Config{
			BatchImage: config.BatchImageConfig{Enabled: true},
			Gateway: config.GatewayConfig{UpstreamKeyConcurrency: config.UpstreamKeyConcurrencyConfig{
				Revision: 1,
				Limits:   map[string]int{config.UpstreamKeyFingerprint("test-secret"): 1},
			}},
		},
	}
	NewBatchImageHandler(svc, nil, nil).Submit(ctx)

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Equal(t, "BATCH_IMAGE_SHARED_KEY_UNAVAILABLE", body.Error.Code)
	require.NotContains(t, recorder.Body.String(), "test-secret")
	require.False(t, repo.created)
	require.False(t, provider.submitted)
}

type batchImageAdmissionRepo struct {
	service.BatchImageRepository
	created bool
}

func (r *batchImageAdmissionRepo) CreateBatchImageJob(context.Context, service.CreateBatchImageJobParams) (*service.BatchImageJob, error) {
	r.created = true
	return nil, nil
}

type batchImageAdmissionAccountRepo struct {
	service.BatchImageAccountSelectionRepository
}

func (*batchImageAdmissionAccountRepo) ListSchedulableByPlatform(context.Context, string) ([]service.Account, error) {
	return []service.Account{{
		ID: 1, Platform: service.PlatformGemini, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, Credentials: map[string]any{"api_key": "test-secret"},
	}}, nil
}

type batchImageAdmissionProvider struct {
	service.BatchImageProvider
	submitted bool
}

func (*batchImageAdmissionProvider) Name() string { return service.BatchImageProviderGeminiAPI }

func (*batchImageAdmissionProvider) SupportsAccount(*service.Account) bool { return true }

func (p *batchImageAdmissionProvider) Submit(context.Context, *service.BatchImageJob, *service.Account, service.BatchImageInput) (*service.BatchProviderJob, error) {
	p.submitted = true
	return nil, nil
}
