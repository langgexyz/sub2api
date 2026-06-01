// Package edgetls builds the mutual-TLS (mTLS) configurations used between
// the edge gateway nodes and the center control plane.
//
// The center acts as a TLS server that requires and verifies edge client
// certificates; each edge acts as a TLS client that verifies the center's
// server certificate and presents its own client certificate.
package edgetls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// loadCAPool reads a PEM-encoded CA bundle from path and returns a cert pool
// containing it. It returns a clear error if the file is missing or contains
// no usable certificates.
func loadCAPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("edgetls: read CA file %q: %w", path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("edgetls: no valid certificates found in CA file %q", path)
	}
	return pool, nil
}

// ServerTLSConfig builds a *tls.Config for the CENTER: it presents
// certFile/keyFile and requires+verifies client certs signed by clientCAFile
// (mTLS). MinVersion is TLS 1.2. It returns a clear error if any file is
// missing or invalid.
func ServerTLSConfig(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("edgetls: load server keypair: %w", err)
	}
	clientCAs, err := loadCAPool(clientCAFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
	}, nil
}

// ClientTLSConfig builds a *tls.Config for the EDGE: it presents
// certFile/keyFile and verifies the server cert against serverCAFile.
// MinVersion is TLS 1.2. It returns a clear error if any file is missing or
// invalid.
func ClientTLSConfig(certFile, keyFile, serverCAFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("edgetls: load client keypair: %w", err)
	}
	rootCAs, err := loadCAPool(serverCAFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
		RootCAs:      rootCAs,
	}, nil
}
