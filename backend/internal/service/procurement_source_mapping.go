package service

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var (
	ErrProcurementSourceMappingInvalid     = infraerrors.BadRequest("INVALID_PROCUREMENT_SOURCE_MAPPING", "source mapping fields, platform, or local account/group relation are invalid")
	ErrProcurementSourceMappingConflict    = infraerrors.Conflict("PROCUREMENT_SOURCE_MAPPING_CONFLICT", "source mapping changed or conflicts with an existing route; reload before editing")
	ErrProcurementSourceMappingNotFound    = infraerrors.NotFound("PROCUREMENT_SOURCE_MAPPING_NOT_FOUND", "source mapping does not exist")
	ErrProcurementSourceMappingUnavailable = infraerrors.ServiceUnavailable("PROCUREMENT_SOURCE_MAPPING_UNAVAILABLE", "source mapping inventory is unavailable")
)

const (
	ProcurementSourceConclusionPending = "pending_verification"
	ProcurementSourceConclusionPaused  = "paused"
	ProcurementSourceConclusionBlocked = "blocked"
	ProcurementSourceConclusionStale   = "review_required"
)

// ProcurementSourceMapping is an administrator-only declaration and current
// identity summary. A current route fingerprint is not proof that business
// requests are pinned to the supplier's external group, so Dispatchable is
// deliberately false until a separate evidence-backed contract exists.
type ProcurementSourceMapping struct {
	ID                  int64     `json:"id"`
	SourceSiteID        int64     `json:"site_id"`
	SiteCode            string    `json:"site_code"`
	SiteName            string    `json:"site_name"`
	SiteRevision        int64     `json:"site_revision"`
	Platform            string    `json:"platform"`
	ExternalGroupID     string    `json:"external_group_id"`
	ExternalGroupName   string    `json:"external_group_name"`
	SourceGroupID       int64     `json:"source_group_id"`
	LocalGroupPlatform  string    `json:"local_group_platform"`
	LocalGroupID        int64     `json:"local_group_id"`
	LocalGroupName      string    `json:"local_group_name"`
	LocalGroupStatus    string    `json:"local_group_status"`
	LocalGroupDeleted   bool      `json:"local_group_deleted"`
	SourceGroupEnabled  bool      `json:"source_group_enabled"`
	SourceGroupRevision int64     `json:"source_group_revision"`
	AccountID           int64     `json:"account_id"`
	AccountName         string    `json:"account_name"`
	AccountPlatform     string    `json:"account_platform"`
	AccountStatus       string    `json:"account_status"`
	AccountDeleted      bool      `json:"account_deleted"`
	InLocalGroup        bool      `json:"in_local_group"`
	Enabled             bool      `json:"enabled"`
	Revision            int64     `json:"revision"`
	UpdatedAt           time.Time `json:"updated_at"`
	Conclusion          string    `json:"conclusion"`
	Reasons             []string  `json:"reasons"`
	RouteIdentity       string    `json:"route_identity"`
	SourceIdentity      string    `json:"source_identity"`
	FixedGroupSelection string    `json:"fixed_group_selection"`
	Dispatchable        bool      `json:"dispatchable"`

	Evidence *ProcurementSourceEvidence `json:"evidence"`
}

type ProcurementSourceMappingPage struct {
	Version    int                        `json:"version"`
	ObservedAt time.Time                  `json:"observed_at"`
	Items      []ProcurementSourceMapping `json:"items"`
	Total      int64                      `json:"total"`
	Page       int                        `json:"page"`
	PageSize   int                        `json:"page_size"`
}

type ProcurementSourceGroupState struct {
	ID       int64 `json:"id"`
	Enabled  bool  `json:"enabled"`
	Revision int64 `json:"revision"`
}

type ProcurementSourceSiteState struct {
	ID       int64  `json:"id"`
	Code     string `json:"code"`
	Name     string `json:"name"`
	Revision int64  `json:"revision"`
}

type ProcurementSourceMappingRepository interface {
	ListProcurementSourceMappings(context.Context, string, string, int, int) (*ProcurementSourceMappingPage, error)
	GetProcurementSourceMapping(context.Context, int64) (*ProcurementSourceMapping, error)
	CreateProcurementSourceMapping(context.Context, domain.ProcurementSourceMappingCommand) (*ProcurementSourceMapping, error)
	RenameProcurementSourceSite(context.Context, int64, domain.ProcurementSupplierSiteRenameCommand) (*ProcurementSourceSiteState, error)
	DeleteProcurementSourceSite(context.Context, int64, int64, bool) error
	SetProcurementSourceMappingEnabled(context.Context, int64, int64, bool, bool) (*ProcurementSourceMapping, error)
	SetProcurementSourceGroupEnabled(context.Context, int64, int64, bool, bool) (*ProcurementSourceGroupState, error)
	DeleteProcurementSourceGroup(context.Context, int64, int64, bool) error
	DeleteProcurementSourceMapping(context.Context, int64, int64, bool) error
}

func (s *ChannelService) ProcurementSourceMappingRepository() (ProcurementSourceMappingRepository, error) {
	if s != nil {
		if repo, ok := s.repo.(ProcurementSourceMappingRepository); ok {
			return repo, nil
		}
	}
	return nil, ErrProcurementSourceMappingUnavailable
}
