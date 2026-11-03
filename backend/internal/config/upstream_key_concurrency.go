package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
)

// UpstreamKeyConcurrencyConfig declares one ceiling per actual supplier key.
// Revision must advance when changing Limits, including when removing a rule.
// Empty Limits with a positive Revision is a versioned removal of all limits.
type UpstreamKeyConcurrencyConfig struct {
	Revision int64          `mapstructure:"revision"`
	Limits   map[string]int `mapstructure:"limits"`
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

// UpstreamCredentialFingerprints identifies final wire credentials without
// retaining plaintext. HTTP and pooled WS use the same extraction rules.
func UpstreamCredentialFingerprints(req *http.Request) []string {
	if req == nil {
		return nil
	}
	seen := map[string]bool{}
	var result []string
	add := func(key string) {
		if strings.TrimSpace(key) == "" {
			return
		}
		fingerprint := UpstreamKeyFingerprint(key)
		if !seen[fingerprint] {
			seen[fingerprint] = true
			result = append(result, fingerprint)
		}
	}
	for _, value := range req.Header.Values("Authorization") {
		scheme, token, ok := strings.Cut(strings.TrimSpace(value), " ")
		if ok && (strings.EqualFold(scheme, "Bearer") || strings.EqualFold(scheme, "Token")) {
			add(token)
		}
	}
	for _, name := range []string{"X-Api-Key", "Api-Key", "X-Goog-Api-Key"} {
		for _, value := range req.Header.Values(name) {
			add(value)
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
