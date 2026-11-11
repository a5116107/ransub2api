package service

import (
	"context"
	"encoding/hex"
	"math"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
)

type LatencyReportFilter struct {
	Start           time.Time
	End             time.Time
	UserID          int64
	APIKeyID        int64
	AccountID       int64
	GroupID         int64
	Model           string
	Platform        string
	IncludeUnbilled bool
}

type LatencyGroup struct {
	UpstreamModel     string                 `json:"upstream_model"`
	ServiceTier       string                 `json:"service_tier"`
	UpstreamEndpoint  string                 `json:"upstream_endpoint"`
	RouteFingerprint  string                 `json:"route_fingerprint"`
	ProxyID           int64                  `json:"proxy_id"`
	Stream            bool                   `json:"stream"`
	CacheSource       string                 `json:"cache_source"`
	TextWaitSamples   int64                  `json:"text_wait_samples"`
	TextWaitP90MS     *float64               `json:"post_upload_text_p90_ms"`
	TextWaitLatest    *time.Time             `json:"text_wait_latest_at"`
	ColdStart         CacheColdStartEvidence `json:"cold_start"`
	AccountID         int64                  `json:"account_id"`
	Model             string                 `json:"model"`
	ReasoningSamples  int64                  `json:"reasoning_samples"`
	TextSamples       int64                  `json:"text_samples"`
	ByteSamples       int64                  `json:"byte_samples"`
	ReasoningAvgMS    *float64               `json:"reasoning_first_avg_ms"`
	TextAvgMS         *float64               `json:"visible_text_first_avg_ms"`
	ByteAvgMS         *float64               `json:"response_byte_first_avg_ms"`
	TextP50MS         *float64               `json:"visible_text_p50_ms"`
	TextP90MS         *float64               `json:"visible_text_p90_ms"`
	TextP99MS         *float64               `json:"visible_text_p99_ms"`
	BodyBucket        string                 `json:"body_bucket"`
	CacheBucket       string                 `json:"cache_bucket"`
	ReasoningEffort   string                 `json:"reasoning_effort"`
	Requests          int64                  `json:"requests"`
	Traces            int64                  `json:"traces"`
	Attempts          int64                  `json:"attempts"`
	UploadSamples     int64                  `json:"upload_samples"`
	WaitSamples       int64                  `json:"wait_samples"`
	VisibleSamples    int64                  `json:"visible_samples"`
	UploadAvgMS       *float64               `json:"body_upload_avg_ms"`
	WaitAvgMS         *float64               `json:"post_upload_first_content_wait_avg_ms"`
	VisibleAvgMS      *float64               `json:"post_upload_first_visible_wait_avg_ms"`
	LargeBodySwitches int64                  `json:"large_body_switches"`
	FailoverSamples   int64                  `json:"failover_samples"`
	LargeBodyRetries  int64                  `json:"large_body_same_account_retries"`
	CostSamples       int64                  `json:"cost_samples"`
	ExtraUploadMS     *float64               `json:"extra_upload_component_ms"`
	ExtraWaitMS       *float64               `json:"extra_wait_component_ms"`
}

// CacheColdStartEvidence compares independent historical populations. It is
// not a same-prompt experiment, a cache TTL, or promised migration savings.
type CacheColdStartEvidence struct {
	Status        string     `json:"status"`
	Method        string     `json:"method"`
	MinSamples    int64      `json:"min_samples"`
	MaxAgeSeconds int64      `json:"max_age_seconds"`
	ColdSamples   int64      `json:"cold_samples"`
	WarmSamples   int64      `json:"warm_samples"`
	ColdP90MS     *float64   `json:"cold_p90_ms"`
	WarmP90MS     *float64   `json:"warm_p90_ms"`
	DeltaMS       *float64   `json:"delta_ms"`
	AsOf          time.Time  `json:"as_of"`
	LatestColdAt  *time.Time `json:"latest_cold_at"`
	LatestWarmAt  *time.Time `json:"latest_warm_at"`
}

type LatencyReport struct {
	ContentCapabilities requesttiming.ContentObservation `json:"content_capabilities"`
	Groups              []LatencyGroup                   `json:"groups"`
	EndToEndMS          *float64                         `json:"end_to_end_ms"`
	EndToEndStatus      string                           `json:"end_to_end_status"`
	EndToEndSource      string                           `json:"end_to_end_source"`
	ClientUploadMS      *float64                         `json:"client_upload_ms"`
	ClientUploadStatus  string                           `json:"client_upload_status"`
	Scope               string                           `json:"scope"`
}

type latencyReportReader interface {
	ReadLatencyReport(context.Context, LatencyReportFilter) ([]LatencyGroup, error)
}

func readLatencyReport(ctx context.Context, reader any, f LatencyReportFilter) (*LatencyReport, error) {
	if !f.End.After(f.Start) || f.End.Sub(f.Start) > 31*24*time.Hour {
		return nil, infraerrors.BadRequest("LATENCY_RANGE_INVALID", "Latency range must be positive and at most 31 days")
	}
	r, ok := reader.(latencyReportReader)
	if !ok {
		return nil, infraerrors.ServiceUnavailable("LATENCY_UNAVAILABLE", "Latency reporting unavailable")
	}
	groups, err := r.ReadLatencyReport(ctx, f)
	if err != nil {
		return nil, err
	}
	annotateCacheColdStart(groups, f, time.Now().UTC())
	return &LatencyReport{ContentCapabilities: requesttiming.UnknownContentObservation(), Groups: groups, EndToEndStatus: "not_collected", EndToEndSource: "none", ClientUploadStatus: "not_collected", Scope: "application_http_attempts_components_not_wall_time"}, nil
}

func annotateCacheColdStart(groups []LatencyGroup, f LatencyReportFilter, now time.Time) {
	type cohort struct {
		account, proxy                                               int64
		model, upstream, tier, endpoint, route, body, effort, source string
		stream                                                       bool
	}
	type pair struct {
		cold, warm *LatencyGroup
		ambiguous  bool
	}
	asOf := f.End
	if now.Before(asOf) {
		asOf = now
	}
	pairs := make(map[cohort]*pair)
	keys := make(map[int]cohort)
	for i := range groups {
		g := &groups[i]
		g.ColdStart = CacheColdStartEvidence{Status: "unknown_scope", Method: "independent_history_p90_difference_not_causal", MinSamples: 5, MaxAgeSeconds: 900, AsOf: asOf}
		route, err := hex.DecodeString(g.RouteFingerprint)
		canonical := g.CacheSource == "canonical_responses_terminal_usage" || g.CacheSource == "canonical_messages_cumulative_usage" ||
			g.CacheSource == "canonical_gemini_usage_metadata" || g.CacheSource == "canonical_chat_completions_usage"
		if !canonical || err != nil || len(route) != 32 || [32]byte(route) == [32]byte{} ||
			g.AccountID <= 0 || g.Model == "" || g.Model == "unknown" || g.UpstreamModel == "" || g.UpstreamModel == "unknown" ||
			g.ServiceTier == "" || g.ServiceTier == "unknown" || g.ServiceTier == "unspecified" ||
			g.UpstreamEndpoint == "" || g.UpstreamEndpoint == "unknown" || g.BodyBucket == "" || g.BodyBucket == "unknown" ||
			g.ReasoningEffort == "" || g.ReasoningEffort == "unknown" || g.ProxyID < 0 || !g.Stream {
			continue
		}
		key := cohort{g.AccountID, g.ProxyID, g.Model, g.UpstreamModel, g.ServiceTier, g.UpstreamEndpoint, g.RouteFingerprint, g.BodyBucket, g.ReasoningEffort, g.CacheSource, g.Stream}
		keys[i] = key
		p := pairs[key]
		if p == nil {
			p = &pair{}
			pairs[key] = p
		}
		switch g.CacheBucket {
		case "0%":
			p.ambiguous = p.ambiguous || p.cold != nil
			p.cold = g
		case "95%+":
			p.ambiguous = p.ambiguous || p.warm != nil
			p.warm = g
		}
	}
	for i, key := range keys {
		p, out := pairs[key], &groups[i].ColdStart
		out.Status = "insufficient_samples"
		if p.cold != nil {
			out.ColdSamples, out.LatestColdAt = p.cold.TextWaitSamples, p.cold.TextWaitLatest
		}
		if p.warm != nil {
			out.WarmSamples, out.LatestWarmAt = p.warm.TextWaitSamples, p.warm.TextWaitLatest
		}
		if p.ambiguous {
			out.Status = "ambiguous_cohort"
			continue
		}
		if p.cold == nil || p.warm == nil || out.ColdSamples < out.MinSamples || out.WarmSamples < out.MinSamples {
			continue
		}
		if out.LatestColdAt == nil || out.LatestWarmAt == nil || out.LatestColdAt.Before(f.Start) || out.LatestWarmAt.Before(f.Start) ||
			out.LatestColdAt.After(asOf) || out.LatestWarmAt.After(asOf) {
			out.Status = "invalid_window"
			continue
		}
		if asOf.Sub(*out.LatestColdAt) > 15*time.Minute || asOf.Sub(*out.LatestWarmAt) > 15*time.Minute {
			out.Status = "stale_samples"
			continue
		}
		cold, warm := p.cold.TextWaitP90MS, p.warm.TextWaitP90MS
		if cold == nil || warm == nil || math.IsNaN(*cold) || math.IsNaN(*warm) || math.IsInf(*cold, 0) || math.IsInf(*warm, 0) || *cold < 0 || *warm < 0 {
			out.Status = "invalid_measurement"
			continue
		}
		delta := *cold - *warm
		out.ColdP90MS, out.WarmP90MS, out.DeltaMS = cold, warm, &delta
		out.Status = "historical_association"
	}
}

func (s *UsageService) LatencyReport(ctx context.Context, f LatencyReportFilter) (*LatencyReport, error) {
	return readLatencyReport(ctx, s.usageRepo, f)
}

func (s *OpsService) LatencyReport(ctx context.Context, f LatencyReportFilter) (*LatencyReport, error) {
	if err := s.RequireMonitoringEnabled(ctx); err != nil {
		return nil, err
	}
	f.IncludeUnbilled = true
	return readLatencyReport(ctx, s.opsRepo, f)
}
