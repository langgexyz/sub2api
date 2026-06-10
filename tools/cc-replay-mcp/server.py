#!/usr/bin/env python3
"""cc-replay MCP server —— ccdirect 生态历史分析 MCP（第2层）。

把 sub2api 的 admin 只读端点（第1层）封装成 Claude Code 可调的 MCP 工具：
  list_user_sessions / get_session_replay / search_prompts
用于内部改进研发流程（问法复盘/教练）。本层是薄适配，不碰 DB、不重复解码——
全部走第1层 HTTP API。

配置（env）：
  SUB2API_BASE_URL     网关地址，如 http://127.0.0.1:18099 或 https://<域名>
  SUB2API_ADMIN_TOKEN  admin 用户的 JWT（Bearer）。role 由网关按 DB 用户判定。
"""

import os
import sys

import httpx
from mcp.server.fastmcp import FastMCP

API = "/api/v1/admin/cc-sessions"

mcp = FastMCP("cc-replay")
_client: httpx.Client | None = None


def _http() -> httpx.Client:
    """惰性构造 HTTP client（导入时不读 env，便于单测）。"""
    global _client
    if _client is None:
        base = os.environ.get("SUB2API_BASE_URL", "").rstrip("/")
        token = os.environ.get("SUB2API_ADMIN_TOKEN", "")
        if not base or not token:
            raise RuntimeError("需设置 SUB2API_BASE_URL 和 SUB2API_ADMIN_TOKEN")
        _client = httpx.Client(
            base_url=base,
            headers={"Authorization": f"Bearer {token}"},
            timeout=30.0,
        )
    return _client


def _get(path: str, params: dict):
    """调第1层 API，返回 data 字段；非 0 code 或 HTTP 错误抛出。"""
    clean = {k: v for k, v in params.items() if v not in (None, "")}
    r = _http().get(path, params=clean)
    r.raise_for_status()
    body = r.json()
    if body.get("code") != 0:
        raise RuntimeError(f"api error: {body.get('message')}")
    return body.get("data")


def _user_param(user: str) -> dict:
    """user 是纯数字 → user_id；否则 → username。"""
    u = str(user).strip()
    return {"user_id": u} if u.isdigit() else {"username": u}


@mcp.tool()
def list_user_sessions(user: str, frm: str = "", to: str = "", limit: int = 50) -> list:
    """列某用户的全部 Claude Code 会话（按开始时间倒序）。

    user: 用户 id（纯数字）或 username。frm/to: 可选 RFC3339 时间范围。
    返回每会话 session_hash / 起止时间 / 请求数 / 模型 / 首个提问摘要 / 是否截断。
    """
    params = _user_param(user)
    params.update({"from": frm, "to": to, "limit": limit})
    return _get(API, params)


@mcp.tool()
def get_session_replay(session_hash: str, mode: str = "full") -> dict:
    """回放单个会话。

    mode=full: 逐轮 turns（user/assistant/tool_use/tool_result，含截断/压缩标记）。
    mode=prompts: 只返回客户真实提问序列（剥 system-reminder / 自动 recap / 去重多形态）。
    """
    return _get(f"{API}/{session_hash}/replay", {"mode": mode})


@mcp.tool()
def search_prompts(query: str, user: str = "", frm: str = "", to: str = "", limit: int = 50) -> list:
    """跨会话按关键词检索请求正文，返回命中会话与片段（每会话首个命中）。"""
    params = {"q": query, "from": frm, "to": to, "limit": limit}
    if user:
        params.update(_user_param(user))
    return _get(f"{API}/search", params)


def main() -> None:
    if not os.environ.get("SUB2API_BASE_URL") or not os.environ.get("SUB2API_ADMIN_TOKEN"):
        print("error: 需设置 SUB2API_BASE_URL 和 SUB2API_ADMIN_TOKEN", file=sys.stderr)
        sys.exit(1)
    mcp.run()


if __name__ == "__main__":
    main()
