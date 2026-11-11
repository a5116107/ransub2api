package service

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func coldStartGroups(now time.Time) []LatencyGroup {
	cold, warm := 6000.0, 1000.0
	groups := make([]LatencyGroup, 2)
	for i := range groups {
		groups[i] = LatencyGroup{AccountID: 7, Model: "public-model", UpstreamModel: "actual-model", ServiceTier: "default",
			UpstreamEndpoint: "/v1/responses", TransportEndpoint: "responses", RouteFingerprint: strings.Repeat("a", 64), ProxyID: 0, Stream: true,
			CacheSource: "canonical_responses_terminal_usage", BodyBucket: "1-4MiB", ReasoningEffort: "high",
			TextWaitSamples: 5, TextWaitLatest: &now}
	}
	groups[0].CacheBucket, groups[0].TextWaitP90MS = "0%", &cold
	groups[1].CacheBucket, groups[1].TextWaitP90MS = "95%+", &warm
	return groups
}

func TestCacheColdStartHistoricalAssociation(t *testing.T) {
	now := time.Now().UTC()
	filter := LatencyReportFilter{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}
	groups := coldStartGroups(now)
	annotateCacheColdStart(groups, filter, now)
	for _, g := range groups {
		e := g.ColdStart
		require.Equal(t, "historical_association", e.Status)
		require.Equal(t, "independent_history_p90_difference_not_causal", e.Method)
		require.EqualValues(t, 5, e.ColdSamples)
		require.EqualValues(t, 5, e.WarmSamples)
		require.EqualValues(t, 5, e.MinSamples)
		require.EqualValues(t, 900, e.MaxAgeSeconds)
		require.Equal(t, now, e.AsOf)
		require.InDelta(t, 5000, *e.DeltaMS, 1e-9)
	}
	*groups[0].TextWaitP90MS = 500
	annotateCacheColdStart(groups, filter, now)
	require.Equal(t, -500.0, *groups[0].ColdStart.DeltaMS, "negative associations are not promised savings or clamped to zero")
}

func TestCacheColdStartRejectsMissingScope(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*LatencyGroup)
	}{
		{"legacy_usage", func(g *LatencyGroup) { g.CacheSource = "billed_usage_positive_read" }},
		{"unknown_route", func(g *LatencyGroup) { g.RouteFingerprint = "unknown" }},
		{"invalid_route", func(g *LatencyGroup) { g.RouteFingerprint = "not-a-route-digest" }},
		{"zero_route", func(g *LatencyGroup) { g.RouteFingerprint = strings.Repeat("0", 64) }},
		{"empty_model", func(g *LatencyGroup) { g.Model = "" }},
		{"unknown_upstream", func(g *LatencyGroup) { g.UpstreamModel = "unknown" }},
		{"missing_tier", func(g *LatencyGroup) { g.ServiceTier = "unknown" }},
		{"unspecified_tier", func(g *LatencyGroup) { g.ServiceTier = "unspecified" }},
		{"missing_endpoint", func(g *LatencyGroup) { g.UpstreamEndpoint = "" }},
		{"missing_physical_endpoint", func(g *LatencyGroup) { g.TransportEndpoint = "" }},
		{"unknown_physical_endpoint", func(g *LatencyGroup) { g.TransportEndpoint = "unknown" }},
		{"unclassified_physical_endpoint", func(g *LatencyGroup) { g.TransportEndpoint = "other" }},
		{"future_physical_endpoint", func(g *LatencyGroup) { g.TransportEndpoint = "future_endpoint" }},
		{"raw_physical_path", func(g *LatencyGroup) { g.TransportEndpoint = "/v1/responses" }},
		{"ws_endpoint", func(g *LatencyGroup) { g.TransportEndpoint = "ws_turn" }},
		{"missing_body", func(g *LatencyGroup) { g.BodyBucket = "" }},
		{"missing_effort", func(g *LatencyGroup) { g.ReasoningEffort = "unknown" }},
		{"missing_proxy", func(g *LatencyGroup) { g.ProxyID = -1 }},
		{"missing_account", func(g *LatencyGroup) { g.AccountID = 0 }},
		{"non_stream", func(g *LatencyGroup) { g.Stream = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			groups := coldStartGroups(now)
			tc.edit(&groups[0])
			annotateCacheColdStart(groups, LatencyReportFilter{Start: now.Add(-time.Hour), End: now}, now)
			require.Equal(t, "unknown_scope", groups[0].ColdStart.Status)
			require.Nil(t, groups[0].ColdStart.DeltaMS)
			require.Equal(t, "insufficient_samples", groups[1].ColdStart.Status)
		})
	}
}

func TestCacheColdStartDoesNotMixCohorts(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*LatencyGroup)
	}{
		{"account", func(g *LatencyGroup) { g.AccountID++ }},
		{"public_model", func(g *LatencyGroup) { g.Model = "another-public-model" }},
		{"upstream_model", func(g *LatencyGroup) { g.UpstreamModel = "another-actual-model" }},
		{"fast", func(g *LatencyGroup) { g.ServiceTier = "priority" }},
		{"protocol", func(g *LatencyGroup) { g.UpstreamEndpoint = "/v1/messages" }},
		{"physical_endpoint", func(g *LatencyGroup) { g.TransportEndpoint = "messages" }},
		{"messages_source_same_endpoint", func(g *LatencyGroup) { g.CacheSource = "canonical_messages_cumulative_usage" }},
		{"gemini_source_same_endpoint", func(g *LatencyGroup) { g.CacheSource = "canonical_gemini_usage_metadata" }},
		{"chat_source_same_endpoint", func(g *LatencyGroup) { g.CacheSource = "canonical_chat_completions_usage" }},
		{"route", func(g *LatencyGroup) { g.RouteFingerprint = strings.Repeat("b", 64) }},
		{"proxy", func(g *LatencyGroup) { g.ProxyID++ }},
		{"body", func(g *LatencyGroup) { g.BodyBucket = "<64KiB" }},
		{"effort", func(g *LatencyGroup) { g.ReasoningEffort = "low" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			groups := coldStartGroups(now)
			tc.edit(&groups[1])
			annotateCacheColdStart(groups, LatencyReportFilter{Start: now.Add(-time.Hour), End: now}, now)
			for _, g := range groups {
				require.Equal(t, "insufficient_samples", g.ColdStart.Status)
				require.Nil(t, g.ColdStart.DeltaMS)
			}
		})
	}
}

func TestCacheColdStartNativeProtocolAssociations(t *testing.T) {
	for _, source := range []string{"canonical_messages_cumulative_usage", "canonical_gemini_usage_metadata", "canonical_chat_completions_usage"} {
		t.Run(source, func(t *testing.T) {
			now := time.Now().UTC()
			groups := coldStartGroups(now)
			for i := range groups {
				groups[i].CacheSource = source
			}
			annotateCacheColdStart(groups, LatencyReportFilter{Start: now.Add(-time.Hour), End: now}, now)
			for _, g := range groups {
				require.Equal(t, "historical_association", g.ColdStart.Status)
				require.Equal(t, "independent_history_p90_difference_not_causal", g.ColdStart.Method)
				require.InDelta(t, 5000, *g.ColdStart.DeltaMS, 1e-9)
			}
		})
	}
}

func TestCacheColdStartPhysicalEndpointAssociations(t *testing.T) {
	for _, endpoint := range []string{"responses", "responses_compact", "chat_completions", "messages", "gemini_generate_content", "gemini_stream_generate_content"} {
		t.Run(endpoint, func(t *testing.T) {
			now := time.Now().UTC()
			groups := coldStartGroups(now)
			for i := range groups {
				groups[i].TransportEndpoint = endpoint
			}
			annotateCacheColdStart(groups, LatencyReportFilter{Start: now.Add(-time.Hour), End: now}, now)
			for _, g := range groups {
				require.Equal(t, "historical_association", g.ColdStart.Status)
				require.InDelta(t, 5000, *g.ColdStart.DeltaMS, 1e-9)
			}
		})
	}
}

func TestCacheColdStartRejectsUncertainMeasurements(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		edit         func([]LatencyGroup)
	}{
		{"few_samples", "insufficient_samples", func(g []LatencyGroup) { g[1].TextWaitSamples = 4 }},
		{"stale", "stale_samples", func(g []LatencyGroup) { v := g[1].TextWaitLatest.Add(-16 * time.Minute); g[1].TextWaitLatest = &v }},
		{"future", "invalid_window", func(g []LatencyGroup) { v := g[1].TextWaitLatest.Add(time.Minute); g[1].TextWaitLatest = &v }},
		{"outside_filter", "invalid_window", func(g []LatencyGroup) { v := g[1].TextWaitLatest.Add(-2 * time.Hour); g[1].TextWaitLatest = &v }},
		{"missing_timestamp", "invalid_window", func(g []LatencyGroup) { g[1].TextWaitLatest = nil }},
		{"missing_p90", "invalid_measurement", func(g []LatencyGroup) { g[1].TextWaitP90MS = nil }},
		{"negative", "invalid_measurement", func(g []LatencyGroup) { v := -1.0; g[1].TextWaitP90MS = &v }},
		{"nan", "invalid_measurement", func(g []LatencyGroup) { v := math.NaN(); g[1].TextWaitP90MS = &v }},
		{"infinite", "invalid_measurement", func(g []LatencyGroup) { v := math.Inf(1); g[1].TextWaitP90MS = &v }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			groups := coldStartGroups(now)
			tc.edit(groups)
			annotateCacheColdStart(groups, LatencyReportFilter{Start: now.Add(-time.Hour), End: now}, now)
			for _, g := range groups {
				require.Equal(t, tc.status, g.ColdStart.Status)
				require.Nil(t, g.ColdStart.DeltaMS)
			}
		})
	}
	now := time.Now().UTC()
	groups := coldStartGroups(now)
	groups = append(groups, groups[1])
	annotateCacheColdStart(groups, LatencyReportFilter{Start: now.Add(-time.Hour), End: now}, now)
	for _, g := range groups {
		require.Equal(t, "ambiguous_cohort", g.ColdStart.Status)
		require.Nil(t, g.ColdStart.DeltaMS)
	}
}
