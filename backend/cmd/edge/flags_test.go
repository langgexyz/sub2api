//go:build unit

package main

import "testing"

func TestRequireSecureCenter(t *testing.T) {
	cases := []struct {
		name          string
		url           string
		allowInsecure bool
		wantErr       bool
	}{
		{"https ok", "https://cchub.example.com/edge", false, false},
		{"http loopback ip ok", "http://127.0.0.1:8080/edge", false, false},
		{"http localhost ok", "http://localhost:8080/edge", false, false},
		{"http ipv6 loopback ok", "http://[::1]:8080/edge", false, false},
		{"http public rejected", "http://cchub.example.com/edge", false, true},
		{"http public allowed with insecure", "http://cchub.example.com/edge", true, false},
		{"bad scheme rejected", "ftp://cchub.example.com", false, true},
		{"https public still ok with insecure off", "https://ccdirect.dev/edge", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := requireSecureCenter(c.url, c.allowInsecure)
			if (err != nil) != c.wantErr {
				t.Fatalf("requireSecureCenter(%q, %v) err=%v, wantErr=%v", c.url, c.allowInsecure, err, c.wantErr)
			}
		})
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for _, h := range []string{"localhost", "127.0.0.1", "::1", "127.0.0.5"} {
		if !isLoopbackHost(h) {
			t.Errorf("isLoopbackHost(%q) = false, want true", h)
		}
	}
	for _, h := range []string{"example.com", "10.0.0.1", "8.8.8.8", ""} {
		if isLoopbackHost(h) {
			t.Errorf("isLoopbackHost(%q) = true, want false", h)
		}
	}
}
