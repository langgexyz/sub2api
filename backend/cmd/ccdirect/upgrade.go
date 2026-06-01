package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/Wei-Shaw/sub2api/ccgw/contract"
)

// cchubReleasePubKey is cchub's base64 Ed25519 release-signing public key, baked
// in at build time via -ldflags "-X main.cchubReleasePubKey=...". When empty,
// release verification is disabled and `upgrade` refuses to run (a self-update
// that trusts an unsigned manifest is a remote-code-execution hole). Get the
// value from cchub's ReleasePublicKey() and embed it for production builds.
var cchubReleasePubKey = ""

// runUpgrade implements `edge upgrade`: ask cchub for the latest release for this
// os/arch, verify cchub's signature over {version,url,sha256}, and if it is newer
// than the running binary, download it, check its SHA-256, and atomically swap
// the running executable. The user re-runs `edge` afterwards (the daemon, #16,
// does this automatically via checkAndUpgrade). The current binary is never left
// half-written (download to a temp file in the same dir, then rename over it).
func runUpgrade(args []string) error {
	cfg := parseFlags(args)
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate current binary: %w", err)
	}
	self, _ = filepath.EvalSymlinks(self)

	newVer, err := checkAndUpgrade(context.Background(), cfg, self)
	if err != nil {
		return err
	}
	if newVer == "" {
		fmt.Printf("already on %s (or no newer release for %s/%s); nothing to do\n", Version, runtime.GOOS, runtime.GOARCH)
		return nil
	}
	fmt.Printf("upgraded %s -> %s. Restart `edge` to run the new version.\n", Version, newVer)
	return nil
}

// checkAndUpgrade fetches cchub's signed release manifest for this os/arch, verifies
// the signature, and if it names a version different from the running one, downloads
// and atomically swaps the binary at selfPath. Returns the new version on a swap, ""
// when there is nothing to do (no release published, or already current). Any error
// leaves selfPath untouched. selfPath is a parameter (not always os.Executable) so
// the daemon ticker and tests can target a known file.
func checkAndUpgrade(ctx context.Context, cfg edgeFlags, selfPath string) (string, error) {
	pub, err := contract.DecodeReleasePubKey(cchubReleasePubKey)
	if err != nil {
		return "", fmt.Errorf("invalid embedded release pubkey: %w", err)
	}
	if pub == nil {
		return "", fmt.Errorf("this build has no embedded cchub release key; refusing to self-update from an unverifiable source (rebuild with -X main.cchubReleasePubKey=...)")
	}

	client := &http.Client{Timeout: 30 * time.Second}
	man, err := fetchReleaseManifest(ctx, client, cfg.center, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", err
	}
	if man.Empty() {
		return "", nil
	}
	if err := contract.VerifyRelease(pub, man); err != nil {
		return "", fmt.Errorf("release manifest failed signature verification: %w", err)
	}
	if man.Version == Version {
		return "", nil
	}
	if err := downloadVerifyReplace(ctx, client, man, selfPath); err != nil {
		return "", err
	}
	return man.Version, nil
}

// fetchReleaseManifest GETs cchub's signed manifest for os/arch. cchubURL is the
// /edge base (as parseFlags produced + already HTTPS-checked).
func fetchReleaseManifest(ctx context.Context, client *http.Client, cchubURL, goos, goarch string) (contract.ReleaseManifest, error) {
	var m contract.ReleaseManifest
	q := url.Values{"os": {goos}, "arch": {goarch}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cchubURL+"/v1/release?"+q.Encode(), nil)
	if err != nil {
		return m, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return m, fmt.Errorf("contact cchub: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return m, fmt.Errorf("cchub returned status %d for release query", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return m, fmt.Errorf("decode release manifest: %w", err)
	}
	return m, nil
}

// downloadVerifyReplace downloads man.URL to a temp file alongside dst, verifies
// its SHA-256 matches man.SHA256, copies dst's permission bits, and atomically
// renames it over dst. A checksum mismatch or any error removes the temp file and
// leaves dst untouched.
func downloadVerifyReplace(ctx context.Context, client *http.Client, man contract.ReleaseManifest, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, man.URL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download release: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download release: status %d", resp.StatusCode)
	}

	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, ".edge-upgrade-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpName) }

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		cleanup()
		return fmt.Errorf("write downloaded binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("flush downloaded binary: %w", err)
	}

	got := hex.EncodeToString(h.Sum(nil))
	if got != man.SHA256 {
		_ = os.Remove(tmpName)
		return fmt.Errorf("checksum mismatch: manifest %s, downloaded %s (refusing to install)", man.SHA256, got)
	}

	// Match the current binary's mode (executable); default 0755 if stat fails.
	mode := os.FileMode(0o755)
	if fi, err := os.Stat(dst); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("chmod new binary: %w", err)
	}
	// Atomic on the same filesystem (temp is in dst's dir). Replaces the running
	// binary's directory entry; the running process keeps the old inode until exit.
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("install new binary over %s: %w", dst, err)
	}
	return nil
}
