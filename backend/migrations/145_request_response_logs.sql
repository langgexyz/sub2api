-- 请求/响应全量日志（input/output 原文），用于 Prompt 质量分析
-- issue #41
--
-- 关联方式：request_id 与 usage_logs.request_id 一致（同一 ResolveUsageBillingRequestID
-- 解析结果），故 session 级费用/token 聚合走 JOIN usage_logs，无需在 usage_logs 冗余存储。
-- session_hash 来自请求体 metadata.user_id 的 session_id（Claude Code 会话 UUID）。

CREATE TABLE IF NOT EXISTS request_response_logs (
    id              BIGSERIAL PRIMARY KEY,
    request_id      VARCHAR(80)  NOT NULL DEFAULT '',
    session_hash    VARCHAR(128),
    user_id         BIGINT,
    api_key_id      BIGINT,
    model           VARCHAR(100) NOT NULL DEFAULT '',
    endpoint        VARCHAR(128) NOT NULL DEFAULT '',
    status_code     INT          NOT NULL DEFAULT 0,
    stream          BOOLEAN      NOT NULL DEFAULT FALSE,
    request_body    BYTEA,
    response_body   BYTEA,
    request_truncated  BOOLEAN   NOT NULL DEFAULT FALSE,
    response_truncated BOOLEAN   NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_rrl_request_id   ON request_response_logs(request_id);
CREATE INDEX IF NOT EXISTS idx_rrl_session_hash ON request_response_logs(session_hash)
    WHERE session_hash IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_rrl_created_at   ON request_response_logs(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_rrl_user_created ON request_response_logs(user_id, created_at DESC);
