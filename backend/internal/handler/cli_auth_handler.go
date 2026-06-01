package handler

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"

	"github.com/gin-gonic/gin"
)

// Loopback + PKCE + device-key CLI login (the edge / ccdirect).
//
// Flow:
//  1. edge starts a loopback server, generates a PKCE verifier/challenge + state
//     + device Ed25519 key, opens the browser to <web>/cli/authorize?...
//  2. browser (already logged in) POSTs /api/v1/auth/cli/authorize (JWT) -> the
//     center mints a single-use authorization_code bound to the user + PKCE
//     challenge + device pubkey, and returns redirect_to (loopback URL + code).
//  3. browser redirects to the loopback URL; the edge receives the code.
//  4. edge POSTs /api/v1/auth/cli/token (public) with code + code_verifier ->
//     the center verifies PKCE, mints a token pair, and binds the refresh token
//     to the device pubkey.
//
// See docs/tech/ccdirect-auth-contract.md (the binding contract).

// deviceSignatureSkew is the maximum allowed clock skew for the device-signature
// timestamp (contract: +/-120s).
const deviceSignatureSkew = 120 * time.Second

// Device-signature headers carried by the edge on bound calls.
const (
	headerCcdirectTimestamp = "X-CCDirect-Timestamp"
	headerCcdirectSignature = "X-CCDirect-Signature"
)

// errInvalidPubKeyLen is returned when a decoded device pubkey is not 32 bytes.
var errInvalidPubKeyLen = errors.New("device pubkey must be 32 bytes")

// CLIAuthorizeRequest is the body of POST /api/v1/auth/cli/authorize.
type CLIAuthorizeRequest struct {
	ResponseType        string `json:"response_type"`
	CodeChallenge       string `json:"code_challenge"`
	CodeChallengeMethod string `json:"code_challenge_method"`
	RedirectURI         string `json:"redirect_uri"`
	State               string `json:"state"`
	DevicePubKey        string `json:"device_pubkey"`
	Name                string `json:"name"`
}

// CLIAuthorizeResponse is the response of POST /api/v1/auth/cli/authorize.
type CLIAuthorizeResponse struct {
	RedirectTo string `json:"redirect_to"`
}

// CLIAuthorize issues a single-use authorization_code for the loopback+PKCE CLI
// login, bound to the authenticated user. Requires a logged-in session (JWT) —
// the browser is already authenticated.
// POST /api/v1/auth/cli/authorize
func (h *AuthHandler) CLIAuthorize(c *gin.Context) {
	var req CLIAuthorizeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}

	// PKCE: only S256 with a non-empty challenge is accepted.
	if req.CodeChallengeMethod != "S256" {
		response.BadRequest(c, "code_challenge_method must be S256")
		return
	}
	if strings.TrimSpace(req.CodeChallenge) == "" {
		response.BadRequest(c, "code_challenge is required")
		return
	}
	if strings.TrimSpace(req.DevicePubKey) == "" {
		response.BadRequest(c, "device_pubkey is required")
		return
	}

	// Loopback-only redirect: the code must land on the initiating machine.
	if !isLoopbackRedirectURI(req.RedirectURI) {
		response.BadRequest(c, "redirect_uri must be a loopback (127.0.0.1/localhost) address")
		return
	}

	code := h.cliGrantStore.create(subject.UserID, req.CodeChallenge, req.DevicePubKey, req.RedirectURI)

	redirectTo := appendQueryParams(req.RedirectURI, map[string]string{
		"code":  code,
		"state": req.State,
	})

	slog.Info("cli_authorize issued",
		"user_id", subject.UserID,
		"redirect_host", redirectHost(req.RedirectURI),
		"name", req.Name)

	response.Success(c, CLIAuthorizeResponse{RedirectTo: redirectTo})
}

// CLITokenRequest is the body of POST /api/v1/auth/cli/token.
type CLITokenRequest struct {
	GrantType    string `json:"grant_type"`
	Code         string `json:"code"`
	CodeVerifier string `json:"code_verifier"`
	RedirectURI  string `json:"redirect_uri"`
}

// CLITokenResponse is the response of POST /api/v1/auth/cli/token.
type CLITokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
	DeviceBound  bool   `json:"device_bound"`
}

// CLIToken exchanges an authorization_code + PKCE verifier for a token pair and
// binds the refresh token to the device pubkey. Public (no auth): identity comes
// from the grant, not the caller.
// POST /api/v1/auth/cli/token
func (h *AuthHandler) CLIToken(c *gin.Context) {
	var req CLITokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	if req.GrantType != "" && req.GrantType != "authorization_code" {
		response.BadRequest(c, "unsupported grant_type")
		return
	}
	if strings.TrimSpace(req.Code) == "" || strings.TrimSpace(req.CodeVerifier) == "" {
		response.BadRequest(c, "code and code_verifier are required")
		return
	}

	// Look up + consume the grant (one-shot). Rejects missing/expired/consumed.
	grant, ok := h.cliGrantStore.consume(req.Code)
	if !ok {
		response.BadRequest(c, "invalid or expired code")
		return
	}

	// PKCE: base64url(sha256(code_verifier)) must equal the stored challenge.
	if !verifyPKCES256(req.CodeVerifier, grant.codeChallenge) {
		response.BadRequest(c, "code_verifier does not match code_challenge")
		return
	}

	// redirect_uri must match the one the grant was issued for.
	if req.RedirectURI != grant.redirectURI {
		response.BadRequest(c, "redirect_uri mismatch")
		return
	}

	// Load the user and mint a token pair, the same path as a password login.
	user, err := h.userService.GetByID(c.Request.Context(), grant.userID)
	if err != nil || user == nil {
		response.Unauthorized(c, "grant user not found")
		return
	}
	if err := ensureLoginUserActive(user); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	tokenPair, err := h.authService.GenerateTokenPair(c.Request.Context(), user, "")
	if err != nil {
		slog.Error("cli_token failed to generate token pair", "error", err, "user_id", user.ID)
		response.InternalError(c, "Failed to generate token")
		return
	}

	// Bind the refresh token to the device pubkey so future refreshes require a
	// device signature.
	h.deviceBindings.bind(tokenPair.RefreshToken, grant.devicePubKey)

	slog.Info("cli_token minted", "user_id", user.ID, "device_bound", true)

	response.Success(c, CLITokenResponse{
		AccessToken:  tokenPair.AccessToken,
		RefreshToken: tokenPair.RefreshToken,
		ExpiresIn:    tokenPair.ExpiresIn,
		TokenType:    "Bearer",
		DeviceBound:  true,
	})
}

// verifyDeviceRefreshSignature checks that a device-bound refresh request
// carries a valid Ed25519 signature over the canonical string, verified against
// the pubkey bound to the refresh token. It returns (ok, isBound). When the
// token is not bound, isBound is false and the caller proceeds unchanged.
//
// rawBody is the exact bytes of the request body (read before binding the JSON).
func (h *AuthHandler) verifyDeviceRefreshSignature(c *gin.Context, refreshToken string, rawBody []byte) (ok bool, isBound bool) {
	pubKeyB64, bound := h.deviceBindings.lookup(refreshToken)
	if !bound {
		return true, false
	}

	tsStr := strings.TrimSpace(c.GetHeader(headerCcdirectTimestamp))
	sigB64 := strings.TrimSpace(c.GetHeader(headerCcdirectSignature))
	if tsStr == "" || sigB64 == "" {
		return false, true
	}

	tsUnix, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return false, true
	}
	// Reject stale/early timestamps (contract: +/-120s).
	skew := time.Since(time.Unix(tsUnix, 0))
	if skew < 0 {
		skew = -skew
	}
	if skew > deviceSignatureSkew {
		return false, true
	}

	pubKey, err := decodeDevicePubKey(pubKeyB64)
	if err != nil {
		return false, true
	}

	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return false, true
	}

	canonical := canonicalSignString(c.Request.Method, c.Request.URL.Path, tsStr, rawBody)
	if !ed25519.Verify(pubKey, []byte(canonical), sig) {
		return false, true
	}
	return true, true
}

// canonicalSignString builds the exact \n-joined canonical string the device
// signs (no trailing newline). See the contract:
//
//	<HTTP_METHOD>
//	<REQUEST_PATH>            // no query
//	<X-CCDirect-Timestamp>
//	<hex(sha256(raw_request_body))>
func canonicalSignString(method, path, timestamp string, rawBody []byte) string {
	sum := sha256.Sum256(rawBody)
	return strings.Join([]string{
		method,
		path,
		timestamp,
		hex.EncodeToString(sum[:]),
	}, "\n")
}

// decodeDevicePubKey decodes a base64 raw 32-byte Ed25519 public key. It accepts
// both standard and URL-safe base64 (padded or not) for robustness.
func decodeDevicePubKey(s string) (ed25519.PublicKey, error) {
	var raw []byte
	var err error
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		raw, err = enc.DecodeString(s)
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, errInvalidPubKeyLen
	}
	return ed25519.PublicKey(raw), nil
}

// verifyPKCES256 reports whether base64url(sha256(verifier)) == challenge using
// a constant-time compare.
func verifyPKCES256(verifier, challenge string) bool {
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) == 1
}

// isLoopbackRedirectURI reports whether redirect is an http(s) URL whose host is
// 127.0.0.1, localhost, or ::1 (loopback only). Any other scheme/host is
// rejected.
func isLoopbackRedirectURI(redirect string) bool {
	u, err := url.Parse(redirect)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	host := u.Hostname()
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

// redirectHost returns the host[:port] of a redirect URI for logging (best
// effort; empty on parse failure).
func redirectHost(redirect string) string {
	u, err := url.Parse(redirect)
	if err != nil {
		return ""
	}
	return u.Host
}

// appendQueryParams adds the given params to a URL's query string, preserving
// any existing query.
func appendQueryParams(rawURL string, params map[string]string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		// Should not happen: redirect_uri was already validated. Fall back to a
		// naive append so the caller still gets a usable URL.
		sep := "?"
		if strings.Contains(rawURL, "?") {
			sep = "&"
		}
		q := url.Values{}
		for k, v := range params {
			q.Set(k, v)
		}
		return rawURL + sep + q.Encode()
	}
	q := u.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// readAndRestoreBody reads the full request body and restores it so a later
// ShouldBindJSON still works. Returns the raw bytes.
func readAndRestoreBody(c *gin.Context) ([]byte, error) {
	if c.Request == nil || c.Request.Body == nil {
		return nil, nil
	}
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil, err
	}
	_ = c.Request.Body.Close()
	c.Request.Body = io.NopCloser(strings.NewReader(string(raw)))
	return raw, nil
}
