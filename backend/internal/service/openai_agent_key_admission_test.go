package service

import (
	"context"
	"crypto/ed25519"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func agentKeyAccount(t *testing.T, id int64) (*Account, agentIdentityKey) {
	t.Helper()
	key, privateKey := newTestAgentIdentityKey(t)
	return &Account{ID: id, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{
		"auth_mode": OpenAIAuthModeAgentIdentity, "agent_runtime_id": key.runtimeID,
		"agent_private_key": privateKey, "task_id": key.taskID}}, key
}

func agentKeyFingerprint(t *testing.T, key agentIdentityKey) string {
	t.Helper()
	publicKey, ok := key.privateKey.Public().(ed25519.PublicKey)
	require.True(t, ok)
	return config.UpstreamAgentKeyFingerprint(publicKey)
}

func agentKeyRequest(t *testing.T, key agentIdentityKey) *http.Request {
	t.Helper()
	assertion, err := buildAgentAssertion(key, time.Now())
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "https://fixture.invalid/v1/responses", strings.NewReader("{}"))
	req.Header.Set("Authorization", assertion)
	return req
}

func TestOpenAIAgentKeyHTTPFinalProofAndShadowParent(t *testing.T) {
	account, key := agentKeyAccount(t, 701)
	req := agentKeyRequest(t, key)
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	resp, err := svc.doOpenAIUpstream(req, "", account)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, []string{agentKeyFingerprint(t, key)}, config.UpstreamCredentialFingerprints(upstream.lastReq))
	for name := range upstream.lastReq.Header {
		require.False(t, strings.Contains(strings.ToLower(name), "agent-public-key"), "the proof must not add a wire header")
	}

	shadow := &Account{ID: 702, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &account.ID}
	repo := &stubOpenAIAccountRepo{accounts: []Account{*account}}
	bound := bindOpenAIAgentPublicKey(agentKeyRequest(t, key), repo, shadow)
	require.Equal(t, []string{agentKeyFingerprint(t, key)}, config.UpstreamCredentialFingerprints(bound))
	unbound := bindOpenAIAgentPublicKey(agentKeyRequest(t, key), nil, shadow)
	require.Equal(t, []string{config.UnverifiedUpstreamAgentCredential}, config.UpstreamCredentialFingerprints(unbound))

	other, _ := agentKeyAccount(t, 703)
	mismatched := bindOpenAIAgentPublicKey(agentKeyRequest(t, key), nil, other)
	require.Equal(t, []string{config.UnverifiedUpstreamAgentCredential}, config.UpstreamCredentialFingerprints(mismatched), "stale or replaced signing keys cannot claim the old proof")
}

func TestOpenAIAgentKeyWSFinalHandshakeAndRotation(t *testing.T) {
	account, key := agentKeyAccount(t, 704)
	pool := newOpenAIWSConnPool(&config.Config{})
	t.Cleanup(pool.Close)
	dialer := &openAIWSCaptureDialer{conn: &openAIWSCaptureConn{}}
	pool.setClientDialerForTest(dialer)
	oldAssertion, err := buildAgentAssertion(key, time.Now().Add(-time.Minute))
	require.NoError(t, err)
	key.runtimeID, key.taskID = "new-runtime", "new-task"
	finalAssertion, err := buildAgentAssertion(key, time.Now())
	require.NoError(t, err)
	conn, err := pool.dialConn(t.Context(), openAIWSAcquireRequest{
		Account: account, WSURL: "wss://fixture.invalid/v1/responses",
		Headers: http.Header{"Authorization": {oldAssertion}},
		HeadersFactory: func(_ context.Context, headers http.Header) (http.Header, error) {
			headers.Set("Authorization", finalAssertion)
			return headers, nil
		},
		BindAgentPublicKey: func(req *http.Request) *http.Request {
			return bindOpenAIAgentPublicKey(req, nil, account)
		},
	})
	require.NoError(t, err)
	t.Cleanup(conn.close)
	want := agentKeyFingerprint(t, key)
	require.Equal(t, []string{want}, conn.upstreamKeyFingerprints)
	require.True(t, dialer.lastHeaders.Get("Authorization") == finalAssertion, "the fingerprint must describe the final dial assertion")
	rotated, _ := agentKeyAccount(t, 705)
	account.Credentials["agent_private_key"] = rotated.Credentials["agent_private_key"]
	require.Equal(t, []string{want}, conn.upstreamKeyFingerprints, "a pooled connection retains its actual handshake key")
	stale := bindOpenAIAgentPublicKey(agentKeyRequest(t, key), nil, account)
	require.Equal(t, []string{config.UnverifiedUpstreamAgentCredential}, config.UpstreamCredentialFingerprints(stale))
}

type agentKeyWSAuthority struct{ calls atomic.Int32 }

func (a *agentKeyWSAuthority) AcquireUpstreamKeySlots(ctx context.Context, _ []string) (context.Context, func(), error) {
	a.calls.Add(1)
	return ctx, nil, nil
}

func TestOpenAIAgentKeyWSRejectsUnverifiedProofBeforeGeneration(t *testing.T) {
	authority := &agentKeyWSAuthority{}
	turn := &openAIWSKeyTurn{authority: authority, fingerprints: []string{config.UnverifiedUpstreamAgentCredential}}
	_, err := turn.begin(t.Context())
	require.True(t, IsUpstreamKeyAdmissionFailure(err))
	require.Zero(t, authority.calls.Load(), "an unverified assertion must not be treated as an unlimited key")
	turn.authority = nil
	ctx, err := turn.begin(t.Context())
	require.NoError(t, err)
	require.Same(t, t.Context(), ctx, "disabled shared policy retains existing behavior")
}
