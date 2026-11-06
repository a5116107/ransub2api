//go:build unit

package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type bpsFileOriginBlockingStore struct{ reads atomic.Int32 }

func (s *bpsFileOriginBlockingStore) LoadBPSState(ctx context.Context, _, _ string) ([]byte, uint64, error) {
	s.reads.Add(1)
	<-ctx.Done()
	return nil, 0, ctx.Err()
}

func (*bpsFileOriginBlockingStore) SaveBPSState(context.Context, string, string, *uint64, []byte) (bool, error) {
	return false, nil
}

type bpsFileOriginCountingStore struct{ reads int }

func (s *bpsFileOriginCountingStore) LoadBPSState(context.Context, string, string) ([]byte, uint64, error) {
	s.reads++
	return nil, 0, nil
}

func (*bpsFileOriginCountingStore) SaveBPSState(context.Context, string, string, *uint64, []byte) (bool, error) {
	return false, nil
}

func TestExcelBPSFileOriginReadRespectsRequestDeadline(t *testing.T) {
	store := &bpsFileOriginBlockingStore{}
	guard := &excelBPSOriginGuard{store: store, origin: excelBPSRouteOrigin(excelAccount(), "workspace-one", "token")}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := guard.check(ctx, []string{"file-timeout-fixture"})
	require.True(t, IsOpenAITurnAdmissionError(err), "%v", err)
	require.Less(t, time.Since(start), time.Second)
	require.EqualValues(t, 1, store.reads.Load())
}

func TestExcelBPSFileOriginDeduplicatesReferences(t *testing.T) {
	store := &bpsFileOriginCountingStore{}
	guard := &excelBPSOriginGuard{store: store, origin: excelBPSRouteOrigin(excelAccount(), "workspace-one", "token")}
	found, err := guard.check(t.Context(), []string{"file-one", "file-one", "file-two", "file-one"})
	require.NoError(t, err)
	require.False(t, found)
	require.Equal(t, 2, store.reads)
}

func TestExcelBPSFileOriginBlocksKnownForeignRouteAndRestores(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	account := excelAccount()
	account.Proxy = &Proxy{Protocol: "http", Host: "original-proxy.invalid", Port: 8080}
	foreign := *account
	foreign.Proxy = &Proxy{Protocol: "http", Host: "different-proxy.invalid", Port: 8080}
	fixtureID := "file-origin-regression-fixture"
	calls, uploads := 0, 0
	svc := openAIClientToolsTestService(nil)
	enableNativeAttachments(svc)
	t.Cleanup(func() { require.NoError(t, svc.CloseExcelBPSImages()) })
	svc.httpUpstream = &bpsTestUpstream{send: func(req *http.Request, _ string) (*http.Response, error) {
		calls++
		if req.URL.String() == basispoints.AttachmentsURL {
			uploads++
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"openai_file_id":%q}`, fixtureID)))}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixture\",\"status\":\"completed\",\"output\":[]}}\n\n"))}, nil
	}}
	forward := func(a *Account, body []byte) (int, error) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
		c.Request.Header.Set("Session-Id", "file-origin-fixture")
		_, err := svc.Forward(context.Background(), c, a, body)
		return rec.Code, err
	}
	imageBody, _ := nativeGatewayBody(t)
	_, err := forward(account, imageBody)
	require.NoError(t, err)
	require.Equal(t, 1, uploads)
	known := []byte(fmt.Sprintf(`{"model":"gpt-6-astra","input":[{"role":"user","content":[{"type":"input_image","file_id":%q}]}]}`, fixtureID))
	unknown := []byte(`{"model":"gpt-6-astra","input":[{"role":"user","content":[{"type":"input_image","file_id":"file-unknown-fixture"}]}]}`)
	before := calls
	_, err = forward(account, known)
	require.NoError(t, err, "the original upstream route remains eligible")
	require.Equal(t, before+1, calls)
	before = calls
	_, err = forward(&foreign, unknown)
	require.NoError(t, err, "unknown old references preserve compatibility")
	require.Equal(t, before+1, calls)
	before = calls
	status, err := forward(&foreign, known)
	require.Error(t, err, "a known file from another route must not be sent")
	require.Equal(t, http.StatusServiceUnavailable, status)
	require.Equal(t, before, calls)
	_, err = forward(account, known)
	require.NoError(t, err, "the original route must recover")
	require.Equal(t, before+1, calls)
}

func TestExcelBPSFileOriginPrincipalEvidence(t *testing.T) {
	account := excelAccount()
	token := excelBPSCacheScopeToken(t, "subject-one", "workspace-one", "old")
	refreshed := excelBPSCacheScopeToken(t, "subject-one", "workspace-one", "new")
	different := excelBPSCacheScopeToken(t, "subject-two", "workspace-one", "new")
	known := excelBPSRouteOrigin(account, "workspace-one", token)
	for _, tc := range []struct {
		name, accountID, token string
		foreign                bool
	}{
		{"same principal after refresh", "workspace-one", refreshed, false},
		{"unknown principal", "workspace-one", "opaque-token", false},
		{"different principal", "workspace-one", different, true},
		{"different workspace", "workspace-two", "opaque-token", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := &basispoints.CatalogCache{}
			require.NoError(t, cache.RememberFileOrigin("file-principal-fixture", known))
			found, err := cache.CheckFileOrigin("file-principal-fixture", excelBPSRouteOrigin(account, tc.accountID, tc.token))
			require.True(t, found)
			if tc.foreign {
				require.ErrorIs(t, err, basispoints.ErrRouteOriginMismatch)
			} else {
				require.NoError(t, err)
			}
		})
	}
	cache := &basispoints.CatalogCache{}
	require.NoError(t, cache.RememberFileOrigin("file-upgrade-fixture", excelBPSRouteOrigin(account, "workspace-one", "opaque-token")))
	_, err := cache.CheckFileOrigin("file-upgrade-fixture", known)
	require.NoError(t, err, "a formerly opaque identity is not proof of a foreign principal")
	require.NoError(t, cache.RememberFileOrigin("file-upgrade-fixture", known), "a confirmed re-upload can add missing principal evidence")
	_, err = cache.CheckFileOrigin("file-upgrade-fixture", excelBPSRouteOrigin(account, "workspace-one", different))
	require.ErrorIs(t, err, basispoints.ErrRouteOriginMismatch)
}

func TestExcelBPSFileOriginRechecksAfterProxyWait(t *testing.T) {
	account := excelAccount()
	account.Extra["openai_excel_bps_mihomo"] = true
	latest := *account
	latest.Proxy = &Proxy{Protocol: "http", Host: "changed.invalid", Port: 8080}
	repo := &turnAdmissionRepo{account: &latest}
	cache := &basispoints.CatalogCache{}
	origin := excelBPSRouteOrigin(account, "workspace-one", "opaque-token")
	require.NoError(t, cache.RememberFileOrigin("file-inflight-fixture", origin))
	guard := &excelBPSOriginGuard{cache: cache, origin: origin}
	ctx := context.WithValue(t.Context(), excelBPSOriginKey{}, guard)
	calls := 0
	svc := &OpenAIGatewayService{accountRepo: repo, requireLatestTurnAdmission: true, httpUpstream: &bpsTestUpstream{send: func(*http.Request, string) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	}}}
	body := []byte(`{"model":"gpt-6-astra","input":[{"role":"user","content":[{"type":"input_image","file_id":"file-inflight-fixture"}]}]}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	lease := &bpsTestLease{}
	acquire := func(context.Context, string, ...string) (string, excelBPSLease, error) {
		return "http://127.0.0.1:19000", lease, nil
	}
	_, _, _, err := svc.doExcelBPSRequest(ctx, c, account, "fixture", body, "opaque-token", "workspace-one", acquire)
	require.True(t, IsOpenAITurnAdmissionError(err), "%v", err)
	require.Zero(t, calls)
	require.Equal(t, 1, repo.reads)
	require.Equal(t, 1, lease.releases)
	require.Zero(t, lease.failures)
	repo.account = account
	response, returned, _, err := svc.doExcelBPSRequest(ctx, c, account, "fixture", body, "opaque-token", "workspace-one", acquire)
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.NoError(t, response.Body.Close())
	returned.Release()
}

func TestExcelBPSFileOriginBlocksChangeDuringAttachmentUpload(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	account := excelAccount()
	latest := *account
	latest.Proxy = &Proxy{Protocol: "http", Host: "changed-during-upload.invalid", Port: 8080}
	repo := &turnAdmissionRepo{account: account}
	svc := openAIClientToolsTestService(nil)
	svc.accountRepo, svc.requireLatestTurnAdmission = repo, true
	svc.cfg.Gateway.Scheduling.OpenAITurnAdmissionCacheTTLSeconds = 300
	enableNativeAttachments(svc)
	t.Cleanup(func() { require.NoError(t, svc.CloseExcelBPSImages()) })
	uploads, sends := 0, 0
	svc.httpUpstream = &bpsTestUpstream{send: func(req *http.Request, _ string) (*http.Response, error) {
		if req.URL.String() == basispoints.AttachmentsURL {
			uploads++
			repo.account = &latest
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"openai_file_id":"file-upload-inflight-fixture"}`))}, nil
		}
		sends++
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixture\",\"status\":\"completed\",\"output\":[]}}\n\n"))}, nil
	}}
	body, _ := nativeGatewayBody(t)
	forward := func() (int, error) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
		c.Request.Header.Set("Session-Id", "upload-inflight-fixture")
		_, err := svc.Forward(t.Context(), c, account, body)
		return rec.Code, err
	}
	status, err := forward()
	require.Error(t, err)
	require.Equal(t, http.StatusServiceUnavailable, status)
	require.Equal(t, 1, uploads)
	require.Zero(t, sends, "the uploaded reference cannot be sent after its selected route changes")
	repo.account = account
	status, err = forward()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, 1, uploads, "the original upload remains reusable on its original route")
	require.Equal(t, 1, sends)
}

func TestExcelBPSFileOriginIgnoresInternalPreflightPlaceholder(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	account := excelAccount()
	other := *account
	other.ID++
	require.NoError(t, excelBPSCatalog.RememberFileOrigin("file-preflight", excelBPSRouteOrigin(&other, "other-workspace", "opaque")))
	svc := openAIClientToolsTestService(nil)
	enableNativeAttachments(svc)
	t.Cleanup(func() { require.NoError(t, svc.CloseExcelBPSImages()) })
	uploads, sends := 0, 0
	svc.httpUpstream = &bpsTestUpstream{send: func(req *http.Request, _ string) (*http.Response, error) {
		if req.URL.String() == basispoints.AttachmentsURL {
			uploads++
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"openai_file_id":"file-real-placeholder-test"}`))}, nil
		}
		sends++
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixture\",\"status\":\"completed\",\"output\":[]}}\n\n"))}, nil
	}}
	body, _ := nativeGatewayBody(t)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	_, err := svc.Forward(t.Context(), c, account, body)
	require.NoError(t, err)
	require.Equal(t, 1, uploads)
	require.Equal(t, 1, sends)
}

func TestExcelBPSFileOriginRejectsChangedBodyOrTarget(t *testing.T) {
	account := excelAccount()
	cache := &basispoints.CatalogCache{}
	origin := excelBPSRouteOrigin(account, "workspace-one", "opaque-token")
	require.NoError(t, cache.RememberFileOrigin("file-protected-fixture", origin))
	ctx := context.WithValue(t.Context(), excelBPSOriginKey{}, &excelBPSOriginGuard{cache: cache, origin: origin})
	svc := &OpenAIGatewayService{}
	good := `{"model":"gpt-6-astra","input":"hi"}`
	request, err := newExcelBPSRequest(ctx, []byte(good), "token", "workspace-one")
	require.NoError(t, err)
	request.Body = io.NopCloser(strings.NewReader(`{"model":"gpt-6-astra","input":[{"role":"user","content":[{"type":"input_image","file_id":"file-protected-fixture"}]}]}`))
	require.True(t, IsOpenAITurnAdmissionError(svc.guardExcelBPSOriginRequest(request, account)), "the actual body must match its replayable copy")
	request, err = newExcelBPSRequest(ctx, []byte(good), "token", "workspace-one")
	require.NoError(t, err)
	request.URL.Host = "foreign.invalid"
	require.True(t, IsOpenAITurnAdmissionError(svc.guardExcelBPSOriginRequest(request, account)), "a BPS-bound request cannot silently switch targets")
	request, err = newExcelBPSRequest(ctx, []byte(good), "token", "workspace-one")
	require.NoError(t, err)
	request.Host = "foreign.invalid"
	require.True(t, IsOpenAITurnAdmissionError(svc.guardExcelBPSOriginRequest(request, account)), "a BPS-bound request cannot override its target host")
}
