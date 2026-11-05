//go:build integration

package repository

import (
	"context"
	"crypto/tls"
	"database/sql"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/regional/control"
	"github.com/Wei-Shaw/sub2api/internal/regional/ledger"
	"github.com/stretchr/testify/require"
)

func TestRegionalControlMTLSScopeAndDurableOutbox(t *testing.T) {
	ctx := context.Background()
	user := regionalLedgerUser(t)
	quota := &ledger.Primary{DB: integrationDB}
	local := &ledger.Local{DB: integrationDB, Region: "us"}
	roots, serverCert, clientCert, otherClient := regionalControlTLS(t)
	scope := control.Scope{Region: "us", CertificateSHA256: regionalControlPin(clientCert), UserIDs: []int64{user}, APIKeyIDs: []int64{1}, AccountIDs: []int64{2}, MaximumGrant: 1_000_000_000, MaximumSlots: 2, MaximumLifetime: time.Hour}
	verified := 0
	handler, err := control.NewHandler(scope, quota, NewRegionalSlotAuthority(integrationDB), func(context.Context, *sql.Tx, ledger.Event) error { verified++; return nil })
	require.NoError(t, err)
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots, Certificates: []tls.Certificate{serverCert}}
	server.StartTLS()
	defer server.Close()
	client, err := control.NewClient(server.URL, roots, clientCert, regionalControlPin(serverCert))
	require.NoError(t, err)
	defer client.Close()
	lease := regionalLedgerLease(fmt.Sprintf("control-%d", user), user, 1_000_000_000)
	require.NoError(t, client.IssueQuota(ctx, lease))
	require.NoError(t, client.IssueQuota(ctx, lease))
	require.NoError(t, local.Install(ctx, lease))
	// A CA-trusted peer with an unassigned leaf pin is still unauthorized.
	wrong, err := control.NewClient(server.URL, roots, otherClient, regionalControlPin(serverCert))
	require.NoError(t, err)
	defer wrong.Close()
	require.Error(t, wrong.IssueQuota(ctx, lease))
	badServer, err := control.NewClient(server.URL, roots, clientCert, regionalControlPin(otherClient))
	require.NoError(t, err)
	defer badServer.Close()
	require.Error(t, badServer.IssueQuota(ctx, lease))
	foreign := lease
	foreign.LeaseID += "-foreign"
	foreign.RegionID = "eu"
	require.Error(t, client.IssueQuota(ctx, foreign))
	oversized := lease
	oversized.LeaseID += "-oversized"
	oversized.Granted += 10
	require.Error(t, client.IssueQuota(ctx, oversized))
	r := regionalLedgerReservation(lease, fmt.Sprintf("control-req-%d", user), 1_000_000_000)
	fresh, err := local.Reserve(ctx, r)
	require.NoError(t, err)
	require.True(t, fresh)
	require.NoError(t, local.Start(ctx, r.RequestID))
	event := regionalLedgerEvent(r, 500_000_000)
	require.NoError(t, local.Complete(ctx, event))
	count, err := local.Flush(ctx, client, 100)
	require.NoError(t, err)
	require.GreaterOrEqual(t, count, 1)
	require.Equal(t, 1, verified)
	require.NoError(t, client.Settle(ctx, event))
	require.Equal(t, 1, verified)
	require.NoError(t, local.Drain(ctx, lease.LeaseID))
	receipt, err := local.Close(ctx, lease.LeaseID)
	require.NoError(t, err)
	require.NoError(t, client.CloseQuota(ctx, receipt))
	var balance string
	require.NoError(t, integrationDB.QueryRow(`SELECT balance::text FROM users WHERE id=$1`, user).Scan(&balance))
	require.Equal(t, "9.50000000", balance)
}
