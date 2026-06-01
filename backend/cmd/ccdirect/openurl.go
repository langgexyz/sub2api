package main

import (
	"os/exec"
	"runtime"
)

// openURL best-effort opens url in the user's default browser. On a headless
// host (e.g. a VPS over SSH) this simply fails silently — the caller has already
// printed the URL for the user to open manually, so it is never load-bearing.
func openURL(url string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
		args = []string{url}
	case "windows":
		cmd = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", url}
	default: // linux, *bsd
		cmd = "xdg-open"
		args = []string{url}
	}
	_ = exec.Command(cmd, args...).Start()
}
