package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestNewAPIAuthorizationSaveRollsBackChangedAccount(t *testing.T) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	defer func() { _ = db.Close() }()
	r := NewNewAPIAuthorizationRepository(db)
	m.ExpectBegin()
	m.ExpectExec("SELECT pg_advisory_xact_lock").WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectQuery("SELECT id, revision").WithArgs("https://example.com", int64(7)).WillReturnRows(sqlmock.NewRows([]string{"id", "revision"}))
	m.ExpectQuery("SELECT platform, type, credentials, proxy_id").WithArgs(int64(1)).WillReturnRows(sqlmock.NewRows([]string{"platform", "type", "credentials", "proxy_id"}).AddRow("openai", "apikey", `{"api_key":"changed","base_url":"https://example.com/v1"}`, nil))
	m.ExpectRollback()
	e = r.Save(context.Background(), &service.NewAPISiteAuthorization{SiteURL: "https://example.com", UserID: 7, Ciphertext: "encrypted"}, []service.NewAPIBindingSave{{Account: &service.Account{ID: 1, Platform: "openai", Type: "apikey", Credentials: map[string]any{"api_key": "original", "base_url": "https://example.com/v1"}}, TokenID: 3}})
	require.ErrorIs(t, e, service.ErrUpstreamBillingProbeIdentityChanged)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestNewAPIAuthorizationSaveUsesEffectiveProtocolSite(t *testing.T) {
	for _, site := range []string{"https://example.com", "https://stale.example"} {
		t.Run(site, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer func() { _ = db.Close() }()
			account := &service.Account{ID: 1, Platform: service.PlatformOpenCodeGo, Type: service.AccountTypeAPIKey,
				Credentials: map[string]any{
					"api_key": "fixture-key", "base_url": "https://stale.example/v1", "api_protocol": service.APIProtocolAdaptive,
					"api_base_urls": map[string]any{service.APIProtocolChatCompletions: "https://example.com/v1"},
				},
			}
			credentials, err := json.Marshal(account.Credentials)
			require.NoError(t, err)
			mock.ExpectBegin()
			mock.ExpectExec("SELECT pg_advisory_xact_lock").WithArgs(site + ":7").WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectQuery("SELECT id, revision").WithArgs(site, int64(7)).WillReturnRows(sqlmock.NewRows([]string{"id", "revision"}))
			mock.ExpectQuery("SELECT platform, type, credentials, proxy_id").WithArgs(int64(1)).WillReturnRows(
				sqlmock.NewRows([]string{"platform", "type", "credentials", "proxy_id"}).AddRow(account.Platform, account.Type, string(credentials), nil))
			if site == "https://example.com" {
				mock.ExpectQuery("INSERT INTO new_api_site_authorizations").WithArgs(site, int64(7), "encrypted").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(11)))
				mock.ExpectQuery("UPDATE accounts a SET extra").WithArgs(int64(11)).WillReturnRows(sqlmock.NewRows([]string{"id"}))
				mock.ExpectExec("INSERT INTO new_api_account_bindings").WithArgs(int64(1), int64(11), int64(3), service.NewAPIAccountFingerprint(account), "vip").WillReturnResult(sqlmock.NewResult(1, 1))
				mock.ExpectExec("UPDATE accounts SET extra").WithArgs(int64(1), site, int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec("INSERT INTO scheduler_outbox").WithArgs(service.SchedulerOutboxEventAccountChanged, int64(1), nil, nil, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
				mock.ExpectCommit()
			} else {
				mock.ExpectRollback()
			}
			err = NewNewAPIAuthorizationRepository(db).Save(context.Background(),
				&service.NewAPISiteAuthorization{SiteURL: site, UserID: 7, Ciphertext: "encrypted"},
				[]service.NewAPIBindingSave{{Account: account, TokenID: 3, Group: "vip"}})
			if site == "https://example.com" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, service.ErrNewAPIConfiguration)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
func TestNewAPIGetBindingNeverSerializesSecret(t *testing.T) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	defer func() { _ = db.Close() }()
	m.ExpectQuery("SELECT b.account_id").WithArgs(int64(1)).WillReturnRows(sqlmock.NewRows([]string{"account_id", "token_id", "fingerprint", "group", "id", "site_url", "user_id", "revision", "access_token_ciphertext"}))
	b, e := NewNewAPIAuthorizationRepository(db).GetBinding(context.Background(), 1)
	require.NoError(t, e)
	require.Nil(t, b)
	require.NoError(t, m.ExpectationsWereMet())
}
func TestNewAPISnapshotRejectsRotatedAuthorizationBeforeAccountWrite(t *testing.T) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	defer func() { _ = db.Close() }()
	r := NewNewAPIAuthorizationRepository(db)
	m.ExpectBegin()
	m.ExpectQuery("SELECT revision FROM new_api_site_authorizations").WithArgs(int64(8)).WillReturnRows(sqlmock.NewRows([]string{"revision"}).AddRow(2))
	m.ExpectRollback()
	e = r.WriteSnapshot(context.Background(), &service.Account{ID: 1}, &service.NewAPIAccountBinding{Profile: service.NewAPISiteAuthorization{ID: 8, Revision: 1}}, &service.UpstreamBillingProbeSnapshot{})
	require.ErrorIs(t, e, service.ErrUpstreamBillingProbeIdentityChanged)
	require.NoError(t, m.ExpectationsWereMet())
}
func TestNewAPIUnbindOnlyRequestedAccountAndOutboxAtomic(t *testing.T) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	defer func() { _ = db.Close() }()
	r := NewNewAPIAuthorizationRepository(db)
	m.ExpectBegin()
	m.ExpectExec("UPDATE accounts SET extra").WithArgs(int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectExec("DELETE FROM new_api_account_bindings WHERE account_id").WithArgs(int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectExec("INSERT INTO scheduler_outbox").WillReturnError(fmt.Errorf("outbox unavailable"))
	m.ExpectRollback()
	require.Error(t, r.Unbind(context.Background(), 1))
	require.NoError(t, m.ExpectationsWereMet())
}
