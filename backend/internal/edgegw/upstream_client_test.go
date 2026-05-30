//go:build unit

package edgegw

import (
	"net/http"
	"testing"
	"time"
)

func TestNewUpstreamClient_Direct(t *testing.T) {
	c, err := NewUpstreamClient("", 0)
	if err != nil {
		t.Fatalf("direct: %v", err)
	}
	if c.Timeout != 5*time.Minute {
		t.Fatalf("default timeout not applied: %v", c.Timeout)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type %T", c.Transport)
	}
	if tr.Proxy != nil {
		t.Fatalf("direct client must have no proxy")
	}
}

func TestNewUpstreamClient_HTTPProxy(t *testing.T) {
	c, err := NewUpstreamClient("http://127.0.0.1:3128", 30*time.Second)
	if err != nil {
		t.Fatalf("http proxy: %v", err)
	}
	tr := c.Transport.(*http.Transport)
	if tr.Proxy == nil {
		t.Fatalf("http proxy must set Transport.Proxy")
	}
	if c.Timeout != 30*time.Second {
		t.Fatalf("timeout not honored: %v", c.Timeout)
	}
}

func TestNewUpstreamClient_SOCKS5(t *testing.T) {
	c, err := NewUpstreamClient("socks5://127.0.0.1:1080", 0)
	if err != nil {
		t.Fatalf("socks5: %v", err)
	}
	if c == nil || c.Transport == nil {
		t.Fatalf("socks5 client/transport nil")
	}
}

func TestNewUpstreamClient_RejectsUnknownScheme(t *testing.T) {
	if _, err := NewUpstreamClient("ftp://nope", 0); err == nil {
		t.Fatalf("must reject unsupported scheme")
	}
}

func TestNewUpstreamClient_RejectsUnparseable(t *testing.T) {
	if _, err := NewUpstreamClient("://bad::url", 0); err == nil {
		t.Fatalf("must reject unparseable proxy url")
	}
}
