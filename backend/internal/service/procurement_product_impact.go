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
	Version            int                          `json:"version"`
	ObservedAt         time.Time                    `json:"observed_at"`
	GroupStatus        string                       `json:"group_status"`
	GateEnabled        bool                         `json:"gate_enabled"`
	BoundAccounts      int                          `json:"bound_accounts"`
	ConsideredAccounts int                          `json:"considered_accounts"`
	UserOverrideCount  int                          `json:"user_override_count"`
	Report             ProfitPreviewGroupReport     `json:"report"`
	ManagementReview   *ProcurementManagementReview `json:"management_review,omitempty"`
}

// This is a read-only legacy-rule review, not consent to take over a product.
// In particular, absence of conflicts never establishes full eligibility.
type ProcurementManagementReview struct {
	Version              int                         `json:"version"`
	Mode                 string                      `json:"mode"`
	Scope                string                      `json:"scope"`
	InventoryComplete    bool                        `json:"inventory_complete"`
	RulesExamined        int                         `json:"rules_examined"`
	ConflictingRules     int                         `json:"conflicting_rules"`
	ProtectionRules      int                         `json:"protection_rules"`
	UnknownRules         int                         `json:"unknown_rules"`
	JudgeDependencyRules int                         `json:"judge_dependency_rules"`
	Rules                []ProcurementManagementRule `json:"rules"`
	Budget               ProcurementManagementBudget `json:"budget"`
	SourceInventory      *ProcurementSourceInventory `json:"source_inventory,omitempty"`
}

// Saved approval is permission to consider supply, never a statement that its
// model, health, cache or current capacity has been verified. Unknown approvals
// keep null counts instead of manufacturing an empty or unrestricted pool.
type ProcurementSourceInventory struct {
	Version                 int                        `json:"version"`
	State                   string                     `json:"state"`
	PolicyRevision          int64                      `json:"policy_revision"`
	ApprovedAccounts        *int                       `json:"approved_accounts"`
	UnboundApprovedAccounts *int                       `json:"unbound_approved_accounts"`
	Groups                  []ProcurementSourceGroup   `json:"groups"`
	Accounts                []ProcurementSourceAccount `json:"accounts"`
}

type ProcurementSourceGroup struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
}

type ProcurementSourceAccount struct {
	AccountID         int64  `json:"account_id"`
	Name              string `json:"name"`
	Platform          string `json:"platform"`
	Status            string `json:"status"`
	Schedulable       bool   `json:"schedulable"`
	InProduct         bool   `json:"in_product"`
	InApprovedSources *bool  `json:"in_approved_sources"`
}

type ProcurementManagementRule struct {
	PlanID                int64  `json:"plan_id"`
	AccountID             int64  `json:"account_id"`
	AccountName           string `json:"account_name"`
	Enabled               bool   `json:"enabled"`
	Action                string `json:"action"`
	Risk                  string `json:"risk"`
	WritesProduct         bool   `json:"writes_product"`
	RetainedOwnership     bool   `json:"retained_ownership"`
	SavedProductOwnership bool   `json:"saved_product_ownership"`
	JudgeDependsOnProduct bool   `json:"judge_depends_on_product"`
}

// The original compiler's saved global inventory, not incremental demand for
// this product, a token estimate, or a capacity reservation.
type ProcurementManagementBudget struct {
	State                 string    `json:"state"`
	Revision              int64     `json:"revision"`
	Enforced              bool      `json:"enforced"`
	InventoryComplete     bool      `json:"inventory_complete"`
	MinimumRequestsPerDay int64     `json:"minimum_requests_per_day"`
	RegularDayCapacity    int64     `json:"regular_day_capacity"`
	UnestimatedSources    int       `json:"unestimated_sources"`
	Reasons               []string  `json:"reasons"`
	Warnings              []string  `json:"warnings"`
	WindowStart           time.Time `json:"window_start"`
	WindowEnd             time.Time `json:"window_end"`
}
