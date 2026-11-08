package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/regional/slots"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type regionalAsyncVideoKeyRecord struct {
	Member  string
	TaskID  string
	TaskKey string
	State   string
	Owner   service.AsyncVideoKeyOwner
	Lease   *service.PersistentUpstreamKeyLease
	Fenced  bool
}

const regionalAsyncVideoSelect = `SELECT member,COALESCE(task_id,''),COALESCE(task_key,''),state,owner,lease,fenced FROM regional_async_video_key_leases`

func (s *httpUpstreamService) regionalAsyncVideoStore() (*regionalConcurrencyCache, error) {
	if s == nil || s.upstreamKeyLimiter == nil || s.upstreamKeyLimiter.regional == nil ||
		s.upstreamKeyLimiter.regional.primary == nil || s.upstreamKeyLimiter.regional.runtimeFailed.Load() {
		return nil, service.ErrUpstreamKeyPolicy
	}
	return s.upstreamKeyLimiter.regional, nil
}

func scanRegionalAsyncVideoRecord(row interface{ Scan(...any) error }) (*regionalAsyncVideoKeyRecord, error) {
	var record regionalAsyncVideoKeyRecord
	var ownerJSON, leaseJSON []byte
	if err := row.Scan(&record.Member, &record.TaskID, &record.TaskKey, &record.State, &ownerJSON, &leaseJSON, &record.Fenced); err != nil {
		return nil, err
	}
	if json.Unmarshal(ownerJSON, &record.Owner) != nil || json.Unmarshal(leaseJSON, &record.Lease) != nil ||
		!record.Owner.Valid() || record.Lease == nil || record.Lease.Validate() != nil ||
		record.Lease.Authority != "regional:primary" || record.Lease.Member != record.Member {
		return nil, service.ErrUpstreamKeyPolicy
	}
	switch record.State {
	case asyncVideoLeaseStateBound:
		if record.TaskID == "" || len(record.TaskID) > 256 || record.TaskID != strings.TrimSpace(record.TaskID) ||
			record.TaskKey != asyncVideoTaskKey(record.Owner, record.TaskID) {
			return nil, service.ErrUpstreamKeyPolicy
		}
	case asyncVideoLeaseStateProvisional, asyncVideoLeaseStateUnknown:
		if record.TaskID != "" || record.TaskKey != "" {
			return nil, service.ErrUpstreamKeyPolicy
		}
	default:
		return nil, service.ErrUpstreamKeyPolicy
	}
	return &record, nil
}

func (s *httpUpstreamService) persistRegionalAsyncVideoKeyLease(ctx context.Context, handoff *service.AsyncVideoKeyHandoff) error {
	store, err := s.regionalAsyncVideoStore()
	if err != nil {
		return err
	}
	lease, member, err := validateAsyncVideoHandoff(handoff)
	if err != nil || lease.Authority != "regional:primary" {
		return service.ErrUpstreamKeyPolicy
	}
	ownerJSON, err := json.Marshal(handoff.Owner())
	if err != nil {
		return err
	}
	leaseJSON, err := json.Marshal(lease)
	if err != nil {
		return err
	}
	result, err := store.database().ExecContext(ctx, `INSERT INTO regional_async_video_key_leases(member,state,owner,lease) VALUES($1,$2,$3::jsonb,$4::jsonb) ON CONFLICT(member) DO NOTHING`,
		member, asyncVideoLeaseStateProvisional, string(ownerJSON), string(leaseJSON))
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return service.ErrUpstreamKeyPolicy
	}
	return nil
}

func (s *httpUpstreamService) bindRegionalAsyncVideoKeyLease(ctx context.Context, handoff *service.AsyncVideoKeyHandoff, taskID string) error {
	lease, member, err := validateAsyncVideoHandoff(handoff)
	if err != nil {
		return err
	}
	return s.bindRegionalAsyncVideoKeyLeaseRecord(ctx, member, taskID, &regionalAsyncVideoKeyRecord{Owner: handoff.Owner(), Lease: lease})
}

func (s *httpUpstreamService) bindRegionalAsyncVideoKeyLeaseByMember(ctx context.Context, member, taskID string) error {
	return s.bindRegionalAsyncVideoKeyLeaseRecord(ctx, strings.TrimSpace(member), taskID, nil)
}

func (s *httpUpstreamService) bindRegionalAsyncVideoKeyLeaseRecord(ctx context.Context, member, taskID string, expected *regionalAsyncVideoKeyRecord) error {
	store, err := s.regionalAsyncVideoStore()
	if err != nil {
		return err
	}
	taskID = strings.TrimSpace(taskID)
	if !strings.HasPrefix(member, "video:") || taskID == "" || len(taskID) > 256 {
		return service.ErrUpstreamKeyPolicy
	}
	tx, err := store.database().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	record, err := scanRegionalAsyncVideoRecord(tx.QueryRowContext(ctx, regionalAsyncVideoSelect+` WHERE member=$1 FOR UPDATE`, member))
	if errors.Is(err, sql.ErrNoRows) {
		return service.ErrUpstreamKeyLease
	}
	if err != nil {
		return err
	}
	if expected != nil && (record.Owner != expected.Owner || !reflect.DeepEqual(record.Lease, expected.Lease)) {
		return service.ErrUpstreamKeyPolicy
	}
	if record.Fenced {
		return service.ErrUpstreamKeyLease
	}
	if record.State == asyncVideoLeaseStateBound {
		if record.TaskID == taskID {
			return nil
		}
		return service.ErrUpstreamKeyPolicy
	}
	_, err = tx.ExecContext(ctx, `UPDATE regional_async_video_key_leases SET task_id=$2,task_key=$3,state='bound',updated_at=clock_timestamp() WHERE member=$1`,
		member, taskID, asyncVideoTaskKey(record.Owner, taskID))
	if err != nil {
		return errors.Join(service.ErrUpstreamKeyPolicy, err)
	}
	return tx.Commit()
}

func (s *httpUpstreamService) markRegionalAsyncVideoKeySubmissionUnknown(ctx context.Context, handoff *service.AsyncVideoKeyHandoff) error {
	store, err := s.regionalAsyncVideoStore()
	if err != nil {
		return err
	}
	lease, member, err := validateAsyncVideoHandoff(handoff)
	if err != nil {
		return err
	}
	tx, err := store.database().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	record, err := scanRegionalAsyncVideoRecord(tx.QueryRowContext(ctx, regionalAsyncVideoSelect+` WHERE member=$1 FOR UPDATE`, member))
	if errors.Is(err, sql.ErrNoRows) {
		return service.ErrUpstreamKeyLease
	}
	if err != nil {
		return err
	}
	if record.Owner != handoff.Owner() || !reflect.DeepEqual(record.Lease, lease) {
		return service.ErrUpstreamKeyPolicy
	}
	if record.State == asyncVideoLeaseStateBound {
		return nil
	}
	if record.Fenced {
		return service.ErrUpstreamKeyLease
	}
	if _, err := tx.ExecContext(ctx, `UPDATE regional_async_video_key_leases SET state='unknown',updated_at=clock_timestamp() WHERE member=$1`, member); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *httpUpstreamService) loadRegionalAsyncVideoKeyLeaseByTask(ctx context.Context, owner service.AsyncVideoKeyOwner, taskID string, allowFenced bool) (*service.PersistentUpstreamKeyLease, error) {
	store, err := s.regionalAsyncVideoStore()
	if err != nil {
		return nil, err
	}
	taskID = strings.TrimSpace(taskID)
	taskKey := asyncVideoTaskKey(owner, taskID)
	if taskKey == "" || !owner.Valid() {
		return nil, service.ErrUpstreamKeyPolicy
	}
	record, err := scanRegionalAsyncVideoRecord(store.database().QueryRowContext(ctx, regionalAsyncVideoSelect+` WHERE task_key=$1`, taskKey))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if record.Owner != owner || record.TaskID != taskID {
		return nil, service.ErrUpstreamKeyPolicy
	}
	if record.Fenced && !allowFenced {
		return nil, service.ErrUpstreamKeyLease
	}
	return record.Lease, nil
}

func (s *httpUpstreamService) resolveRegionalAsyncVideoKeyOwner(ctx context.Context, principal service.AsyncVideoKeyOwner, taskID string) (*service.AsyncVideoKeyOwner, error) {
	store, err := s.regionalAsyncVideoStore()
	if err != nil {
		return nil, err
	}
	taskID = strings.TrimSpace(taskID)
	taskKey := asyncVideoTaskKey(principal, taskID)
	if taskKey == "" {
		return nil, service.ErrUpstreamKeyPolicy
	}
	record, err := scanRegionalAsyncVideoRecord(store.database().QueryRowContext(ctx, regionalAsyncVideoSelect+` WHERE task_key=$1`, taskKey))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if record.TaskID != taskID || record.TaskKey != taskKey {
		return nil, service.ErrUpstreamKeyPolicy
	}
	return &record.Owner, nil
}

func (s *httpUpstreamService) releaseRegionalAsyncVideoKeyLease(ctx context.Context, lease *service.PersistentUpstreamKeyLease, owner service.AsyncVideoKeyOwner, taskID string) error {
	store, err := s.regionalAsyncVideoStore()
	if err != nil {
		return err
	}
	if lease == nil || lease.Validate() != nil || lease.Authority != "regional:primary" || !owner.Valid() {
		return service.ErrUpstreamKeyPolicy
	}
	tx, err := store.database().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	record, err := scanRegionalAsyncVideoRecord(tx.QueryRowContext(ctx, regionalAsyncVideoSelect+` WHERE member=$1 FOR UPDATE`, lease.Member))
	if errors.Is(err, sql.ErrNoRows) {
		return service.ErrUpstreamKeyLease
	}
	if err != nil {
		return err
	}
	if record.Owner != owner || !reflect.DeepEqual(record.Lease, lease) || record.TaskID != strings.TrimSpace(taskID) {
		return service.ErrUpstreamKeyPolicy
	}
	if !record.Fenced {
		if _, err := tx.ExecContext(ctx, `UPDATE regional_async_video_key_leases SET fenced=true,release_due_at=clock_timestamp(),updated_at=clock_timestamp() WHERE member=$1`, record.Member); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_, err = s.finishRegionalAsyncVideoKeyRelease(ctx, record.Member)
	return err
}

func (s *httpUpstreamService) releaseRegionalAsyncVideoKeyLeaseByMember(ctx context.Context, member string) error {
	store, err := s.regionalAsyncVideoStore()
	if err != nil {
		return err
	}
	member = strings.TrimSpace(member)
	if !strings.HasPrefix(member, "video:") {
		return service.ErrUpstreamKeyPolicy
	}
	record, err := scanRegionalAsyncVideoRecord(store.database().QueryRowContext(ctx, regionalAsyncVideoSelect+` WHERE member=$1`, member))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.releaseRegionalAsyncVideoKeyLease(ctx, record.Lease, record.Owner, record.TaskID)
}

func (s *httpUpstreamService) releaseRegionalAsyncVideoKeyLeaseByHandoff(ctx context.Context, handoff *service.AsyncVideoKeyHandoff) error {
	lease, member, err := validateAsyncVideoHandoff(handoff)
	if err != nil {
		return err
	}
	store, err := s.regionalAsyncVideoStore()
	if err != nil {
		return err
	}
	record, err := scanRegionalAsyncVideoRecord(store.database().QueryRowContext(ctx, regionalAsyncVideoSelect+` WHERE member=$1`, member))
	if errors.Is(err, sql.ErrNoRows) {
		return service.ErrUpstreamKeyLease
	}
	if err != nil {
		return err
	}
	return s.releaseRegionalAsyncVideoKeyLease(ctx, lease, handoff.Owner(), record.TaskID)
}

func (s *httpUpstreamService) finishRegionalAsyncVideoKeyRelease(ctx context.Context, member string) (bool, error) {
	store, err := s.regionalAsyncVideoStore()
	if err != nil {
		return false, err
	}
	tx, err := store.database().BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	record, err := scanRegionalAsyncVideoRecord(tx.QueryRowContext(ctx, regionalAsyncVideoSelect+` WHERE member=$1 FOR UPDATE`, member))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !record.Fenced {
		return false, service.ErrUpstreamKeyLease
	}
	resources := append([]service.PersistentUpstreamKeyResource(nil), record.Lease.Resources...)
	sort.Slice(resources, func(i, j int) bool { return resources[i].ResourceID < resources[j].ResourceID })
	for _, resource := range resources {
		if err := store.releaseTx(ctx, tx, slots.Resource{Kind: "upstream_key", ID: resource.ResourceID, Fingerprint: resource.Fingerprint}, record.Member); err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM regional_async_video_key_leases WHERE member=$1`, record.Member); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *httpUpstreamService) recoverRegionalAsyncVideoKeyReleases(ctx context.Context, limit int) (int, error) {
	store, err := s.regionalAsyncVideoStore()
	if err != nil {
		return 0, err
	}
	if limit <= 0 || limit > 128 {
		return 0, service.ErrUpstreamKeyPolicy
	}
	rows, err := store.database().QueryContext(ctx, `SELECT member FROM regional_async_video_key_leases WHERE fenced AND release_due_at<=clock_timestamp() ORDER BY release_due_at,member LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	var members []string
	for rows.Next() {
		var member string
		if err := rows.Scan(&member); err != nil {
			_ = rows.Close()
			return 0, err
		}
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	recovered := 0
	var recoveryErr error
	for _, member := range members {
		if err := ctx.Err(); err != nil {
			return recovered, errors.Join(recoveryErr, err)
		}
		released, err := s.finishRegionalAsyncVideoKeyRelease(ctx, member)
		if err != nil {
			result, deferErr := store.database().ExecContext(ctx, `UPDATE regional_async_video_key_leases SET release_due_at=clock_timestamp()+INTERVAL '30 seconds',updated_at=clock_timestamp() WHERE member=$1 AND fenced`, member)
			if deferErr != nil {
				return recovered, errors.Join(recoveryErr, err, deferErr)
			}
			count, countErr := result.RowsAffected()
			if countErr != nil {
				return recovered, errors.Join(recoveryErr, err, countErr)
			}
			if count != 0 {
				recoveryErr = errors.Join(recoveryErr, err)
			}
			continue
		}
		if released {
			recovered++
		}
	}
	return recovered, recoveryErr
}
