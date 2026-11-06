package config

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func upstreamAgentFixtureAssertion(t *testing.T, key ed25519.PrivateKey, runtime, task, timestamp string) string {
	t.Helper()
	signature := ed25519.Sign(key, []byte(runtime+":"+task+":"+timestamp))
	body, err := json.Marshal(map[string]string{"agent_runtime_id": runtime, "task_id": task,
		"timestamp": timestamp, "signature": base64.StdEncoding.EncodeToString(signature)})
	require.NoError(t, err)
	return "AgentAssertion " + base64.RawURLEncoding.EncodeToString(body)
}

func TestUpstreamAgentCredentialProofAndKeyIdentity(t *testing.T) {
	first := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize))
	second := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, ed25519.SeedSize))
	firstPublic, ok := first.Public().(ed25519.PublicKey)
	require.True(t, ok)
	secondPublic, ok := second.Public().(ed25519.PublicKey)
	require.True(t, ok)
	assertion := upstreamAgentFixtureAssertion(t, first, "runtime-a", "task-a", "2026-01-01T00:00:00Z")
	request := httptest.NewRequest(http.MethodPost, "https://fixture.invalid/v1/responses", nil)
	request.Header.Set("Authorization", assertion)
	firstFingerprint := UpstreamAgentKeyFingerprint(firstPublic)
	require.NotEmpty(t, firstFingerprint)
	require.NotEqual(t, firstFingerprint, UpstreamAgentKeyFingerprint(secondPublic))
	require.NotEqual(t, firstFingerprint, UpstreamKeyFingerprint(string(firstPublic)))
	require.Equal(t, []string{UnverifiedUpstreamAgentCredential}, UpstreamCredentialFingerprints(request))
	request = request.WithContext(WithUpstreamAgentPublicKey(request.Context(), firstPublic))
	require.Equal(t, []string{firstFingerprint}, UpstreamCredentialFingerprints(request))
	request.Header.Set("Authorization", upstreamAgentFixtureAssertion(t, first, "runtime-b", "task-b", "2026-01-01T00:00:01Z"))
	require.Equal(t, []string{firstFingerprint}, UpstreamCredentialFingerprints(request), "same signing key survives runtime, task and timestamp changes")
	request.Header.Set("Authorization", upstreamAgentFixtureAssertion(t, second, "runtime-a", "task-a", "2026-01-01T00:00:00Z"))
	require.Equal(t, []string{UnverifiedUpstreamAgentCredential}, UpstreamCredentialFingerprints(request), "a different signing key cannot reuse the first key's proof")
	request = request.WithContext(WithUpstreamAgentPublicKey(request.Context(), secondPublic))
	require.Equal(t, []string{UpstreamAgentKeyFingerprint(secondPublic)}, UpstreamCredentialFingerprints(request))

	copyPublic := append(ed25519.PublicKey(nil), firstPublic...)
	ctx := WithUpstreamAgentPublicKey(context.Background(), copyPublic)
	copyPublic[0] ^= 0xff
	request = request.WithContext(ctx)
	request.Header.Set("Authorization", assertion)
	require.Equal(t, []string{firstFingerprint}, UpstreamCredentialFingerprints(request), "a caller cannot mutate a bound public key")
}

func TestUpstreamAgentCredentialRejectsAmbiguousOrAlteredAssertions(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{3}, ed25519.SeedSize))
	publicKey, ok := key.Public().(ed25519.PublicKey)
	require.True(t, ok)
	valid := upstreamAgentFixtureAssertion(t, key, "runtime", "task", "2026-01-01T00:00:00Z")
	encoded := strings.TrimPrefix(valid, "AgentAssertion ")
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	require.NoError(t, err)
	duplicate := strings.Replace(string(raw), `"agent_runtime_id":"runtime"`, `"agent_runtime_id":"runtime","agent_runtime_id":"other"`, 1)
	require.True(t, string(raw) != duplicate, "fixture must contain a duplicate field")
	for _, assertion := range []string{
		"AgentAssertion", "AgentAssertion invalid", "AgentAssertion " + strings.Repeat("A", 9000),
		"AgentAssertion " + base64.RawURLEncoding.EncodeToString([]byte(duplicate)),
		"AgentAssertion " + base64.RawURLEncoding.EncodeToString(bytes.Replace(raw, []byte(`"task"`), []byte(`"changed"`), 1)),
		valid + " extra",
	} {
		request := httptest.NewRequest(http.MethodPost, "https://fixture.invalid/v1/responses", nil)
		request.Header.Set("Authorization", assertion)
		request = request.WithContext(WithUpstreamAgentPublicKey(request.Context(), publicKey))
		require.Equal(t, []string{UnverifiedUpstreamAgentCredential}, UpstreamCredentialFingerprints(request))
	}
}
