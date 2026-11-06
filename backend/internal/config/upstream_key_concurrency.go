package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

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
