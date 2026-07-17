# CC 会话回放 / 历史分析（cc-replay）

数据源是 `request_response_logs`（网关原文捕获，见 `internal/server/middleware/request_response_capture.go`）。目标：内部改进研发流程——按用户回放 Claude Code 会话、复盘问法、跨会话检索。

## 三层结构

1. **归一化纯函数** `internal/service/cc_session_replay.go`
   把同会话的多条原文行归一成对话流。`mode=full` 输出逐轮 turns（user / assistant / tool_use / tool_result，含截断与压缩标记）；`mode=prompts` 只输出用户真实提问序列（剥 `<system-reminder>` 段、自动 recap、多形态去重）。
2. **admin 只读端点** `internal/handler/cc_session_handler.go` + `internal/server/routes/cc_session.go`
   - `GET /api/v1/admin/cc-sessions?user_id|username&from&to&limit` 列会话摘要
   - `GET /api/v1/admin/cc-sessions/:hash/replay?mode=full|prompts` 回放
   - `GET /api/v1/admin/cc-sessions/search?q=&user_id&from&to&limit` 跨会话检索
3. **cc-replay MCP** `tools/cc-replay-mcp/`
   薄封装第 2 层 HTTP API 为 Claude Code 可调工具（`list_user_sessions` / `get_session_replay` / `search_prompts`）。配置 `SUB2API_BASE_URL` + `SUB2API_ADMIN_TOKEN`（admin JWT）。

## 检索的硬边界（prod 实测后定）

原文列是 BYTEA，`convert_from + ILIKE` 没有索引可用，扫描成本 ∝ 行数 × 体积（prod 行均约 0.7MB）。无界检索在 GB 级数据上必超时（15s 被 postgres 取消，端点 500，prod 实测）。因此：

- service 层：不传 `from` 时默认只搜最近 7 天（`ccSearchDefaultWindow`）。
- repository 层：便宜过滤（user/时间）下推内层，内层按时间倒序硬顶 `ccSearchScanCap=500` 行，昂贵解码只作用在裁剪后的行上。
- 更早历史用显式 `from`/`to` 分段查。要全库任意搜需先做「提取 prompt 列 + pg_trgm GIN」（未做，等真实需求）。

## 保留策略（与检索共生的决策）

`ops.cleanup.request_response_log_retention_days`：**本表 0 = 永不清理（默认）**，与其他 retention 键的 0=TRUNCATE 语义不同（映射函数 `requestResponseLogPlanDays`，注释里有 why）。原文日志是回放/历史分析的唯一数据源，按 TTL 删历史必须是显式运维决策。prod 通过 `~/ccdirect.env` 显式声明 0（deploy.sh `--env-file` 挂载）。

磁盘账（2026-07-17）：表 5.6GB / 月增约 5.6GB / 盘余 11GB——约 2026-09 前需在「加盘 / 开 TTL / 归档后删」间拍板。

## 运维备忘

- admin JWT 铸法：服务器 `~/mint_admin_jwt.py`（HS256；`token_version = 0 XOR sha256(lower(email)+"\n"+password_hash) 前 8 字节`，secret 取容器 `/app/data/config.yaml` 的 `jwt.secret`）。
- 容器 env 通道：deploy.sh 不再继承旧容器 env（历史引号累积 bug），覆盖项写服务器 `~/ccdirect.env`。
