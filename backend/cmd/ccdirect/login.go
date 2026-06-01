package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/ccgw/contract"
)

// Loopback + PKCE + device-key login client for the edge (ccdirect). authBase is
// the sub2api API root (center base with any trailing /edge stripped); cchubBase
// is the edge control-plane base (…/edge) used for GET /v1/config; centerWebBase
// is the sub2api web origin that serves the /cli/authorize SPA route.
// See docs/tech/ccdirect-auth-contract.md.

// loginResult carries everything a successful login yields.
type loginResult struct {
	access     string
	refresh    string
	ccdirectID string
	secret     []byte
}

// cliTokenResp is the POST /api/v1/auth/cli/token response. Fields may be
// returned either at the top level or wrapped in a data envelope depending on
// the center's response style; both are decoded.
type cliTokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Data         struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	} `json:"data"`
}

func (r cliTokenResp) tokens() (access, refresh string) {
	access, refresh = r.AccessToken, r.RefreshToken
	if access == "" {
		access = r.Data.AccessToken
	}
	if refresh == "" {
		refresh = r.Data.RefreshToken
	}
	return access, refresh
}

// callbackResult is what the loopback /callback handler captures.
type callbackResult struct {
	code  string
	state string
}

// loginTimeout bounds how long we wait for the browser-side authorization.
const loginTimeout = 5 * time.Minute

// loopbackLogin runs the loopback + PKCE + device-key login: it spins up a
// localhost callback server, opens the browser to the center's /cli/authorize
// page (carrying the PKCE challenge, redirect_uri, state, and device pubkey),
// waits for the authorization code to land on the loopback server, exchanges it
// for tokens at /api/v1/auth/cli/token, then fetches the edge config (seal
// secret + edge id). Blocks until success, error, or ctx done.
func loopbackLogin(ctx context.Context, hc *http.Client, authBase, centerWebBase, cchubBase string, dk deviceKey) (loginResult, error) {
	verifier, err := genVerifier()
	if err != nil {
		return loginResult{}, fmt.Errorf("generate code verifier: %w", err)
	}
	challenge := codeChallenge(verifier)
	state, err := genState()
	if err != nil {
		return loginResult{}, fmt.Errorf("generate state: %w", err)
	}

	// Start the loopback callback server on an OS-assigned port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return loginResult{}, fmt.Errorf("start loopback listener: %w", err)
	}
	tcpAddr, _ := ln.Addr().(*net.TCPAddr)
	port := tcpAddr.Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	resultCh := make(chan callbackResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		code, st := q.Get("code"), q.Get("state")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if code == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "login failed: no authorization code received. You can close this tab.")
			return
		}
		_, _ = io.WriteString(w, "Login complete. You can close this tab and return to the terminal.")
		select {
		case resultCh <- callbackResult{code: code, state: st}:
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 15 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	// Build the browser authorization URL.
	authURL := buildAuthorizeURL(centerWebBase, challenge, redirectURI, state, dk.publicKeyB64())
	fmt.Println()
	fmt.Println("  To authorize this edge, open this URL in your browser:")
	fmt.Printf("    %s\n", authURL)
	fmt.Println()
	openURL(authURL)
	fmt.Print("  Waiting for authorization in the browser…")

	// Block on the callback (with ctx + a timeout).
	timeout := time.NewTimer(loginTimeout)
	defer timeout.Stop()
	var cb callbackResult
	select {
	case <-ctx.Done():
		fmt.Println()
		return loginResult{}, ctx.Err()
	case <-timeout.C:
		fmt.Println()
		return loginResult{}, errors.New("timed out waiting for browser authorization")
	case cb = <-resultCh:
	}
	fmt.Println(" ok")

	if cb.state != state {
		return loginResult{}, errors.New("state mismatch (possible CSRF); aborting login")
	}

	// Exchange the authorization code for tokens.
	tokResp, err := postJSON[cliTokenResp](ctx, hc, authBase+"/api/v1/auth/cli/token", map[string]string{
		"grant_type":    "authorization_code",
		"code":          cb.code,
		"code_verifier": verifier,
		"redirect_uri":  redirectURI,
	}, "")
	if err != nil {
		return loginResult{}, fmt.Errorf("exchange authorization code: %w", err)
	}
	access, refresh := tokResp.tokens()
	if access == "" {
		return loginResult{}, errors.New("center returned an empty access token")
	}

	cfg, err := fetchConfig(ctx, hc, cchubBase, access)
	if err != nil {
		return loginResult{}, fmt.Errorf("fetch edge config: %w", err)
	}
	return loginResult{access: access, refresh: refresh, ccdirectID: cfg.CCDirectID, secret: []byte(cfg.TokenSecret)}, nil
}

// genVerifier returns a PKCE code_verifier: 43-char base64url (no padding) of 32
// random bytes.
func genVerifier() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// codeChallenge returns base64url(sha256(verifier)) (PKCE S256).
func codeChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// genState returns base64url(16 random bytes), echoed back by the callback and
// checked by the edge.
func genState() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// buildAuthorizeURL composes <centerWebBase>/cli/authorize?... per the contract.
func buildAuthorizeURL(centerWebBase, challenge, redirectURI, state, devicePubB64 string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	q.Set("device_pubkey", devicePubB64)
	if host, err := os.Hostname(); err == nil && host != "" {
		q.Set("name", host)
	}
	return strings.TrimRight(centerWebBase, "/") + "/cli/authorize?" + q.Encode()
}

// fetchConfig calls GET /edge/v1/config with the owner JWT and returns the
// center-issued edge config (seal secret, edge id, platforms, …).
func fetchConfig(ctx context.Context, hc *http.Client, cchubBase, access string) (contract.EnrollResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(cchubBase, "/")+"/v1/config", nil)
	if err != nil {
		return contract.EnrollResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := hc.Do(req)
	if err != nil {
		return contract.EnrollResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return contract.EnrollResponse{}, fmt.Errorf("config status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var out contract.EnrollResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return contract.EnrollResponse{}, err
	}
	return out, nil
}

// logoutCenter best-effort revokes the refresh token server-side.
func logoutCenter(ctx context.Context, hc *http.Client, authBase, refresh string) {
	if refresh == "" {
		return
	}
	_, _ = postJSON[struct{}](ctx, hc, authBase+"/api/v1/auth/logout", map[string]string{"refresh_token": refresh}, "")
}

// postJSON posts an optional JSON body and decodes the JSON response into T.
func postJSON[T any](ctx context.Context, hc *http.Client, url string, body any, bearer string) (T, error) {
	var zero T
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return zero, err
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, rdr)
	if err != nil {
		return zero, err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return zero, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return zero, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return zero, err
	}
	return out, nil
}

// authBaseFromCenter strips a trailing /edge from the center edge base so auth
// calls reach the sub2api API root.
func authBaseFromCenter(cchubBase string) string {
	return strings.TrimSuffix(strings.TrimRight(cchubBase, "/"), "/edge")
}
