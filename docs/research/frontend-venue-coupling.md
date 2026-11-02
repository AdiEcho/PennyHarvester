# 前端多场所耦合审计（Polymarket → +predict.fun）

| 项目 | 内容 |
|---|---|
| 范围 | `frontend/`（React 19 + Vite 6 + TS 5.7），只读审计，未修改任何代码 |
| 目标 | 为接入 predict.fun（BNB Chain、USDT 18 位小数、LIMIT/MARKET、maker rebate、无 FAK）列出全部 Polymarket 假设与需要 venue 维度的位置 |
| 方法 | 逐文件通读 `frontend/src` 全部 34 个 ts/tsx 文件（含 188 行 `index.css`）与 `frontend/index.html`、`vite.config.ts`、`tailwind.config.js`（前 80 行：颜色/token）；对后端只读核对 `backend/internal/api/router.go`、`backend/internal/api/system.go`、`backend/internal/engine/types.go` 以确认 DTO 契约 |
| 关联文档 | `docs/multi-venue-architecture.md`（设计提案）。其 §4.2 已给出一张粗略的前端改造表；本文是它的行级展开，并补充了该表遗漏的 6 个问题（见 §6） |
| 状态 | 提案，未实现。所有行号基于当前 HEAD |

**约定**：`file:line` 均指当前仓库状态；引用 UI 字符串保持原文（中文）。`+` 表示新增字段，均建议可选（`?`）以便前端先于后端上线。

---

## 0. 摘要

前端**完全没有** venue 概念：`grep -rn venue frontend/src` 为 0 命中。唯一存在的作用域维度是账户模式 `Account.mode`（`types.ts:1,6`）与 `accountId`（`account-context.tsx:59`）。整个前端假设：

- 只有一个现金余额、一种货币（`$`）、一条净值曲线（`dashboard.tsx:89,156`）；
- 一套全局参数（`Settings` 全标量，`types.ts:213-232`）；
- 一套实盘凭据（`LiveSettings`/`SystemRuntime` 单值，`types.ts:247-254,313-323`）；
- 「协议」只有 Polymarket 的 v1/v2 一个维度（`types.ts:103-104`、`opportunities.tsx:150-160`）；
- 「订单类型」只有 `buy|sell|settle`（`types.ts:195`），FAK/FOK 完全隐式。

真正的结构性阻塞（不是文案问题）只有 5 处：

1. **`Settings` 是扁平标量 + 单次 `PUT /settings`**（`settings.tsx:37-135,429-456`）——per-venue 参数必须先把 schema/draft/validate 抽出来，否则两套参数会挤在同一个 `Draft` 类型里。
2. **`Metrics` 无 venue、无 currency**（`types.ts:34-53`）——单张现金卡（`dashboard.tsx:154-162`）无处安放第二种货币。
3. **`SystemSettings.data_source` / `live` 是单值对**（`types.ts:242-254,331-339`）——系统设置页 1144 行里，`EndpointSection` 与 `LiveTradingSection` 都是单场所形状。
4. **`usePolling` 在 fetcher 身份变化时清空数据**（`usePolling.ts:22-28`）——venue 筛选 Tab 一旦进入 fetcher 依赖，每次切换 Tab 都会白屏重载（`isLoading` 变 true），这是最容易踩的实现陷阱。
5. **雷达用 `token_id` 单键做 Map key**（`opportunities.tsx:64-85,208,264-276`）——两个场所的 token/onChainId 都是十进制 ERC-1155 id 字符串，键空间重叠会产生「A 场所的持仓标记出现在 B 场所行情上」这类**静默错数据**。

最便宜的三个改动（1–2 天量级）：

- `lib/venue.ts`（venue 元数据）+ `VenueBadge`/`VenueFilter` 两个小组件，替换散落的 `Polymarket`/`CLOB`/`pUSD` 字面量（§3.2、§3.4）；
- 雷达/持仓/订单的 **client-side venue 筛选**（`opportunities.tsx:247-256`、`positions.tsx:54`、`orders.tsx:74`）；
- `formatCurrency(value, currency?)` 与 `formatPrice(value, venue?)` 加可选参数，默认行为不变（`format.ts:17,52`）。

看起来便宜、实际不便宜的：参数设置（§3.5）与系统设置（§3.6）的 venue Tab——它们的成本不在 UI，而在**逐字段决定「共享还是按场所」**以及**保存语义**（部分保存 vs 整体保存）。

---

## 1. 现状：状态如何流动（后续所有结论的事实基础）

- **启动链**：`main.tsx:10-14` → `ThemeProvider` → `ToastProvider` → `App`（`App.tsx:20-31`）→ `AuthGate`（`auth-gate.tsx:12-49`，仅调 `api.authStatus`）→ `AccountProvider` → `AppShell` → 6 个视图按 `view` 切换（`App.tsx:24-29`）。
- **账户上下文**：`AccountProvider` 每 30s 轮询 `/accounts`（`account-context.tsx:22`），选中 id 存 `localStorage['pennyharvester.account']`（`:6,26-27,42,48`），失效时回落到 `is_default` 或第一个（`:36-43`）。`accountId` 是唯一被下传给各视图的「作用域」维度（`:59`）。
- **轮询**：`usePolling` 默认 20s（`usePolling.ts:10`），`document.hidden` 时跳过并暂停定时器（`:40,67-72`），出错保留上一轮数据（`:54-57`，配合 `InlineError` 的「当前显示的是上一轮数据」文案，`page-header.tsx:23-34`）。
- **fetcher 身份语义**：`usePolling.ts:22-28` —— `previousFetcher.current !== fetcher` 时清空 `data`/`error`。因为视图里 fetcher 都是 `useCallback([accountId, ...])`（`dashboard.tsx:183-184`、`positions.tsx:54`、`orders.tsx:74-77`、`opportunities.tsx:214,218`），**任何筛选维度进入依赖数组都会触发一次数据清空**。
- **各视图轮询点**：总览 2 个（dashboard 20s + 最近订单 20s，`dashboard.tsx:183-186`）、雷达 2 个（机会 20s + 当前账户未平仓持仓 20s，`opportunities.tsx:214-219`）+ 一次性 `/settings`（`:237-245`）、持仓 1 个（`positions.tsx:54-55`）、订单 1 个（`orders.tsx:74-77`）、参数设置 2 个（`/settings` 30s + `/dashboard` 20s，`settings.tsx:396-399`）、系统设置手动 `load()`（`system.tsx:682-702`）、`AppShell` 健康检查 30s（`app-shell.tsx:144-145`）。
- **后端契约（只读核对）**：`/api/v1` 路由见 `router.go:25-60`；`opportunities` 处理器完全不读 query（`router.go:173-175`）；`positions`/`orders` 只读 `account_id`/`status`/`limit`/`offset`（`router.go:221-275`）；**`grep -rn "venue" backend --include=*.go` 无结果**（0 命中）。即：今天前端**无法**从 API 拿到任何 venue 维度，也无法按 venue 过滤。
- **前端测试**：`frontend/package.json` 只有 `dev/build/preview/typecheck`，无测试框架。本文所有改动**没有自动化回归网**，`tsc -b` 是唯一门禁。

---

## 2. (a) Polymarket 假设清单

> 表格四列：`file:line` | 现状 | 为什么两场所会坏 | 最小改动。
> 「坏」分三级：**L1 文案错误**（只是说错话）、**L2 语义错误**（数值/标签/交互在一个场所上不成立）、**L3 静默错数据**（不报错但结果错，最危险）。

### 2.1 DTO（`frontend/src/types.ts`）

| file:line | 现状 | 为什么坏 | 最小改动 |
|---|---|---|---|
| `types.ts:3-16` `Account` | 7 个实盘字段 `live_configured/live_ready/live_signer/live_maker/live_error`，无 `venue` | 两个场所的凭据/就绪状态互不相同；一个账户若跨场所，一个 badge（`app-shell.tsx:193-195`）无法表达「PM 就绪、PF 未配置」 | `+ venue?: VenueId`（推荐：账户即场所，见 §3.1）。`live_*` 语义改为「该账户所属场所的就绪状态」，字段名不变 |
| `types.ts:23-32` `Health` | `mode`、`default_account_mode`、`live_*` 全局单值 | 目前只用到 `health.status`（`app-shell.tsx:153`），但 `live_ready` 一旦被用于门禁就会把 PM 的未就绪映射到 PF | 保留；`+ venues?: { venue, live_ready, live_error }[]`。**不要**用 `Health.live_ready` 做下单门禁 |
| `types.ts:34-53` `Metrics` | `cash`/`wallet_cash`/`wallet_cash_stale`/`equity`/`fees_paid`/`open_exposure`… 全标量、无 `currency` | USDC(6) 与 USDT(18) 不可相加；`wallet_cash` 是「链上抵押余额」语义，PF 的对应物是 BSC 上的 USDT 余额，取值路径与 stale 语义不同 | `+ currency: 'USDC'\|'USDT'`（用于标签，不参与运算）；若坚持多场所账户，再加 `by_venue?: Partial<Record<VenueId, Metrics>>` |
| `types.ts:55-61` `Engine` | 单 `running`/`last_scan_at`/`last_error`/`scan_count` | 架构文档 §2.4 要求「按场所故障隔离」；今天一个场所 502 会让整张「系统状态」卡（`dashboard.tsx:210-248`）红掉，用户无法判断是哪个场所 | `+ venues?: { venue, ok, last_scan_at, last_error }[]`；`last_error` 保留为首错汇总 |
| `types.ts:63-66` `Source` | `name` + `last_sync_at`，无 venue | 两个行情源（Gamma / PF）无法同时展示；`dashboard.tsx:230`「行情数据源」只有一行 | `+ venue?: VenueId`，并把 `Dashboard.source` 改成 `sources: Source[]`（保留 `source` 兼容一版） |
| `types.ts:68-71` `EquityPoint` | `{ time, value }` | 曲线的 `value` 无货币/场所；混算后 `formatAxisCurrency` 会统一打成 `$`（`format.ts:27-35`） | `+ venue?: VenueId` `+ currency?`；单场所账户可只用 `value` |
| `types.ts:73-79` `Dashboard` | 单 `metrics`/`engine`/`source`/`equity` | 一次响应只能表达一个场所的资金与状态 | 首选**不改**（账户即场所，见 §3.1）；否则 `+ by_venue` |
| `types.ts:81-105` `Opportunity` | `token_id`(91) 作身份；`condition_id`(83)；`version?`(103-104) 仅 PM 协议版本；`question_zh`/`event_title_zh`/`outcome_zh`(85,87,90) 是 Gamma locale；`sports_game`/`overtime_risk`(100-101) 来自 PM tag 启发式 | L2：PF 行没有 `venue` 就不可能与 PM 行区分；PF 无 `version`，`ProtocolBadge` 直接不渲染（`opportunities.tsx:151`）；PF 无中文文案；PF 的 `endsAt` 在 category 上，`end_date`/`days_to_end` 可能缺失 → 行必须能被显式标记为 unsupported | `+ venue: VenueId`、`+ currency`、`+ unsupported?: string`、`+ order_types?: ('LIMIT'\|'MARKET')[]`、`+ min_notional?: number`。`version` 保留为 PM 专属可选字段 |
| `types.ts:118-124` `OpportunitiesResponse` | `source: string` 单值 | 两场所扫描后 `source` 只能拼字符串（`opportunities.tsx:394` 直接显示） | `+ sources?: { venue, name, scanned_at }[]`，`source` 保留兼容 |
| `types.ts:128-149` `OpportunityQuote` | `limit_price`(133)、`fee_rate`(137)、`payout`(140)、`fully_filled`(145)、`exceeds_max_price`(146)、`max_price`(147) | L2：PF `MARKET` 单没有限价；PF 费率是「每市场 `feeRateBps`、maker 0 + rebate」，与 PM 的 `FeeSchedule` 按类别计费不同构；`payout` 隐含「1 份额 = 1 单位抵押物」 | `+ venue`、`+ currency`、`+ order_type?`、`+ maker_fee?`/`+ rebate?`；`limit_price` 变为可选 |
| `types.ts:151` `PositionStatus` | `open\|won\|lost\|split\|sold` | `split` 是 PM 的 50/50 平局退款语义（`status-badges.tsx:17`「平局退款」）；PF 的 resolver 不保证有该状态 | 保留枚举，`+ venue` 后由 `venueMeta` 决定文案；新增状态时才扩展 |
| `types.ts:153-176` `Position` | `condition_id`(157)、`token_id`(160)、`side:'BUY'`(161)、`exit_kind`(164)，无 venue/currency | L3：持仓表与「已买入」标记都无法区分场所；`cost`/`fee` 单位未标注 | `+ venue`、`+ currency`、`+ entry_order_type?` |
| `types.ts:186-206` `Order` | `type: 'buy'\|'sell'\|'settle'`(195)、`action`(197)、`external_id`(204) 隐含 CLOB 订单号 | **前端完全没有 LIMIT/MARKET 概念**；PF 的 MARKET 不是 FAK，不能沿用「立即成交剩余取消」文案；`external_id` 在 PF 是数字订单号 | `+ venue`、`+ order_type?: 'LIMIT'\|'MARKET'\|'FAK'\|'FOK'`、`+ limit_price?`、`+ external_id_label?`（或由 venueMeta 推导） |
| `types.ts:213-232` `Settings` | 18 个标量字段 | L2：两场所的价格区间、流动性、单笔金额、敞口、费率、分类模型都不同；`default_category`(225) 与 `sports_policy`(226) 是 PM tag 体系；`max_price_change_24h`(227) 是 Gamma `oneDayPriceChange`；`fee_bps`/`slippage_bps`(222-223) 只对 PM paper 有意义 | 拆「共享」与「按场所」：共享 `scan_interval_seconds`、`initial_bankroll`、`entry_window_minutes`、`sports_entry_window_minutes`、`stop_loss_*`；按场所 `min_price/max_price/min_liquidity/order_size/max_position/max_exposure/slippage_bps/fee_bps/default_category/sports_policy/max_price_change_24h`。形状：`+ venues?: Record<VenueId, VenueSettings>`，旧字段保留并镜像 `venues.polymarket` |
| `types.ts:242-245` `DataSourceSettings` | `gamma_base_url` + `clob_base_url` | L2：字段名就是场所名；PF 需要 `base_url` + testnet 开关 + `x-api-key` | 保留兼容；新增 `+ venues?: Record<VenueId, { base_url, trade_url?, testnet?, api_key? }>` |
| `types.ts:247-254` `LiveSettings` | `private_key`/`funder`/`signature_type`/`api_key`/`api_secret`/`api_passphrase` | L2：`funder` + `signature_type` + L2 三件套是 PM 专属；PF 是 EOA/Kernel + `x-api-key` + JWT | 保留 `polymarket` 形状；新增 `VenueLiveSettings`（`private_key`、`api_key`、`rpc_url`、`wallet_kind`） |
| `types.ts:256-268` `LiveProbeResult` | `mode:'eoa'\|'proxy'`(263)、`cash`/`allowance`(265-266) | L2：PF 探测返回的是 USDT 余额与授权，`allowance` 是 CTF Exchange 授权，语义不同 | `+ venue`、`+ currency`；`mode` 换成 `wallet_kind?: string` 并用 venueMeta 解释 |
| `types.ts:270-300` `RelayerProbeResult`/`WalletCandidate`/`FunderDiscoveryResult` | PM 代理钱包发现（profile / CREATE2 / clob-proxy-wallets） | L2：PF 无 funder 概念，钱包发现流程完全不同 | 整体标注为 PM-only（保留在 `system.tsx` 的 PM Tab 内） |
| `types.ts:302-311` `LiveEnvironment` | `clob_base_url`、`polygon_rpc_url`、私钥/L2 的 `*_set` | L2：PF 需要 `bsc_rpc_url` 与 `api_key_set`，没有 passphrase | `+ venue` / `+ bsc_rpc_url?`；或整块下沉为 `Record<VenueId, LiveEnvironment>` |
| `types.ts:313-323` `SystemRuntime` | 单对 `gamma_base_url`/`clob_base_url` + 单 `live_environment` | L2：`EndpointSection` 只渲染一对 URL（`system.tsx:152-195`） | `+ venues?: Record<VenueId, { base_url, trade_url, live_environment, live_ready, live_error }>` |
| `types.ts:331-339` `SystemSettings` | 单 `data_source`/`live`/`runtime` + `proxy_probe_url`(337) | L2：`proxy_probe_url` 由后端写死 `clob.polymarket.com/time`（`backend/internal/api/system.go:21`），PF-only 用户测的是 PM 连通性 | `+ venues`；`proxy_probe_url` 变成 per-venue 或至少标注「探测 PM」 |

### 2.2 API 客户端（`frontend/src/lib/api.ts`）

| file:line | 现状 | 为什么坏 | 最小改动 |
|---|---|---|---|
| `api.ts:27` | `API_BASE = VITE_API_BASE_URL \|\| '/api/v1'` | 无 | 不动 |
| `api.ts:82-85` `withAccount()` | 唯一 query 助手，只会拼 `account_id` | L2：没有 venue 维度可传；两个场所的列表接口无法过滤 | 改为 `withQuery(path, { account_id?, venue?, limit?, offset? })`（保留 `withAccount` 作为薄封装，避免改动 5 个调用点） |
| `api.ts:98` `opportunities` | `request('/opportunities')`，**零参数** | L3：无法按 venue 过滤；前端只能全量拉取后 client-side 过滤，与后端分页/截断叠加时结果不可信 | `(venue?: VenueId, signal?) => .../opportunities?venue=...`。后端当前忽略 query（`router.go:173-175`），所以这是**前端先行、后端随后**的安全改法 |
| `api.ts:99-104` `quoteOpportunity` | body 只有 `{ token_id, amount }` | L2：PF 的 token 是 BSC 上的 onChainId，报价必须知道场所才能选订单簿与费率 | body `+ venue`；后端在缺省时按 PM 处理 |
| `api.ts:105-106` `positions` | `withAccount('/positions?status=...')` | L2：无 venue | `+ venue?`（作为第 4 个可选参数，保持调用点兼容） |
| `api.ts:107-113` `orders` | `account_id` + `status` + `limit` + `offset` | L3：`limit/offset` 是**服务端分页**，前端再按 venue 过滤会让「共 N 条」与页大小失配（见 §4.7） | `+ venue?` 进 query，不要 client-side 过滤分页数据 |
| `api.ts:114-116` `settings`/`updateSettings` | 整对象 `PUT` | L2：per-venue 设置若拆成多次 PUT，会出现「保存 A 场所时覆盖 B 场所」；若整对象 PUT，前端必须持有完整 `venues` map | 保持整对象 PUT，前端本地维护完整 map（最省后端改动）；后端做「缺字段不动」的合并 |
| `api.ts:130` `testProxy` | body `{ proxy }` | L1/L2：探测目标是后端常量 `clob.polymarket.com/time`（`system.go:21`） | `+ venue?`，后端按场所选探测 URL |
| `api.ts:132-137` `testLive(live, clobBaseURL)` | 位置参数 `clobBaseURL` | L2：PF 没有 clobBaseURL，签名/鉴权模型不同 | 新增 `testVenueLive(venue, settings)`，保留旧函数给 PM |
| `api.ts:138-154` `testRelayer` | PM relayer 专用 | L1/L2：PF 无 relayer | PM Tab 内保留 |
| `api.ts:155-160` `detectWallet` | 位置参数 `clobBaseURL` | L2：PM 钱包发现算法（profile/CREATE2）对 PF 不适用 | 改名 `detectPmWallet` 或包进 `testVenueLive` 的 PM 分支 |
| `api.ts:163-169` `scan` | **已经用 `URLSearchParams`** 拼 `execute` + `account_id` | 这是全项目唯一现成的 query 构造点；`scan` 本身是全局动作，两场所扫描语义需明确（全场所 or 当前场所） | 直接 `params.set('venue', ...)`；确认后端「scan 全部启用场所」语义 |
| `api.ts:170-174` `createOrder` | body `{ market_id, outcome, token_id, amount }` | **L2 最严重**：无 venue、无 `order_type`、无 `limit_price`、无 `slippage_bps`/`isMinAmountOut`。PF 只有 LIMIT/MARKET，PM 走 FAK，前端连选择入口都没有 | body `+ venue`、`+ order_type?`、`+ limit_price?`、`+ slippage_bps?`。UI 侧见 §3.7 |

### 2.3 格式化 / 市场文案 / 分类

| file:line | 现状 | 为什么坏 | 最小改动 |
|---|---|---|---|
| `format.ts:1-6,17-21` `usd` / `formatCurrency` | `Intl.NumberFormat('en-US', { currency:'USD' })`，输出 `$1,234.56` | L2：USDT 金额也会显示 `$`，用户无法分辨币种；`Intl` 对非 ISO-4217 的 `'USDT'` 行为不保证（V8 会把代码当符号输出，属实现细节，须用 `currencyDisplay` 显式固定） | `formatCurrency(value, currency: QuoteCurrency = 'USDC', signed?)`；内部用两个 `Intl` 实例缓存。`formatCurrency` 实有 30 个调用点，可通过默认参数零行为变化 |
| `format.ts:8-13,23-25` `compactUsd` / `formatCompactCurrency` | 同上，compact 记法 | L2：同上 | 同上加 `currency` 参数 |
| `format.ts:27-35` `formatAxisCurrency` | USD + 每次调用新建 `Intl` 实例 | L2：两场所曲线不能共用一条 `$` 轴；每次 tick 新建 formatter 是额外开销 | `+ currency`；把实例提到模块常量（顺带修性能） |
| `format.ts:52-55` `formatPrice` | `` `$${value.toFixed(3)}` `` —— 给**概率**（0–1）加 `$`，固定 3 位 | L1/L2：`$0.975` 对 PM 是「0.975 美元/份」，对 PF 是「0.975 USDT/份」，同为概率但符号误导；PF tick size 若不同（1e-4/1e-2），固定 3 位会显示成无意义的精度 | `formatPrice(value, opts?: { currency?: QuoteCurrency; digits?: number })`；或 `formatProbability(value, venue)` 不带货币符号 |
| `format.ts:57-63` `formatPriceChange` | `value*100` 后加 `¢`，注释写明来自 Gamma `oneDayPriceChange` | L2：`¢` 是 PM 的「概率分」单位；PF 若无 24h 涨跌（`price_change_24h` 为空）行内会整块消失（`opportunities.tsx:418` 已有 `!= null` 守卫，不会崩，但列语义变成场所相关） | 保留；`+ unit?: 'cent'\|'pt'`，PF 若提供则用 `pt` 或不显示 |
| `format.ts:37-40,103-109` `formatNumber` / `numberFormatter` | 默认 2 位；`numberFormatter` 导出但**全仓库无引用**（`grep numberFormatter` 仅 `format.ts:109`） | L3：份额用 `formatNumber(x, 4)`（`positions.tsx:77`、`orders.tsx:119`、`opportunities.tsx:470`）显示 4 位小数；PF 18 位最小单位下的小额持仓会显示成 `0.0000`，「已买入 0.0000 份」是静默错误 | 份额格式化改为按场所小数位：`formatShares(value, venue)`；顺手删除 `numberFormatter` 死导出 |
| `market-language.ts:26-39` `marketQuestion` | 依次取 `question_zh` → `event_title_zh` + `group_item_title` → 原文 | L2：PF 无中文字段，中文模式下整列回退英文，但页脚仍写「优先使用 Polymarket 中文文案」（`opportunities.tsx:393`） | 文案改 per-venue（venueMeta.l10n: 'zh+en' \| 'en'）；`marketQuestion` 只在 `venueMeta.hasZh` 时走翻译分支 |
| `market-language.ts:52-67` `localizeOutcome` | 硬编码 `over/under/yes/no/draw` | L2：PF 的 outcome 常是队名/数字，命中率未知 | 保留；unknown 直接返回原文（现行为已是如此） |
| `market-language.ts:69-85` `localizeGroupItem` | 正则匹配 `Spread:`、`Exact Score:`、`O/U`、`Both Teams to Score`、`Moneyline`、`Team Totals`、`Halftime Result`、`Player Props` | L2：PM 体育盘口文案体系；PF 不同 → 这些分支对 PF 是死代码（无害但会误导维护者） | 移入 `venueMeta.pm` 或 `lib/pm-market-language.ts` |
| `market-language.ts:87-100` `marketURL` | `parsed.hostname === 'polymarket.com'` 才加 `/zh` | L1/L2：语言本地化 URL 策略应属于 venue 元数据；另外 `www.polymarket.com`、`polymarket.us` 等 host 不匹配（现有小 bug）；PF 链接在中文模式下不变（行为可接受，但没地方声明） | `venueMeta.localizeURL?(url, lang)`；PM 实现里用 `hostname.endsWith('polymarket.com')` |
| `market-language.ts:7-16` `readMarketLanguage` | 默认 `'zh'` 存 localStorage | L1：对纯英文场所，默认中文 = 必然回退，用户以为选择没生效 | 保留；在语言下拉旁按当前 venue 提示「该场所暂无中文」 |
| `category.ts:3-15` `categoryFilters` | `all/non_sports/sports/politics/crypto/finance/tech/culture/other` | L2：「非体育」是 PM tag 体系派生；PF 的 `category` 是自有 slug（且 `endsAt` 挂在 category 上） | venueMeta 提供 `categories`；共享 chip 行在「全部场所」下只保留 `all` + 两场所交集的粗分类，或把 chip 行改为「venue × category」两级（§3.4） |
| `category.ts:17-25,27-29` `categoryLabels` | 中文标签映射 | L2：未知分类回退「其他」，PF 的分类会全被塞进「其他」 | `categoryLabel(category, venue?)`，PF 走 `venueMeta.categoryLabels` |
| `category.ts:35-37` `normalizeCategoryFilter` | 非法值回落 `'non_sports'` | L2：对 PF-only 视图，`non_sports` 没有意义 | 回落值改为 `venueMeta.defaultCategory` |
| `category.ts:39-43` `matchesCategory` | 基于 `item.category === filter` | L2：`non_sports` 判断依赖 PM 的 `sports` 值 | 保留；在 `+ venue` 后由 venueMeta 把 PF 分类映射到共享集合，或只在 PM 行上应用该筛选 |
| `category.ts:45-62` `categoryCounts` | 输入 `allItems`，输出各 chip 计数 | L3：计数在 venue 过滤**之前**计算（`opportunities.tsx:248` 对 `allItems`，`:249-256` 才做 category/keyword 过滤）→ 加了 venue 筛选后，chip 上的数字会包含另一场所，与可见行数不符 | 先按 venue scope 过滤再 `categoryCounts`；或 `categoryCounts(items, venueScope)` |

### 2.4 轮询与账户上下文

| file:line | 现状 | 为什么坏 | 最小改动 |
|---|---|---|---|
| `usePolling.ts:22-28` | fetcher 身份变化即清空 `data`/`error` | **L3 交互级**：venue 筛选若写进 `useCallback([venue])`，每次切 Tab 都 `setData(null)` → `isLoading=true` → 整块表格变骨架屏，且丢失上一轮数据无法做「stale」提示。这是把筛选做「便宜」还是做「坏」的分水岭 | 加 `{ keepPreviousData?: boolean }`；或把 fetcher 固定为 `() => api.positions(accountId, status, venue)` 并用 ref 读取最新 venue（fetcher 不变，不清空）；推荐前者，语义更明确 |
| `usePolling.ts:10` | 默认 20s | L2：两场所 × 每视图 → 请求数翻倍；若再为每场所各建一个轮询（例如每场所一张现金卡），dashboard 会从 2 个轮询变 6 个 | 新增 `GET /dashboard` 的 `by_venue` 字段（一次请求多场所），而不是 N 个轮询 |
| `usePolling.ts:54-57` | 出错时保留旧数据 + 单个 `error` 字符串 | L2：一个场所 502 → 全局 `InlineError`（`page-header.tsx:23-34`），用户以为 PM 也挂了。缺「按场所的 stale/健康」表达 | 错误体带 venue（`ApiError` 已有 `status`，可加 `venue?`）；`InlineError` 旁边加场所级状态点（§3.3） |
| `account-context.tsx:6,26-27,42,48` | `localStorage['pennyharvester.account']` | L1：账户即场所后，用户切换场所 = 切换账户，选中项跨场所持久化是合理的；但**没有「最近使用的场所」概念**，雷达的场所 scope 需要另一个键 | 新增 `pennyharvester.venue-scope`（雷达/持仓/订单共享），与 account 键分离 |
| `account-context.tsx:36-43` | 账户失效回落 `is_default`/第一个 | L1：两场所各有 live/paper 后，「默认账户」的语义变成「默认 (venue,mode)」 | 保留；`is_default` 语义写成「默认 (venue, mode) 组合」 |
| `account-context.tsx:22` | 账户列表 30s 轮询 | 无（账户数很少） | 不动 |

### 2.5 总览（`views/dashboard.tsx`）

| file:line | 现状 | 为什么坏 | 最小改动 |
|---|---|---|---|
| `dashboard.tsx:196-199` | PageHeader 描述：「当前账户的买单会直接通过真实钱包提交到 **Polymarket**」 | L1 | `venue?.label ?? 'Polymarket'` |
| `dashboard.tsx:89` | 卡片副标题 `{label} · USD · {n} 个记录点` | L2：USDT 曲线也写 USD；「USD」是硬编码字面量，不是来自数据 | `· {currency} ·`（来自 `metrics.currency` 或 venueMeta） |
| `dashboard.tsx:147-162` **单张现金卡** | 卡 2：label「策略可用资金/可用虚拟资金」，value `formatCurrency(metrics.cash)`，detail「钱包 pUSD {wallet_cash}」 | **L2 核心问题**：只有一个 `cash` 和一个链上余额位。两场所要么挤成一行、要么只能显示当前场所（此时「策略可用资金」标签会让人误以为是总额） | 首选（账户即场所）：本卡保持不变，只把「pUSD」换成 `venueMeta.collateralSymbol`。若确实要多场所聚合：卡 2 改为「按场所分行」列表，**每行独立货币、永不求和**（§3.4） |
| `dashboard.tsx:160` | `钱包 pUSD` + `wallet_cash_stale` 的「暂显上次余额」 | L1/L2：PF 的链上余额读取路径不同（BSC RPC），stale 语义需保留但名称要变 | `venueMeta.collateralSymbol` + `venueMeta.walletLabel` |
| `dashboard.tsx:163-169` | 「累计盈亏」+ `fees_paid` | L3：若聚合两场所，`fees_paid` 会把 PM taker 费与 PF taker 费（2% × min(p,1−p) × shares）相加，而 PF **maker 0 费 + rebate** 在现有字段里无法表达（负数费用） | 单场所账户无问题；聚合时按场所分列，并在有 rebate 时把 rebate 单列 |
| `dashboard.tsx:170-176` | 「当前持仓投入」`open_exposure` | L3：同上，跨币种敞口相加无意义 | 同上 |
| `dashboard.tsx:183-186` | 两个轮询：`api.dashboard(accountId)` + `api.orders(accountId,'all',6,0)` | L2：两者都缺 venue；若一个账户跨场所，「最近订单」会混场所且无 venue 列（列头见 `:260`） | 账户即场所时无需改；否则 `+ venue` 列 |
| `dashboard.tsx:218-227` | 「系统状态」：引擎行 + `isLive ? '实盘钱包' : '当前账户'` 行，钱包值取 `live_maker \|\| live_signer` | L2：`live_maker`/`live_signer` 是 PM 的 Signer/Maker 概念；PF 只有一个钱包地址 | 该行文案与取值改为 venueMeta 驱动（PM：Maker/Signer；PF：钱包地址） |
| `dashboard.tsx:228-231` | 「行情数据源」单行 `data.source.name` | L2：两场所两个源 | `data.sources?.map(...)` 或按场所 scope 显示 |
| `dashboard.tsx:240` | 提示文案含环境变量 `POLYMARKET_PRIVATE_KEY` | L1 | 改为该场所的环境变量名（venueMeta.envHint） |
| `dashboard.tsx:259-263` 最近订单表 | 列：市场/方向、类型、金额、状态、时间；`formatCurrency(order.amount)`；类型文案 `order.type === 'buy' ? 买入 : '结算'` | **L3 现有 bug**：`Order.type` 有 `'sell'`（`types.ts:195`），止损卖出（`action==='stop'`）在总览里会被显示成「结算」；`orders.tsx:30-34` 的 `orderTypeLabel` 是正确实现，两处不一致。两场所后还会混入 LIMIT/MARKET | 复用 `orderTypeLabel`；`+ venue` 列；金额带 currency |

### 2.6 机会雷达（`views/opportunities.tsx`）

| file:line | 现状 | 为什么坏 | 最小改动 |
|---|---|---|---|
| `opportunities.tsx:39-46` `MarketLink` | `aria-label="在新窗口打开 Polymarket 市场"` | L1（无障碍） | 用 venueMeta.label |
| `opportunities.tsx:64-85` `heldPositionsByToken` | `Map<string, HeldPosition>`，键为 `position.token_id`（`:69,78-84`） | **L3**：仅用 token_id 作键。两场所的 token/onChainId 都是十进制 ERC-1155 id 字符串，小整数 id 在 PF 侧很可能存在 → PM 持仓会在 PF 行显示「已买入」并给出错误的买入价 | 键改为 `${venue}:${token_id}`，所有 `heldByToken.get(...)`（`:417,418,430`）同步 |
| `opportunities.tsx:91-103` `HeldBadge`/`HeldPrice` | 显示「已买入」「买入价」 | L3：继承上一条；`formatPrice(held.entryPrice)` 也没有货币 | 跟随修 `formatPrice` |
| `opportunities.tsx:116-144` `RealYield` | tooltip「读取 **Polymarket** 实时订单簿与 FeeSchedule」；渲染 `net_yield`、`成交均价`、`费`、`深度不足` | L2：PF 的报价模型不同（MARKET + slippageBps + isMinAmountOut，maker 0 费 + rebate）；同名 `net_yield` 在两场所含义不同却并排展示，用户会直接横向比较 | 组件加 `venue`；PF 显示 `MARKET`/`LIMIT` 与预估滑点、maker/taker 区分；文案走 venueMeta |
| `opportunities.tsx:150-160` `ProtocolBadge` | 仅 `item.version === 'v2'` 时渲染 danger badge，title「Polymarket Protocol V2 市场（positionId + ExchangeV3）…」 | **L2**：产品里实际只有 V2 徽章，v1 无任何标记（与「v1/v2 徽章」的预期不符）；PF 行既无 venue 标识也无 `version`，`ProtocolBadge` 返回 `null` → PF 行看起来和 PM v1 行完全一样 | 泛化为两层：`VenueBadge`（恒定显示，PM/PF chip）+ `ProtocolBadge`（仅 PM 的 v2 danger）；PF 显示能力 chip（`LIMIT/MARKET`、`maker 0 费`）或 `unsupported` 原因 |
| `opportunities.tsx:162-190` `OpportunityCard`（移动端卡片） | 徽章行 `marketOutcome` + `categoryLabel` + `HeldBadge` + 体育 + `ProtocolBadge`；`formatPrice(item.price)`；`formatPriceChange` | L2：缺 venue chip；`formatPrice` 带 `$` | 加 `VenueBadge`，其余跟随格式化函数 |
| `opportunities.tsx:208` `quotes` state | `Record<string, QuoteState>`，键 `item.token_id` | **L3**：同 `heldPositionsByToken`，两场所 token 冲突会展示错误报价 | 键 `${venue}:${token_id}`（`:260,264-276,420,430,481-482,492` 同步） |
| `opportunities.tsx:209,237-245` `orderSize` | 从 `/settings` 读单个 `settings.order_size` 作为报价与预估金额 | L3：PF 的最小下单额（约 1 USDT）与 `order_size` 无关；用同一个数字给两场所报价会产生不可比的「真实收益」 | per-venue：`settings.venues[venue].order_size`；报价按钮显示当前场所金额 |
| `opportunities.tsx:214-220` | `api.opportunities()` 无参 + `api.positions(accountId,'open')` | L2：雷达是**唯一不做账户隔离**的列表（全局机会），因此也是唯一必须真正做 venue 维度的列表 | `api.opportunities(venueScope)`；持仓标记接口也应带 venue 或由 `Position.venue` 自行区分 |
| `opportunities.tsx:247-256` | `allItems` → `counts`（基于 `allItems`）→ `filtered`（category + keyword） | **L3**：venue 若加进 `filtered` 而不加进 `counts`/`allItems`，chip 数字与可见行不一致（见 §2.3 `categoryCounts`） | 先 venue 再 category；`counts` 与 `filtered` 用同一个 `scoped` 数组 |
| `opportunities.tsx:362` | 「N 个匹配结果 / 共 {data.total} 个」 | L3：`data.total` 是后端总数（跨场所）；client-side venue 过滤后「共」值仍是全局 → 数字对不上 | `+ venue` query；或把「共」改成「已载入」措辞 |
| `opportunities.tsx:366-383` 分类 chip 行 | 单行 chip：全部/非体育/政治/…，各带计数 | L2：两场所分类体系不同，单行 chip 表达不了「场所 × 分类」；chip 数会变 8 → 16 | 加**独立的一行 venue chip**（放在分类行上方，`aria-pressed`），分类 chip 作用在 venue 过滤之后；避免把 venue 塞进同一行 |
| `opportunities.tsx:387-395` 说明条 | 6 条静态说明，含「按『参数设置 → 单笔金额』」「来源 {data.source}」 | L2：单笔金额是场所相关的；`data.source` 单值 | 按 scope 显示；`来源` 改成 `sources.map(venue+name)` |
| `opportunities.tsx:407-429` 桌面表 | `min-w-[1080px]`、7 列、`key={`${item.market_id}-${item.token_id}`}` | L2：加 venue chip 后表更宽（手机端已有独立卡片分支，可接受）；`market_id` 跨场所也可能重复（PM 是数字串、PF 是整数）→ key 应含 venue | `key={`${venue}:${market_id}:${token_id}`}`；chip 进「市场/方向」单元格（`:417`） |
| `opportunities.tsx:434-456` 扫描对话框 | 「实盘账户会向 **Polymarket CLOB** 提交真实买单，并动用真实 **pUSD**」 | L1/L2：`scan` 是全局动作，两场所下这句话只对 PM 成立；且没说清扫描会扫哪些场所 | 文案按「本次扫描的场所集合」动态生成（scope 为 all 时列出两个场所） |
| `opportunities.tsx:458-508` 买入对话框 | `:462`「订单将提交至 Polymarket CLOB」；`:464`「向 Polymarket 提交真实买单」；`:473` 金额单位写死 `USD`；`:480`「真实净收益（实盘盘口 + FeeSchedule）」；`:487`「限价 {limit_price}」；`:491`「读取 Polymarket 实时订单簿与 taker 手续费」；`:497` 确认文案「订单会立即提交到 Polymarket」；`:499` `pUSD 授权` / `POLYMARKET_PRIVATE_KEY` | **L2 集中区**：这是单笔真实下单的确认页，每一句都直接决定用户是否理解自己在哪个场所、用什么币、什么订单类型下单 | 全部走 venueMeta；`USD` → `venueMeta.currency`；`限价` 行仅在 LIMIT 或 PM 显示；**新增订单类型选择**（见 §3.7） |
| `opportunities.tsx:495-498` 确认勾选 | 单一勾选「我确认这是真实资金实盘买入」 | L2：PF market 单可能以更差价格成交（slippageBps），确认文案需包含订单类型与滑点 | 勾选文案按 order_type 生成 |
| `opportunities.tsx:195` | `liveReady = !isLive \|\| Boolean(account?.live_ready)` | **L2**：全局就绪位。两场所下，PF 未配置会禁用 PM 的下单按钮（反之亦然） | `venueReady = !isLive \|\| (account.venue === itemVenue && account.live_ready)`；PF 未就绪只灰 PF 行 |

### 2.7 持仓（`views/positions.tsx`）

| file:line | 现状 | 为什么坏 | 最小改动 |
|---|---|---|---|
| `positions.tsx:15-22` `tabs` | 全部/持有中/已获胜/分摊退款/已止损/未获胜 | L1/L2：「分摊退款」是 PM 50/50 语义 | 保留；文案由 `PositionStatusBadge`/tabs 的 venueMeta 决定（或统一改成中性「退款」） |
| `positions.tsx:54` | `api.positions(accountId, status)` | L2：无 venue；一个账户跨场所时列表混排且无列 | `+ venue` |
| `positions.tsx:59` | 描述：「结算损益为链上可领取权益，**需在 Polymarket 手动领取**」 | L1/L2：PF 的赎回路径不同（自建 resolver / redeem 交易），这句话对 PF 是错的 | venueMeta.settlementHint |
| `positions.tsx:32` | 卡片小标题「实盘头寸/模拟头寸 #{id}」 | L1：无 venue | 「{venueLabel} 实盘头寸」 |
| `positions.tsx:35` | `formatNumber(position.shares, 4)` + `formatCurrency(position.cost)` | L3：4 位小数截断（18 位场所）、货币符号错 | `formatShares(x, venue)` + `formatCurrency(x, currency)` |
| `positions.tsx:73-74` 桌面表 | 7 列、`min-w-[1080px]`、无 venue 列 | L2：混排无法区分；加列后宽度需重排（`lg` 断点已有卡片分支兜底） | 在「市场/方向」单元格加 chip（`:77`），不必新增列 |
| `positions.tsx:77` | 行内：`formatPrice(entry/current)`、`formatNumber(shares,4)`、`formatCurrency(cost/fee/pnl)` | L3：同 `:35`；另外「费用」列在 PF 有 rebate 时可能为负 | 跟随格式化函数 |
| `positions.tsx:65` | 「{n} 个未平仓实盘头寸」 | L1：跨场所时应说「2 个场所共 N 个」或按 scope | scope 文案 |

### 2.8 订单（`views/orders.tsx`）

| file:line | 现状 | 为什么坏 | 最小改动 |
|---|---|---|---|
| `orders.tsx:15` | `PAGE_SIZE = 100` | L3：与服务端 `limit` 绑定（`api.ts:107-113`） | 保持 |
| `orders.tsx:17-23` `filters` | 仅状态维度：全部/已成交/已结算/待对账/已拒绝 | L2：缺 venue 维度；但 venue 必须是**服务端**过滤（见 §4.7） | 在状态 tabs 右侧加 venue chip/Select，走 query |
| `orders.tsx:25-28` `orderKindLabel` | 手动/自动/止损保护 | L1 | 不动 |
| `orders.tsx:30-34` `orderTypeLabel` | `stop` → 实盘/模拟止损卖出；`sell` → 卖出；`buy` → 买入；其它 → 结算 | **L2**：无 LIMIT/MARKET 表达；PM 的 FAK/FOK 不可见 | `orderTypeLabel(order, venueMeta)` → 「LIMIT 买入」/「MARKET 卖出」等 |
| `orders.tsx:36-65` `OrderCard` + `:59` | 移动卡片，`CLOB {order.external_id}` | L1/L2：PF 的 external id 不是 CLOB 订单号 | `venueMeta.externalIdLabel` |
| `orders.tsx:81` | 描述：「包括 **Polymarket CLOB 订单号**、成交、结算…」 | L1 | venueMeta |
| `orders.tsx:95` | 空态：「并附 **CLOB 订单号**」 | L1 | venueMeta |
| `orders.tsx:103-105` | `min-w-[1120px]`，6 列，无 venue 列 | L2：混排无法区分 | chip 进「订单/市场」单元格（`:112`） |
| `orders.tsx:118-119` | `formatCurrency(order.amount)` / `formatPrice(order.price)` / `formatNumber(order.shares,4)` | L3：货币与精度 | 跟随格式化函数 |
| `orders.tsx:123` | `CLOB {order.external_id}` | L1/L2 | venueMeta |
| `orders.tsx:133-139` 分页 | `data.total > PAGE_SIZE` 时显示「第 x–y 条，共 N 条」；下一页按钮用 `offset+items.length >= data.total` 禁用 | **L3**：`data.total` 是服务端跨场所总数。若改为 client-side venue 过滤，页内行数 < 100，但文案仍说「共 N 条」，且「下一页」在错误的点禁用 | venue 必须进 query（`api.orders(..., venue)`）；不要在分页数据上做 client-side venue 过滤 |

### 2.9 参数设置（`views/settings.tsx`）

| file:line | 现状 | 为什么坏 | 最小改动 |
|---|---|---|---|
| `settings.tsx:37-42` | `numericKeys`/`selectKeys`/`Draft`/`FieldErrors` 全扁平 | **L2 结构性问题**：`Draft` 是一维 `Record<key,string>`；per-venue 参数无处安放 | 抽出 `settings/schema.ts`，把 `Draft` 改为 `{ shared: SharedDraft; venues: Record<VenueId, VenueDraft> }`（§3.5） |
| `settings.tsx:62-83` `toDraft` / `:85-106` `toSettings` | 标量 ↔ 表单字符串的一一映射 | L2：需要按场所映射，且旧 `Settings` 与 `venues` map 的双向兼容（缺 `venues` 时合成 PM） | `toDraft` 接受 `Settings` 返回分组 draft；`toSettings` 输出「旧字段 + venues map」双写 |
| `settings.tsx:108-135` `validate` | 单套校验：`min_price>0`、`max_price<1`、`min≤max`、`max_days 1–3650`、`order_size>0`、`max_position≥order_size`、`max_exposure≥order_size`、`slippage_bps 0–1000`、`fee_bps 0–1000`、`max_price_change_24h 0–1` 等 | L2：校验规则本身大多是场所无关的「概率/币值」规则（可复用），但 `max_price<1` 的上界、`max_position≥order_size` 的交叉约束必须**按场所分组**校验，否则 A 场所的单笔金额会去校验 B 场所的敞口 | `validate(settings, venue)`，错误键加 venue 前缀（`pm.order_size`），`FieldErrors` 改成嵌套 |
| `settings.tsx:44-60` | `defaultCategoryOptions`(44-54)、`sportsPolicyOptions`(56-60) | L2：PM 分类与体育 tag 体系 | 选项来自 venueMeta；PF 的分类来自其自身 taxonomy |
| `settings.tsx:137-160` `NumberField` | `unit: string` 自由文本（`USD`/`天`/`分钟`/`bps`/`× 入场价`） | L1 简单：unit 已经是字符串，per-venue 货币只需传 `venueMeta.currency` | 调用点传 venue 单位 |
| `settings.tsx:204-383` `AccountSection` | 账户管理 + 自动交易 + 新建/删除账户 | L2：`hasLive`（`:215`）是「每实例一个实盘账户」；账户创建只有 `mode`（`:355-374`），没有场所 | 创建对话框加「场所」选择；`hasLive` → `hasLive(venue)` |
| `settings.tsx:312` | 「账户模式：真实资金 · **CLOB** / 虚拟资金 · 本地撮合」 | L1/L2：把「CLOB」当成唯一撮合方式的同义词，既没说是哪个场所，也掩盖了 PF 的订单类型差异（LIMIT/MARKET、maker 0 费）；用户会以为两场所的下单模型一致 | 「真实资金 · {venueLabel} · {orderTypes.join('/')}」 |
| `settings.tsx:313` | `$${account.baseline_equity.toFixed(2)}` | **L3**：绕过 `formatCurrency`，硬编码 `$` + 2 位小数 → USDT-18 账户的基准权益被静默截断并标错币种 | `formatCurrency(account.baseline_equity, currency)` |
| `settings.tsx:314-315` | 「交易钱包 (Maker)」/「签名地址」`live_maker`/`live_signer` | L2：PM 的 Maker/Signer 二分；PF 单钱包 | venueMeta.walletRows |
| `settings.tsx:320` | 「请检查 Funder、API 凭据与 **pUSD** 授权」+ `POLYMARKET_PRIVATE_KEY` | L1/L2 | venueMeta |
| `settings.tsx:336` | 说明块：「…下单前需在 **Polymarket** 完成 **pUSD** 授权…每个实例只允许一个实盘账户」 | L1/L2：最后一句在两场所下必须变成「每个 (场所, 模式) 一个」 | venueMeta + 文案修正 |
| `settings.tsx:344` | 实盘自动交易二次确认：「自动提交 **Polymarket** 买单」 | L1 | venueMeta |
| `settings.tsx:385-401` `SettingsView` 状态 | 单 `draft`/`dirty`/`errors`；`isLive` 来自当前账户 | L2：Tab 化后 `dirty` 需按场所跟踪（否则「放弃更改」会丢弃另一 Tab 的编辑）；现在 `load` 的 `if (!dirty) setDraft(...)`（`:403-407`）会把新的 venue tab 数据整体重灌 | `dirty: Record<VenueId \| 'shared', boolean>`；`reset` 只重置当前 Tab（或明确「全部放弃」） |
| `settings.tsx:411-420` `update`/`updateStopLoss` | 扁平 key 更新 | L2 | `update(venue, key, value)` |
| `settings.tsx:429-456` `save` | `toSettings(draft)` → 全量 `PUT /settings` | L2：单次保存必须携带两场所的完整参数（否则后端合并逻辑不清） | 一次 PUT 提交完整 `{shared, venues}`；保存后按返回值重灌（现逻辑 `:443-449` 可复用） |
| `settings.tsx:476-483` 引擎卡 | 全局启停 + `engine.last_error` | L2：两场所后需要按场所状态；「最近错误」单值会让用户误判哪个场所挂了 | 引擎卡加 per-venue 状态行（来自 `Engine.venues`） |
| `settings.tsx:490-509` 市场筛选 | 最低/最高价格 `unit="USD"`、`min_liquidity` `USD`、`max_price_change_24h` `unit="USD"` | **L3 单位错误（现存）**：价格为概率（0–1），`unit="USD"` 是错的标签，`max_price_change_24h` 更是「概率美元」；两场所后这一栏必须按场所各自成组 | Tab 化；unit 改为 venueMeta 的措辞（「价格档位」/`{currency}/份`） |
| `settings.tsx:511-518` 资金与风控 | `initial_bankroll`(513)、`order_size`(514)、`max_position`(515)、`max_exposure`(516) 均 `USD` | L3：`max_exposure` 说明是「全部未平仓头寸的风险预算上限」→ 两场所下若后端跨币种求和，上限会随汇率漂移 | `initial_bankroll` 共享（虚拟本金）；`order_size`/`max_position`/`max_exposure` 按场所 |
| `settings.tsx:520-529` 尾盘窗口 | `entry_window_minutes`/`sports_entry_window_minutes` | L1/L2：可共享；但「体育类」是 PM 分类概念 | 保留共享，或按场所覆盖（推荐共享，减少表单） |
| `settings.tsx:531-549` 止损 | 描述「**Polymarket 没有交易所原生止损单**…」；`:547`「实盘卖单使用 **FOK** 立即成交或全部取消」 | **L2**：PF 只有 LIMIT/MARKET，没有 FOK；这句对 PF 直接错误 | venueMeta 驱动：PM→FOK 文案，PF→MARKET + slippage 文案 |
| `settings.tsx:551-564` 扫描与模拟成交 | 描述「实盘成交价以 **CLOB** 订单簿为准」；`slippage_bps`(554)、`fee_bps`(555) 说明「等效费率 ≈ 类别费率 × (1 − 价格)」；`:558` 提示「**Polymarket** 对多数类别收取 taker 费…建议不低于 10 bps」 | **L2**：`fee_bps` 的等效公式是 PM 专属；PF paper 应按 `feeRateBps × min(p,1−p)`，且 maker 0 费 | `slippage_bps`/`fee_bps` 按场所；提示文案按场所（PM 4–25bps / PF ≈2–10bps of notional） |
| `settings.tsx:566-569` 底部粘性保存条 | 全局 `dirty` + 「保存参数」 | L2：Tab 化后「有尚未保存的更改」必须指明是哪个场所，否则用户在 A Tab 改完切到 B Tab 会看不到脏标记 | 文案带 Tab 名，或每个 Tab 内各自 sticky bar |

### 2.10 系统设置（`views/system.tsx`，1144 行）

| file:line | 现状 | 为什么坏 | 最小改动 |
|---|---|---|---|
| `system.tsx:51-55` `walletSourceLabels` | `polymarket-profile` / `create2-derivation` / `clob-proxy-wallets` | L1：PM 专属来源名 | PM Tab 内保留 |
| `system.tsx:56-57` | `PUBLIC_GAMMA_URL`/`PUBLIC_CLOB_URL` 常量 | L1/L2：默认地址是场所元数据 | 移入 venueMeta.defaults |
| `system.tsx:85-99` `urlError` | 通用 http(s) 校验 | 无 | 复用 |
| `system.tsx:102-108` `effectiveSignatureType`/`walletMode` | `signature_type === 0 ? 'eoa' : 'proxy'` | L2：PF 无 signature type 概念 | PM-only |
| `system.tsx:110-135` `liveValidation` | 私钥 `^0x[0-9a-fA-F]{64}$`、Funder 规则、L2 三件套必须同填或同空 | **L2**：PF 不用 funder/L2 passphrase；且 `POLYMARKET_FUNDER` 写进错误文案（`:121`） | 按场所拆 `validateVenueLive(venue, draft)` |
| `system.tsx:137-195` `EndpointSection` | 「行情与交易接口」；Gamma 地址（`:161`）、CLOB 地址（`:172`）、placeholder 用 `PUBLIC_*`（`:171,178`）、「当前生效」两行（`:174,183`）、「清空为默认值」（`:192-194`） | **L2 核心**：整块只有一对 URL；PF 需要 base_url + testnet + `x-api-key`，且 PF 的「行情」与「交易」是同一 host（`api.predict.fun`） | 改成 venue Tab 内的 `VenueEndpointSection`：PM 保留两栏；PF 一栏 `base_url` + `testnet` 开关 + `api_key` + 默认值链接 |
| `system.tsx:197-665` `LiveTradingSection`（约 470 行） | PM 钱包模式卡（`:399-435`，`:422`「Polymarket 代理钱包」）、自动识别（`:241-337`）、签名类型 Select（`:447-465`）、Funder（`:470-495`，`:492` Polygon RPC）、CLOB L2 凭据（`:537-586`）、Relayer（`:588-634`）、探测结果（`:636-652`，`:641` pUSD）、密钥存储警告（`:654-660`） | **L2 核心**：这一整节是 PM 单场所模型；直接在里面加 `if (venue === 'predictfun')` 会变成不可维护的分支汤（这也是它必须**先拆分**的原因，见 §5） | 拆成 `PmLiveTradingSection`（现状原样迁移）+ `PfLiveTradingSection`（私钥、`api_key`、BSC RPC、钱包类型 EOA/Kernel、连接测试） |
| `system.tsx:225-229` | 状态徽章 `runtime.live_ready ? '凭据本地校验通过' : …` | L2：per-venue 需两套 runtime | 来自 `runtime.venues[venue]` |
| `system.tsx:362-385` `testCredentials` + `:377` | `formatCurrency(result.cash ?? 0)`，toast「Signer … 抵押余额 …」 | L2：PF 的余额是 BSC USDT，`cash` 语义需带 currency | `formatCurrency(cash, currency)`；文案按场所 |
| `system.tsx:667-702` `SystemView` `load()` | 一次 `api.system()`，三个 draft（proxy/data/live） | L2：需要 venues 维度的 draft；`load` 里 `setLiveDraft(current && liveDirty ? …)` 的单脏标记会阻断开场所 Tab | `drafts: Record<VenueId, VenueDraft>` + `dirty: Record<string, boolean>` |
| `system.tsx:678` | `savingSection: 'proxy' \| 'data' \| 'live' \| null` | L2：加场所后需要区分是哪个场所的哪个 section（否则两个 Tab 同时保存会互相覆盖 draft）；这个联合类型是保存并发控制的唯一真相 | `type SaveKey = 'proxy' \| 'data:<venue>' \| 'live:<venue>'` |
| `system.tsx:704-712` `applySaved` | 按 section 重置对应 draft | L2：per-venue 需按 venue+section 重置，且不能覆盖其它 Tab 的未保存编辑 | `applySaved(saved, key)` 用 key 前缀比较 |
| `system.tsx:714-734` `saveSection` | body `{ proxy?, data_source?, live? }` | L2：请求体需 `{ venues: { pm: {...}, pf: {...} } }` 或 `{ data_source: { venue, ... } }` 两种形状之一，必须与后端一次谈定 | 决定：**一次 PUT 带完整 venues map**（最省前端竞态） |
| `system.tsx:759-772` `saveData` | 校验 gamma/clob URL 后保存 | L2：PF 校验 base_url + api_key 必填（若 testnet=false） | `saveData(venue)` |
| `system.tsx:790-798` `saveLive` | `liveValidation` + 保存，成功文案「**CLOB 客户端**已按新凭据重建」 | L2 | 按场所 |
| `system.tsx:826-834` PageHeader | 「集中配置 **Polymarket** 接口地址、实盘交易凭据…」 | L1 | 去场所化 |
| `system.tsx:873-969` 代理 Section | 描述「行情（Gamma）与实盘（CLOB）请求共用同一出口」（`:876`） | L1/L2：PF 走 BSC 与另一个域，出口需求可能不同；至少文案要改，是否需要 per-venue 代理是产品决策 | 文案改为「所有场所请求共用」；若要 per-venue 代理，`ProxyConfig` 也要 venue 维度（成本高，建议 P2） |
| `system.tsx:939-960` 连接测试 UI | 展示 `system?.proxy_probe_url \|\| 'Polymarket CLOB'`（`:952`），后端常量固定 `clob.polymarket.com/time`（`backend/internal/api/system.go:21`） | **L2**：PF-only 部署的「测试连接」测的是 PM 可达性；用户会以为 PF 通 | 探测目标按场所（后端 `proxyProbeURL` 变 map）；UI 显示「将访问 {venue} {target}」 |
| `system.tsx:986-1144` `PasswordSection` | 访问密码 | **无耦合**（确认不用改） | 无 |

### 2.11 外壳 / 登录 / 入口 / 徽章

| file:line | 现状 | 为什么坏 | 最小改动 |
|---|---|---|---|
| `index.html:8` | `meta description`「PennyHarvester — **Polymarket** 高胜率尾盘策略…」 | L1（SEO/分享卡） | 去场所化或列两场所 |
| `index.html:9` | `<title>PennyHarvester · **Polymarket** 尾盘策略工作台</title>` | L1（浏览器标签） | 「多场所尾盘策略工作台」 |
| `app-shell.tsx:58` | 侧栏副标题「**Polymarket** 尾盘策略工作台」 | L1 | 同上 |
| `app-shell.tsx:31-38` `navigation` | 6 个视图，无场所维度 | L2 决策点：场所**不做成导航项**（否则 6 视图 × 2 场所），而做成筛选/作用域 | 导航不动；场所 scope 放视图内部（§3.3） |
| `app-shell.tsx:40-47` `viewNames` | 视图名 | 无 | 不动 |
| `app-shell.tsx:95-107` 模式卡 | 「直接连接 **Polymarket** 真实盘口 / 使用真实 **pUSD** 下单交易」 | **L2**：这是常驻侧栏，说的是当前账户的场所与抵押物 | `当前账户.venue` → venueMeta（`直接连接 {label} 真实盘口` / `使用 {collateral} 下单`） |
| `app-shell.tsx:132-135` `AccountSwitcher` | 列表项 `{item.name} · {实盘\|模拟}` | L2：两场所账户混在一列，只能靠名字区分 | `{name} · {venueShort} · {实盘\|模拟}`；或按 venue 分组（`SelectGroup`，`select.tsx:7` 已导出） |
| `app-shell.tsx:179-183` 面包屑 | 「实盘交易 / {viewName}」 | L1：缺场所 | 「{venueLabel} · 实盘 / {view}」 |
| `app-shell.tsx:186-187` 服务状态 | 只反映 `health.status` | L2：一次 health 只能给聚合状态；两场所下一个挂了仍显示「服务在线」 | 可接受（保留），但若加 `/venues` 健康则在此显示分场所点 |
| `app-shell.tsx:200-210` 实盘未就绪横幅 | 全局一条，文案含 `POLYMARKET_FUNDER`/`POLYMARKET_PRIVATE_KEY`/`pUSD`（`:206`） | **L2**：横幅按 `account.live_ready` 触发；若账号跨场所，PF 未就绪会把 PM 页也刷成红条 | 按 (account, venue) 显示；文案走 venueMeta |
| `app-shell.tsx:215` 页脚 | 「PennyHarvester · **Polymarket** 实盘交易模式 / 模拟演练模式」 | L1 | 场所化或去场所化 |
| `app-shell.tsx:216` | 「页面开启时每 20 秒自动同步最新数据」 | L2 硬编码：实际间隔来自各 `usePolling` 默认值（`usePolling.ts:10`），且订单/账户/健康是 20/30/30s 混合 | 与 infra 常量统一；两场所后语句仍需成立（建议「每 20 秒」保持不变，但把常量抽到 `lib/polling.ts`） |
| `auth-gate.tsx:84` | 登录页副标题「**Polymarket** 尾盘策略工作台」 | L1 | 同步 `index.html`/`app-shell` 的措辞 |
| `status-badges.tsx:13-19` `PositionStatusBadge` | `split` → 「平局退款」 | L2（见 §2.1 `PositionStatus`） | 文案按 venue 或改中性 |
| `status-badges.tsx:21-26` `ModeBadge` | 实盘 LIVE / 模拟 PAPER | **无耦合**（账户模式与场所正交） | 不动 |
| `status-badges.tsx:28-34` `OrderModeBadge` | 手动/自动/止损 × 实盘/模拟 | 无耦合；但 venue 徽章建议放同一文件（新增 `VenueBadge`） | `+ VenueBadge({ venue })` |
| `ui/badge.tsx:5-18` | 7 个 variant：default/success/warning/danger/info/navy/lime | 无耦合；venue chip 复用 `info`/`default` 即可，不需要新 variant（若要品牌色再加两个） | 可选：`+ venue-pm`/`+ venue-pf` |

### 2.12 已确认无耦合（不要动）

- `ui/button.tsx`、`ui/card.tsx`、`ui/dialog.tsx`、`ui/input.tsx`、`ui/select.tsx`、`ui/switch.tsx`、`ui/feedback.tsx`、`page-header.tsx`、`toast.tsx`、`theme-context.tsx`、`theme-toggle.tsx`、`lib/utils.ts`、`lib/theme-context.tsx`、`index.css`（设计 token 只有一组 `--series-equity`，两场所曲线若要区分颜色才需要加 `--series-equity-alt`）。
- `risk-notice.tsx`：只有「价格 0.99 也不代表稳赚」这类概率措辞，两场所通用（`:14,19`）。
- `main.tsx`、`App.tsx`：纯装配。
- `market-language-select.tsx`：语言下拉与场所正交。

---

## 3. (b) 最小多场所 UI 模型

### 3.1 前提决策：账户即场所（推荐），还是账户跨场所

| | 方案 A：账户 = (venue, mode)（架构文档 §3.3） | 方案 B：一个账户跨场所 |
|---|---|---|
| 后端 | `accounts.venue` 列；`(venue,mode)` 唯一 | 账户不变，所有账本加 venue 列 |
| `Metrics`/`Dashboard` | **不用改形状**（一个账户一个场所） | 必须 `by_venue` + currency |
| 现金卡 | 单卡即可，只需把「pUSD」换成场所抵押物 | 必须 per-venue 现金卡（不可求和） |
| 持仓/订单 | 已按 `accountId` 隔离 → 只需 chip 提示 | 需要服务端 venue 过滤 + 汇总列 |
| 雷达 | **仍必须**做 venue 维度（雷达是全局机会，不按账户） | 同 |
| 参数设置 | 必须按场所 | 必须按场所 |
| 系统设置 | 必须按场所 | 必须按场所 |
| 切换方式 | 账户下拉即场所切换（要多一个 `· PM/PF` 后缀） | 需要独立的场所 scope |
| 前端工作量 | ≈ 60% | 100% |

**建议 A**，理由：`positions`/`orders`/`dashboard` 三个视图已经以 `accountId` 为唯一作用域（`positions.tsx:54`、`orders.tsx:74`、`dashboard.tsx:183`），方案 A 让这三个视图**不需要任何求和/聚合逻辑**，也就自动消灭了 §4 里的大部分静默错误。方案 B 需要一次性改完 4 个 DTO + 3 个视图，且必然出现「跨币种求和」的诱惑。

> 注意：即使选 A，「雷达 + 参数设置 + 系统设置 + 总览的系统状态卡 + 扫描摘要」这 5 处仍然必须有场所维度——它们不按账户隔离。

### 3.2 新增 `frontend/src/lib/venue.ts`（唯一真相源）

```ts
export type VenueId = 'polymarket' | 'predictfun'
export type QuoteCurrency = 'USDC' | 'USDT'
export type OrderType = 'FAK' | 'FOK' | 'LIMIT' | 'MARKET'

export interface VenueMeta {
  id: VenueId
  label: string                 // 'Polymarket' | 'predict.fun'
  short: string                 // 'PM' | 'PF'（chip 用）
  currency: QuoteCurrency
  currencyDecimals: number      // 6 | 18
  collateralSymbol: string      // 'pUSD' | 'USDT'
  collateralName: string        // '钱包 pUSD' | '钱包 USDT'
  orderTypes: OrderType[]       // PM: FAK/FOK;  PF: LIMIT/MARKET
  makerFeeFree: boolean         // false | true
  hasProtocolVersion: boolean   // true | false  → 控制 ProtocolBadge
  hasZh: boolean                // true | false  → 控制语言提示
  externalIdLabel: string       // 'CLOB' | 'PF order'
  defaultCategories: string[]   // 分类 chip 的来源
  defaultCategory: string       // normalizeCategoryFilter 的回退值
  settlementHint: string        // 持仓页描述
  envHint: string               // 'POLYMARKET_PRIVATE_KEY' | 'PREDICTFUN_PRIVATE_KEY'
  defaults: { baseURL: string; tradeURL?: string }
  accentClass: string           // chip 配色（复用 ui/badge 的 variant 亦可）
}

export const venueOrder: VenueId[] = ['polymarket', 'predictfun']
export const venues: Record<VenueId, VenueMeta>
export function isVenueId(v: unknown): v is VenueId
// 旧响应兼容：venue 缺失时一律按 polymarket 解释
export function venueOf(x: { venue?: string | null }): VenueId
```

要点：

- `venueOf()` 是**前端先行上线**的关键：后端还没加字段时所有列表按 PM 渲染，行为与今天完全一致。
- 所有硬编码字面量集中到这一个对象；视图里只允许出现 `venues[x].label`，不允许再出现 `'Polymarket'`（可用 ESLint `no-restricted-syntax` 或 CI `grep` 守门）。
- 币种不在 `venueMeta` 里做换算——**永远不做换算**（见 §4.1）。

### 3.3 场所作用域（scope）：`lib/venue-scope.tsx`

```tsx
export type VenueScope = VenueId | 'all'
interface VenueScopeValue {
  scope: VenueScope
  setScope: (s: VenueScope) => void
  enabled: VenueId[]              // 来自 GET /venues（未实现时 = venueOrder）
}
```

- 持久化键：`localStorage['pennyharvester.venue-scope']`（与账户键分离，`account-context.tsx:6` 那一套不动）。
- Provider 挂在 `App.tsx:22`（`AccountProvider` 内侧），与账户上下文并列；**不要**塞进 `AccountContext`，两者生命周期/持久化键不同。
- 默认值：`enabled.length === 1 ? enabled[0] : 'all'`（只有一个场所时不显示 chip 行，避免 PM-only 用户看到无意义 UI）。
- **关键实现约束（来自 `usePolling.ts:22-28`）**：视图内部的 fetcher 不要直接把 `scope` 放进 `useCallback` 依赖。两种安全写法：
  1. fetcher 保持稳定（`useCallback([accountId], …)`），内部用 `scopeRef.current` 读取最新 scope，并手动 `refresh()` —— 但这样服务端过滤时每次切换都要等一次请求且保留旧数据（`isRefreshing` 而非 `isLoading`），体验最好；
  2. 给 `usePolling` 加 `keepPreviousData` 选项，允许 fetcher 依赖 scope，但切换时不清空。
- 推荐 1（额外好处：scope 只是「视图可见性」，请求参数可以滞后一拍，避免快速点 chip 时打出一串请求）。

### 3.4 组件级改造（按视图）

**A. `components/venue-filter.tsx`（新，约 40 行）**

```tsx
export function VenueFilter({ value, onChange, counts, health }: {
  value: VenueScope
  onChange: (v: VenueScope) => void
  counts?: Partial<Record<VenueId, number>>
  health?: Partial<Record<VenueId, 'ok' | 'degraded' | 'down'>>
})
```

- 视觉与交互**完全复刻**现有分类 chip（`opportunities.tsx:371-381` 的 `aria-pressed` + 圆角 pill），放在分类 chip 行**上方**独立一行，避免 8 → 16 个 chip 挤在一行（`opportunities.tsx:366-383`）。
- `counts` 用于显示各场所机会数；`health` 用于在 agent 挂掉时把 chip 变琥珀色（解决 §2.4「非场所健康不可见」）。
- 只有 `enabled.length > 1` 时渲染；PM-only 部署零变化。

**B. 雷达（`opportunities.tsx`）**

- `scoped = allItems.filter(venueMatches(scope))` → `counts = categoryCounts(scoped)` → `filtered`（顺序：venue → category → keyword）。修 §2.3/§2.6 的计数错位。
- `heldByToken` / `quotes` 键改 `${venue}:${token_id}`（§4.5）。
- `VenueBadge` 进「市场/方向」单元格（桌面 `:417`，卡片 `:166`）。
- `liveReady` 改成逐行：PM 行用 PM 就绪，PF 行用 PF 就绪（`item.venue`）；底部全局 banner（`:499`）改成「该场所未就绪」的行内提示。
- 报价按钮金额：`orderSizeFor(item.venue)`。

**C. 总览（`dashboard.tsx`）**

- 方案 A：`MetricsCards` 只需把 `钱包 pUSD`（`:160`）换成 `venues[account.venue].collateralName`；`USD`（`:89`）换 `metrics.currency ?? venueMeta.currency`。
- 方案 B（若坚持聚合）：新增 `PerVenueCashCard`，结构固定为「每个场所一行，永不舍入到同一行」：

```tsx
{venueOrder.map(id => (
  <div key={id}> {/* 不用 flex-1 合并 */}
    <span>{venues[id].label}</span>
    <span>{formatCurrency(m[id].cash, venues[id].currency)}</span>
    <span>{venues[id].collateralSymbol} {formatCurrency(m[id].wallet_cash, venues[id].currency)}</span>
  </div>
))}
```
- 缺数据时显示 `—`（而不是 0），避免「USDT 卡显示 $0.00」这种静默误导。
- 「系统状态」卡（`:210-248`）：引擎行下方插入 per-venue 健康行；`行情数据源`（`:228-231`）改 `sources.map`。
- 最近订单表（`:259-263`）复用 `orderTypeLabel`（修现存 sell→结算 bug），加 venue chip。

**D. 持仓 / 订单**

- venue chip 进第一列单元格（`positions.tsx:77`、`orders.tsx:112`），**不新增列**（`min-w-[1080px]`/`[1120px]` 已经接近可读上限）。
- 订单的 venue 过滤走 query（`api.orders(..., venue)`），不要 client-side（§4.7）。
- `orderTypeLabel` 增加 venue：PF 显示 `LIMIT/MARKET`。

**E. 参数设置（见 §3.5）**

### 3.5 参数设置：venue Tab + 共享区

结构（拆分后，见 §5）：

```
SettingsView
├─ EngineCard                       // 全局；加 per-venue 状态行
├─ AccountSection                   // 账户即场所；新建时选场所
├─ SharedSettingsSection            // scan_interval / initial_bankroll / entry windows / stop loss
└─ VenueSettingsTabs                // Tab: PM | PF（enabled 才渲染）
   └─ VenueSettingsForm(venue)
      ├─ 市场筛选（min/max price、min_liquidity、default_category、sports_policy、max_price_change_24h）
      ├─ 资金与风控（order_size、max_position、max_exposure）
      └─ 模拟成交（slippage_bps、fee_bps）
```

- **共享 vs 按场所的判定标准**：影响「扫描节奏 / 虚拟本金 / 平仓保护」的 → 共享；影响「选哪个盘口 / 下多少钱 / 按什么费率撮合」的 → 按场所。
- Tab 切换只改「渲染哪个 venue 的 draft」，**不能触发** `setDraft(null)`；`dirty` 用 `Record<VenueId | 'shared', boolean>`，底部保存条显示「PM 有未保存更改」。
- 保存仍是一次 `PUT /settings`，body 为 `{ ...sharedFields, venues: { polymarket: {...}, predictfun: {...} } }`；前端负责把后端返回的 `venues` 重新灌入（替换 `settings.tsx:403-407` 的单 draft 逻辑）。
- `validate` 逐 venue 跑，错误键带前缀（`pm.order_size`），`FieldErrors` 变 `Record<string, string>`（现有 `Partial<Record<NumericKey | SelectKey, string>>` 保留给 shared）。
- **不要**把 tab 状态放进 URL/路由——项目没有 router（`App.tsx:12-18` 用 `useState<View>`），引入 router 是超范围。

### 3.6 系统设置：venue Tab（必须先拆分 §5.1）

```
SystemView
├─ VenueTabs (PM | PF)
├─ VenueEndpointSection(venue)     // PM: gamma+clob；PF: base_url+testnet+api_key
├─ VenueLiveSection(venue)         // PM: 现状原样；PF: 私钥+api_key+BSC RPC+钱包类型
├─ ProxySection                    // 共享（文案去场所化）
└─ PasswordSection                 // 共享，不动
```

- 保存并发控制：`savingSection` 从 `'data'|'live'` 变成 `` `data:${VenueId}` | `live:${VenueId}` | 'proxy' ``；`applySaved` 只重置对应 key 的 draft（否则在 PF Tab 保存会清空 PM 的未保存编辑）。
- 连通性测试：「测试凭据」（`:362-385`）与「测试连接」（`:939-960`）都必须显示「将访问 {venue label} {target}」，并把 `proxy_probe_url` 改成 per-venue。
- PM 专属的 Relayer 与钱包自动识别（`:241-337,588-634`）只剩在 PM Tab 内，**不要**为 PF 强行复用。
- PF 的 `testnet` 开关应带显著的「测试网」徽章，避免用户以为在真实 BNB 主网下单。

### 3.7 下单票据（order ticket）：唯一必须新增的交互

现状买入对话框（`opportunities.tsx:458-508`）只有「金额」一个输入，订单类型完全隐式。PF 需要显式类型，最小增量：

```tsx
// OrderTicketFields({ venue, value, onChange })
// PM: 只显示金额（默认 FAK，文案沿用现状）
// PF: 类型 Segmented [LIMIT | MARKET]
//     LIMIT  → 价格输入（默认 = 当前指示价）+ 金额
//     MARKET → 金额 + 滑点上限 bps（默认取 venue 默认值），文案说明「可能以更差价格成交」
```

- `createOrder` body（`api.ts:170-174`）同步扩展：`{ venue, market_id, outcome, token_id, amount, order_type?, limit_price?, slippage_bps? }`。
- 确认文案（`:497`）必须包含「{venue} · {order_type} · {currency}{amount}」，否则用户无法在确认页复核。
- PF 的 maker 策略（架构文档结论：PF 应做 maker）在 v1 **不要**做 UI 支持：只做 taker 的 LIMIT/MARKET 选择，maker 挂单/撤单是独立功能（`SupportsCancel`）。

### 3.8 需要新增的 API 能力（前端视角的最小集合）

| 能力 | 形状 | 前端消费点 |
|---|---|---|
| `GET /venues` | `{ items: [{ venue, label, currency, enabled, order_types, live_ready, live_error, healthy, last_scan_at }] }` | `VenueScope` provider、`VenueFilter` 健康点、`settings` 的 tab 列表 |
| 列表 `?venue=` | `opportunities`/`positions`/`orders`/`scan` | §3.4 |
| `Dashboard.by_venue`（可选） | 方案 B 才需要 | 总览现金卡 |
| `Settings.venues` | `Record<VenueId, VenueSettings>` | 参数设置 Tab |
| `SystemSettings.venues` | `Record<VenueId, { base_url, trade_url, testnet, live_environment, live_ready, live_error }>` | 系统设置 Tab |
| `POST /accounts { venue }` | 账户创建带场所 | `settings.tsx:355-374` |

---

## 4. (c) 静默算错的地方（L3 清单）

> 判定标准：不抛错、不告警、UI 数值看起来正常，但结果错误。

1. **跨币种相加（任何地方求和 `cash`/`equity`/`exposure`/`fees`）**
   `types.ts:37-46` + `dashboard.tsx:156,172`。USDC 与 USDT 是两种资产，不同链；前端**没有**任何换算层（`format.ts` 全文无 FX）。一旦后端返回聚合值，前端会用 `$`（`format.ts:1-6,17-21`）渲染，用户无法察觉这是两种币的混合。

2. **净值曲线单序列**
   `dashboard.tsx:89` 写死 `USD`；`types.ts:68-71` 的 `value` 无单位；图表只有一条 `dataKey="value"`（`dashboard.tsx:131-132`，`Area` 与 `Line` 共用同一 dataKey，颜色也共用 `--series-equity`）与一个 Y 轴 formatter（`dashboard.tsx:129` → `formatAxisCurrency`，`format.ts:27-35`）。若快照按场所混合，曲线形状会包含两种币的相对波动，`total_return`（`dashboard.tsx:152`）随之变成无意义的百分比——甚至可能仅因 USDT/USDC 溢价变化而显示「盈利」。

3. **`fees_paid` 语义冲突**
   `dashboard.tsx:168`。PM 的 taker 费按类别费率 × `min(p,1−p)` 计（settings 注释 `settings.tsx:555` 也承认「等效费率 ≈ 类别费率 × (1 − 价格)」），PF 是每市场 `feeRateBps`（实测 200）× `min(p,1−p)`，且 **maker 0 费 + 最高 30% rebate**。把三者加进一个 `fees_paid` 会让「手续费」既不是 PM 口径也不是 PF 口径；rebate 是负费用，若被加进去会让总费用变小而账面现金并未同步。

4. **雷达「真实收益」并排可比**
   `opportunities.tsx:116-144` + `types.ts:128-149`。PM 用 FAK 走卖单簿、`limit_price` 是可用最高限价；PF 用 `MARKET + slippageBps + isMinAmountOut`，实际成交可能劣于报价。两个 `net_yield` 数字在 UI 上完全同构（`:486`），用户会横比——但 PF 一侧是乐观估计。同理 `fully_filled`/`available_notional`（`:489`）在 PF 上语义不同。

5. **`token_id` 单键 Map（真·错数据，不只是汇总）**
   `opportunities.tsx:64-85`（`heldPositionsByToken`）与 `:208`（`quotes`）。PM 的 `token_id` 是 `clobTokenIds` 里的十进制 ERC-1155 id；PF 的 `onChainId` 也是 ERC-1155 id，且很可能是小整数。键空间重叠 ⇒ 一个场所的未平仓持仓会点亮另一场所的「已买入」标记，并显示对方的买入价（`:470`），报价也会串（`:420,481-482`）。**必须在接入第二个场所之前修**，否则是纯粹的错误信息。

6. **场所过滤后 chip 计数**
   `opportunities.tsx:248` 用 `allItems` 算 `categoryCounts`，`:249-256` 才过滤。若 venue 只加进 `filtered`，chip 显示「体育 120」而可见行只有 30 条。

7. **「共 N 个 / 共 N 条」与分页**
   `opportunities.tsx:362` 的 `data.total`、`orders.tsx:133-139` 的 `data.total` 都是服务端全局值。client-side venue 过滤后：雷达「共 240 个」但只显示 120；订单页每页显示不足 100 条却仍说「共 240 条」，且「下一页」会在错误的偏移处禁用（`:138`）。必须服务端过滤。

8. **单笔金额与敞口上限跨场所比较**
   `settings.tsx:514-516` 一处 `order_size`、一处 `max_exposure`。PF 最小下单约 1 USDT、PM 可按 0.01 级下单；用同一数字给两场所报价（`opportunities.tsx:240,389,482`）会产生看似可比、实则费率/最小额都不同的「真实收益」。`max_exposure` 若后端跨币种求和，风控上限会随稳定币价差漂移。

9. **份额精度截断**
   `positions.tsx:35,77`、`orders.tsx:119`、`opportunities.tsx:470` 都是 `formatNumber(x, 4)`。PF 18 位最小单位下，小额持仓会显示 `0.0000 份`，而「已买入 0.0000 份 · 成本 0.50 USDT」自相矛盾；PM 6 位精度下同样可能截断到 0。

10. **基准权益硬编码格式**
    `settings.tsx:313` 的 `` `$${account.baseline_equity.toFixed(2)}` `` 绕过 `formatCurrency`。USDT 账户的基准权益被显示为 `$`、2 位小数（静默四舍五入），后续 `total_return` 也以它为分母（后端），前端展示与计算口径不一致。

11. **`max_price_change_24h` 的单位标签**
    `settings.tsx:500` `unit="USD"`（实际是概率变动 0–1）。两场所若只有一方提供该指标（PF 未必有 24h 变化），同一「0 表示不限制」的设置在一侧静默失效。

12. **扫描摘要的「N 个盘口已下单」不分场所**
    `opportunities.tsx:300-310`：`result.total` 与 `orders_placed` 都是跨场所聚合，toast 无法告诉用户自动买入发生在哪个场所——而这是真金白银的动作。

13. **总览「最近订单」把卖出显示为「结算」（现存 bug）**
    `dashboard.tsx:262`：`order.type === 'buy' ? 买入 : '结算'`。`Order.type` 含 `'sell'`（`types.ts:195`），止损卖出单会显示成「结算」；`orders.tsx:30-34` 是正确实现。两场所后会再加一层「LIMIT/MARKET」混淆。

14. **`split` 状态跨场所语义**
    `status-badges.tsx:17`「平局退款」。若 PF 的 resolver 产生「无效/取消 → 部分退款」的持仓，用 `split` 表示并显示「平局退款」是错的（应为「无效市场退款」）。

---

## 5. (d) 需要拆分的文件

> 只列已完整通读的文件。行数为 `wc -l` 实测。拆分原则：**先纯机械搬运（不改行为、一个 commit），再做 venue 改动**；否则在 1144 行的文件里插分支会不可评审。

### 5.1 `views/system.tsx`（1144 行）— 必须先拆

| 目标文件 | 迁移范围（现行号） | 说明 |
|---|---|---|
| `views/system/section.tsx` | `Section` 59-83、`urlError` 85-99 | `EndpointSection`/`LiveTradingSection`/代理/`PasswordSection` 四处共用，先下沉 |
| `views/system/pm/live-trading-section.tsx` | `LiveTradingSection` 197-665 + `effectiveSignatureType` 102-108、`walletMode` 106-108、`liveValidation` 110-135、`walletSourceLabels` 51-55、`REDACTED_SECRET` 50 | 整块原样搬运，**不改一行逻辑** |
| `views/system/pm/endpoint-section.tsx` | `EndpointSection` 137-195 + `PUBLIC_*` 56-57 | PM 专用；PF 版本另建 |
| `views/system/proxy-section.tsx` | `SystemView` 内 873-969 的代理 Section + `modeLabels` 44-48 + `testProxy` 806-823 | 共享，文案先去场所化 |
| `views/system/password-section.tsx` | `PasswordSection` 986-1144 | 纯搬迁 |
| `views/system/index.tsx` | `SystemView` 667-871（PageHeader/加载态/三个 section 编排）+ 971-984（`PasswordSection` 渲染与收尾） | 只保留 draft/dirty/save 编排与 Tab |
| `views/system/types.ts` | `SaveKey` 联合类型（现 `:678`） | 收敛保存并发控制 |

拆分后 `index.tsx` 预计 ~180 行，`pm/live-trading-section.tsx` ~500 行（仍然大，但它是**单一职责**：PM 实盘凭据；PF 那份是独立文件，不会继续膨胀）。

### 5.2 `views/settings.tsx`（576 行）

| 目标文件 | 迁移范围 |
|---|---|
| `views/settings/schema.ts` | 37-135（keys、`Draft`、`toDraft`、`toSettings`、`validate`、选项常量 44-60） |
| `views/settings/account-section.tsx` | 204-383 |
| `views/settings/shared-section.tsx` | 引擎卡 476-483 + 尾盘窗口 520-529 + 止损 531-549 + 扫描与模拟成交 551-564 中**共享**部分 |
| `views/settings/venue-section.tsx` | 市场筛选 490-509 + 资金与风控 511-518 + 按场所的费率/滑点 |
| `views/settings/index.tsx` | 385-475 + 485-489 + 566-576（编排、保存条） |

### 5.3 `views/opportunities.tsx`（511 行）

| 目标文件 | 迁移范围 |
|---|---|
| `opportunities/held.ts` | `HeldPosition` 54-62、`heldPositionsByToken` 64-85、`heldTitle` 87-89、`HeldBadge` 91-98、`HeldPrice` 100-103 |
| `opportunities/quote.tsx` | `QuoteState` 48-52、`quoteAge` 105-114、`RealYield` 116-144 |
| `opportunities/badges.tsx` | `ProtocolBadge` 150-160（→ `VenueBadge` + `ProtocolBadge`） |
| `opportunities/opportunity-row.tsx` | 桌面行 415-425 + `OpportunityCard` 162-190 + `MarketLink` 39-46 |
| `opportunities/buy-dialog.tsx` | 458-508（venue/order-type 感知的票据） |
| `opportunities/scan-dialog.tsx` | 434-456 |
| `opportunities/index.tsx` | 192-433 的状态与列表装配、430-432 的双列表分支、434-509 的两个对话框、510 收尾 |

### 5.4 `types.ts`（353 行）

按域拆为 `types/account.ts`（1-79：`AccountMode`/`Account`/`AccountsResponse`/`Health`/`Metrics`/`Engine`/`Source`/`EquityPoint`/`Dashboard`）、`types/market.ts`（81-149：`Opportunity`/`ScanAutoTrade`/`OpportunitiesResponse`/`OpportunityQuote`）、`types/ledger.ts`（151-211：`Position`/`Order`）、`types/settings.ts`（213-245：`Settings`/`ProxyConfig`/`DataSourceSettings`）、`types/system.ts`（247-353），并由 `types.ts` **re-export**：

```ts
export * from './types/account'
export * from './types/market'
…
```

这样 `@/types` 的 20 多个 import 全部零改动（`api.ts:1-25` 是最大消费者）。理由：`types.ts` 一旦加上 `venue`/`currency`/`unsupported`/`order_type`，会再长 60–80 行，且四块域的变更节奏完全不同。

### 5.5 `views/dashboard.tsx`（274 行）

| 目标文件 | 迁移范围 |
|---|---|
| `dashboard/metrics-cards.tsx` | `StatCard` 44-64 + `MetricsCards` 142-179 |
| `dashboard/equity-chart.tsx` | `EquityTooltip` 66-78 + `EquityChart` 80-140 |
| `dashboard/status-card.tsx` | 210-248 |
| `dashboard/orders-preview.tsx` | 251-271（最近订单卡，含移动端分支 266-268） |
| `dashboard/index.tsx` | 181-208（`DashboardView` 头部与三卡布局）+ 249-274（收尾） |

### 5.6 不需要拆

`views/orders.tsx`(146)、`views/positions.tsx`(86)、`components/app-shell.tsx`(223)、`lib/*`（最大 `api.ts` 175）、`components/ui/*`（均 < 100）。它们是「加一个 chip / 一个参数」的量级，拆开反而增加跳转成本。

### 5.7 拆分顺序（避免长命分支）

1. `types.ts` re-export 拆分（零行为变化）；
2. `system.tsx` 机械拆分（零行为变化）；
3. `settings.tsx` 机械拆分（零行为变化）；
4. `lib/venue.ts` + `VenueBadge` + `VenueFilter`（新文件，无侵入）；
5. 雷达 venue 过滤 + `token_id` 键修复（此处才有第一个行为变化）；
6. 参数设置 Tab、系统设置 Tab；
7. 订单票据（LIMIT/MARKET）。

---

## 6. 与 `docs/multi-venue-architecture.md` §4.2 的差异

该文档的前端表方向正确（venue 元数据、雷达 Tab、持仓/订单文案、总览分场所现金、参数/系统 Tab、`market-language` 去硬编码、外壳文案）。本审计补充它**没有提到**的 6 项：

1. `usePolling.ts:22-28` 的 fetcher 身份清空语义 —— 直接决定筛选 Tab 的实现方式（§2.4、§3.3）。
2. `token_id` 单键 Map 冲突（§4.5）—— 该文档完全未覆盖，且是风险最高的一条静默错数据（不报错、只显示错信息）。
3. 订单分页与「共 N 条」在 client-side 过滤下的失真（§4.7）。
4. 总览把 `sell` 显示成「结算」（§4.13）—— 现存 bug，与 venue 无关但会在改造中被放大。
5. `formatPrice` 给概率加 `$`、`formatPriceChange` 的 `¢`、`formatNumber(shares,4)` 的 18 位精度问题（§2.3、§4.9）。
6. `Intl.NumberFormat` 对 `'USDT'` 的 `currencyDisplay` 行为需显式固定（§2.3 第一行）——不建议直接 `currency:'USDT'` 依赖默认符号。

另外该文档建议 `frontend/src/types.ts:80-330` 「所有 DTO 加 `venue`/`currency`/`unsupported`」——本审计建议**分级**：`Opportunity`/`Quote`/`Position`/`Order` 必加；`Metrics`/`Dashboard` 若采用「账户即场所」可只加 `currency`；`LiveProbeResult`/`FunderDiscoveryResult` 反而应标记为 PM-only 而非泛化。

---

## 7. 未决问题（需要先定契约）

1. **账户是否绑定场所**（方案 A vs B，§3.1）。这是所有 DTO 形状的上游决策，建议本周定。
2. **`scan` 的场所语义**：扫全部启用场所，还是只扫当前 scope？影响 `ScanAutoTrade`（`types.ts:110-116`）是否要 `by_venue`。
3. **`Order.type` 的扩展方式**：新增并列字段 `order_type: 'LIMIT'|'MARKET'|'FAK'|'FOK'`，还是把 `type` 重命名为 `side` 再用 `order_type`？（后者是破坏性变更，涉及后端 `settle` 语义。）
4. **PF 的 `end_date` 缺失**（架构文档 §1：`endsAt` 在 category 上）——前端需要 `unsupported` 原因字符串的稳定枚举，以便显示「无到期时间，已排除」而不是空白行。
5. **USDT 的展示符号**：`USDT` / `₮` / `$`？建议直接用 `USDT`（避免误认美元）。
6. **是否需要在 UI 暴露 PF 的 maker 策略**（挂单/撤单）。v1 建议不做，但确认后 `Capabilities` 里的 `SupportsCancel` 才能决定 `Order` DTO。
7. **`proxy_probe_url` 是否 per-venue**（后端常量 `system.go:21` 的改造）。

---

## 附录 A：审计覆盖文件（全部通读）

`frontend/index.html`(28)、`vite.config.ts`(28)、`tailwind.config.js`（前 80 行：颜色/token 与 `darkMode`）、`src/main.tsx`(16)、`src/App.tsx`(34)、`src/types.ts`(353)、`src/index.css`(188)、`src/hooks/usePolling.ts`(92)、`src/lib/api.ts`(175)、`src/lib/format.ts`(109)、`src/lib/market-language.ts`(100)、`src/lib/category.ts`(63)、`src/lib/account-context.tsx`(73)、`src/lib/theme-context.tsx`(54)、`src/lib/utils.ts`(6)、`src/views/dashboard.tsx`(274)、`src/views/opportunities.tsx`(511)、`src/views/positions.tsx`(86)、`src/views/orders.tsx`(146)、`src/views/settings.tsx`(576)、`src/views/system.tsx`(1144)、`src/components/app-shell.tsx`(223)、`src/components/auth-gate.tsx`(119)、`src/components/status-badges.tsx`(34)、`src/components/page-header.tsx`(35)、`src/components/risk-notice.tsx`(26)、`src/components/toast.tsx`(58)、`src/components/theme-toggle.tsx`(30)、`src/components/market-language-select.tsx`(36)、`src/components/ui/{badge,button,card,dialog,feedback,input,select,switch}.tsx`(24/50/29/46/61/24/94/20)。

后端只读核对：`backend/internal/api/router.go`（路由与 query 解析）、`backend/internal/api/system.go`（`proxyProbeURL` 常量）、`backend/internal/engine/types.go`（`Settings`/`Opportunity`/`Position`/`Order`/`Metrics` 结构，用于确认前端 DTO 与后端一致且均无 venue）。

未改动任何代码；本文档为唯一新增文件。
