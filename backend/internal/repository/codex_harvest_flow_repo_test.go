package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestHarvestFlowAppendInsertsThenCaps(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	at := time.Date(2026, 9, 21, 4, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO codex_harvest_flow_events").
		WithArgs("evt-1", at, "probe", "probe_hit", int64(2), "20x", "gpt-6-astra", "japan-08", 200, 292, 10, 292, 10, true, false, "success", "", "", nil).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("DELETE FROM codex_harvest_flow_events").
		WithArgs(harvestFlowPersistCap).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
	require.NoError(t, NewCodexHarvestFlowRepository(db).Append(context.Background(), service.CodexHarvestFlowEvent{
		ID: "evt-1", At: at, Stage: "probe", Kind: "probe_hit", AccountID: 2, AccountName: "20x",
		Model: "gpt-6-astra", Node: "japan-08", HTTPStatus: 200, Length: 292, Blocks: 10,
		ExpectedLength: 292, ExpectedBlocks: 10, Accepted: true, Result: "success",
	}))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHarvestFlowListReturnsChronological(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	newer := time.Date(2026, 9, 21, 4, 1, 0, 0, time.UTC)
	older := time.Date(2026, 9, 21, 4, 0, 0, 0, time.UTC)
	mock.ExpectQuery("SELECT event_id, at, stage, kind").
		WithArgs(harvestFlowPersistCap).
		WillReturnRows(sqlmock.NewRows([]string{
			"event_id", "at", "stage", "kind", "account_id", "account_name", "model", "node",
			"http_status", "length", "blocks", "expected_length", "expected_blocks", "accepted", "standby", "result", "reason", "detail", "budget_deferral",
		}).AddRow("evt-2", newer, "select", "candy_deferred", int64(2), "20x", "gpt-6-astra", "japan-08", 0, 0, 0, 0, 0, false, false, "", "", "", `{"reason":"request_limit","scope":"key","retry_at":"2026-09-21T04:02:00Z"}`).
			AddRow("evt-1", older, "probe", "probe_hit", int64(2), "20x", "gpt-6-astra", "japan-08", 200, 292, 10, 292, 10, true, false, "success", "", "", nil))
	events, err := NewCodexHarvestFlowRepository(db).List(context.Background(), 200)
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Equal(t, "evt-1", events[0].ID)
	require.Equal(t, "evt-2", events[1].ID)
	require.Nil(t, events[0].BudgetDeferral)
	require.Equal(t, "key", events[1].BudgetDeferral.Scope)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHarvestFlowAppendRejectsEmptyID(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	require.Error(t, NewCodexHarvestFlowRepository(db).Append(context.Background(), service.CodexHarvestFlowEvent{}))
	require.NoError(t, mock.ExpectationsWereMet())
}
