package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/regional/video"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const (
	asyncVideoKeyLeasePrefix  = "concurrency:upstream_key:async_video:"
	asyncVideoReleaseClaimTTL = 2 * time.Minute
)

var bindAsyncVideoKeyLeaseScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[4]) ~= 0 then return 0 end
local member = redis.call('GET', KEYS[1])
if not member then return 0 end
if member ~= ARGV[3] then return -2 end
local current = cjson.decode(member)
if current['task_id'] and current['task_id'] ~= '' and current['task_id'] ~= ARGV[4] then return -1 end
local existing = redis.call('GET', KEYS[2])
if existing and cjson.decode(existing)['member'] ~= ARGV[1] then return -1 end
redis.call('SET', KEYS[1], ARGV[2])
redis.call('SET', KEYS[2], ARGV[2])
redis.call('ZREM', KEYS[3], ARGV[1])
return 1
`)

var deleteAsyncVideoKeyLeaseScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return 0 end
redis.call('DEL', KEYS[1])
if KEYS[2] ~= '' and redis.call('GET', KEYS[2]) == ARGV[1] then
  redis.call('DEL', KEYS[2])
end
redis.call('ZREM', KEYS[3], ARGV[2])
redis.call('DEL', KEYS[4])
redis.call('ZREM', KEYS[5], ARGV[2])
return 1
`)

var markAsyncVideoKeyUnknownScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[3]) ~= 0 then return 0 end
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return -2 end
redis.call('SET', KEYS[1], ARGV[2])
redis.call('ZADD', KEYS[2], ARGV[3], ARGV[4])
return 1
`)

var persistAsyncVideoKeyLeaseScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) ~= 0 or redis.call('EXISTS', KEYS[3]) ~= 0 then return 0 end
redis.call('SET', KEYS[1], ARGV[1])
redis.call('ZADD', KEYS[2], ARGV[2], ARGV[3])
return 1
`)

var fenceAsyncVideoKeyLeaseScript = redis.NewScript(`
redis.replicate_commands()
if ARGV[3] == '1' and redis.call('EXISTS', KEYS[1]) == 0 then return 0 end
if redis.call('GET', KEYS[2]) ~= ARGV[2] then return 0 end
if KEYS[4] ~= '' and redis.call('GET', KEYS[4]) ~= ARGV[2] then return 0 end
local clock = redis.call('TIME')
local now = tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000)
redis.call('SET', KEYS[1], '1')
redis.call('ZADD', KEYS[3], now, ARGV[1])
if not redis.call('ZSCORE', KEYS[5], ARGV[1]) then
  redis.call('ZADD', KEYS[5], now, ARGV[1])
end
return 1
`)

var claimAsyncVideoKeyReleaseScript = redis.NewScript(`
local score = redis.call('ZSCORE', KEYS[1], ARGV[1])
if not score or tonumber(score) > tonumber(ARGV[2]) then return 0 end
redis.call('ZADD', KEYS[1], ARGV[3], ARGV[1])
return 1
`)

var deferAsyncVideoKeyReleaseScript = redis.NewScript(`
redis.replicate_commands()
local score = redis.call('ZSCORE', KEYS[1], ARGV[1])
if not score or tonumber(score) ~= tonumber(ARGV[2]) then return 0 end
local clock = redis.call('TIME')
local now = tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000)
redis.call('ZADD', KEYS[1], now + tonumber(ARGV[3]), ARGV[1])
return 1
`)

type asyncVideoKeyLeaseRecord struct {
	TaskID    string                              `json:"task_id,omitempty"`
	Member    string                              `json:"member"`
	State     string                              `json:"state"`
	Lease     *service.PersistentUpstreamKeyLease `json:"lease"`
	Owner     service.AsyncVideoKeyOwner          `json:"owner"`
	CreatedAt int64                               `json:"created_at"`
	UpdatedAt int64                               `json:"updated_at"`
}

var _ service.AsyncVideoKeyLeaseController = (*httpUpstreamService)(nil)
var _ service.AsyncVideoKeyReleaseRecovery = (*httpUpstreamService)(nil)

const (
	asyncVideoLeaseStateProvisional = "provisional"
	asyncVideoLeaseStateUnknown     = "unknown"
	asyncVideoLeaseStateBound       = "bound"
)

func asyncVideoMemberKey(member string) string {
	return asyncVideoKeyLeasePrefix + "member:" + strings.TrimSpace(member)
}

func asyncVideoFenceKey(member string) string {
	return asyncVideoKeyLeasePrefix + "fence:" + strings.TrimSpace(member)
}

func asyncVideoUnknownKey() string { return asyncVideoKeyLeasePrefix + "unknown" }
func asyncVideoReleaseKey() string { return asyncVideoKeyLeasePrefix + "release" }

func (s *httpUpstreamService) asyncVideoLeaseStore() (*upstreamKeyLimiter, error) {
	if s == nil || s.upstreamKeyLimiter == nil || s.upstreamKeyLimiter.rdb == nil {
		return nil, service.ErrUpstreamKeyPolicy
	}
	// Regional ownership must stay in its SQL authority, not the Redis store.
	if s.upstreamKeyLimiter.regional != nil {
		return nil, service.ErrUpstreamKeyPolicy
	}
	return s.upstreamKeyLimiter, nil
}

func (s *httpUpstreamService) regionalAsyncVideoEnabled() bool {
	return s != nil && s.upstreamKeyLimiter != nil && s.upstreamKeyLimiter.regional != nil
}

func (s *httpUpstreamService) IsLocalAsyncVideoKeyReleaseRecovery() bool {
	store, err := s.regionalAsyncVideoStore()
	return err == nil && store.local != nil
}

func validateAsyncVideoHandoff(handoff *service.AsyncVideoKeyHandoff) (*service.PersistentUpstreamKeyLease, string, error) {
	if handoff == nil {
		return nil, "", service.ErrUpstreamKeyPolicy
	}
	lease := handoff.Lease()
	member := strings.TrimSpace(handoff.Member())
	if lease == nil || lease.Validate() != nil || member == "" || lease.Member != member || !strings.HasPrefix(member, "video:") || !handoff.Owner().Valid() {
		return nil, "", service.ErrUpstreamKeyPolicy
	}
	return lease, member, nil
}

func (s *httpUpstreamService) PersistAsyncVideoKeyLease(ctx context.Context, handoff *service.AsyncVideoKeyHandoff) error {
	if s.regionalAsyncVideoEnabled() {
		return s.persistRegionalAsyncVideoKeyLease(ctx, handoff)
	}
	limiter, err := s.asyncVideoLeaseStore()
	if err != nil {
		return err
	}
	lease, member, err := validateAsyncVideoHandoff(handoff)
	if err != nil {
		return err
	}
	now := time.Now().UTC().UnixMilli()
	record := asyncVideoKeyLeaseRecord{Member: member, State: asyncVideoLeaseStateProvisional, Lease: lease, Owner: handoff.Owner(), CreatedAt: now, UpdatedAt: now}
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	result, err := persistAsyncVideoKeyLeaseScript.Run(ctx, limiter.rdb,
		[]string{asyncVideoMemberKey(member), asyncVideoUnknownKey(), asyncVideoFenceKey(member)}, payload, now, member).Int()
	if err != nil {
		return err
	}
	if result != 1 {
		return service.ErrUpstreamKeyPolicy
	}
	return nil
}

func (s *httpUpstreamService) BindAsyncVideoKeyLease(ctx context.Context, handoff *service.AsyncVideoKeyHandoff, taskID string) error {
	if s.regionalAsyncVideoEnabled() {
		return s.bindRegionalAsyncVideoKeyLease(ctx, handoff, taskID)
	}
	limiter, err := s.asyncVideoLeaseStore()
	if err != nil {
		return err
	}
	lease, member, err := validateAsyncVideoHandoff(handoff)
	if err != nil {
		return err
	}
	taskID = strings.TrimSpace(taskID)
	if taskID == "" || len(taskID) > 256 {
		return service.ErrUpstreamKeyPolicy
	}
	return bindAsyncVideoKeyLeaseRecord(ctx, limiter, member, taskID, &asyncVideoKeyLeaseRecord{Owner: handoff.Owner(), Lease: lease})
}

func bindAsyncVideoKeyLeaseRecord(ctx context.Context, limiter *upstreamKeyLimiter, member, taskID string, expected *asyncVideoKeyLeaseRecord) error {
	if limiter == nil || limiter.rdb == nil {
		return service.ErrUpstreamKeyPolicy
	}
	for attempt := 0; attempt < 3; attempt++ {
		raw, err := limiter.rdb.Get(ctx, asyncVideoMemberKey(member)).Bytes()
		if errors.Is(err, redis.Nil) {
			return service.ErrUpstreamKeyLease
		}
		if err != nil {
			return err
		}
		var record asyncVideoKeyLeaseRecord
		if json.Unmarshal(raw, &record) != nil || record.Member != member || record.Lease == nil || record.Lease.Validate() != nil ||
			record.Lease.Member != member || !record.Owner.Valid() {
			return service.ErrUpstreamKeyPolicy
		}
		if expected != nil && (record.Owner != expected.Owner || !reflect.DeepEqual(record.Lease, expected.Lease)) {
			return service.ErrUpstreamKeyPolicy
		}
		switch record.State {
		case asyncVideoLeaseStateProvisional, asyncVideoLeaseStateUnknown:
			if record.TaskID != "" {
				return service.ErrUpstreamKeyPolicy
			}
		case asyncVideoLeaseStateBound:
			if record.TaskID == "" || len(record.TaskID) > 256 || record.TaskID != strings.TrimSpace(record.TaskID) {
				return service.ErrUpstreamKeyPolicy
			}
		default:
			return service.ErrUpstreamKeyPolicy
		}
		record.TaskID = taskID
		record.State = asyncVideoLeaseStateBound
		record.UpdatedAt = time.Now().UTC().UnixMilli()
		payload, err := json.Marshal(&record)
		if err != nil {
			return err
		}
		result, err := bindAsyncVideoKeyLeaseScript.Run(ctx, limiter.rdb,
			[]string{asyncVideoMemberKey(member), video.TaskKey(record.Owner, taskID), asyncVideoUnknownKey(), asyncVideoFenceKey(member)},
			member, payload, raw, taskID).Int()
		if err != nil {
			return err
		}
		switch result {
		case 1:
			return nil
		case 0:
			return service.ErrUpstreamKeyLease
		case -2:
			continue
		default:
			return fmt.Errorf("async video task lease is already bound")
		}
	}
	return service.ErrUpstreamKeyLease
}

func (s *httpUpstreamService) BindAsyncVideoKeyLeaseByMember(ctx context.Context, member, taskID string) error {
	if s.regionalAsyncVideoEnabled() {
		return s.bindRegionalAsyncVideoKeyLeaseByMember(ctx, member, taskID)
	}
	limiter, err := s.asyncVideoLeaseStore()
	if err != nil {
		return err
	}
	member = strings.TrimSpace(member)
	taskID = strings.TrimSpace(taskID)
	if !strings.HasPrefix(member, "video:") || taskID == "" || len(taskID) > 256 {
		return service.ErrUpstreamKeyPolicy
	}
	return bindAsyncVideoKeyLeaseRecord(ctx, limiter, member, taskID, nil)
}

func (s *httpUpstreamService) MarkAsyncVideoKeySubmissionUnknown(ctx context.Context, handoff *service.AsyncVideoKeyHandoff) error {
	if s.regionalAsyncVideoEnabled() {
		return s.markRegionalAsyncVideoKeySubmissionUnknown(ctx, handoff)
	}
	limiter, err := s.asyncVideoLeaseStore()
	if err != nil {
		return err
	}
	lease, member, err := validateAsyncVideoHandoff(handoff)
	if err != nil {
		return err
	}
	key := asyncVideoMemberKey(member)
	for attempt := 0; attempt < 3; attempt++ {
		raw, err := limiter.rdb.Get(ctx, key).Bytes()
		if errors.Is(err, redis.Nil) {
			return service.ErrUpstreamKeyLease
		}
		if err != nil {
			return err
		}
		var record asyncVideoKeyLeaseRecord
		if json.Unmarshal(raw, &record) != nil || record.Member != member || record.Lease == nil || record.Lease.Validate() != nil ||
			!reflect.DeepEqual(record.Lease, lease) || record.Owner != handoff.Owner() {
			return service.ErrUpstreamKeyPolicy
		}
		switch record.State {
		case asyncVideoLeaseStateBound:
			if record.TaskID == "" || len(record.TaskID) > 256 || record.TaskID != strings.TrimSpace(record.TaskID) {
				return service.ErrUpstreamKeyPolicy
			}
			return nil
		case asyncVideoLeaseStateProvisional, asyncVideoLeaseStateUnknown:
			if record.TaskID != "" {
				return service.ErrUpstreamKeyPolicy
			}
		default:
			return service.ErrUpstreamKeyPolicy
		}
		record.State = asyncVideoLeaseStateUnknown
		record.UpdatedAt = time.Now().UTC().UnixMilli()
		payload, err := json.Marshal(record)
		if err != nil {
			return err
		}
		result, err := markAsyncVideoKeyUnknownScript.Run(ctx, limiter.rdb, []string{key, asyncVideoUnknownKey(), asyncVideoFenceKey(member)},
			raw, payload, record.UpdatedAt, member).Int()
		if err != nil {
			return err
		}
		switch result {
		case 1:
			return nil
		case -2:
			continue
		default:
			return service.ErrUpstreamKeyLease
		}
	}
	return service.ErrUpstreamKeyLease
}

func (s *httpUpstreamService) ReleaseAsyncVideoKeyLease(ctx context.Context, handoff *service.AsyncVideoKeyHandoff) error {
	if s.regionalAsyncVideoEnabled() {
		return s.releaseRegionalAsyncVideoKeyLeaseByHandoff(ctx, handoff)
	}
	limiter, err := s.asyncVideoLeaseStore()
	if err != nil {
		return err
	}
	lease, member, err := validateAsyncVideoHandoff(handoff)
	if err != nil {
		return err
	}
	taskID := ""
	if raw, getErr := limiter.rdb.Get(ctx, asyncVideoMemberKey(member)).Bytes(); getErr == nil {
		var record asyncVideoKeyLeaseRecord
		if json.Unmarshal(raw, &record) != nil || record.Member != member {
			return service.ErrUpstreamKeyPolicy
		}
		taskID = record.TaskID
	} else if errors.Is(getErr, redis.Nil) {
		return service.ErrUpstreamKeyLease
	} else {
		return getErr
	}
	return s.releaseAsyncVideoKeyLease(ctx, limiter, lease, handoff.Owner(), taskID, false)
}

func (s *httpUpstreamService) LoadAsyncVideoKeyLeaseByTask(ctx context.Context, owner service.AsyncVideoKeyOwner, taskID string) (*service.PersistentUpstreamKeyLease, error) {
	if s.regionalAsyncVideoEnabled() {
		return s.loadRegionalAsyncVideoKeyLeaseByTask(ctx, owner, taskID, false)
	}
	return s.loadAsyncVideoKeyLeaseByTask(ctx, owner, taskID, false)
}

func (s *httpUpstreamService) ResolveAsyncVideoKeyOwner(ctx context.Context, principal service.AsyncVideoKeyOwner, taskID string) (*service.AsyncVideoKeyOwner, error) {
	if s.regionalAsyncVideoEnabled() {
		return s.resolveRegionalAsyncVideoKeyOwner(ctx, principal, taskID)
	}
	if s != nil && (s.upstreamKeyLimiter == nil || s.upstreamKeyLimiter.regional != nil) {
		return nil, nil
	}
	limiter, err := s.asyncVideoLeaseStore()
	if err != nil {
		return nil, err
	}
	key := video.TaskKey(principal, taskID)
	if key == "" {
		return nil, service.ErrUpstreamKeyPolicy
	}
	raw, err := limiter.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record asyncVideoKeyLeaseRecord
	if json.Unmarshal(raw, &record) != nil || !record.Owner.Valid() || record.State != asyncVideoLeaseStateBound ||
		record.TaskID != strings.TrimSpace(taskID) || video.TaskKey(record.Owner, record.TaskID) != key ||
		record.Lease == nil || record.Lease.Validate() != nil || record.Member != record.Lease.Member {
		return nil, service.ErrUpstreamKeyPolicy
	}
	memberRaw, err := limiter.rdb.Get(ctx, asyncVideoMemberKey(record.Member)).Bytes()
	if err != nil || string(memberRaw) != string(raw) {
		return nil, service.ErrUpstreamKeyPolicy
	}
	return &record.Owner, nil
}

func (s *httpUpstreamService) loadAsyncVideoKeyLeaseByTask(ctx context.Context, owner service.AsyncVideoKeyOwner, taskID string, allowFenced bool) (*service.PersistentUpstreamKeyLease, error) {
	if s != nil && (s.upstreamKeyLimiter == nil || s.upstreamKeyLimiter.regional != nil) {
		return nil, nil
	}
	limiter, err := s.asyncVideoLeaseStore()
	if err != nil {
		return nil, err
	}
	taskID = strings.TrimSpace(taskID)
	taskKey := video.TaskKey(owner, taskID)
	if taskKey == "" {
		return nil, service.ErrUpstreamKeyPolicy
	}
	raw, err := limiter.rdb.Get(ctx, taskKey).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, err
	}
	var record asyncVideoKeyLeaseRecord
	if err := json.Unmarshal(raw, &record); err != nil || record.Owner != owner || record.State != asyncVideoLeaseStateBound ||
		record.Lease == nil || record.Lease.Validate() != nil || record.Member != record.Lease.Member || record.TaskID != taskID {
		return nil, service.ErrUpstreamKeyPolicy
	}
	memberRaw, err := limiter.rdb.Get(ctx, asyncVideoMemberKey(record.Member)).Bytes()
	if err != nil || string(memberRaw) != string(raw) {
		return nil, service.ErrUpstreamKeyPolicy
	}
	fenced, err := limiter.rdb.Exists(ctx, asyncVideoFenceKey(record.Member)).Result()
	if err != nil || (fenced != 0 && !allowFenced) {
		return nil, service.ErrUpstreamKeyLease
	}
	return record.Lease, nil
}

func (s *httpUpstreamService) ReleaseAsyncVideoKeyLeaseByTask(ctx context.Context, owner service.AsyncVideoKeyOwner, taskID string) error {
	if s.regionalAsyncVideoEnabled() {
		lease, err := s.loadRegionalAsyncVideoKeyLeaseByTask(ctx, owner, taskID, true)
		if err != nil || lease == nil {
			return err
		}
		return s.releaseRegionalAsyncVideoKeyLease(ctx, lease, owner, taskID)
	}
	lease, err := s.loadAsyncVideoKeyLeaseByTask(ctx, owner, taskID, true)
	if err != nil || lease == nil {
		return err
	}
	return s.releaseAsyncVideoKeyLease(ctx, s.upstreamKeyLimiter, lease, owner, taskID, false)
}

func (s *httpUpstreamService) ReleaseAsyncVideoKeyLeaseByMember(ctx context.Context, member string) error {
	if s.regionalAsyncVideoEnabled() {
		return s.releaseRegionalAsyncVideoKeyLeaseByMember(ctx, member)
	}
	return s.releaseAsyncVideoKeyLeaseByMember(ctx, member, false)
}

func (s *httpUpstreamService) releaseAsyncVideoKeyLeaseByMember(ctx context.Context, member string, requireFence bool) error {
	limiter, err := s.asyncVideoLeaseStore()
	if err != nil {
		return err
	}
	member = strings.TrimSpace(member)
	if !strings.HasPrefix(member, "video:") {
		return service.ErrUpstreamKeyPolicy
	}
	raw, err := limiter.rdb.Get(ctx, asyncVideoMemberKey(member)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			if requireFence {
				return service.ErrUpstreamKeyLease
			}
			return nil
		}
		return err
	}
	var record asyncVideoKeyLeaseRecord
	if err := json.Unmarshal(raw, &record); err != nil || record.Member != member || record.Lease == nil || record.Lease.Validate() != nil ||
		record.Lease.Member != member || !record.Owner.Valid() {
		return service.ErrUpstreamKeyPolicy
	}
	return s.releaseAsyncVideoKeyLease(ctx, limiter, record.Lease, record.Owner, record.TaskID, requireFence)
}

func (s *httpUpstreamService) releaseAsyncVideoKeyLease(ctx context.Context, limiter *upstreamKeyLimiter, lease *service.PersistentUpstreamKeyLease, owner service.AsyncVideoKeyOwner, taskID string, requireFence bool) error {
	if limiter == nil || limiter.rdb == nil || lease == nil || lease.Validate() != nil || !owner.Valid() {
		return service.ErrUpstreamKeyPolicy
	}
	raw, err := limiter.rdb.Get(ctx, asyncVideoMemberKey(lease.Member)).Bytes()
	if errors.Is(err, redis.Nil) {
		return service.ErrUpstreamKeyLease
	}
	if err != nil {
		return err
	}
	var record asyncVideoKeyLeaseRecord
	if json.Unmarshal(raw, &record) != nil || record.Member != lease.Member || record.Owner != owner ||
		!reflect.DeepEqual(record.Lease, lease) || record.TaskID != taskID {
		return service.ErrUpstreamKeyPolicy
	}
	taskKey := ""
	switch record.State {
	case asyncVideoLeaseStateBound:
		if taskID == "" || len(taskID) > 256 || taskID != strings.TrimSpace(taskID) {
			return service.ErrUpstreamKeyPolicy
		}
		taskKey = video.TaskKey(owner, taskID)
	case asyncVideoLeaseStateProvisional, asyncVideoLeaseStateUnknown:
		if taskID != "" {
			return service.ErrUpstreamKeyPolicy
		}
	default:
		return service.ErrUpstreamKeyPolicy
	}
	fenceRequired := "0"
	if requireFence {
		fenceRequired = "1"
	}
	result, err := fenceAsyncVideoKeyLeaseScript.Run(ctx, limiter.rdb,
		[]string{asyncVideoFenceKey(lease.Member), asyncVideoMemberKey(lease.Member), asyncVideoUnknownKey(), taskKey, asyncVideoReleaseKey()},
		lease.Member, raw, fenceRequired).Int()
	if err != nil {
		return err
	}
	if result != 1 {
		return service.ErrUpstreamKeyLease
	}
	if err := s.releasePersistentUpstreamKeySlots(ctx, lease); err != nil {
		return err
	}
	return deleteAsyncVideoKeyLease(limiter, ctx, lease.Member, owner, taskID)
}

func deleteAsyncVideoKeyLease(limiter *upstreamKeyLimiter, ctx context.Context, member string, owner service.AsyncVideoKeyOwner, taskID string) error {
	if limiter == nil || limiter.rdb == nil {
		return service.ErrUpstreamKeyPolicy
	}
	payload, err := limiter.rdb.Get(ctx, asyncVideoMemberKey(member)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil
	}
	if err != nil {
		return err
	}
	result, err := deleteAsyncVideoKeyLeaseScript.Run(ctx, limiter.rdb,
		[]string{asyncVideoMemberKey(member), video.TaskKey(owner, taskID), asyncVideoUnknownKey(), asyncVideoFenceKey(member), asyncVideoReleaseKey()}, payload, member).Int()
	if err != nil {
		return err
	}
	if result != 1 {
		return service.ErrUpstreamKeyLease
	}
	return nil
}

func (s *httpUpstreamService) RecoverAsyncVideoKeyReleases(ctx context.Context, limit int) (int, error) {
	if s.regionalAsyncVideoEnabled() {
		return s.recoverRegionalAsyncVideoKeyReleases(ctx, limit)
	}
	if s != nil && (s.upstreamKeyLimiter == nil || s.upstreamKeyLimiter.regional != nil) {
		return 0, nil
	}
	limiter, err := s.asyncVideoLeaseStore()
	if err != nil {
		return 0, err
	}
	if limit <= 0 || limit > 128 {
		return 0, service.ErrUpstreamKeyPolicy
	}
	serverNow, err := limiter.rdb.Time(ctx).Result()
	if err != nil {
		return 0, err
	}
	nowMillis := serverNow.UnixMilli()
	members, err := limiter.rdb.ZRangeByScore(ctx, asyncVideoReleaseKey(), &redis.ZRangeBy{ //nolint:staticcheck // Redis 6.0 compatibility; replacement options require Redis 6.2.
		Min: "-inf", Max: strconv.FormatInt(nowMillis, 10), Count: int64(limit),
	}).Result()
	if err != nil {
		return 0, err
	}
	recovered := 0
	var recoveryErr error
	for _, member := range members {
		if err := ctx.Err(); err != nil {
			return recovered, errors.Join(recoveryErr, err)
		}
		claimUntil := nowMillis + asyncVideoReleaseClaimTTL.Milliseconds()
		claimed, claimErr := claimAsyncVideoKeyReleaseScript.Run(ctx, limiter.rdb, []string{asyncVideoReleaseKey()}, member, nowMillis, claimUntil).Int()
		if claimErr != nil {
			return recovered, errors.Join(recoveryErr, claimErr)
		}
		if claimed != 1 {
			continue
		}
		if err := s.releaseAsyncVideoKeyLeaseByMember(ctx, member, true); err != nil {
			deferred, deferErr := deferAsyncVideoKeyReleaseScript.Run(ctx, limiter.rdb,
				[]string{asyncVideoReleaseKey()},
				member, claimUntil, (30 * time.Second).Milliseconds()).Int()
			if deferErr != nil {
				return recovered, errors.Join(recoveryErr, err, deferErr)
			}
			if deferred == 1 {
				recoveryErr = errors.Join(recoveryErr, err)
			}
			continue
		}
		recovered++
	}
	return recovered, recoveryErr
}
