#!/usr/bin/env bash
# 检查网关模型 ID 与 models.dev 的一致性。
#
# 输入: 网关/账号池的全部模型 ID（stdin 每行一个）
# 输出: 与 models.dev（全量 provider）对比的差异清单
#
# 用法:
#   psql 查询出的模型 ID | bash scripts/check-models-dev.sh
#   PROXY_URL=http://127.0.0.1:7890 bash scripts/check-models-dev.sh < ids.txt
#
# 一致性判定: ID 出现在 models.dev 任意 provider 即视为一致（opencode/zen 系
# 应命中 opencode provider, google 系应命中 google provider）。

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CURL_ARGS=(-s -m 60)
if [ -n "${PROXY_URL:-}" ]; then
  CURL_ARGS+=(-x "${PROXY_URL}")
fi

TMP=$(mktemp)
IDS_TMP=$(mktemp)
trap 'rm -f "${TMP}" "${IDS_TMP}"' EXIT

echo "info: 拉取 https://models.dev/api.json ..." >&2
curl "${CURL_ARGS[@]}" https://models.dev/api.json -o "${TMP}"

cat > "${IDS_TMP}"

python3 - "${TMP}" "${IDS_TMP}" << 'PYEOF'
import json
import sys

data = json.load(open(sys.argv[1]))

# models.dev 全量 ID + 各 ID 所属 provider
id_provider = {}
for prov, pv in data.items():
    for mid in pv.get("models", {}):
        id_provider.setdefault(mid, []).append(prov)

oc_models = set(data.get("opencode", {}).get("models", {}).keys())

with open(sys.argv[2], encoding="utf-8") as f:
    models = [line.strip() for line in f if line.strip()]
if not models:
    sys.exit("usage: 从 stdin 输入模型 ID（每行一个）")

mismatch = []
print(f"{'模型 ID':<36} {'models.dev':<12} {'所属 provider'}")
print("-" * 78)
for mid in sorted(set(models)):
    provs = id_provider.get(mid)
    if provs:
        oc = "opencode" if mid in oc_models else ""
        print(f"{mid:<36} {'有':<12} {', '.join(provs)} {oc}")
    else:
        print(f"{mid:<36} {'无':<12} (自定义 ID)")
        mismatch.append(mid)

print()
if mismatch:
    print(f"error: {len(mismatch)} 个模型 ID 与 models.dev 不一致: {mismatch}")
    sys.exit(1)
print("ok: 全部模型 ID 与 models.dev 一致")
PYEOF
