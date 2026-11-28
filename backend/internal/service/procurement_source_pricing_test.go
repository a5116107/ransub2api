package service

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func procurementSourcePricingFixture(t *testing.T, raw string) (*ChannelService, *ProcurementSourceMapping) {
	t.Helper()
	pricing := NewPricingService(&config.Config{}, nil)
	data, err := pricing.parsePricingData([]byte(raw))
	require.NoError(t, err)
	pricing.pricingData, pricing.lastUpdated = data, time.Now().UTC().Add(-time.Minute)
	channel := NewChannelService(nil, nil, nil, pricing, nil)
	row, a, binding, now := procurementEvidenceFixture(t)
	row.Evidence = BuildProcurementSourceEvidence(&row, a, binding, now)
	return channel, &row
}

func TestProcurementSourcePricingUsesExactCatalogAndNativeUnitCalculation(t *testing.T) {
	s, row := procurementSourcePricingFixture(t, `{"gpt-fixture":{"litellm_provider":"openai","mode":"chat","input_cost_per_token":0.000002,"output_cost_per_token":0.00001,"cache_read_input_token_cost":0.0000002,"cache_creation_input_token_cost":0.0000025}}`)
	s.AddProcurementSourcePricing(row)
	quote := row.Evidence.Models.Entries[0].UnitQuote
	require.NotNil(t, quote)
	require.Equal(t, "estimated", quote.Status)
	require.Equal(t, "USD", quote.Currency)
	require.Equal(t, "per_1m_tokens_base_context", quote.Unit)
	require.Len(t, quote.BaselineHash, 64)
	require.NotNil(t, quote.CatalogUpdatedAt)
	require.Equal(t, .5, *quote.EffectiveGroupRate)
	require.Len(t, quote.Components, 4)
	require.InDelta(t, 1, *quote.Components[0].Amount, 1e-12)
	require.InDelta(t, 5, *quote.Components[1].Amount, 1e-12)
	require.InDelta(t, .1, *quote.Components[2].Amount, 1e-12)
	require.InDelta(t, 1.25, *quote.Components[3].Amount, 1e-12)
	require.Contains(t, quote.Pending, "supplier_components_unverified")
	require.Equal(t, "scope_not_covered", row.Evidence.Models.Entries[1].UnitQuote.Status)
}

func TestProcurementSourcePricingMissingZeroAndPriorityAreNotSubstituted(t *testing.T) {
	s, row := procurementSourcePricingFixture(t, `{"gpt-fixture":{"litellm_provider":"openai","mode":"chat","input_cost_per_token":0,"output_cost_per_token":0.00001,"input_cost_per_token_priority":0.000004,"output_cost_per_token_priority":0.00002,"cache_read_input_token_cost":null,"cache_creation_input_token_cost":-1}}`)
	row.Evidence.Models.Entries[1].Conclusion = "covered"
	s.AddProcurementSourcePricing(row)
	normal := row.Evidence.Models.Entries[0].UnitQuote
	require.Equal(t, "partial", normal.Status)
	require.NotNil(t, normal.Components[0].Amount)
	require.Zero(t, *normal.Components[0].Amount)
	require.Nil(t, normal.Components[2].Amount)
	require.Nil(t, normal.Components[3].Amount)
	fast := row.Evidence.Models.Entries[1].UnitQuote
	require.InDelta(t, 2, *fast.Components[0].Amount, 1e-12)
	require.InDelta(t, 10, *fast.Components[1].Amount, 1e-12)
	require.Nil(t, fast.Components[2].Amount, "priority must not borrow an undeclared cache price")
}

func TestProcurementSourcePricingNeverUsesFamilyFallbackOrOtherVendor(t *testing.T) {
	s, row := procurementSourcePricingFixture(t, `{"gpt-fixture":{"litellm_provider":"anthropic","mode":"chat","input_cost_per_token":0.000002,"output_cost_per_token":0.00001}}`)
	s.AddProcurementSourcePricing(row)
	require.Equal(t, "provider_mismatch", row.Evidence.Models.Entries[0].UnitQuote.Status)
	row.Evidence.Models.Entries[0].Model = "gpt-fixture-unknown"
	s.AddProcurementSourcePricing(row)
	require.Equal(t, "model_not_exact", row.Evidence.Models.Entries[0].UnitQuote.Status)
	require.Empty(t, row.Evidence.Models.Entries[0].UnitQuote.Components)
}

func TestProcurementSourcePricingIsVersionedAndDoesNotMutateTheCatalog(t *testing.T) {
	s, row := procurementSourcePricingFixture(t, `{"gpt-fixture":{"litellm_provider":"openai","mode":"chat","input_cost_per_token":0.000002,"output_cost_per_token":0.00001,"cache_read_input_token_cost":0,"cache_creation_input_token_cost":0,"long_context_input_token_threshold":1,"long_context_input_cost_multiplier":10}}`)
	before, err := json.Marshal(s.pricingService.pricingData)
	require.NoError(t, err)
	s.AddProcurementSourcePricing(row)
	quote := row.Evidence.Models.Entries[0].UnitQuote
	require.Equal(t, "estimated", quote.Status)
	require.InDelta(t, 1, *quote.Components[0].Amount, 1e-12, "per-million is a unit, not a million-token long-context request")
	after, err := json.Marshal(s.pricingService.pricingData)
	require.NoError(t, err)
	require.Equal(t, before, after)
	hash := quote.BaselineHash
	s.AddProcurementSourcePricing(row)
	require.Equal(t, hash, row.Evidence.Models.Entries[0].UnitQuote.BaselineHash)
	s.pricingService.pricingData["gpt-fixture"].InputCostPerToken = .000003
	s.AddProcurementSourcePricing(row)
	require.NotEqual(t, hash, row.Evidence.Models.Entries[0].UnitQuote.BaselineHash)
	require.InDelta(t, 1.5, *row.Evidence.Models.Entries[0].UnitQuote.Components[0].Amount, 1e-12)
}

func TestProcurementSourcePricingRechecksEvidenceAndNeverUsesCOrSavedRate(t *testing.T) {
	s, row := procurementSourcePricingFixture(t, `{"gpt-fixture":{"litellm_provider":"openai","mode":"chat","input_cost_per_token":0.000002,"output_cost_per_token":0.00001}}`)
	rate := 0.0
	row.Evidence.Selection.EffectiveGroupRate = &rate
	s.AddProcurementSourcePricing(row)
	require.Zero(t, *row.Evidence.Models.Entries[0].UnitQuote.Components[0].Amount)
	row.Evidence.Models.ExpiresAt = probeTimePtr(time.Now().Add(-time.Second))
	s.AddProcurementSourcePricing(row)
	require.Equal(t, "evidence_expired", row.Evidence.Models.Entries[0].UnitQuote.Status)
	require.Empty(t, row.Evidence.Models.Entries[0].UnitQuote.Components)
	row.Evidence.Models.ExpiresAt = probeTimePtr(time.Now().Add(time.Hour))
	row.Evidence.Selection.EffectiveGroupRate = nil
	s.AddProcurementSourcePricing(row)
	require.Equal(t, "rate_unavailable", row.Evidence.Models.Entries[0].UnitQuote.Status)
}

func TestProcurementSourcePricingDeclarationPresenceIsNotZeroOrLooseJSON(t *testing.T) {
	for _, fields := range []string{
		`"cache_read_input_token_cost":null`,
		`"cache_read_input_token_cost":-1`,
		`"Cache_Read_Input_Token_Cost":0.0000002`,
		`"cache_read_input_token_cost":0.0000002,"Cache_Read_Input_Token_Cost":0`,
		`"cache_read_input_token_cost":0,"cache_read_input_token_cost":0.0000002`,
	} {
		t.Run(fields, func(t *testing.T) {
			s, row := procurementSourcePricingFixture(t, fmt.Sprintf(`{"gpt-fixture":{"litellm_provider":"openai","mode":"chat","input_cost_per_token":0.000002,"output_cost_per_token":0.00001,%s}}`, fields))
			s.AddProcurementSourcePricing(row)
			require.Nil(t, row.Evidence.Models.Entries[0].UnitQuote.Components[2].Amount)
		})
	}
}

func TestProcurementSourcePricingNativePolicyMismatchNeverInventsAFreeFastTier(t *testing.T) {
	s, row := procurementSourcePricingFixture(t, `{"gpt-fixture":{"litellm_provider":"openai","mode":"chat","input_cost_per_token":0.000002,"output_cost_per_token":0.00001,"input_cost_per_token_priority":0,"output_cost_per_token_priority":0}}`)
	row.Evidence.Models.Entries[1].Conclusion = "covered"
	s.AddProcurementSourcePricing(row)
	fast := row.Evidence.Models.Entries[1].UnitQuote
	require.Equal(t, "partial", fast.Status)
	require.Equal(t, "billing_policy_differs", fast.Components[0].Status)
	require.Nil(t, fast.Components[0].Amount)
}

func TestProcurementSourcePricingConcurrentCatalogReloadIsOneCoherentBaseline(t *testing.T) {
	s, row := procurementSourcePricingFixture(t, `{"gpt-fixture":{"litellm_provider":"openai","mode":"chat","input_cost_per_token":0.000002,"output_cost_per_token":0.00001,"cache_read_input_token_cost":0,"cache_creation_input_token_cost":0}}`)
	// Reproduce the original hot-reload contract: replace the map under the
	// same lock, never mutate a returned catalog entry in place.
	one := s.pricingService.pricingData
	two, err := s.pricingService.parsePricingData([]byte(`{"gpt-fixture":{"litellm_provider":"openai","mode":"chat","input_cost_per_token":0.000004,"output_cost_per_token":0.00002,"cache_read_input_token_cost":0,"cache_creation_input_token_cost":0}}`))
	require.NoError(t, err)
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		for i := range 100 {
			s.pricingService.mu.Lock()
			if i%2 == 0 {
				s.pricingService.pricingData = two
			} else {
				s.pricingService.pricingData = one
			}
			s.pricingService.mu.Unlock()
		}
	}()
	for range 100 {
		s.AddProcurementSourcePricing(row)
		quote := row.Evidence.Models.Entries[0].UnitQuote
		require.InDelta(t, *quote.Components[0].Amount*5, *quote.Components[1].Amount, 1e-12)
	}
	wait.Wait()
}
