//go:build integration

package repository

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type latencyCacheFixture struct {
	tx    *dbent.Tx
	repo  *usageLogRepository
	usage service.UsageLog
	trace requesttiming.Snapshot
}

func newLatencyCacheFixture(t *testing.T) latencyCacheFixture {
	t.Helper()
	tx := testEntTx(t)
	client := tx.Client()
	u := mustCreateUser(t, client, &service.User{Email: "cache-cohorts@synthetic.invalid"})
	k := mustCreateApiKey(t, client, &service.APIKey{UserID: u.ID, Key: "synthetic-cache-cohorts", Name: "synthetic"})
	a := mustCreateAccount(t, client, &service.Account{Name: "synthetic-cache-cohorts"})
	upstream, tier, effort, endpoint := "actual-model", "default", "high", "/v1/responses"
	end := 8000.0
	return latencyCacheFixture{tx: tx, repo: newUsageLogRepositoryWithSQL(client, tx),
		usage: service.UsageLog{UserID: u.ID, APIKeyID: k.ID, AccountID: a.ID, Model: "cache-fixture", UpstreamModel: &upstream,
			ServiceTier: &tier, ReasoningEffort: &effort, UpstreamEndpoint: &endpoint, Stream: true,
			InputTokens: 100, CreatedAt: time.Now().UTC().Add(-time.Minute)},
		trace: requesttiming.Snapshot{Version: 1, Status: 200, BodyBytes: 2 << 20, TotalMS: end, Attempts: []requesttiming.Attempt{
			{Kind: "http", Number: 1, AccountID: a.ID, RouteFingerprint: strings.Repeat("a", 64), ProxyID: 0, StartMS: 0,
				EndMS: &end, Status: 200, Terminal: "completed", CacheUsage: &requesttiming.CacheUsage{Status: "observed", InputTokens: 100},
				Events: map[string]float64{"headers_written": 10, "request_written": 20, "first_visible_text": 6020}},
		}},
	}
}

func (f latencyCacheFixture) insert(t *testing.T) int64 {
	t.Helper()
	_, err := f.repo.Create(t.Context(), &f.usage)
	require.NoError(t, err)
	raw, err := json.Marshal(f.trace)
	require.NoError(t, err)
	_, err = f.tx.ExecContext(t.Context(), `INSERT INTO request_timing_details(usage_log_id,trace_id,detail) VALUES($1,$2,$3::jsonb)`, f.usage.ID, uuid.NewString(), string(raw))
	require.NoError(t, err)
	return f.usage.ID
}

func TestLatencyCacheCanonicalCountersAndBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, bucket, source string
		edit                 func(*latencyCacheFixture)
	}{
		{"explicit_miss", "0%", "canonical_responses_terminal_usage", func(*latencyCacheFixture) {}},
		{"explicit_hit", "95%+", "canonical_responses_terminal_usage", func(f *latencyCacheFixture) {
			f.usage.InputTokens, f.usage.CacheReadTokens, f.trace.Attempts[0].CacheUsage.ReadTokens = 5, 95, 95
		}},
		{"writes_in_denominator", "<80%", "canonical_responses_terminal_usage", func(f *latencyCacheFixture) {
			f.usage.InputTokens, f.usage.CacheReadTokens, f.usage.CacheCreationTokens = 1, 99, 1000
			f.trace.Attempts[0].CacheUsage = &requesttiming.CacheUsage{Status: "observed", InputTokens: 1100, ReadTokens: 99, WriteTokens: 1000, WriteReported: true}
		}},
		{"missing_read", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.trace.Attempts[0].CacheUsage.Status = "not_reported" }},
		{"legacy_zero_is_unknown", "unknown", "none", func(f *latencyCacheFixture) { f.trace.Attempts[0].CacheUsage = nil }},
		{"counter_mismatch", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.usage.InputTokens++ }},
		{"negative_counter", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.trace.Attempts[0].CacheUsage.ReadTokens = -1 }},
		{"excess_read", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.trace.Attempts[0].CacheUsage.ReadTokens = 101 }},
		{"negative_write", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) {
			f.trace.Attempts[0].CacheUsage.WriteReported = true
			f.trace.Attempts[0].CacheUsage.WriteTokens = -1
		}},
		{"wrong_account", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.trace.Attempts[0].AccountID++ }},
		{"wrong_attempt_kind", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.trace.Attempts[0].Kind = "egress" }},
		{"failed_terminal", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.trace.Attempts[0].Terminal = "failed" }},
		{"http_error", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.trace.Attempts[0].Status = 502 }},
		{"request_error", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.trace.Status = 502 }},
		{"canceled", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.trace.Canceled = true }},
		{"disconnected", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.trace.ClientDisconnect = true }},
		{"downstream_error", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.trace.DownstreamError = true }},
		{"capture_gap", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.trace.Attempts[0].Events["typed_content_truncated"] = 1 }},
		{"truncated", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.trace.Truncated = true }},
		{"transport_error", "unknown", "unverified_terminal_usage", func(f *latencyCacheFixture) { f.trace.Attempts[0].Error = "timeout" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLatencyCacheFixture(t)
			tc.edit(&f)
			f.insert(t)
			groups, err := f.repo.ReadLatencyReport(t.Context(), service.LatencyReportFilter{Start: f.usage.CreatedAt.Add(-time.Minute), End: time.Now().UTC(), UserID: f.usage.UserID})
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Equal(t, tc.bucket, groups[0].CacheBucket)
			require.Equal(t, tc.source, groups[0].CacheSource)
		})
	}
}

func TestLatencyCacheRecentWindowDoesNotBorrowOldSamples(t *testing.T) {
	f := newLatencyCacheFixture(t)
	now := time.Now().UTC()
	for _, read := range []int{0, 95} {
		for i := range 6 {
			f.usage.InputTokens, f.usage.CacheReadTokens = 100-read, read
			f.trace.Attempts[0].CacheUsage.ReadTokens = int64(read)
			f.usage.CreatedAt = now.Add(-time.Minute)
			if i < 5 {
				f.usage.CreatedAt = now.Add(-30 * time.Minute)
			}
			f.insert(t)
		}
	}
	groups, err := f.repo.ReadLatencyReport(t.Context(), service.LatencyReportFilter{Start: now.Add(-time.Hour), End: now, UserID: f.usage.UserID})
	require.NoError(t, err)
	require.Len(t, groups, 2)
	for _, g := range groups {
		require.EqualValues(t, 6, g.Requests, "the report retains the requested historical population")
		require.EqualValues(t, 1, g.TextWaitSamples, "one recent sample must not make five old samples look fresh")
		require.Equal(t, now.Add(-time.Minute).Unix(), g.TextWaitLatest.Unix())
	}
}

func TestLatencyCacheCohortsKeepRouteModelTierAndProtocolSeparate(t *testing.T) {
	f := newLatencyCacheFixture(t)
	now := time.Now().UTC()
	f.insert(t)
	for _, edit := range []func(*latencyCacheFixture){
		func(f *latencyCacheFixture) { v := "another-model"; f.usage.UpstreamModel = &v },
		func(f *latencyCacheFixture) { v := "priority"; f.usage.ServiceTier = &v },
		func(f *latencyCacheFixture) { v := "/v1/messages"; f.usage.UpstreamEndpoint = &v },
		func(f *latencyCacheFixture) { f.trace.Attempts[0].RouteFingerprint = strings.Repeat("b", 64) },
		func(f *latencyCacheFixture) { f.trace.Attempts[0].ProxyID = 9 },
		func(f *latencyCacheFixture) { f.usage.Stream = false },
	} {
		copy := f
		copy.trace.Attempts = append([]requesttiming.Attempt(nil), f.trace.Attempts...)
		edit(&copy)
		copy.insert(t)
	}
	groups, err := f.repo.ReadLatencyReport(t.Context(), service.LatencyReportFilter{Start: now.Add(-time.Hour), End: now, UserID: f.usage.UserID})
	require.NoError(t, err)
	require.Len(t, groups, 7)
	for _, g := range groups {
		require.EqualValues(t, 1, g.Requests)
	}
}

func TestLatencyCacheOnlyFinalAttemptCanOwnCacheEvidence(t *testing.T) {
	f := newLatencyCacheFixture(t)
	previous := f.trace.Attempts[0]
	previous.AccountID++
	previous.CacheUsage = &requesttiming.CacheUsage{Status: "observed", InputTokens: 100, ReadTokens: 95}
	f.trace.Attempts = append([]requesttiming.Attempt{previous}, f.trace.Attempts...)
	f.trace.Attempts[1].Number = 2
	id := f.insert(t)
	filter := service.LatencyReportFilter{Start: f.usage.CreatedAt.Add(-time.Minute), End: time.Now().UTC(), UserID: f.usage.UserID}
	groups, err := f.repo.ReadLatencyReport(t.Context(), filter)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.Equal(t, "0%", groups[0].CacheBucket, "a previous account's hit must not be assigned to the final request")
	require.EqualValues(t, 1, groups[0].TextWaitSamples)
	raw, err := json.Marshal(f.trace)
	require.NoError(t, err)
	_, err = f.tx.ExecContext(t.Context(), `INSERT INTO request_timing_details(usage_log_id,trace_id,detail) VALUES($1,$2,$3::jsonb)`, id, uuid.NewString(), string(raw))
	require.NoError(t, err)
	groups, err = f.repo.ReadLatencyReport(t.Context(), filter)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.EqualValues(t, 1, groups[0].Requests)
	require.EqualValues(t, 2, groups[0].Traces)
	require.Equal(t, "unknown", groups[0].CacheBucket, "multiple traces cannot independently claim one billed usage record")
	require.Equal(t, "unverified_terminal_usage", groups[0].CacheSource)
}
