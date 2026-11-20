package repository

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func expectAccountGroupBindingsAccount(mock sqlmock.Sqlmock, id int64) {
	mock.ExpectQuery(`SELECT id FROM accounts.*FOR NO KEY UPDATE`).WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(id))
}

func TestOAuthDegradationAuditDoesNotBlockQualityGroupRestore(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	mock.ExpectBegin()
	tx, err := db.Begin()
	require.NoError(t, err)
	expectAccountGroupBindingsAccount(mock, 8)
	state := qualityState{Removed: []qualityGroup{{AccountID: 8, GroupID: 21}}}
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM groups`).WithArgs(int64(21), int64(8)).
		WillReturnRows(sqlmock.NewRows([]string{"exists", "replaced"}).AddRow(true, false))
	mock.ExpectExec(`INSERT INTO account_groups`).WithArgs(int64(8), int64(21), 0, state.Removed[0].CreatedAt, nil, nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE accounts SET updated_at=clock_timestamp\(\)`).WithArgs(int64(8)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	conflict, err := restoreQualityGroups(context.Background(), tx, 8, &state)
	require.NoError(t, err)
	require.False(t, conflict)
	require.Nil(t, state.RestoreDiagnostic, "historical group removal is not an active binding restriction")
	mock.ExpectRollback()
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}
