//go:build unit

package handler

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
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

func TestBatchImageDownloadSlowDownstreamDeadline(t *testing.T) {
	gin.SetMode(gin.TestMode)
	image := make([]byte, 256*1024)
	_, err := rand.Read(image)
	require.NoError(t, err)
	provider := &batchImageRouteDownloadProvider{data: fmt.Sprintf(`{"key":"one","response":{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":%q}}]}}]}}`+"\n", base64.StdEncoding.EncodeToString(image))}
	accountID := int64(3)
	repo := &batchImageRouteDownloadRepo{job: &service.BatchImageJob{BatchID: "imgbatch_route", AccountID: &accountID,
		Provider: service.BatchImageProviderGeminiAPI, Status: service.BatchImageJobStatusCompleted, RouteFingerprint: "fixture-route"}}
	download := &service.BatchImageDownloadService{Repo: repo,
		ProviderRegistry: service.NewBatchImageProviderRegistry(provider),
		AccountResolver:  &batchImageRouteDownloadResolver{account: &service.Account{ID: accountID}},
		Config:           &config.Config{BatchImage: config.BatchImageConfig{MaxDownloadDurationSeconds: 1}}}
	handler := NewBatchImageHandler(&service.BatchImagePublicService{Repo: repo}, download, nil)
	finished := make(chan error, 1)
	router := gin.New()
	router.GET("/v1/images/batches/:id/download", func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 1, UserID: 2})
		handler.Download(c)
		finished <- c.Request.Context().Err()
	})
	socketSetup := make(chan error, 1)
	server := httptest.NewUnstartedServer(router)
	server.Config.ConnState = func(conn net.Conn, state http.ConnState) {
		if state != http.StateNew {
			return
		}
		tcp, ok := conn.(*net.TCPConn)
		if !ok {
			socketSetup <- fmt.Errorf("download fixture connection is %T", conn)
			return
		}
		socketSetup <- tcp.SetWriteBuffer(1024)
	}
	server.Start()
	defer server.Close()
	address, err := net.ResolveTCPAddr("tcp", strings.TrimPrefix(server.URL, "http://"))
	require.NoError(t, err)
	conn, err := net.DialTCP("tcp", nil, address)
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()
	require.NoError(t, <-socketSetup)
	require.NoError(t, conn.SetReadBuffer(1024))
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	_, err = fmt.Fprintf(conn, "GET /v1/images/batches/imgbatch_route/download HTTP/1.1\r\nHost: %s\r\n\r\n", address)
	require.NoError(t, err)
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	// Do not drain the body: the small TCP receive buffer forces a real
	// blocked server write, independently of the upstream reader's context.
	select {
	case err = <-finished:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(3 * time.Second):
		t.Fatal("a stalled downstream socket kept the download handler alive")
	}
	require.Zero(t, repo.downloads)
	require.Nil(t, repo.job.DownloadedAt)
}

func TestBatchImageDownloadDeadlineResetsForKeepAlive(t *testing.T) {
	gin.SetMode(gin.TestMode)
	download := &service.BatchImageDownloadService{Config: &config.Config{BatchImage: config.BatchImageConfig{MaxDownloadDurationSeconds: 1}}}
	handler := NewBatchImageHandler(nil, download, nil)
	router := gin.New()
	router.GET("/download", func(c *gin.Context) {
		finish := handler.bindDownloadDeadline(c)
		defer finish()
		c.String(http.StatusOK, "download")
	})
	router.GET("/health", func(c *gin.Context) { c.String(http.StatusOK, "healthy") })
	server := httptest.NewServer(router)
	defer server.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	reader := bufio.NewReader(conn)
	request := func(path string) string {
		_, writeErr := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: fixture\r\n\r\n", path)
		require.NoError(t, writeErr)
		response, readErr := http.ReadResponse(reader, nil)
		require.NoError(t, readErr)
		body, readErr := io.ReadAll(response.Body)
		require.NoError(t, readErr)
		require.NoError(t, response.Body.Close())
		return string(body)
	}
	require.Equal(t, "download", request("/download"))
	<-time.After(1100 * time.Millisecond)
	require.Equal(t, "healthy", request("/health"), "the same connection must remain usable after the old download deadline")
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
