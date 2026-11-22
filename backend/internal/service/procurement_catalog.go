package service

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/tidwall/gjson"
)

const ProcurementCatalogSettingKey = "upstream_procurement_catalog"

var (
	ErrProcurementCatalogInvalid     = infraerrors.BadRequest("INVALID_PROCUREMENT_CATALOG", "invalid procurement catalog or explicit request scopes")
	ErrProcurementCatalogConflict    = infraerrors.Conflict("PROCUREMENT_CATALOG_CONFLICT", "procurement catalog changed; review the latest version before saving")
	ErrProcurementCatalogUnavailable = infraerrors.ServiceUnavailable("PROCUREMENT_CATALOG_UNAVAILABLE", "procurement catalog unavailable; retry without replacing the saved configuration")
)

type ProcurementCatalogSnapshot struct {
	Catalog *domain.ProcurementCatalog `json:"catalog"`
	Gaps    []domain.ProcurementGap    `json:"gaps"`
}

type ProcurementScopeClassification struct {
	ProfitPreviewRequestScope
	State         string                     `json:"state"`
	Reason        string                     `json:"reason,omitempty"`
	Applicability string                     `json:"applicability"`
	RateSource    string                     `json:"rate_source"`
	EffectiveRate *string                    `json:"effective_rate"`
	Bucket        *domain.ProcurementBucket  `json:"bucket"`
	Qualification string                     `json:"qualification"`
	Quality       *QualityScopeEvidence      `json:"quality_evidence,omitempty"`
	Availability  *AvailabilityScopeEvidence `json:"availability_evidence,omitempty"`
	Product       *ProcurementProductPreview `json:"product_procurement,omitempty"`
}

type ProcurementPreviewCommand struct {
	Requests          []ProfitPreviewRequestScope `json:"requests"`
	EvaluateCandidate bool                        `json:"evaluate_candidate,omitempty"`
	ProductGroupID    *int64                      `json:"product_group_id,omitempty"`
}

// This is a read-only projection at the group's default sale price, never a
// user-specific price quote or a scheduling/quality/cache qualification.
type ProcurementCandidatePreview struct {
	InProduct   bool   `json:"in_product"`
	SourceState string `json:"source_state"`
}

type ProcurementProductPreview struct {
	Candidate       *ProcurementCandidatePreview       `json:"candidate,omitempty"`
	PriceValidUntil *time.Time                         `json:"price_valid_until"`
	AccountControls *ProcurementAccountControlsPreview `json:"account_controls"`
	EvaluatedGates  *ProcurementEvaluatedGatesPreview  `json:"evaluated_gates,omitempty"`
	AnswerQuality   *ProcurementAnswerQualityPreview   `json:"answer_quality,omitempty"`
	Availability    *ProcurementAvailabilityPreview    `json:"availability,omitempty"`
	Version         int                                `json:"version"`
	GroupID         int64                              `json:"group_id"`
	PolicyRevision  int64                              `json:"policy_revision"`
	CatalogRevision *int64                             `json:"catalog_revision,omitempty"`
	PriceBasis      string                             `json:"price_basis"`
	DefaultD        float64                            `json:"default_d"`
	Threshold       float64                            `json:"threshold"`
	State           string                             `json:"state"`
	PriceClass      string                             `json:"price_class"`
	Reason          string                             `json:"reason,omitempty"`
}

// Only the explicitly evaluated product constraints are intersected here.
// This snapshot is never full model/cache qualification or send authorization.
type ProcurementEvaluatedGatesPreview struct {
	Version       int                               `json:"version"`
	State         string                            `json:"state"`
	Reason        string                            `json:"reason"`
	PassedGates   int                               `json:"passed_gates"`
	RequiredGates int                               `json:"required_gates"`
	EvaluatedAt   time.Time                         `json:"evaluated_at"`
	ValidUntil    *time.Time                        `json:"valid_until"`
	Gates         []ProcurementEvaluatedGatePreview `json:"gates"`
}

// Only required, configured gates are present, in the original evaluation
// order. Each deadline is copied from its source, never extended by the summary.
type ProcurementEvaluatedGatePreview struct {
	Key        string     `json:"key"`
	State      string     `json:"state"`
	Reason     string     `json:"reason"`
	ValidUntil *time.Time `json:"valid_until"`
}

const ProcurementPreviewTTL = 30 * time.Second

// A short-lived snapshot of the original account controls, not a capacity
// reservation, parent credential check or complete product qualification.
type ProcurementAccountControlsPreview struct {
	Version     int        `json:"version"`
	State       string     `json:"state"`
	Reason      string     `json:"reason"`
	EvaluatedAt time.Time  `json:"evaluated_at"`
	ValidUntil  *time.Time `json:"valid_until"`
}

func procurementAccountControlsAt(account *Account, id int64, at time.Time) *ProcurementAccountControlsPreview {
	out := &ProcurementAccountControlsPreview{Version: 1, State: "unknown", Reason: "account_unavailable", EvaluatedAt: at}
	if account == nil || account.ID != id || at.IsZero() {
		return out
	}
	if !slices.Contains([]string{StatusActive, StatusDisabled, StatusError}, account.Status) {
		out.Reason = "account_state_unconfirmed"
		return out
	}
	out.State, out.Reason = "pass", "account_controls_passed"
	if reason := account.schedulingBlockReasonAt(at); reason != "" {
		out.State, out.Reason = "fail", reason
	}
	until := at.Add(ProcurementPreviewTTL)
	shorten := func(boundary *time.Time) {
		if boundary != nil && boundary.After(at) && boundary.Before(until) {
			until = *boundary
		}
	}
	if account.AutoPauseOnExpired {
		shorten(account.ExpiresAt)
	}
	shorten(account.OverloadUntil)
	shorten(account.RateLimitResetAt)
	shorten(account.TempUnschedulableUntil)
	if account.SupportsAccountQuota() {
		tz, err := time.LoadLocation(account.GetQuotaResetTimezone())
		if err != nil {
			tz = time.UTC
		}
		if account.GetQuotaDailyLimit() > 0 {
			boundary := account.getExtraTime("quota_daily_start").Add(24 * time.Hour)
			if account.GetQuotaDailyResetMode() == "fixed" {
				boundary = nextFixedDailyReset(account.GetQuotaDailyResetHour(), tz, at)
			}
			shorten(&boundary)
		}
		if account.GetQuotaWeeklyLimit() > 0 {
			boundary := account.getExtraTime("quota_weekly_start").Add(7 * 24 * time.Hour)
			if account.GetQuotaWeeklyResetMode() == "fixed" {
				boundary = nextFixedWeeklyReset(account.GetQuotaWeeklyResetDay(), account.GetQuotaWeeklyResetHour(), tz, at)
			}
			shorten(&boundary)
		}
	}
	out.ValidUntil = &until
	return out
}

func (p *ProcurementProductPreview) priceState() string {
	switch {
	case p.State == "constraints_passed":
		return "pass"
	case p.State == "product_inactive", p.State == "not_associated":
		return "fail"
	case p.State == "blocked" && slices.Contains([]string{ProfitPreviewClassRejectedThreshold, ProfitPreviewClassRejectedProcurement, ProfitPreviewClassRejectedSource, ProfitPreviewClassRejectedBucket, ProfitPreviewClassRejectedSaleRate}, p.PriceClass):
		return "fail"
	default:
		return "unknown"
	}
}

// Bound the already-evaluated original price gate, without calculating a second
// price. Configuration changes still require a new read/final-send decision.
func procurementPriceDeadlineAt(account *Account, product *Group, at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	until := at.Add(ProcurementPreviewTTL)
	shorten := func(boundary time.Time) {
		if boundary.After(at) && boundary.Before(until) {
			until = boundary
		}
	}
	minuteBoundary := func(location *time.Location) {
		local := at.In(location)
		// Peak rules have minute precision. Do not predict beyond the next
		// local minute or timezone transition, even if the price stays equal.
		shorten(at.Add(time.Minute - time.Duration(local.Second())*time.Second - time.Duration(local.Nanosecond())))
		_, zoneEnd := local.ZoneBounds()
		shorten(zoneEnd)
	}
	if product != nil && product.IsSubscriptionType() && product.PeakRateEnabled {
		minuteBoundary(timezone.Location())
	}
	if account != nil && account.Type == AccountTypeAPIKey {
		present := false
		if upstreamBillingRateSyncEnabled(account) {
			if snapshot := decodeUpstreamBillingProbeSnapshot(account.Extra); snapshot != nil {
				if snapshot.FreshUntil != nil {
					shorten(*snapshot.FreshUntil)
				}
				declaration, exists := upstreamBillingApplicabilityFromData(snapshot.Data)
				present = exists
				if declaration != nil {
					shorten(declaration.ExpiresAt)
				}
				if peak, _ := snapshot.Data["peak_rate_enabled"].(bool); peak {
					name, _ := snapshot.Data["timezone"].(string)
					if location, err := time.LoadLocation(name); err == nil {
						minuteBoundary(location)
					}
				}
			}
		}
		// An explicit supplier declaration, including an exclusion or invalid
		// one, never inherits the administrator's fallback declaration.
		if !present {
			if manual, state := manualBillingApplicabilityAt(account, at); state == "active" {
				shorten(manual.ExpiresAt)
			}
		}
	}
	return &until
}

func (p *ProcurementProductPreview) AssessEvidence(policy *ProcurementPolicy, quality *QualityScopeEvidence, availability *AvailabilityScopeEvidence, at time.Time) {
	if p == nil {
		return
	}
	p.AnswerQuality = ProcurementAnswerQuality(policy, quality, at)
	p.Availability = ProcurementAvailability(policy, availability, at)
	out := &ProcurementEvaluatedGatesPreview{Version: 4, State: "pass", Reason: "all_evaluated_gates_passed", EvaluatedAt: at, Gates: make([]ProcurementEvaluatedGatePreview, 0, 4)}
	add := func(key, state, reason string, until *time.Time) {
		gate := ProcurementEvaluatedGatePreview{Key: key, State: state, Reason: reason}
		if (state == "pass" || state == "fail") && until != nil && until.After(at) {
			deadline := *until
			gate.ValidUntil = &deadline
		}
		out.Gates = append(out.Gates, gate)
	}
	price := p.priceState()
	switch {
	case p.PriceValidUntil == nil || !p.PriceValidUntil.After(at) || p.PriceValidUntil.After(at.Add(ProcurementPreviewTTL)):
		add("price", "unknown", "price_constraints_unavailable", nil)
	case price == "pass":
		add("price", "pass", "price_constraints_passed", p.PriceValidUntil)
	case price == "fail":
		add("price", "fail", "price_constraints_failed", p.PriceValidUntil)
	default:
		add("price", "unknown", "price_constraints_unavailable", nil)
	}
	controls := p.AccountControls
	if controls == nil || controls.Version != 1 || !controls.EvaluatedAt.Equal(at) || controls.ValidUntil == nil || !controls.ValidUntil.After(at) {
		add("account_controls", "unknown", "account_controls_unavailable", nil)
	} else if controls.State == "fail" {
		add("account_controls", "fail", "account_controls_failed", controls.ValidUntil)
	} else if controls.State == "pass" && controls.Reason == "account_controls_passed" {
		add("account_controls", "pass", "account_controls_passed", controls.ValidUntil)
	} else {
		add("account_controls", "unknown", "account_controls_unavailable", nil)
	}
	if p.AnswerQuality.Required {
		reason := "answer_quality_unavailable"
		if p.AnswerQuality.State == "fail" {
			reason = "answer_quality_failed"
		} else if p.AnswerQuality.State == "pass" {
			reason = "answer_quality_passed"
		}
		var until *time.Time
		if quality != nil {
			until = quality.ValidUntil
		}
		add("answer_quality", p.AnswerQuality.State, reason, until)
	}
	if p.Availability.Required {
		reason := "availability_unavailable"
		if p.Availability.State == "fail" {
			reason = "availability_failed"
		} else if p.Availability.State == "pass" {
			reason = "availability_passed"
		}
		var until *time.Time
		if availability != nil {
			until = availability.ValidUntil
		}
		add("availability", p.Availability.State, reason, until)
	}
	// The explanation and aggregate have one decision source, including every
	// simultaneous failure/unknown and the original first-failure precedence.
	out.RequiredGates = len(out.Gates)
	for _, gate := range out.Gates {
		if gate.State == "pass" {
			out.PassedGates++
		} else if out.State == "pass" || gate.State == "fail" && out.State != "fail" {
			out.State, out.Reason = gate.State, gate.Reason
		}
		if gate.ValidUntil != nil && (out.ValidUntil == nil || gate.ValidUntil.Before(*out.ValidUntil)) {
			deadline := *gate.ValidUntil
			out.ValidUntil = &deadline
		}
	}
	p.EvaluatedGates = out
}

type ProcurementClassificationPreview struct {
	ProcurementCatalogSnapshot
	ObservedAt time.Time                        `json:"observed_at"`
	Results    []ProcurementScopeClassification `json:"results"`
}

type ProcurementCatalogRepository interface {
	ReadProcurementCatalog(context.Context) (*domain.ProcurementCatalog, error)
	SaveProcurementCatalog(context.Context, *domain.ProcurementCatalog) (*domain.ProcurementCatalog, error)
	PreviewProcurementClassification(context.Context, *ProcurementPreviewCommand) (*ProcurementClassificationPreview, error)
	PreviewProcurementProductImpact(context.Context, int64, ...int64) (*ProcurementProductImpact, error)
}

func (s *ChannelService) ProcurementCatalogRepository() (ProcurementCatalogRepository, error) {
	if s != nil {
		if r, ok := s.repo.(ProcurementCatalogRepository); ok {
			return r, nil
		}
	}
	return nil, ErrProcurementCatalogUnavailable
}

func procurementUniqueObject(value gjson.Result) (map[string]gjson.Result, bool) {
	fields, valid := availabilityJSONObject(value)
	count := 0
	value.ForEach(func(_, _ gjson.Result) bool { count++; return true })
	// Wire parsers allow identical repeated fields for compatibility. A full
	// configuration command must instead reject every duplicate, even equal.
	return fields, valid && count == len(fields)
}

func procurementCatalogObject(value gjson.Result, names ...string) bool {
	fields, valid := procurementUniqueObject(value)
	if !valid || len(fields) != len(names) {
		return false
	}
	for _, name := range names {
		if _, ok := fields[name]; !ok {
			return false
		}
	}
	return true
}

func ParseProcurementCatalogCommand(raw []byte) (*domain.ProcurementCatalog, error) {
	var catalog domain.ProcurementCatalog
	if len(raw) > 65536 || json.Unmarshal(raw, &catalog) != nil || catalog.ValidateDraft() != nil {
		return nil, ErrProcurementCatalogInvalid
	}
	return &catalog, nil
}

func ParseProcurementPreviewCommand(raw []byte) (*ProcurementPreviewCommand, error) {
	value := gjson.ParseBytes(raw)
	fields, valid := procurementUniqueObject(value)
	if len(raw) > 65536 || !gjson.ValidBytes(raw) || !valid || (len(fields) < 1 || len(fields) > 3) || !value.Get("requests").IsArray() {
		return nil, ErrProcurementCatalogInvalid
	}
	command := &ProcurementPreviewCommand{}
	if field, present := fields["product_group_id"]; present {
		var id int64
		if json.Unmarshal([]byte(field.Raw), &id) != nil || id < 1 || id > domain.MaxProcurementPolicyRevision {
			return nil, ErrProcurementCatalogInvalid
		}
		command.ProductGroupID = &id
	}
	known := 1
	if command.ProductGroupID != nil {
		known++
	}
	if field, present := fields["evaluate_candidate"]; present {
		if field.Type != gjson.True && field.Type != gjson.False {
			return nil, ErrProcurementCatalogInvalid
		}
		command.EvaluateCandidate = field.Bool()
		known++
	}
	if len(fields) != known || command.EvaluateCandidate && command.ProductGroupID == nil {
		return nil, ErrProcurementCatalogInvalid
	}
	items := value.Get("requests").Array()
	if len(items) == 0 || len(items) > 100 {
		return nil, ErrProcurementCatalogInvalid
	}
	requests := make([]ProfitPreviewRequestScope, 0, len(items))
	for _, item := range items {
		if !procurementCatalogObject(item, "account_id", "public_model", "scope") {
			return nil, ErrProcurementCatalogInvalid
		}
		if _, valid := procurementUniqueObject(item.Get("scope")); !valid {
			return nil, ErrProcurementCatalogInvalid
		}
		request, err := ParseProfitPreviewRequestScope([]byte(item.Raw))
		if err != nil || request.AccountID < 1 || request.AccountID > domain.MaxProcurementPolicyRevision || !billingDeclarationToken(request.PublicModel, 200) {
			return nil, ErrProcurementCatalogInvalid
		}
		requests = append(requests, request)
	}
	command.Requests = requests
	return command, nil
}

// ClassifyProcurementScope consumes the original U_gate and exact applicability
// contracts. A catalog match is never a quality/cache or product qualification.
func ClassifyProcurementScope(catalog *domain.ProcurementCatalog, account *Account, request ProfitPreviewRequestScope, at time.Time, product *Group, evaluateCandidate bool) ProcurementScopeClassification {
	result := ProcurementScopeClassification{ProfitPreviewRequestScope: request, State: "scope_unconfirmed", Applicability: "unknown", RateSource: "unknown", Qualification: "not_evaluated"}
	if product != nil {
		productView := *product
		productView.ProcurementCatalog = catalog.Clone()
		report := PreviewProfitAdmission([]ProfitPreviewGroupInput{{Group: &productView, Accounts: []*Account{account}, RequestScopes: []ProfitPreviewRequestScope{request}}}, at)[0]
		verdict := report.ScopeVerdicts[0]
		projection := &ProcurementProductPreview{Version: 1, GroupID: product.ID, PriceBasis: "group_default", DefaultD: report.DefaultD, Threshold: report.ThresholdDefault, State: "not_evaluated", PriceClass: verdict.PriceClass, Reason: verdict.Reason}
		projection.AccountControls = procurementAccountControlsAt(account, request.AccountID, at)
		projection.CatalogRevision = report.ProcurementCatalogRevision
		if product.ProcurementPolicy != nil {
			projection.PolicyRevision = product.ProcurementPolicy.Revision
		}
		if evaluateCandidate {
			projection.Version = 2
			projection.Candidate = &ProcurementCandidatePreview{SourceState: "unknown"}
			if account != nil && account.ID == request.AccountID {
				projection.Candidate.InProduct = slices.Contains(account.GroupIDs, product.ID)
				policy := product.ProcurementPolicy
				// Candidate expansion requires positive, explicit group authority.
				// Legacy original-pool limits are not an approved supply inventory.
				if policy != nil && policy.Version >= 6 && policy.SourceGroupIDs != nil {
					if allowed, err := policy.AllowsSourceAccount(product.ID, account.ID, product.ProcurementSources); err == nil {
						projection.Candidate.SourceState = "fail"
						if allowed {
							projection.Candidate.SourceState = "pass"
						}
					}
				}
			}
		}
		switch {
		case product.Status != StatusActive:
			projection.State, projection.Reason = "product_inactive", "product_inactive"
		case !report.EffectiveGate:
			projection.State, projection.Reason = "disabled", "profit_gate_disabled"
		case projection.Candidate != nil && projection.Candidate.SourceState == "unknown":
			projection.State, projection.Reason = "not_evaluated", "candidate_sources_unconfirmed"
		case !evaluateCandidate && account != nil && !slices.Contains(account.GroupIDs, product.ID):
			projection.State, projection.Reason = "not_associated", "account_not_in_product"
		case verdict.Admitted && projection.Candidate != nil && projection.Candidate.SourceState == "fail":
			projection.State, projection.PriceClass, projection.Reason = "blocked", ProfitPreviewClassRejectedSource, "candidate_source_not_approved"
		case verdict.Admitted:
			projection.State = "constraints_passed"
		case verdict.PriceClass != "" && verdict.PriceClass != ProfitPreviewClassAdmitted:
			projection.State = "blocked"
		}
		if projection.priceState() != "unknown" {
			projection.PriceValidUntil = procurementPriceDeadlineAt(account, product, at)
		}
		projection.AssessEvidence(product.ProcurementPolicy, nil, nil, at)
		result.Product = projection
	}
	if catalog.Validate() != nil {
		result.State = "catalog_unavailable"
		result.Reason = "catalog_unavailable"
		return result
	}
	if len(catalog.Buckets) == 0 {
		result.State = "not_configured"
		return result
	}
	if account == nil || account.ID != request.AccountID {
		result.State = "account_unavailable"
		return result
	}
	result.RateSource = ProfitPreviewRateSourceManual
	if account.Type == AccountTypeAPIKey && upstreamBillingRateSyncEnabled(account) {
		result.RateSource = ProfitPreviewRateSourceUpstreamProbe
	}
	rate, reason := profitControlAccountRateAt(account, at)
	if reason != "" {
		result.State = "rate_unavailable"
		result.Reason = reason
		return result
	}
	value := strconv.FormatFloat(rate, 'f', -1, 64)
	result.EffectiveRate = &value
	if !billingApplicabilityScopeShapeKnown(request.Scope) {
		result.Reason = "profit_upstream_scope_unknown"
		return result
	}
	result.Applicability, result.Reason = upstreamBillingScopeVerdictAt(account, &request.Scope, at)
	if !account.IsModelSupported(request.PublicModel) {
		result.Reason = "model_not_supported"
		return result
	}
	if AvailabilityModelForAccount(account, request.PublicModel) != request.Scope.Model {
		result.Reason = "model_mapping_unconfirmed"
		return result
	}
	if result.Applicability != "covered" {
		return result
	}
	var err error
	result.Bucket, err = catalog.ClassifyRate(rate)
	if err != nil {
		result.State = "catalog_unavailable"
		result.Reason = "catalog_unavailable"
		return result
	}
	result.State = "unclassified"
	if result.Bucket != nil {
		result.State = "classified"
	}
	return result
}
