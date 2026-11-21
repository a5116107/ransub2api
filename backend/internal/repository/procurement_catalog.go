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
	dbgroup "github.com/Wei-Shaw/sub2api/ent/group"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
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

func (r *channelRepository) PreviewProcurementClassification(ctx context.Context, command *service.ProcurementPreviewCommand) (*service.ProcurementClassificationPreview, error) {
	if command == nil || len(command.Requests) < 1 || len(command.Requests) > 100 || command.ProductGroupID != nil && (*command.ProductGroupID < 1 || *command.ProductGroupID > domain.MaxProcurementPolicyRevision) {
		return nil, service.ErrProcurementCatalogInvalid
	}
	requests := command.Requests
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
	var product *service.Group
	if command.ProductGroupID != nil {
		entity, err := client.Group.Query().Where(dbgroup.IDEQ(*command.ProductGroupID), dbgroup.DeletedAtIsNil()).Only(ctx)
		if err != nil {
			return nil, err
		}
		product = groupEntityToService(entity)
		product.ProcurementSources, _ = loadProcurementSources(ctx, tx, product)
	}
	entities, err := observerAccountQuery(ctx, client.Account.Query()).Where(dbaccount.IDIn(ids...), dbaccount.DeletedAtIsNil()).All(ctx)
	if err != nil {
		return nil, err
	}
	accounts, err := newAccountRepositoryWithSQL(client, tx, nil).accountsToService(ctx, entities)
	if err != nil {
		return nil, err
	}
	// Include both native performance and AI-ops pauses in this same snapshot,
	// as the original final-turn admission reader does.
	paused, err := performancePaused(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]*service.Account, len(accounts))
	for i := range accounts {
		accounts[i].PerformancePaused = paused[accounts[i].ID]
		byID[accounts[i].ID] = &accounts[i]
	}
	quality, err := readQualityScopesInTx(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	out := &service.ProcurementClassificationPreview{ProcurementCatalogSnapshot: service.ProcurementCatalogSnapshot{Catalog: catalog, Gaps: catalog.Gaps()}, Results: make([]service.ProcurementScopeClassification, 0, len(requests))}
	availability := make(map[int64][]*service.AvailabilityReport, len(accounts))
	for i := range accounts {
		account := &accounts[i]
		prepared := account
		if account.HasManagedOpenAIConnections() && account.AuthConnectionID == "" {
			prepared = nil // Inventory cannot certify a connection not selected here.
		}
		availability[account.ID], err = readAvailabilityReportsInTx(ctx, tx, "account_id", account.ID, prepared)
		if err != nil {
			return nil, err
		}
	}
	// Expiry is checked at the end of loading, not at transaction start.
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&out.ObservedAt); err != nil {
		return nil, err
	}
	qualityRules := quality.assess(out.ObservedAt)
	for _, request := range requests {
		result := service.ClassifyProcurementScope(catalog, byID[request.AccountID], request, out.ObservedAt, product)
		result.Quality = service.AssessQualityScopeEvidence(byID[request.AccountID], request.Scope, qualityRules[request.AccountID], out.ObservedAt, quality.limited)
		result.Availability = service.AssessAvailabilityScopeEvidence(byID[request.AccountID], &request.Scope, availability[request.AccountID], out.ObservedAt)
		if result.Product != nil {
			result.Product.AssessEvidence(product.ProcurementPolicy, result.Quality, result.Availability, out.ObservedAt)
		}
		out.Results = append(out.Results, result)
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// Caller holds the same read-only snapshot as the product policy. A missing,
// disabled, foreign or self source invalidates the whole selected authority.
func loadProcurementSources(ctx context.Context, db sqlQueryer, product *service.Group) (*domain.ProcurementSourceSnapshot, error) {
	policy := product.ProcurementPolicy
	if policy == nil || policy.Version < 6 || policy.SourceGroupIDs == nil {
		return nil, nil
	}
	if policy.Validate() != nil {
		return nil, service.ErrProcurementPolicyInvalid
	}
	groups := policy.SourceGroupIDs
	out := &domain.ProcurementSourceSnapshot{Version: 1, ProductGroupID: product.ID, PolicyRevision: policy.Revision, GroupIDs: append([]int64{}, groups...), AccountIDs: []int64{}}
	var count int
	if err := scanSingleRow(ctx, db, `SELECT count(*) FROM groups WHERE id=ANY($1) AND id<>$2 AND deleted_at IS NULL AND status='active' AND platform=$3`, []any{pq.Array(groups), product.ID, product.Platform}, &count); err != nil {
		return nil, err
	}
	if count != len(groups) {
		return nil, service.ErrProcurementPolicyInvalid
	}
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT a.id FROM account_groups ag JOIN accounts a ON a.id=ag.account_id WHERE ag.group_id=ANY($1) AND a.deleted_at IS NULL AND a.platform=$2 ORDER BY a.id LIMIT 10001`, pq.Array(groups), product.Platform)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out.AccountIDs = append(out.AccountIDs, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := out.ValidateFor(product.ID, policy); err != nil {
		return nil, err
	}
	return out, nil
}
