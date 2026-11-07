package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const (
	asyncVideoKeyLeasePrefix = "concurrency:upstream_key:async_video:"
)

var bindAsyncVideoKeyLeaseScript = redis.NewScript(`
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
return 1
`)

type asyncVideoKeyLeaseRecord struct {
	TaskID    string                              `json:"task_id,omitempty"`
	Member    string                              `json:"member"`
	State     string                              `json:"state"`
	Lease     *service.PersistentUpstreamKeyLease `json:"lease"`
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

func asyncVideoTaskKey(taskID string) string {
	return asyncVideoKeyLeasePrefix + "task:" + strings.TrimSpace(taskID)
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
	if lease == nil || lease.Validate() != nil || member == "" || lease.Member != member || !strings.HasPrefix(member, "video:") {
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
	record := asyncVideoKeyLeaseRecord{Member: member, State: asyncVideoLeaseStateProvisional, Lease: lease, CreatedAt: now, UpdatedAt: now}
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	key := asyncVideoMemberKey(member)
	if existing, getErr := limiter.rdb.Get(ctx, key).Bytes(); getErr == nil {
		var prior asyncVideoKeyLeaseRecord
		if json.Unmarshal(existing, &prior) != nil || prior.Member != member {
			return service.ErrUpstreamKeyPolicy
		}
		return nil
	} else if !errors.Is(getErr, redis.Nil) {
		return getErr
	}
	pipe := limiter.rdb.TxPipeline()
	pipe.Set(ctx, key, payload, 0)
	pipe.ZRem(ctx, asyncVideoUnknownKey(), member)
	_, err = pipe.Exec(ctx)
	return err
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
	if record.Lease.Member != lease.Member {
		return service.ErrUpstreamKeyPolicy
	}
	return bindAsyncVideoKeyLeaseRecord(ctx, limiter, member, taskID, &record)
}

func bindAsyncVideoKeyLeaseRecord(ctx context.Context, limiter *upstreamKeyLimiter, member, taskID string, record *asyncVideoKeyLeaseRecord) error {
	if limiter == nil || record == nil || record.Member != member || record.Lease == nil || record.Lease.Validate() != nil {
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
		[]string{asyncVideoMemberKey(member), asyncVideoTaskKey(taskID), asyncVideoUnknownKey()},
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
	if record.Lease.Member != lease.Member {
		return service.ErrUpstreamKeyPolicy
	}
	record.State = asyncVideoLeaseStateUnknown
	record.UpdatedAt = time.Now().UTC().UnixMilli()
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	pipe := limiter.rdb.TxPipeline()
	pipe.Set(ctx, key, payload, 0)
	pipe.ZAdd(ctx, asyncVideoUnknownKey(), redis.Z{Score: float64(record.UpdatedAt), Member: member})
	_, err = pipe.Exec(ctx)
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
		if json.Unmarshal(raw, &record) == nil && record.Member == member {
			taskID = record.TaskID
		}
	} else if !errors.Is(getErr, redis.Nil) {
		return getErr
	}
	if err := s.ReleasePersistentUpstreamKeySlots(ctx, lease); err != nil && !errors.Is(err, service.ErrUpstreamKeyLease) {
		return err
	}
	return deleteAsyncVideoKeyLease(limiter, ctx, member, taskID)
}

func (s *httpUpstreamService) ReleaseAsyncVideoKeyLeaseByTask(ctx context.Context, taskID string) error {
	limiter, err := s.asyncVideoLeaseStore()
	if err != nil {
		return err
	}
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil
	}
	taskKey := asyncVideoTaskKey(taskID)
	raw, err := limiter.rdb.Get(ctx, taskKey).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil
		}
		return err
	}
	var record asyncVideoKeyLeaseRecord
	if err := json.Unmarshal(raw, &record); err != nil || record.Member == "" || record.Lease == nil || record.Lease.Validate() != nil || record.TaskID != taskID {
		return service.ErrUpstreamKeyPolicy
	}
	if err := s.ReleasePersistentUpstreamKeySlots(ctx, record.Lease); err != nil && !errors.Is(err, service.ErrUpstreamKeyLease) {
		return err
	}
	return deleteAsyncVideoKeyLease(limiter, ctx, record.Member, taskID)
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
	if err := json.Unmarshal(raw, &record); err != nil || record.Member != member || record.Lease == nil || record.Lease.Validate() != nil {
		return service.ErrUpstreamKeyPolicy
	}
	if err := s.ReleasePersistentUpstreamKeySlots(ctx, record.Lease); err != nil && !errors.Is(err, service.ErrUpstreamKeyLease) {
		return err
	}
	return deleteAsyncVideoKeyLease(limiter, ctx, member, record.TaskID)
}

func deleteAsyncVideoKeyLease(limiter *upstreamKeyLimiter, ctx context.Context, member, taskID string) error {
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
		[]string{asyncVideoMemberKey(member), asyncVideoTaskKey(taskID), asyncVideoUnknownKey()}, payload, member).Int()
	return err
}
