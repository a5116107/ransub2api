package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/regional/video"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

const asyncVideoDirectoryCompleted = "completed"

func (c *regionalConcurrencyCache) hasAsyncVideoDirectory() bool {
	return c.runtimeConfig != nil && (c.runtimeConfig.Listen != "" || c.runtimeConfig.Endpoint != "" || c.runtimeConfig.VideoForward != nil)
}

// Export serializes revisions and captures bound tasks in one SQL snapshot.
// A size/scan/commit failure publishes nothing, never an apparently empty page.
func (c *regionalConcurrencyCache) exportAsyncVideoDirectory(ctx context.Context) (video.Snapshot, error) {
	snapshot := video.Snapshot{SchemaVersion: video.SchemaVersion, Authority: c.persistentUpstreamKeyAuthority(), Entries: []video.Entry{}}
	tx, err := c.database().BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return snapshot, err
	}
	defer func() { _ = tx.Rollback() }()
	// The lifetime starts with the captured SQL view, before any row-lock
	// wait. A slow export must not re-date old task evidence as freshly read.
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&snapshot.IssuedAt); err != nil {
		return snapshot, err
	}
	snapshot.ExpiresAt = snapshot.IssuedAt.Add(video.Lifetime)
	if err = tx.QueryRowContext(ctx, `INSERT INTO regional_async_video_directory_revisions(authority,revision) VALUES($1,1) ON CONFLICT(authority) DO UPDATE SET revision=regional_async_video_directory_revisions.revision+1 RETURNING revision`, snapshot.Authority).Scan(&snapshot.Revision); err != nil {
		return snapshot, err
	}
	rows, err := tx.QueryContext(ctx, regionalAsyncVideoSelect+` WHERE state='bound' AND lease->>'authority'=$1 ORDER BY task_key LIMIT $2`, snapshot.Authority, video.MaxEntries+1)
	if err != nil {
		return snapshot, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record, err := c.scanRegionalAsyncVideoRecord(rows)
		if err != nil {
			return snapshot, err
		}
		snapshot.Entries = append(snapshot.Entries, video.Entry{TaskKey: record.TaskKey, Member: record.Member, Owner: record.Owner, Fenced: record.Fenced})
	}
	if err = rows.Err(); err != nil {
		return snapshot, err
	}
	if err = rows.Close(); err != nil {
		return snapshot, err
	}
	var now time.Time
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return snapshot, err
	}
	if !snapshot.Fresh(now) {
		return snapshot, video.ErrUnavailable
	}
	return snapshot, tx.Commit()
}

func readAsyncVideoDirectorySnapshot(ctx context.Context, tx *sql.Tx, consumer, authority string, lock bool) (video.Snapshot, error) {
	var snapshot video.Snapshot
	var revision int64
	var raw []byte
	query := `SELECT revision,payload FROM regional_async_video_directory_snapshots WHERE consumer=$1 AND authority=$2`
	if lock {
		query += ` FOR UPDATE`
	}
	if err := tx.QueryRowContext(ctx, query, consumer, authority).Scan(&revision, &raw); err != nil {
		return snapshot, errors.Join(video.ErrUnavailable, err)
	}
	if json.Unmarshal(raw, &snapshot) != nil || snapshot.Revision != revision || snapshot.Authority != authority || snapshot.Validate() != nil {
		return snapshot, video.ErrConflict
	}
	return snapshot, nil
}

func installAsyncVideoDirectorySnapshot(ctx context.Context, tx *sql.Tx, consumer string, snapshot video.Snapshot) error {
	if !video.ValidAuthority(consumer) || consumer == snapshot.Authority {
		return video.ErrConflict
	}
	if err := snapshot.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO regional_async_video_directory_snapshots(consumer,authority,revision,payload) VALUES($1,$2,$3,$4::jsonb) ON CONFLICT DO NOTHING`, consumer, snapshot.Authority, snapshot.Revision, string(raw)); err != nil {
		return err
	}
	previous, err := readAsyncVideoDirectorySnapshot(ctx, tx, consumer, snapshot.Authority, true)
	if err != nil {
		return err
	}
	if snapshot.Revision < previous.Revision || snapshot.IssuedAt.Before(previous.IssuedAt) ||
		(snapshot.Revision == previous.Revision && !snapshot.Equal(previous)) {
		return video.ErrConflict
	}
	// Check at the end, after contention. Same-revision retries cannot revive
	// expired evidence or move its deadline forward.
	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	if !snapshot.Fresh(now) {
		return video.ErrUnavailable
	}
	_, err = tx.ExecContext(ctx, `UPDATE regional_async_video_directory_snapshots SET revision=$3,payload=$4::jsonb WHERE consumer=$1 AND authority=$2`, consumer, snapshot.Authority, snapshot.Revision, string(raw))
	return err
}

// The existing listener has one fixed, certificate-pinned regional assignment.
// Neither a request body nor a directory entry can invent another source.
func (c *regionalConcurrencyCache) exchangeAsyncVideoDirectory(ctx context.Context, region string, report video.Snapshot) (video.Directory, error) {
	var directory video.Directory
	if c == nil || c.primary == nil || c.runtimeConfig == nil || c.runtimeFailed.Load() ||
		region != c.runtimeConfig.Scope.Region || report.Authority != video.LocalAuthority(region) {
		return directory, video.ErrConflict
	}
	tx, err := c.database().BeginTx(ctx, nil)
	if err != nil {
		return directory, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = installAsyncVideoDirectorySnapshot(ctx, tx, video.PrimaryAuthority, report); err != nil {
		return directory, err
	}
	if err = tx.Commit(); err != nil {
		return directory, err
	}
	primary, err := c.exportAsyncVideoDirectory(ctx)
	if err != nil {
		return directory, err
	}
	directory.Snapshots = []video.Snapshot{primary, report}
	return directory, directory.Validate(region)
}

func (c *regionalConcurrencyCache) refreshAsyncVideoDirectory(ctx context.Context) error {
	if c != nil && c.videoForwarder != nil {
		return c.refreshForwardVideoDirectory(ctx)
	}
	if c == nil || c.local == nil || c.brokerClient == nil || c.runtimeFailed.Load() {
		return video.ErrUnavailable
	}
	report, err := c.exportAsyncVideoDirectory(ctx)
	if err != nil {
		return err
	}
	directory, err := c.brokerClient.SyncVideoDirectory(ctx, report)
	if err != nil {
		return err
	}
	tx, err := c.database().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, snapshot := range directory.Snapshots {
		if snapshot.Authority == report.Authority {
			continue
		}
		if err = installAsyncVideoDirectorySnapshot(ctx, tx, report.Authority, snapshot); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Poll every configured source through its existing pinned media client. The
// bounded fanout is background metadata only; business lookups read local SQL.
func (c *regionalConcurrencyCache) refreshForwardVideoDirectory(ctx context.Context) error {
	if c.runtimeFailed.Load() {
		return video.ErrUnavailable
	}
	authorities := c.videoForwarder.Authorities()
	results := make(chan error, len(authorities))
	for _, authority := range authorities {
		go func() {
			cycle, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			snapshot, err := c.videoForwarder.Snapshot(cycle, authority)
			if err == nil {
				var tx *sql.Tx
				tx, err = c.database().BeginTx(cycle, nil)
				if err == nil {
					defer func() { _ = tx.Rollback() }()
					err = installAsyncVideoDirectorySnapshot(cycle, tx, c.persistentUpstreamKeyAuthority(), snapshot)
					if err == nil {
						err = tx.Commit()
					}
				}
			}
			results <- err
		}()
	}
	var joined error
	for range authorities {
		joined = errors.Join(joined, <-results)
	}
	return joined
}

func (c *regionalConcurrencyCache) checkAsyncVideoDirectory(ctx context.Context, tx *sql.Tx, taskKey string, local *regionalAsyncVideoKeyRecord) error {
	if !c.hasAsyncVideoDirectory() {
		return nil
	}
	if c.runtimeFailed.Load() {
		return video.ErrUnavailable
	}
	authorities := []string{video.PrimaryAuthority}
	if c.primary != nil {
		authorities[0] = video.LocalAuthority(c.runtimeConfig.Scope.Region)
	}
	if c.runtimeConfig.VideoForward != nil {
		authorities = nil
		for _, peer := range c.runtimeConfig.VideoForward.Peers {
			authorities = append(authorities, peer.Authority)
		}
		if len(authorities) == 0 {
			return video.ErrUnavailable
		}
	}
	snapshots := make([]video.Snapshot, 0, len(authorities))
	for _, authority := range authorities {
		snapshot, err := readAsyncVideoDirectorySnapshot(ctx, tx, c.persistentUpstreamKeyAuthority(), authority, false)
		if err != nil {
			return err
		}
		snapshots = append(snapshots, snapshot)
	}
	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	var remote *video.RemoteTask
	for _, snapshot := range snapshots {
		if !snapshot.Fresh(now) {
			return video.ErrUnavailable
		}
		for _, entry := range snapshot.Entries {
			if entry.TaskKey == taskKey {
				if local != nil || remote != nil {
					return video.ErrConflict
				}
				remote = &video.RemoteTask{Authority: snapshot.Authority, Entry: entry}
			}
		}
	}
	if remote != nil {
		if remote.Entry.Fenced {
			return video.ErrUnavailable
		}
		return remote
	}
	if local == nil {
		// A fresh directory with no match is still not authority to guess a
		// sticky account: the source may not have exported its newest task yet.
		return errors.Join(video.ErrUnavailable, video.ErrTaskUnknown)
	}
	return nil
}

// Local lease and foreign directory evidence share one read-only transaction.
// Terminal release bypasses the directory: expired metadata cannot strand a
// proven terminal task's existing capacity or grant ownership of another one.
func (c *regionalConcurrencyCache) readRegionalAsyncVideoTask(ctx context.Context, taskKey string, checkDirectory bool) (*regionalAsyncVideoKeyRecord, error) {
	tx, err := c.database().BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	record, err := c.scanRegionalAsyncVideoRecord(tx.QueryRowContext(ctx, regionalAsyncVideoSelect+` WHERE task_key=$1`, taskKey))
	if errors.Is(err, sql.ErrNoRows) {
		record, err = c.readCompletedAsyncVideoTask(ctx, tx, taskKey)
	}
	if err != nil {
		return nil, err
	}
	if checkDirectory {
		if err := c.checkAsyncVideoDirectory(ctx, tx, taskKey, record); err != nil {
			return nil, errors.Join(service.ErrUpstreamKeyPolicy, err)
		}
	}
	return record, tx.Commit()
}

// A prepared lookup can wait before HTTP admission. Re-read current local
// ownership and directory evidence without allocating another persistent slot.
func (c *regionalConcurrencyCache) checkAsyncVideoDirectoryLookup(ctx context.Context, lease *service.PersistentUpstreamKeyLease, taskID string) error {
	if !c.hasAsyncVideoDirectory() {
		return nil
	}
	owner, ok := service.AsyncVideoKeyOwnerFromContext(ctx)
	if !ok {
		return service.ErrUpstreamKeyPolicy
	}
	record, err := c.readRegionalAsyncVideoTask(ctx, video.TaskKey(owner, taskID), true)
	if err != nil {
		return err
	}
	if record == nil || record.Fenced || record.Owner != owner || !reflect.DeepEqual(record.Lease, lease) {
		return service.ErrUpstreamKeyPolicy
	}
	return nil
}

func (c *regionalConcurrencyCache) readCompletedAsyncVideoTask(ctx context.Context, tx *sql.Tx, taskKey string) (*regionalAsyncVideoKeyRecord, error) {
	if !c.hasAsyncVideoDirectory() {
		return nil, nil
	}
	record := &regionalAsyncVideoKeyRecord{TaskKey: taskKey, State: asyncVideoDirectoryCompleted}
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT member,task_id,owner FROM regional_async_video_completed_tasks WHERE authority=$1 AND task_key=$2 AND expires_at>clock_timestamp()`, c.persistentUpstreamKeyAuthority(), taskKey).Scan(&record.Member, &record.TaskID, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if json.Unmarshal(raw, &record.Owner) != nil || !(video.Entry{TaskKey: taskKey, Member: record.Member, Owner: record.Owner}).Valid() || record.TaskID == "" || len(record.TaskID) > 256 || record.TaskID != strings.TrimSpace(record.TaskID) || video.TaskKey(record.Owner, record.TaskID) != taskKey {
		return nil, service.ErrUpstreamKeyPolicy
	}
	return record, nil
}

func (c *regionalConcurrencyCache) pruneCompletedAsyncVideoTasks(ctx context.Context) error {
	_, err := c.database().ExecContext(ctx, `WITH expired AS (SELECT authority,task_key FROM regional_async_video_completed_tasks WHERE authority=$1 AND expires_at<=clock_timestamp() ORDER BY expires_at LIMIT 256 FOR UPDATE SKIP LOCKED) DELETE FROM regional_async_video_completed_tasks t USING expired e WHERE t.authority=e.authority AND t.task_key=e.task_key`, c.persistentUpstreamKeyAuthority())
	return err
}
