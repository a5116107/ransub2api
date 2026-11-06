package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

// Keep the existing record key: old readers continue to consume the two integer
// keys, while new readers consume one versioned record. No raw response ID,
// request body, upstream token or node address is stored in a Redis key.
func httpResponseOwnerKeys(groupID int64, responseID string) []string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(responseID)))
	id := hex.EncodeToString(sum[:])
	return []string{
		buildSessionKey(groupID, "openai:http-response-owner:v1:"+id),
		buildSessionKey(groupID, "openai:http-response-owner:user:"+id),
		buildSessionKey(groupID, "openai:http-response-owner:key:"+id),
	}
}

var errHTTPResponseOwnerUnavailable = errors.New("HTTP response owner unavailable")

type httpResponseOwnerRecord struct {
	Version   int                               `json:"version"`
	User      string                            `json:"user"`
	Key       string                            `json:"key"`
	Scope     service.OpenAIWSContinuationScope `json:"scope"`
	ScopeHash string                            `json:"scope_hash"`
}

var setHTTPResponseOwnerScript = redis.NewScript(`
local value, user, key = ARGV[1], ARGV[2], ARGV[3]
local previous = redis.call('GET', KEYS[1])
local legacy = redis.call('MGET', KEYS[2], KEYS[3])
if previous then
    local ok, record = pcall(cjson.decode, previous)
    if not ok or type(record) ~= 'table' or record.version ~= 2
        or record.user ~= user or record.scope_hash ~= ARGV[5]
        or record.user ~= legacy[1] or record.key ~= legacy[2]
        or redis.call('PTTL', KEYS[1]) <= 0 then return 0 end
    value, user, key = previous, record.user, record.key
elseif legacy[1] ~= false or legacy[2] ~= false then
    return 0
end
redis.call('SET', KEYS[1], value, 'PX', ARGV[4])
redis.call('SET', KEYS[2], user, 'PX', ARGV[4])
redis.call('SET', KEYS[3], key, 'PX', ARGV[4])
return 1
`)

var getHTTPResponseOwnerScript = redis.NewScript(`
local record = redis.call('GET', KEYS[1])
local legacy = redis.call('MGET', KEYS[2], KEYS[3])
if record ~= false then
    if legacy[1] == false or legacy[2] == false or redis.call('PTTL', KEYS[1]) <= 0 then
        return {'inconsistent'}
    end
    if record ~= 'v1:' .. legacy[1] .. ':' .. legacy[2] then
        local ok, decoded = pcall(cjson.decode, record)
        if not ok or type(decoded) ~= 'table' or decoded.version ~= 2
            or decoded.user ~= legacy[1] or decoded.key ~= legacy[2] then return {'inconsistent'} end
    end
    return {record}
end
if legacy[1] == false or legacy[2] == false then return {} end
return {'legacy', legacy[1], legacy[2]}
`)

func (c *gatewayCache) SetHTTPResponseOwner(ctx context.Context, groupID int64, responseID string, owner service.OpenAIHTTPResponseOwner, ttl time.Duration) error {
	if c == nil || c.rdb == nil || groupID < 0 || strings.TrimSpace(responseID) == "" || !owner.Valid() || ttl < time.Millisecond {
		return errHTTPResponseOwnerUnavailable
	}
	user, key := strconv.FormatInt(owner.UserID, 10), strconv.FormatInt(owner.APIKeyID, 10)
	record := httpResponseOwnerRecord{Version: 2, User: user, Key: key, Scope: owner.Scope, ScopeHash: codexTurnStateScopeHash(owner.Scope)}
	data, err := json.Marshal(record)
	if err != nil {
		return errHTTPResponseOwnerUnavailable
	}
	// One atomic publication also preserves the old reader's exact wire format.
	stored, err := setHTTPResponseOwnerScript.Run(ctx, c.rdb, httpResponseOwnerKeys(groupID, responseID), data, user, key, ttl.Milliseconds(), record.ScopeHash).Int()
	if err != nil || stored != 1 {
		return errHTTPResponseOwnerUnavailable
	}
	return nil
}

func (c *gatewayCache) GetHTTPResponseOwner(ctx context.Context, groupID int64, responseID string) (service.OpenAIHTTPResponseOwner, bool, error) {
	if c == nil || c.rdb == nil || groupID < 0 || strings.TrimSpace(responseID) == "" {
		return service.OpenAIHTTPResponseOwner{}, false, errHTTPResponseOwnerUnavailable
	}
	values, err := getHTTPResponseOwnerScript.Run(ctx, c.rdb, httpResponseOwnerKeys(groupID, responseID)).StringSlice()
	if err != nil {
		// Do not leak Redis topology or fall back to stale process-local grants.
		return service.OpenAIHTTPResponseOwner{}, false, errHTTPResponseOwnerUnavailable
	}
	if len(values) == 0 {
		return service.OpenAIHTTPResponseOwner{}, false, nil
	}
	var parts []string
	if len(values) == 1 {
		if strings.HasPrefix(values[0], "{") {
			var record httpResponseOwnerRecord
			if err := json.Unmarshal([]byte(values[0]), &record); err != nil || record.Version != 2 || record.ScopeHash != codexTurnStateScopeHash(record.Scope) {
				return service.OpenAIHTTPResponseOwner{}, false, errHTTPResponseOwnerUnavailable
			}
			user, userErr := strconv.ParseInt(record.User, 10, 64)
			key, keyErr := strconv.ParseInt(record.Key, 10, 64)
			owner := service.OpenAIHTTPResponseOwner{UserID: user, APIKeyID: key, Scope: record.Scope}
			if userErr != nil || keyErr != nil || !owner.Valid() {
				return service.OpenAIHTTPResponseOwner{}, false, errHTTPResponseOwnerUnavailable
			}
			return owner, true, nil
		}
		parts = strings.Split(values[0], ":")
		if len(parts) != 3 || parts[0] != "v1" {
			return service.OpenAIHTTPResponseOwner{}, false, errHTTPResponseOwnerUnavailable
		}
	} else if len(values) == 3 && values[0] == "legacy" {
		parts = values
	} else {
		return service.OpenAIHTTPResponseOwner{}, false, errHTTPResponseOwnerUnavailable
	}
	user, userErr := strconv.ParseInt(parts[1], 10, 64)
	key, keyErr := strconv.ParseInt(parts[2], 10, 64)
	if userErr != nil || keyErr != nil || user <= 0 || key <= 0 {
		return service.OpenAIHTTPResponseOwner{}, false, errHTTPResponseOwnerUnavailable
	}
	// Legacy records prove only a tenant; their unknown scope is never backfilled.
	return service.OpenAIHTTPResponseOwner{UserID: user, APIKeyID: key}, true, nil
}

var _ service.OpenAIHTTPResponseOwnerCache = (*gatewayCache)(nil)
