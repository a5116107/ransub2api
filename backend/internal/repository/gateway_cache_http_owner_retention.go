package repository

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

// Validate all three representations before changing any deadline. Retention
// never restores a missing legacy mirror, upgrades a legacy-only record, or
// overwrites a user/key changed since the caller's ownership read.
var refreshHTTPResponseOwnerScript = redis.NewScript(`
local current = redis.call('MGET', KEYS[1], KEYS[2], KEYS[3])
for i = 1, 3 do
  if current[i] ~= ARGV[i] then return 0 end
end
for i = 1, 3 do
  local remaining = redis.call('PTTL', KEYS[i])
  if remaining >= 0 and remaining < tonumber(ARGV[4]) then
    redis.call('PEXPIRE', KEYS[i], ARGV[4])
  end
end
return 1
`)

func (c *gatewayCache) RefreshHTTPResponseOwner(ctx context.Context, groupID int64, responseID string, owner service.OpenAIResponseOwner, ttl time.Duration) (bool, error) {
	if c == nil || c.rdb == nil || groupID < 0 || strings.TrimSpace(responseID) == "" || !owner.Valid() || ttl.Milliseconds() <= 0 {
		return false, errResponseOwnerUnavailable
	}
	user, key := strconv.FormatInt(owner.UserID, 10), strconv.FormatInt(owner.APIKeyID, 10)
	record, err := json.Marshal(responseOwnerRecord{Version: 2, User: user, Key: key, Scope: owner.Scope, ScopeHash: codexTurnStateScopeHash(owner.Scope)})
	if err != nil {
		return false, errResponseOwnerUnavailable
	}
	// Compare the full creation scope, not just the tenant/key pair. A changed
	// account, route, model or tier must invalidate a stale retention read.
	result, err := refreshHTTPResponseOwnerScript.Run(ctx, c.rdb, responseOwnerKeys(groupID, responseID), record, user, key, ttl.Milliseconds()).Int()
	if err != nil {
		return false, errResponseOwnerUnavailable
	}
	return result == 1, nil
}

var _ service.OpenAIHTTPResponseOwnerRetentionCache = (*gatewayCache)(nil)
