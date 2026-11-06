//go:build unit

package handler

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func TestBatchImageUnconfirmedSubmitHTTPResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	batchImageError(ctx, service.ErrBatchImageSubmitUnconfirmed.WithMetadata(map[string]string{
		"batch_id": "imgbatch_fixture", "private": "fixture-secret",
	}))
	require.Equal(t, http.StatusConflict, recorder.Code)
	require.Contains(t, recorder.Body.String(), `"code":"BATCH_IMAGE_SUBMIT_UNCONFIRMED"`)
	require.Contains(t, recorder.Body.String(), `"batch_id":"imgbatch_fixture"`)
	require.NotContains(t, recorder.Body.String(), "fixture-secret")
}

func TestBatchImageDownloadRouteErrorJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/images/batches/imgbatch_route/download", nil)
	ctx.Params = gin.Params{{Key: "id", Value: "imgbatch_route"}}
	ctx.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 1, UserID: 2})
	accountID := int64(3)
	repo := &batchImageRouteDownloadRepo{job: &service.BatchImageJob{BatchID: "imgbatch_route", AccountID: &accountID,
		Provider: service.BatchImageProviderGeminiAPI, Status: service.BatchImageJobStatusCompleted}}
	download := &service.BatchImageDownloadService{Repo: repo,
		ProviderRegistry: service.NewBatchImageProviderRegistry(&batchImageAdmissionProvider{}),
		AccountResolver:  &batchImageRouteDownloadResolver{}}
	NewBatchImageHandler(nil, download, nil).Download(ctx)
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.Contains(t, recorder.Header().Get("Content-Type"), "application/json")
	require.Empty(t, recorder.Header().Get("Content-Disposition"))
	require.Contains(t, recorder.Body.String(), `"code":"BATCH_IMAGE_ROUTE_UNAVAILABLE"`)
}

func TestBatchImageDownloadDeferredRouteError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	largeImage := make([]byte, 128*1024)
	_, err := rand.Read(largeImage)
	require.NoError(t, err)
	for _, tc := range []struct {
		name    string
		image   []byte
		started bool
	}{
		{name: "before_first_image"},
		{name: "buffered_image", image: []byte("first")},
		{name: "streamed_image", image: largeImage, started: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/images/batches/imgbatch_route/download", nil)
			ctx.Params = gin.Params{{Key: "id", Value: "imgbatch_route"}}
			ctx.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 1, UserID: 2})
			accountID := int64(3)
			repo := &batchImageRouteDownloadRepo{job: &service.BatchImageJob{BatchID: "imgbatch_route", AccountID: &accountID,
				Provider: service.BatchImageProviderGeminiAPI, Status: service.BatchImageJobStatusCompleted, RouteFingerprint: "fixture-route"}}
			provider := &batchImageRouteDownloadProvider{}
			if tc.image != nil {
				provider.data = fmt.Sprintf(`{"key":"one","response":{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":%q}}]}}]}}`+"\n", base64.StdEncoding.EncodeToString(tc.image))
			}
			download := &service.BatchImageDownloadService{Repo: repo,
				ProviderRegistry: service.NewBatchImageProviderRegistry(provider),
				AccountResolver:  &batchImageRouteDownloadResolver{account: &service.Account{ID: accountID}}}
			NewBatchImageHandler(&service.BatchImagePublicService{Repo: repo}, download, nil).Download(ctx)
			require.Zero(t, repo.downloads, "an interrupted download must not be marked successful")
			require.Nil(t, repo.job.DownloadedAt)
			if !tc.started {
				require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
				require.Contains(t, recorder.Header().Get("Content-Type"), "application/json")
				require.Empty(t, recorder.Header().Get("Content-Disposition"))
				var body struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
				require.Equal(t, "BATCH_IMAGE_ROUTE_UNAVAILABLE", body.Error.Code)
				return
			}
			require.Equal(t, http.StatusOK, recorder.Code, "a written streaming response keeps its original status")
			require.Equal(t, "application/zip", recorder.Header().Get("Content-Type"))
			require.Positive(t, recorder.Body.Len())
			require.NotContains(t, recorder.Body.String(), "BATCH_IMAGE_ROUTE_UNAVAILABLE", "JSON must not be appended to a started ZIP")
			_, archiveErr := zip.NewReader(bytes.NewReader(recorder.Body.Bytes()), int64(recorder.Body.Len()))
			require.Error(t, archiveErr, "a refused second object must not finish a valid partial archive")
		})
	}
}

type batchImageRouteDownloadProvider struct {
	batchImageAdmissionProvider
	data string
}

func (p *batchImageRouteDownloadProvider) OpenResult(context.Context, *service.BatchImageJob, *service.Account) (io.ReadCloser, string, error) {
	return io.NopCloser(io.MultiReader(strings.NewReader(p.data), batchImageRouteErrorReader{})), "application/jsonl", nil
}

type batchImageRouteErrorReader struct{}

func (batchImageRouteErrorReader) Read([]byte) (int, error) {
	return 0, service.ErrBatchImageRouteUnavailable
}

type batchImageRouteDownloadRepo struct {
	service.BatchImageRepository
	job       *service.BatchImageJob
	downloads int
}

func (r *batchImageRouteDownloadRepo) GetBatchImageJobForDownload(context.Context, int64, int64, string) (*service.BatchImageJob, error) {
	return r.job, nil
}

func (r *batchImageRouteDownloadRepo) GetBatchImageJobByBatchIDForOwner(context.Context, int64, int64, string) (*service.BatchImageJob, error) {
	return r.job, nil
}

func (r *batchImageRouteDownloadRepo) MarkBatchImageDownloaded(_ context.Context, _ string, downloadedAt time.Time) error {
	r.downloads++
	r.job.DownloadedAt = &downloadedAt
	return nil
}

func (*batchImageRouteDownloadRepo) ListBatchImageItemsForDownload(_ context.Context, _ string, status string, _ int) ([]*service.BatchImageItem, error) {
	if status == service.BatchImageItemStatusSuccess {
		return []*service.BatchImageItem{{CustomID: "one", Status: status, ImageCount: 1}}, nil
	}
	return nil, nil
}

type batchImageRouteDownloadResolver struct {
	account *service.Account
}

func (r *batchImageRouteDownloadResolver) ResolveBatchImageAccount(context.Context, int64) (*service.Account, error) {
	if r.account != nil {
		return r.account, nil
	}
	return nil, service.ErrBatchImageRouteUnavailable
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

func (*batchImageAdmissionProvider) SupportsAccount(*service.Account) bool    { return true }
func (*batchImageAdmissionProvider) RouteFingerprint(*service.Account) string { return "fixture-route" }

func (p *batchImageAdmissionProvider) Submit(context.Context, *service.BatchImageJob, *service.Account, service.BatchImageInput) (*service.BatchProviderJob, error) {
	p.submitted = true
	return nil, nil
}
