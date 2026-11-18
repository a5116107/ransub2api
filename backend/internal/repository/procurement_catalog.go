package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

const procurementCatalogLockID int64 = 0x4250535052434154

func loadProcurementCatalog(ctx context.Context, db sqlQueryer) (*domain.ProcurementCatalog, error) {
	var raw string
	err := scanSingleRow(ctx, db, `SELECT value FROM settings WHERE key=$1`, []any{service.ProcurementCatalogSettingKey}, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return &domain.ProcurementCatalog{Version: 1, Buckets: []domain.ProcurementBucket{}}, nil
	}
	if err != nil {
		return nil, err
	}
	catalog, err := service.ParseProcurementCatalogCommand([]byte(raw))
	if err != nil || catalog.Validate() != nil {
		return nil, service.ErrProcurementCatalogUnavailable
	}
	return catalog.WithRevision(catalog.Revision), nil
}

func (r *channelRepository) ReadProcurementCatalog(ctx context.Context) (*domain.ProcurementCatalog, error) {
	return loadProcurementCatalog(ctx, r.db)
}

func (r *channelRepository) SaveProcurementCatalog(ctx context.Context, draft *domain.ProcurementCatalog) (*domain.ProcurementCatalog, error) {
	if draft.ValidateDraft() != nil || draft.Revision == domain.MaxProcurementPolicyRevision {
		return nil, service.ErrProcurementCatalogInvalid
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	// Serialize first creation as well as updates. Never hold this lock during
	// network calls, probes, or classification reads.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, procurementCatalogLockID); err != nil {
		return nil, err
	}
	current, err := loadProcurementCatalog(ctx, tx)
	if err != nil {
		return nil, err
	}
	if current.Revision != draft.Revision {
		return nil, service.ErrProcurementCatalogConflict
	}
	next := draft.WithRevision(draft.Revision + 1)
	raw, err := json.Marshal(next)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES($1,$2,NOW())
 ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,updated_at=EXCLUDED.updated_at`, service.ProcurementCatalogSettingKey, string(raw)); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return next, nil
}

func (r *channelRepository) PreviewProcurementClassification(ctx context.Context, requests []service.ProfitPreviewRequestScope) (*service.ProcurementClassificationPreview, error) {
	if len(requests) < 1 || len(requests) > 100 {
		return nil, service.ErrProcurementCatalogInvalid
	}
	ids := make([]int64, 0, len(requests))
	for _, request := range requests {
		if request.AccountID < 1 {
			return nil, service.ErrProcurementCatalogInvalid
		}
		ids = append(ids, request.AccountID)
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	catalog, err := loadProcurementCatalog(ctx, tx)
	if err != nil {
		return nil, err
	}
	// Reuse the original Account projection and group/proxy mapper in this
	// exact SQL snapshot; no second credential-bearing inventory is persisted.
	client := dbent.NewClient(dbent.Driver(entsql.NewDriver(dialect.Postgres, entsql.Conn{ExecQuerier: tx})))
	entities, err := observerAccountQuery(ctx, client.Account.Query()).Where(dbaccount.IDIn(ids...), dbaccount.DeletedAtIsNil()).All(ctx)
	if err != nil {
		return nil, err
	}
	accounts, err := newAccountRepositoryWithSQL(client, tx, nil).accountsToService(ctx, entities)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]*service.Account, len(accounts))
	for i := range accounts {
		byID[accounts[i].ID] = &accounts[i]
	}
	out := &service.ProcurementClassificationPreview{ProcurementCatalogSnapshot: service.ProcurementCatalogSnapshot{Catalog: catalog, Gaps: catalog.Gaps()}, Results: make([]service.ProcurementScopeClassification, 0, len(requests))}
	// Expiry is checked at the end of loading, not at transaction start.
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&out.ObservedAt); err != nil {
		return nil, err
	}
	for _, request := range requests {
		out.Results = append(out.Results, service.ClassifyProcurementScope(catalog, byID[request.AccountID], request, out.ObservedAt))
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}
