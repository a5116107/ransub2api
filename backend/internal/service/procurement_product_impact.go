package service

import (
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// Bounded, administrator-only price diagnostics. This is not full eligibility,
// a user-specific quote, a forecast of free slots, or a configuration command.
const ProcurementProductImpactAccountLimit = 1000

var ErrProcurementProductImpactTooLarge = infraerrors.BadRequest("PROCUREMENT_PRODUCT_IMPACT_TOO_LARGE", "product exceeds 1000 accounts; use explicit scope diagnostics")

type ProcurementProductImpact struct {
	Version            int                      `json:"version"`
	ObservedAt         time.Time                `json:"observed_at"`
	GroupStatus        string                   `json:"group_status"`
	GateEnabled        bool                     `json:"gate_enabled"`
	BoundAccounts      int                      `json:"bound_accounts"`
	ConsideredAccounts int                      `json:"considered_accounts"`
	UserOverrideCount  int                      `json:"user_override_count"`
	Report             ProfitPreviewGroupReport `json:"report"`
}
