#!/usr/bin/env bash
# 同步 models.dev 模型目录到 backend/internal/service/modelsdevdata/。
#
# 数据源: https://models.dev/api.json（全量 provider）。
# 用途: 模型能力元数据（attachment / reasoning / effort 档位）以 models.dev
#       为准，避免本地手维护名单与上游脱节；生成 gzip 内嵌文件。
#
# 用法:
#   bash scripts/sync-models-dev.sh                # 默认走直连
#   PROXY_URL=http://127.0.0.1:7890 bash scripts/sync-models-dev.sh   # 走代理
#
# 生成后: git 提交 gzip 文件即可（数据内嵌进二进制）。

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
OUT="${REPO_ROOT}/backend/internal/service/modelsdevdata/models-dev-all.json.gz"

CURL_ARGS=(-s -m 60)
if [ -n "${PROXY_URL:-}" ]; then
  CURL_ARGS+=(-x "${PROXY_URL}")
fi

TMP=$(mktemp)
trap 'rm -f "${TMP}"' EXIT

echo "info: 拉取 https://models.dev/api.json ..."
curl "${CURL_ARGS[@]}" https://models.dev/api.json -o "${TMP}"

python3 - "${TMP}" "${OUT}" << 'PYEOF'
import gzip
import json
import sys

src, dst = sys.argv[1], sys.argv[2]
data = json.load(open(src))

out = {}
for prov, pv in data.items():
    for mid, m in (pv.get("models") or {}).items():
        entry = out.setdefault(mid, {"attachment": False, "reasoning": False, "efforts": None})
        if m.get("attachment"):
            entry["attachment"] = True
        if m.get("reasoning"):
            entry["reasoning"] = True
        efforts = []
        for opt in (m.get("reasoning_options") or []):
            if opt.get("type") == "effort" and opt.get("values"):
                # "none" 是 opencode 客户端专用关闭值，非 OpenAI 协议档位
                efforts = [v for v in opt["values"] if v != "none"]
                break
        if efforts and (entry["efforts"] is None or len(efforts) > len(entry["efforts"])):
            entry["efforts"] = efforts

raw = json.dumps(out, ensure_ascii=False, separators=(",", ":")).encode()
comp = gzip.compress(raw, 9)
with open(dst, "wb") as f:
    f.write(comp)

print(f"ok: 写出 {len(out)} 个模型的能力数据 ({len(comp)//1024} KB gzip) -> {dst}")
PYEOF
