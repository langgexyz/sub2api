# 跨组模型路由（一把 key 跨平台调模型）

status: design / 草案 v1
scope: backend (gateway routing, account scheduling, group admin) + frontend (group 路由表编辑)
issue: #82

> 目标：Anthropic 组的 key 直接发 `grok-4.5` 就能调到 Grok 账号，用户无感知背后换了账号池。
> 手段：**不动 api_key schema、不动协议矩阵、不混装账号**，只把「有效组」的解析提前到协议路由之前。

## 1. 背景与目标

网关目前挂了多个上游平台（anthropic / openai / gemini / antigravity / grok，见 `internal/domain/constants.go:19-25`）。一个用户要同时用 Claude + GPT + Grok，只能签三把 key、绑三个组，客户端侧要手工切换。

目标：

1. 一把 key 能调到跨平台的模型，模型名即路由键（发 `grok-4.5` 就到 Grok）。
2. 组的可用模型列表如实包含跨组模型（Anthropic 组"里"能看到 `grok-4.5`）。
3. 不牺牲账单/份额边界的清晰性 —— 组仍是单平台，账号不混装。
4. 复用已有机制，不做大重构。

非目标：让 api_key 绑多个组（见 3.1 为什么不走这条）。

## 2. 现状：两根轴，一根已解耦，一根没有

### 2.1 入口方言 ⊥ 上游平台 —— 已经解耦（无需改动）

入口方言由**打哪个端点**决定，不由 `group.platform` 决定。`group.platform` 只选上游适配器。四格今天全通：

| 入口方言 | anthropic 组 | openai / grok 组 |
|---|---|---|
| `/v1/messages`（Anthropic 方言） | `Gateway.Messages` 原生直出 | `OpenAIGateway.Messages` 翻译（需 `AllowMessagesDispatch`；grok 免检，见 `handler/openai_gateway_handler.go:106-114`） |
| `/v1/chat/completions`（OpenAI 方言） | `Gateway.ChatCompletions` 翻译 | `OpenAIGateway.ChatCompletions` |

分支代码在 `server/routes/gateway.go:130-136`（messages）与 `:174-180`（chat/completions）。

**结论：协议从来不是墙。** 本设计原样复用这张矩阵，一格不改。

### 2.2 上游平台 ← 组 —— 这才是墙

`group.platform` 除了选适配器，还直接变成选号 SQL 的 WHERE 条件：

```
gateway_scheduling.go:47          platform = group.Platform
  -> scheduler_snapshot_service.go:704   ListSchedulableByGroupIDAndPlatform(groupID, platform)
  -> account_repo.go:1188                platforms: []string{platform}  -> SQL: account.platform IN (...)
```

所以 grok 账号绑进 anthropic 组会在选号阶段被过滤掉，候选集空 → `ErrNoAvailableAccounts`（`gateway_scheduling.go:657` / `:748` / `:1971`）。协议翻译得再好，池子里没有那个号。

唯一的跨平台例外是 anthropic/gemini 组可捎带 `antigravity` 账号（`scheduler_snapshot_service.go:680` 的 platforms 白名单 + `:695` 的 `IsMixedSchedulingEnabled()`），grok 不在白名单里。

### 2.3 根因

`group` 耦合了四个正交职责：

1. 授权边界（`user_allowed_groups`）
2. 调度池（`account_groups`）
3. 协议选择器（`group.platform` → 上游适配器）
4. 平台过滤器（`group.platform` → 选号 WHERE）

3、4 把「上游怎么说话」这个**账号属性**塞进了「用户能用什么」这个**授权概念**里。

## 3. 核心设计

### 3.1 为什么不给 key 绑多个组

`api_key.group_id` 是标量 N:1（`ent/schema/api_key.go:44` + `:128-131` 的 `Unique()` edge）。改成多对多会引入新歧义：同一模型名在两个组都有账号选哪个？账单算谁的？`user_allowed_groups` 和 key 的多组关系谁说了算？

这些问题在「组内单平台 + 跨组路由」里根本不存在 —— key 永远只锚一个源组，账单永远算源组，路由只是把**某几个显式声明的模型**导流到目标组。

### 3.2 地基已存在

`group` 已有 group→group 跳转：

- `ent/schema/group.go:158` `fallback_group_id`、`:162` `fallback_group_id_on_invalid_request`
- `gateway_scheduling.go:849-876` `resolveGatewayGroup()` —— 完整的跳转解析器：走 fallback 链、`visited` map 环检测、返回解析后的 group 和 groupID
- `gateway_scheduling.go:47` 的 `platform = group.Platform` 用的是**解析后**的组

也就是说「组 A 的请求落到组 B 去调度」架构上早已承认，**调度侧这半条路本来就通**。

### 3.3 唯一卡点

`server/routes/gateway.go:319-325`：

```go
func getGroupPlatform(c *gin.Context) string {
	apiKey, ok := middleware.GetAPIKeyFromContext(c)
	if !ok || apiKey.Group == nil {
		return ""
	}
	return apiKey.Group.Platform   // <- 未解析的组
}
```

入口协议路由读的是 **key 绑的组**，而 `resolveGatewayGroup` 的跳转发生在 handler **内部**的选号阶段 —— 时机太晚，管不到协议分支。

现有 fallback 机制没爆过，只因实践中 fallback 组都是同平台。一旦跨平台跳转，就会出现「调度选了 Grok 账号，但入口早已按 anthropic 把请求交给原生 Anthropic 栈」的错配。

**核心改动 = 把「有效组」解析提前到协议路由之前。**

### 3.4 目标链路

```
key A (绑 group1, platform=anthropic)
  └─ POST /v1/messages   model=grok-4.5
       │
       ├─[新增] ResolveEffectiveGroup 中间件
       │    读 body.model=grok-4.5 -> 查 group_model_routes(group1)
       │    命中 -> effective group = group5 (platform=grok)，写入 ctx
       │    未命中 -> effective group = group1（原样）
       │
       ├─ getGroupPlatform 改读 ctx 里的 effective group = "grok"
       │    -> 协议矩阵原样复用 -> OpenAIGateway.Messages
       │
       └─ SelectAccountForModel(groupID=5)
            -> platform = "grok" -> group5 的 Grok 账号
```

组仍单平台（group1=anthropic，group5=grok），账号不混装，账单/份额边界清晰。

## 4. 数据模型

新表 `group_model_routes`：

| 列 | 类型 | 说明 |
|---|---|---|
| `id` | bigint PK | |
| `group_id` | bigint | 源组（key 绑的组） |
| `model_pattern` | varchar(200) | 模型名或通配符，复用 `ResolveMappedModel` 的最长优先匹配语义（`service/account.go:788`） |
| `target_group_id` | bigint | 目标组 |
| `priority` | int | 同 pattern 多条时的顺序，默认 50（对齐 `account_groups.priority` 约定） |
| `enabled` | bool | 灰度/急停开关 |
| `created_at` / `updated_at` | timestamptz | |

索引：`(group_id, enabled)`；唯一约束 `(group_id, model_pattern)`。

**为什么用独立表而非 group 的 JSONB 列**：可索引、可 admin CRUD、可审计、可单条灰度。JSONB 列在多组路由规模上会退化成全表反序列化。

## 5. 解析流程

新增 `ResolveEffectiveGroup` 中间件，挂在 `/v1`、`/v1beta`、`/antigravity/*` 组上，位置在 `RequireGroupAssignment` 之后、协议分支之前。

1. 从 ctx 取 apiKey，无 group 直接放行（未分组 key 的语义不变，见 `gateway_scheduling.go:48-51` 硬编码 anthropic）。
2. 解析 body 的 `model` 字段。**body 必须 buffer 后回填**，否则下游 handler 读不到（`/v1` 组上已有 body 限制中间件，复用其 buffer）。解析失败/无 model 字段 → 放行，走原组。
3. 查 `group_model_routes(group_id=源组, enabled=true)`，按最长优先通配符匹配。未命中 → 放行。
4. 命中 → 调 `resolveGatewayGroup(target_group_id)` 做二次解析（目标组自身可能有 `claude_code_only` fallback 链），**复用其 `visited` 环检测**，把跨组路由和 fallback 链合并成同一个解析器。
5. 把解析结果写入 ctx（新 ctxkey `EffectiveGroup`），同时记录源组 ID 供计费使用。
6. `getGroupPlatform()` 改读 ctx 的 effective group；`SelectAccountForModel` 的 groupID 入参改传 effective group ID。

缓存：路由表随 group 一起进现有 group 缓存，避免每请求查库。变更走 `scheduler_outbox` 的 `full_rebuild` 事件刷新（与账号归组变更同机制）。

## 6. 待拍板决策点

### D1. 计费/配额归属：源组 or 目标组

- **推荐：账单/配额算源组（group1），账号份额/容量限制算目标组（group5）。**
- 理由：授权边界跟着 key 走，用户感知一致（"我用的是我这个组的额度"）；物理资源约束在目标组那边，`CheckAccountShareLimits` 必须按实际出量的号算。
- 风险：源组的定价表里没有 `grok-4.5` 的价怎么办 —— 需确认三定价源（LiteLLM / 渠道 DB / fallback）对跨组模型的解析路径，定价按**模型名**查而非按组查，理论上不受影响，但要实测。

### D2. 冲突优先级：路由表 vs 本组账号

- **推荐：路由表优先，且显式声明即拦截**，不做「本组找不到才跳」的隐式 fallback。
- 理由：隐式兜底让排查变噩梦（同一个模型名有时走本组有时跳组），违反显式决策点原则。

### D3.（已解决）入口方言 × 目标平台矩阵

原以为要逐格补齐，实测四格全通（见 2.1），**原样复用，本期不改**。

## 7. 影响面（blast radius）

grep 出的同 pattern 命中点，逐条定性：

| 位置 | 现状 | 本期是否要改 |
|---|---|---|
| `routes/gateway.go:319-325` `getGroupPlatform` | 读 `apiKey.Group.Platform` | **改** —— 读 effective group |
| `routes/gateway.go:130-136` / `:174-180` 协议分支 | 依赖 `getGroupPlatform` | 不改（自动跟随） |
| `gateway_scheduling.go:41-47` `resolveGatewayGroup` | fallback 链解析 | **改** —— 合并跨组路由，与中间件共用 |
| `service/admin_group.go:80` 模型聚合 | `if acc.Platform != platform { continue }` 按 platform 过滤 | **改** —— 并入路由表目标模型，这是"组里有 grok-4.5"的 API 兑现点 |
| `service/openai_messages_dispatch.go:62-71` `ResolveMessagesDispatchModel` | 判 `g.Platform == PlatformGrok`，Claude 系模型名 → `xai.DefaultModelMapping()["grok"]` | **待验** —— `grok-4.5` 非 Claude 家族名返回空串，下游行为需实测（见 8.3 负向用例） |
| `service/openai_gateway_grok_cache.go:78-87` `isGrokRequestContext` | 判 `apiKey.Group.Platform == PlatformGrok` | **改** —— 跨组后 apiKey.Group 仍是 group1，会漏掉 prompt-cache 身份注入 |
| `middleware.go:137-152` `RequireGroupAssignment` | 只校验 key 有没有绑组 | 不改 |
| `admin_account.go:895` `checkMixedChannelRisk` | `getAccountPlatform`（`:977-986`）不认 grok，返回空串 skip | **改** —— 补全 platform 识别（本设计不混装账号，但这个静默 skip 本身是 bug） |
| `scheduler_snapshot_service.go:679-709` | 按 (groupID, platform) 分桶 | 不改（跨组后传的就是目标组 ID，天然命中目标组的桶） |

**关键：`isGrokRequestContext` 这处最容易漏。** 它从 `apiKey.Group` 读而非 effective group，跨组路由后会静默不注入 grok 的租户隔离 `prompt_cache_key`，表现是缓存串租户 —— 不报错，只是错。

## 8. 测试计划（实施前锁定，不事后现编）

### 8.1 验收用例（D 决策每分支一条）

| # | 输入 | 最终状态断言 |
|---|---|---|
| A1 | key A(group1/anthropic) POST `/v1/messages` model=`grok-4.5` | 200；`usage_logs` 该 request_id 的 account 属 group5 且 `platform=grok`；响应内容来自 Grok |
| A2 | key A POST `/v1/chat/completions` model=`grok-4.5` | 同 A1（OpenAI 方言那格） |
| A3 | key A POST `/v1/messages` model=`claude-opus-4-5` | 200；账号仍属 group1/anthropic（未命中路由表 → 不跳组） |
| A4 | D1：A1 成功后 | 计费记在 group1 的配额；group5 的账号份额（`CheckAccountShareLimits`）按实际出量扣 |
| A5 | D2：group1 同时有支持 `grok-4.5` 的本组账号 + 路由表条目 | 走路由表（跳 group5），本组账号不被选中 |
| A6 | `GET /v1/models`（或 admin 组详情）for group1 | 返回列表含 `grok-4.5` |

### 8.2 分层覆盖

1. 单元：路由表最长优先匹配（含通配符）、环检测、effective group 解析。
2. 服务层：`resolveGatewayGroup` 合并后对既有 fallback 链的 parity（见 8.3 N4）。
3. e2e：真环境打部署后的站，A1-A6 全跑。
4. 回归：blast radius 表里每个"不改"的点各一条（协议四格 + 未分组 key + antigravity 混合调度）。

### 8.3 负向 / edge（最易漏，显式点名）

| # | 场景 | 期望 |
|---|---|---|
| N1 | 路由表指向的目标组被删 / disabled | 明确报错，不静默回落源组 |
| N2 | 环：group1→group5→group1 | `visited` 检测命中，报 `fallback group cycle detected`，不死循环 |
| N3 | body 非法 JSON / 无 model 字段 | 中间件放行走原组，不 500 |
| N4 | 既有 `claude_code_only` fallback 链（非跨组） | 行为与改动前逐字节一致（parity baseline） |
| N5 | `grok-4.5` 经 `ResolveMessagesDispatchModel` 返回空串 | 确认下游是原样透传还是报错；若报错则本期必须修 |
| N6 | 跨组后 `isGrokRequestContext` | `prompt_cache_key` 正确注入，且租户隔离不串（这是"不报错只是错"的那类） |
| N7 | 目标组内无可调度账号 | 报错文案要带"源组 X 经路由表跳到目标组 Y，Y 内 N 个账号均不可调度"，不是裸 `no available accounts` |
| N8 | 路由表 disabled 后 | 立即回落源组行为，无需重启（验缓存刷新） |

## 9. 分期交付

1. **P1**：表 + ent schema + `go generate ./ent` + admin CRUD + 路由表缓存。无行为变更，可独立合。
2. **P2**：`ResolveEffectiveGroup` 中间件 + `getGroupPlatform` 改读 effective group + 解析器合并。加 `group.routing_mode enum('platform','model')` 灰度开关，默认 `platform`（老行为）。
3. **P3**：`admin_group.go:80` 模型聚合并入路由表目标模型 + `isGrokRequestContext` 修正 + `checkMixedChannelRisk` 补 platform 识别 + N7 报错文案。
4. **P4**：e2e 全跑 + prod 灰度（先单组开 `routing_mode=model`）。

## 10. 回滚

纯加表 + 加列 + 代码改动，无不可逆数据足迹：

- 急停：路由表 `enabled=false`（秒级，走缓存刷新）。
- 组级回滚：`routing_mode` 改回 `platform`。
- 代码回滚：`git revert`，新表留着不影响老逻辑。
