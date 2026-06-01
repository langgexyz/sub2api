#!/usr/bin/env bash
# edgegw-demo.sh — boot the distributed edge end to end and send a request
# through it: client -> edge -> center(lease) -> edge -> mock upstream -> stream
# -> center(settle). See docs/tech/distributed-edge.md.
#
# Usage: backend/scripts/edgegw-demo.sh
set -euo pipefail

cd "$(dirname "$0")/.."   # backend/

OUT=bin/edgegw-demo
mkdir -p "$OUT"

CENTER_ADDR=127.0.0.1:9000
CCDIRECT_ADDR=127.0.0.1:8088
MOCK_ADDR=127.0.0.1:9100

echo "info: building binaries"
go build -o "$OUT/center" ./cmd/cchub
go build -o "$OUT/edge" ./cmd/ccdirect
go build -o "$OUT/mockupstream" ./cmd/mockupstream

# Account registry: one account whose upstream points at the mock, with a model
# mapping so the demo shows the edge rewriting claude-x -> upstream-y.
cat > "$OUT/accounts.json" <<JSON
[
  {
    "id": "acc-1",
    "platform": "anthropic",
    "home_edge_id": "edge-local",
    "upstream_base_url": "http://${MOCK_ADDR}",
    "upstream_token": "real-upstream-token-1",
    "model_mapping": { "claude-x": "upstream-y" }
  }
]
JSON

pids=()
cleanup() {
  echo "info: shutting down"
  for pid in "${pids[@]:-}"; do kill "$pid" 2>/dev/null || true; done
}
trap cleanup EXIT

"$OUT/mockupstream" -addr "$MOCK_ADDR" & pids+=($!)
"$OUT/center" -addr "$CENTER_ADDR" -accounts "$OUT/accounts.json" -max-per-key 4 & pids+=($!)
"$OUT/edge" -addr "$CCDIRECT_ADDR" -center "http://${CENTER_ADDR}" -edge-id edge-local & pids+=($!)

# Wait for health.
for url in "http://${CENTER_ADDR}/healthz" "http://${CCDIRECT_ADDR}/healthz"; do
  for _ in $(seq 1 50); do
    if curl -fsS "$url" >/dev/null 2>&1; then break; fi
    sleep 0.1
  done
done

echo
echo "info: === non-streaming request through the edge ==="
curl -sS -X POST "http://${CCDIRECT_ADDR}/v1/messages" \
  -H 'Content-Type: application/json' \
  -H 'x-api-key: key-1' \
  -d '{"model":"claude-x","stream":false,"messages":[{"role":"user","content":"hi"}]}'
echo
echo
echo "info: === streaming request through the edge ==="
curl -sS -N -X POST "http://${CCDIRECT_ADDR}/v1/messages" \
  -H 'Content-Type: application/json' \
  -H 'x-api-key: key-1' \
  -d '{"model":"claude-x","stream":true,"messages":[{"role":"user","content":"hi"}]}'
echo
echo "info: demo complete (note the mock upstream log lines show the edge"
echo "info: presented the unwrapped upstream token and the mapped model upstream-y)"
