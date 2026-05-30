# 分布式 Edge 网关设计草案

状态: 草案 (discussion / 待拆 issue)
适用范围: backend gateway 数据面下沉到多地 edge,中心保留控制面

## 1. 目标与驱动力

把当前单进程 gateway 拆成 control plane(中心) + data plane(多 edge),驱动力:

1. 分散出口 IP: 上游(Anthropic / OpenAI / Gemini / Antigravity)按 IP 限流/风控,需要多地多稳定 IP 出口。
2. 降低延迟 / 就近: edge 离用户更近,减少中心中转。
3. 水平扩展中继: 转换 + 流式中继是带宽/CPU 大头,做成无状态可横向扩。

## 2. 部署形态(已锁定)

- 每个 edge = 一台 VPS + 代理 + 一个稳定出口 IP。
- account -> edge 是硬绑定: 每个上游账号"住"在某台 VPS 上,该账号所有上游流量(含 OAuth refresh)只从这台 VPS 的稳定 IP 出去,IP 永不漂移。
- edge 本身无持久状态、可随时重装/替换。凭据不落 edge 磁盘(见第 5 节)。

## 3. 控制面 / 数据面职责

中心 (control plane):
- 账号注册表 + 凭据(AES 加密持久化,单一信任根)
- 调度器: 账号选择(全局负载/限流/配额/排除/粘性感知)
- 准入控制: 并发槽 / 限流 / 配额预扣 / 计费资格
- 粘性会话存储 (session_hash -> account_id)
- 计费账本 + 用量聚合
- OAuth refresh 单写者(执行见第 5 节)
- lease token 签发 + settle 收口
- 配置

edge (data plane):
- 接收 prompt,请求/响应转换(复用 internal/pkg/{claude,openai,gemini,antigravity} transformer)
- 用中心下发的短时 token 从本机稳定 IP 直连上游
- 流式中继 + 从流计 token
- 把用量回报中心 settle

**edge 资源依赖(刻意做到最小):edge 是纯无状态进程,零基础设施。**

| 资源 | 依赖 | 说明 |
|---|---|---|
| PostgreSQL | 否 | 账号/key/用量全在中心 DB,edge 从不连 |
| Redis | 否 | 并发槽/粘性/配额缓存都在中心,edge 不连 |
| 本地磁盘 | 几乎否 | 仅 enroll 后一个小 state 文件(`edge.json`,可选);lease token 用完即弃不落盘 |
| 中心 sub2api | 是(唯一) | `/edge/v1/{lease,settle,register,heartbeat}` HTTP |
| 上游 provider | 是 | 直连 MiMo/Anthropic/OpenAI(本职) |
| 出口代理 | 是(中心下发) | 出口代理 URL 由中心 enroll 下发(给稳定出口 IP),非 edge 本地选配 |

含义:edge 部署极轻 = 一个二进制 + 一个 enroll token,VPS 拉起即用,无需装 PG/Redis;挂了重装零数据损失(有状态的一切都在中心)。

**edge 侧零手工配置原则:edge 的运行参数全部由中心 enroll 下发**——token-secret(seal 密钥)、mTLS 证书、出口代理 URL、心跳间隔、failover 数、platforms 等,enroll 时一次性从中心取回并持久化;edge 命令行只需一个 enroll token。本地 flag 仅作开发期 override 存在。这也是 §8「edge 无状态可替换」的前提。

## 4. 请求时序

```
client → ingress edge(就近接入)
  → 中心 Lease(apiKeyID, model, sessionHash, requestID)
       中心: 准入(并发/限流/配额预扣) + 调度选账号 + 粘性查询
       返回: {
         accountID, homeEdgeID, upstreamEndpoint,
         leaseToken,            // 该账号上游 access token,mTLS 下发,edge 用完即弃不缓存
         modelMapping,          // requested → upstream 模型名
         tlsFingerprintProfile, proxyURL?,
         rankedCandidates[],    // 备选账号(本地 failover,各带自己的 leaseToken)
         slotHandle             // settle 时归还并发槽
       }
  → 若 ingress edge ≠ homeEdge: 经 mTLS 把请求转给 homeEdge(account 所在 VPS)
       homeEdge: 转换 + 用 leaseToken 从本机稳定 IP 直连上游 + 流式
  → 流经 ingress 回 client
  → 中心 Settle(requestID, accountID, inputTokens, outputTokens, cacheTokens,
                 statusCode, latency, failoverPath[], partial)
       中心: 记用量 + 配额对账(多退少补) + 释放槽 + 回写粘性 + 更新账号负载/限流
```

note: ingress edge 与 homeEdge 同机时少一跳。prompt 体只在 mTLS 可信集群内流转,不落第三方。AI 流式中上游腿是延迟大头,所以"就近"主要优化 ingress 这一腿,收益有限,不为它过度设计。

## 5. 凭据模型(已锁定: A + refresh 走 home edge IP)

- 中心持全量凭据(AES 加密),是单一信任根。
- 每请求 Lease 时,中心把该账号当前有效的上游 access token 经 mTLS 通道下发给 homeEdge(`leaseToken`)。edge 用完即弃,不跨请求缓存、不落盘。
- edge 身份用 mTLS 客户端证书,每台一张,可单独吊销。中心据 `requestID` + edgeID 审计。
- OAuth refresh: 中心是单写者(持 refresh token、决定何时刷),但执行刷新的那次 HTTP 请求经 home edge 的代理发出,使上游看到的来源 IP 与该账号数据面一致(永不漂移)。刷新拿到新 access token 后中心更新持久化并在下次 Lease 下发。

warning(待验证): 必须先确认每个 provider 的 refresh 端点是否校验来源 IP、refresh token 是否轮换。若某 provider 不校验 IP,该账号的 refresh 可退化为中心直接刷以简化;若校验,则强制走 home edge 出口。这条进第 11 节待办。

泄漏面分析:
- 中心被攻破: 全量凭据在中心(本方案的固有代价,靠中心侧加密 + 访问控制 + 审计收敛)。
- 单 edge 被攻破: 仅泄漏该 VPS 在途若干账号的短时 access token + 该 VPS 名下账号(本就绑死在此,无额外扩散);立即吊销该 edge 证书止血。

## 6. 调度变化

`SelectAccountWithLoadAwareness` 扩展:
- 账号选择仍是全局决策(负载/限流/配额/排除/粘性),但选中账号即确定 homeEdge(其稳定 IP)。
- account -> edge 是硬绑定,不是打分权重 —— 由部署结构决定,无需把 IP 亲和塞进 SchedulerScoreWeights。
- 新增 edge 健康度作为可调度前提: homeEdge 不健康时,其名下账号视为暂不可调度(见第 8 节故障域)。

## 7. 一致性

底座已就绪(并发 concurrencyHelper、计费 billingCacheService、粘性 GetCachedSessionAccountID、配额 DB 写聚合 flusher 均为 Redis/DB 共享态),多 edge 共用同一份准入存储天然可行。需补:

1. 配额防双花: Lease 时按乐观额度预扣,Settle 时按真实 token 对账多退少补。纯靠 Settle 后扣会在高并发下透支。
2. slot 可靠释放: edge 崩溃/客户端断连时,靠 Settle 兜底 + lease TTL 自动回收(现有 wrapReleaseOnDone + sticky TTL 即此模式雏形)。
3. Settle 幂等: requestID 做幂等键,重复 Settle 不重复扣费;edge 中途挂上报 partial=true,中心按部分流对账。

## 8. 故障域与韧性

- edge = 它名下账号的故障域。某 VPS 挂了,其账号暂不可用(不能随手迁到别的 IP,否则上游看到 IP 突变可能要求重登/风控)。这是"稳定 IP"的固有代价。
- 缓解: 账号按 group 分散到多台 edge,避免单台承载某 group 全部账号;edge 健康探测纳入调度前提,自动剔除不可用 edge 的账号。
- edge 无状态: VPS 重装/替换不丢账号(凭据在中心),拉起接入即恢复。

## 9. 账号录入流程变化

- 账号注册/OAuth 授权必须从其 home edge 的 IP 发起,使上游从第一次起就看到一致 IP。录入流程要选定 home edge 并经其代理完成授权。
- 录入后凭据回中心加密持久化;refresh 按第 5 节经 home edge IP 执行。

## 10. 渐进迁移(映射现有代码,可独立上线/回滚)

1. 把 `gateway_handler.go:117 Messages()` 的第 5-14 步(准入/调度/选号/转发/计费)抽成中心内部 `lease()` / `settle()` 两函数,纯重构,行为不变,单进程内调用。
2. 给两函数包 mTLS gRPC;现有 gateway handler 变成"第一个 edge 客户端"(in-process loopback),验证协议无回归。
3. 把转换 + 转发 + 流式 + token 计数从 handler 切出成独立 edge 二进制,复用 internal/pkg/{claude,openai,gemini,antigravity} 与 tlsfingerprint / proxyurl。
4. failover_loop.go 的状态机平移到 edge 侧(消费 rankedCandidates 本地 failover);仅候选耗尽才回中心重 lease。
5. edge 部署到多地多 VPS,接入 account -> edge 硬绑定 + 经 home edge IP 的 refresh。
6. 灰度: 单 region 先切 edge,中心 in-process 路径作 fallback。

## 10.5 已实现 (runnable PoC)

第 1 步的契约 + 一个端到端可跑的中心/边缘系统已落地在 `backend/internal/edgegw/`,用内存实现使其无需 Postgres/Redis 即可运行与测试,与生产网关共享同一套 `Lease`/`Settle` 契约。

代码:
- `contract.go` — LeaseRequest / Candidate / LeaseResult / SettleRequest / SettleResult + 错误码(契约,即未来 gRPC/proto 的等价物)。
- `coordinator.go` — Coordinator,组合 5 个小接口(Admission / Billing / Scheduler / StickyStore / UsageSink + TokenMinter)实现 Lease/Settle:准入预扣 → 计费预检 → 调度选号 → 粘性提升 → mint 短时 token;Settle 幂等 + 释放槽 + 配额对账 + 粘性回写。被拒的 Lease 不泄漏 admission 槽。
- `memimpl.go` — 内存实现:MemRegistry(兼 least-load 调度器)、MemAdmission(per-key 并发)、MemSticky、MemUsageSink、HMACMinter(签名短时 token,包住账号真实上游 token)。生产用 Redis + 真 GatewayService/BillingCacheService 背书同一接口。
- `center_server.go` — 中心 HTTP:POST /v1/lease、/v1/settle,带账号 in-flight 计数平衡。
- `edge_relay.go` — 边缘数据面:收 prompt → 调中心 Lease → 用 leased token 从本机直连上游 → 流式回传 → Settle;沿 ranked 候选本地 failover;model mapping 重写。
- 二进制:`cmd/center`、`cmd/edge`、`cmd/mockupstream`。

跑起来(一行,自带 mock 上游,无需真凭据):

```
backend/scripts/edgegw-demo.sh
```

它构建三个二进制、起 mock 上游 + center + edge,然后发一条非流式与一条流式请求穿过 edge。可见:edge 把 `claude-x` 重写为映射后的 `upstream-y`、向上游出示从 lease 解包的真实 token、流式 SSE 正常回传。

测试:`cd backend && go test -tags=unit ./internal/edgegw/`(13 个,含 4 个端到端:全链路 / 流式 / 本地 failover / no-account 透传;coordinator 单测覆盖准入短路、计费拒绝释放槽、no-account 释放槽、粘性提升、Settle 幂等不双扣不双释放、上游错误不绑定粘性)。

尚未做(明确的后续):mTLS(当前 PoC 用明文 HTTP,token envelope + 签名结构已就位)、Redis 背书的 admission、中心 Lease/Settle 背靠真实 `GatewayService`/`BillingCacheService`、refresh 经 home edge 出口的执行、edge 健康探测。

## 10.6 edge = sub2api 的透明中转(对外契约)

edge 对外提供的就是 sub2api 网关本身的能力:client 把 base URL 从中心改成某个 edge,**用同一把 sub2api API key、同一套路径**(`/v1/messages` `/v1/chat/completions` `/v1/responses` `/v1beta/...` `/antigravity/...`)发请求,行为与直连中心一致 —— edge 是 drop-in 的中转节点。

这带出一条必须守死的**双凭据边界**:
- **入站** = client 的 sub2api API key。edge 把它交给中心 `Lease` 做鉴权 + 调度,**不转发给上游**。
- **出站** = 中心 lease 出的 provider token,按账号 `AuthScheme` 应用到上游请求。

edge 转发上游前**剥掉 client 的所有凭据头**(`Authorization` / `x-api-key` / `x-goog-api-key` / `api-key`),换成 leased provider 凭据。client 的 sub2api key 永不到达上游 provider。(实现:`edge_relay.go` 的 `hopByHopHeaders` 剥离 + `AuthScheme.apply` 重新注入;测试见 provider/stress 用例。)

## 10.7 多 Provider 支持(edgegw Provider 抽象)

edge 通过 `Provider`(`provider.go`)吸收各上游协议差异,中心在 `Candidate` 上带 `Platform` + `AuthScheme` 数据驱动:
- **鉴权**:`AuthScheme`{Header/Prefix/QueryParam/Extra} 表达 Anthropic(`x-api-key` + `anthropic-version`)、OpenAI(`Authorization: Bearer`)、Gemini(`?key=`)等;零值默认 Bearer。
- **模型映射落点**:Anthropic/OpenAI 改 body `model`;Gemini 改 URL path `models/<model>:action`;Antigravity 按 path 形态二选一。
- **用量解析**:`UsageParser` 流式(SSE 逐行)/非流式(JSON)统一抽取 —— 兼容 Anthropic(`usage.input/output_tokens`,含 `message`/`response` 嵌套)、OpenAI chat(`prompt/completion_tokens`)、OpenAI responses(`response.usage`)、Gemini(`usageMetadata.promptTokenCount/candidatesTokenCount`);解析不到时回退响应头。

测试覆盖(`go test -tags=unit -race ./internal/edgegw/`,34 个):provider 单测(各平台 prepare + 鉴权 + 各 usage 形态 + SSE 分片写)、egress 代理透传、并发无槽泄漏(100 并发)、稳定性(300 顺序)、网络波动 failover(5xx + 连接 drop + 延迟,全部成功且无泄漏)、全挂干净失败无泄漏。

## 10.8 账号两类:固定 Key vs OAuth-refresh(决定要不要 refresh-via-edge)

上游账号按凭据生命周期分两类,**只有第二类才需要第 5 节的 refresh-via-home-edge 机制**:

**edge = 纯 relay,只多了 lease/settle 这一套,不引入任何新协议/鉴权扩展。** 对 center(sub2api)而言,edge 只是「多了一个执行账号的 Provider」;center↔edge↔上游走 sub2api 已有协议。edge 忠实转发客户端请求,只换三样:鉴权→`Authorization: Bearer <lease 来的 token>`、端点→账号的 upstream base、模型名→按映射改;其余客户端 header(anthropic-version / anthropic-beta / user-agent 等)原样转发。

**统一 Bearer,无 auth-scheme 分叉**:`candidateFromAccount` 用 sub2api 已有公共方法 `GatewayService.GetAccessToken(ctx, account)` 解析出 access_token(apikey 账号它就是那把 key,OAuth 账号是刷新后的 token),edge 一律以 `Bearer` 呈现。MiMo 这类 anthropic-兼容上游 + OpenAI + OAuth 都用 Bearer(MiMo `/anthropic` 与 `/v1` 均实测接受 Bearer)。bedrock/service_account 这类无 bearer token 的不经 edge。edge 对 apikey/OAuth **完全无感**。

**两协议(Anthropic / OpenAI)同时支持,均已端到端实测**。按协议取 base(`AnthropicProtocolProvider`=`GetBaseURL`;`OpenAIProtocolProvider`=`GetOpenAIBaseURL`——对应 sub2api 的两个 gateway service),非「猜 base」。URL 拼接走 `edgegw.JoinUpstreamURL`(镜像 sub2api 的 `buildOpenAIEndpointURL`):base 末段是版本号(`/v1`)时按 OpenAI 约定接相对路径,避免 `/v1/v1` 重复;对 anthropic(`.../anthropic`+`/v1/messages`)与 openai(`.../v1`+`/v1/chat/completions`)都正确。MiMo 同一把 key 配两个账号(anthropic 平台填 `/anthropic`,openai 平台填 `/v1`)即可两协议并用——实测:OpenAI 客户端 `/v1/chat/completions` 与 Anthropic 客户端 `/v1/messages` 经 edge 均正常返回。**未改 sub2api 核心任何文件**。

1. **固定 Key(如 MiMo / 任意静态 api-key 上游)**:`GetAccessToken` 返回静态 key;edge 以 `Bearer` 呈现,端点用账号 `GetBaseURL()`,模型按映射改。**没有 refresh,数据面本就从 edge IP 出**。部署形态已端到端验证(MiMo,real sub2api,uniform Bearer)。

2. **OAuth(Claude/Codex/Gemini OAuth)**:`GetAccessToken` 返回刷新后的 oauth token,edge 同样以 `Bearer` 呈现,端点用平台默认(如 `https://api.anthropic.com`)。Claude Code 指纹头(anthropic-beta/user-agent/x-stainless-*)由**客户端自带**(edge 原样转发),无需中心注入——非 Claude Code 客户端打 OAuth 账号仍走中心网关。可选:中心 refresh 经 home edge 出口(第 5 节)保证 IP 一致。代码已支持(`candidateFromAccount` 统一 Bearer + 平台默认 base),待真实 Claude OAuth 账号 e2e 验证。

结论:固定 Key 类(MiMo)= 已完成、零残留;OAuth 类只差「oauth token → auth/base 映射」这一小步,token 解析本身已被 `GetAccessToken` 统一覆盖。

**架构铁律(本次确立):edge + center 都是 sub2api 之上的「叠加扩展」,只用 sub2api 现有公共 API + 新增文件/包(edgegw、edge_center_handler),绝不改 sub2api 核心文件(gateway_service.go 等)。** 这样 fork 能持续从上游主仓库升级而不冲突。本次已据此回退所有对 `gateway_service.go` 的改动,改用现成的 `GetAccessToken`。

## 11. 剩余工作:复用 sub2api 扩展(不重造)

原则:每项都接到 sub2api 已有能力上,`EdgeCenterHandler` / edge 只做薄扩展;OAuth 流程、加密、定价、并发 Redis 槽一律复用现成的。固定 key 类(MiMo)已完成,以下基本只属于 OAuth-refresh 类 + 两个 class-agnostic 加固。

### P0 — Anthropic OAuth 账号经 edge(已落地)

token 解析统一走 `GatewayService.GetAccessToken`(内部已 dispatch apikey/oauth + 自动 refresh)。`edge_center_handler.go: edgeAuthAndBase` 已补 `tokenType="oauth"`:Anthropic OAuth → `Authorization: Bearer <oauth token>` + base `https://api.anthropic.com`。

**关键洞察:不需要中心注入 Claude Code 指纹头(anthropic-beta/user-agent/x-stainless-*)。** sub2api 只在 `mimicClaudeCode = IsOAuth && !isClaudeCode`(即客户端不是 Claude Code)时才注入;当**客户端本身就是 Claude Code**,它自带这些头,edge 原样转发(edge 只剥凭据头)。所以 OAuth 账号经 edge **支持 Claude Code 客户端**;非 Claude Code 客户端打 OAuth 账号仍需走中心网关。配套:edge 现保留客户端 query string(`?beta=true`)。

- 改动只在扩展文件:`edge_center_handler.go`(oauth 映射)+ `edgegw/edge_relay.go`(转发 query)。`gateway_service.go` 等核心不碰。
- 单测:`edgeAuthAndBase` 各分支(apikey anthropic/openai、oauth anthropic→Bearer+api.anthropic.com、非-anthropic oauth/未知类型 unsupported)+ edge query 转发 e2e。
- **未 e2e 实测**:手头无可用的 Claude OAuth 账号(Docker 里的 Claude OAuth 账号上游凭据已失效);代码完成 + 单测覆盖,待用真实 Claude OAuth 账号 + Claude Code 客户端验证。
- 后续:OpenAI/Gemini/Antigravity OAuth(各自 base + 指纹)、非-Claude-Code 客户端打 OAuth。

### P1 — center 的 refresh 经 home edge 出口(仅 OAuth 类、可选)
- 边界重申:**刷新逻辑在 center,不在 edge**;edge 只是出借自己的稳定 IP 作出口代理。
- 复用:已建的 edge `/internal/egress` 出口原语(`internalKey` 已门控)+ sub2api 的 token provider / `tokenRefreshService`。
- 扩展点(只在 center):给 token provider 的刷新 HTTP 注入"经账号 home edge 的 /internal/egress 发出"的 transport hook,使 refresh 与数据面同 IP(中心仍是单写者)。
- 注:固定 key 类不涉及;是否强制取决于各 provider 是否校验 refresh 来源 IP,可先按需开启。

### P1 — Settle 记用量/计费(复用 gateway 同一条记账)
- 复用:`UsageRecordWorkerPool` + gateway 路径里 `submitUsageRecordTask` 构造的 `usageService` 用量任务(定价/缓存/写聚合 flusher 全现成)。
- 扩展点:Lease 时在 `leasedSlot` 多存 {apiKey, user, account, model};Settle 收到 edge 上报的 tokens 后,构造与 gateway 同样的 `UsageRecordTask` 提交。
- 不重造:定价、计费、配额扣减、写聚合一律复用。

### P2 — 多 center 副本的槽释放(当前单进程内存 map)
- 复用:`ConcurrencyService` 的 Redis 并发槽 key(`AcquireAccountSlot` 管理的那套)。
- 扩展点:`ConcurrencyService` 现无 release-by-key,补 `ReleaseAccountSlot(accountID)`(decrement 同一 Redis key);Settle 按 accountID 释放,不再依赖内存闭包 → center 可多副本。

### P2 — token 信封加密
- 现状:mTLS 已保护传输(已锁决策),edge 必须拿明文 token 调上游,信封加密边际收益低 → 降级可选。
- 若做:复用 `repository/aes_encryptor` + 共享 key。

### 待定(非阻塞)
- ingress != egress 路由:ingress 转发到 home edge vs 直接路由 client 到 home edge。
- lease/settle 升 gRPC + proto 定稿 + 错误码。
- edge 健康探测剔除阈值/窗口(`edgereg` 已有 liveness,接入调度过滤即可)。
- account→edge 硬绑定录入流程(注册时从 home edge IP 完成 OAuth)。
