# ccdirect auth contract (loopback + PKCE + device key)

This is the SHARED CONTRACT for the parallel implementation. cchub (backend),
frontend, and ccdirect (edge CLI) must all conform to the exact field names,
URLs, and crypto below. When in doubt, this doc wins.

Naming note: code still uses `edge`/`edgegw` package names in THIS PR (rename to
ccdirect/cchub is a later mechanical sweep). "ccdirect" = the CLI/data plane,
"cchub" = the center/control plane (= sub2api fork).

## Threats & which layer handles each

1. **Phishing** (victim tricked into approving attacker's login) → solved by
   **loopback + PKCE**: the authorization code is delivered to the *initiating
   machine's* `127.0.0.1:<port>`, which an attacker cannot receive; PKCE makes an
   intercepted code useless without the verifier.
2. **Token theft** (someone copies session.json) → solved by **device key**:
   the refresh token is bound to a device Ed25519 public key; refreshing requires
   a signature from the matching private key, which never leaves the machine.

This REPLACES the prior device-flow (RFC 8628) login. Remove: the
`/auth/device/{token,approve}` endpoints, `device_code_store.go`,
`device_auth_handler.go`, the `/device` frontend page, and the edge's
device-login client. Keep the lease/settle/heartbeat data plane unchanged.

## Crypto primitives (exact)

- **PKCE**: `code_verifier` = 43 chars base64url(32 random bytes).
  `code_challenge` = base64url(sha256(code_verifier)). method = `S256`.
- **state**: base64url(16 random bytes), echoed back, checked by edge.
- **device key**: Ed25519. Private key stored at
  `~/.config/sub2api-edge/device_key` (0600). Public key sent as
  base64(raw 32-byte pubkey).
- **authorization_code**: 32 random bytes hex, single-use, TTL 120s.

## Endpoints (cchub)

### GET `/cli/authorize` (frontend SPA route, requiresAuth)
Query params the edge puts in the browser URL:
`response_type=code` · `code_challenge` · `code_challenge_method=S256` ·
`redirect_uri` (must be `http://127.0.0.1:<port>/callback` or
`http://localhost:<port>/callback`) · `state` · `device_pubkey` ·
`name` (optional human label of the edge, e.g. hostname).
Page shows "Authorize ccdirect on this machine?" + the requesting redirect host
+ Authorize / Cancel. If not logged in, the normal guard sends to
`/login?redirect=<this full path>` and back. **Carry the whole query through
login intact** (URL-encode the redirect param — this is the bug from before).

### POST `/api/v1/auth/cli/authorize` (JWT required)
Body: `{response_type:"code", code_challenge, code_challenge_method:"S256",
redirect_uri, state, device_pubkey, name?}`.
Backend MUST:
- reject if `redirect_uri` host is not `127.0.0.1`/`localhost` (loopback only).
- reject if `code_challenge_method != "S256"` or `code_challenge` empty.
- generate `authorization_code`, store grant
  `{code → userID, code_challenge, device_pubkey, redirect_uri, exp: now+120s,
  consumed:false}` in an in-memory TTL store (mirror the prior device store /
  edgereg pattern; ONLY this authenticated path writes to it).
Return: `{ "redirect_to": "<redirect_uri>?code=<authorization_code>&state=<state>" }`.
Frontend then does `window.location.href = redirect_to`.

### POST `/api/v1/auth/cli/token` (public)
Body: `{ grant_type:"authorization_code", code, code_verifier, redirect_uri }`.
Backend MUST:
- look up the grant by `code`; reject if missing/expired/consumed; mark consumed.
- verify `base64url(sha256(code_verifier)) == grant.code_challenge`.
- verify `redirect_uri == grant.redirect_uri`.
- mint tokens via `AuthService.GenerateTokenPair(ctx, user, "")` (same as login).
- **bind**: record `refresh_token → device_pubkey` (see binding below).
Return: `{ access_token, refresh_token, expires_in, token_type:"Bearer",
device_bound:true }`.

### POST `/api/v1/auth/refresh` (existing) — add device-signature requirement IF bound
If the presented refresh token is device-bound, the request MUST carry a valid
device signature (headers below) over the refresh body; cchub verifies against
the bound pubkey before rotating. Unbound refresh tokens (web app) behave as
today. Rotation keeps the same device binding.

## Device signature headers (edge → cchub, on bound calls)

Sent on `/api/v1/auth/refresh` (and reserved for future lease signing):
- `X-Ccdirect-Timestamp: <unix seconds>`
- `X-Ccdirect-Signature: base64( ed25519_sign( canonical ) )`

Canonical string (exact, `\n`-joined, no trailing newline):
```
<HTTP_METHOD>
<REQUEST_PATH>            // e.g. /api/v1/auth/refresh, no query
<X-Ccdirect-Timestamp>
<hex(sha256(raw_request_body))>
```
cchub verifies: timestamp within ±120s; signature valid for the pubkey bound to
the refresh token. The pubkey is looked up server-side from the binding (NOT
taken from a request header — never trust a client-supplied pubkey on a bound
call).

## Binding storage (cchub)

Store `refresh_token_id → device_pubkey` so it survives across rotation and (for
MVP) process restart is acceptable to lose (edge re-logs-in). Simplest MVP:
piggyback on the existing refresh-token cache/record — add a `device_pubkey`
field. If that's heavy, a dedicated in-memory map keyed by a hash of the refresh
token is acceptable for MVP (document the limitation: lost on cchub restart →
edge must re-login). Pick the lighter one; note the choice.

## Edge (ccdirect) login flow

`ccdirect` (no saved session) on start, or `/login`:
1. load-or-create device Ed25519 key (`device_key`, 0600).
2. gen PKCE verifier+challenge, state.
3. start loopback `http://127.0.0.1:0` (OS-assigned port), 1 handler `/callback`.
4. open browser to `<center-web>/cli/authorize?...` (params above). Print the URL
   too (headless fallback).
5. block until `/callback?code&state` hits the loopback server; check state.
6. `POST /api/v1/auth/cli/token {code, code_verifier, redirect_uri}` → tokens.
7. save session (owner_access+owner_refresh) as today; keep device key.
8. shut down loopback server. Print `logged in as <email>`.

`<center-web>` = the center base with `/edge` stripped (the sub2api web origin).
The edge already has `CCDIRECT_CCHUB_URL` ending in `/edge`; reuse
`authBaseFromCenter`.

Token refresh (existing `refreshOwner`): add the device signature headers; on
success persist rotated pair (already wired via SetOnRefresh).

## What stays the same
- lease/settle/heartbeat, seal token, `/edge/v1/config`, owner JWT presentation
  to `/edge/v1/*` — unchanged. (config still fetched with owner JWT after login.)
- session.json format (owner_access + owner_refresh). NEW sibling file
  `device_key` (raw private key, 0600).
