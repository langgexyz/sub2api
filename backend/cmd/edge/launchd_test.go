//go:build unit

package main

import (
	"encoding/xml"
	"io"
	"strings"
	"testing"
	"time"
)

func TestRenderLaunchdPlistWellFormed(t *testing.T) {
	plist := renderLaunchdPlist("dev.ccdirect.edge", "/usr/local/bin/edge",
		[]string{"daemon", "run", "-center", "https://cchub.example/edge", "-addr", ":8088"},
		"/Users/x/Library/Logs/ccdirect-edge.log")

	// Must be well-formed XML.
	dec := xml.NewDecoder(strings.NewReader(plist))
	for {
		_, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("plist is not well-formed XML: %v\n%s", err, plist)
		}
	}

	for _, want := range []string{
		"<key>Label</key><string>dev.ccdirect.edge</string>",
		"<string>/usr/local/bin/edge</string>",
		"<string>daemon</string>",
		"<string>run</string>",
		"<string>https://cchub.example/edge</string>",
		"<key>RunAtLoad</key><true/>",
		"<key>KeepAlive</key><true/>",
		"ccdirect-edge.log",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("plist missing %q:\n%s", want, plist)
		}
	}
}

func TestRenderLaunchdPlistEscapesXML(t *testing.T) {
	plist := renderLaunchdPlist("L", "/bin/edge", []string{"-center", "https://h/edge?a=1&b=2"}, "/log")
	if strings.Contains(plist, "a=1&b=2") {
		t.Fatalf("ampersand not escaped:\n%s", plist)
	}
	if !strings.Contains(plist, "a=1&amp;b=2") {
		t.Fatalf("expected escaped ampersand:\n%s", plist)
	}
}

func TestDaemonArgsFromConfig(t *testing.T) {
	cfg := edgeFlags{
		center:          "https://cchub/edge",
		addr:            ":8088",
		heartbeat:       10 * time.Second,
		maxFailover:     3,
		upstreamTimeout: 5 * time.Minute,
		upgradeInterval: 6 * time.Hour,
		// platforms / egressIP / upstreamProxy / internalKey / statePath empty
	}
	args := daemonArgsFromConfig(cfg)
	joined := strings.Join(args, " ")

	if args[0] != "daemon" || args[1] != "run" {
		t.Fatalf("args must start with `daemon run`: %v", args)
	}
	for _, want := range []string{"-center https://cchub/edge", "-addr :8088", "-upgrade-interval 6h0m0s"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args missing %q: %v", want, args)
		}
	}
	for _, omit := range []string{"-platforms", "-egress-ip", "-upstream-proxy", "-internal-key", "-session"} {
		if strings.Contains(joined, omit) {
			t.Fatalf("empty knob %q should be omitted: %v", omit, args)
		}
	}

	// Set optional knobs -> they appear.
	cfg.platforms = "anthropic,openai"
	cfg.internalKey = "secret"
	args2 := strings.Join(daemonArgsFromConfig(cfg), " ")
	if !strings.Contains(args2, "-platforms anthropic,openai") || !strings.Contains(args2, "-internal-key secret") {
		t.Fatalf("optional knobs not passed through: %s", args2)
	}
}
