package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const codexTurnStateOriginPrefix = "openai:codex-turn-origin:"

var _ service.OpenAICodexTurnStateStore = (*gatewayCache)(nil)

var errCodexTurnStateOriginInvalid = errors.New("invalid Codex turn-state provenance")

type codexTurnStateOriginRecord struct {
	Version   int                               `json:"version"`
	Scope     service.OpenAIWSContinuationScope `json:"scope"`
	ScopeHash string                            `json:"scope_hash"`
}

// One opaque state may be reused by a provider. Preserve its live first
// observation instead of authorizing it under a different route's scope.
var setCodexTurnStateOrigin = redis.NewScript(`
local previous = redis.call('GET', KEYS[1])
if previous then
  local ok, record = pcall(cjson.decode, previous)
  if not ok or type(record) ~= 'table' or record.version ~= 1
    or redis.call('PTTL', KEYS[1]) <= 0 then return 0 end
  if record.scope_hash ~= ARGV[2] then return 0 end
end
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[3])
return 1
`)

// Read the value and its bounded lifetime atomically. Instance wall clocks
// must not expire or reassign a provenance record that Redis still owns.
var getCodexTurnStateOrigin = redis.NewScript(`
local record = redis.call('GET', KEYS[1])
if not record then return nil end
if redis.call('PTTL', KEYS[1]) <= 0 then
  return redis.error_reply('invalid Codex turn-state provenance lifetime')
end
return record
`)

func codexTurnStateScopeHash(scope service.OpenAIWSContinuationScope) string {
	// Scope contains only primitive fields; its JSON encoding cannot fail.
	data, _ := json.Marshal(scope)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func validCodexTurnStateOriginKey(key string) bool {
	if len(key) != 64 {
		return false
	}
	for _, ch := range key {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

func (c *gatewayCache) SetOpenAICodexTurnStateOrigin(ctx context.Context, key string, scope service.OpenAIWSContinuationScope, ttl time.Duration) error {
	if !validCodexTurnStateOriginKey(key) || scope.AccountID <= 0 || scope.RouteFingerprint == [32]byte{} || scope.Model == "" || ttl < time.Millisecond {
		return errCodexTurnStateOriginInvalid
	}
	scopeHash := codexTurnStateScopeHash(scope)
	data, err := json.Marshal(codexTurnStateOriginRecord{Version: 1, Scope: scope, ScopeHash: scopeHash})
	if err != nil {
		return err
	}
	stored, err := setCodexTurnStateOrigin.Run(ctx, c.rdb, []string{codexTurnStateOriginPrefix + key}, data, scopeHash, ttl.Milliseconds()).Int()
	if err != nil {
		return err
	}
	if stored != 1 {
		return errCodexTurnStateOriginInvalid
	}
	return nil
}

func (c *gatewayCache) GetOpenAICodexTurnStateOrigin(ctx context.Context, key string) (service.OpenAIWSContinuationScope, bool, error) {
	if !validCodexTurnStateOriginKey(key) {
		return service.OpenAIWSContinuationScope{}, false, errCodexTurnStateOriginInvalid
	}
	data, err := getCodexTurnStateOrigin.Run(ctx, c.rdb, []string{codexTurnStateOriginPrefix + key}).Text()
	if errors.Is(err, redis.Nil) {
		return service.OpenAIWSContinuationScope{}, false, nil
	}
	if err != nil {
		return service.OpenAIWSContinuationScope{}, false, err
	}
	var record codexTurnStateOriginRecord
	if err := json.Unmarshal([]byte(data), &record); err != nil {
		return service.OpenAIWSContinuationScope{}, false, errCodexTurnStateOriginInvalid
	}
	if record.Version != 1 || record.Scope.AccountID <= 0 || record.Scope.RouteFingerprint == [32]byte{} || record.Scope.Model == "" || record.ScopeHash != codexTurnStateScopeHash(record.Scope) {
		return service.OpenAIWSContinuationScope{}, false, errCodexTurnStateOriginInvalid
	}
	return record.Scope, true, nil
}
