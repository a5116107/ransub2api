package repository

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/stretchr/testify/require"
)

func TestTimingWriteReplacesUntrustedCacheMigrationProjection(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	end, earlier := 1000.0, 100.0
	s := requesttiming.Snapshot{Version: 1, Status: 200, Terminal: "completed", Outcome: "success", TotalMS: end,
		TraceID: "00000000-0000-0000-0000-000000000001", Attempts: []requesttiming.Attempt{
			{Kind: "http", Number: 1, AccountID: 7, RouteFingerprint: strings.Repeat("a", 64), EndMS: &earlier},
			{Kind: "http", Number: 2, AccountID: 8, RouteFingerprint: strings.Repeat("b", 64), StartMS: 200, EndMS: &end,
				UpstreamCredential: &requesttiming.UpstreamCredentialObservation{Status: "observed", Source: "http_transport_handoff", Fingerprint: strings.Repeat("c", 64)},
				Status:             200, Terminal: "completed", CacheUsage: &requesttiming.CacheUsage{Status: "observed", InputTokens: 1000, ReadTokens: 900}},
		}}
	s.Latency = &requesttiming.LatencySummary{CacheMigration: requesttiming.CacheMigrationObservation{Status: "forged"}}
	want := s
	derived := requesttiming.DeriveLatency(s)
	require.Equal(t, "bounded_attribution_unknown", derived.CacheMigration.Status)
	want.Latency = &derived
	raw, err := json.Marshal(want)
	require.NoError(t, err)
	mock.ExpectExec("INSERT INTO request_timing_details").WithArgs(s.TraceID, string(raw), "client:migration", int64(8)).WillReturnResult(sqlmock.NewResult(0, 1))
	repo := newUsageLogRepositoryWithSQL(nil, db)
	require.NoError(t, repo.writeTiming(t.Context(), timingWrite{requestID: "client:migration", apiKeyID: 8, data: s}))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestTimingWriteJoinsBillingIdentityAndKeepsDistinctTraces(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	repo := newUsageLogRepositoryWithSQL(nil, db)
	for _, traceID := range []string{"00000000-0000-0000-0000-000000000001", "00000000-0000-0000-0000-000000000002"} {
		mock.ExpectExec("INSERT INTO request_timing_details").WithArgs(traceID, sqlmock.AnyArg(), "client:repeat", int64(8)).WillReturnResult(sqlmock.NewResult(0, 1))
		if err := repo.writeTiming(context.Background(), timingWrite{requestID: "client:repeat", apiKeyID: 8, data: requesttiming.Snapshot{TraceID: traceID}}); err != nil {
			t.Fatal(err)
		}
	}
	mock.ExpectQuery("SELECT detail FROM request_timing_details WHERE usage_log_id").WithArgs(int64(9)).WillReturnRows(sqlmock.NewRows([]string{"detail"}).AddRow(`{"trace_id":"one"}`).AddRow(`{"trace_id":"two"}`))
	got, err := repo.RequestTimings(context.Background(), 9)
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v %v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestTimingBindingWSUsesExactlyOneCollectorPerUsage(t *testing.T) {
	for _, finishBeforeBinding := range []bool{false, true} {
		t.Run(map[bool]string{false: "bind first", true: "finish first"}[finishBeforeBinding], func(t *testing.T) {
			db, _, err := sqlmock.New()
			require.NoError(t, err)
			defer func() { _ = db.Close() }()
			repo := &usageLogRepository{timingQueue: make(chan timingWrite, 4), db: db}
			repo.timingOnce.Do(func() {})
			parent := requesttiming.New(time.Now(), 0)
			ctx := requesttiming.With(t.Context(), parent)
			seen := map[string]bool{}
			for _, requestID := range []string{"turn-one", "turn-two"} {
				turn := requesttiming.NewWSTurn(time.Now(), 17, 0, [32]byte{1})
				turn.Content([]byte(`{"type":"response.output_text.delta","delta":"private-answer"}`))
				turn.Content([]byte(`{"type":"response.completed"}`))
				if finishBeforeBinding {
					turn.Finish(nil, false, false)
				}
				repo.RecordRequestTiming(requesttiming.WithUsageTurn(ctx, turn.Collector), requestID, 8)
				if !finishBeforeBinding {
					require.Empty(t, repo.timingQueue)
					turn.Finish(nil, false, false)
				}
				select {
				case job := <-repo.timingQueue:
					require.Equal(t, requestID, job.requestID)
					require.EqualValues(t, 8, job.apiKeyID)
					require.Equal(t, "ws_turn", job.data.TTFTMode)
					require.False(t, seen[job.data.TraceID])
					seen[job.data.TraceID] = true
					require.Len(t, job.data.Attempts, 1)
					require.Equal(t, "observed", requesttiming.DeriveContentObservation(job.data).TextStream.Status)
				default:
					t.Fatal("turn diagnostic missing")
				}
			}
			parent.Finish(200, false)
			require.Empty(t, repo.timingQueue, "the enclosing request must not create another trace for either usage row")
		})
	}
}
func TestTimingBindingDoesNotWriteUntilRequestFinished(t *testing.T) {
	// A manually supplied queue exercises lifecycle without leaking a worker.
	repo := &usageLogRepository{timingQueue: make(chan timingWrite, 1)}
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	repo.db = db
	repo.timingOnce.Do(func() {})
	c := requesttiming.New(time.Now(), 0)
	ctx := requesttiming.With(context.Background(), c)
	repo.RecordRequestTiming(ctx, "client:r", 3)
	select {
	case <-repo.timingQueue:
		t.Fatal("premature snapshot")
	default:
	}
	c.Finish(200, false)
	select {
	case job := <-repo.timingQueue:
		raw, err := json.Marshal(job.data)
		if err != nil || len(raw) == 0 || job.data.Status != 200 {
			t.Fatal("invalid final snapshot")
		}
	default:
		t.Fatal("missing final snapshot")
	}
}
