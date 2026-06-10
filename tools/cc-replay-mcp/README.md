# cc-replay MCP

ccdirect 生态历史分析 MCP（第2层）。把 sub2api 的 admin 只读端点封装成 Claude Code
可调的工具，用于内部改进研发流程（问法复盘/教练）。

## 工具
- `list_user_sessions(user, frm?, to?, limit?)` 列某用户全部 CC 会话
- `get_session_replay(session_hash, mode=full|prompts)` 回放会话
- `search_prompts(query, user?, frm?, to?, limit?)` 跨会话检索提问

## 配置
- `SUB2API_BASE_URL` 网关地址（如 http://127.0.0.1:18099）
- `SUB2API_ADMIN_TOKEN` admin 用户 JWT（Bearer；role 由网关按 DB 用户判定）

## 运行
    pip install -r requirements.txt
    SUB2API_BASE_URL=... SUB2API_ADMIN_TOKEN=... python server.py

注册到 Claude Code（mcp 配置）后即可调用。数据源 request_response_logs（admin-only）。
