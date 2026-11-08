package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const (
	asyncVideoKeyLeasePrefix = "concurrency:upstream_key:async_video:"
)

var bindAsyncVideoKeyLeaseScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[4]) ~= 0 then return 0 end
local member = redis.call('GET', KEYS[1])
if not member then return 0 end
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
return 1
`)

var markAsyncVideoKeyUnknownScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[3]) ~= 0 then return 0 end
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return 0 end
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
if redis.call('GET', KEYS[2]) ~= ARGV[3] then return 0 end
if KEYS[4] ~= '' and redis.call('GET', KEYS[4]) ~= ARGV[3] then return 0 end
redis.call('SET', KEYS[1], '1')
redis.call('ZADD', KEYS[3], ARGV[1], ARGV[2])
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

func asyncVideoTaskKey(owner service.AsyncVideoKeyOwner, taskID string) string {
	if !owner.ValidPrincipal() || strings.TrimSpace(taskID) == "" {
		return ""
	}
	value, _ := json.Marshal(struct {
		UserID   int64  `json:"user_id"`
		APIKeyID int64  `json:"api_key_id"`
		GroupID  int64  `json:"group_id"`
		Provider string `json:"provider"`
		TaskID   string `json:"task_id"`
	}{owner.UserID, owner.APIKeyID, owner.GroupID, owner.Provider, strings.TrimSpace(taskID)})
	sum := sha256.Sum256(value)
	return asyncVideoKeyLeasePrefix + "task:v2:" + hex.EncodeToString(sum[:])
}

func asyncVideoUnknownKey() string { return asyncVideoKeyLeasePrefix + "unknown" }

func (s *httpUpstreamService) asyncVideoLeaseStore() (*upstreamKeyLimiter, error) {
	if s == nil || s.upstreamKeyLimiter == nil || s.upstreamKeyLimiter.rdb == nil {
		return nil, service.ErrUpstreamKeyPolicy
	}
	// The regional authority owns the slot, but this process has no regional
	// task-record table yet. Reject rather than split ownership across Redis and
	// SQL; ordinary regional HTTP/WS admission remains unchanged.
	if s.upstreamKeyLimiter.regional != nil {
		return nil, service.ErrUpstreamKeyPolicy
	}
	return s.upstreamKeyLimiter, nil
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
	priorRaw, err := limiter.rdb.Get(ctx, asyncVideoMemberKey(member)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return service.ErrUpstreamKeyLease
		}
		return err
	}
	var record asyncVideoKeyLeaseRecord
	if err := json.Unmarshal(priorRaw, &record); err != nil || record.Member != member || record.Lease == nil || record.Lease.Validate() != nil {
		return service.ErrUpstreamKeyPolicy
	}
	if record.Lease.Member != lease.Member || record.Owner != handoff.Owner() {
		return service.ErrUpstreamKeyPolicy
	}
	return bindAsyncVideoKeyLeaseRecord(ctx, limiter, member, taskID, &record)
}

func bindAsyncVideoKeyLeaseRecord(ctx context.Context, limiter *upstreamKeyLimiter, member, taskID string, record *asyncVideoKeyLeaseRecord) error {
	if limiter == nil || record == nil || record.Member != member || record.Lease == nil || record.Lease.Validate() != nil ||
		record.Lease.Member != member || !record.Owner.Valid() {
		return service.ErrUpstreamKeyPolicy
	}
	record.TaskID = taskID
	record.State = asyncVideoLeaseStateBound
	record.UpdatedAt = time.Now().UTC().UnixMilli()
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	result, err := bindAsyncVideoKeyLeaseScript.Run(ctx, limiter.rdb,
		[]string{asyncVideoMemberKey(member), asyncVideoTaskKey(record.Owner, taskID), asyncVideoUnknownKey(), asyncVideoFenceKey(member)},
		member, payload, 0, taskID).Int()
	if err != nil {
		return err
	}
	switch result {
	case 1:
		return nil
	case 0:
		return service.ErrUpstreamKeyLease
	default:
		return fmt.Errorf("async video task lease is already bound")
	}
}

func (s *httpUpstreamService) BindAsyncVideoKeyLeaseByMember(ctx context.Context, member, taskID string) error {
	limiter, err := s.asyncVideoLeaseStore()
	if err != nil {
		return err
	}
	member = strings.TrimSpace(member)
	taskID = strings.TrimSpace(taskID)
	if !strings.HasPrefix(member, "video:") || taskID == "" || len(taskID) > 256 {
		return service.ErrUpstreamKeyPolicy
	}
	raw, err := limiter.rdb.Get(ctx, asyncVideoMemberKey(member)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return service.ErrUpstreamKeyLease
		}
		return err
	}
	var record asyncVideoKeyLeaseRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return service.ErrUpstreamKeyPolicy
	}
	return bindAsyncVideoKeyLeaseRecord(ctx, limiter, member, taskID, &record)
}

func (s *httpUpstreamService) MarkAsyncVideoKeySubmissionUnknown(ctx context.Context, handoff *service.AsyncVideoKeyHandoff) error {
	limiter, err := s.asyncVideoLeaseStore()
	if err != nil {
		return err
	}
	lease, member, err := validateAsyncVideoHandoff(handoff)
	if err != nil {
		return err
	}
	key := asyncVideoMemberKey(member)
	raw, err := limiter.rdb.Get(ctx, key).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return service.ErrUpstreamKeyLease
		}
		return err
	}
	var record asyncVideoKeyLeaseRecord
	if err := json.Unmarshal(raw, &record); err != nil || record.Member != member || record.Lease == nil || record.Lease.Validate() != nil {
		return service.ErrUpstreamKeyPolicy
	}
	if record.Lease.Member != lease.Member || record.Owner != handoff.Owner() {
		return service.ErrUpstreamKeyPolicy
	}
	if record.State == asyncVideoLeaseStateBound {
		return nil
	}
	record.State = asyncVideoLeaseStateUnknown
	record.UpdatedAt = time.Now().UTC().UnixMilli()
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = markAsyncVideoKeyUnknownScript.Run(ctx, limiter.rdb, []string{key, asyncVideoUnknownKey(), asyncVideoFenceKey(member)},
		raw, payload, record.UpdatedAt, member).Int()
	return err
}

func (s *httpUpstreamService) ReleaseAsyncVideoKeyLease(ctx context.Context, handoff *service.AsyncVideoKeyHandoff) error {
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
	return s.releaseAsyncVideoKeyLease(ctx, limiter, lease, handoff.Owner(), taskID)
}

func (s *httpUpstreamService) LoadAsyncVideoKeyLeaseByTask(ctx context.Context, owner service.AsyncVideoKeyOwner, taskID string) (*service.PersistentUpstreamKeyLease, error) {
	return s.loadAsyncVideoKeyLeaseByTask(ctx, owner, taskID, false)
}

func (s *httpUpstreamService) ResolveAsyncVideoKeyOwner(ctx context.Context, principal service.AsyncVideoKeyOwner, taskID string) (*service.AsyncVideoKeyOwner, error) {
	if s != nil && (s.upstreamKeyLimiter == nil || s.upstreamKeyLimiter.regional != nil) {
		return nil, nil
	}
	limiter, err := s.asyncVideoLeaseStore()
	if err != nil {
		return nil, err
	}
	key := asyncVideoTaskKey(principal, taskID)
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
		record.TaskID != strings.TrimSpace(taskID) || asyncVideoTaskKey(record.Owner, record.TaskID) != key ||
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
	taskKey := asyncVideoTaskKey(owner, taskID)
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
	lease, err := s.loadAsyncVideoKeyLeaseByTask(ctx, owner, taskID, true)
	if err != nil || lease == nil {
		return err
	}
	return s.releaseAsyncVideoKeyLease(ctx, s.upstreamKeyLimiter, lease, owner, taskID)
}

func (s *httpUpstreamService) ReleaseAsyncVideoKeyLeaseByMember(ctx context.Context, member string) error {
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
			return nil
		}
		return err
	}
	var record asyncVideoKeyLeaseRecord
	if err := json.Unmarshal(raw, &record); err != nil || record.Member != member || record.Lease == nil || record.Lease.Validate() != nil ||
		record.Lease.Member != member || !record.Owner.Valid() {
		return service.ErrUpstreamKeyPolicy
	}
	return s.releaseAsyncVideoKeyLease(ctx, limiter, record.Lease, record.Owner, record.TaskID)
}

func (s *httpUpstreamService) releaseAsyncVideoKeyLease(ctx context.Context, limiter *upstreamKeyLimiter, lease *service.PersistentUpstreamKeyLease, owner service.AsyncVideoKeyOwner, taskID string) error {
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
		taskKey = asyncVideoTaskKey(owner, taskID)
	case asyncVideoLeaseStateProvisional, asyncVideoLeaseStateUnknown:
		if taskID != "" {
			return service.ErrUpstreamKeyPolicy
		}
	default:
		return service.ErrUpstreamKeyPolicy
	}
	result, err := fenceAsyncVideoKeyLeaseScript.Run(ctx, limiter.rdb,
		[]string{asyncVideoFenceKey(lease.Member), asyncVideoMemberKey(lease.Member), asyncVideoUnknownKey(), taskKey},
		time.Now().UTC().UnixMilli(), lease.Member, raw).Int()
	if err != nil {
		return err
	}
	if result != 1 {
		return service.ErrUpstreamKeyLease
	}
	if err := s.ReleasePersistentUpstreamKeySlots(ctx, lease); err != nil {
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
	_, err = deleteAsyncVideoKeyLeaseScript.Run(ctx, limiter.rdb,
		[]string{asyncVideoMemberKey(member), asyncVideoTaskKey(owner, taskID), asyncVideoUnknownKey(), asyncVideoFenceKey(member)}, payload, member).Int()
	return err
}
