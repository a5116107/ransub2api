package repository

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"fmt"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

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
