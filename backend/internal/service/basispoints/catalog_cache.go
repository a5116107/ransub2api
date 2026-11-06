package basispoints

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

var ErrRouteOriginMismatch = errors.New("basispoints state belongs to a different upstream route")

// RouteOrigin stores only digests. An empty principal means that an OAuth
// identity could not be established and is not proof of a different owner.
type RouteOrigin struct {
	Route     string `json:"route"`
	Account   string `json:"account,omitempty"`
	Principal string `json:"principal,omitempty"`
}

func fileOriginKey(id string) string {
	digest := sha256.Sum256([]byte(id))
	return "\x00file-origin:v1:" + hex.EncodeToString(digest[:])
}

func validRouteOrigin(origin RouteOrigin) bool {
	if len(origin.Route) != 64 || (origin.Account != "" && len(origin.Account) != 64) || (origin.Principal != "" && len(origin.Principal) != 64) {
		return false
	}
	_, routeErr := hex.DecodeString(origin.Route)
	if routeErr != nil {
		return false
	}
	for _, digest := range []string{origin.Account, origin.Principal} {
		if digest != "" {
			if _, err := hex.DecodeString(digest); err != nil {
				return false
			}
		}
	}
	return true
}

func sameRouteOrigin(left, right RouteOrigin) bool {
	return left.Route == right.Route &&
		(left.Account == "" || right.Account == "" || left.Account == right.Account) &&
		(left.Principal == "" || right.Principal == "" || left.Principal == right.Principal)
}

// CheckFileOrigin preserves the existing unknown-reference behavior. Storage
// failure is distinct from a missing record and never becomes an unknown pass.
func (c *CatalogCache) CheckFileOrigin(id string, origin RouteOrigin) (bool, error) {
	if !ValidAttachmentID(id) {
		return false, ErrStateUnavailable
	}
	return c.checkOrigin(fileOriginKey(id), origin)
}

func encryptedOriginKey(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "\x00encrypted-origin:v1:" + hex.EncodeToString(digest[:])
}

// CheckEncryptedOrigin treats absent or expired evidence as unknown. The value
// is only hashed and is never persisted as plaintext.
func (c *CatalogCache) CheckEncryptedOrigin(value string, origin RouteOrigin) (bool, error) {
	if value == "" {
		return false, ErrStateUnavailable
	}
	return c.checkOrigin(encryptedOriginKey(value), origin)
}

func (c *CatalogCache) checkOrigin(key string, origin RouteOrigin) (bool, error) {
	if c == nil || !validRouteOrigin(origin) {
		return false, ErrStateUnavailable
	}
	raw, _ := c.snapshot(key)
	if err := c.Err(); err != nil {
		return false, err
	}
	if len(raw) == 0 {
		return false, nil
	}
	var stored RouteOrigin
	if json.Unmarshal(raw, &stored) != nil || !validRouteOrigin(stored) {
		return false, ErrStateUnavailable
	}
	if !sameRouteOrigin(stored, origin) {
		return true, ErrRouteOriginMismatch
	}
	return true, nil
}

// RememberFileOrigin claims only IDs returned by an actual successful upload.
// CAS retains the first live observation under concurrent provider responses.
func (c *CatalogCache) RememberFileOrigin(id string, origin RouteOrigin) error {
	if !ValidAttachmentID(id) {
		return ErrStateUnavailable
	}
	return c.rememberOrigin(fileOriginKey(id), origin)
}

// RememberEncryptedOrigin claims only state produced by the upstream. The
// same bounded CAS store and first-owner rule are shared with file origins.
func (c *CatalogCache) RememberEncryptedOrigin(value string, origin RouteOrigin) error {
	if value == "" {
		return ErrStateUnavailable
	}
	return c.rememberOrigin(encryptedOriginKey(value), origin)
}

func (c *CatalogCache) rememberOrigin(key string, origin RouteOrigin) error {
	if c == nil || !validRouteOrigin(origin) {
		return ErrStateUnavailable
	}
	for range 8 {
		raw, version := c.snapshot(key)
		if err := c.Err(); err != nil {
			return err
		}
		if len(raw) != 0 {
			var previous RouteOrigin
			if json.Unmarshal(raw, &previous) != nil || !validRouteOrigin(previous) {
				return ErrStateUnavailable
			}
			if !sameRouteOrigin(previous, origin) {
				return ErrRouteOriginMismatch
			}
			if (previous.Account != "" || origin.Account == "") && (previous.Principal != "" || origin.Principal == "") {
				return nil
			}
			if previous.Account == "" {
				previous.Account = origin.Account
			}
			if previous.Principal == "" {
				previous.Principal = origin.Principal
			}
			origin = previous
		}
		encoded, err := json.Marshal(origin)
		if err != nil {
			return ErrStateUnavailable
		}
		if c.commit(key, version, encoded) {
			return nil
		}
		if err := c.Err(); err != nil {
			return err
		}
	}
	return ErrStateUnavailable
}

// CatalogCache holds immutable, versioned client declarations and bounded
// route-origin digests, not responses, raw file IDs, or encrypted states.
// Catalog callers require an account/key/trusted-session scope; origins use hashed keys.
// Explicit tools replace the catalog (including []); omitted tools inherit it.
// Additional tools merge through Prepare's duplicate/schema validation.
type CatalogCache struct {
	ctx      context.Context
	store    StateStore
	storeErr error
	mu       sync.Mutex
	entries  map[string]*list.Element
	order    list.List
	bytes    int
	version  uint64
	now      func() time.Time
}
type catalogEntry struct {
	scope   string
	raw     []byte
	version uint64
	touched time.Time
}

func (c *CatalogCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}
func (c *CatalogCache) remove(e *list.Element) {
	v, _ := e.Value.(catalogEntry)
	delete(c.entries, v.scope)
	c.bytes -= len(v.raw)
	c.order.Remove(e)
}
func (c *CatalogCache) prune(now time.Time) {
	for e := c.order.Front(); e != nil; e = c.order.Front() {
		entry, _ := e.Value.(catalogEntry)
		if now.Sub(entry.touched) < replayCacheIdleTTL {
			break
		}
		c.remove(e)
	}
}
func (c *CatalogCache) snapshot(scope string) ([]byte, uint64) {
	if c.store != nil {
		raw, version, err := c.store.LoadBPSState(c.ctx, scope, "catalog")
		c.stateError(err)
		return raw, version
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	c.prune(now)
	e := c.entries[scope]
	if e == nil {
		return nil, 0
	}
	v, _ := e.Value.(catalogEntry)
	v.touched = now
	e.Value = v
	c.order.MoveToBack(e)
	return v.raw, v.version
}
func (c *CatalogCache) commit(scope string, expected uint64, raw []byte) bool {
	if c.store != nil {
		changed, err := c.store.SaveBPSState(c.ctx, scope, "catalog", &expected, raw)
		c.stateError(err)
		return changed
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	c.prune(now)
	current := uint64(0)
	old := c.entries[scope]
	if old != nil {
		entry, _ := old.Value.(catalogEntry)
		current = entry.version
	}
	if current != expected {
		return false
	}
	if c.entries == nil {
		c.entries = make(map[string]*list.Element)
	}
	if old != nil {
		c.remove(old)
	}
	c.version++
	c.entries[scope] = c.order.PushBack(catalogEntry{scope: scope, raw: raw, version: c.version, touched: now})
	c.bytes += len(raw)
	for len(c.entries) > 512 || c.bytes > 16<<20 {
		c.remove(c.order.Front())
	}
	return true
}

func PrepareWithCatalog(raw []byte, scope string, replay *ReplayCache, cache *CatalogCache) ([]byte, *Bridge, error) {
	return prepareWithCatalog(raw, scope, replay, cache, nil)
}

func prepareWithCatalog(raw []byte, scope string, replay *ReplayCache, cache *CatalogCache, nativeToolImages map[string]bool) ([]byte, *Bridge, error) {
	if cache == nil || scope == "" {
		return prepare(raw, scope, replay, nativeToolImages)
	}
	var source object
	if decode(raw, &source) != nil || source == nil {
		return nil, nil, fmt.Errorf("invalid Basispoints request JSON")
	}
	if text(source["tool_choice"]) == "none" {
		return prepare(raw, scope, replay, nativeToolImages)
	}
	_, explicit := source["tools"]
	for attempt := 0; attempt < 8; attempt++ {
		previous, version := cache.snapshot(scope)
		if err := cache.Err(); err != nil {
			return nil, nil, err
		}
		var candidate object
		if err := decode(raw, &candidate); err != nil {
			return nil, nil, err
		}
		if !explicit && previous != nil {
			var inherited []any
			if err := decode(previous, &inherited); err != nil {
				return nil, nil, err
			}
			candidate["tools"] = inherited
		}
		encoded, err := json.Marshal(candidate)
		if err != nil {
			return nil, nil, err
		}
		body, b, err := prepareWithInheritedCatalog(encoded, scope, replay, nativeToolImages, !explicit && previous != nil)
		if err != nil {
			return nil, nil, err
		}
		keys := make([]string, 0, len(b.tools))
		for key := range b.tools {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		declarations := make([]any, 0, len(keys))
		for _, key := range keys {
			info := b.tools[key]
			var declaration any = info.Catalog
			if info.Namespace != "" {
				declaration = object{"type": "namespace", "name": info.Namespace, "tools": []any{declaration}}
			}
			declarations = append(declarations, declaration)
		}
		saved, err := json.Marshal(declarations)
		if err != nil {
			return nil, nil, err
		}
		if len(saved) > 1<<20 {
			return nil, nil, fmt.Errorf("basispoints session tool catalog exceeds 1 MiB")
		}
		if cache.commit(scope, version, saved) {
			return body, b, nil
		}
		if err := cache.Err(); err != nil {
			return nil, nil, err
		}
		// A concurrent explicit replacement is authoritative. This request keeps
		// its own validated catalog, but cannot overwrite the newer snapshot.
		if explicit {
			return body, b, nil
		}
		// Concurrent incremental declarations merge against a fresh snapshot.
	}
	return nil, nil, fmt.Errorf("basispoints tool catalog changed concurrently; retry request")
}

// Reprepare applies uploaded attachment references using this request's validated
// catalog. It neither reads nor replaces a newer shared session catalog.
func (b *Bridge) Reprepare(raw []byte) ([]byte, *Bridge, error) {
	var source object
	if err := decode(raw, &source); err != nil || source == nil {
		return nil, nil, fmt.Errorf("invalid Basispoints request JSON")
	}
	if _, explicit := source["tools"]; !explicit {
		keys := make([]string, 0, len(b.tools))
		for key := range b.tools {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		declarations := make([]any, 0, len(keys))
		for _, key := range keys {
			info := b.tools[key]
			var declaration any = info.Catalog
			if info.Namespace != "" {
				declaration = object{"type": "namespace", "name": info.Namespace, "tools": []any{declaration}}
			}
			declarations = append(declarations, declaration)
		}
		source["tools"] = declarations
	}
	encoded, err := json.Marshal(source)
	if err != nil {
		return nil, nil, err
	}
	return prepare(encoded, b.scope, b.replay, b.nativeToolImages)
}
