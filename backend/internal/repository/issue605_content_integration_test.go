//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestIssue605UsageContentBatchAndLayeredSQL(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	repo := newUsageLogRepositoryWithSQL(client, tx)
	u := mustCreateUser(t, client, &service.User{Email: "issue605@synthetic.invalid"})
	k := mustCreateApiKey(t, client, &service.APIKey{UserID: u.ID, Key: "synthetic605", Name: "synthetic"})
	a := mustCreateAccount(t, client, &service.Account{Name: "synthetic605", Platform: service.PlatformOpenAI})
	now := time.Now().UTC().Add(-time.Minute)
	end := 50.0
	var ids []int64
	for i, effort := range []string{"xhigh", "medium", "low"} {
		latency := 100 + i*200
		usage := &service.UsageLog{UserID: u.ID, APIKeyID: k.ID, AccountID: a.ID, Model: "fixture", Stream: true, InputTokens: 1, FirstTokenMs: &latency, ReasoningEffort: &effort, CreatedAt: now}
		_, err := repo.Create(ctx, usage)
		require.NoError(t, err)
		ids = append(ids, usage.ID)
		s := requesttiming.Snapshot{Status: 200, TotalMS: 1000, Attempts: []requesttiming.Attempt{{Kind: "http", AccountID: a.ID, StartMS: 0, EndMS: &end, ProxyID: 2, Status: 200, Terminal: "completed",
			Events:     map[string]float64{"first_byte": 10, "first_reasoning": float64(latency), "first_visible_text": float64(latency + 100)},
			TextStream: &requesttiming.TextStreamTiming{Chunks: 2, FirstMS: float64(latency + 100), LastMS: float64(latency + 200), MaxGapMS: 100, Terminal: "completed", TerminalMS: float64(latency + 250)},
		}}}
		if i == 1 {
			s.Attempts[0].TextStream.Protocol = "messages"
		}
		if i == 2 {
			s.Attempts[0].TextStream.Protocol = "gemini"
			s.Attempts[0].Events["typed_gemini_content"] = 1
		}
		raw, err := json.Marshal(s)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `INSERT INTO request_timing_details(usage_log_id,trace_id,detail)VALUES($1,$2,$3::jsonb)`, usage.ID, uuid.NewString(), string(raw))
		require.NoError(t, err)
	}
	items, err := repo.ReadUsageContentTimings(ctx, ids)
	require.NoError(t, err)
	require.Equal(t, 100.0, *items[ids[0]].ReasoningFirstMS)
	require.Equal(t, 200.0, *items[ids[0]].VisibleTextFirstMS)
	require.Equal(t, 10.0, *items[ids[0]].ResponseByteFirstMS)
	require.Equal(t, "proxy_obscured", items[ids[0]].UpstreamSocket.Status)
	for _, id := range ids {
		stream := items[id].TextStream
		require.Equal(t, "observed", stream.Status)
		require.Equal(t, 2, *stream.Chunks)
		require.Equal(t, 1, *stream.GapSamples)
		require.Equal(t, 100.0, *stream.SpanMS)
		require.Equal(t, 100.0, *stream.MaxGapMS)
		require.Equal(t, 50.0, *stream.TailWaitMS)
	}
	require.Equal(t, "typed_responses_sse", items[ids[0]].TextStream.Source)
	require.Equal(t, "typed_messages_sse", items[ids[1]].TextStream.Source)
	require.Equal(t, "final_physical_http_messages_text_deltas_at_parser", items[ids[1]].TextStream.Scope)
	require.Nil(t, items[ids[1]].OutputRate.TokensPerSecond)
	require.Equal(t, "typed_gemini_content", items[ids[2]].TextStream.Source)
	require.Equal(t, "final_physical_http_gemini_primary_text_chunks_at_parser", items[ids[2]].TextStream.Scope)
	require.Nil(t, items[ids[2]].OutputRate.TokensPerSecond)
	require.Nil(t, items[ids[2]].Cache.ReadRatio)
	groups, err := repo.ReadLatencyReport(ctx, service.LatencyReportFilter{Start: now.Add(-time.Minute), End: now.Add(time.Minute), UserID: u.ID})
	require.NoError(t, err)
	require.Len(t, groups, 3)
	for _, g := range groups {
		require.Equal(t, a.ID, g.AccountID)
		require.Equal(t, "fixture", g.Model)
		require.EqualValues(t, 1, g.ReasoningSamples)
		require.EqualValues(t, 1, g.TextSamples)
		require.EqualValues(t, 1, g.ByteSamples)
		require.NotNil(t, g.TextP90MS)
	}
	// A diagnostic cap may hide a text event. Neither per-row projection nor
	// layered SQL may publish a fabricated first text or inter-delta timeline.
	_, err = tx.ExecContext(ctx, `UPDATE request_timing_details SET detail=jsonb_set(detail,'{attempts,0,events,typed_content_truncated}','999'::jsonb) WHERE usage_log_id=$1`, ids[0])
	require.NoError(t, err)
	items, err = repo.ReadUsageContentTimings(ctx, ids)
	require.NoError(t, err)
	require.Nil(t, items[ids[0]].ReasoningFirstMS)
	require.Nil(t, items[ids[0]].VisibleTextFirstMS)
	require.Equal(t, 10.0, *items[ids[0]].ResponseByteFirstMS, "HTTP byte evidence is independent of the content parser cap")
	require.Equal(t, "incomplete_capture", items[ids[0]].TextStream.Status)
	require.Nil(t, items[ids[0]].TextStream.Chunks)
	require.Nil(t, items[ids[0]].TextStream.MaxGapMS)
	groups, err = repo.ReadLatencyReport(ctx, service.LatencyReportFilter{Start: now.Add(-time.Minute), End: now.Add(time.Minute), UserID: u.ID})
	require.NoError(t, err)
	for _, g := range groups {
		if g.ReasoningEffort == "xhigh" {
			require.Zero(t, g.ReasoningSamples)
			require.Zero(t, g.TextSamples)
			require.Nil(t, g.TextP90MS)
			require.EqualValues(t, 1, g.ByteSamples)
		}
	}
	effort := "xhigh"
	signals, err := repo.ReadPrioritySchedulingSignals(ctx, service.PrioritySchedulingQuery{AccountIDs: []int64{a.ID}, Model: "fixture", UsageSince: now.Add(-time.Minute), UsageUntil: now.Add(time.Minute), QualitySince: now.Add(-time.Minute), ObservationEffort: &effort})
	require.NoError(t, err)
	require.EqualValues(t, 1, signals[a.ID].Samples)
	require.Equal(t, 100.0, signals[a.ID].P90TTFTMs, "different effort must not enter the priority history latency population")
	// A failed earlier attempt on another account must not enter the billed
	// account's typed latency population, even though upload components remain.
	mixed := requesttiming.Snapshot{TotalMS: 1000, Attempts: []requesttiming.Attempt{
		{Kind: "http", AccountID: a.ID + 99, StartMS: 0, EndMS: &end, Events: map[string]float64{"first_reasoning": 900, "first_visible_text": 950}},
		{Kind: "http", AccountID: a.ID, StartMS: 0, EndMS: &end, Events: map[string]float64{"first_byte": 10, "first_reasoning": 100, "first_visible_text": 200}},
	}}
	payload, err := json.Marshal(mixed)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `UPDATE request_timing_details SET detail=$2::jsonb WHERE usage_log_id=$1`, ids[0], string(payload))
	require.NoError(t, err)
	items, err = repo.ReadUsageContentTimings(ctx, ids)
	require.NoError(t, err)
	require.Equal(t, "not_collected", items[ids[0]].TextStream.Status, "historical JSON must not invent zero gaps or inherit earlier attempts")
	require.Nil(t, items[ids[0]].TextStream.MaxGapMS)
	groups, err = repo.ReadLatencyReport(ctx, service.LatencyReportFilter{Start: now.Add(-time.Minute), End: now.Add(time.Minute), UserID: u.ID})
	require.NoError(t, err)
	for _, g := range groups {
		if g.ReasoningEffort == "xhigh" {
			require.EqualValues(t, 1, g.TextSamples)
			require.Equal(t, 200.0, *g.TextP90MS, "only final matching-account content can enter account/model/effort quantiles")
		}
	}

	_, err = tx.ExecContext(ctx, `INSERT INTO request_timing_details(usage_log_id,trace_id,detail)VALUES($1,$2,'{}'::jsonb)`, ids[0], uuid.NewString())
	require.NoError(t, err)
	items, err = repo.ReadUsageContentTimings(ctx, ids)
	require.NoError(t, err)
	require.Nil(t, items[ids[0]].ReasoningFirstMS, "multiple traces for one billing row cannot invent one timeline")
	require.Equal(t, "not_collected", items[ids[0]].TextStream.Status)
	require.Nil(t, items[ids[0]].TextStream.Chunks)
}
