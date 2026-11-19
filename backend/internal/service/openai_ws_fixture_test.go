package service

// Shared fixtures must compile with and without the unit build tag.

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	coderws "github.com/coder/websocket"
)

type availabilityWSHTTPUpstream struct{ client *http.Client }

func (u availabilityWSHTTPUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.client.Do(req)
}

func (u availabilityWSHTTPUpstream) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, id, concurrency)
}

func availabilityWSTestServices(t *testing.T, server *httptest.Server) (*AccountTestService, *OpenAIGatewayService, *Account) {
	t.Helper()
	account := &Account{ID: 901, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{"api_key": "fixture", "base_url": server.URL},
		Extra:       map[string]any{"responses_websockets_v2_enabled": true}}
	cfg := newOpenAIWSExecutionScopeTestConfig()
	upstream := availabilityWSHTTPUpstream{client: server.Client()}
	gateway := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, cache: &stubGatewayCache{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector()}
	t.Cleanup(gateway.CloseOpenAIWSPool)
	svc := &AccountTestService{cfg: cfg, httpUpstream: upstream, openaiGatewayService: gateway,
		accountRepo: &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{account.ID: account}}}}
	return svc, gateway, account
}

func qualityBudgetWSServer(t *testing.T, beforeReply ...func(int32)) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	handshakes, frames := &atomic.Int32{}, &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handshakes.Add(1)
		if r.Header.Get("Authorization") != "Bearer fixture" {
			http.Error(w, "invalid quality budget fixture credential", http.StatusUnauthorized)
			return
		}
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		for {
			_, _, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			n := frames.Add(1)
			if len(beforeReply) != 0 {
				beforeReply[0](n)
			}
			if err := conn.Write(r.Context(), coderws.MessageText, []byte(`{"type":"response.output_text.delta","delta":"21"}`)); err != nil {
				return
			}
			message := `{"type":"response.completed","response":{"id":"resp_budget_` + strconv.Itoa(int(n)) + `","status":"completed","model":"target","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"21"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`
			if err := conn.Write(r.Context(), coderws.MessageText, []byte(message)); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return server, handshakes, frames
}
