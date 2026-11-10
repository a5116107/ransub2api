package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/regional"
	"github.com/stretchr/testify/require"
)

func TestPricingVideoContracts(t *testing.T) {
	for _, raw := range []string{
		`{"model":"video-alias","video":{"provider":"minimax-h3","upstream_model":"MiniMax-H3","resolution":"768p","duration_seconds":5,"basis":"request","unit_nano_usd":20000000,"sale_numerator":1,"sale_denominator":10}}`,
		`{"model":"video-alias","video":{"provider":"minimax-h3","upstream_model":"MiniMax-H3","resolution":"768p","duration_seconds":5,"basis":"second","unit_nano_usd":20000000,"sale_numerator":1,"sale_denominator":10}}`,
		`{"model":"grok-imagine-video","video":{"provider":"grok","upstream_model":"grok-imagine-video","resolution":"720p","duration_seconds":6,"basis":"second","unit_nano_usd":70000000,"sale_numerator":3,"sale_denominator":20}}`,
		`{"model":"seedance-alias","video":{"provider":"seedance","upstream_model":"doubao-seedance-2-0","basis":"completion_tokens","unit_nano_usd":28000000000,"sale_numerator":9,"sale_denominator":50,"max_completion_tokens":1000000}}`,
	} {
		var price Price
		require.NoError(t, json.Unmarshal([]byte(raw), &price))
		require.NoError(t, price.Validate(), raw)
		_, err := price.Charge(0, 0, 0, 0)
		require.Error(t, err, "video prices must not enter text-token settlement")
		_, err = price.Maximum(1)
		require.Error(t, err, "text output limits must not bound video work")
	}
}

func TestPricingTextVersionRemainsCompatible(t *testing.T) {
	raw := []byte(`{"model":"mock-text","input_nano_per_million":1000000000,"output_nano_per_million":2000000000,"cache_creation_nano_per_million":3000000000,"cache_read_nano_per_million":100000000,"max_input_tokens":1000,"max_output_tokens":200}`)
	digest := sha256.Sum256(raw)
	require.Equal(t, hex.EncodeToString(digest[:]), textPrice().Version())
}

func videoPrice() Price {
	return Price{Model: "video-alias", Video: &VideoPrice{
		VideoRequest: VideoRequest{Provider: "minimax-h3", UpstreamModel: "MiniMax-H3", Resolution: "768p", DurationSeconds: 5},
		Basis:        "request", UnitNano: 20_000_000, SaleNumerator: 1, SaleDenominator: 10,
	}}
}

func TestPricingVideoExactQuotesAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*VideoPrice)
		tokens  int64
		base    regional.Amount
		charge  regional.Amount
		maximum regional.Amount
	}{
		{"request", func(*VideoPrice) {}, 0, 20_000_000, 2_000_000, 2_000_000},
		{"second", func(v *VideoPrice) { v.Basis = "second" }, 0, 100_000_000, 10_000_000, 10_000_000},
		{"grok", func(v *VideoPrice) {
			v.VideoRequest = VideoRequest{Provider: "grok", UpstreamModel: "grok-imagine-video", Resolution: "720p", DurationSeconds: 6}
			v.Basis, v.UnitNano, v.SaleNumerator, v.SaleDenominator = "second", 70_000_000, 3, 20
		}, 0, 420_000_000, 63_000_000, 63_000_000},
		{"seedance", func(v *VideoPrice) {
			v.VideoRequest = VideoRequest{Provider: "seedance", UpstreamModel: "doubao-seedance-2-0"}
			v.Basis, v.UnitNano, v.SaleNumerator, v.SaleDenominator, v.MaxCompletionTokens = "completion_tokens", 28_000_000_000, 9, 50, 1_000_000
		}, 12_345, 345_660_000, 62_218_800, 5_040_000_000},
		{"round_once", func(v *VideoPrice) {
			v.UnitNano, v.SaleNumerator, v.SaleDenominator = 1, 2, 1
		}, 0, 10, 10, 10},
		{"free_sale", func(v *VideoPrice) {
			v.SaleNumerator, v.SaleDenominator = 0, 1
		}, 0, 20_000_000, 0, 0},
		{"free_tariff", func(v *VideoPrice) { v.UnitNano = 0 }, 0, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := videoPrice()
			tc.mutate(p.Video)
			v := VideoUsage{VideoRequest: p.Video.VideoRequest, TaskID: "provider-task", State: "completed", CompletionTokens: tc.tokens}
			base, charge, err := p.VideoCosts(v)
			require.NoError(t, err)
			require.Equal(t, tc.base, base)
			require.Equal(t, tc.charge, charge)
			maximum, err := p.VideoMaximum(v.VideoRequest)
			require.NoError(t, err)
			require.Equal(t, tc.maximum, maximum)
			for _, state := range []string{"failed", "cancelled", "expired"} {
				v.State, v.CompletionTokens = state, 0
				base, charge, err = p.VideoCosts(v)
				require.NoError(t, err)
				require.Zero(t, base)
				require.Zero(t, charge)
			}
		})
	}
}

func TestPricingVideoRefusesUnsupportedOrChangedEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*VideoUsage)
	}{
		{"queued", func(v *VideoUsage) { v.State = "queued" }},
		{"unknown", func(v *VideoUsage) { v.State = "unknown" }},
		{"empty_task", func(v *VideoUsage) { v.TaskID = "" }},
		{"task_space", func(v *VideoUsage) { v.TaskID += " " }},
		{"task_control", func(v *VideoUsage) { v.TaskID += "\n" }},
		{"other_model", func(v *VideoUsage) { v.UpstreamModel = "another-model" }},
		{"other_provider", func(v *VideoUsage) { v.Provider = "grok" }},
		{"other_resolution", func(v *VideoUsage) { v.Resolution = "720p" }},
		{"other_duration", func(v *VideoUsage) { v.DurationSeconds++ }},
		{"negative_tokens", func(v *VideoUsage) { v.CompletionTokens = -1 }},
		{"unpriced_tokens", func(v *VideoUsage) { v.CompletionTokens = 1 }},
		{"failed_with_usage", func(v *VideoUsage) { v.State, v.CompletionTokens = "failed", 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := videoPrice()
			v := VideoUsage{VideoRequest: p.Video.VideoRequest, TaskID: "provider-task", State: "completed"}
			tc.mutate(&v)
			_, _, err := p.VideoCosts(v)
			require.Error(t, err)
		})
	}
	p := videoPrice()
	p.Video.VideoRequest = VideoRequest{Provider: "seedance", UpstreamModel: "seedance"}
	p.Video.Basis, p.Video.MaxCompletionTokens = "completion_tokens", 100
	_, _, err := p.VideoCosts(VideoUsage{VideoRequest: p.Video.VideoRequest, TaskID: "task", State: "completed", CompletionTokens: 101})
	require.ErrorContains(t, err, "exceed")
	p = videoPrice()
	p.Video.UnitNano, p.Video.Basis = math.MaxInt64, "second"
	_, err = p.VideoMaximum(p.Video.VideoRequest)
	require.ErrorContains(t, err, "range")
}

func TestPricingVideoRejectsMixedOrUnboundedContracts(t *testing.T) {
	for _, mutate := range []func(*Price){
		func(p *Price) { p.Input = 1 },
		func(p *Price) { p.MaxInput = 1 },
		func(p *Price) { p.Video.SaleNumerator = -1 },
		func(p *Price) { p.Video.SaleDenominator = 0 },
		func(p *Price) { p.Video.SaleNumerator, p.Video.SaleDenominator = 2, 20 },
		func(p *Price) { p.Video.UnitNano = -1 },
		func(p *Price) { p.Video.Basis = "completion_tokens" },
		func(p *Price) { p.Video.MaxCompletionTokens = 100 },
		func(p *Price) { p.Video.Provider = "unknown" },
		func(p *Price) { p.Video.DurationSeconds = 16 },
		func(p *Price) { p.Video.Resolution = "720p" },
		func(p *Price) {
			p.Video.VideoRequest = VideoRequest{Provider: "seedance", UpstreamModel: "seedance"}
			p.Video.Basis = "completion_tokens"
		},
	} {
		p := videoPrice()
		mutate(&p)
		require.Error(t, p.Validate())
	}
}

func TestPricingVideoDimensionsAndTariffChangeVersion(t *testing.T) {
	for _, mutate := range []func(*Price){
		func(p *Price) { p.Model += "-new" },
		func(p *Price) { p.Video.UpstreamModel += "-new" },
		func(p *Price) { p.Video.Provider = "grok" },
		func(p *Price) { p.Video.Resolution = "720p" },
		func(p *Price) { p.Video.DurationSeconds++ },
		func(p *Price) { p.Video.Basis = "second" },
		func(p *Price) { p.Video.UnitNano++ },
		func(p *Price) { p.Video.SaleNumerator++ },
		func(p *Price) { p.Video.SaleDenominator++ },
		func(p *Price) { p.Video.MaxCompletionTokens++ },
	} {
		p, original := videoPrice(), videoPrice().Version()
		mutate(&p)
		require.NotEqual(t, original, p.Version())
	}
}

func textPrice() Price {
	return Price{Model: "mock-text", Input: 1_000_000_000, Output: 2_000_000_000, CacheCreation: 3_000_000_000, CacheRead: 100_000_000, MaxInput: 1000, MaxOutput: 200}
}
func TestPricingExactBoundsAndIdentity(t *testing.T) {
	p := textPrice()
	charge, err := p.Charge(10, 5, 0, 10)
	require.NoError(t, err)
	require.Equal(t, regional.Amount(21000), charge)
	maximum, err := p.Maximum(10)
	require.NoError(t, err)
	require.Equal(t, regional.Amount(3020000), maximum)
	_, err = p.Charge(999, 0, 1, 1)
	require.Error(t, err)
	_, err = p.Maximum(0)
	require.Error(t, err)
	_, err = p.Maximum(201)
	require.Error(t, err)
	changed := p
	changed.MaxInput++
	require.NotEqual(t, p.Version(), changed.Version())
	changed = p
	changed.Input++
	require.NotEqual(t, p.Version(), changed.Version())
	p.Input = 1
	p.Output = 1
	charge, err = p.Charge(1, 1, 0, 0)
	require.NoError(t, err)
	require.Equal(t, regional.Amount(10), charge)
	p.Input = math.MaxInt64
	p.MaxInput = math.MaxInt32
	_, err = p.Maximum(1)
	require.Error(t, err)
}
