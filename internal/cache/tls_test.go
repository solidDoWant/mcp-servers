package cache

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testCA issues certificates for the TLS tests.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue returns a PEM certificate and key for cn, for a server (with an IP
// address) or a client.
func (ca *testCA) issue(t *testing.T, cn string, server bool) (certPEM, keyPEM []byte) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	require.NoError(t, err)

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

func writeFile(t *testing.T, path string, b []byte) string {
	t.Helper()
	require.NoError(t, os.WriteFile(path, b, 0o600))

	return path
}

// tlsServer starts a miniredis that requires a client certificate from ca,
// and records the common name of each client certificate it sees.
func tlsServer(t *testing.T, ca *testCA) (*miniredis.Miniredis, func() []string) {
	t.Helper()

	certPEM, keyPEM := ca.issue(t, "server", true)

	serverCert, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)

	clients := x509.NewCertPool()
	clients.AddCert(ca.cert)

	var (
		mu  sync.Mutex
		cns []string
	)

	m, err := miniredis.RunTLS(&tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clients,
		MinVersion:   tls.VersionTLS12,
		VerifyConnection: func(cs tls.ConnectionState) error {
			mu.Lock()
			defer mu.Unlock()

			cns = append(cns, cs.PeerCertificates[0].Subject.CommonName)

			return nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(m.Close)

	return m, func() []string {
		mu.Lock()
		defer mu.Unlock()

		return append([]string(nil), cns...)
	}
}

func TestMutualTLS(t *testing.T) {
	t.Run("connects with a client certificate the server trusts", func(t *testing.T) {
		ca := newTestCA(t)
		m, seen := tlsServer(t, ca)
		dir := t.TempDir()
		cert, key := ca.issue(t, "svc-a", false)

		store, err := NewRedisStore("rediss://"+m.Addr(), TLSFiles{
			CertFile: writeFile(t, filepath.Join(dir, "tls.crt"), cert),
			KeyFile:  writeFile(t, filepath.Join(dir, "tls.key"), key),
			CAFile:   writeFile(t, filepath.Join(dir, "ca.crt"), ca.pem),
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = store.Close() })

		require.NoError(t, store.Set(t.Context(), "svc-a:k", []byte("v"), time.Minute))

		v, ok, err := store.Get(t.Context(), "svc-a:k")
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, "v", string(v))
		assert.Contains(t, seen(), "svc-a")
	})

	t.Run("fails to connect without a client certificate", func(t *testing.T) {
		ca := newTestCA(t)
		m, _ := tlsServer(t, ca)

		store, err := NewRedisStore("rediss://"+m.Addr(), TLSFiles{CAFile: writeFile(t, filepath.Join(t.TempDir(), "ca.crt"), ca.pem)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = store.Close() })

		require.Error(t, store.Set(t.Context(), "k", []byte("v"), time.Minute))
	})

	t.Run("reads a renewed client certificate for each new connection", func(t *testing.T) {
		ca := newTestCA(t)
		dir := t.TempDir()
		certPath, keyPath := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")

		cert, key := ca.issue(t, "first", false)
		writeFile(t, certPath, cert)
		writeFile(t, keyPath, key)

		cfg := &tls.Config{}
		require.NoError(t, TLSFiles{CertFile: certPath, KeyFile: keyPath}.apply(cfg))

		got, err := cfg.GetClientCertificate(nil)
		require.NoError(t, err)

		leaf, err := x509.ParseCertificate(got.Certificate[0])
		require.NoError(t, err)
		assert.Equal(t, "first", leaf.Subject.CommonName)

		// cert-manager renews it: the next connection presents the new one.
		cert, key = ca.issue(t, "renewed", false)
		writeFile(t, certPath, cert)
		writeFile(t, keyPath, key)

		got, err = cfg.GetClientCertificate(nil)
		require.NoError(t, err)

		leaf, err = x509.ParseCertificate(got.Certificate[0])
		require.NoError(t, err)
		assert.Equal(t, "renewed", leaf.Subject.CommonName)
	})

	invalid := []struct {
		name  string
		url   string
		files func(dir string) TLSFiles
		want  string
	}{
		{"a certificate without its key", "rediss://h:6379", func(string) TLSFiles { return TLSFiles{CertFile: "a"} }, "both a client certificate and its key"},
		{"TLS files with a plain redis:// URL", "redis://h:6379", func(string) TLSFiles { return TLSFiles{CAFile: "a"} }, "need a rediss:// cache URL"},
		{"a client certificate that doesn't exist", "rediss://h:6379", func(dir string) TLSFiles {
			return TLSFiles{CertFile: filepath.Join(dir, "none.crt"), KeyFile: filepath.Join(dir, "none.key")}
		}, "load the cache client certificate"},
		{"a CA file without certificates", "rediss://h:6379", func(dir string) TLSFiles {
			return TLSFiles{CAFile: writeFile(t, filepath.Join(dir, "ca.crt"), []byte("not a certificate"))}
		}, "holds no PEM certificates"},
	}

	for _, tc := range invalid {
		t.Run("refuses "+tc.name+" at start-up", func(t *testing.T) {
			_, err := NewRedisStore(tc.url, tc.files(t.TempDir()))

			require.ErrorContains(t, err, tc.want)
		})
	}
}
