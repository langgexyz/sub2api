package handler

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"

	"github.com/gin-gonic/gin"
)

// Device-authorization (RFC 8628) endpoints for CLI login (the edge).
//
// Flow:
//  1. edge  -> POST /auth/device/code            (public) -> {device_code, user_code, verification_uri[_complete], expires_in, interval}
//  2. user  -> opens verification_uri in browser (already logged in) and approves
//  3. front -> POST /auth/device/approve         (JWT)    binds the user_code to the logged-in user
//  4. edge  -> POST /auth/device/token (polling)  (public) -> status; on "approved" returns access_token + refresh_token
//
// Tokens are minted with AuthService.GenerateTokenPair — the same path as a
// password login — so the edge ends up holding an ordinary sub2api user session.

// DeviceCodeResponse is the RFC 8628 device-authorization response.
type DeviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// DeviceCode starts a device-authorization grant. Public (no auth): the caller
// is an unauthenticated CLI.
// POST /api/v1/auth/device/code
func (h *AuthHandler) DeviceCode(c *gin.Context) {
	deviceCode, userCode := h.deviceStore.create()
	base := requestBaseURL(c)
	verifyURI := base + "/device"
	resp := DeviceCodeResponse{
		DeviceCode:              deviceCode,
		UserCode:                userCode,
		VerificationURI:         verifyURI,
		VerificationURIComplete: verifyURI + "?user_code=" + userCode,
		ExpiresIn:               int(h.deviceStore.ttl.Seconds()),
		Interval:                int(h.deviceStore.interval.Seconds()),
	}
	response.Success(c, resp)
}

// DeviceTokenRequest is the edge's polling request.
type DeviceTokenRequest struct {
	DeviceCode string `json:"device_code" binding:"required"`
}

// DeviceTokenResponse is the polling result. Status is one of:
// pending | slow_down | approved | denied | expired. Tokens are present only
// when status == "approved".
type DeviceTokenResponse struct {
	Status       string `json:"status"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
}

// DeviceToken is polled by the edge until the grant is approved, then returns a
// freshly minted token pair. Public (no auth): identity comes from the approved
// grant, not the caller.
// POST /api/v1/auth/device/token
func (h *AuthHandler) DeviceToken(c *gin.Context) {
	var req DeviceTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	res := h.deviceStore.poll(req.DeviceCode)
	if res.status != "approved" {
		response.Success(c, DeviceTokenResponse{Status: res.status})
		return
	}

	// Approved: load the user the grant was bound to and mint a token pair, the
	// same way a password login does (respondWithTokenPair).
	user, err := h.userService.GetByID(c.Request.Context(), res.userID)
	if err != nil || user == nil {
		response.Unauthorized(c, "device grant user not found")
		return
	}
	if err := ensureLoginUserActive(user); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	tokenPair, err := h.authService.GenerateTokenPair(c.Request.Context(), user, "")
	if err != nil {
		response.InternalError(c, "Failed to generate token")
		return
	}
	response.Success(c, DeviceTokenResponse{
		Status:       "approved",
		AccessToken:  tokenPair.AccessToken,
		RefreshToken: tokenPair.RefreshToken,
		ExpiresIn:    tokenPair.ExpiresIn,
		TokenType:    "Bearer",
	})
}

// DeviceApproveRequest carries the user_code the human read off the CLI.
type DeviceApproveRequest struct {
	UserCode string `json:"user_code" binding:"required"`
}

// DeviceApprove binds a pending grant to the authenticated user. Requires a
// logged-in session (JWT) — the browser is already authenticated.
// POST /api/v1/auth/device/approve
func (h *AuthHandler) DeviceApprove(c *gin.Context) {
	var req DeviceApproveRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}
	userCode := normalizeUserCode(req.UserCode)
	if !h.deviceStore.approve(userCode, subject.UserID) {
		response.NotFound(c, "Unknown or expired code")
		return
	}
	response.Success(c, gin.H{"approved": true})
}

// DeviceVerify lets the frontend check a user_code is live before showing the
// approve button. Requires login (the approve page is authenticated).
// GET /api/v1/auth/device/verify?user_code=XXXX-XXXX
func (h *AuthHandler) DeviceVerify(c *gin.Context) {
	userCode := normalizeUserCode(c.Query("user_code"))
	response.Success(c, gin.H{"valid": h.deviceStore.userCodeExists(userCode)})
}

// normalizeUserCode upper-cases and trims so "wxyz-1234" / " WXYZ-1234 " match.
func normalizeUserCode(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}

// requestBaseURL reconstructs the public origin (scheme://host) of the center
// from the incoming request, honoring a reverse proxy's X-Forwarded-Proto.
func requestBaseURL(c *gin.Context) string {
	scheme := "http"
	if proto := c.GetHeader("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	} else if c.Request.TLS != nil {
		scheme = "https"
	}
	host := c.Request.Host
	if fwd := c.GetHeader("X-Forwarded-Host"); fwd != "" {
		host = fwd
	}
	return scheme + "://" + host
}
