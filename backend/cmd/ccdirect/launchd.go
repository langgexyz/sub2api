package main

import (
	"encoding/xml"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// launchd.go is the macOS process-manager integration for `edge daemon`
// (install/uninstall/status). It writes a LaunchAgent plist with KeepAlive so
// launchd restarts the daemon on crash and after a self-update exit, then loads
// it. Linux systemd is a TODO (#16); install/uninstall/status refuse on non-darwin.

const launchdLabel = "dev.ccdirect.edge"

// launchdPlistPath returns ~/Library/LaunchAgents/<label>.plist.
func launchdPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
}

// launchdLogPath returns the daemon's stdout/stderr log file.
func launchdLogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Logs", "ccdirect-edge.log"), nil
}

// daemonArgsFromConfig reconstructs the `daemon run` argv that reproduces cfg, so
// the installed agent serves with the same operational knobs the user installed
// with. Empty/secret-free knobs are omitted; the rest are passed explicitly (not
// relying on env or defaults) so the plist is self-describing.
func daemonArgsFromConfig(cfg edgeFlags) []string {
	args := []string{
		"daemon", "run",
		"-center", cfg.center,
		"-addr", cfg.addr,
		"-heartbeat", cfg.heartbeat.String(),
		"-max-failover", strconv.Itoa(cfg.maxFailover),
		"-upstream-timeout", cfg.upstreamTimeout.String(),
		"-upgrade-interval", cfg.upgradeInterval.String(),
	}
	if cfg.platforms != "" {
		args = append(args, "-platforms", cfg.platforms)
	}
	if cfg.egressIP != "" {
		args = append(args, "-egress-ip", cfg.egressIP)
	}
	if cfg.upstreamProxy != "" {
		args = append(args, "-upstream-proxy", cfg.upstreamProxy)
	}
	if cfg.internalKey != "" {
		args = append(args, "-internal-key", cfg.internalKey)
	}
	if cfg.statePath != "" {
		args = append(args, "-session", cfg.statePath)
	}
	return args
}

// renderLaunchdPlist builds the LaunchAgent plist XML. binPath is the absolute edge
// binary path; args is the full argv after it; logPath captures stdout+stderr.
func renderLaunchdPlist(label, binPath string, args []string, logPath string) string {
	progArgs := []string{"    <string>" + xmlString(binPath) + "</string>"}
	for _, a := range args {
		progArgs = append(progArgs, "    <string>"+xmlString(a)+"</string>")
	}
	return strings.Join([]string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">`,
		`<plist version="1.0">`,
		"<dict>",
		"  <key>Label</key><string>" + xmlString(label) + "</string>",
		"  <key>ProgramArguments</key>",
		"  <array>",
		strings.Join(progArgs, "\n"),
		"  </array>",
		"  <key>RunAtLoad</key><true/>",
		"  <key>KeepAlive</key><true/>",
		"  <key>StandardOutPath</key><string>" + xmlString(logPath) + "</string>",
		"  <key>StandardErrorPath</key><string>" + xmlString(logPath) + "</string>",
		"</dict>",
		"</plist>",
		"",
	}, "\n")
}

// xmlString escapes a value for inclusion in plist text content.
func xmlString(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// requireDarwin fails fast with a clear message on non-macOS hosts.
func requireDarwin(action string) {
	if runtime.GOOS != "darwin" {
		log.Fatalf("edge daemon %s: launchd integration is macOS-only; Linux systemd is not implemented yet (#16)", action)
	}
}

// runDaemonInstall writes + loads the LaunchAgent. Idempotent: an existing agent is
// unloaded and overwritten so re-running with new flags re-installs cleanly.
func runDaemonInstall(args []string) {
	requireDarwin("install")
	cfg := parseFlags(args)

	bin, err := os.Executable()
	if err != nil {
		log.Fatalf("edge daemon install: locate binary: %v", err)
	}
	bin, _ = filepath.EvalSymlinks(bin)

	plistPath, err := launchdPlistPath()
	if err != nil {
		log.Fatalf("edge daemon install: %v", err)
	}
	logPath, err := launchdLogPath()
	if err != nil {
		log.Fatalf("edge daemon install: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		log.Fatalf("edge daemon install: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		log.Fatalf("edge daemon install: %v", err)
	}

	plist := renderLaunchdPlist(launchdLabel, bin, daemonArgsFromConfig(cfg), logPath)

	// Unload any prior agent so load doesn't fail on a duplicate label.
	_ = exec.Command("launchctl", "unload", plistPath).Run()
	// 0600: the plist may embed -internal-key.
	if err := os.WriteFile(plistPath, []byte(plist), 0o600); err != nil {
		log.Fatalf("edge daemon install: write plist: %v", err)
	}
	if out, err := exec.Command("launchctl", "load", "-w", plistPath).CombinedOutput(); err != nil {
		log.Fatalf("edge daemon install: launchctl load: %v: %s", err, strings.TrimSpace(string(out)))
	}
	fmt.Printf("installed launchd agent %s\n  plist: %s\n  logs:  %s\n", launchdLabel, plistPath, logPath)
	fmt.Println("the daemon now starts at login and restarts on crash / self-update.")
}

// runDaemonUninstall unloads + removes the LaunchAgent. Safe to run when not
// installed (missing plist / not-loaded are ignored).
func runDaemonUninstall(_ []string) {
	requireDarwin("uninstall")
	plistPath, err := launchdPlistPath()
	if err != nil {
		log.Fatalf("edge daemon uninstall: %v", err)
	}
	_ = exec.Command("launchctl", "unload", "-w", plistPath).Run()
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		log.Fatalf("edge daemon uninstall: remove plist: %v", err)
	}
	fmt.Printf("uninstalled launchd agent %s\n", launchdLabel)
}

// runDaemonStatus reports launchd load state and, if the daemon is up, its relay
// status over the control socket.
func runDaemonStatus(args []string) {
	requireDarwin("status")
	cfg := parseFlags(args)

	if out, err := exec.Command("launchctl", "list", launchdLabel).CombinedOutput(); err != nil {
		fmt.Printf("launchd: not loaded (%s)\n", launchdLabel)
	} else {
		fmt.Printf("launchd: loaded (%s)\n%s", launchdLabel, out)
	}

	if sock, err := controlSocketPath(resolveSessionPath(cfg)); err == nil {
		fmt.Println("relay:")
		printClientStatus(sock)
	}
}
