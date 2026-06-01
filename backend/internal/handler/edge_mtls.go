package handler

import (
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/url"
	"os"

	"github.com/gin-gonic/gin"
)

// EdgeMTLSGuard enforces mutual TLS on the edge control-plane routes
// (/edge/v1/*). When an edge client CA is configured (CCDIRECT_MTLS_CLIENT_CA file),
// every request to these routes must present a client certificate that verifies
// against that CA — narrowing "who can obtain a lease token" from "anyone who
// can reach /edge/v1/lease" to "a node holding a center-issued edge certificate"
// (revoke the cert to cut a node off).
//
// The client cert is taken either from the in-process TLS handshake
// (Request.TLS, when sub2api itself terminates TLS with RequestClientCert) or
// from a trusted reverse proxy that forwards it as a PEM header
// (X-Edge-Client-Cert, URL-escaped — the common nginx/Caddy deployment, since
// sub2api normally runs behind a TLS-terminating proxy).
//
// When no CA is configured the guard is a no-op (dev / single-host). It is
// additive: it only gates the edge group, never the gateway or admin surface.
type EdgeMTLSGuard struct {
	pool *x509.CertPool
}

// clientCertHeader carries a reverse-proxy-forwarded client certificate (PEM,
// URL-escaped). nginx: proxy_set_header X-Edge-Client-Cert $ssl_client_escaped_cert;
const clientCertHeader = "X-Edge-Client-Cert"

// NewEdgeMTLSGuard loads the edge client CA from CCDIRECT_MTLS_CLIENT_CA if set.
func NewEdgeMTLSGuard() (*EdgeMTLSGuard, error) {
	path := os.Getenv("CCDIRECT_MTLS_CLIENT_CA")
	if path == "" {
		return &EdgeMTLSGuard{}, nil
	}
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errEdgeCAParse
	}
	return &EdgeMTLSGuard{pool: pool}, nil
}

// Enabled reports whether mTLS enforcement is active.
func (g *EdgeMTLSGuard) Enabled() bool { return g != nil && g.pool != nil }

// Middleware verifies the client certificate when enforcement is enabled.
func (g *EdgeMTLSGuard) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !g.Enabled() {
			c.Next()
			return
		}
		chain := edgeClientChain(c.Request)
		if len(chain) == 0 {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": gin.H{"code": "mtls_required", "message": "edge client certificate required"},
			})
			return
		}
		opts := x509.VerifyOptions{
			Roots:         g.pool,
			Intermediates: x509.NewCertPool(),
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}
		for _, ic := range chain[1:] {
			opts.Intermediates.AddCert(ic)
		}
		if _, err := chain[0].Verify(opts); err != nil {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": gin.H{"code": "mtls_invalid", "message": "edge client certificate not trusted"},
			})
			return
		}
		c.Next()
	}
}

// edgeClientChain returns the presented client cert chain, from the in-process
// TLS handshake or a reverse-proxy-forwarded PEM header.
func edgeClientChain(r *http.Request) []*x509.Certificate {
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		return r.TLS.PeerCertificates
	}
	raw := r.Header.Get(clientCertHeader)
	if raw == "" {
		return nil
	}
	if unescaped, err := url.QueryUnescape(raw); err == nil {
		raw = unescaped
	}
	var certs []*x509.Certificate
	rest := []byte(raw)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
			certs = append(certs, cert)
		}
	}
	return certs
}

type edgeCAErr string

func (e edgeCAErr) Error() string { return string(e) }

const errEdgeCAParse = edgeCAErr("edge mTLS client CA: no certificates parsed")
