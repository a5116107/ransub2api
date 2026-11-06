package config

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// This non-fingerprint is rejected by the shared authority when an Agent
// assertion cannot be tied to the actual signing key. It is never persisted.
const UnverifiedUpstreamAgentCredential = "!unverified-openai-agent!"

type upstreamAgentPublicKeyContextKey struct{}

// WithUpstreamAgentPublicKey carries only a public key, scoped to this request.
// The final assertion is verified against it before its key identity is used.
func WithUpstreamAgentPublicKey(ctx context.Context, publicKey ed25519.PublicKey) context.Context {
	if len(publicKey) != ed25519.PublicKeySize {
		return ctx
	}
	return context.WithValue(ctx, upstreamAgentPublicKeyContextKey{}, append(ed25519.PublicKey(nil), publicKey...))
}

// UpstreamAgentKeyFingerprint identifies the signing key across runtime IDs,
// task changes and renewed assertions without storing private key material.
func UpstreamAgentKeyFingerprint(publicKey ed25519.PublicKey) string {
	if len(publicKey) != ed25519.PublicKeySize {
		return ""
	}
	data := append([]byte("\x00openai-agent-ed25519\x00"), publicKey...)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func upstreamAgentAssertionFingerprint(ctx context.Context, encoded string) string {
	publicKey, ok := ctx.Value(upstreamAgentPublicKeyContextKey{}).(ed25519.PublicKey)
	if !ok || len(publicKey) != ed25519.PublicKeySize || len(encoded) > 8192 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return ""
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return ""
	}
	fields := make(map[string]string, 4)
	for decoder.More() {
		nameToken, tokenErr := decoder.Token()
		name, valid := nameToken.(string)
		if tokenErr != nil || !valid {
			return ""
		}
		switch name {
		case "agent_runtime_id", "task_id", "timestamp", "signature":
		default:
			return ""
		}
		if _, duplicate := fields[name]; duplicate {
			return ""
		}
		var value string
		if decoder.Decode(&value) != nil || value == "" {
			return ""
		}
		fields[name] = value
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') || len(fields) != 4 {
		return ""
	}
	if _, err = decoder.Token(); err != io.EOF {
		return ""
	}
	signature, err := base64.StdEncoding.DecodeString(fields["signature"])
	if err != nil || !ed25519.Verify(publicKey, []byte(fields["agent_runtime_id"]+":"+fields["task_id"]+":"+fields["timestamp"]), signature) {
		return ""
	}
	return UpstreamAgentKeyFingerprint(publicKey)
}

// UpstreamKeyConcurrencyConfig declares one ceiling per actual supplier key.
// Revision must advance when changing Limits, including when removing a rule.
// Empty Limits with a positive Revision is a versioned removal of all limits.
type UpstreamKeyConcurrencyConfig struct {
	Revision int64          `mapstructure:"revision"`
	Limits   map[string]int `mapstructure:"limits"`
}

// EncodeLimits supplies the same canonical policy identity to both authorities.
func (c UpstreamKeyConcurrencyConfig) EncodeLimits() ([]byte, string, error) {
	limits := c.Limits
	if limits == nil {
		limits = map[string]int{}
	}
	raw, err := json.Marshal(limits)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(raw)
	return raw, hex.EncodeToString(digest[:]), nil
}

func (c UpstreamKeyConcurrencyConfig) Configured() bool { return c.Revision != 0 || len(c.Limits) != 0 }

func (c UpstreamKeyConcurrencyConfig) Validate() error {
	if !c.Configured() {
		return nil
	}
	if c.Revision <= 0 || c.Revision > 9007199254740991 {
		return fmt.Errorf("gateway.upstream_key_concurrency.revision must be a positive exact Redis Lua integer")
	}
	for fingerprint, limit := range c.Limits {
		decoded, err := hex.DecodeString(fingerprint)
		if err != nil || len(decoded) != sha256.Size || strings.ToLower(fingerprint) != fingerprint {
			return fmt.Errorf("gateway.upstream_key_concurrency requires lowercase SHA256 fingerprints")
		}
		if limit <= 0 || limit > 1000000 {
			return fmt.Errorf("gateway.upstream_key_concurrency limits must be between 1 and 1000000")
		}
	}
	return nil
}

// UpstreamKeyFingerprint intentionally excludes account, product, protocol and
// endpoint: using the identical credential through multiple local accounts must
// never multiply capacity. It makes no claim about shared prompt caches.
func UpstreamKeyFingerprint(key string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(key)))
	return hex.EncodeToString(sum[:])
}

// UpstreamAWSAccessKeyFingerprint identifies a SigV4 access-key ID, not its
// per-request signature or secret. Its namespace prevents an unrelated Bearer
// token with the same text from consuming this AWS credential's capacity.
func UpstreamAWSAccessKeyFingerprint(accessKeyID string) string {
	// NUL cannot occur in an HTTP header value, so this domain cannot collide
	// with any valid Bearer or API-Key header value.
	return UpstreamKeyFingerprint("\x00aws4-access-key-id\x00" + strings.TrimSpace(accessKeyID))
}

// sigV4AccessKeyID reads the stable ID from the actual AWS4 authorization
// header. It does not verify signatures or treat a role/session as the same ID.
func sigV4AccessKeyID(value string) string {
	scheme, fields, ok := strings.Cut(strings.TrimSpace(value), " ")
	if !ok || !strings.EqualFold(scheme, "AWS4-HMAC-SHA256") {
		return ""
	}
	var credential string
	credentialFound := false
	for _, field := range strings.Split(fields, ",") {
		name, candidate, found := strings.Cut(strings.TrimSpace(field), "=")
		if !found || name != "Credential" {
			continue
		}
		if credentialFound {
			return ""
		}
		credentialFound = true
		credential = candidate
	}
	parts := strings.Split(credential, "/")
	if len(parts) != 5 || parts[0] == "" || len(parts[0]) > 128 || len(parts[1]) != 8 ||
		parts[2] == "" || parts[3] == "" || parts[4] != "aws4_request" {
		return ""
	}
	for _, c := range parts[0] {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return ""
		}
	}
	for _, c := range parts[1] {
		if c < '0' || c > '9' {
			return ""
		}
	}
	for _, scope := range parts[2:4] {
		if strings.ContainsAny(scope, " \t\r\n") {
			return ""
		}
	}
	return parts[0]
}

// UpstreamCredentialFingerprints identifies final wire credentials without
// retaining plaintext. HTTP and pooled WS use the same extraction rules.
func UpstreamCredentialFingerprints(req *http.Request) []string {
	if req == nil {
		return nil
	}
	seen := map[string]bool{}
	var result []string
	addFingerprint := func(fingerprint string) {
		if !seen[fingerprint] {
			seen[fingerprint] = true
			result = append(result, fingerprint)
		}
	}
	add := func(key string) {
		if strings.TrimSpace(key) == "" {
			return
		}
		addFingerprint(UpstreamKeyFingerprint(key))
	}
	// Some upstream builders deliberately preserve wire casing by inserting
	// keys directly into http.Header. Header.Values canonicalizes its lookup
	// and silently misses those keys, although net/http still sends them.
	// Scan every casing variant deterministically without changing wire bytes.
	for _, wanted := range []string{"Authorization", "X-Api-Key", "Api-Key", "X-Goog-Api-Key"} {
		var names []string
		for name := range req.Header {
			if strings.EqualFold(name, wanted) {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		for _, name := range names {
			for _, value := range req.Header[name] {
				if wanted == "Authorization" {
					fields := strings.Fields(value)
					if len(fields) > 0 && strings.EqualFold(fields[0], "AgentAssertion") {
						fingerprint := ""
						if len(fields) == 2 {
							fingerprint = upstreamAgentAssertionFingerprint(req.Context(), fields[1])
						}
						if fingerprint == "" {
							fingerprint = UnverifiedUpstreamAgentCredential
						}
						addFingerprint(fingerprint)
						continue
					}
					scheme, token, ok := strings.Cut(strings.TrimSpace(value), " ")
					if ok && (strings.EqualFold(scheme, "Bearer") || strings.EqualFold(scheme, "Token")) {
						add(token)
					} else if keyID := sigV4AccessKeyID(value); keyID != "" {
						// Add uses the same hashing and de-duplication path as other
						// credentials, with a distinct AWS identity namespace.
						addFingerprint(UpstreamAWSAccessKeyFingerprint(keyID))
					}
				} else {
					add(value)
				}
			}
		}
	}
	if req.URL != nil {
		for _, name := range []string{"key", "api_key"} {
			for _, value := range req.URL.Query()[name] {
				add(value)
			}
		}
	}
	return result
}
