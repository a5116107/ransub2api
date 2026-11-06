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
