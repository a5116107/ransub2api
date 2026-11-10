package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/regional/video"
)

const (
	videoPrimary = "primary"
	videoLocal   = "local"
)

// VideoTaskRecord recovers the original hold, including a completed event for
// exact replay after a lost acknowledgement. Loading never dispatches work.
type VideoTaskRecord struct {
	Reservation Reservation
	Price       Price
	TaskID      string
	State       string
	Event       *Event
}

// VideoTaskLedger extends the existing request ledger with durable provider
// identity. Bind is valid only after Start; unknown submissions retain the hold.
type VideoTaskLedger interface {
	RequestLedger
	BindVideoTask(context.Context, string, string) error
	RejectVideoTask(context.Context, string) error
	LoadVideoTask(context.Context, video.Owner, string) (VideoTaskRecord, error)
	ResolveVideoTaskOwner(context.Context, video.Owner, string) (*video.Owner, error)
}

// RejectVideoTask is only for a trusted adapter's explicit creation rejection.
// Unknown sends keep their hold. A bound task can never enter this refund path.
func (p *PrimaryRequests) RejectVideoTask(ctx context.Context, id string) error {
	return p.finish(ctx, id, nil, true)
}

func (l *Local) RejectVideoTask(ctx context.Context, id string) error {
	return l.finish(ctx, id, nil, true)
}

func verifyUnboundVideoRejection(ctx context.Context, tx *sql.Tx, side string, r Reservation) error {
	key, taskID, err := readVideoIdentity(ctx, tx, side, r)
	if err != nil {
		return err
	}
	if key != "" || taskID != "" {
		return ErrConflict
	}
	return nil
}

func reserveVideoIdentity(ctx context.Context, tx *sql.Tx, side string, r Reservation) error {
	if r.Video == nil {
		return nil
	}
	if err := r.validateVideoOrigin(); err != nil {
		return err
	}
	raw, err := json.Marshal(r.VideoOrigin)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO regional_video_financial_bindings(ledger,request_id,lease_id,region_id,member,origin) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, side, r.RequestID, r.LeaseID, r.RegionID, r.VideoOrigin.Member, string(raw))
	if err != nil {
		return err
	}
	_, _, err = readVideoIdentity(ctx, tx, side, r)
	return err
}

func readVideoIdentity(ctx context.Context, tx *sql.Tx, side string, r Reservation) (string, string, error) {
	if r.Video == nil || r.validateVideoOrigin() != nil {
		return "", "", ErrConflict
	}
	var raw []byte
	var member, lease, region, taskKey, taskID string
	err := tx.QueryRowContext(ctx, `SELECT origin,member,lease_id,region_id,COALESCE(task_key,''),COALESCE(task_id,'') FROM regional_video_financial_bindings WHERE ledger=$1 AND request_id=$2 FOR UPDATE`, side, r.RequestID).Scan(&raw, &member, &lease, &region, &taskKey, &taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrConflict
	}
	if err != nil {
		return "", "", err
	}
	var origin VideoOrigin
	if json.Unmarshal(raw, &origin) != nil || origin != *r.VideoOrigin || member != origin.Member || lease != r.LeaseID || region != r.RegionID {
		return "", "", ErrConflict
	}
	return taskKey, taskID, nil
}

func bindVideoIdentity(ctx context.Context, tx *sql.Tx, side string, r Reservation, taskID string) error {
	if !validVideoTaskID(taskID) {
		return ErrConflict
	}
	oldKey, oldID, err := readVideoIdentity(ctx, tx, side, r)
	if err != nil {
		return err
	}
	key := video.TaskKey(r.VideoOrigin.Owner, taskID)
	if oldKey != "" {
		if oldKey != key || oldID != taskID {
			return ErrConflict
		}
		return nil
	}
	_, err = tx.ExecContext(ctx, `UPDATE regional_video_financial_bindings SET task_key=$3,task_id=$4 WHERE ledger=$1 AND request_id=$2`, side, r.RequestID, key, taskID)
	var state interface{ SQLState() string }
	if errors.As(err, &state) && state.SQLState() == "23505" {
		return ErrConflict
	}
	return err
}

func verifyBoundVideoIdentity(ctx context.Context, tx *sql.Tx, side string, r Reservation, e Event) error {
	if r.Video == nil && e.Video == nil {
		return nil
	}
	if !videoMatchesReservation(r, e) {
		return ErrConflict
	}
	key, taskID, err := readVideoIdentity(ctx, tx, side, r)
	if err != nil {
		return err
	}
	if taskID != e.Video.TaskID || key != video.TaskKey(r.VideoOrigin.Owner, e.Video.TaskID) {
		return ErrConflict
	}
	return nil
}

type videoRequestLedger struct {
	db     *sql.DB
	region string
	local  bool
}

func (v videoRequestLedger) side() string {
	if v.local {
		return videoLocal
	}
	return videoPrimary
}

func (v videoRequestLedger) bind(ctx context.Context, id, taskID string) error {
	tx, err := v.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	query := `SELECT request,state FROM regional_primary_requests WHERE request_id=$1 FOR UPDATE`
	if v.local {
		// Match Local.Start/finish's lease -> request -> identity lock order.
		var lease string
		if err := tx.QueryRowContext(ctx, `SELECT lease_id FROM regional_local_requests WHERE request_id=$1`, id).Scan(&lease); err != nil {
			return err
		}
		if _, _, err := localLease(ctx, tx, lease, v.region); err != nil {
			return err
		}
		query = `SELECT request,state FROM regional_local_requests WHERE request_id=$1 FOR UPDATE`
	}
	var raw []byte
	var state string
	if err := tx.QueryRowContext(ctx, query, id).Scan(&raw, &state); err != nil {
		return err
	}
	if state != "started" && state != "completed" {
		return ErrAlreadyDispatched
	}
	var r Reservation
	if err := json.Unmarshal(raw, &r); err != nil {
		return err
	}
	if r.RequestID != id || (v.local && r.RegionID != v.region) {
		return ErrConflict
	}
	if err := bindVideoIdentity(ctx, tx, v.side(), r, taskID); err != nil {
		return err
	}
	return tx.Commit()
}

func (v videoRequestLedger) load(ctx context.Context, owner video.Owner, taskID string, exactRoute bool) (VideoTaskRecord, error) {
	var result VideoTaskRecord
	validOwner := owner.Valid()
	if !exactRoute {
		validOwner = owner.ValidPrincipal() && owner.AccountID == 0 && owner.RouteFingerprint == [32]byte{}
	}
	if !validOwner || !validVideoTaskID(taskID) {
		return result, ErrConflict
	}
	query := `SELECT b.request_id,b.lease_id,b.region_id,b.member,b.origin,b.task_id,r.request,r.state,r.event FROM regional_video_financial_bindings b JOIN regional_primary_requests r ON r.request_id=b.request_id WHERE b.ledger=$1 AND b.task_key=$2`
	if v.local {
		query = `SELECT b.request_id,b.lease_id,b.region_id,b.member,b.origin,b.task_id,r.request,r.state,o.event FROM regional_video_financial_bindings b JOIN regional_local_requests r ON r.request_id=b.request_id LEFT JOIN regional_usage_outbox o ON o.event_id=r.request_id WHERE b.ledger=$1 AND b.task_key=$2`
	}
	var id, lease, region, member string
	var origin, request, event []byte
	err := v.db.QueryRowContext(ctx, query, v.side(), video.TaskKey(owner, taskID)).Scan(&id, &lease, &region, &member, &origin, &result.TaskID, &request, &result.State, &event)
	if err != nil {
		return result, err
	}
	r := &result.Reservation
	var storedOrigin VideoOrigin
	if json.Unmarshal(request, r) != nil || json.Unmarshal(origin, &storedOrigin) != nil || r.Video == nil || r.validateVideoOrigin() != nil ||
		*r.VideoOrigin != storedOrigin || storedOrigin.Member != member || result.TaskID != taskID ||
		r.RequestID != id || r.LeaseID != lease || r.RegionID != region || (v.local && region != v.region) || (!v.local && (lease != "" || region != "")) ||
		(result.State != "started" && result.State != "completed") {
		return VideoTaskRecord{}, ErrConflict
	}
	requestedOwner := storedOrigin.Owner
	if !exactRoute {
		requestedOwner.AccountID, requestedOwner.RouteFingerprint = 0, [32]byte{}
	}
	if requestedOwner != owner {
		return VideoTaskRecord{}, ErrConflict
	}
	result.Price, err = readPrice(ctx, v.db, r.PricingVersion, r.Model)
	if err != nil {
		return VideoTaskRecord{}, err
	}
	maximum, err := result.Price.VideoMaximum(*r.Video)
	if err != nil || maximum != r.Maximum {
		return VideoTaskRecord{}, ErrConflict
	}
	if result.State == "completed" {
		result.Event = new(Event)
		if len(event) == 0 || json.Unmarshal(event, result.Event) != nil || !videoMatchesReservation(*r, *result.Event) || result.Event.validateVideo() != nil {
			return VideoTaskRecord{}, ErrConflict
		}
		e := result.Event
		u := e.Usage
		if u.EventID != id || u.LeaseID != lease || u.RegionID != region || u.UserID != r.UserID || u.APIKeyID != r.APIKeyID || u.AccountID != r.AccountID ||
			!u.AdmittedAt.Equal(r.AdmittedAt) || u.CompletedAt.Before(u.AdmittedAt) || u.Charge < 0 || u.Charge > r.Maximum ||
			e.Model != r.Model || e.PricingVersion != r.PricingVersion || e.PayloadSHA256 != r.PayloadSHA256 || e.Video.TaskID != taskID {
			return VideoTaskRecord{}, ErrConflict
		}
		_, charge, err := result.Price.VideoCosts(*e.Video)
		if err != nil || charge != u.Charge {
			return VideoTaskRecord{}, ErrConflict
		}
	}
	return result, nil
}

func (l *Local) BindVideoTask(ctx context.Context, id, taskID string) error {
	return (videoRequestLedger{db: l.DB, region: l.Region, local: true}).bind(ctx, id, taskID)
}

func (p *PrimaryRequests) BindVideoTask(ctx context.Context, id, taskID string) error {
	return (videoRequestLedger{db: p.DB}).bind(ctx, id, taskID)
}

func (l *Local) LoadVideoTask(ctx context.Context, owner video.Owner, taskID string) (VideoTaskRecord, error) {
	return (videoRequestLedger{db: l.DB, region: l.Region, local: true}).load(ctx, owner, taskID, true)
}

func (p *PrimaryRequests) LoadVideoTask(ctx context.Context, owner video.Owner, taskID string) (VideoTaskRecord, error) {
	return (videoRequestLedger{db: p.DB}).load(ctx, owner, taskID, true)
}

// Resolving returns evidence of the original route, never a new assignment or
// send authorization. Only local requests are joined; reconciled remote events
// in the primary namespace do not make the primary their execution owner.
func (v videoRequestLedger) resolveOwner(ctx context.Context, principal video.Owner, taskID string) (*video.Owner, error) {
	record, err := v.load(ctx, principal, taskID, false)
	if err != nil {
		return nil, err
	}
	owner := record.Reservation.VideoOrigin.Owner
	return &owner, nil
}

func (l *Local) ResolveVideoTaskOwner(ctx context.Context, principal video.Owner, taskID string) (*video.Owner, error) {
	return (videoRequestLedger{db: l.DB, region: l.Region, local: true}).resolveOwner(ctx, principal, taskID)
}

func (p *PrimaryRequests) ResolveVideoTaskOwner(ctx context.Context, principal video.Owner, taskID string) (*video.Owner, error) {
	return (videoRequestLedger{db: p.DB}).resolveOwner(ctx, principal, taskID)
}
