package ccdirect

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/proxy"
)

// NewUpstreamClient builds the HTTP client the edge uses to call upstream. When
// proxyURL is non-empty the upstream request egresses through that proxy, which
// is how an edge presents its VPS's stable IP to the provider. Supported
// schemes: http, https, socks5, socks5h. An empty proxyURL means direct egress
// (the edge's own IP).
func NewUpstreamClient(proxyURL string, timeout time.Duration) (*http.Client, error) {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}

	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("parse upstream proxy %q: %w", proxyURL, err)
		}
		switch u.Scheme {
		case "http", "https":
			transport.Proxy = http.ProxyURL(u)
		case "socks5", "socks5h":
			dialer, err := proxy.FromURL(u, proxy.Direct)
			if err != nil {
				return nil, fmt.Errorf("build socks5 dialer for %q: %w", proxyURL, err)
			}
			cd, ok := dialer.(proxy.ContextDialer)
			if !ok {
				return nil, fmt.Errorf("socks5 dialer for %q does not support context", proxyURL)
			}
			transport.DialContext = cd.DialContext
		default:
			return nil, fmt.Errorf("unsupported upstream proxy scheme %q (want http/https/socks5)", u.Scheme)
		}
	}

	return &http.Client{Transport: transport, Timeout: timeout}, nil
}
