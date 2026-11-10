package ledger

import (
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/regional"
	"github.com/Wei-Shaw/sub2api/internal/regional/video"
)

// Reservation is immutable request identity, persisted BEFORE upstream work.
// The request/event ID is globally unique. A repeated ID never dispatches again.
type Reservation struct {
	RequestID      string          `json:"request_id"`
	LeaseID        string          `json:"lease_id"`
	RegionID       string          `json:"region_id"`
	UserID         int64           `json:"user_id"`
	APIKeyID       int64           `json:"api_key_id"`
	AccountID      int64           `json:"account_id"`
	AdmittedAt     time.Time       `json:"admitted_at"`
	Maximum        regional.Amount `json:"maximum_nano_usd"`
	PayloadSHA256  string          `json:"payload_sha256"`
	PricingVersion string          `json:"pricing_version"`
	Model          string          `json:"model"`
	Video          *VideoRequest   `json:"video,omitempty"`
	VideoOrigin    *VideoOrigin    `json:"video_origin,omitempty"`
}

// VideoOrigin freezes the submission member and authenticated route before
// dispatch. It survives slot release and never contains provider credentials.
type VideoOrigin struct {
	Member string      `json:"member"`
	Owner  video.Owner `json:"owner"`
}

func (o VideoOrigin) valid(user, key, account int64, provider string) bool {
	return video.ValidMember(o.Member) && o.Owner.Valid() && o.Owner.UserID == user &&
		o.Owner.APIKeyID == key && o.Owner.AccountID == account && o.Owner.Provider == provider
}

// Event retains raw measured usage and immutable pricing identity for primary
// reconciliation. No prompts, credentials or model output are persisted here.
type Event struct {
	Usage               regional.UsageEvent `json:"usage"`
	Model               string              `json:"model"`
	PricingVersion      string              `json:"pricing_version"`
	PayloadSHA256       string              `json:"payload_sha256"`
	InputTokens         int64               `json:"input_tokens"`
	OutputTokens        int64               `json:"output_tokens"`
	CacheCreationTokens int64               `json:"cache_creation_tokens"`
	CacheReadTokens     int64               `json:"cache_read_tokens"`
	Video               *VideoUsage         `json:"video,omitempty"`
	VideoOrigin         *VideoOrigin        `json:"video_origin,omitempty"`
}

func (r Reservation) Validate(l regional.Lease) error {
	e := regional.UsageEvent{SchemaVersion: regional.SchemaVersion, EventID: r.RequestID, LeaseID: r.LeaseID, RegionID: r.RegionID, UserID: r.UserID, APIKeyID: r.APIKeyID, AccountID: r.AccountID, AdmittedAt: r.AdmittedAt, CompletedAt: r.AdmittedAt, Charge: r.Maximum}
	if err := e.ValidateAgainst(l); err != nil {
		return err
	}
	if r.Maximum < 0 || r.Model == "" || r.PricingVersion == "" || !validDigest(r.PayloadSHA256) {
		return errors.New("reservation requires bounded price and payload identity")
	}
	if r.Video != nil {
		if !l.AllowsVideoPrice(r.PricingVersion) || r.VideoOrigin == nil || r.VideoOrigin.Owner.GroupID != l.GroupID {
			return ErrConflict
		}
		if err := r.Video.validate(); err != nil {
			return err
		}
	}
	if err := r.validateVideoOrigin(); err != nil {
		return err
	}
	_, err := USD(r.Maximum)
	return err
}
func validDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
func (e Event) Validate(l regional.Lease) error {
	if err := e.Usage.ValidateAgainst(l); err != nil {
		return err
	}
	if e.Model == "" || e.PricingVersion == "" || !validDigest(e.PayloadSHA256) || e.InputTokens < 0 || e.OutputTokens < 0 || e.CacheCreationTokens < 0 || e.CacheReadTokens < 0 {
		return errors.New("invalid regional usage evidence")
	}
	if err := e.validateVideo(); err != nil {
		return err
	}
	if e.Video != nil && (!l.AllowsVideoPrice(e.PricingVersion) || e.VideoOrigin.Owner.GroupID != l.GroupID) {
		return ErrConflict
	}
	_, err := USD(e.Usage.Charge)
	return err
}

func (e Event) validateVideo() error {
	if e.Video == nil {
		if e.VideoOrigin != nil {
			return ErrConflict
		}
		return nil
	}
	if e.VideoOrigin == nil || !e.VideoOrigin.valid(e.Usage.UserID, e.Usage.APIKeyID, e.Usage.AccountID, e.Video.Provider) {
		return ErrConflict
	}
	if err := e.Video.validate(); err != nil {
		return err
	}
	if e.InputTokens != 0 || e.CacheCreationTokens != 0 || e.CacheReadTokens != 0 || e.OutputTokens != e.Video.CompletionTokens {
		return ErrConflict
	}
	return nil
}

func (r Reservation) validateVideoOrigin() error {
	if r.Video == nil {
		if r.VideoOrigin != nil {
			return ErrConflict
		}
		return nil
	}
	if r.VideoOrigin == nil || !r.VideoOrigin.valid(r.UserID, r.APIKeyID, r.AccountID, r.Video.Provider) {
		return ErrConflict
	}
	return nil
}

func videoMatchesReservation(r Reservation, e Event) bool {
	if r.Video == nil || e.Video == nil {
		return r.Video == nil && e.Video == nil && r.VideoOrigin == nil && e.VideoOrigin == nil
	}
	return *r.Video == e.Video.VideoRequest && r.VideoOrigin != nil && e.VideoOrigin != nil && *r.VideoOrigin == *e.VideoOrigin
}
