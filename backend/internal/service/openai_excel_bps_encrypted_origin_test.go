//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestExcelBPSEncryptedOriginBlocksKnownForeignAndRestores(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	for _, kind := range []string{"reasoning", "compaction"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", kind, stream), func(t *testing.T) {
				account := excelAccount()
				account.Proxy = &Proxy{Protocol: "http", Host: "origin.invalid", Port: 8080}
				foreign := *account
				foreign.Proxy = &Proxy{Protocol: "http", Host: "foreign.invalid", Port: 8080}
				opaque := "fixture-ciphertext-" + kind + fmt.Sprint(stream)
				item := map[string]any{"type": kind, "encrypted_content": opaque}
				if kind == "reasoning" {
					item["summary"] = []any{}
				}
				event, err := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_cipher_fixture", "status": "completed", "output": []any{item}}})
				require.NoError(t, err)
				calls := 0
				var wireBodies []string
				svc := openAIClientToolsTestService(nil)
				t.Cleanup(func() { require.NoError(t, svc.CloseExcelBPSImages()) })
				svc.httpUpstream = &bpsTestUpstream{send: func(req *http.Request, _ string) (*http.Response, error) {
					calls++
					requestWire, err := io.ReadAll(req.Body)
					require.NoError(t, err)
					wireBodies = append(wireBodies, string(requestWire))
					wire := `{"type":"response.completed","response":{"id":"resp_next","status":"completed","output":[]}}`
					if calls == 1 {
						wire = string(event)
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data: " + wire + "\n\n"))}, nil
				}}
				forward := func(a *Account, input any) (*httptest.ResponseRecorder, error) {
					body, err := json.Marshal(map[string]any{"model": "gpt-6-astra", "stream": stream, "input": input})
					require.NoError(t, err)
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
					c.Request.Header.Set("Session-Id", "cipher-origin-"+kind+fmt.Sprint(stream))
					_, err = svc.Forward(context.Background(), c, a, body)
					return rec, err
				}
				response, err := forward(account, "first")
				require.NoError(t, err)
				require.Contains(t, response.Body.String(), opaque, "the client must actually receive the opaque state")
				unknown := map[string]any{"type": kind, "encrypted_content": opaque + "-unknown"}
				input := func(value map[string]any) []any {
					return []any{map[string]any{"role": "user", "content": "continue"}, value}
				}
				_, err = forward(&foreign, input(unknown))
				require.NoError(t, err, "unknown historical state keeps its original compatibility")
				require.Contains(t, wireBodies[len(wireBodies)-1], opaque+"-unknown", "unknown state actually reaches the upstream")
				before := calls
				response, err = forward(&foreign, input(item))
				require.Error(t, err, "known foreign state must be rejected")
				require.Equal(t, http.StatusServiceUnavailable, response.Code)
				require.Equal(t, before, calls, "do not send known foreign ciphertext upstream")
				_, err = forward(account, input(item))
				require.NoError(t, err, "the original route remains eligible")
				require.Equal(t, before+1, calls)
			})
		}
	}
}

// A streaming item can be delivered even if the terminal response omits it.
// Its first actual delivery must still establish a route before it is exposed.
func TestExcelBPSEncryptedOriginRecordsStreamingItemBeforeTerminal(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	account := excelAccount()
	foreign := *account
	foreign.Proxy = &Proxy{Protocol: "http", Host: "foreign-stream.invalid", Port: 8080}
	const opaque = "early-stream-cipher-fixture"
	calls := 0
	svc := openAIClientToolsTestService(nil)
	t.Cleanup(func() { require.NoError(t, svc.CloseExcelBPSImages()) })
	svc.httpUpstream = &bpsTestUpstream{send: func(*http.Request, string) (*http.Response, error) {
		calls++
		wire := `data: {"type":"response.output_item.done","item":{"type":"reasoning","summary":[],"encrypted_content":"` + opaque + `"}}` + "\n\n" +
			`data: {"type":"response.completed","response":{"id":"resp_early","status":"completed","output":[]}}` + "\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(wire))}, nil
	}}
	forward := func(a *Account, input any) (*httptest.ResponseRecorder, error) {
		body, err := json.Marshal(map[string]any{"model": "gpt-6-astra", "stream": true, "input": input})
		require.NoError(t, err)
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
		_, err = svc.Forward(t.Context(), c, a, body)
		return rec, err
	}
	rec, err := forward(account, "first")
	require.NoError(t, err)
	require.Contains(t, rec.Body.String(), opaque)
	before := calls
	rec, err = forward(&foreign, []any{map[string]any{"role": "user", "content": "continue"}, map[string]any{"type": "reasoning", "encrypted_content": opaque}})
	require.Error(t, err)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, before, calls)
}

func TestExcelBPSEncryptedOriginRechecksAfterProxyWait(t *testing.T) {
	account := excelAccount()
	account.Extra["openai_excel_bps_mihomo"] = true
	latest := *account
	latest.Proxy = &Proxy{Protocol: "http", Host: "changed-encrypted.invalid", Port: 8080}
	repo := &turnAdmissionRepo{account: &latest}
	cache := &basispoints.CatalogCache{}
	origin := excelBPSRouteOrigin(account, "workspace-one", "opaque-token")
	const opaque = "inflight-encrypted-fixture"
	require.NoError(t, cache.RememberEncryptedOrigin(opaque, origin))
	guard := &excelBPSOriginGuard{cache: cache, origin: origin}
	ctx := context.WithValue(t.Context(), excelBPSOriginKey{}, guard)
	calls := 0
	svc := &OpenAIGatewayService{accountRepo: repo, requireLatestTurnAdmission: true, httpUpstream: &bpsTestUpstream{send: func(*http.Request, string) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	}}}
	body := []byte(`{"model":"gpt-6-astra","input":[{"type":"reasoning","encrypted_content":"` + opaque + `"}]}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	lease := &bpsTestLease{}
	acquire := func(context.Context, string, ...string) (string, excelBPSLease, error) {
		return "http://127.0.0.1:19000", lease, nil
	}
	_, _, _, err := svc.doExcelBPSRequest(ctx, c, account, "fixture", body, "opaque-token", "workspace-one", acquire)
	require.True(t, IsOpenAITurnAdmissionError(err), "%v", err)
	require.Zero(t, calls)
	require.Equal(t, 1, lease.releases)
	require.Zero(t, lease.failures)
	repo.account = account
	response, returned, _, err := svc.doExcelBPSRequest(ctx, c, account, "fixture", body, "opaque-token", "workspace-one", acquire)
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.NoError(t, response.Body.Close())
	returned.Release()
}

func TestExcelBPSEncryptedOriginClaimsDuringProxyWait(t *testing.T) {
	account := excelAccount()
	account.Extra["openai_excel_bps_mihomo"] = true
	foreign := *account
	foreign.Proxy = &Proxy{Protocol: "http", Host: "claimed-elsewhere.invalid", Port: 8080}
	cache := &basispoints.CatalogCache{}
	guard := &excelBPSOriginGuard{cache: cache, origin: excelBPSRouteOrigin(account, "workspace-one", "opaque-token")}
	ctx := context.WithValue(t.Context(), excelBPSOriginKey{}, guard)
	const opaque = "newly-claimed-encrypted-fixture"
	calls := 0
	svc := &OpenAIGatewayService{httpUpstream: &bpsTestUpstream{send: func(*http.Request, string) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	}}}
	body := []byte(`{"model":"gpt-6-astra","input":[{"type":"compaction","encrypted_content":"` + opaque + `"}]}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	lease := &bpsTestLease{}
	acquire := func(context.Context, string, ...string) (string, excelBPSLease, error) {
		require.NoError(t, cache.RememberEncryptedOrigin(opaque, excelBPSRouteOrigin(&foreign, "workspace-one", "opaque-token")))
		return "http://127.0.0.1:19000", lease, nil
	}
	_, _, _, err := svc.doExcelBPSRequest(ctx, c, account, "fixture", body, "opaque-token", "workspace-one", acquire)
	require.True(t, IsOpenAITurnAdmissionError(err), "%v", err)
	require.Zero(t, calls)
	require.Equal(t, 1, lease.releases)
	require.Zero(t, lease.failures)
}

// Non-stream clients never receive intermediate output items. Such an item must
// not claim ownership unless it is part of the returned terminal response.
func TestExcelBPSEncryptedOriginDoesNotClaimUndeliveredJSONItem(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	account := excelAccount()
	foreign := *account
	foreign.Proxy = &Proxy{Protocol: "http", Host: "undelivered-foreign.invalid", Port: 8080}
	const opaque = "not-delivered-json-cipher-fixture"
	calls := 0
	svc := openAIClientToolsTestService(nil)
	t.Cleanup(func() { require.NoError(t, svc.CloseExcelBPSImages()) })
	svc.httpUpstream = &bpsTestUpstream{send: func(req *http.Request, _ string) (*http.Response, error) {
		calls++
		if calls == 2 {
			raw, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			require.Contains(t, string(raw), opaque, "unobserved history must pass through")
		}
		wire := ""
		if calls == 1 {
			wire = `data: {"type":"response.output_item.done","item":{"type":"reasoning","encrypted_content":"` + opaque + `"}}` + "\n\n"
		}
		wire += `data: {"type":"response.completed","response":{"id":"resp_undelivered","status":"completed","output":[]}}` + "\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(wire))}, nil
	}}
	forward := func(a *Account, input any) (*httptest.ResponseRecorder, error) {
		body, err := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": input})
		require.NoError(t, err)
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
		_, err = svc.Forward(t.Context(), c, a, body)
		return rec, err
	}
	rec, err := forward(account, "first")
	require.NoError(t, err)
	require.NotContains(t, rec.Body.String(), opaque)
	_, err = forward(&foreign, []any{map[string]any{"role": "user", "content": "continue"}, map[string]any{"type": "reasoning", "encrypted_content": opaque}})
	require.NoError(t, err)
	require.Equal(t, 2, calls)
}
