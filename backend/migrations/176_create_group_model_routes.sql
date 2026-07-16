-- Cross-group model routing (issue #82, docs/tech/cross-group-model-routing.md)
-- Lets a group delegate specific model patterns to another group, so one api_key can
-- reach models served by accounts on a different upstream platform (e.g. an Anthropic
-- group serving grok-4.5 from a Grok group).
--
-- This is the "pick a group" level. It is orthogonal to groups.model_routing
-- (added by migration 040/041), which is the "pick an account inside the group" level.
-- Cross-group routing runs first; the target group's model_routing then applies.
--
-- No FK to groups(id) on purpose: groups is a soft-delete table (deleted_at), so a hard
-- FK would still permit references to soft-deleted rows and buys nothing. Dangling /
-- disabled targets are rejected explicitly in the application layer instead of silently
-- falling back to the source group.

CREATE TABLE IF NOT EXISTS group_model_routes (
    id BIGSERIAL PRIMARY KEY,
    group_id BIGINT NOT NULL,
    model_pattern VARCHAR(200) NOT NULL,
    target_group_id BIGINT NOT NULL,
    priority INTEGER NOT NULL DEFAULT 50,
    enabled BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Hot path: resolve the effective group for (source group, model) on every request.
CREATE INDEX IF NOT EXISTS idx_group_model_routes_group_enabled ON group_model_routes (group_id, enabled);

-- One rule per (group, pattern): keeps the resolver deterministic and makes admin edits idempotent.
CREATE UNIQUE INDEX IF NOT EXISTS idx_group_model_routes_group_pattern ON group_model_routes (group_id, model_pattern);
