package service

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/tidwall/gjson"
)

var (
	ErrProcurementManagementInvalid     = infraerrors.BadRequest("PROCUREMENT_MANAGEMENT_INVALID", "invalid review authorization")
	ErrProcurementManagementConflict    = infraerrors.Conflict("PROCUREMENT_MANAGEMENT_CONFLICT", "review changed or expired; preview again")
	ErrProcurementManagementUnavailable = infraerrors.ServiceUnavailable("PROCUREMENT_MANAGEMENT_UNAVAILABLE", "product management could not be checked")
)

const ProcurementManagementPreviewTTL = 2 * time.Minute

type ProcurementManagementState struct {
	Version           int    `json:"version"`
	GroupID           int64  `json:"group_id"`
	Revision          int64  `json:"revision"`
	Mode              string `json:"mode"`
	CurrentOwned      int    `json:"current_owned"`
	RetainedOwned     int    `json:"retained_owned"`
	ManualMemberships int    `json:"manual_memberships"`
}

type ProcurementMembershipReview struct {
	AccountID   int64                         `json:"account_id"`
	AccountName string                        `json:"account_name"`
	Ownership   *domain.AccountGroupOwnership `json:"ownership"`
	Claimable   bool                          `json:"claimable"`
	Reasons     []string                      `json:"reasons"`
	Protections []string                      `json:"protections"`
}

type ProcurementManagementPreview struct {
	Version     int                           `json:"version"`
	State       *ProcurementManagementState   `json:"state"`
	ObservedAt  time.Time                     `json:"observed_at"`
	ExpiresAt   time.Time                     `json:"expires_at"`
	Fingerprint string                        `json:"fingerprint"`
	CanConfirm  bool                          `json:"can_confirm"`
	Memberships []ProcurementMembershipReview `json:"memberships"`
	Review      *ProcurementManagementReview  `json:"review"`
}

type ProcurementManagementCommand struct {
	Version          int       `json:"version"`
	Mode             string    `json:"mode"`
	ExpectedRevision int64     `json:"expected_revision"`
	AccountIDs       []int64   `json:"account_ids,omitempty"`
	ObservedAt       time.Time `json:"observed_at,omitempty"`
	Fingerprint      string    `json:"fingerprint,omitempty"`
}

type ProcurementManagementRepository interface {
	ReadProcurementManagement(context.Context, int64) (*ProcurementManagementState, error)
	PreviewProcurementManagement(context.Context, int64, []int64) (*ProcurementManagementPreview, error)
	SaveProcurementManagement(context.Context, int64, *ProcurementManagementCommand) (*ProcurementManagementState, error)
}

func (s *ChannelService) ProcurementManagementRepository() (ProcurementManagementRepository, error) {
	if s != nil {
		if r, ok := s.repo.(ProcurementManagementRepository); ok {
			return r, nil
		}
	}
	return nil, ErrProcurementManagementUnavailable
}

func ValidProcurementManagementIDs(ids []int64) bool {
	if len(ids) == 0 || len(ids) > ProcurementProductImpactAccountLimit {
		return false
	}
	seen := map[int64]bool{}
	for _, id := range ids {
		if id < 1 || id > domain.MaxProcurementPolicyRevision || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

func ParseProcurementManagementPreview(raw []byte) ([]int64, error) {
	if !gjson.ValidBytes(raw) || !procurementCatalogObject(gjson.ParseBytes(raw), "account_ids") {
		return nil, ErrProcurementManagementInvalid
	}
	var command struct {
		AccountIDs []int64 `json:"account_ids"`
	}
	if json.Unmarshal(raw, &command) != nil || !ValidProcurementManagementIDs(command.AccountIDs) {
		return nil, ErrProcurementManagementInvalid
	}
	slices.Sort(command.AccountIDs)
	return command.AccountIDs, nil
}

func ParseProcurementManagementCommand(raw []byte) (*ProcurementManagementCommand, error) {
	var cmd ProcurementManagementCommand
	if !gjson.ValidBytes(raw) || gjson.GetBytes(raw, "expected_revision").Type != gjson.Number || json.Unmarshal(raw, &cmd) != nil || cmd.Validate() != nil {
		return nil, ErrProcurementManagementInvalid
	}
	names := []string{"version", "mode", "expected_revision"}
	if cmd.Mode == "review" {
		names = append(names, "account_ids", "observed_at", "fingerprint")
	}
	if !procurementCatalogObject(gjson.ParseBytes(raw), names...) {
		return nil, ErrProcurementManagementInvalid
	}
	slices.Sort(cmd.AccountIDs)
	return &cmd, nil
}

func (c *ProcurementManagementCommand) Validate() error {
	if c == nil || c.Version != 1 || c.ExpectedRevision < 0 || c.ExpectedRevision >= domain.MaxProcurementPolicyRevision {
		return ErrProcurementManagementInvalid
	}
	switch c.Mode {
	case "manual":
		if len(c.AccountIDs) != 0 || c.Fingerprint != "" || !c.ObservedAt.IsZero() {
			return ErrProcurementManagementInvalid
		}
	case "review":
		if !ValidProcurementManagementIDs(c.AccountIDs) || c.ObservedAt.IsZero() || len(c.Fingerprint) != 64 {
			return ErrProcurementManagementInvalid
		}
		for _, ch := range c.Fingerprint {
			if !('0' <= ch && ch <= '9' || 'a' <= ch && ch <= 'f') {
				return ErrProcurementManagementInvalid
			}
		}
	default:
		return ErrProcurementManagementInvalid
	}
	return nil
}
