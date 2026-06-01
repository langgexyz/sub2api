//go:build unit

package handler

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// mkCA creates a self-signed CA and returns (caCertPEM, signer cert, signer key).
func mkCA(t *testing.T) ([]byte, *x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "edge-ca"},
		NotBefore:             time.Unix(1_700_000_000, 0),
		NotAfter:              time.Unix(1_900_000_000, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return caPEM, cert, key
}

// mkClientCertPEM issues a client cert signed by the CA and returns its PEM.
func mkClientCertPEM(t *testing.T, caCert *x509.Certificate, caKey *ecdsa.PrivateKey, signer bool) []byte {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "edge-1"},
		NotBefore:    time.Unix(1_700_000_000, 0),
		NotAfter:     time.Unix(1_900_000_000, 0),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	parent, parentKey := caCert, caKey
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func newGuardWithCA(t *testing.T, caPEM []byte) *CCDirectMTLSGuard {
	t.Helper()
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCDIRECT_MTLS_CLIENT_CA", caPath)
	g, err := NewCCDirectMTLSGuard()
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	return g
}

func runGuard(g *CCDirectMTLSGuard, setup func(*http.Request)) int {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/edge/v1/lease", g.Middleware(), func(c *gin.Context) { c.Status(http.StatusOK) })
	req := httptest.NewRequest(http.MethodPost, "/edge/v1/lease", nil)
	if setup != nil {
		setup(req)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

func TestEdgeMTLSGuard_Disabled_NoCA(t *testing.T) {
	t.Setenv("CCDIRECT_MTLS_CLIENT_CA", "")
	g, err := NewCCDirectMTLSGuard()
	if err != nil {
		t.Fatal(err)
	}
	if g.Enabled() {
		t.Fatalf("guard must be disabled without a CA")
	}
	if code := runGuard(g, nil); code != http.StatusOK {
		t.Fatalf("disabled guard must pass, got %d", code)
	}
}

func TestEdgeMTLSGuard_NoCert_Rejected(t *testing.T) {
	caPEM, _, _ := mkCA(t)
	g := newGuardWithCA(t, caPEM)
	if code := runGuard(g, nil); code != http.StatusForbidden {
		t.Fatalf("missing client cert must be 403, got %d", code)
	}
}

func TestEdgeMTLSGuard_ValidForwardedCert_Accepted(t *testing.T) {
	caPEM, caCert, caKey := mkCA(t)
	g := newGuardWithCA(t, caPEM)
	clientPEM := mkClientCertPEM(t, caCert, caKey, false)
	code := runGuard(g, func(r *http.Request) {
		r.Header.Set(clientCertHeader, url.QueryEscape(string(clientPEM)))
	})
	if code != http.StatusOK {
		t.Fatalf("valid forwarded client cert must pass, got %d", code)
	}
}

func TestEdgeMTLSGuard_UntrustedCert_Rejected(t *testing.T) {
	caPEM, _, _ := mkCA(t)
	g := newGuardWithCA(t, caPEM)
	// Cert signed by a DIFFERENT CA.
	_, otherCert, otherKey := mkCA(t)
	rogue := mkClientCertPEM(t, otherCert, otherKey, false)
	code := runGuard(g, func(r *http.Request) {
		r.Header.Set(clientCertHeader, url.QueryEscape(string(rogue)))
	})
	if code != http.StatusForbidden {
		t.Fatalf("untrusted client cert must be 403, got %d", code)
	}
}
