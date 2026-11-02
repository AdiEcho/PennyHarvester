# PennyHarvester 多场所架构：接入 predict.fun

> 状态：设计提案（未实现）。本文档的每条事实都标注了来源；标注「推测」的条目需要在 testnet 或链上自行验证。

---

## 0. 结论摘要

### 可以做，而且对这套策略反而更划算，但有四个硬前提

1. **predict.fun（下称 PF）是自营 CLOB，不是聚合器。** 它跑在 BNB Chain（chainId 56），抵押物是 **USDT（18 位小数）**，有自己的一套 CTF Exchange 合约与 resolver。它**不路由到 Polymarket，也不能复用 Polymarket 凭据**——签名 domain 的 `verifyingContract` 与 `chainId` 都不同。
   （来源：官方 SDK `Constants.ts` `PROTOCOL_NAME = "predict.fun CTF Exchange"`、`ChainId.BnbMainnet = 56`、`USDT = 0x55d398326f99059fF775485246999027B3197955`）

2. **纯 API 可接，且平台鼓励程序化交易**：官方 TS/Python SDK + 自助开发者后台（2025-09-01 上线）+ 做市积分计划。认证是三层：`x-api-key`（应用级）+ JWT（钱包 EIP-191 签名换取）+ 订单 EIP-712 签名。

3. **官方文档站已下线**：`docs.predict.fun` 与 `dev.predict.fun` 均 DNS 不解析（2026-10-27 安全事件，约 400k USDT 被非授权转出，官方称调查结束前保持离线）。**唯一可核验来源是 SDK 源码 + 实测接口**。这是本方案最大的工程不确定性，直接决定了「必须先跑 testnet + 签名一致性测试」。

4. **本仓库当前是「单场所」结构**：`Engine.provider` 与 `Engine.live` 都是单值字段，`LiveExecutor` 的签名里直接写死了 `conditionID`（Polymarket 的 fee schedule key），`positions`/`orders`/`equity_snapshots` 表**没有 venue 列**。所以接第二个场所不是「再加一个 client」，而是先做场所抽象。

### 一个反直觉但关键的经济学结论

PF 的 taker 费是 `feeRateBps/10000 × min(p, 1−p) × shares`（`feeRateBps = 200`，即 2% 基准），而 maker **完全免费**（另有最高 30% rebate）。换算成「占名义金额的比例」：

| 成交价 p | PF taker 费（bps of notional） | Polymarket taker 费（类别费率 0.05，bps） |
|---:|---:|---:|
| 0.95 | 10.5 | 25 |
| 0.97 | 6.2 | 15 |
| 0.98 | 4.1 | 10 |
| 0.99 | 2.0 | 5 |

因为 `min(p, 1−p)` 在尾盘价位（p→1）趋近于 0，**PF 的费率结构对「0.95–0.99 尾盘」这段策略带是友好的，比 Polymarket 便宜约 2.4 倍**；它贵的是中间价（p≈0.5 时等效 2%），而那正是本策略主动回避的区域。

推论：**PF 上应该做 maker 挂单（0 费 + rebate），而不是像 Polymarket 那样用 FAK 追价**。这意味着执行策略必须按场所可配置，而不是「一套执行逻辑套两个交易所」。

---

## 1. 影响架构的 PF 事实清单

| 维度 | PF 事实 | 对本仓库的影响 |
|---|---|---|
| 链 | BNB Chain 56（+ Blast 遗留，TVL≈0） | 需要独立的 BSC RPC 配置；`wallet.go` 里 Polygon 专用常量不可复用 |
| 抵押 | USDT，**18 位小数** | 现有代码金额全程 `float64`，PM 侧是 6 位 USDC/pUSD；**金额精度必须重做** |
| 行情 API | `https://api.predict.fun`（mainnet，需 `x-api-key`）；`https://api-testnet.predict.fun`（无需 key） | testnet 可零成本先接只读，这是排期上的最大利好 |
| 分页 | cursor 游标，响应信封 `{success, cursor, data[]}` | 与 PM 的 `after_cursor` 语义一致，`market.Provider` 抽象足以容纳 |
| **到期时间** | `/v1/markets` **不带** per-market 到期时间；`endsAt` 在 **category** 上 | **对本系统最致命**：尾部策略的核心输入就是「剩余到期天数」。必须 join `/v1/categories/{slug}` 并缓存；拿不到就排除（fail-closed） |
| 订单簿 | 只报 `indexSet=1` 一侧，NO 侧要自己用 `1−p` 反演 | 报价层必须做 side 映射，写错就是「买到了反向仓位」 |
| 订单类型 | **只有 `LIMIT` 和 `MARKET`**；无 GTC/GTD/FOK/FAK | PM 侧用 FAK；PF 侧 `MARKET + slippageBps + isMinAmountOut` 只是近似，**滑点语义不同 → 回测与实盘会有偏差** |
| 手续费 | `feeRateBps` 每市场下发（实测 200），maker 0 | 现有 `feeFromSchedule` 是 PM 的 `/clob-markets` 专用，必须抽成 per-venue 模型 |
| 多结果 | `isNegRisk` + `NEG_RISK_CTF_EXCHANGE` + `NEG_RISK_ADAPTER` | 现有模型假设二元（payout 1/0）。**v1 只接 `len(outcomes)==2`，其余显式 unsupported** |
| 最小下单 | 约 1 USDT（官方文档快照；第三方评测表写 $5，冲突） | 需 testnet 实测确认；风控常量必须 per-venue |
| 钱包 | 支持 EOA（`signatureType=0`）或 Kernel 智能账户（Privy 创建，私钥可导出） | 与 PM 的 proxy/safe/deposit 体系**完全不相通**，必须独立实现 |
| 交叉引用 | 每个市场带 `polymarketConditionIds: string[]`（实测非空） | **这是做跨场所比价/路由的天然 join key**（语义是否为「同一问题镜像」未见官方说明，标注推测） |
| 结算 | 自建 `resolverAddress` + `oracleQuestionId`；币价盘用 Chainlink on BSC | 结算判定逻辑与 PM 的 UMA 不同源，**跨场所「同一问题」并不完全可替代** |

---

## 2. 目标架构

### 2.1 分层

```
                      ┌─────────────────────────────────────────┐
   frontend           │  venue 元数据驱动（label/货币/能力矩阵）  │
                      └───────────────────┬─────────────────────┘
                                          │ venue 字段贯穿 DTO
┌─────────────────────────────────────────▼─────────────────────────────────────┐
│ api        venues 注册表 · 路由加 venue 维度 · 设置分场所                        │
├───────────────────────────────────────────────────────────────────────────────┤
│ engine     扫描编排（按场所并发）· 策略（场所中立）· 执行策略（per-venue policy）│
│            风控（per-venue + 聚合）· 记账（venue 维度）· 仪表盘（分币种）        │
├──────────────────────────┬────────────────────────────────────────────────────┤
│ venue/polymarket         │ venue/predictfun                                   │
│  gamma.go (Provider)     │  client.go   REST + 限流 + cursor                  │
│  clob_l2.go (L2 HMAC)    │  auth.go     /v1/auth/message → EIP-191 → JWT      │
│  live.go    (EIP-712)    │  order.go    EIP-712 "predict.fun CTF Exchange"    │
│  wallet.go  (CREATE2)    │  market.go   Provider 实现（markets+categories）   │
│  relayer.go              │  executor.go Executor 实现（LIMIT/MARKET）         │
├──────────────────────────┴────────────────────────────────────────────────────┤
│ store      positions/orders/equity_snapshots 加 venue 列（幂等迁移）            │
└───────────────────────────────────────────────────────────────────────────────┘
```

### 2.2 新增 `backend/internal/venue/venue.go`：场所中立词汇

现有问题：`engine.LiveExecutor.Buy(ctx, tokenID, conditionID string, ...)` 把 Polymarket 的「conditionID 用来查 fee schedule」这件事写进了接口。第二个场所的 fee 来自市场对象本身，没有 conditionID 这个概念。

```go
package venue

type ID string

const (
	Polymarket ID = "polymarket"
	PredictFun ID = "predictfun"
)

// Instrument 是场所中立的可交易标的。它是 (venue, marketID, outcomeID) 三元组，
// ConditionID 仅用于结算归集与跨场所比对，不参与下单。
type Instrument struct {
	Venue       ID     `json:"venue"`
	MarketID    string `json:"market_id"`    // PM: Gamma market id；PF: 整数 market id
	OutcomeID   string `json:"outcome_id"`   // PM: clobTokenId/positionId；PF: onChainId (ERC-1155)
	ConditionID string `json:"condition_id"` // 结算条件；PM conditionId / PF conditionId
}

// QuoteCurrency 区分 USDC 与 USDT：两者永远不能相加。
type QuoteCurrency string

const (
	USDC QuoteCurrency = "USDC"
	USDT QuoteCurrency = "USDT"
)

// Capabilities 是能力矩阵：引擎用它决定「这个场所能不能做这件事」，
// 而不是在各处写 if venue == "polymarket"。
type Capabilities struct {
	Venue         ID
	Label         string
	Currency      QuoteCurrency
	QuoteDecimals int // PM: 6, PF: 18

	MinNotional   float64
	OrderTypes    []OrderType // Taker(FAK/MARKET) / Maker(LIMIT)
	MakerFeeFree  bool
	SupportsCancel bool
	SupportsOnChainSettle bool

	// EndDateGranularity 告诉引擎到期时间是否可信：
	// MarketEnd / EventEnd / Unknown。Unknown 的市场不进入尾盘雷达。
	EndDateGranularity EndDateGranularity

	// BinaryOnly 为 true 时，多结果市场被排除。
	BinaryOnly bool
}
```

### 2.3 扩展既有抽象（改动最小的一组）

`market.Provider` 已经是真接口，**只需加两件事**：

```go
type Provider interface {
	Venue() venue.ID                      // 新增
	ListOpen(ctx context.Context, maxDays int) ([]Market, error)
	Get(ctx context.Context, id string) (Market, error)
	Name() string
}
```

`market.Market` 去掉 Polymarket 语义泄漏。**不要删字段**（会破坏 3 个包的测试与前端类型），而是：
- 加 `Venue venue.ID`；
- `URL()` 改成 provider 提供（`Market.URL string` 由 provider 填，删除 `market.go:58` 的 `https://polymarket.com/event/` 硬编码）；
- `TokenID` → 保留为数据库列名，语义升级为「venue 内 outcome id」；
- 新增 `OutcomeCount int` / `Unsupported string`（带原因，供前端灰显而不是静默丢弃）。

`engine.LiveExecutor` 重命名为 `VenueExecutor`（保留 `LiveExecutor` 作为别名，避免测试与 `types.go:695` 引用全部改）：

```go
type VenueExecutor interface {
	Venue() venue.ID
	Capabilities() venue.Capabilities

	Configured() bool
	Signer() string
	Maker() string
	ReadyError() string

	// Buy 在签名后、提交前调用 onSigned——这条「先落库再提交」的不变式必须保留到 PF。
	Buy(ctx context.Context, in venue.Instrument, req BuyRequest, onSigned func(externalID string) error) (Fill, error)

	LookupOrder(ctx context.Context, externalID string, in venue.Instrument) (Fill, bool, error)
	EstimateBuyFee(ctx context.Context, in venue.Instrument, amount, price float64) (float64, error)
	Quote(ctx context.Context, in venue.Instrument, amount float64) (Quote, error)
	Collateral(ctx context.Context) (Cash, error)
}
```

`engine.Engine` 的字段从单值改成注册表：

```go
type Engine struct {
	venues    *venue.Registry // { providers, executors, capabilities }
	...
}

type Registry struct {
	providers map[venue.ID]market.Provider
	executors map[venue.ID]VenueExecutor
}
```

### 2.4 扫描编排：从「全有全无」到「按场所故障隔离」

现状 `engine.go:557 scan()` 一次 `provider.ListOpen` 拿全部市场，一个上游挂了整轮就废。改法：

```
scan(ctx, execute)
  ├─ 并行：每个启用场所各自 crawl（独立 ctx 超时 30s）
  │    ├─ provider.ListOpen(maxDays)
  │    ├─ findOpportunity()  ← 用该场所的 Settings
  │    └─ classify + 本地化
  ├─ 合并去重（key = venue|marketID|outcomeID）
  ├─ 单场所失败 → 记录 venue health（前端可见），不阻塞其他场所
  ├─ 逐账户：reconcileLiveOrders → refreshPositions → autoTrade
  └─ snapshot（按场所/币种分别写）
```

去重 key 必须带 venue，否则 PM 与 PF 的同一个条件会被当成一条机会。

### 2.5 执行策略按场所可配（这才是真正的收益点）

```go
type ExecutionPolicy struct {
	Mode         PolicyMode // Taker | Maker | TakerThenMaker
	MaxPrice     float64    // 该场所的最高买入价
	SlippageBps  int        // PF 的 MARKET 单必需
	PostOnly     bool
}
```

- Polymarket：`Taker` + FAK（现状不变）。
- predict.fun：`Maker`（LIMIT 挂单，0 费 + rebate）或 `Taker`（MARKET + slippageBps + isMinAmountOut）。
- 这条 policy 进 `Settings`，按 venue 存，前端「参数设置」分场所展示。

---

## 3. 数据模型与迁移

### 3.1 SQLite（`backend/internal/store/sqlite.go`）

三张表加 venue 列，全部走现有幂等 `ensureColumn`（`sqlite.go:343`），不重建表：

| 表 | 新增列 | 说明 |
|---|---|---|
| `positions` | `venue TEXT NOT NULL DEFAULT 'polymarket'` | |
| `orders` | `venue TEXT NOT NULL DEFAULT 'polymarket'` | |
| `equity_snapshots` | `venue TEXT NOT NULL DEFAULT 'polymarket'` | 净值曲线不能被 USDC 与 USDT 混算 |
| `positions` | `currency TEXT NOT NULL DEFAULT 'USDC'` | 展示与汇总时防串币 |

索引：`CREATE INDEX ... ON orders(venue, status)`、`positions(venue, account_id, status)`。

**不需要 DDL 迁移的部分**：`settings.payload` 与 `system_settings.payload` 都是 JSON 文本（`sqlite.go:64-73`），设置层扩容可直接在 JSON 里加 `venues` map。

### 3.2 设置结构（`engine/types.go:181`）

```go
// 新结构（旧字段保留，LoadSystemSettings 时从旧字段合成，保证旧库可读）
type SystemSettings struct {
	Proxy    netproxy.Config               `json:"proxy"`
	Venues   map[venue.ID]VenueSettings    `json:"venues"`
	// 兼容层：旧字段继续回显，读写时与 Venues[polymarket] 双向同步
	DataSource DataSourceSettings `json:"data_source"`
	Live       LiveSettings       `json:"live"`
}

type VenueSettings struct {
	Enabled     bool   `json:"enabled"`
	BaseURL     string `json:"base_url"`
	TradeURL    string `json:"trade_url"`
	Testnet     bool   `json:"testnet"`
	APIKey      string `json:"api_key"`       // PF 专用，AES 加密 + ****** 掩码
	PrivateKey  string `json:"private_key"`
	Funder      string `json:"funder"`
	SignatureType int  `json:"signature_type"`
	RPCURL      string `json:"rpc_url"`       // BSC for PF
	Execution   ExecutionPolicy `json:"execution"`
}
```

密文逻辑直接复用 `store/secrets.go` 的 `enc:v1:` 前缀与 `decryptSetting`，掩码规则沿用「前端原样回传 `******` 时后端还原」。

### 3.3 账户模型

`Account` 加 `Venue venue.ID`。「每个实例只允许一个实盘账户」（`engine.go:471`）改为**每个 (venue, mode) 允许一个**。模拟账户的 `FeeBPS`/`SlippageBPS` 也提升为 per-venue。

---

## 4. 前后端改造清单

### 4.1 后端

| 文件 | 改动 |
|---|---|
| `backend/internal/venue/venue.go`（新） | `ID`/`Instrument`/`Capabilities`/`Registry` |
| `backend/internal/market/market.go:11-154` | `Market` 加 `Venue`/`URL`/`OutcomeCount`/`Unsupported`；删除 `:58` 的 polymarket.com 硬编码 |
| `backend/internal/market/gamma.go:92` | `Name()` 保留，新增 `Venue() → Polymarket` |
| `backend/internal/venue/predictfun/client.go`（新） | REST 客户端：`x-api-key`、cursor 分页、`golang.org/x/time/rate` 限流（240 req/min/key）、`netproxy.Manager` 注入（复用现有代理） |
| `backend/internal/venue/predictfun/auth.go`（新） | `/v1/auth/message` → EIP-191 `personal_sign`（go-ethereum `crypto.Sign`）→ `/v1/auth` → JWT 带过期缓存 |
| `backend/internal/venue/predictfun/order.go`（新） | EIP-712：domain `{name:"predict.fun CTF Exchange", version:"1", chainId:56, verifyingContract}`；按 `isNegRisk`/`isYieldBearing` 选 exchange 合约；`apitypes.TypedData` 签名 |
| `backend/internal/venue/predictfun/market.go`（新） | `market.Provider` 实现：markets + categories(缓存 endsAt) join；`indexSet=1` → NO 侧 `1−p` 反演；`len(outcomes)!=2` → `Unsupported="multi-outcome"`；无 endsAt → `Unsupported="no-end-date"` |
| `backend/internal/venue/predictfun/executor.go`（新） | `VenueExecutor` 实现；18 位小数换算用 `big.Rat`，**只在边界转 float**；`MARKET + slippageBps + isMinAmountOut` |
| `backend/internal/engine/engine.go:17-40` | `provider`/`live` → `*venue.Registry` |
| `backend/internal/engine/engine.go:557-673` | `scan` 按场所并发 + 故障隔离 |
| `backend/internal/engine/engine.go:754/907` | `autoTrade`/`PlaceOrder` 按 `instrument.Venue` 选 executor |
| `backend/internal/engine/engine.go:1011-1019` | PM 的 V2 拒单逻辑改为能力矩阵里的 `SupportsInstrument()` 判定 |
| `backend/internal/engine/types.go:491` | `Opportunity` 加 `Venue`/`Currency`/`Unsupported` |
| `backend/internal/engine/types.go:695` | `LiveExecutor` → `VenueExecutor`（别名保留） |
| `backend/internal/api/router.go:24-60` | 所有市场/持仓/订单路由加可选 `?venue=`；新增 `GET /venues`（能力矩阵 + 健康） |
| `backend/internal/api/system.go:30-74` | 请求/响应体加 `venues`；探测端点按场所选择探测 URL（现在写死 `clob.polymarket.com/time`，`system.go:21`） |
| `backend/internal/config/config.go:34-51` | 新增 `PREDICTFUN_API_KEY` / `PREDICTFUN_PRIVATE_KEY` / `BSC_RPC_URL` / `PREDICTFUN_BASE_URL`，默认 testnet 可用 |

### 4.2 前端

| 文件 | 改动 |
|---|---|
| `frontend/src/lib/venue.ts`（新） | `venueMeta: Record<Venue, {label, currency, short, color, docs}>`，替代散落的 `"Polymarket"`/`"CLOB"`/`"pUSD"` 字面量 |
| `frontend/src/types.ts:80-330` | 所有 DTO 加 `venue`/`currency`/`unsupported` |
| `frontend/src/views/opportunities.tsx` | 加场所筛选 Tab（全部/PM/PF）+ 每行 venue chip；`ProtocolBadge`（`opportunities.tsx:95-104`）泛化为「场所 + 协议」，PF 显示 `LIMIT/MARKET`；`Unsupported` 行灰显并给出原因 |
| `frontend/src/views/positions.tsx:55` | 「需在 Polymarket 手动领取」→ 按 venue 取文案（PF 走 redeem 交易） |
| `frontend/src/views/orders.tsx:48-112` | 「CLOB 订单号」→「外部订单号」，列头随 venue 变 |
| `frontend/src/views/dashboard.tsx:160-198` | 「钱包 pUSD」→ 分场所现金卡片（USDC / USDT 两行，永不合并求和） |
| `frontend/src/views/settings.tsx` | 「参数设置」加场所维度：每场所一套 price/liquidity/maxDays/execution policy |
| `frontend/src/views/system.tsx:51-57,153-183,395-641,832-952` | 「系统设置」改成场所 Tab：PM tab（现状）/ PF tab（base_url、testnet 开关、api key、私钥、BSC RPC、签名类型、连接测试） |
| `frontend/src/lib/market-language.ts:87-96` | `parsed.hostname === 'polymarket.com'` 才加 `/zh` 的分支，改成 venue 元数据里的 `localizedUrl` |
| `frontend/src/components/app-shell.tsx:58-215` | 导航与页脚文案去 Polymarket 化 |

### 4.3 跨场所比价（P1，价值最高但风险也最高）

PF 的市场对象带 `polymarketConditionIds`，这是现成的 join key：

```
market_links(venue_a, market_a_id, venue_b, market_b_id, confidence)
```

功能：
1. **只读比价**（先做）：雷达里把「同一问题」的两条机会配对，并排显示两边**费后净收益**，高亮更优一侧。
2. **路由执行**（后做）：`POST /accounts/:id/orders` 允许 `venue=auto`，由后端选费后净收益最高的一侧。
3. **风险提示（必须）**：两个场所的 resolver / oracle 不同源，且结算时间可能不同。**「同一问题」不等价于「同一资产」**，UI 必须显式提示，不能默认合并敞口。

---

## 5. 分阶段路线

| Phase | 内容 | 产出验收 | 工作量 |
|---|---|---|---|
| **P0 抽象层（无新场所）** | venue 包、Provider/Executor 加 `Venue()`、`Instrument` 贯通、三表加 venue 列、前端 venue 元数据、`GET /venues` | `go test -race ./...` 全绿；PM 行为零变化；旧库自动迁移后持仓/订单仍可见 | 3–4 人日 |
| **P1 PF 只读** | `client.go` + `market.go`（testnet，无需 key）：市场/订单簿/分类 endsAt join、unsupported 判定、雷达可展示 PF 机会 | 雷达里 PF 机会数与 PF 官网一致（抽样 10 个人工核对）；到期时间缺失的市场被正确排除 | 4–6 人日 |
| **P2 PF 模拟盘** | paper 账户绑定 venue；per-venue 费率/滑点/最小下单；17→**18 位小数**改造 | 模拟成交、持仓、结算、净值曲线在 PF 上跑通 | 3–4 人日 |
| **P3 签名与 testnet 实盘** | `auth.go` + `order.go`；**签名一致性 harness**：固定订单在本地 Go 与官方 Python/TS SDK 各签名一次，比对 EIP-712 digest | digest 完全一致；testnet 用 MARKET 单成交一笔；`unknown` 状态能被 `GET /v1/orders` 对账 | 5–8 人日（不确定性主要在这里） |
| **P4 mainnet 小资金** | mainnet API key、`setApprovals()`、首单 1–5 USDT、限额全线下调 | 真实成交、真实费率、真实结算闭环 | 2–3 人日 + 观察期 |
| **P5 比价/路由** | `market_links`、费后净收益配对、`venue=auto` 路由 | 比价正确率抽样核对；路由默认关闭，需显式开启 | 4–6 人日 |

**总前置硬门槛（P3 之前必须解决）**：mainnet `x-api-key`（`https://developers.predict.fun` 自助创建）+ BSC 上有 USDT + 一次 `setApprovals()`。

---

## 6. 风险与 fail-closed 原则

| 风险 | 严重度 | 对策 |
|---|---|---|
| **官方文档站下线，接口无契约** | 高 | 在 `venue/predictfun` 里 pin 住 SDK 源码的 commit 与观测到的响应结构；所有解析对未知字段宽容、对**缺失关键字段严格失败**；每个场所独立 feature flag |
| **PF 市场缺 per-market 到期时间** | 高 | 尾盘策略的核心输入。拿不到 `endsAt` → `Unsupported="no-end-date"` → 不进雷达、不下单 |
| **18 位小数 vs 现有 float64 金额** | 高 | 在 PF client 内部用 `big.Rat`/`big.Int` 表示 wei 级金额，只在写库/出 API 时转 float；`Capabilities.QuoteDecimals` 驱动换算，禁止手写 `1e18` |
| **无 FAK，`MARKET + slippageBps` 语义不同** | 中 | 回测与实盘分开口径；PF 侧优先做 maker（0 费），把「成交不确定性」交给策略层而不是撮合层 |
| **多结果 / negRisk 市场** | 中 | v1 只接二元；`isNegRisk` 不等于不能接，但必须先算清 sum(price)≠1 的敞口，否则直接 unsupported |
| **订单簿只报 indexSet=1** | 中 | side 映射写成有测试覆盖的纯函数，`_book_for_side` 式反演（`NO = 1 − p`）单独测 |
| **USDT 与 USDC 混算** | 中 | `equity_snapshots.venue` + `currency` 列；仪表盘分币种展示，**任何跨币种求和都是 bug** |
| **2026-10 安全事件（API key 泄露类）** | 中 | PF 用独立 API key、独立钱包私钥、独立数据库加密密钥；不复用 PM 凭据；开启 IP 白名单（若后台支持） |
| **跨场所「同一问题」不等价** | 中 | 比价功能默认只读；路由执行默认关闭；UI 强制提示 resolver 差异 |

---

## 7. 需要你决定的三件事

1. **优先级**：是先做 P0+P1（纯只读、零资金、1–2 周内能看到 PF 机会并和 PM 比价），还是直接冲 P3（testnet 实盘签名）？
   - 建议先 P0+P1：文档站下线意味着 P3 的不确定性最高，不该放在最前面。
2. **PF 上的执行策略选 maker 还是 taker**：maker（0 费 + rebate，但可能不成交）会改变策略性格，从「扫尾盘」变成「挂尾盘」。这决定 `ExecutionPolicy` 的默认值。
3. **跨场所比价要不要做**：PF 的 `polymarketConditionIds` 让这件事技术上很便宜，但「同一问题不等价」的解释成本在 UI 和合规上不低。建议只做只读比价，不做自动路由。

---

## 附录 A：PF 关键接口（实测）

```
GET  https://api-testnet.predict.fun/v1/markets?limit=&cursor=      # 无需 key
GET  https://api.predict.fun/v1/markets                            # 401 without x-api-key
GET  /v1/markets/{id}
GET  /v1/markets/{id}/orderbook                                    # 只含 indexSet=1 一侧
GET  /v1/categories/{slug}                                         # 含 endsAt 与 markets[]
GET  /v1/auth/message?address=0x...                                # 取待签消息
POST /v1/auth            {signer, message, signature}              # → JWT
GET  /v1/orders                                                    # 需 JWT
POST /v1/orders          {data:{order:{...}, strategy, pricePerShare, slippageBps, isMinAmountOut}}
```

市场对象关键字段：`id` / `question` / `categorySlug` / `tradingStatus` / `status` / `isNegRisk` / `isYieldBearing` / `feeRateBps`(200) / `makerFeeBps`(0) / `decimalPrecision` / `conditionId` / `resolverAddress` / `oracleQuestionId` / `outcomes[].name|indexSet|onChainId|bestBid|bestAsk` / `polymarketConditionIds[]` / `marketVariant`。

## 附录 B：PF 合约地址（BSC mainnet，来源官方 SDK `Constants.ts`）

| 合约 | 地址 |
|---|---|
| CTF_EXCHANGE | `0x8BC070BEdAB741406F4B1Eb65A72bee27894B689` |
| NEG_RISK_CTF_EXCHANGE | `0x365fb81bd4A24D6303cd2F19c349dE6894D8d58A` |
| NEG_RISK_ADAPTER | `0xc3Cf7c252f65E0d8D88537dF96569AE94a7F1A6E` |
| CONDITIONAL_TOKENS | `0x22DA1810B194ca018378464a58f6Ac2B10C9d244` |
| YIELD_BEARING_CTF_EXCHANGE | `0x6bEb5a40C032AFc305961162d8204CDA16DECFa5` |
| USDT | `0x55d398326f99059fF775485246999027B3197955` |

## 附录 C：PF EIP-712 签名域（来源官方 SDK）

```
domain      = { name: "predict.fun CTF Exchange", version: "1", chainId: 56, verifyingContract }
order fields= [salt, maker, signer, taker, tokenId, makerAmount, takerAmount,
               expiration, nonce, feeRateBps, side, signatureType]
signatureType: EOA=0, POLY_PROXY=1, POLY_GNOSIS_SAFE=2   (支持 EIP-1271)
```

`tokenId` 是 `outcomes[].onChainId`（77 位 ERC-1155 position id），**不是** `conditionId`，也不是整数 `market id`——这四层 id 混用是接入时最容易犯的错误。
