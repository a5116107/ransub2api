package repository

import (
	"database/sql"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/regional/ledger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestRegionalSalePolicyPreservesProviderSemantics(t *testing.T) {
	for _, tc := range []struct {
		name, provider, base, override, video, peak, want string
		independent, active                               bool
	}{
		{"grok_group", "grok", "0.12", "", "100", "3", "3/25", false, true},
		{"grok_user", "grok", "0.12", "0.15", "100", "3", "3/20", false, true},
		{"grok_video", "grok", "0.12", "0.15", "0.18", "3", "9/50", true, true},
		{"grok_free", "grok", "0.12", "0.15", "0", "3", "0", true, true},
		{"grok_disabled_video", "grok", "0.12", "", "NaN", "NaN", "3/25", false, true},
		{"minimax_group", "minimax-h3", "0.12", "0.15", "100", "3", "3/25", true, true},
		{"minimax_free", "minimax-h3", "0", "0.15", "100", "3", "0", true, true},
		{"seedance_group", "seedance", "0.12", "", "100", "3", "3/25", true, false},
		{"seedance_user_peak", "seedance", "0.12", "0.15", "100", "3", "9/20", true, true},
		{"seedance_group_peak", "seedance", "0.12", "", "100", "1.250000", "3/20", false, true},
		{"seedance_free_peak", "seedance", "0.12", "", "100", "0", "0", true, true},
		{"seedance_ignored_video", "seedance", "0.12", "", "NaN", "2", "6/25", true, true},
		{"text_peak", "", "0.12", "0.15", "100", "3", "9/20", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := regionalSalePolicy{group: service.Group{VideoRateIndependent: tc.independent}, base: tc.base, video: tc.video, peak: tc.peak, override: sql.NullString{String: tc.override, Valid: tc.override != ""}}
			rate, err := p.multiplier(tc.provider, tc.active)
			require.NoError(t, err)
			require.Equal(t, tc.want, rate.RatString())
		})
	}
}

func TestRegionalSalePolicyRejectsInvalidEffectiveRates(t *testing.T) {
	for _, raw := range []string{"", "NaN", "Infinity", "-Infinity", "-0.000001", "100000000000000000000", "0.0000000000000000000001"} {
		t.Run(raw, func(t *testing.T) {
			_, err := regionalSaleDecimal(raw)
			require.Error(t, err)
		})
	}
}

func TestRegionalSalePolicyFreezesPeakAtAdmission(t *testing.T) {
	p := regionalSalePolicy{group: service.Group{Platform: service.PlatformOpenAI, SubscriptionType: service.SubscriptionTypeSubscription, PeakRateEnabled: true, PeakStart: "14:00", PeakEnd: "18:00"}, base: "0.120000", peak: "1.250000"}
	template := ledger.Price{Model: "video", Video: &ledger.VideoPrice{VideoRequest: ledger.VideoRequest{Provider: "seedance", UpstreamModel: "ep-seedance"}, Basis: "completion_tokens", UnitNano: 28_000_000_000, SaleNumerator: 1, SaleDenominator: 10, MaxCompletionTokens: 1_000_000}}
	for _, tc := range []struct {
		hour, minute, numerator, denominator int
	}{
		{13, 59, 3, 250}, {14, 0, 3, 200}, {17, 59, 3, 200}, {18, 0, 3, 250},
	} {
		at := time.Date(2026, 11, 11, tc.hour, tc.minute, 0, 0, timezone.Location())
		price, err := p.derive(template, at)
		require.NoError(t, err)
		require.EqualValues(t, tc.numerator, price.Video.SaleNumerator)
		require.EqualValues(t, tc.denominator, price.Video.SaleDenominator)
		require.Equal(t, template.Version(), price.Video.TemplateVersion)
	}
	p.group.SubscriptionType = service.SubscriptionTypeStandard
	price, err := p.derive(template, time.Date(2026, 11, 11, 15, 0, 0, 0, timezone.Location()))
	require.NoError(t, err)
	require.EqualValues(t, 250, price.Video.SaleDenominator, "standard groups do not apply peak")
}
