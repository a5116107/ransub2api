//go:build integration

package repository

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/regional/ledger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRegionalPricingCanonicalSettlementAndLostAck(t *testing.T) {
	ctx := context.Background()
	user := regionalLedgerUser(t)
	client := testEntClient(t)
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user, Key: "sk-mock-" + uuid.NewString(), Name: "regional-pricing"})
	account := mustCreateAccount(t, client, &service.Account{Name: "mock-" + uuid.NewString(), Type: service.AccountTypeAPIKey})
	p := &ledger.Primary{DB: integrationDB}
	l := regionalLedgerLease(t, fmt.Sprintf("price-%d", user), user, 5_000_000_000)
	require.NoError(t, p.Issue(ctx, l))
	local := &ledger.Local{DB: integrationDB, Region: l.RegionID}
	require.NoError(t, local.Install(ctx, l))
	price := ledger.Price{Model: "mock-text", Input: 1_000_000_000, Output: 2_000_000_000, CacheCreation: 3_000_000_000, CacheRead: 100_000_000, MaxInput: 1000, MaxOutput: 100}
	require.NoError(t, ledger.InstallPrice(ctx, integrationDB, price))
	require.NoError(t, ledger.InstallPrice(ctx, integrationDB, price))
	_, err := integrationDB.ExecContext(ctx, "UPDATE regional_prices SET price='{}' WHERE version=$1", price.Version())
	require.Error(t, err)
	r := regionalLedgerReservation(t, l, fmt.Sprintf("price-request-%d", user), 4_000_000_000)
	r.APIKeyID = key.ID
	r.AccountID = account.ID
	r.Model = price.Model
	r.PricingVersion = price.Version()
	fresh, err := local.Reserve(ctx, r)
	require.NoError(t, err)
	require.True(t, fresh)
	require.NoError(t, local.Start(ctx, r.RequestID))
	charge, err := price.Charge(10, 5, 0, 0)
	require.NoError(t, err)
	e := regionalLedgerEvent(r, charge)
	require.NoError(t, local.Complete(ctx, e))
	verified := 0
	scope := func(context.Context, *sql.Tx, ledger.Event) error { verified++; return nil }
	bad := e
	bad.Usage.Charge += 10
	_, err = p.Settle(ctx, l.RegionID, bad, ledger.PricingVerifier(scope))
	require.ErrorIs(t, err, ledger.ErrConflict)
	require.Zero(t, verified)
	_, err = p.Settle(ctx, l.RegionID, e, ledger.PricingVerifier(nil))
	require.Error(t, err)
	failedScope := ledger.PricingVerifier(func(context.Context, *sql.Tx, ledger.Event) error { return errors.New("admission not authorized") })
	_, err = p.Settle(ctx, l.RegionID, e, failedScope)
	require.Error(t, err)
	applied, err := p.Settle(ctx, l.RegionID, e, ledger.PricingVerifier(scope))
	require.NoError(t, err)
	require.True(t, applied)
	// A fresh process replays the exact outbox after authority commit/ack loss.
	local = &ledger.Local{DB: integrationDB, Region: l.RegionID}
	pending, err := local.Pending(ctx, 100)
	require.NoError(t, err)
	require.Contains(t, pending, e)
	applied, err = (&ledger.Primary{DB: integrationDB}).Settle(ctx, l.RegionID, e, ledger.PricingVerifier(scope))
	require.NoError(t, err)
	require.False(t, applied)
	require.Equal(t, 1, verified)
	var count int
	var amount string
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT count(*),min(actual_cost)::text FROM usage_logs WHERE request_id=$1 AND api_key_id=$2", r.RequestID, key.ID).Scan(&count, &amount))
	require.Equal(t, 1, count)
	require.Equal(t, "0.0000200000", amount)
	require.NoError(t, local.Acknowledge(ctx, e))
	require.NoError(t, local.Drain(ctx, l.LeaseID))
	receipt, err := local.Close(ctx, l.LeaseID)
	require.NoError(t, err)
	require.NoError(t, p.Close(ctx, l.RegionID, receipt))
	var balance string
	require.NoError(t, integrationDB.QueryRow("SELECT balance::text FROM users WHERE id=$1", user).Scan(&balance))
	require.Equal(t, "9.99998000", balance)
}

func TestRegionalGrantCompetesWithPrimaryInflightHold(t *testing.T) {
	ctx := context.Background()
	user := regionalLedgerUser(t)
	t.Cleanup(func() { _, _ = integrationDB.Exec("DELETE FROM regional_primary_requests WHERE user_id=$1", user) })
	primary := &ledger.Primary{DB: integrationDB}
	holds := &ledger.PrimaryRequests{DB: integrationDB, Authorize: func(context.Context, *sql.Tx, ledger.Reservation) error { return nil }, SettleScope: func(context.Context, *sql.Tx, ledger.Event) error { return nil }, CancelScope: func(context.Context, *sql.Tx, ledger.Reservation) error { return nil }}
	r := ledger.Reservation{RequestID: fmt.Sprintf("hold-%d", user), UserID: user, Maximum: 6_000_000_000, PayloadSHA256: strings.Repeat("a", 64), Model: "mock-text", PricingVersion: "mock-v1", AdmittedAt: time.Now()}
	l := regionalLedgerLease(t, fmt.Sprintf("grant-race-%d", user), user, 6_000_000_000)
	var wg sync.WaitGroup
	wg.Add(2)
	errs := make(chan error, 2)
	go func() { defer wg.Done(); _, err := holds.Reserve(ctx, r); errs <- err }()
	go func() { defer wg.Done(); errs <- primary.Issue(ctx, l) }()
	wg.Wait()
	close(errs)
	success, exhausted := 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else {
			require.ErrorIs(t, err, ledger.ErrExhausted)
			exhausted++
		}
	}
	require.Equal(t, 1, success)
	require.Equal(t, 1, exhausted)
	var balance string
	require.NoError(t, integrationDB.QueryRow("SELECT balance::text FROM users WHERE id=$1", user).Scan(&balance))
	require.Equal(t, "4.00000000", balance)
}

func TestRegionalZeroPriceReservationsAndSettlement(t *testing.T) {
	ctx := context.Background()
	user := regionalLedgerUser(t)
	client := testEntClient(t)
	group := mustCreateGroup(t, client, &service.Group{Name: "free-" + uuid.NewString(), Platform: service.PlatformOpenAI, RateMultiplier: 1})
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user, GroupID: &group.ID, Key: "sk-mock-" + uuid.NewString(), Name: "free"})
	account := mustCreateAccount(t, client, &service.Account{Name: "free-" + uuid.NewString(), Type: service.AccountTypeAPIKey, Platform: service.PlatformOpenAI, Status: "active", Schedulable: true})
	_, err := integrationDB.ExecContext(ctx, "INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)", account.ID, group.ID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.Exec("DELETE FROM usage_logs WHERE user_id=$1", user)
		_, _ = integrationDB.Exec("DELETE FROM regional_primary_requests WHERE user_id=$1", user)
		_, _ = integrationDB.Exec("DELETE FROM regional_budget_allocations WHERE user_id=$1", user)
	})
	free := ledger.Price{Model: "free-regional", MaxInput: 100, MaxOutput: 100}
	require.NoError(t, ledger.InstallPrice(ctx, integrationDB, free))
	primary, holds, verify := NewRegionalFundsAuthorities(integrationDB, func(context.Context, *sql.Tx) error { return nil })
	lease := regionalLedgerLease(t, "free-lease-"+uuid.NewString(), user, 1_000_000_000)
	lease.APIKeyID, lease.AccountID = key.ID, account.ID
	require.NoError(t, primary.Issue(ctx, lease))
	local := &ledger.Local{DB: integrationDB, Region: lease.RegionID}
	require.NoError(t, local.Install(ctx, lease))
	reservation := regionalLedgerReservation(t, lease, "free-local-"+uuid.NewString(), 0)
	reservation.APIKeyID, reservation.AccountID = key.ID, account.ID
	reservation.Model, reservation.PricingVersion = free.Model, free.Version()
	for _, field := range []string{"input", "output", "creation", "read", "missing", "mismatch"} {
		bad := reservation
		bad.RequestID = "free-bad-" + uuid.NewString()
		price := free
		switch field {
		case "input":
			price.Input = 1
		case "output":
			price.Output = 1
		case "creation":
			price.CacheCreation = 1
		case "read":
			price.CacheRead = 1
		case "missing":
			bad.PricingVersion = strings.Repeat("f", 64)
		case "mismatch":
			bad.Model = "different-model"
		}
		if field != "missing" && field != "mismatch" {
			require.NoError(t, ledger.InstallPrice(ctx, integrationDB, price))
			bad.PricingVersion = price.Version()
		}
		_, err := local.Reserve(ctx, bad)
		require.Error(t, err, "local must reject %s zero reservation", field)
		bad.LeaseID = ""
		_, err = holds.Reserve(ctx, bad)
		require.Error(t, err, "primary must reject %s zero reservation", field)
	}
	fresh, err := local.Reserve(ctx, reservation)
	require.NoError(t, err)
	require.True(t, fresh)
	fresh, err = local.Reserve(ctx, reservation)
	require.NoError(t, err)
	require.False(t, fresh)
	require.NoError(t, local.Start(ctx, reservation.RequestID))
	require.ErrorIs(t, local.CancelBeforeStart(ctx, reservation.RequestID), ledger.ErrAlreadyDispatched)
	// A complete HTTP forwarding attempt uses the same SQL ledger as the
	// regional runtime; admission here is a deterministic fixture, not the
	// production cutover/authentication composition root.
	var sends atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_fixture","model":"free-regional","output":[],"usage":{"input_tokens":10,"output_tokens":5}}`)
	}))
	t.Cleanup(provider.Close)
	account.Credentials = map[string]any{"api_key": "supplier-fixture", "base_url": provider.URL}
	account.Concurrency = 1
	_, err = integrationDB.ExecContext(ctx, "UPDATE accounts SET credentials=jsonb_build_object('api_key','supplier-fixture','base_url',$2::text) WHERE id=$1", account.ID, provider.URL)
	require.NoError(t, err)
	requestID := "free-forward-" + uuid.NewString()
	meter := &service.RegionalMetering{Ledger: local, Admit: func(_ context.Context, authenticated *service.APIKey, selected *service.Account, _ []byte) (ledger.Reservation, ledger.Price, error) {
		if authenticated.ID != key.ID || selected.ID != account.ID {
			return ledger.Reservation{}, ledger.Price{}, ledger.ErrConflict
		}
		r := regionalLedgerReservation(t, lease, requestID, 0)
		r.APIKeyID, r.AccountID = key.ID, account.ID
		return r, free, nil
	}}
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowPrivateHosts: true, AllowInsecureHTTP: true}}}
	gateway := service.ProvideOpenAIGatewayService(service.OpenAIGatewayDependencies{Config: cfg, Accounts: NewAccountRepository(client, integrationDB, nil), RegionalMetering: meter, Upstream: newHTTPUpstream(cfg, nil, nil, nil)})
	t.Cleanup(gateway.StopOpenAICodexTicketHarvester)
	t.Cleanup(gateway.StopAsyncVideoKeyReleaseRecovery)
	body := []byte(`{"model":"free-regional","input":"hello","max_output_tokens":10}`)
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("api_key", key)
	result, err := gateway.Forward(ctx, c, account, body)
	require.NoError(t, err, "provider sends=%d, status=%d, response=%q", sends.Load(), recorder.Code, recorder.Body.String())
	require.True(t, result.RegionalMetered)
	require.EqualValues(t, 1, sends.Load())
	forwarded, err := local.Pending(ctx, 100)
	require.NoError(t, err)
	require.Len(t, forwarded, 1)
	require.Equal(t, requestID, forwarded[0].Usage.EventID)
	require.Zero(t, forwarded[0].Usage.Charge)
	_, err = primary.Settle(ctx, lease.RegionID, forwarded[0], verify)
	require.NoError(t, err)
	require.NoError(t, local.Acknowledge(ctx, forwarded[0]))
	require.NoError(t, local.Drain(ctx, lease.LeaseID))
	_, err = local.Close(ctx, lease.LeaseID)
	require.Error(t, err, "zero-valued started requests must still block drain")
	event := regionalLedgerEvent(reservation, 0)
	badEvent := event
	badEvent.Usage.Charge = 10
	require.ErrorIs(t, local.Complete(ctx, badEvent), ledger.ErrConflict)
	local = &ledger.Local{DB: integrationDB, Region: lease.RegionID}
	require.NoError(t, local.Complete(ctx, event))
	require.NoError(t, local.Complete(ctx, event))
	badEvent = event
	badEvent.OutputTokens++
	require.ErrorIs(t, local.Complete(ctx, badEvent), ledger.ErrConflict)
	_, err = local.Close(ctx, lease.LeaseID)
	require.Error(t, err, "zero-valued outbox must still block drain until acknowledged")
	applied, err := primary.Settle(ctx, lease.RegionID, event, verify)
	require.NoError(t, err)
	require.True(t, applied)
	applied, err = primary.Settle(ctx, lease.RegionID, event, verify)
	require.NoError(t, err)
	require.False(t, applied)
	require.NoError(t, local.Acknowledge(ctx, event))
	receipt, err := local.Close(ctx, lease.LeaseID)
	require.NoError(t, err)
	require.NoError(t, primary.Close(ctx, lease.RegionID, receipt))
	request := regionalLedgerReservation(t, lease, "free-primary-"+uuid.NewString(), 0)
	request.LeaseID = ""
	request.APIKeyID, request.AccountID = key.ID, account.ID
	request.Model, request.PricingVersion = free.Model, free.Version()
	fresh, err = holds.Reserve(ctx, request)
	require.NoError(t, err)
	require.True(t, fresh)
	require.NoError(t, holds.Start(ctx, request.RequestID))
	primaryEvent := regionalLedgerEvent(request, 0)
	results := make(chan error, 4)
	for i := 0; i < cap(results); i++ {
		go func() { results <- holds.Complete(ctx, primaryEvent) }()
	}
	for i := 0; i < cap(results); i++ {
		require.NoError(t, <-results)
	}
	badPrimaryEvent := primaryEvent
	badPrimaryEvent.OutputTokens++
	require.ErrorIs(t, holds.Complete(ctx, badPrimaryEvent), ledger.ErrConflict)
	var count int
	var total, balance string
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT count(*),COALESCE(sum(actual_cost),0)::text FROM usage_logs WHERE request_id IN ($1,$2,$3)", reservation.RequestID, request.RequestID, requestID).Scan(&count, &total))
	require.Equal(t, 3, count)
	require.Equal(t, "0.0000000000", total)
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT balance::text FROM users WHERE id=$1", user).Scan(&balance))
	require.Equal(t, "10.00000000", balance)
}

func TestRegionalZeroPriceMigrationUpgradeAndRollback(t *testing.T) {
	ctx := context.Background()
	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	tables := []string{"regional_local_requests", "regional_primary_requests", "regional_budget_allocations"}
	for _, table := range tables {
		_, err = tx.ExecContext(ctx, "CREATE TEMP TABLE "+table+" (maximum BIGINT NOT NULL CHECK (maximum > 0 AND maximum % 10 = 0)) ON COMMIT DROP")
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, "INSERT INTO "+table+" VALUES (10)")
		require.NoError(t, err)
	}
	migration, err := migrations.FS.ReadFile("333_regional_zero_price_reservations.sql")
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		_, err = tx.ExecContext(ctx, string(migration))
		require.NoError(t, err, "migration is repeatable over existing positive holds")
	}
	for _, table := range tables {
		_, err = tx.ExecContext(ctx, "INSERT INTO "+table+" VALUES (0)")
		require.NoError(t, err)
		for _, invalid := range []int64{-10, 1} {
			_, err = tx.ExecContext(ctx, "SAVEPOINT invalid_amount")
			require.NoError(t, err)
			_, err = tx.ExecContext(ctx, "INSERT INTO "+table+" VALUES ($1)", invalid)
			require.Error(t, err, "negative and sub-quantum amounts remain invalid")
			_, err = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT invalid_amount")
			require.NoError(t, err)
		}
		_, err = tx.ExecContext(ctx, "SAVEPOINT rollback_probe")
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, "ALTER TABLE "+table+" DROP CONSTRAINT "+table+"_maximum_check")
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, "ALTER TABLE "+table+" ADD CONSTRAINT "+table+"_maximum_check CHECK (maximum > 0 AND maximum % 10 = 0)")
		require.Error(t, err, "rollback must reject unresolved zero rows rather than delete them")
		_, err = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT rollback_probe")
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE maximum=0")
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, "ALTER TABLE "+table+" DROP CONSTRAINT "+table+"_maximum_check")
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, "ALTER TABLE "+table+" ADD CONSTRAINT "+table+"_maximum_check CHECK (maximum > 0 AND maximum % 10 = 0)")
		require.NoError(t, err)
		var count int
		require.NoError(t, tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE maximum=10").Scan(&count))
		require.Equal(t, 1, count)
	}
}
