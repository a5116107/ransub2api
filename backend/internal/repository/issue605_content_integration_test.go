//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestIssue605WSTurnTimingPersistenceAndUsageIsolation(t *testing.T) {
	client := testEntClient(t)
	repo := newUsageLogRepositoryWithSQL(client, integrationDB)
	repo.timingOnce.Do(func() {})
	repo.timingQueue = make(chan timingWrite, 4)
	u := mustCreateUser(t, client, &service.User{Email: "ws-timing-" + uuid.NewString() + "@synthetic.invalid"})
	k := mustCreateApiKey(t, client, &service.APIKey{UserID: u.ID, Key: "ws-timing-fixture-" + uuid.NewString(), Name: "synthetic"})
	a := mustCreateAccount(t, client, &service.Account{Name: "ws-timing", Platform: service.PlatformOpenAI})
	// The real asynchronous writer needs committed usage rows. Reclaim only this
	// fixture's records so other integration suites keep their own totals.
	t.Cleanup(func() {
		for _, statement := range []struct {
			sql string
			id  int64
		}{
			{"DELETE FROM request_timing_details WHERE usage_log_id IN (SELECT id FROM usage_logs WHERE user_id=$1)", u.ID},
			{"DELETE FROM usage_logs WHERE user_id=$1", u.ID},
			{"DELETE FROM api_keys WHERE user_id=$1", u.ID},
			{"DELETE FROM accounts WHERE id=$1", a.ID},
			{"DELETE FROM users WHERE id=$1", u.ID},
		} {
			_, err := integrationDB.ExecContext(context.Background(), statement.sql, statement.id)
			require.NoError(t, err)
		}
	})
	parent := requesttiming.New(time.Now(), 0)
	ctx := requesttiming.With(t.Context(), parent)
	var ids []int64
	traceIDs := map[string]bool{}
	for turn := range 2 {
		requestID := fmt.Sprintf("ws-timing-%s-%d", uuid.NewString(), turn)
		usage := &service.UsageLog{RequestID: requestID, UserID: u.ID, APIKeyID: k.ID, AccountID: a.ID, Model: "fixture", Stream: true, InputTokens: 10, CreatedAt: time.Now()}
		_, err := repo.Create(t.Context(), usage)
		require.NoError(t, err)
		ids = append(ids, usage.ID)
		capture := requesttiming.NewWSTurn(time.Now(), a.ID, 0, [32]byte{1})
		capture.Content([]byte(`{"type":"response.reasoning_summary_text.delta","delta":"private-thought"}`))
		capture.Content([]byte(`{"type":"response.output_text.delta","delta":"private-answer"}`))
		time.Sleep(300 * time.Millisecond)
		capture.Content([]byte(`{"type":"response.output_text.delta","delta":"private-answer"}`))
		if turn == 0 {
			capture.Content([]byte(`{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"private-answer"}]}],"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":8},"output_tokens":120,"output_tokens_details":{"reasoning_tokens":20}}}}`))
		} else {
			capture.Content([]byte(`{"type":"response.completed"}`))
		}
		repo.RecordRequestTiming(requesttiming.WithUsageTurn(ctx, capture.Collector), requestID, k.ID)
		require.Empty(t, repo.timingQueue)
		capture.Finish(nil, false, false)
		select {
		case job := <-repo.timingQueue:
			require.Equal(t, requestID, job.requestID)
			require.NoError(t, repo.writeTiming(t.Context(), job))
		default:
			t.Fatal("missing finished turn persistence job")
		}
		traces, err := repo.RequestTimings(t.Context(), usage.ID)
		require.NoError(t, err)
		require.Len(t, traces, 1)
		var s requesttiming.Snapshot
		require.NoError(t, json.Unmarshal(traces[0], &s))
		require.False(t, traceIDs[s.TraceID])
		traceIDs[s.TraceID] = true
		require.Equal(t, "ws_turn", s.TTFTMode)
		require.NotContains(t, string(traces[0]), "private-")
		connections, err := repo.ListConnectionTraces(t.Context(), service.ConnectionTraceFilter{Start: time.Now().Add(-time.Minute), End: time.Now().Add(time.Minute), RequestID: requestID, UserID: u.ID, Page: 1, PageSize: 20})
		require.NoError(t, err)
		require.Len(t, connections.Items, 1)
		require.Empty(t, connections.Items[0].Method, "a physical WS generation does not prove an ingress HTTP method")
		require.Empty(t, connections.Items[0].HTTPVersion)
		require.Zero(t, connections.Items[0].Detail.Status)
	}
	parent.Finish(200, false)
	require.Empty(t, repo.timingQueue)
	items, err := repo.ReadUsageContentTimings(t.Context(), ids)
	require.NoError(t, err)
	require.Len(t, items, 2)
	for _, id := range ids {
		v := items[id]
		require.Equal(t, "typed_responses_ws", v.Source)
		require.Equal(t, "observed", v.TextStream.Status)
		require.Equal(t, 2, *v.TextStream.Chunks)
		require.NotNil(t, v.ReasoningFirstMS)
		require.NotNil(t, v.VisibleTextFirstMS)
		require.Nil(t, v.ResponseByteFirstMS)
		require.Equal(t, "physical_websocket_turn_responses_text_at_parser", v.OutputRate.Scope)
		require.Equal(t, "physical_websocket_turn_responses_terminal_usage", v.Cache.Scope)
		if id == ids[0] {
			require.Equal(t, "observed", v.Cache.Status)
			require.InDelta(t, 0.8, *v.Cache.ReadRatio, 1e-9)
			require.Equal(t, "estimated", v.OutputRate.Status)
			require.NotNil(t, v.OutputRate.TokensPerSecond)
		} else {
			require.Nil(t, v.OutputRate.TokensPerSecond, "the following turn must not borrow terminal usage")
			require.Nil(t, v.Cache.ReadRatio)
		}
	}
	groups, err := repo.ReadLatencyReport(t.Context(), service.LatencyReportFilter{Start: time.Now().Add(-time.Minute), End: time.Now().Add(time.Minute), UserID: u.ID})
	require.NoError(t, err)
	for _, g := range groups {
		require.Zero(t, g.ByteSamples)
		require.Zero(t, g.TextSamples, "WS parser offsets must not join HTTP latency cohorts")
		require.Nil(t, g.TextP90MS)
	}
}
