package repository

import (
	"context"
	"database/sql"
	"errors"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	dbaccountgroup "github.com/Wei-Shaw/sub2api/ent/accountgroup"
	dbgroup "github.com/Wei-Shaw/sub2api/ent/group"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *channelRepository) PreviewProcurementProductImpact(ctx context.Context, groupID int64) (*service.ProcurementProductImpact, error) {
	if groupID < 1 || groupID > domain.MaxProcurementPolicyRevision {
		return nil, service.ErrProcurementCatalogInvalid
	}
	if _, scoped := service.ObserverGroupIDs(ctx); scoped || adminaccess.FromContext(ctx) != nil {
		return nil, service.ErrObserverScope
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	client := dbent.NewClient(dbent.Driver(entsql.NewDriver(dialect.Postgres, entsql.Conn{ExecQuerier: tx})))
	entity, err := client.Group.Query().Where(dbgroup.IDEQ(groupID), dbgroup.DeletedAtIsNil()).Only(ctx)
	if dbent.IsNotFound(err) {
		return nil, service.ErrGroupNotFound
	}
	if err != nil {
		return nil, err
	}
	group := groupEntityToService(entity)
	group.ProcurementCatalog, err = loadProcurementCatalog(ctx, tx)
	if err != nil {
		return nil, err
	}
	// Preserve an unavailable source authority as unknown in the original price
	// verdict, but never suppress database/cancellation errors as empty sources.
	group.ProcurementSources, err = loadProcurementSources(ctx, tx, group)
	if err != nil && !errors.Is(err, service.ErrProcurementPolicyInvalid) {
		return nil, err
	}
	entities, err := client.Account.Query().Where(dbaccount.DeletedAtIsNil(),
		dbaccount.HasAccountGroupsWith(dbaccountgroup.GroupIDEQ(groupID))).Order(dbaccount.ByID()).
		Limit(service.ProcurementProductImpactAccountLimit + 1).All(ctx)
	if err != nil {
		return nil, err
	}
	if len(entities) > service.ProcurementProductImpactAccountLimit {
		return nil, service.ErrProcurementProductImpactTooLarge
	}
	accounts, err := newAccountRepositoryWithSQL(client, tx, nil).accountsToService(ctx, entities)
	if err != nil {
		return nil, err
	}
	out := &service.ProcurementProductImpact{Version: 1, GroupStatus: group.Status, GateEnabled: group.ProfitControlEnabled, BoundAccounts: len(accounts)}
	var minOverride sql.NullFloat64
	if err = tx.QueryRowContext(ctx, `SELECT min(rate_multiplier)::float8,count(*) FROM user_group_rate_multipliers
	 WHERE group_id=$1 AND rate_multiplier>=0 AND rate_multiplier NOT IN ('NaN'::numeric,'Infinity'::numeric)`, groupID).Scan(&minOverride, &out.UserOverrideCount); err != nil {
		return nil, err
	}
	// Time-dependent peaks and evidence expire against the end of this single
	// snapshot read, not an earlier time captured before waiting on SQL.
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&out.ObservedAt); err != nil {
		return nil, err
	}
	in := service.ProfitPreviewGroupInput{Group: group, AssumeEnabled: true}
	if minOverride.Valid {
		in.UserOverrides = map[int64]float64{0: minOverride.Float64} // no user identities leave this read
	}
	for i := range accounts {
		account := &accounts[i]
		// Only saved administrative eligibility is considered. Temporary health,
		// model/scope support, cooldown, quota and live slots are not simulated.
		if account.Status == service.StatusActive && account.Schedulable {
			in.Accounts = append(in.Accounts, account)
		}
	}
	out.ConsideredAccounts = len(in.Accounts)
	out.Report = service.PreviewProfitAdmission([]service.ProfitPreviewGroupInput{in}, out.ObservedAt)[0]
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}
