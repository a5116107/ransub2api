package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewBedrockSignerFromAccount_DefaultRegion(t *testing.T) {
	account := &Account{
		Platform: PlatformAnthropic,
		Type:     AccountTypeBedrock,
		Credentials: map[string]any{
			"aws_access_key_id":     "test-akid",
			"aws_secret_access_key": "test-secret",
		},
	}

	signer, err := NewBedrockSignerFromAccount(account)
	require.NoError(t, err)
	require.NotNil(t, signer)
	assert.Equal(t, defaultBedrockRegion, signer.region)
}

func TestFilterBetaTokens(t *testing.T) {
	tokens := []string{"interleaved-thinking-2025-05-14", "tool-search-tool-2025-10-19"}
	filterSet := map[string]struct{}{
		"tool-search-tool-2025-10-19": {},
	}

	assert.Equal(t, []string{"interleaved-thinking-2025-05-14"}, filterBetaTokens(tokens, filterSet))
	assert.Equal(t, tokens, filterBetaTokens(tokens, nil))
	assert.Nil(t, filterBetaTokens(nil, filterSet))
}

// The production builder signs with AWS SDK SigV4; the slot identity must be
// stable even when signature, region, or temporary session token changes.
func TestBedrockSignedRequestUsesStableSharedKeyIdentity(t *testing.T) {
	const keyID = "AKIDEXAMPLE12345"
	var previous string
	for _, tc := range []struct{ region, secret, session string }{
		{"us-east-1", "fixture-secret-one", "fixture-session-one"},
		{"eu-west-1", "fixture-secret-two", "fixture-session-two"},
	} {
		signer := NewBedrockSigner(keyID, tc.secret, tc.session, tc.region)
		req, err := (&GatewayService{}).buildUpstreamRequestBedrock(t.Context(), []byte(`{}`), "anthropic.claude-v2", tc.region, false, signer)
		require.NoError(t, err)
		require.Contains(t, req.Header.Get("Authorization"), "AWS4-HMAC-SHA256")
		fingerprints := config.UpstreamCredentialFingerprints(req)
		require.Equal(t, []string{config.UpstreamAWSAccessKeyFingerprint(keyID)}, fingerprints)
		if previous != "" {
			require.Equal(t, previous, fingerprints[0])
		}
		previous = fingerprints[0]
		require.NotEqual(t, config.UpstreamKeyFingerprint(keyID), fingerprints[0], "an AWS access-key ID is not a Bearer API key")
	}
}
