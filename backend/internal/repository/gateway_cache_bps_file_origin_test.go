package repository

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type bpsFileOriginSettings struct{ service.SettingRepository }

func (bpsFileOriginSettings) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	values := make(map[string]string, len(keys))
	for _, key := range keys {
		switch key {
		case service.SettingKeyExcelBPSImageMode:
			values[key] = service.ExcelBPSImageModeNative
		case service.SettingKeyExcelBPSEnabled, service.SettingKeyExcelBPSImageRelayEnabled:
			values[key] = "true"
		}
	}
	return values, nil
}

func (bpsFileOriginSettings) GetValue(context.Context, string) (string, error) { return "", nil }
func (bpsFileOriginSettings) GetAll(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}

func TestBPSFileOriginSharedStore(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr(), MaxRetries: -1})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	testBPSFileOriginSharedStore(t, client)
	testBPSFileOriginAcrossGateways(t, client, server.SetError)

	_, cache := basispoints.NewPersistentCaches(t.Context(), &gatewayCache{rdb: client})
	server.SetError("fixture outage")
	_, err := cache.CheckFileOrigin("file-shared-fixture", basispoints.RouteOrigin{Route: fmt.Sprintf("%064x", 1)})
	require.ErrorIs(t, err, basispoints.ErrStateUnavailable)
	server.SetError("")
	_, recovered := basispoints.NewPersistentCaches(t.Context(), &gatewayCache{rdb: client})
	found, err := recovered.CheckFileOrigin("file-shared-fixture", basispoints.RouteOrigin{Route: fmt.Sprintf("%064x", 1)})
	require.True(t, found)
	require.NoError(t, err)
	server.FastForward(bpsStateTTL + time.Second)
	found, err = recovered.CheckFileOrigin("file-shared-fixture", basispoints.RouteOrigin{Route: fmt.Sprintf("%064x", 2)})
	require.False(t, found, "expired origin is unknown, not evidence of another route")
	require.NoError(t, err)
	found, err = recovered.CheckEncryptedOrigin("cipher-shared-fixture", basispoints.RouteOrigin{Route: fmt.Sprintf("%064x", 2)})
	require.False(t, found, "expired ciphertext origin must remain compatible")
	require.NoError(t, err)
}

func TestBPSFileOriginSharedStoreRealRedis(t *testing.T) {
	binary, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skip("redis-server is required for this isolated adapter check")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	_, port, err := net.SplitHostPort(address)
	require.NoError(t, err)
	require.NoError(t, listener.Close())
	cmd := exec.Command(binary, "--bind", "127.0.0.1", "--port", port, "--dir", t.TempDir(), "--save", "", "--appendonly", "no")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	client := redis.NewClient(&redis.Options{Addr: address, MaxRetries: -1, DialTimeout: 200 * time.Millisecond})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	require.Eventually(t, func() bool { return client.Ping(t.Context()).Err() == nil }, 3*time.Second, 10*time.Millisecond)
	testBPSFileOriginSharedStore(t, client)
	testBPSFileOriginAcrossGateways(t, client, nil)
}

func testBPSFileOriginSharedStore(t *testing.T, client *redis.Client) {
	t.Helper()
	newCache := func() *basispoints.CatalogCache {
		_, cache := basispoints.NewPersistentCaches(t.Context(), &gatewayCache{rdb: client})
		return cache
	}
	origin := basispoints.RouteOrigin{Route: fmt.Sprintf("%064x", 1), Account: fmt.Sprintf("%064x", 11)}
	foreign := basispoints.RouteOrigin{Route: fmt.Sprintf("%064x", 2), Account: origin.Account}
	require.NoError(t, newCache().RememberFileOrigin("file-shared-fixture", origin))
	found, err := newCache().CheckFileOrigin("file-shared-fixture", foreign)
	require.True(t, found)
	require.ErrorIs(t, err, basispoints.ErrRouteOriginMismatch)
	found, err = newCache().CheckFileOrigin("file-unknown-fixture", foreign)
	require.False(t, found)
	require.NoError(t, err)
	require.NoError(t, newCache().RememberEncryptedOrigin("cipher-shared-fixture", origin))
	found, err = newCache().CheckEncryptedOrigin("cipher-shared-fixture", foreign)
	require.True(t, found)
	require.ErrorIs(t, err, basispoints.ErrRouteOriginMismatch)
	found, err = newCache().CheckEncryptedOrigin("cipher-unknown-fixture", foreign)
	require.False(t, found)
	require.NoError(t, err)

	const competitors = 16
	results := make(chan error, competitors)
	var wg sync.WaitGroup
	for i := range competitors {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			candidate := basispoints.RouteOrigin{Route: fmt.Sprintf("%064x", n+100)}
			results <- newCache().RememberFileOrigin("file-concurrent-fixture", candidate)
		}(i)
	}
	wg.Wait()
	close(results)
	winners := 0
	for result := range results {
		if result == nil {
			winners++
		} else {
			require.ErrorIs(t, result, basispoints.ErrRouteOriginMismatch)
		}
	}
	require.Equal(t, 1, winners, "only one distinct origin can claim a live file")
	cipherResults := make(chan error, competitors)
	for i := range competitors {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			candidate := basispoints.RouteOrigin{Route: fmt.Sprintf("%064x", n+200)}
			cipherResults <- newCache().RememberEncryptedOrigin("cipher-concurrent-fixture", candidate)
		}(i)
	}
	wg.Wait()
	close(cipherResults)
	winners = 0
	for result := range cipherResults {
		if result == nil {
			winners++
		} else {
			require.ErrorIs(t, result, basispoints.ErrRouteOriginMismatch)
		}
	}
	require.Equal(t, 1, winners, "only one distinct origin can claim live ciphertext")
	keys, err := client.Keys(t.Context(), bpsStatePrefix+"*").Result()
	require.NoError(t, err)
	require.NotEmpty(t, keys)
	for _, key := range keys {
		require.NotContains(t, key, "file-shared-fixture")
		require.NotContains(t, key, "file-concurrent-fixture")
		require.NotContains(t, key, "cipher-shared-fixture")
		require.NotContains(t, key, "cipher-concurrent-fixture")
		if key != bpsStatePrefix+"lru" && key != bpsStatePrefix+"sizes" {
			raw, err := client.HGet(t.Context(), key, "raw").Result()
			require.NoError(t, err)
			require.NotContains(t, raw, "cipher-shared-fixture")
			require.NotContains(t, raw, "cipher-concurrent-fixture")
		}
	}
}

func testBPSFileOriginAcrossGateways(t *testing.T, client *redis.Client, setError func(string)) {
	t.Helper()
	t.Setenv("DATA_DIR", t.TempDir())
	var uploads, sends atomic.Int32
	var encryptedOutput, lastRequest atomic.Value
	encryptedOutput.Store("")
	lastRequest.Store("")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/basispoints/api/attachments" {
			uploads.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"openai_file_id":"file-gateway-fixture"}`)
			return
		}
		sends.Add(1)
		body, _ := io.ReadAll(req.Body)
		lastRequest.Store(string(body))
		w.Header().Set("Content-Type", "text/event-stream")
		item, _ := encryptedOutput.Load().(string)
		if item != "" {
			_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"item\":%s}\n\n", item)
			_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_shared_cipher\",\"status\":\"completed\",\"output\":[%s]}}\n\n", item)
			return
		}
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_shared_file\",\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	t.Cleanup(upstream.Close)
	account := &service.Account{ID: 42, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{"access_token": "opaque-fixture", "chatgpt_account_id": "workspace-fixture"}, Extra: map[string]any{"openai_excel_bps": true, "openai_passthrough": true}}
	repo := &lifecycleAccountRepo{account: account}
	cfg := &config.Config{}
	cfg.Gateway.ExcelBPSStateRedisEnabled = true
	settings := service.NewSettingService(bpsFileOriginSettings{}, cfg)
	newGateway := func() *service.OpenAIGatewayService {
		svc := service.NewOpenAIGatewayService(repo, nil, nil, nil, nil, nil, nil, NewGatewayCache(client), cfg, nil, nil, nil, nil, nil, &bpsScopeFixtureUpstream{server: upstream}, nil, nil, nil, nil, nil, nil, settings, nil)
		t.Cleanup(svc.StopOpenAICodexTicketHarvester)
		t.Cleanup(svc.CloseOpenAIWSPool)
		t.Cleanup(func() { require.NoError(t, svc.CloseExcelBPSImages()) })
		return svc
	}
	first, second := newGateway(), newGateway()
	forward := func(svc *service.OpenAIGatewayService, selected *service.Account, body []byte) (*httptest.ResponseRecorder, error) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
		_, err := svc.Forward(context.Background(), c, selected, body)
		return rec, err
	}
	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	imageBody, err := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(encoded.Bytes())}}}}})
	require.NoError(t, err)
	_, err = forward(first, account, imageBody)
	require.NoError(t, err)
	require.EqualValues(t, 1, uploads.Load(), "the first gateway must really upload the image")
	require.EqualValues(t, 1, sends.Load())
	fileBody := func(id string) []byte {
		return []byte(fmt.Sprintf(`{"model":"gpt-6-astra","input":[{"role":"user","content":[{"type":"input_image","file_id":%q}]}]}`, id))
	}
	if setError != nil {
		setError("fixture state unavailable")
		rec, err := forward(second, account, fileBody("file-gateway-fixture"))
		require.Error(t, err)
		require.Equal(t, http.StatusServiceUnavailable, rec.Code)
		require.EqualValues(t, 1, sends.Load(), "store errors must not be treated as unknown origins")
		setError("")
	}
	changed := *account
	changed.Proxy = &service.Proxy{Protocol: "http", Host: "different.invalid", Port: 8080}
	repo.account = &changed
	rec, err := forward(second, &changed, fileBody("file-gateway-fixture"))
	require.Error(t, err)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.EqualValues(t, 1, sends.Load())
	_, err = forward(second, &changed, fileBody("file-unknown-fixture"))
	require.NoError(t, err)
	require.EqualValues(t, 2, sends.Load())
	repo.account = account
	_, err = forward(second, account, fileBody("file-gateway-fixture"))
	require.NoError(t, err)
	require.EqualValues(t, 3, sends.Load())

	for _, kind := range []string{"reasoning", "compaction"} {
		for _, stream := range []bool{false, true} {
			opaque := fmt.Sprintf("cipher-gateway-%s-%t", kind, stream)
			item := map[string]any{"type": kind, "encrypted_content": opaque}
			if kind == "reasoning" {
				item["summary"] = []any{}
			}
			itemJSON, err := json.Marshal(item)
			require.NoError(t, err)
			encryptedOutput.Store(string(itemJSON))
			firstBody, err := json.Marshal(map[string]any{"model": "gpt-6-astra", "stream": stream, "input": "first"})
			require.NoError(t, err)
			repo.account = account
			rec, err := forward(first, account, firstBody)
			require.NoError(t, err)
			require.Contains(t, rec.Body.String(), opaque, "the first gateway must actually deliver ciphertext")
			// A second route that independently returns identical state must not
			// overwrite the first owner or expose it to its client.
			repo.account = &changed
			beforeConflict := sends.Load()
			rec, err = forward(second, &changed, firstBody)
			require.Error(t, err)
			require.Equal(t, beforeConflict+1, sends.Load())
			require.NotContains(t, rec.Body.String(), opaque)
			if stream {
				require.Contains(t, rec.Body.String(), "response.failed")
			} else {
				require.Equal(t, http.StatusServiceUnavailable, rec.Code)
			}
			encryptedOutput.Store("")
			inputBody := func(value string) []byte {
				body, e := json.Marshal(map[string]any{"model": "gpt-6-astra", "stream": stream, "input": []any{map[string]any{"role": "user", "content": "continue"}, map[string]any{"type": kind, "encrypted_content": value}}})
				require.NoError(t, e)
				return body
			}
			repo.account = &changed
			before := sends.Load()
			rec, err = forward(second, &changed, inputBody(opaque))
			require.Error(t, err)
			require.Equal(t, http.StatusServiceUnavailable, rec.Code)
			require.Equal(t, before, sends.Load(), "shared foreign ciphertext must not reach upstream")
			_, err = forward(second, &changed, inputBody(opaque+"-unknown"))
			require.NoError(t, err)
			lastBody, ok := lastRequest.Load().(string)
			require.True(t, ok)
			require.Contains(t, lastBody, opaque+"-unknown")
			repo.account = account
			_, err = forward(second, account, inputBody(opaque))
			require.NoError(t, err, "the original route must recover across gateways")
		}
	}

	if setError != nil {
		for _, stream := range []bool{false, true} {
			const secret = "cipher-store-outage-fixture"
			encryptedOutput.Store(`{"type":"reasoning","encrypted_content":"` + secret + `"}`)
			body, err := json.Marshal(map[string]any{"model": "gpt-6-astra", "stream": stream, "input": "fresh text"})
			require.NoError(t, err)
			setError("fixture state unavailable")
			rec, err := forward(first, account, body)
			setError("")
			require.Error(t, err, "unrecorded ciphertext must not be delivered")
			require.NotContains(t, rec.Body.String(), secret)
			if stream {
				require.Contains(t, rec.Body.String(), "response.failed")
			} else {
				require.Equal(t, http.StatusServiceUnavailable, rec.Code)
			}
			rec, err = forward(first, account, body)
			require.NoError(t, err, "delivery recovers once the store is available")
			require.Contains(t, rec.Body.String(), secret)
			encryptedOutput.Store("")
		}
	}
}
