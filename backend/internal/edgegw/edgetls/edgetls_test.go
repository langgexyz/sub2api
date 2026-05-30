//go:build unit

package edgetls

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
	"testing"
	"time"
)

// certKeyPaths holds the on-disk PEM paths for an issued certificate.
type certKeyPaths struct {
	certPath string
	keyPath  string
}

// testCA is a self-signed certificate authority used to issue leaf certs.
type testCA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	caPath  string // PEM file containing the CA certificate
	tempDir string
}

// newTestCA creates an in-memory self-signed CA and writes its certificate
// PEM into dir.
func newTestCA(t *testing.T, dir string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "edgetls-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	caPath := filepath.Join(dir, "ca.pem")
	writePEM(t, caPath, "CERTIFICATE", der)
	return &testCA{cert: cert, key: key, caPath: caPath, tempDir: dir}
}

// issue creates a leaf certificate signed by the CA and writes the cert and
// key PEM files into the CA's temp dir under the given name prefix.
func (ca *testCA) issue(t *testing.T, name string, isServer bool) certKeyPaths {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate %s key: %v", name, err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if isServer {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.DNSNames = []string{"localhost"}
		tmpl.IPAddresses = []net.IP{net.IPv4(127, 0, 0, 1)}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("create %s cert: %v", name, err)
	}
	certPath := filepath.Join(ca.tempDir, name+".pem")
	keyPath := filepath.Join(ca.tempDir, name+"-key.pem")
	writePEM(t, certPath, "CERTIFICATE", der)
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal %s key: %v", name, err)
	}
	writePEM(t, keyPath, "EC PRIVATE KEY", keyDER)
	return certKeyPaths{certPath: certPath, keyPath: keyPath}
}

// writePEM encodes der under blockType and writes it to path.
func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	buf := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatalf("write PEM %q: %v", path, err)
	}
}

// TestMTLSEndToEnd proves the server and client configs interoperate over a
// real TLS handshake with mutual authentication.
func TestMTLSEndToEnd(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCA(t, dir)
	server := ca.issue(t, "server", true)
	client := ca.issue(t, "client", false)

	serverCfg, err := ServerTLSConfig(server.certPath, server.keyPath, ca.caPath)
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}
	clientCfg, err := ClientTLSConfig(client.certPath, client.keyPath, ca.caPath)
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverCfg)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	const payload = byte(0x42)
	serverErr := make(chan error, 1)
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			serverErr <- aerr
			return
		}
		defer func() { _ = conn.Close() }()
		tlsConn, ok := conn.(*tls.Conn)
		if !ok {
			serverErr <- errNotTLS
			return
		}
		if herr := tlsConn.Handshake(); herr != nil {
			serverErr <- herr
			return
		}
		if _, werr := tlsConn.Write([]byte{payload}); werr != nil {
			serverErr <- werr
			return
		}
		serverErr <- nil
	}()

	conn, err := tls.Dial("tcp", ln.Addr().String(), clientCfg)
	if err != nil {
		t.Fatalf("tls.Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("client read: %v", err)
	}
	if buf[0] != payload {
		t.Fatalf("client read = %#x, want %#x", buf[0], payload)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("server side: %v", err)
	}
}

// TestMTLSRejectsClientWithoutCert verifies that a client which presents no
// client certificate fails the handshake against a server configured with
// RequireAndVerifyClientCert.
func TestMTLSRejectsClientWithoutCert(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCA(t, dir)
	server := ca.issue(t, "server", true)

	serverCfg, err := ServerTLSConfig(server.certPath, server.keyPath, ca.caPath)
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}

	caPool, err := loadCAPool(ca.caPath)
	if err != nil {
		t.Fatalf("loadCAPool: %v", err)
	}
	// Client trusts the server CA but presents no client certificate.
	clientCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    caPool,
		ServerName: "localhost",
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverCfg)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		if tlsConn, ok := conn.(*tls.Conn); ok {
			// Expected to fail because the client sends no cert.
			_ = tlsConn.Handshake()
		}
	}()

	conn, err := tls.Dial("tcp", ln.Addr().String(), clientCfg)
	if err != nil {
		// Handshake rejected immediately: this is the expected outcome.
		return
	}
	// On some TLS versions the client Dial returns before the server has
	// verified the (missing) client certificate. The rejection then surfaces
	// on the first read. Either way the connection must not be usable.
	defer func() { _ = conn.Close() }()
	buf := make([]byte, 1)
	if _, rerr := conn.Read(buf); rerr == nil {
		t.Fatal("expected mTLS to reject a client without a certificate, but the connection succeeded")
	}
}

// TestServerTLSConfigErrors verifies that missing files produce errors.
func TestServerTLSConfigErrors(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCA(t, dir)
	server := ca.issue(t, "server", true)
	missing := filepath.Join(dir, "does-not-exist.pem")

	if _, err := ServerTLSConfig(missing, server.keyPath, ca.caPath); err == nil {
		t.Fatal("expected error for missing cert file")
	}
	if _, err := ServerTLSConfig(server.certPath, server.keyPath, missing); err == nil {
		t.Fatal("expected error for missing client CA file")
	}
}

// TestClientTLSConfigErrors verifies that missing files produce errors.
func TestClientTLSConfigErrors(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCA(t, dir)
	client := ca.issue(t, "client", false)
	missing := filepath.Join(dir, "does-not-exist.pem")

	if _, err := ClientTLSConfig(missing, client.keyPath, ca.caPath); err == nil {
		t.Fatal("expected error for missing cert file")
	}
	if _, err := ClientTLSConfig(client.certPath, client.keyPath, missing); err == nil {
		t.Fatal("expected error for missing server CA file")
	}
}

// errNotTLS is returned by the test server goroutine when the accepted
// connection is unexpectedly not a *tls.Conn.
var errNotTLS = errTLS("accepted connection is not a *tls.Conn")

// errTLS is a lightweight error type for test sentinels.
type errTLS string

func (e errTLS) Error() string { return string(e) }
