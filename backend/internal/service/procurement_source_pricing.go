package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"strings"
	"time"
)

// A local catalog estimate is not a supplier component-price contract or cash
// cost. It never participates in dispatch, margin checks or user billing.
type ProcurementSourceUnitQuote struct {
	Version            int                              `json:"version"`
	Status             string                           `json:"status"`
	Currency           string                           `json:"currency"`
	Unit               string                           `json:"unit"`
	BaselineHash       string                           `json:"baseline_hash"`
	CatalogUpdatedAt   *time.Time                       `json:"catalog_updated_at"`
	ExpiresAt          *time.Time                       `json:"expires_at"`
	EffectiveGroupRate *float64                         `json:"effective_group_rate"`
	Components         []ProcurementSourceUnitComponent `json:"components"`
	Pending            []string                         `json:"pending"`
}

type ProcurementSourceUnitComponent struct {
	Code   string   `json:"code"`
	Status string   `json:"status"`
	Amount *float64 `json:"amount"`
}

// AddProcurementSourcePricing enriches only the administrator's existing read
// model. Hold one catalog read lock per source so hot reload cannot mix model
// versions within a single result. No network/files, writes or mutable caches.
func (s *ChannelService) AddProcurementSourcePricing(row *ProcurementSourceMapping) {
	if row == nil || row.Evidence == nil || row.Evidence.Models.Status != "declared" {
		return
	}
	if s != nil && s.pricingService != nil {
		s.pricingService.mu.RLock()
		defer s.pricingService.mu.RUnlock()
	}
	for i := range row.Evidence.Models.Entries {
		entry := &row.Evidence.Models.Entries[i]
		entry.UnitQuote = s.procurementSourceUnitQuote(row, entry, time.Now().UTC())
	}
}

func (s *ChannelService) procurementSourceUnitQuote(row *ProcurementSourceMapping, entry *ProcurementSourceModelEntry, at time.Time) *ProcurementSourceUnitQuote {
	evidence := row.Evidence
	quote := &ProcurementSourceUnitQuote{Version: 1, Status: "scope_not_covered", Currency: "USD", Unit: "per_1m_tokens_base_context",
		Components: []ProcurementSourceUnitComponent{}, Pending: []string{"supplier_baseline_unverified", "supplier_components_unverified", "other_components_unreviewed"}}
	if entry.Conclusion != "covered" {
		return quote
	}
	if evidence.Models.ExpiresAt == nil || !at.Before(*evidence.Models.ExpiresAt) || at.Before(evidence.AssessedAt) ||
		(evidence.Selection.Status != "supplier_declared" && evidence.Selection.Status != "receipt_confirmed") {
		quote.Status = "evidence_expired"
		return quote
	}
	quote.ExpiresAt = evidence.Models.ExpiresAt
	rate := evidence.Selection.EffectiveGroupRate
	if rate == nil || !newAPIFinite(*rate) || *rate < 0 {
		quote.Status = "rate_unavailable"
		return quote
	}
	quote.EffectiveGroupRate = rate
	if s == nil || s.pricingService == nil {
		quote.Status = "catalog_unavailable"
		return quote
	}
	// Exact upstream wire name only: no model-family, alias, date stripping or
	// built-in fallback. An identical lowercase key is not model authenticity.
	model := strings.ToLower(strings.TrimSpace(entry.Model))
	pricing := s.pricingService.pricingData[model]
	if pricing == nil {
		quote.Status = "model_not_exact"
		return quote
	}
	provider := map[string]string{PlatformOpenAI: "openai", PlatformAnthropic: "anthropic", PlatformGemini: "gemini", PlatformGrok: "xai",
		PlatformDeepseek: "deepseek", PlatformKimi: "moonshot", PlatformZhipu: "z_ai", PlatformMiniMax: "minimax", PlatformMistral: "mistral", PlatformQwen: "qwen"}[row.Platform]
	if provider == "" {
		quote.Status = "unsupported_scope"
		return quote
	}
	if strings.ToLower(pricing.LiteLLMProvider) != provider {
		quote.Status = "provider_mismatch"
		return quote
	}
	if pricing.TokenPricingAbsent || pricing.Mode != "chat" || entry.Context != "all" ||
		(entry.ServiceTier != "unspecified" && entry.ServiceTier != "default" && entry.ServiceTier != "priority" && entry.ServiceTier != "fast") {
		quote.Status = "unsupported_scope"
		return quote
	}
	native := (&BillingService{}).modelPricingFromCatalog(model, pricing, evidence.AssessedAt)
	raw, err := json.Marshal(struct {
		Version  string
		Model    string
		Catalog  *LiteLLMModelPricing
		Declared map[string]bool
		Native   *ModelPricing
	}{"token-unit-v1", model, pricing, pricing.DeclaredTokenPrices, native})
	if err != nil {
		quote.Status = "invalid_catalog"
		return quote
	}
	hash := sha256.Sum256(raw)
	quote.BaselineHash = hex.EncodeToString(hash[:])
	if !s.pricingService.lastUpdated.IsZero() {
		quote.CatalogUpdatedAt = probeTimePtr(s.pricingService.lastUpdated)
	}
	quote.Status = "estimated"
	fields := []struct {
		code, key          string
		ordinary, priority float64
		tokens             UsageTokens
	}{
		{"input", "input_cost_per_token", pricing.InputCostPerToken, pricing.InputCostPerTokenPriority, UsageTokens{InputTokens: 1}},
		{"output", "output_cost_per_token", pricing.OutputCostPerToken, pricing.OutputCostPerTokenPriority, UsageTokens{OutputTokens: 1}},
		{"cache_read", "cache_read_input_token_cost", pricing.CacheReadInputTokenCost, pricing.CacheReadInputTokenCostPriority, UsageTokens{CacheReadTokens: 1}},
		{"cache_write", "cache_creation_input_token_cost", pricing.CacheCreationInputTokenCost, pricing.CacheCreationInputTokenCostPriority, UsageTokens{CacheCreationTokens: 1}},
	}
	for _, field := range fields {
		component := ProcurementSourceUnitComponent{Code: field.code, Status: "not_declared"}
		expected := field.ordinary
		if entry.ServiceTier == "priority" || entry.ServiceTier == "fast" {
			field.key += "_priority"
			expected = field.priority
		}
		if pricing.DeclaredTokenPrices[field.key] {
			// Reuse the native one-token calculation; a million-token test
			// request would accidentally select a long-context session tier.
			cost := (&BillingService{}).computeTokenBreakdown(native, field.tokens, 1, entry.ServiceTier, false)
			scale := math.Max(math.Abs(cost.TotalCost), math.Abs(expected))
			if !newAPIFinite(cost.TotalCost) || cost.TotalCost < 0 || math.Abs(cost.TotalCost-expected) > scale*profitControlRateEpsilon {
				component.Status = "billing_policy_differs"
			} else if amount := cost.TotalCost * *rate * 1e6; newAPIFinite(amount) {
				component.Status, component.Amount = "estimated", &amount
			}
		}
		if component.Amount == nil {
			quote.Status = "partial"
		}
		quote.Components = append(quote.Components, component)
	}
	return quote
}
