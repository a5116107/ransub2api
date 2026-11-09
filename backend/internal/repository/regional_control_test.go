package repository

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/regional/control"
	"github.com/Wei-Shaw/sub2api/internal/regional/slots"
	"github.com/Wei-Shaw/sub2api/internal/regional/video"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRegionalControlSlotLifetimeUsesAuthorityClock(t *testing.T) {
	_, _, client, _ := regionalControlTLS(t)
	peer, err := x509.ParseCertificate(client.Certificate[0])
	require.NoError(t, err)
	for _, operation := range []string{"issue", "renew"} {
		for _, tc := range []struct {
			name       string
			offset     time.Duration
			issuedAgo  time.Duration
			extra      time.Duration
			clockError bool
			want       int
		}{
			{name: "database ahead", offset: time.Hour, want: http.StatusServiceUnavailable},
			{name: "database behind", offset: -time.Hour, want: http.StatusServiceUnavailable},
			{name: "long running lease", issuedAgo: time.Hour, want: http.StatusServiceUnavailable},
			{name: "over maximum", extra: time.Microsecond, want: http.StatusForbidden},
			{name: "clock unavailable", clockError: true, want: http.StatusServiceUnavailable},
		} {
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				db, mock, err := sqlmock.New()
				require.NoError(t, err)
				t.Cleanup(func() {
					mock.ExpectClose()
					require.NoError(t, db.Close())
					require.NoError(t, mock.ExpectationsWereMet())
				})
				authorityNow := time.Now().UTC().Truncate(time.Microsecond).Add(tc.offset)
				clock := mock.ExpectQuery(`SELECT clock_timestamp\(\)`)
				if tc.clockError {
					clock.WillReturnError(errors.New("fixture clock unavailable"))
				} else {
					clock.WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(authorityNow))
				}
				if tc.want == http.StatusServiceUnavailable && !tc.clockError {
					// Reaching the authority proves admission used its clock. The
					// real SQL lifecycle tests cover issuance and renewal themselves.
					mock.ExpectBegin().WillReturnError(errors.New("fixture authority reached"))
				}
				scope := control.Scope{Region: "us", CertificateSHA256: regionalControlPin(client), AccountIDs: []int64{2}, MaximumGrant: 1_000_000_000, MaximumSlots: 1, MaximumLifetime: time.Minute}
				authority := NewRegionalSlotAuthority(db)
				authority.GrantGuard = func(context.Context, *sql.Tx) error { return nil }
				handler, err := control.NewSlotsHandler(scope, authority)
				require.NoError(t, err)
				grant := slots.Grant{LeaseID: uuid.NewString(), Region: "us", Resource: slots.Resource{Kind: "account", ID: 2}, Slots: 1, Revision: 1,
					IssuedAt: authorityNow.Add(-tc.issuedAgo), ExpiresAt: authorityNow.Add(time.Minute + tc.extra)}
				if operation == "renew" {
					grant.Revision++
				}
				raw, err := json.Marshal(grant)
				require.NoError(t, err)
				request := httptest.NewRequest(http.MethodPost, control.Prefix+"/slots/"+operation, bytes.NewReader(raw))
				request.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, PeerCertificates: []*x509.Certificate{peer}, VerifiedChains: [][]*x509.Certificate{{peer}}}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				require.Equal(t, tc.want, response.Code)
			})
		}
	}
}

func regionalControlTLS(t *testing.T) (*x509.CertPool, tls.Certificate, tls.Certificate, tls.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-regional-control-ca"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, root, root, &key.PublicKey, key)
	require.NoError(t, err)
	ca, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	makeCert := func(serial int64, usage x509.ExtKeyUsage) tls.Certificate {
		leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: fmt.Sprintf("peer-%d", serial)}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
		raw, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, key)
		require.NoError(t, err)
		return tls.Certificate{Certificate: [][]byte{raw, der}, PrivateKey: leafKey}
	}
	return roots, makeCert(2, x509.ExtKeyUsageServerAuth), makeCert(3, x509.ExtKeyUsageClientAuth), makeCert(4, x509.ExtKeyUsageClientAuth)
}
func regionalControlPin(cert tls.Certificate) string {
	sum := sha256.Sum256(cert.Certificate[0])
	return hex.EncodeToString(sum[:])
}

func TestRegionalControlVideoDirectoryValidatesConfirmation(t *testing.T) {
	roots, serverCert, clientCert, _ := regionalControlTLS(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	report := video.Snapshot{SchemaVersion: video.SchemaVersion, Authority: video.LocalAuthority("us"), Revision: 10, IssuedAt: now, ExpiresAt: now.Add(video.Lifetime), Entries: []video.Entry{}}
	primary := report
	primary.Authority = video.PrimaryAuthority
	for _, tc := range []struct {
		name  string
		edit  func(*video.Directory)
		valid bool
	}{
		{name: "confirmed", valid: true},
		{name: "missing stream", edit: func(d *video.Directory) { d.Snapshots = d.Snapshots[:1] }},
		{name: "duplicate stream", edit: func(d *video.Directory) { d.Snapshots[0] = d.Snapshots[1] }},
		{name: "foreign stream", edit: func(d *video.Directory) { d.Snapshots[0].Authority = video.LocalAuthority("eu") }},
		{name: "changed acknowledgement", edit: func(d *video.Directory) { d.Snapshots[1].Revision++ }},
		{name: "unsupported schema", edit: func(d *video.Directory) { d.Snapshots[0].SchemaVersion++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := video.Directory{Snapshots: []video.Snapshot{primary, report}}
			if tc.edit != nil {
				tc.edit(&directory)
			}
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != control.Prefix+"/video-directory/sync" {
					t.Errorf("unexpected directory path: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(directory); err != nil {
					t.Errorf("encode directory: %v", err)
				}
			}))
			server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots, Certificates: []tls.Certificate{serverCert}}
			server.StartTLS()
			t.Cleanup(server.Close)
			client, err := control.NewClient(server.URL, roots, clientCert, regionalControlPin(serverCert))
			require.NoError(t, err)
			t.Cleanup(client.Close)
			_, err = client.SyncVideoDirectory(t.Context(), report)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestRegionalAsyncVideoDirectoryExportLifetimeUsesCapturedView(t *testing.T) {
	for _, tc := range []struct {
		name    string
		elapsed time.Duration
		fail    bool
	}{
		{name: "within lifetime", elapsed: video.Lifetime - time.Microsecond},
		{name: "deadline reached", elapsed: video.Lifetime, fail: true},
		{name: "delayed past deadline", elapsed: video.Lifetime + time.Hour, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() {
				mock.ExpectClose()
				require.NoError(t, db.Close())
				require.NoError(t, mock.ExpectationsWereMet())
			})
			now := time.Now().UTC().Truncate(time.Microsecond)
			mock.ExpectBegin()
			mock.ExpectQuery(`SELECT clock_timestamp\(\)`).WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(now))
			mock.ExpectQuery(`INSERT INTO regional_async_video_directory_revisions`).WithArgs(video.PrimaryAuthority).WillReturnRows(sqlmock.NewRows([]string{"revision"}).AddRow(1))
			mock.ExpectQuery(`SELECT member,COALESCE`).WithArgs(video.PrimaryAuthority, video.MaxEntries+1).WillReturnRows(sqlmock.NewRows([]string{"member", "task_id", "task_key", "state", "owner", "lease", "fenced"}))
			mock.ExpectQuery(`SELECT clock_timestamp\(\)`).WillReturnRows(sqlmock.NewRows([]string{"now"}).AddRow(now.Add(tc.elapsed)))
			if tc.fail {
				mock.ExpectRollback()
			} else {
				mock.ExpectCommit()
			}
			cache := &regionalConcurrencyCache{primary: NewRegionalSlotAuthority(db)}
			snapshot, err := cache.exportAsyncVideoDirectory(t.Context())
			if tc.fail {
				require.ErrorIs(t, err, video.ErrUnavailable)
			} else {
				require.NoError(t, err)
			}
			require.True(t, snapshot.IssuedAt.Equal(now), "a later SQL clock must not re-date the captured view")
			require.True(t, snapshot.ExpiresAt.Equal(now.Add(video.Lifetime)))
		})
	}
}
