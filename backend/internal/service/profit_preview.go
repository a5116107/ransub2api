package service

// 分组利润控制离线预演：使用生产只读导出的分组配置、账号倍率、探测状态、
// 用户覆盖倍率与主力模型清单，复用线上 U/D/epsilon 语义推演准入。
// 自动同步的 API Key 复用线上快照有效性与实时高峰规则；手工账号使用保存倍率。

import (
	"errors"
	"math"
	"sort"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/tidwall/gjson"
)

const (
	ProfitPreviewRateSourceManual        = "manual"
	ProfitPreviewRateSourceUpstreamProbe = "upstream_probe_sync"

	ProfitPreviewWarningProbeMissing      = "probe_snapshot_missing"
	ProfitPreviewWarningProbeStale        = "probe_snapshot_stale"
	ProfitPreviewWarningProbeFailed       = "probe_failed"
	ProfitPreviewWarningProbeUnsupported  = "probe_unsupported"
	ProfitPreviewWarningProbeInvalid      = "probe_rate_evidence_invalid"
	ProfitPreviewWarningScopeNotEvaluated = "probe_request_scope_not_evaluated"
	ProfitPreviewWarningManualRateOne     = "manual_rate_1_suspected_unmaintained"
)

// ProfitPreviewGroupInput 是一个分组的预演输入。
type ProfitPreviewGroupInput struct {
	Group *Group
	// Accounts 为该分组绑定且状态可调度的账号，允许 Anthropic/Gemini 分组中
	// 参与 mixed scheduling 的 Antigravity 账号。
	Accounts []*Account
	// UserOverrides 为该分组所有用户覆盖倍率（user_id → 倍率）。
	UserOverrides map[int64]float64
	// Models 为该分组近期主力模型清单。
	Models []string
	// RequestScopes are explicit final wire scopes, never inferred from the
	// public model or an account-level price. No prompt/credential is accepted.
	RequestScopes []ProfitPreviewRequestScope
	// AssumeEnabled 在分组当前关闭时仍按其保存配置启用利润门，供部署前执行
	// “先关组、预演、逐组重开”。
	AssumeEnabled bool
}

type ProfitPreviewRequestScope struct {
	AccountID   int64                    `json:"account_id"`
	PublicModel string                   `json:"public_model"`
	Scope       AvailabilityRequestScope `json:"scope"`
}

type ProfitPreviewScopeVerdict struct {
	ProfitPreviewRequestScope
	Applicability     string `json:"applicability"`
	PriceClass        string `json:"price_class"`
	Reason            string `json:"reason,omitempty"`
	Admitted          bool   `json:"admitted"`
	AdmittedUnderMinD bool   `json:"admitted_under_min_d"`
}

func ParseProfitPreviewRequestScope(raw []byte) (ProfitPreviewRequestScope, error) {
	var request ProfitPreviewRequestScope
	top := gjson.ParseBytes(raw)
	selectors, selectorsValid := availabilityJSONObject(top)
	if !gjson.ValidBytes(raw) || !selectorsValid || len(selectors) != 3 {
		return request, errors.New("invalid request_scope")
	}
	for _, name := range []string{"account_id", "public_model", "scope"} {
		if _, present := selectors[name]; !present {
			return request, errors.New("unknown or missing request_scope selector")
		}
	}
	if !decodeBillingApplicabilityFields(top, &request, "account_id", "public_model", "scope") {
		return request, errors.New("invalid request_scope")
	}
	fields, valid := availabilityJSONObject(top.Get("scope"))
	allowed := map[string]bool{
		"version": true, "protocol": true, "stream": true, "model": true,
		"service_tier": true, "reasoning_effort": true, "capability": true,
		"reasoning_summary": true, "include_encrypted_content": true, "text_verbosity": true,
		"thinking_mode": true, "thinking_budget": true, "protocol_options": true,
	}
	if !valid || len(fields) == 0 || len(fields) > len(allowed) {
		return ProfitPreviewRequestScope{}, errors.New("invalid request_scope.scope")
	}
	for name := range fields {
		if !allowed[name] {
			return ProfitPreviewRequestScope{}, errors.New("unknown request_scope.scope field")
		}
	}
	if !decodeBillingApplicabilityFields(top.Get("scope"), &request.Scope,
		"version", "protocol", "stream", "model", "service_tier", "reasoning_effort", "reasoning_summary", "include_encrypted_content", "text_verbosity", "capability", "thinking_mode", "thinking_budget", "protocol_options") {
		return ProfitPreviewRequestScope{}, errors.New("invalid request_scope.scope")
	}
	if _, valid := availabilityResponsesReasoningSummary(fields["reasoning_summary"]); !valid {
		return ProfitPreviewRequestScope{}, errors.New("invalid request_scope.scope reasoning_summary")
	}
	if verbosity, present := fields["text_verbosity"]; present {
		if _, valid := availabilityOption(verbosity, "low", "medium", "high"); !valid {
			return ProfitPreviewRequestScope{}, errors.New("invalid request_scope.scope text_verbosity")
		}
	}
	if encrypted := fields["include_encrypted_content"]; encrypted.Exists() && encrypted.Type != gjson.True && encrypted.Type != gjson.False {
		return ProfitPreviewRequestScope{}, errors.New("invalid request_scope.scope include_encrypted_content")
	}
	return request, nil
}

// 预演账号分类与线上否决原因一一对应。
const (
	ProfitPreviewClassAdmitted             = "admitted"
	ProfitPreviewClassRejectedThreshold    = "rejected_threshold"
	ProfitPreviewClassRejectedProcurement  = "rejected_procurement_range"
	ProfitPreviewClassRejectedSource       = "rejected_procurement_source"
	ProfitPreviewClassRejectedBucket       = "rejected_procurement_bucket"
	ProfitPreviewClassUnknownCatalog       = "rejected_procurement_catalog_unavailable"
	ProfitPreviewClassUnknownProcurement   = "rejected_procurement_policy_unavailable"
	ProfitPreviewClassRejectedInvalidRate  = "rejected_invalid_account_rate"
	ProfitPreviewClassRejectedUpstreamRate = "rejected_upstream_rate_unavailable"
	ProfitPreviewClassRejectedSaleRate     = "rejected_account_sale_rate_mismatch"
)

// ProfitPreviewAccountVerdict 是单账号的预演结论。
type ProfitPreviewAccountVerdict struct {
	AccountID   int64    `json:"account_id"`
	Name        string   `json:"name"`
	Platform    string   `json:"platform"`
	Class       string   `json:"class"`
	AccountRate *float64 `json:"account_rate,omitempty"`
	// EffectiveRate is U at evalAt; AccountRate remains the stored base rate.
	// Invalid or unavailable rate evidence leaves EffectiveRate absent.
	EffectiveRate *float64 `json:"effective_upstream_rate,omitempty"`
	RateSource    string   `json:"rate_source"`
	Warnings      []string `json:"warnings,omitempty"`
	// RejectedUnderMinD 表示账号在最低有效 D（用户覆盖最坏口径）下会被排除。
	RejectedUnderMinD bool `json:"rejected_under_min_d,omitempty"`
	// SupportedModels 为该账号支持的主力模型子集。
	SupportedModels []string `json:"supported_models,omitempty"`
}

// ProfitPreviewGroupReport 是单分组的预演报告。
type ProfitPreviewGroupReport struct {
	ProcurementCatalogRevision *int64             `json:"procurement_catalog_revision,omitempty"`
	ProcurementPolicy          *ProcurementPolicy `json:"procurement_policy,omitempty"`
	GroupID                    int64              `json:"group_id"`
	GroupName                  string             `json:"group_name"`
	Platform                   string             `json:"platform"`
	EffectiveGate              bool               `json:"effective_gate"`
	AssumedEnabled             bool               `json:"assumed_enabled,omitempty"`
	// DefaultD / MinEffectiveD：分组默认 D 与最低有效 D（含 evalAt 高峰因子；
	// MinEffectiveD = min(分组默认, 全部非空用户覆盖) × 高峰因子）。
	DefaultD      float64 `json:"default_d"`
	MinEffectiveD float64 `json:"min_effective_d"`
	// ThresholdDefault / ThresholdMinD：两档 D 对应的准入阈值。
	ThresholdDefault float64 `json:"threshold_default"`
	ThresholdMinD    float64 `json:"threshold_min_d"`
	// RemainingByModel 仅表示利润门准入账号数，不模拟健康、冷却、限流或槽位。
	RemainingByModel     map[string]int                `json:"profit_admitted_by_model"`
	RemainingByModelMinD map[string]int                `json:"profit_admitted_by_model_min_d"`
	ModelCountsScope     string                        `json:"model_counts_scope"`
	Verdicts             []ProfitPreviewAccountVerdict `json:"verdicts"`
	ScopeVerdicts        []ProfitPreviewScopeVerdict   `json:"scope_verdicts,omitempty"`
}

// PreviewProfitAdmission 对五大平台分组推演利润门准入结果。未启用的分组仅在
// AssumeEnabled=true 时按保存配置执行门；Composite 分组始终不直接安装利润门。
func PreviewProfitAdmission(inputs []ProfitPreviewGroupInput, evalAt time.Time) []ProfitPreviewGroupReport {
	reports := make([]ProfitPreviewGroupReport, 0, len(inputs))
	for _, in := range inputs {
		if in.Group == nil {
			continue
		}
		group := in.Group
		effectiveGate := (group.ProfitControlEnabled || in.AssumeEnabled) && profitControlPlatformSupported(group.Platform)
		report := ProfitPreviewGroupReport{
			ProcurementPolicy:    group.ProcurementPolicy.Clone(),
			GroupID:              group.ID,
			GroupName:            group.Name,
			Platform:             group.Platform,
			EffectiveGate:        effectiveGate,
			AssumedEnabled:       in.AssumeEnabled && !group.ProfitControlEnabled && effectiveGate,
			RemainingByModel:     make(map[string]int, len(in.Models)),
			RemainingByModelMinD: make(map[string]int, len(in.Models)),
			ModelCountsScope:     "account_price_only",
		}
		if group.ProcurementCatalog != nil {
			revision := group.ProcurementCatalog.Revision
			report.ProcurementCatalogRevision = &revision
		}
		for _, model := range in.Models {
			report.RemainingByModel[model] = 0
			report.RemainingByModelMinD[model] = 0
		}
		accountsByID := make(map[int64]*Account, len(in.Accounts))
		for _, account := range in.Accounts {
			if account != nil {
				accountsByID[account.ID] = account
			}
		}

		peak := group.PeakMultiplierAt(evalAt)
		defaultD := group.RateMultiplier * peak
		minRate := group.RateMultiplier
		for _, override := range in.UserOverrides {
			if math.IsNaN(override) || math.IsInf(override, 0) || override < 0 {
				continue
			}
			if override < minRate {
				minRate = override
			}
		}
		minD := minRate * peak
		deduction := group.ProfitMinMargin + group.ProfitSafetyBuffer
		thresholdDefault := clampProfitControlThreshold(defaultD * (1 - deduction))
		thresholdMinD := clampProfitControlThreshold(minD * (1 - deduction))
		report.DefaultD = defaultD
		report.MinEffectiveD = minD
		report.ThresholdDefault = thresholdDefault
		report.ThresholdMinD = thresholdMinD

		for _, account := range in.Accounts {
			if account == nil {
				continue
			}
			verdict := previewAccountProfitAdmission(account, effectiveGate, thresholdDefault, thresholdMinD, evalAt, group)
			admittedDefault := verdict.Class == ProfitPreviewClassAdmitted
			admittedMinD := admittedDefault && !verdict.RejectedUnderMinD
			for _, model := range in.Models {
				if !account.IsModelSupportedInGroup(&group.ID, model) {
					continue
				}
				verdict.SupportedModels = append(verdict.SupportedModels, model)
				if admittedDefault {
					report.RemainingByModel[model]++
				}
				if admittedMinD {
					report.RemainingByModelMinD[model]++
				}
			}
			sort.Strings(verdict.SupportedModels)
			report.Verdicts = append(report.Verdicts, verdict)
		}
		for _, request := range in.RequestScopes {
			report.ScopeVerdicts = append(report.ScopeVerdicts, previewProfitRequestScope(accountsByID[request.AccountID], request, group.ID, effectiveGate, thresholdDefault, thresholdMinD, evalAt, group))
		}
		sort.Slice(report.Verdicts, func(i, j int) bool { return report.Verdicts[i].AccountID < report.Verdicts[j].AccountID })
		reports = append(reports, report)
	}
	return reports
}

func previewProfitRequestScope(account *Account, request ProfitPreviewRequestScope, groupID int64, enabled bool, threshold, minThreshold float64, at time.Time, group *Group) ProfitPreviewScopeVerdict {
	result := ProfitPreviewScopeVerdict{ProfitPreviewRequestScope: request, Applicability: "unknown"}
	if account == nil {
		result.Reason = "account_unavailable"
		return result
	}
	price := previewAccountProfitAdmission(account, enabled, threshold, minThreshold, at, group)
	result.PriceClass = price.Class
	if !billingApplicabilityScopeShapeKnown(request.Scope) {
		result.Reason = "profit_upstream_scope_unknown"
		return result
	}
	result.Applicability, result.Reason = upstreamBillingScopeVerdictAt(account, &request.Scope, at)
	switch {
	case !enabled:
		result.Reason = "profit_gate_disabled"
	case request.PublicModel == "" || !account.IsModelSupportedInGroup(&groupID, request.PublicModel):
		result.Reason = "model_not_supported"
	case AvailabilityModelForAccount(account, request.PublicModel) != request.Scope.Model:
		result.Reason = "model_mapping_unconfirmed"
	case price.Class != ProfitPreviewClassAdmitted:
		result.Reason = price.Class
	case result.Applicability == "covered":
		result.Admitted = true
		result.AdmittedUnderMinD = !price.RejectedUnderMinD
	}
	return result
}

func previewAccountProfitAdmission(
	account *Account,
	effectiveGate bool,
	thresholdDefault float64,
	thresholdMinD float64,
	evalAt time.Time,
	group *Group,
) ProfitPreviewAccountVerdict {
	verdict := ProfitPreviewAccountVerdict{
		AccountID:  account.ID,
		Name:       account.Name,
		Platform:   account.Platform,
		RateSource: ProfitPreviewRateSourceManual,
	}
	if account.Type == AccountTypeAPIKey && upstreamBillingRateSyncEnabled(account) {
		verdict.RateSource = ProfitPreviewRateSourceUpstreamProbe
		verdict.Warnings = append(verdict.Warnings, profitPreviewProbeWarnings(account, evalAt)...)
		if snapshot := decodeUpstreamBillingProbeSnapshot(account.Extra); snapshot != nil {
			if _, present := snapshot.Data[upstreamBillingApplicabilityKey]; present {
				// This preview has no prepared protocol/tier/effort/context.
				// An account-level price verdict must not claim wire qualification.
				verdict.Warnings = append(verdict.Warnings, ProfitPreviewWarningScopeNotEvaluated)
			}
		}
	} else if account.RateMultiplier != nil && *account.RateMultiplier == 1 {
		verdict.Warnings = append(verdict.Warnings, ProfitPreviewWarningManualRateOne)
	}

	upstream, reason := profitControlAccountRateAt(account, evalAt)
	if reason != openAIProfitFilterReasonInvalidAccountRate {
		rate := *account.RateMultiplier
		verdict.AccountRate = &rate
	}
	if reason == "" {
		verdict.EffectiveRate = &upstream
	} else if reason == openAIProfitFilterReasonUpstreamRateUnavailable && len(verdict.Warnings) == 0 {
		verdict.Warnings = append(verdict.Warnings, ProfitPreviewWarningProbeInvalid)
	}
	var policy *ProcurementPolicy
	var catalog *domain.ProcurementCatalog
	var sources *domain.ProcurementSourceSnapshot
	var groupID int64
	if group != nil {
		policy, catalog, sources, groupID = group.ProcurementPolicy, group.ProcurementCatalog, group.ProcurementSources, group.ID
	}
	procurementReason := procurementPolicyVetoReason(policy, catalog, account, upstream, sources, groupID)
	switch {
	case !effectiveGate:
		verdict.Class = ProfitPreviewClassAdmitted
	case reason == openAIProfitFilterReasonInvalidAccountRate:
		verdict.Class = ProfitPreviewClassRejectedInvalidRate
	case reason == openAIProfitFilterReasonUpstreamRateUnavailable:
		verdict.Class = ProfitPreviewClassRejectedUpstreamRate
	case reason == openAIProfitFilterReasonAccountSaleRateMismatch:
		verdict.Class = ProfitPreviewClassRejectedSaleRate
	case profitControlOverThreshold(upstream, thresholdDefault):
		verdict.Class = ProfitPreviewClassRejectedThreshold
	case procurementReason == openAIProfitFilterReasonProcurementUnavailable:
		verdict.Class = ProfitPreviewClassUnknownProcurement
	case procurementReason == openAIProfitFilterReasonProcurementRange:
		verdict.Class = ProfitPreviewClassRejectedProcurement
	case procurementReason == openAIProfitFilterReasonProcurementSource:
		verdict.Class = ProfitPreviewClassRejectedSource
	case procurementReason == openAIProfitFilterReasonProcurementBucket:
		verdict.Class = ProfitPreviewClassRejectedBucket
	case procurementReason == openAIProfitFilterReasonProcurementCatalog:
		verdict.Class = ProfitPreviewClassUnknownCatalog
	default:
		verdict.Class = ProfitPreviewClassAdmitted
		verdict.RejectedUnderMinD = profitControlOverThreshold(upstream, thresholdMinD)
	}
	return verdict
}

func profitPreviewProbeWarnings(account *Account, evalAt time.Time) []string {
	snapshot := decodeUpstreamBillingProbeSnapshot(account.Extra)
	if snapshot == nil {
		return []string{ProfitPreviewWarningProbeMissing}
	}
	switch snapshot.Status {
	case UpstreamBillingProbeStatusFailed:
		return []string{ProfitPreviewWarningProbeFailed}
	case UpstreamBillingProbeStatusUnsupported:
		return []string{ProfitPreviewWarningProbeUnsupported}
	case UpstreamBillingProbeStatusOK:
		if snapshot.FreshUntil == nil || !evalAt.Before(*snapshot.FreshUntil) {
			return []string{ProfitPreviewWarningProbeStale}
		}
	}
	return nil
}
