package repository

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type qualityAPIKeyPendingState struct{}

func (qualityAPIKeyPendingState) Match(value driver.Value) bool {
	raw, ok := value.(string)
	if !ok {
		return false
	}
	var state qualityState
	return json.Unmarshal([]byte(raw), &state) == nil && state.Action == service.QualityActionRemoveModel && state.Quality5xxEpisode == 1 && !state.NativeRecovery && state.PreviousConcurrency == nil && state.AppliedConcurrency == nil && len(state.ModelApplied) == 0
}

func TestQuality5xxAPIKeyImmediateEpisodePreservesNativeSettings(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC()
	cfg := `{"question_kind":"candy","parallel_count":1,"quality":{"action":"remove_models","remove_models":["target"],"trigger_on_upstream_5xx":true}}`
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id,account_id,model_id").WithArgs(int64(7), int64(43)).WillReturnRows(sqlmock.NewRows([]string{"id", "account_id", "model_id", "cron_expression", "enabled", "max_results", "auto_recover", "last_run_at", "next_run_at", "created_at", "updated_at", "pelican_config", "running_until"}).AddRow(7, 43, "target", "*/30 * * * *", true, 100, false, nil, now.Add(time.Hour), now, now, cfg, now.Add(time.Minute)))
	mock.ExpectQuery("SELECT type,status FROM accounts").WithArgs(int64(43)).WillReturnRows(sqlmock.NewRows([]string{"type", "status"}).AddRow(service.AccountTypeAPIKey, "active"))
	mock.ExpectQuery("SELECT state FROM account_quality_states").WithArgs(int64(7)).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT EXISTS").WithArgs(int64(43), int64(7)).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectExec("SELECT set_config").WithArgs("7").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT platform,type,concurrency").WithArgs(int64(43)).WillReturnRows(sqlmock.NewRows([]string{"platform", "type", "concurrency", "credentials", "extra"}).AddRow("openai", "apikey", 17, `{}`, `{"unrelated":"kept"}`))
	mock.ExpectExec("INSERT INTO account_quality_states").WithArgs(int64(7), qualityAPIKeyPendingState{}).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE scheduled_test_plans SET next_run_at").WithArgs(int64(7), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	applied, err := (&scheduledTestPlanRepository{db: db}).applyImmediateAccountFault(t.Context(), 7, 43, true)
	require.NoError(t, err)
	require.True(t, applied)
	require.NoError(t, mock.ExpectationsWereMet(), "API Key protection must not write OAuth concurrency/ramp or fabricate a failed quality result")
}
