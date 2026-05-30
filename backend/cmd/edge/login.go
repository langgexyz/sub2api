package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/edgegw"
)

// Device-authorization (RFC 8628) client for the edge. authBase is the sub2api
// API root (center base with any trailing /edge stripped); centerEdge is the
// edge control-plane base (…/edge) used for GET /v1/config.

type deviceCodeResp struct {
	Data struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	} `json:"data"`
}

type deviceTokenResp struct {
	Data struct {
		Status       string `json:"status"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	} `json:"data"`
}

// loginResult carries everything a successful device login yields.
type loginResult struct {
	access  string
	refresh string
	edgeID  string
	secret  []byte
}

// deviceLogin runs the full device flow: request a code, show it + open the
// browser, poll until approved, then fetch the edge config (seal secret + edge
// id) with the new access token. Blocks until success, error, or ctx done.
func deviceLogin(ctx context.Context, hc *http.Client, authBase, centerEdge string) (loginResult, error) {
	codeResp, err := postJSON[deviceCodeResp](ctx, hc, authBase+"/api/v1/auth/device/code", nil, "")
	if err != nil {
		return loginResult{}, fmt.Errorf("request device code: %w", err)
	}
	d := codeResp.Data
	if d.DeviceCode == "" || d.UserCode == "" {
		return loginResult{}, errors.New("center returned an empty device code")
	}

	verifyURL := d.VerificationURIComplete
	if verifyURL == "" {
		verifyURL = d.VerificationURI
	}
	fmt.Println()
	fmt.Println("  To authorize this edge, open:")
	fmt.Printf("    %s\n", verifyURL)
	fmt.Printf("  and confirm the code:  %s\n", d.UserCode)
	fmt.Println()
	openURL(verifyURL)

	interval := time.Duration(d.Interval) * time.Second
	if interval < time.Second {
		interval = 5 * time.Second
	}

	body := map[string]string{"device_code": d.DeviceCode}
	fmt.Print("  Waiting for approval")
	for {
		select {
		case <-ctx.Done():
			fmt.Println()
			return loginResult{}, ctx.Err()
		case <-time.After(interval):
		}
		tokResp, err := postJSON[deviceTokenResp](ctx, hc, authBase+"/api/v1/auth/device/token", body, "")
		if err != nil {
			fmt.Print(".")
			continue
		}
		switch tokResp.Data.Status {
		case "pending", "slow_down":
			fmt.Print(".")
			continue
		case "approved":
			fmt.Println(" ok")
			access, refresh := tokResp.Data.AccessToken, tokResp.Data.RefreshToken
			cfg, err := fetchConfig(ctx, hc, centerEdge, access)
			if err != nil {
				return loginResult{}, fmt.Errorf("fetch edge config: %w", err)
			}
			return loginResult{access: access, refresh: refresh, edgeID: cfg.EdgeID, secret: []byte(cfg.TokenSecret)}, nil
		case "denied":
			fmt.Println()
			return loginResult{}, errors.New("authorization denied")
		case "expired":
			fmt.Println()
			return loginResult{}, errors.New("the code expired before approval")
		default:
			fmt.Println()
			return loginResult{}, fmt.Errorf("unexpected status %q", tokResp.Data.Status)
		}
	}
}

// fetchConfig calls GET /edge/v1/config with the owner JWT and returns the
// center-issued edge config (seal secret, edge id, platforms, …).
func fetchConfig(ctx context.Context, hc *http.Client, centerEdge, access string) (edgegw.EnrollResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(centerEdge, "/")+"/v1/config", nil)
	if err != nil {
		return edgegw.EnrollResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := hc.Do(req)
	if err != nil {
		return edgegw.EnrollResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return edgegw.EnrollResponse{}, fmt.Errorf("config status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var out edgegw.EnrollResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return edgegw.EnrollResponse{}, err
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
func authBaseFromCenter(centerEdge string) string {
	return strings.TrimSuffix(strings.TrimRight(centerEdge, "/"), "/edge")
}
