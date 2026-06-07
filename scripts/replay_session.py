#!/usr/bin/env python3
"""
Session 回放：把 request_response_logs 里捕获的一次会话还原成可读 transcript。

数据来源：每条捕获记录的 request_body 携带「到那一刻为止的完整对话历史」，
因此一个 session 的「最新一条」记录的 request_body 就装着整段对话（除最后一句
AI 回复），其 response_body 是最后一句 AI 回复。两者拼起来 = 100% 完整回放。

用法：
    # 按 Claude Code 会话 UUID 回放（session_hash 来自 metadata.user_id）
    scripts/replay_session.py --session 5fbbce57-a97b-48f1-9c41-f614bda5688c

    # 按单条记录 id 回放（适合无 session_hash 的 OpenCode 等客户端）
    scripts/replay_session.py --id 1

    # 列出最近的 session
    scripts/replay_session.py --list

输出：
    <out>/transcript.md   逐轮对话（user/assistant/thinking/tool/tool_result/image）
    <out>/images/*        请求里内嵌的图片，按出现顺序解码落盘，transcript 内引用

默认从 prod 经 ssh + docker exec psql 取数（只读）。本地有 DB 时用 --dsn 直连。
"""
import argparse
import base64
import json
import os
import re
import subprocess
import sys

PROD_HOST = os.environ.get("S2A_HOST", "47.243.157.87")
PROD_PORT = os.environ.get("S2A_SSH_PORT", "55555")
PROD_USER = os.environ.get("S2A_USER", "zero")
PG_CONTAINER = os.environ.get("S2A_PG_CONTAINER", "s2a-pg")
PG_USER = os.environ.get("S2A_PG_USER", "sub2api")
PG_DB = os.environ.get("S2A_PG_DB", "sub2api")


def run_sql(sql, dsn=None):
    """跑只读 SQL，返回 stdout 文本。dsn 为空则走 prod ssh+docker。"""
    if dsn:
        return subprocess.check_output(["psql", dsn, "-tAc", sql], text=True)
    remote = f"docker exec {PG_CONTAINER} psql -U {PG_USER} -d {PG_DB} -tAc {shquote(sql)}"
    return subprocess.check_output(
        ["ssh", "-p", PROD_PORT, f"{PROD_USER}@{PROD_HOST}", remote], text=True
    )


def shquote(s):
    return "'" + s.replace("'", "'\\''") + "'"


def list_sessions(dsn):
    sql = (
        "SELECT COALESCE(session_hash,'(no-session id='||id||')'), count(*) OVER (PARTITION BY session_hash), "
        "model, max(created_at) OVER (PARTITION BY session_hash) "
        "FROM request_response_logs ORDER BY created_at DESC LIMIT 40;"
    )
    print(run_sql(sql, dsn))


def fetch_latest_row(session, row_id, dsn):
    """取目标记录的 request_body/response_body（base64 编码避免二进制损坏）。"""
    if row_id:
        where = f"id = {int(row_id)}"
        order = "id"
    else:
        # 同一 session 取消息数最多的一条（= 历史最全），并列再取最新。
        where = f"session_hash = {shquote(session)}"
        order = "jsonb_array_length(convert_from(request_body,'UTF8')::jsonb->'messages') DESC, created_at DESC"
    sep = "E'\\x1f'"
    sql = (
        f"SELECT id || {sep} || COALESCE(session_hash,'') || {sep} || model || {sep} || "
        f"encode(request_body,'base64') || {sep} || encode(COALESCE(response_body,''),'base64') "
        f"FROM request_response_logs WHERE {where} ORDER BY {order} LIMIT 1;"
    )
    out = run_sql(sql, dsn).strip()
    if not out:
        sys.exit(f"error: 没找到记录（session={session} id={row_id}）")
    parts = out.split("\x1f")
    if len(parts) != 5:
        sys.exit(f"error: 返回字段数异常（{len(parts)}）")
    rid, sess, model, req_b64, resp_b64 = parts
    req = base64.b64decode(req_b64).decode("utf-8", "replace") if req_b64 else ""
    resp = base64.b64decode(resp_b64).decode("utf-8", "replace") if resp_b64 else ""
    return rid, sess, model, req, resp


def clean_context(text):
    """折叠 Claude Code 自动注入的 system-reminder/CLAUDE.md 噪音，突出真实输入。"""
    return re.sub(r"<system-reminder>.*?</system-reminder>", "[CC 注入 context]", text, flags=re.S)


def render_block(block, img_dir, img_idx):
    """渲染一个 content block，返回 markdown 字符串。img_idx 为可变计数 list。"""
    t = block.get("type")
    if t == "text":
        return clean_context(block.get("text", ""))
    if t == "thinking":
        body = block.get("thinking", "").strip()
        return f"<details><summary>thinking</summary>\n\n{clean_context(body)}\n\n</details>" if body else ""
    if t == "tool_use":
        name = block.get("name", "?")
        inp = json.dumps(block.get("input", {}), ensure_ascii=False)
        if len(inp) > 600:
            inp = inp[:600] + " …(截断)"
        return f"`[TOOL → {name}]` {inp}"
    if t == "tool_result":
        content = block.get("content", "")
        if isinstance(content, list):
            content = " ".join(
                c.get("text", "") for c in content if isinstance(c, dict) and c.get("type") == "text"
            )
        content = str(content)
        if len(content) > 400:
            content = content[:400] + " …(截断)"
        return f"`[tool_result]` {content}"
    if t == "image":
        src = block.get("source", {})
        data = src.get("data", "")
        mt = src.get("media_type", "image/png")
        ext = "jpg" if "jpeg" in mt else mt.split("/")[-1]
        img_idx[0] += 1
        fn = f"img{img_idx[0]}.{ext}"
        if data:
            os.makedirs(img_dir, exist_ok=True)
            with open(os.path.join(img_dir, fn), "wb") as f:
                f.write(base64.b64decode(data))
        return f"`[图片]` ![{fn}](images/{fn}) ({mt}, {len(data)} b64 chars)"
    return f"`[{t}]`"


def render_message(m, img_dir, img_idx):
    role = m.get("role", "?")
    content = m.get("content")
    parts = []
    if isinstance(content, str):
        parts.append(clean_context(content))
    elif isinstance(content, list):
        for blk in content:
            if isinstance(blk, dict):
                s = render_block(blk, img_dir, img_idx)
                if s:
                    parts.append(s)
    return role, "\n\n".join(p for p in parts if p.strip())


def parse_sse_text(sse):
    """从 Anthropic SSE 响应里抽出 assistant 文本。"""
    out = []
    for line in sse.splitlines():
        line = line.strip()
        if not line.startswith("data:"):
            continue
        try:
            obj = json.loads(line[5:].strip())
        except Exception:
            continue
        d = obj.get("delta", {})
        if d.get("type") == "text_delta":
            out.append(d.get("text", ""))
    return "".join(out)


def main():
    ap = argparse.ArgumentParser(description="回放捕获的一次会话")
    ap.add_argument("--session", help="session_hash（Claude Code 会话 UUID）")
    ap.add_argument("--id", help="单条 request_response_logs.id")
    ap.add_argument("--list", action="store_true", help="列出最近 session")
    ap.add_argument("--dsn", help="Postgres DSN（直连本地 DB；缺省走 prod ssh）")
    ap.add_argument("--out", default=".cache/replay", help="输出目录")
    args = ap.parse_args()

    if args.list:
        list_sessions(args.dsn)
        return
    if not args.session and not args.id:
        ap.error("需要 --session 或 --id（或 --list）")

    rid, sess, model, req, resp = fetch_latest_row(args.session, args.id, args.dsn)
    out_dir = os.path.join(args.out, sess or f"id-{rid}")
    img_dir = os.path.join(out_dir, "images")
    os.makedirs(out_dir, exist_ok=True)

    rb = json.loads(req)
    messages = rb.get("messages", [])
    img_idx = [0]

    lines = [
        f"# Session 回放",
        "",
        f"- session_hash: `{sess or '(无)'}`",
        f"- 末条记录 id: `{rid}`",
        f"- model: `{model}`",
        f"- 消息数: {len(messages)}（+ 最后一句 AI 回复）",
        "",
        "---",
        "",
    ]

    # system prompt（首条，常含客户端身份）
    system = rb.get("system")
    if system:
        sys_text = system if isinstance(system, str) else " ".join(
            s.get("text", "") for s in system if isinstance(s, dict)
        )
        sys_text = clean_context(sys_text)
        lines += [
            "## system prompt",
            f"<details><summary>{len(sys_text)} 字符</summary>\n\n```\n{sys_text[:2000]}\n```\n\n</details>",
            "",
        ]

    for i, m in enumerate(messages, 1):
        role, body = render_message(m, img_dir, img_idx)
        if not body.strip():
            continue
        lines += [f"### #{i} · {role}", "", body, ""]

    final = parse_sse_text(resp)
    if final.strip():
        lines += ["### #末 · assistant（最终回复）", "", final, ""]

    transcript = os.path.join(out_dir, "transcript.md")
    with open(transcript, "w") as f:
        f.write("\n".join(lines))

    print(f"ok: 回放已生成 {transcript}")
    print(f"ok: 图片 {img_idx[0]} 张 → {img_dir}")


if __name__ == "__main__":
    main()
