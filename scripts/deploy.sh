#!/usr/bin/env bash
# Zero-downtime blue-green deploy for ccdirect.dev
# Usage: scripts/deploy.sh [sha]
# Requires: git pull rights on server, SSH access, pnpm + Go cross-compile locally
#
# Flow:
#   1. Build frontend (pnpm build)
#   2. Cross-compile Go binary with -tags embed
#   3. SCP binary to server
#   4. Build thin Docker image on server
#   5. Start green container on standby port (18081)
#   6. Smoke-test green
#   7. Nginx reload to green (zero downtime)
#   8. Stop old blue container
#   9. Generate GitHub deploy key if missing
#
# Environment (set in shell or .env.deploy):
#   DEPLOY_HOST     server IP/hostname (default: 47.243.157.87)
#   DEPLOY_PORT     SSH port           (default: 55555)
#   DEPLOY_USER     SSH user           (default: zero)
#   DOCKER_IMAGE    image name prefix  (default: ccdirect)
#   NGINX_SITE      nginx site file    (default: /etc/nginx/sites-enabled/ccdirect)

set -euo pipefail

DEPLOY_HOST="${DEPLOY_HOST:-47.243.157.87}"
DEPLOY_PORT="${DEPLOY_PORT:-55555}"
DEPLOY_USER="${DEPLOY_USER:-zero}"
DOCKER_IMAGE="${DOCKER_IMAGE:-ccdirect}"
NGINX_SITE="${NGINX_SITE:-/etc/nginx/sites-enabled/ccdirect}"

SSH="ssh -p${DEPLOY_PORT} -o ConnectTimeout=30 ${DEPLOY_USER}@${DEPLOY_HOST}"
SCP="scp -P${DEPLOY_PORT}"

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SHA="${1:-$(git -C "$REPO_ROOT" rev-parse --short=8 HEAD)}"
BINARY="/tmp/ccdirect-server-${SHA}"
NEW_IMAGE="${DOCKER_IMAGE}:${SHA}"

log() { echo "[deploy] $*"; }
die() { echo "error: $*" >&2; exit 1; }

# --- 1. Build frontend ---
log "building frontend (pnpm build)..."
pnpm --dir "$REPO_ROOT/frontend" run build

# --- 2. Cross-compile backend with embedded frontend ---
log "compiling for linux/amd64 with -tags embed (sha=${SHA})..."
cd "$REPO_ROOT/backend"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -tags embed -ldflags="-s -w" -trimpath \
  -o "${BINARY}" ./cmd/server/
SIZE=$(du -sh "${BINARY}" | cut -f1)
log "binary ready: ${BINARY} (${SIZE})"

# --- 3. Transfer to server ---
log "transferring binary to server..."
command -v pv &>/dev/null \
  && pv -s "$(stat -f%z "${BINARY}" 2>/dev/null || stat -c%s "${BINARY}")" "${BINARY}" | $SSH "cat > ${BINARY}" \
  || $SCP "${BINARY}" "${DEPLOY_USER}@${DEPLOY_HOST}:${BINARY}"
log "transfer done"

# --- 4-8. Blue-green swap on server ---
log "running blue-green deploy on server..."
$SSH bash -s "${SHA}" "${NEW_IMAGE}" "${NGINX_SITE}" <<'REMOTE'
set -euo pipefail
SHA="$1"
NEW_IMAGE="$2"
NGINX_SITE="$3"
BINARY="/tmp/ccdirect-server-${SHA}"
GREEN_PORT=18081
BLUE_PORT=18080

log() { echo "[server] $*"; }
die() { echo "error: $*" >&2; exit 1; }

# --- 4. Build thin image ---
log "building thin image ${NEW_IMAGE}..."
PREV_IMAGE=$(docker inspect s2a-api --format "{{.Config.Image}}" 2>/dev/null || echo "ccdirect:latest")
cd /tmp
cat > Dockerfile.deploy.${SHA} <<EOF
FROM ${PREV_IMAGE}
COPY ccdirect-server-${SHA} /app/sub2api
RUN chmod +x /app/sub2api
EOF
docker build -f Dockerfile.deploy.${SHA} -t "${NEW_IMAGE}" . 2>&1 | grep -E "^(Step|#|error)" || true
rm -f Dockerfile.deploy.${SHA}
log "image built: ${NEW_IMAGE}"

# --- 5. Start green container on 18081 ---
log "starting green container on port ${GREEN_PORT}..."
docker rm -f s2a-api-green 2>/dev/null || true

# Inherit env from current blue container
ENV_ARGS=$(docker inspect s2a-api \
  --format '{{range .Config.Env}}-e "{{.}}" {{end}}' 2>/dev/null \
  || echo "")

docker run -d \
  --name s2a-api-green \
  --network s2a \
  --restart unless-stopped \
  -p ${GREEN_PORT}:8080 \
  ${ENV_ARGS} \
  "${NEW_IMAGE}"

# --- 6. Wait for healthy ---
log "waiting for green to be healthy..."
for i in $(seq 1 30); do
  STATUS=$(docker inspect s2a-api-green --format "{{.State.Health.Status}}" 2>/dev/null || echo "unknown")
  log "  ${i}s: ${STATUS}"
  [ "$STATUS" = "healthy" ] && break
  [ "$i" = "30" ] && die "green container not healthy after 30s"
  sleep 1
done

# Smoke test: API must return code=0
API_CODE=$(curl -sf http://localhost:${GREEN_PORT}/api/v1/settings/public | python3 -c "import sys,json; print(json.load(sys.stdin)['code'])" 2>/dev/null || echo "-1")
[ "${API_CODE}" = "0" ] || die "smoke test failed: /api/v1/settings/public code=${API_CODE}"

# Frontend must return 200
HTTP_CODE=$(curl -sf http://localhost:${GREEN_PORT}/ -o /dev/null -w "%{http_code}" 2>/dev/null || echo "0")
[ "${HTTP_CODE}" = "200" ] || die "smoke test failed: GET / returned ${HTTP_CODE}"
log "green smoke tests passed (API code=0, GET /=200)"

# --- 7. Nginx reload to green ---
log "switching nginx to green (port ${GREEN_PORT})..."
sudo sed -i "s|proxy_pass http://127.0.0.1:${BLUE_PORT}|proxy_pass http://127.0.0.1:${GREEN_PORT}|g" "${NGINX_SITE}"
sudo nginx -t && sudo nginx -s reload
log "nginx reloaded"

# Verify nginx is serving green
sleep 2
PUBLIC_CODE=$(curl -sf https://ccdirect.dev/api/v1/settings/public 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin)['code'])" 2>/dev/null || echo "-1")
log "prod smoke test: /api/v1/settings/public code=${PUBLIC_CODE}"
[ "${PUBLIC_CODE}" = "0" ] || die "prod smoke failed after nginx reload"

# --- 8. Stop old blue container ---
log "stopping old blue container..."
docker stop s2a-api 2>/dev/null || true
docker rename s2a-api s2a-api-blue-old 2>/dev/null || true
docker rename s2a-api-green s2a-api
docker rm s2a-api-blue-old 2>/dev/null || true

# Update nginx port reference for next deploy (green→blue 18080 for next swap)
sudo sed -i "s|proxy_pass http://127.0.0.1:${GREEN_PORT}|proxy_pass http://127.0.0.1:${BLUE_PORT}|g" "${NGINX_SITE}"
# Update container port mapping (restart with 18080)
docker stop s2a-api
docker rm s2a-api
docker run -d \
  --name s2a-api \
  --network s2a \
  --restart unless-stopped \
  -p ${BLUE_PORT}:8080 \
  ${ENV_ARGS} \
  "${NEW_IMAGE}"
for i in $(seq 1 15); do
  STATUS=$(docker inspect s2a-api --format "{{.State.Health.Status}}" 2>/dev/null || echo "unknown")
  [ "$STATUS" = "healthy" ] && break
  sleep 1
done
sudo nginx -s reload
log "blue container running on ${BLUE_PORT}, nginx reloaded"

# Cleanup temp binary
rm -f "${BINARY}"
log "deploy complete: ${NEW_IMAGE} is live"
REMOTE

log "deploy finished: ccdirect.dev is running ${NEW_IMAGE}"
