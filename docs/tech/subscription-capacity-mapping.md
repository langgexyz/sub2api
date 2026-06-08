# 订阅容量反推与 API Key 均分映射

status: design / 草案
scope: backend (account scheduling, billing, rate limit)

## 1. 背景与目标

网关的上游是 Anthropic 订阅账号（Pro / Max 5x / 10x / 20x），每个账号有「按周 + 按用量 + 占比」的额度限制。目标：

1. 不靠人工配置，动态反推每个订阅账号的真实周容量（API 等价美元）。
2. 把账号自动归档（Pro / 5x / 10x / 20x）。
3. 把一个账号的容量映射成一个「订阅」，N 个 API Key 订进来，按 max-min 公平（均分保底 + 闲置回收）切分。
4. 整个链路自适应：上游调限、账号升降级、Key 增减，分配自动跟随。

## 2. 核心洞察

### 2.1 反推（已在真实数据上验证）

上游每个响应头携带占比信号，网关已在被动采样：

- `anthropic-ratelimit-unified-7d-utilization`（周窗口占比，0~1）
- `anthropic-ratelimit-unified-5h-utilization`（5h 窗口占比，0~1）

反推公式（分母用 cost 不用 token，已验证）：

```
账号窗口容量 = 窗口内消耗成本 / 窗口 utilization
```

prod 实测（账号 12 "009"，oauth）：

| 窗口 | 窗口内成本 | utilization | 反推容量 |
|---|---|---|---|
| 7d | $21.03 | 0.11 | $191/周 |
| 5h | $4.51 | 0.18 | $25 / 5h |

验证点：窗口对齐干净（账号历史就在窗口内，无截断）；成本当分母把 opus/sonnet 混用归一化；5h 与 7d 内部自洽（7d ÷ 5h ≈ 7.6，远小于理论上限，符合「5h 防突发、7d 卡持续」的设计）。

### 2.2 定档（离散阶梯让噪声消失）

档位倍率写在名字里（相对 Pro 是 1 : 5 : 10 : 20）。反推容量按比例就近吸附到阶梯，档位间距 2 倍，±30% 误差仍分类正确。

锚定基线（009 = Pro，$191/周）：

| 档位 | 周容量（API 等价） |
|---|---|
| Pro | $191 |
| Max 5x | $956 |
| Max 10x | $1,912 |
| Max 20x | $3,824 |

注：单账号无法聚类标定，需一个已知档位账号作锚。多账号后可改为全池聚类自标定（簇间比 1:5:10:20 同时定档位 + Pro 基线）。

### 2.3 三层级联 + max-min 公平

```
Account（真相，反推发现）  容量 C：7d / 5h
   │ account_group (多对多 + priority，可多账号入一池, C池 = ΣC)
Group（订阅产品/池顶）  weekly_limit_usd = C池 × 安全系数；subscription_type=定档
   │ group_id（Key 自助订入，N 动态）
API Key（客户切片）  保底 = C池 / N_active；可借至池顶；撞顶返回 429
```

## 3. 现有地基盘点（决定扩展是「接线」而非「新建」）

| 能力 | 现状 | 位置 |
|---|---|---|
| utilization 被动采样 | 已有，每响应写 account.extra | `ratelimit_service.go:1404-1412`（`session_window_utilization` / `passive_usage_7d_utilization`） |
| utilization 消费 | 已有（展示用量进度） | `account_usage_service.go:448, 1259` |
| 订阅层 | 已有 `UserSubscription`（user+group 维度，track 日/周/月用量） | `subscription_service.go` |
| group 池顶限额 | 已有 daily/weekly/monthly_limit_usd + 强制 | `group` schema；`subscription_service.go:840/855 CheckUsageLimits/ValidateAndCheckLimits` |
| group 限额热路径强制 | 已有，auth 中间件调用 | `server/middleware/api_key_auth.go:184` |
| group 订阅类型字段 | 已有 `subscription_type` | `group` schema |
| group fallback（非配额触发） | 已有 `fallback_group_id`（claude_code_only 客户端降级）+ `fallback_group_id_on_invalid_request`（无效请求改道重试，如提示词过长）。**均不按配额耗尽触发**，不能直接复用为「撞顶溢出」 | `gateway_service.go:2211`；`gateway_handler.go:796-828` |
| per-key 限额 | 已有 rate_limit_5h/1d/7d + usage 窗口追踪 | `api_key` schema；`billing_cache_service.go:650-660 evaluateRateLimits` |
| account-group 多对多 | 已有（复合主键 + priority） | `account_group` schema |
| 账号反应式 429 保护 | 已有（读 utilization/exceeded 头标记不可调度） | `ratelimit_service.go:1037-1128` |

结论：订阅、池顶、per-key 限额、账号反应式 429 保护全已就位。真正新增只有两块：账号容量反推/定档，和 per-key 准入从「静态上限」改成「max-min 公平」。注意：现有 fallback 字段均非配额耗尽触发，**撞池顶没有现成溢出机制**，本方案撞顶先直接返回 429（见 M3）。

## 4. 扩展设计（标注 复用 / 接线 / 新增）

### M1 账号容量反推 + 定档（新增）

挂在已消费 utilization 的 `account_usage_service.go`。后台周期任务（或在 utilization 采样后触发）：

1. 取窗口内成本：从 usage_logs 按 account_id 汇总滚动 7d / 5h 的 `total_cost`。
2. 反推 `C_7d = cost_7d / util_7d`，`C_5h = cost_5h / util_5h`（util 低于置信门槛则跳过）。
3. EWMA 平滑：`C ← α·新值 + (1-α)·旧值`。
4. 吸档（log 空间就近）+ 滞回（连续 N 次越界才换档）。
5. 写回 `account.extra`：`inferred_capacity_7d` / `inferred_capacity_5h` / `inferred_tier` / `tier_confidence` / `capacity_updated_at`。

冷启动：无流量时档位标 `unknown`，给保守默认（按 Pro）直到反推出真值。

### M2 容量 → 池顶接线（接线）

后台 reconcile 任务：对每个 group，`weekly_limit_usd = Σ(成员账号 inferred_capacity_7d) × 安全系数`。复用现有 group 限额强制（`ValidateAndCheckLimits`）——它已经是「池顶」，只是限额值从人工配改成反推驱动。`subscription_type` 同步成员账号定档结果。

注：group 只有 daily/weekly/monthly，没有 5h 池顶。5h 靠 per-key 准入（M3）+ 账号反应式 429（已有）兜底。

### M3 max-min 公平准入（核心，改写 `evaluateRateLimits`）

现状（静态 per-key 上限）：

```go
if apiKey.RateLimit7d > 0 && usage7d >= apiKey.RateLimit7d {
    return ErrAPIKeyRateLimit7dExceeded
}
```

改为 max-min 公平（保底 + 回收）：

```
floor_7d = poolCap_7d / N_active
admit if:
    poolUsage_7d < poolCap_7d        // 池子有空 → 快路径，实现"闲置回收"
    OR usage7d < floor_7d            // 没超自己保底 → 永不饿死
reject otherwise → 返回 429       // 池满 且 已超公平份额
```

撞顶处理（本期）：直接返回 429（沿用 `ErrAPIKeyRateLimit7dExceeded` / `ErrAPIKeyRateLimit5hExceeded`），客户端自行重试 / 等窗口重置。**不复用现有 fallback 字段**（它们看客户端类型 / 请求合法性，非配额触发）。配额耗尽溢出留作后续可选项：(a) 新增「池耗尽 → 换备用 group/账号」路径；(b) 一个 group 绑多账号，靠池内多账号分摊消化（account_group 已支持多对多）。

5h 同理跑一套，取严。需要给 `evaluateRateLimits` 额外传入：

- `poolCap`（= group.weekly_limit_usd / 5h 等价，来自 M2）
- `poolUsage`（Σ group 内 key 当前窗口 usage —— 新增聚合，热路径走 Redis 缓存）
- `N_active`（group 内近窗口有请求的 key 数 —— 新增计数，缓存）

per-key 的静态 `rate_limit_7d` 保留为「硬上限」可选项（VIP/防滥用封顶），与公平准入叠加：先过公平准入，再过静态封顶（若设）。

### M4 可观测 + admin（新增）

账号列表展示：反推容量 / 定档 / tier_confidence / 当前 utilization / 池子 key 承载率。先只读观测，验证控制环稳定后再开闭环。

## 5. 数据模型变更（净新增）

account.extra 新增（JSONB，无 schema 迁移）：

```
inferred_capacity_7d   float   反推周容量(USD)
inferred_capacity_5h   float   反推5h容量(USD)
inferred_tier          string  pro|max5x|max10x|max20x|unknown
tier_confidence        float   0~1
capacity_updated_at    string  RFC3339
```

group：复用现有 `subscription_type` / `weekly_limit_usd` / `daily_limit_usd`，无新字段（值改由 M2 驱动）。`fallback_group_id` 不在本方案复用范围（非配额触发）。可选新增 group.extra 标记 `capacity_auto = true`（该 group 池顶由反推托管，禁止人工覆盖）。

api_key：复用现有 usage 窗口字段。新增缓存（非 schema）：group 维度 `poolUsage` / `N_active`。

## 6. 关键算法

反推 + EWMA（M1）：

```
C_raw = cost_window / utilization        (utilization > 置信门槛, 如 0.05)
C = α * C_raw + (1-α) * C_prev           (α 如 0.3)
```

吸档 + 滞回（M1）：

```
ladder = {pro:1, max5x:5, max10x:10, max20x:20} * pro_baseline
tier* = argmin_t | log(C) - log(ladder[t]) |
若 tier* != 当前档: 累计计数++; 连续 >= H(如3) 次才切换, 否则保持
```

max-min 准入（M3，7d 与 5h 各一套）：

```
floor = poolCap / max(N_active, 1)
admit = (poolUsage < poolCap) || (keyUsage < floor)
```

## 7. 动态闭环与稳定性

```
上游调限 → 反推观测C(EWMA) → C变 → 池顶&每Key保底自动重算
Key订/退订 → N_active变 → 保底自动重算
            ↑                                    ↓
            └── fair-use 反馈 ← 分配/用法 ← 准入决策
```

这是自反系统（你的用法会改变被测容量，fair-use 会惩罚突发）。设计要点：

- 负反馈优先：安全系数 < 1 留 headroom，不把池子推到反推估计的 100%。
- 滞回防档位横跳。
- 稳态采样：突发期 utilization 降权或丢弃。
- 5h 是超售/突发最脆维度——多 key 同 5h 同步打可能在任何单 key 撞保底前打爆账号 5h，连累全 group。错峰假设不成立时收紧 5h 安全系数。

## 8. 实施阶段（先只读，后闭环）

1. 阶段一（只读观测）：M1 反推/定档 + M4 展示。跑数周确认控制环稳定（容量估计不震荡、定档不横跳）。**不改任何强制逻辑。**
2. 阶段二（池顶接线）：M2 反推驱动 group.weekly_limit_usd（可先 dry-run 日志对比人工配值）。
3. 阶段三（公平准入）：M3 改写 evaluateRateLimits，灰度单个 group。
4. 阶段四：全量 + 多账号池聚类自标定 + （可选）配额耗尽溢出机制（新增路径或池内多账号分摊）。

## 9. 开放参数 / 待定决策

| 参数 | 候选 | 备注 |
|---|---|---|
| 安全系数 | 0.85 / 0.9 | headroom 给反推误差 + 突发 |
| 「活跃 key」定义 | 全部 / 近 7d / 近 5h 有请求 | 建议后者，否则保底过小 |
| 定档置信门槛 | util_7d >= 0.05 | 低于不定档 |
| EWMA α | 0.3 | 跟踪 vs 平滑权衡 |
| 滞回次数 H | 3 | 防档位横跳 |
| per-key 静态封顶 | 是否叠加 | VIP/防滥用 |

## 10. 风险

- 反推依赖 utilization 头持续可得；上游若改变头语义需适配（现有解析在 `ratelimit_service.go`）。
- 周限非死值（fair-use 动态调），档位当慢变量、跨周确认。
- 冷/低流量账号长期 unknown，无法分配——需保守默认或主动小流量探测。
- poolUsage / N_active 热路径聚合的缓存一致性（复用现有 billing cache 失效机制）。
