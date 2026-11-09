//go:build unit

package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// --- shared test helpers ---

type queuedHTTPUpstream struct {
	responses     []*http.Response
	requests      []*http.Request
	tlsFlags      []bool
	attachRequest bool
}

func (u *queuedHTTPUpstream) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return nil, fmt.Errorf("unexpected Do call")
}

func (u *queuedHTTPUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	u.requests = append(u.requests, req)
	u.tlsFlags = append(u.tlsFlags, profile != nil)
	if len(u.responses) == 0 {
		return nil, fmt.Errorf("no mocked response")
	}
	resp := u.responses[0]
	u.responses = u.responses[1:]
	if u.attachRequest {
		resp.Request = req
	}
	return resp, nil
}

func newJSONResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// --- test functions ---

func newTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/1/test", nil)
	return c, rec
}

type openAIAccountTestRepo struct {
	mockAccountRepoForGemini
	updatedExtra       map[string]any
	bulkUpdatedIDs     []int64
	bulkUpdatedPayload AccountBulkUpdate
	rateLimitedID      int64
	rateLimitedAt      *time.Time
	clearedErrorID     int64
	setErrorID         int64
	setErrorMsg        string
}

func (r *openAIAccountTestRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	r.updatedExtra = updates
	return nil
}

func (r *openAIAccountTestRepo) BulkUpdate(_ context.Context, ids []int64, updates AccountBulkUpdate) (int64, error) {
	r.bulkUpdatedIDs = append([]int64(nil), ids...)
	r.bulkUpdatedPayload = updates
	return int64(len(ids)), nil
}

func (r *openAIAccountTestRepo) SetRateLimited(_ context.Context, id int64, resetAt time.Time) error {
	r.rateLimitedID = id
	r.rateLimitedAt = &resetAt
	return nil
}

func (r *openAIAccountTestRepo) ClearError(_ context.Context, id int64) error {
	r.clearedErrorID = id
	return nil
}

func (r *openAIAccountTestRepo) SetError(_ context.Context, id int64, errorMsg string) error {
	r.setErrorID = id
	r.setErrorMsg = errorMsg
	return nil
}

func TestAccountTestService_OpenAISuccessPersistsSnapshotFromHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, recorder := newTestContext()

	resp := newJSONResponse(http.StatusOK, "")
	resp.Body = io.NopCloser(strings.NewReader(`data: {"type":"response.completed"}

`))
	resp.Header.Set("x-codex-primary-used-percent", "88")
	resp.Header.Set("x-codex-primary-reset-after-seconds", "604800")
	resp.Header.Set("x-codex-primary-window-minutes", "10080")
	resp.Header.Set("x-codex-secondary-used-percent", "42")
	resp.Header.Set("x-codex-secondary-reset-after-seconds", "18000")
	resp.Header.Set("x-codex-secondary-window-minutes", "300")

	repo := &openAIAccountTestRepo{}
	upstream := &queuedHTTPUpstream{responses: []*http.Response{resp}}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}
	account := &Account{
		ID:          89,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "test-token"},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")
	require.NoError(t, err)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, HTTPUpstreamProfileOpenAI, HTTPUpstreamProfileFromContext(upstream.requests[0].Context()))
	require.NotEmpty(t, repo.updatedExtra)
	require.Equal(t, 42.0, repo.updatedExtra["codex_5h_used_percent"])
	require.Equal(t, 88.0, repo.updatedExtra["codex_7d_used_percent"])
	require.Contains(t, recorder.Body.String(), "test_complete")
}

func managedConnectionProbeFixture(t *testing.T, resp *http.Response) (*AccountTestService, *OpenAIOAuthReauthService, *connectionTypeRepo, *openAIAccountTestRepo, *Account, *queuedHTTPUpstream) {
	t.Helper()
	upstream := &queuedHTTPUpstream{responses: []*http.Response{resp}}
	reauth, connectionRepo := connectionTypeService(AccountTypeOAuth)
	parent, err := reauth.accounts.GetAccount(context.Background(), 42)
	require.NoError(t, err)
	parent.Extra = map[string]any{"openai_connections_managed": true}
	connection := &OpenAIAuthConnection{
		ID:           "c910c45b-79a0-4367-a1dc-60e12210e199",
		AccountID:    parent.ID,
		Kind:         "codex",
		Status:       "active",
		RefreshState: "idle",
		Revision:     1,
		Preferred:    true,
		Capabilities: []string{"responses", "chat_completions", "responses_compact"},
	}
	require.NoError(t, reauth.encryptConnectionCredentials(connection, map[string]any{
		"access_token": "managed-connection-token",
		"client_id":    openai.ClientID,
		"expires_at":   fmt.Sprint(time.Now().Add(time.Hour).Unix()),
	}))
	connectionRepo.rows = []*OpenAIAuthConnection{connection}
	connectionRepo.managed = true
	repo := &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{
		accountsByID: map[int64]*Account{parent.ID: parent},
	}}
	gateway := &OpenAIGatewayService{excelOAuthReauth: reauth, cfg: &config.Config{}}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream, openaiGatewayService: gateway}
	return svc, reauth, connectionRepo, repo, parent, upstream
}

func TestAccountTestService_ManagedConnectionProbeUsesProjectedCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, recorder := newTestContext()
	resp := newJSONResponse(http.StatusOK, "")
	resp.Body = io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\"}\n\n"))
	svc, _, _, _, parent, upstream := managedConnectionProbeFixture(t, resp)

	require.NoError(t, svc.TestAccountConnection(ctx, parent.ID, "gpt-5.4", "", ""))
	require.Len(t, upstream.requests, 1)
	require.Equal(t, "Bearer managed-connection-token", upstream.requests[0].Header.Get("Authorization"))
	require.NotContains(t, recorder.Body.String(), "OPENAI_CONNECTION_NOT_CAPABLE")
}

type managedProbeChangingConnections struct {
	*connectionTypeRepo
	lists int
}

func (r *managedProbeChangingConnections) ListAuthConnections(ctx context.Context, id int64) ([]*OpenAIAuthConnection, error) {
	r.lists++
	if r.lists == 4 {
		r.rows[0].Revision++
	}
	return r.connectionTypeRepo.ListAuthConnections(ctx, id)
}

func TestAccountTestService_ManagedConnectionProbeFencesRevisionBeforeSend(t *testing.T) {
	ctx, _ := newTestContext()
	svc, reauth, connections, _, parent, upstream := managedConnectionProbeFixture(t, newJSONResponse(http.StatusOK, ""))
	changing := &managedProbeChangingConnections{connectionTypeRepo: connections}
	reauth.repo = changing
	err := svc.TestAccountConnection(ctx, parent.ID, "gpt-5.4", "", "")
	require.Error(t, err, "a revoked or revised connection must be rejected before sending")
	require.GreaterOrEqual(t, changing.lists, 4)
	require.Empty(t, upstream.requests)
}

func TestAccountTestService_ManagedConnectionProbe401PreservesParent(t *testing.T) {
	ctx, _ := newTestContext()
	svc, _, _, repo, parent, upstream := managedConnectionProbeFixture(t, newJSONResponse(http.StatusUnauthorized, `{}`))
	err := svc.TestAccountConnection(ctx, parent.ID, "gpt-5.4", "", "")
	require.Error(t, err)
	require.Len(t, upstream.requests, 1)
	require.Zero(t, repo.setErrorID, "a managed grant's 401 must not disable its parent account")
}

func TestAccountTestService_ManagedCompactProbePreservesParentState(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			ctx, _ := newTestContext()
			resp := newJSONResponse(status, compactProbeSSESuccessBody)
			resp.Header.Set("x-codex-primary-used-percent", "99")
			svc, _, _, repo, parent, upstream := managedConnectionProbeFixture(t, resp)
			err := svc.TestAccountConnection(ctx, parent.ID, "gpt-5.4", "", AccountTestModeCompact)
			if status == http.StatusOK {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.Len(t, upstream.requests, 1)
			require.Zero(t, repo.setErrorID)
			require.Empty(t, repo.updatedExtra, "one managed grant cannot write account-wide compact or quota observations")
		})
	}
}

func TestAccountTestService_ManagedConnectionBackgroundProbeRecordsScope(t *testing.T) {
	wire := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"21\"}\n\n" +
		"data: {\"type\":\"response.completed\"}\n\n"
	resp := newJSONResponse(http.StatusOK, wire)
	resp.Header.Set("Content-Type", "text/event-stream")
	svc, _, _, _, parent, upstream := managedConnectionProbeFixture(t, resp)
	upstream.attachRequest = true
	cfg := &PelicanTestConfig{QuestionKind: "candy", Prompt: "Return 21", ReasoningEffort: "high", ParallelCount: 1,
		Availability: &AvailabilityPolicy{ValidForSeconds: 60}}
	result, err := svc.RunPelicanBackground(t.Context(), parent.ID, "gpt-5.4", cfg)
	require.NoError(t, err)
	require.Equal(t, "success", result.Status, result.ErrorMessage)
	probe := result.PelicanConfig.AvailabilityResult
	require.Equal(t, "pass", probe.Status)
	require.NotNil(t, probe.Scope)
	require.NotEmpty(t, probe.Scope.ConnectionScope)
	require.NotContains(t, probe.Scope.ConnectionScope, "connection")
	require.Len(t, upstream.requests, 1)
	require.Equal(t, "Bearer managed-connection-token", upstream.requests[0].Header.Get("Authorization"))
}

func TestAccountTestService_BackgroundProbePinsManagedConnection(t *testing.T) {
	const peerID = "8110c45b-79a0-4367-a1dc-60e12210e199"
	for _, scenario := range []string{"preferred", "peer", "missing", "disabled", "managed_disabled", "wrong_account", "gateway_unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			wire := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"21\"}\n\n" +
				"data: {\"type\":\"response.completed\"}\n\n"
			resp := newJSONResponse(http.StatusOK, wire)
			resp.Header.Set("Content-Type", "text/event-stream")
			svc, reauth, connections, _, parent, upstream := managedConnectionProbeFixture(t, resp)
			upstream.attachRequest = true
			peer := &OpenAIAuthConnection{ID: peerID, AccountID: parent.ID, Kind: "codex", Status: "active", Revision: 1, Capabilities: []string{"responses"}}
			require.NoError(t, reauth.encryptConnectionCredentials(peer, map[string]any{"access_token": "synthetic-peer-token", "expires_at": fmt.Sprint(time.Now().Add(time.Hour).Unix())}))
			connections.rows = append(connections.rows, peer)
			ctx := t.Context()
			switch scenario {
			case "peer":
				ctx = withAvailabilityProbeConnection(ctx, parent.ID, peerID)
			case "missing":
				ctx = withAvailabilityProbeConnection(ctx, parent.ID, "badc45b0-79a0-4367-a1dc-60e12210e199")
			case "disabled":
				peer.Status = "disabled"
				ctx = withAvailabilityProbeConnection(ctx, parent.ID, peerID)
			case "managed_disabled":
				connections.managed = false
				ctx = withAvailabilityProbeConnection(ctx, parent.ID, peerID)
			case "wrong_account":
				ctx = withAvailabilityProbeConnection(ctx, parent.ID+1, peerID)
			case "gateway_unavailable":
				svc.openaiGatewayService = nil
				ctx = withAvailabilityProbeConnection(ctx, parent.ID, peerID)
			}
			cfg := &PelicanTestConfig{QuestionKind: "candy", Prompt: "Return 21", ReasoningEffort: "high", ParallelCount: 1, Availability: &AvailabilityPolicy{ValidForSeconds: 60}}
			result, err := svc.RunPelicanBackground(ctx, parent.ID, "gpt-5.4", cfg)
			require.NoError(t, err)
			if scenario == "preferred" || scenario == "peer" {
				require.Equal(t, "success", result.Status, result.ErrorMessage)
				require.Len(t, upstream.requests, 1)
				token := "managed-connection-token"
				id := connections.rows[0].ID
				if scenario == "peer" {
					token, id = "synthetic-peer-token", peerID
				}
				require.Equal(t, "Bearer "+token, upstream.requests[0].Header.Get("Authorization"))
				require.Equal(t, AvailabilityConnectionScope(&Account{AuthConnectionID: id, AuthConnectionRevision: 1}), result.PelicanConfig.AvailabilityResult.Scope.ConnectionScope)
			} else {
				require.Equal(t, "failed", result.Status, "an unavailable explicit target must not silently use the preferred grant")
				require.Empty(t, upstream.requests)
			}
		})
	}
}

func TestAccountTestService_ManagedGatewayProbeRecordsScope(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	for _, kind := range []string{"bps", "siwc"} {
		t.Run(kind, func(t *testing.T) {
			svc, reauth, connections, _, parent, _ := managedConnectionProbeFixture(t, newJSONResponse(http.StatusOK, ""))
			connection := connections.rows[0]
			connection.Kind = kind
			connection.Capabilities = []string{"responses"}
			clientID := "synthetic-siwc-client"
			if kind == "bps" {
				clientID = openai.ExcelClientID
			}
			require.NoError(t, reauth.encryptConnectionCredentials(connection, map[string]any{
				"access_token": "managed-connection-token", "client_id": clientID,
				"chatgpt_account_id": "synthetic-workspace", "expires_at": fmt.Sprint(time.Now().Add(time.Hour).Unix()),
			}))
			calls := 0
			upstream := &bpsTestUpstream{send: func(req *http.Request, _ string) (*http.Response, error) {
				calls++
				require.Equal(t, "Bearer managed-connection-token", req.Header.Get("Authorization"))
				wire := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"21\"}\n\n" +
					"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixture\",\"status\":\"completed\",\"output\":[]}}\n\n"
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Request: req,
					Body: io.NopCloser(strings.NewReader(wire))}, nil
			}}
			gateway := openAIClientToolsTestService(nil)
			gateway.httpUpstream, gateway.excelOAuthReauth = upstream, reauth
			svc.httpUpstream, svc.openaiGatewayService = upstream, gateway
			t.Cleanup(func() { require.NoError(t, gateway.CloseExcelBPSImages()) })
			cfg := &PelicanTestConfig{QuestionKind: "candy", Prompt: "Return 21", ReasoningEffort: "high", ParallelCount: 1,
				Availability: &AvailabilityPolicy{ValidForSeconds: 60}}
			result, err := svc.RunPelicanBackground(t.Context(), parent.ID, "gpt-5.4", cfg)
			require.NoError(t, err)
			require.Equal(t, "success", result.Status, result.ErrorMessage)
			require.Equal(t, 1, calls)
			probe := result.PelicanConfig.AvailabilityResult
			require.Equal(t, "pass", probe.Status)
			require.NotNil(t, probe.Scope)
			require.NotEmpty(t, probe.Scope.ConnectionScope)
			if kind == "bps" {
				require.Equal(t, "bps_responses", probe.Scope.Protocol)
			} else {
				require.Equal(t, "responses_http", probe.Scope.Protocol)
			}
		})
	}
}

func TestAccountTestService_OpenAIOAuthTestNormalizesGPT56Alias(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := newTestContext()

	resp := newJSONResponse(http.StatusOK, "")
	resp.Body = io.NopCloser(strings.NewReader(`data: {"type":"response.completed"}

`))

	upstream := &queuedHTTPUpstream{responses: []*http.Response{resp}}
	svc := &AccountTestService{httpUpstream: upstream}
	account := &Account{
		ID:          90,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "test-token"},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.6", "", "")
	require.NoError(t, err)
	require.Len(t, upstream.requests, 1)

	body, err := io.ReadAll(upstream.requests[0].Body)
	require.NoError(t, err)
	require.Equal(t, "gpt-5.6-sol", gjson.GetBytes(body, "model").String())
}

func TestAccountTestService_OpenAIShadowUsesParentCredentialsAndShadowModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, recorder := newTestContext()

	resp := newJSONResponse(http.StatusOK, "")
	resp.Body = io.NopCloser(strings.NewReader(`data: {"type":"response.completed"}

`))

	parentID := int64(100)
	parent := &Account{
		ID:       parentID,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Status:   StatusActive,
		Credentials: map[string]any{
			"access_token":       "parent-token",
			"chatgpt_account_id": "org-parent",
		},
	}
	shadow := &Account{
		ID:              200,
		Platform:        PlatformOpenAI,
		Type:            AccountTypeOAuth,
		Status:          StatusActive,
		ParentAccountID: &parentID,
		QuotaDimension:  QuotaDimensionSpark,
		Concurrency:     2,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"gpt-5.3-codex-spark": "gpt-5.3-codex-spark",
			},
		},
	}

	repo := &openAIAccountTestRepo{
		mockAccountRepoForGemini: mockAccountRepoForGemini{
			accountsByID: map[int64]*Account{
				parentID: parent,
				200:      shadow,
			},
		},
	}
	upstream := &queuedHTTPUpstream{responses: []*http.Response{resp}}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}

	err := svc.TestAccountConnection(ctx, shadow.ID, "gpt-5.3-codex-spark", "", "")
	require.NoError(t, err)
	require.Len(t, upstream.requests, 1)
	req := upstream.requests[0]
	require.Equal(t, "Bearer parent-token", req.Header.Get("Authorization"))
	require.Equal(t, "org-parent", req.Header.Get("chatgpt-account-id"))
	body, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Equal(t, "gpt-5.3-codex-spark", gjson.GetBytes(body, "model").String())
	require.Contains(t, recorder.Body.String(), `"success":true`)
}

func TestAccountTestService_OpenAIStreamEOFBeforeCompletedFails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, recorder := newTestContext()

	resp := newJSONResponse(http.StatusOK, "")
	resp.Body = io.NopCloser(strings.NewReader(`data: {"type":"response.output_text.delta","delta":"hi"}

`))

	upstream := &queuedHTTPUpstream{responses: []*http.Response{resp}}
	svc := &AccountTestService{httpUpstream: upstream}
	account := &Account{
		ID:          90,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "test-token"},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")
	require.Error(t, err)
	require.Contains(t, recorder.Body.String(), "response.completed")
	require.NotContains(t, recorder.Body.String(), `"success":true`)
}

func TestAccountTestService_DeepSeekCustomBaseURLUsesV1ResponsesPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := newTestContext()

	resp := newJSONResponse(http.StatusOK, "")
	resp.Body = io.NopCloser(strings.NewReader(`data: {"type":"response.completed"}

`))
	upstream := &queuedHTTPUpstream{responses: []*http.Response{resp}}
	svc := &AccountTestService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          91,
		Platform:    PlatformDeepseek,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":      "sk-test",
			"base_url":     "https://relay.example.com/v1",
			"api_protocol": APIProtocolResponses,
		},
		Extra: map[string]any{
			openai_compat.ExtraKeyResponsesSupported: true,
		},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")
	require.NoError(t, err)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, "https://relay.example.com/v1/responses", upstream.requests[0].URL.String())
}

func TestAccountTestService_DeepSeekResponsesRoutesToOpenAIProbe(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := newTestContext()

	resp := newJSONResponse(http.StatusOK, "")
	resp.Body = io.NopCloser(strings.NewReader(`data: {"type":"response.completed"}

`))
	upstream := &queuedHTTPUpstream{responses: []*http.Response{resp}}
	svc := &AccountTestService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          93,
		Platform:    PlatformDeepseek,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":      "sk-test",
			"base_url":     "https://relay.example.com/v1",
			"api_protocol": APIProtocolResponses,
		},
		Extra: map[string]any{
			openai_compat.ExtraKeyResponsesSupported: true,
		},
	}
	repo := &openAIAccountTestRepo{
		mockAccountRepoForGemini: mockAccountRepoForGemini{
			accountsByID: map[int64]*Account{93: account},
		},
	}
	svc.accountRepo = repo

	err := svc.TestAccountConnection(ctx, account.ID, "gpt-5.4", "", "")
	require.NoError(t, err)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, "https://relay.example.com/v1/responses", upstream.requests[0].URL.String())
}

func TestAccountTestService_DeepSeekDefaultBaseURLUsesNativeResponsesPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := newTestContext()

	resp := newJSONResponse(http.StatusOK, "")
	resp.Body = io.NopCloser(strings.NewReader(`data: {"type":"response.completed"}

`))
	upstream := &queuedHTTPUpstream{responses: []*http.Response{resp}}
	svc := &AccountTestService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          92,
		Platform:    PlatformDeepseek,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":      "sk-test",
			"api_protocol": APIProtocolResponses,
		},
		Extra: map[string]any{
			openai_compat.ExtraKeyResponsesSupported: true,
		},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")
	require.NoError(t, err)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, "https://api.deepseek.com/responses", upstream.requests[0].URL.String())
}

func TestAccountTestService_OpenAI429PersistsSnapshotAndRateLimitState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := newTestContext()

	resp := newJSONResponse(http.StatusTooManyRequests, `{"error":{"type":"usage_limit_reached","message":"limit reached","resets_at":1777283883}}`)
	resp.Header.Set("x-codex-primary-used-percent", "100")
	resp.Header.Set("x-codex-primary-reset-after-seconds", "604800")
	resp.Header.Set("x-codex-primary-window-minutes", "10080")
	resp.Header.Set("x-codex-secondary-used-percent", "100")
	resp.Header.Set("x-codex-secondary-reset-after-seconds", "18000")
	resp.Header.Set("x-codex-secondary-window-minutes", "300")

	repo := &openAIAccountTestRepo{}
	upstream := &queuedHTTPUpstream{responses: []*http.Response{resp}}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}
	account := &Account{
		ID:          88,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusError,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "test-token"},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")
	require.Error(t, err)
	require.NotEmpty(t, repo.updatedExtra)
	require.Equal(t, 100.0, repo.updatedExtra["codex_5h_used_percent"])
	require.Equal(t, account.ID, repo.rateLimitedID)
	require.NotNil(t, repo.rateLimitedAt)
	require.Equal(t, account.ID, repo.clearedErrorID)
	require.Equal(t, StatusActive, account.Status)
	require.Empty(t, account.ErrorMessage)
	require.NotNil(t, account.RateLimitResetAt)
}

func TestAccountTestService_OpenAI429BodyOnlyPersistsRateLimitAndClearsStaleError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := newTestContext()

	resp := newJSONResponse(http.StatusTooManyRequests, `{"error":{"type":"usage_limit_reached","message":"limit reached","resets_at":"1777283883"}}`)

	repo := &openAIAccountTestRepo{}
	upstream := &queuedHTTPUpstream{responses: []*http.Response{resp}}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}
	account := &Account{
		ID:           77,
		Platform:     PlatformOpenAI,
		Type:         AccountTypeOAuth,
		Status:       StatusError,
		ErrorMessage: "Access forbidden (403): account may be suspended or lack permissions",
		Concurrency:  1,
		Credentials:  map[string]any{"access_token": "test-token"},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")
	require.Error(t, err)
	require.Equal(t, account.ID, repo.rateLimitedID)
	require.NotNil(t, repo.rateLimitedAt)
	require.Equal(t, account.ID, repo.clearedErrorID)
	require.Equal(t, StatusActive, account.Status)
	require.Empty(t, account.ErrorMessage)
	require.NotNil(t, account.RateLimitResetAt)
	require.Empty(t, repo.updatedExtra)
}

func TestAccountTestService_OpenAIAPIKey429DoesNotSyncUpstreamPlanType(t *testing.T) {
	for _, currentPlan := range []string{"", "plus"} {
		for _, errorType := range []string{"usage_limit_reached", "rate_limit_exceeded"} {
			t.Run(currentPlan+"/"+errorType, func(t *testing.T) {
				repo := &openAIAccountTestRepo{}
				svc := &AccountTestService{accountRepo: repo}
				credentials := map[string]any{"api_key": "test-key"}
				if currentPlan != "" {
					credentials["plan_type"] = currentPlan
				}
				account := &Account{ID: 82, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Credentials: credentials}
				body := []byte(`{"error":{"type":"` + errorType + `","plan_type":"pro","resets_at":4102444800}}`)

				svc.reconcileOpenAI429State(context.Background(), account, http.Header{}, body)

				require.Empty(t, repo.bulkUpdatedIDs)
				require.Nil(t, repo.bulkUpdatedPayload.Credentials)
				require.Equal(t, currentPlan, account.GetCredential("plan_type"))
				if currentPlan == "" {
					require.NotContains(t, account.Credentials, "plan_type")
				}
				require.Equal(t, AccountTypeAPIKey, account.Type)
				require.Equal(t, "test-key", account.GetCredential("api_key"))
				require.Zero(t, repo.rateLimitedID)
				require.Nil(t, account.RateLimitedAt)
				require.Nil(t, account.RateLimitResetAt)
			})
		}
	}
}

func TestAccountTestService_OpenAI429SyncsObservedPlanType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := newTestContext()

	resp := newJSONResponse(http.StatusTooManyRequests, `{"error":{"type":"usage_limit_reached","message":"limit reached","plan_type":"free","resets_at":1777283883}}`)

	repo := &openAIAccountTestRepo{}
	upstream := &queuedHTTPUpstream{responses: []*http.Response{resp}}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}
	account := &Account{
		ID:          81,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "test-token", "plan_type": "plus"},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")
	require.Error(t, err)
	require.Equal(t, []int64{account.ID}, repo.bulkUpdatedIDs)
	require.Equal(t, "free", repo.bulkUpdatedPayload.Credentials["plan_type"])
	require.Equal(t, "free", account.Credentials["plan_type"])
	require.Equal(t, account.ID, repo.rateLimitedID)
	require.NotNil(t, account.RateLimitResetAt)
}

func TestAccountTestService_OpenAI429ActiveAccountDoesNotClearError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := newTestContext()

	resp := newJSONResponse(http.StatusTooManyRequests, `{"error":{"type":"usage_limit_reached","message":"limit reached","resets_in_seconds":3600}}`)

	repo := &openAIAccountTestRepo{}
	upstream := &queuedHTTPUpstream{responses: []*http.Response{resp}}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}
	account := &Account{
		ID:          78,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "test-token"},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")
	require.Error(t, err)
	require.Equal(t, account.ID, repo.rateLimitedID)
	require.NotNil(t, repo.rateLimitedAt)
	require.Zero(t, repo.clearedErrorID)
	require.Equal(t, StatusActive, account.Status)
	require.NotNil(t, account.RateLimitResetAt)
}

func TestAccountTestService_OpenAI429WithoutResetSignalDoesNotMutateRuntimeState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := newTestContext()

	resp := newJSONResponse(http.StatusTooManyRequests, `{"error":{"type":"usage_limit_reached","message":"limit reached"}}`)

	repo := &openAIAccountTestRepo{}
	upstream := &queuedHTTPUpstream{responses: []*http.Response{resp}}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}
	account := &Account{
		ID:           79,
		Platform:     PlatformOpenAI,
		Type:         AccountTypeOAuth,
		Status:       StatusError,
		ErrorMessage: "stale 403",
		Concurrency:  1,
		Credentials:  map[string]any{"access_token": "test-token"},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")
	require.Error(t, err)
	require.Zero(t, repo.rateLimitedID)
	require.Nil(t, repo.rateLimitedAt)
	require.Zero(t, repo.clearedErrorID)
	require.Equal(t, StatusError, account.Status)
	require.Equal(t, "stale 403", account.ErrorMessage)
	require.Nil(t, account.RateLimitResetAt)
}

func TestAccountTestService_OpenAI401SetsPermanentErrorOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := newTestContext()

	resp := newJSONResponse(http.StatusUnauthorized, `{"error":"bad token"}`)

	repo := &openAIAccountTestRepo{}
	upstream := &queuedHTTPUpstream{responses: []*http.Response{resp}}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}
	account := &Account{
		ID:          80,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "test-token"},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")
	require.Error(t, err)
	require.Equal(t, account.ID, repo.setErrorID)
	require.Contains(t, repo.setErrorMsg, "Authentication failed (401)")
	require.Zero(t, repo.rateLimitedID)
	require.Zero(t, repo.clearedErrorID)
	require.Nil(t, account.RateLimitResetAt)
}

func TestAccountTestService_OpenAIAPIKeyResponsesUsesCodexProbeHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := newTestContext()

	resp := newJSONResponse(http.StatusOK, "")
	resp.Body = io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\"}\n\n"))
	upstream := &queuedHTTPUpstream{responses: []*http.Response{resp}}
	svc := &AccountTestService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          95,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://compat-upstream.example/v1",
		},
		Extra: map[string]any{openai_compat.ExtraKeyResponsesSupported: true},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")
	require.NoError(t, err)
	require.Len(t, upstream.requests, 1)
	req := upstream.requests[0]
	require.Equal(t, "https://compat-upstream.example/v1/responses", req.URL.String())
	requireOpenAICodexProbeHeaders(t, req.Header)
}

func TestAccountTestService_OpenAIAPIKeyResponsesUnsupportedUsesChatCompletionsPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, recorder := newTestContext()

	upstreamBody := strings.Join([]string{
		`data: {"id":"chatcmpl_test","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"pong"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_test","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	svc := &AccountTestService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          91,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://compat-upstream.example/v1",
		},
		Extra: map[string]any{openai_compat.ExtraKeyResponsesSupported: false},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "hello", "")
	require.NoError(t, err)
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, HTTPUpstreamProfileOpenAI, HTTPUpstreamProfileFromContext(upstream.lastReq.Context()))
	require.Equal(t, "https://compat-upstream.example/v1/chat/completions", upstream.lastReq.URL.String())
	require.Equal(t, "Bearer sk-test", upstream.lastReq.Header.Get("Authorization"))
	require.Equal(t, "text/event-stream", upstream.lastReq.Header.Get("Accept"))
	require.Equal(t, "gpt-5.4", gjson.GetBytes(upstream.lastBody, "model").String())
	require.True(t, gjson.GetBytes(upstream.lastBody, "stream").Bool())
	require.Equal(t, "hello", gjson.GetBytes(upstream.lastBody, "messages.0.content").String())
	require.False(t, gjson.GetBytes(upstream.lastBody, "input").Exists())
	body := recorder.Body.String()
	require.Contains(t, body, "pong")
	require.Contains(t, body, "已通过 /v1/chat/completions 验证")
	require.Contains(t, body, `"success":true`)
	require.NotContains(t, body, "当前测试接口仅支持 Responses API 路径")
}

func TestAccountTestService_OpenAIChatCompletionsPathReturns4xx(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, recorder := newTestContext()

	upstream := &httpUpstreamRecorder{resp: newJSONResponse(http.StatusBadRequest, `{"error":{"message":"bad request"}}`)}
	svc := &AccountTestService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          92,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://compat-upstream.example",
		},
		Extra: map[string]any{openai_compat.ExtraKeyResponsesSupported: false},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")
	require.Error(t, err)
	require.Equal(t, "https://compat-upstream.example/v1/chat/completions", upstream.lastReq.URL.String())
	require.Contains(t, err.Error(), "Chat Completions API (/v1/chat/completions) returned 400")
	require.Contains(t, recorder.Body.String(), "/v1/chat/completions")
	require.NotContains(t, recorder.Body.String(), `"success":true`)
}

func TestAccountTestService_OpenAIChatCompletionsPathTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, recorder := newTestContext()

	upstream := &httpUpstreamRecorder{err: context.DeadlineExceeded}
	svc := &AccountTestService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          93,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://compat-upstream.example",
		},
		Extra: map[string]any{openai_compat.ExtraKeyResponsesSupported: false},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")
	require.Error(t, err)
	require.Equal(t, "https://compat-upstream.example/v1/chat/completions", upstream.lastReq.URL.String())
	require.Contains(t, err.Error(), "Chat Completions API (/v1/chat/completions) request failed")
	require.Contains(t, err.Error(), context.DeadlineExceeded.Error())
	require.Contains(t, recorder.Body.String(), "/v1/chat/completions")
	require.NotContains(t, recorder.Body.String(), `"success":true`)
}

func TestAccountTestService_OpenAIChatCompletionsPathRejectsNonJSONStream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, recorder := newTestContext()

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader("data: not-json\n\n")),
	}}
	svc := &AccountTestService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          94,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://compat-upstream.example",
		},
		Extra: map[string]any{openai_compat.ExtraKeyResponsesSupported: false},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")
	require.Error(t, err)
	require.Equal(t, "https://compat-upstream.example/v1/chat/completions", upstream.lastReq.URL.String())
	require.Contains(t, err.Error(), "Invalid Chat Completions response from /v1/chat/completions")
	require.Contains(t, recorder.Body.String(), "/v1/chat/completions")
	require.NotContains(t, recorder.Body.String(), `"success":true`)
}

func TestAccountTestService_AstraSourceProbeDoesNotChangeSchedulingState(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			ctx, _ := newTestContext()
			ctx.Request = ctx.Request.WithContext(WithAstraSourceAcquisition(ctx.Request.Context()))
			resp := newJSONResponse(status, `{"error":{"type":"usage_limit_reached","resets_at":1999999999}}`)
			resp.Header.Set("x-codex-primary-used-percent", "100")
			resp.Header.Set("x-codex-primary-reset-after-seconds", "3600")
			repo := &openAIAccountTestRepo{}
			svc := &AccountTestService{accountRepo: repo, httpUpstream: &queuedHTTPUpstream{responses: []*http.Response{resp}}}
			account := &Account{ID: 88, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"access_token": "test-token"}}
			require.Error(t, svc.testOpenAIAccountConnection(ctx, account, "gpt-6-astra", "Reply OK", ""))
			require.Zero(t, repo.setErrorID)
			require.Zero(t, repo.rateLimitedID)
			require.Zero(t, repo.clearedErrorID)
			require.Empty(t, repo.updatedExtra)
			require.Equal(t, StatusActive, account.Status)
			require.True(t, account.Schedulable)
			require.Nil(t, account.RateLimitResetAt)
		})
	}
}
