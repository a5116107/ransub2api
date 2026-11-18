package service

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
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
	State         string                    `json:"state"`
	Reason        string                    `json:"reason,omitempty"`
	Applicability string                    `json:"applicability"`
	RateSource    string                    `json:"rate_source"`
	EffectiveRate *string                   `json:"effective_rate"`
	Bucket        *domain.ProcurementBucket `json:"bucket"`
	Qualification string                    `json:"qualification"`
}

type ProcurementClassificationPreview struct {
	ProcurementCatalogSnapshot
	ObservedAt time.Time                        `json:"observed_at"`
	Results    []ProcurementScopeClassification `json:"results"`
}

type ProcurementCatalogRepository interface {
	ReadProcurementCatalog(context.Context) (*domain.ProcurementCatalog, error)
	SaveProcurementCatalog(context.Context, *domain.ProcurementCatalog) (*domain.ProcurementCatalog, error)
	PreviewProcurementClassification(context.Context, []ProfitPreviewRequestScope) (*ProcurementClassificationPreview, error)
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
	value := gjson.ParseBytes(raw)
	if len(raw) > 65536 || !gjson.ValidBytes(raw) || !procurementCatalogObject(value, "version", "revision", "buckets") || value.Get("revision").Type == gjson.Null || !value.Get("buckets").IsArray() {
		return nil, ErrProcurementCatalogInvalid
	}
	for _, bucket := range value.Get("buckets").Array() {
		if !procurementCatalogObject(bucket, "code", "name", "min_rate", "max_rate") {
			return nil, ErrProcurementCatalogInvalid
		}
	}
	var catalog domain.ProcurementCatalog
	if json.Unmarshal(raw, &catalog) != nil || catalog.ValidateDraft() != nil {
		return nil, ErrProcurementCatalogInvalid
	}
	return &catalog, nil
}

func ParseProcurementPreviewCommand(raw []byte) ([]ProfitPreviewRequestScope, error) {
	value := gjson.ParseBytes(raw)
	if len(raw) > 65536 || !gjson.ValidBytes(raw) || !procurementCatalogObject(value, "requests") || !value.Get("requests").IsArray() {
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
	return requests, nil
}

// ClassifyProcurementScope consumes the original U_gate and exact applicability
// contracts. A catalog match is never a quality/cache or product qualification.
func ClassifyProcurementScope(catalog *domain.ProcurementCatalog, account *Account, request ProfitPreviewRequestScope, at time.Time) ProcurementScopeClassification {
	result := ProcurementScopeClassification{ProfitPreviewRequestScope: request, State: "scope_unconfirmed", Applicability: "unknown", RateSource: "unknown", Qualification: "not_evaluated"}
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
