#!/usr/bin/env bash
# 同步 models.dev（opencode provider）模型目录到 backend/internal/service/modelsdevdata/。
#
# 数据源: https://models.dev/api.json 的 "opencode" provider（zen 网关官方目录）。
# 用途: 模型能力元数据（attachment / reasoning effort）以 models.dev 为准，
#       避免本地手维护名单与上游脱节。
#
# 用法:
#   bash scripts/sync-models-dev.sh                # 默认走直连
#   PROXY_URL=http://127.0.0.1:7890 bash scripts/sync-models-dev.sh   # 走代理
#
# 生成后: git 提交 resources 文件即可（数据内嵌进二进制）。

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
OUT="${REPO_ROOT}/backend/internal/service/modelsdevdata/models-dev-opencode.json"
KNOWN_OUT="${REPO_ROOT}/backend/internal/service/modelsdevdata/models-dev-known-ids.json"

CURL_ARGS=(-s -m 60)
if [ -n "${PROXY_URL:-}" ]; then
  CURL_ARGS+=(-x "${PROXY_URL}")
fi

TMP=$(mktemp)
trap 'rm -f "${TMP}"' EXIT

echo "info: 拉取 https://models.dev/api.json ..."
curl "${CURL_ARGS[@]}" https://models.dev/api.json -o "${TMP}"

python3 - "${TMP}" "${OUT}" "${KNOWN_OUT}" << 'PYEOF'
import json
import sys

src, dst, known_dst = sys.argv[1], sys.argv[2], sys.argv[3]
data = json.load(open(src))
provider = data.get("opencode")
if provider is None:
    sys.exit("error: models.dev 响应里没有 opencode provider")
models = provider.get("models")
if not models:
    sys.exit("error: opencode provider 没有 models")

out = {"id": provider.get("id"), "name": provider.get("name"), "api": provider.get("api"), "models": models}
with open(dst, "w", encoding="utf-8") as f:
    json.dump(out, f, ensure_ascii=False, indent=1)

# 全量已知 ID 集合（/v1/models 输出过滤用：隐藏不在 models.dev 的自定义 ID）
known = set()
for pv in data.values():
    known.update(pv.get("models", {}).keys())
with open(known_dst, "w", encoding="utf-8") as f:
    json.dump(sorted(known), f)

print(f"ok: 写出 {len(models)} 个 opencode 模型 -> {dst}")
print(f"ok: 写出 {len(known)} 个全量已知 ID -> {known_dst}")
PYEOF
