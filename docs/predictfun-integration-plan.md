# predict.fun 接入方案（架构 + 前后端改造）

> 状态：**已实施**（2026-11，M0–M3 全部落地并通过 `go test ./... -race` 与真机联调；2026-11-03 补齐自动结算与链上赎回，见 §12–§14；仍未做：代理钱包签名、合并/转换、WebSocket 盘口）。
> 本文是施工前的设计记录，取代 `docs/multi-venue-architecture.md` 中"六边形重写"的部分（该文件已删除）。实际落地与本文的差异逐条列在 §9。
> 接口契约以 `docs/venue-api-contract.md` 为准；结论先看 §0。

## 0. TL;DR

1. **不要重构成六边形。** 仓库里 `backend/internal/{adapters,domain,platform,ports,execution,service,strategy,app}` 全是空目录（0 个 `.go`），说明那份设计只落地了目录、没落地代码。正确做法是**在现有包结构上加一层薄接口**：新增 `backend/internal/venue`（接口 + 注册表）与 `backend/internal/predictfun`（适配器），把现有 Polymarket 代码按同一接口收口。改动面约为六边形方案的 40%，回归风险低得多。
2. **三条硬约束决定整体设计**：
   - **两条链、两套钱**：Polymarket=USDC(Polygon)，predict.fun=USDT(BNB Chain 56)。本金不可相加，必须**分账 + 折算**，不能出现"总可用 100 USDC"这种把两种币混起来的数字。
   - **PF 费率是价格的函数**：`taker fee = feeRateBps × min(p, 1−p) × shares`，maker=0。当 `p<0.5` 时 `fee/notional = feeRateBps = 2%`，与价格无关。对"低价捡币"策略等于**恒定 2% 的额外成本**——不把它扣进 edge，所有 <10¢ 的机会都是假机会。
   - **PF 只有一条盘口**：`GET /v1/markets/{id}/orderbook` 只回 Yes（indexSet=1），No 侧必须 `1−p` 反向推导且深度方向要对调；`decimalPrecision` 逐市场（0.01 / 0.001），最小单 $1，**下单签名必须在重试循环之外**（否则重试会铸新 salt → 重复下单）。
3. **落地分成 5 个里程碑**，M0 是纯重构（行为不变、全测试绿），M1 只读扫描，M2 才开真实下单。详见 §5。

## 1. 现状与耦合点

> 完整矩阵见 `docs/research/backend-venue-coupling.md`（532 行，~90 行 `file:line` 证据）与 `docs/research/frontend-venue-coupling.md`（643 行）。`grep -rn venue backend frontend/src` → **0 命中**。

真正挡路的不是"加一个客户端"，而是**三件耦合的事**：

1. **单值装配**：`Engine.provider market.Provider` / `Engine.live LiveExecutor` 是标量（`backend/internal/engine/engine.go:19-20`，注入点 `engine.go:40`、`cmd/server/main.go:57-70`），调用链里没有任何 venue 键。执行器接口本身长着 Polymarket 的脸：`LiveExecutor.Buy/Sell/LookupOrder/EstimateBuyFee/Quote` 都收 PM 的 `conditionID`，因为它就是 `client.FeeSchedule(ctx, conditionID)` 的键（`engine/types.go:769-788`，`live/live.go:520/621/728/762/888`）。
2. **标识是裸字符串**：扫描用 `map[string]market.Market` 以 Gamma `market.ID` 为键（`engine/engine.go:595-597`），对账/刷新用 `order.MarketID`/`position.MarketID`（`engine.go:751/1467`），自动下单去重用 `orderedMarkets[opportunity.MarketID]`（`engine.go:852-861,928-937`）。两个场所会在**每一个**这类 map 里碰撞。
3. **钱是 float64，且抵押品写死 6 位小数**：`live/clob_l2.go:177` 与 `live/live.go:1126` 都是 `units / 1_000_000`；`shareScale=10_000`（`live.go:811`）、成交价 `*1e6` 取整（`live.go:836`）、`parseAmount` 解析失败**静默返回 0**（`live.go:1162-1167`）；全仓库没有 decimal/big.Rat 金额类型，SQLite 列全是 `REAL`。PF 的 USDT 是 18 位小数，**差 1e12**。

其余六条（同样致命但不是最难）：schema 无 venue/currency（`store/sqlite.go:79-127`）；设置是单场所结构（`engine/types.go:217-222/226-229/273-280`，探活 URL 是编译期常量 `api/system.go:20`）；**结算靠 Gamma 的 outcome 价格推断**（`market/market.go:71-123` 的 0.999/0.001、0.499–0.501 规则），PF 走自己的 resolver，PF 持仓会**永远不结算**且不报错（`engine/engine.go:744 refreshPositions` 从不问执行器）；进程内只允许一个实盘账户（`engine.go:473-476`）；整轮扫描串行在一把 `tradeMu` + 90s deadline 下（`engine.go:577,108`）——PF 上游一慢会拖死 PM 扫描。

**手数统计**：仅标 `HARD` 的就有 31 处、10 个文件（`engine.go` 14、`market.go` 4、`types.go` 4、`main.go` 3，其余 1 处各）。

`docs/multi-venue-architecture.md` §4.1 的 12 处行号引用**全部对不上**（见 §6），不能当施工图用。

## 2. 目标架构

### 2.1 分层与依赖方向

保持现有包，只加两个包、一个接口：

```
internal/api        HTTP 层（新增 venue 过滤参数）
internal/engine     策略/风控/决策（不感知具体场所，只认 venue.Adapter）
internal/venue      接口 + 注册表 + 领域枚举（新增）
internal/predictfun PF 适配器：REST/鉴权/签名/精度/订单（新增）
internal/live       现有 Polymarket 执行器 → 实现 venue.Trading（改名不动）
internal/market     现有 Polymarket 行情 → 实现 venue.MarketData
internal/store      SQLite（加 venue/currency 维度）
internal/config     按场所命名空间化
```

依赖方向：`api → engine → venue(接口) ← predictfun / live / market`。适配器只依赖 `venue`，**不依赖 engine、不依赖 store**。

### 2.2 接口草案

```go
package venue

type ID string // "polymarket" | "predictfun"

type Currency string // "USDC" | "USDT"

// 行情：无需私钥即可运行（扫街模式）
type MarketData interface {
    Venue() ID
    Currency() Currency
    ListOpenMarkets(ctx context.Context) ([]market.Market, error)
    GetMarket(ctx context.Context, id string) (market.Market, error)
    // Book 返回"原生方向"盘口：调用方要 No 时请用 AskFor/Outcome 参数，
    // 由适配器决定是原生查询还是 (1-p) 反演 + 深度对调。
    Book(ctx context.Context, marketID string, outcome market.Outcome) (market.Book, error)
}

// 交易：需要私钥
type Trading interface {
    Venue() ID
    Account(ctx context.Context) (Account, error) // cash + positions（本场所）
    Place(ctx context.Context, req OrderRequest) (OrderAck, error)
    Cancel(ctx context.Context, orderID string) error
    OpenOrders(ctx context.Context) ([]OrderAck, error)
}

// 手续费模型（引擎算 edge 用）
type FeeModel interface {
    // TakerFee 返回按名义金额计的费率（PM=0，PF=min(p,1-p) 折算后 * feeRateBps/1e4）
    TakerFee(price float64, shares float64) float64
}
```

`OrderRequest` 携带 `MarketID / Outcome / Side / Price / Shares / ClientOrderID / TimeInForce`；`Price` 用**已按该市场 tick 量化后的值**，量化责任在引擎（见 §3.6），适配器只做单位换算与最终校验。

### 2.3 领域模型变更

| 字段 | 现在 | 改为 | 说明 |
|---|---|---|---|
| `market.Market.Venue` | 无 | `venue.ID` | 唯一真源，所有下游从它取 |
| `market.Market.URL` | 硬编码 `polymarket.com/event/` | 由适配器提供 | 现在 `backend/internal/market/market.go:57-62` 写死 |
| `market.Market.TickSize` | 隐含 0.01 | 每市场显式 | PF：`10^-decimalPrecision`（0.01 或 0.001） |
| `market.Market.MinShares` | 无 | 显式（可 0） | PF：`≥1 USDT 且 ≥1 share` |
| `FeeRate` | 无 | `venue.FeeModel` | PF `feeRateBps=200` 来自市场字段，不要写死 |
| `Currency` | 无 | `venue.Currency` | 用于分账与 UI 标注 |

金额单位约定：**引擎内部一律用 float64 的名义 USDC/USDT 与股数**（与现状一致，不引入 decimal 库）；**精度只在适配器边界处理**（PF 出站 ×1e18 → uint256 string，入站 /1e18）。

### 2.4 资金与风险分区

现状 `Engine` 只有一个 `provider`/`live` 字段（`backend/internal/engine/engine.go:19-31`）与一份 `settings`。改为：

```go
type Engine struct {
    venues  map[venue.ID]venueBundle // bundle = MarketData + Trading + FeeModel
    risk    RiskBook                 // 跨场所敞口聚合
    ...
}
```

风控三层，**必须显式分开**：

1. **场所内**（per-venue cash bucket）：`MaxExposure`/`MaxPosition` 按币种独立计算，复用现有逻辑（`engine.go:1263-1359` 那几条 cost/cash/exposure 判定），只是把 `snapshot.Cash` 换成 `snapshot.Cash[currency]`。
2. **全局**（折算后）：新增 `MaxTotalExposureUSD`，把两场所按 1:1（USDT≈USD，可配汇率）折算求和；超限只拒绝**新开仓**，不阻断减仓/止损。
3. **场所限额**：`MaxOrderUSDPerVenue`、`DailyLossPerVenue`——PF 初期小额试水，避免一个新适配器的 bug 打穿本金。

**权益快照**双轨：`equity_snapshots` 增加 `venue` + `currency`，历史数据回填 `polymarket/USDC`；UI 上分别画两条曲线，另加一条折算合计。

### 2.5 SQLite 迁移

沿用现有幂等迁移模式（`backend/internal/store/sqlite.go:141-147` + `positions_new` 重建）：

```sql
ALTER TABLE positions        ADD COLUMN venue TEXT NOT NULL DEFAULT 'polymarket';
ALTER TABLE orders           ADD COLUMN venue TEXT NOT NULL DEFAULT 'polymarket';
ALTER TABLE equity_snapshots ADD COLUMN venue TEXT NOT NULL DEFAULT 'polymarket';
ALTER TABLE equity_snapshots ADD COLUMN currency TEXT NOT NULL DEFAULT 'USDC';
CREATE INDEX idx_positions_venue_market ON positions(venue, market_id);
CREATE UNIQUE INDEX idx_orders_venue_ext ON orders(venue, external_id) WHERE external_id <> '';
```

`settings`/`system_settings` 是单行 JSON payload（`sqlite.go:54-127`），**不拆表**，改为 JSON 内嵌 `venues: { polymarket: {...}, predictfun: {...} }`，读取时兼容旧扁平结构（旧键 = polymarket）。`accounts` 同理按 venue 增加一条记录（PF 是独立热钱包）。

## 3. predict.fun 适配器实现要点（逐条陷阱）

| # | 陷阱 | 正确做法 |
|---|---|---|
| 3.1 | 只有 Yes 盘口 | `Book(outcome=No)` = `1−p` 量化到 tick；`bids↔asks` 对调、按新价格重排 |
| 3.2 | tick 逐市场 | 读取市场 `decimalPrecision`，所有出价必须量化；否则签名 hash 与撮合端不一致 |
| 3.3 | 数额用 wei(1e18) | 出站 `strconv.FormatUint(shares*1e18)`；`amount=taker`、`pricePaid=maker` 语义不可颠倒 |
| 3.4 | 最小单 | 下单前校验 `shares*price ≥ 1 USDT` 且 `shares ≥ 1`，否则直接拒（现在会浪费一次 401/400 往返） |
| 3.5 | 鉴权两步 | `GET /v1/auth/message`（纯文本，EIP-191 personal_sign）→ `POST /v1/auth {signer,message,signature}` → JWT；请求头用 `Authorization: Bearer` + `x-api-key`；JWT 过期/401 时清缓存重签一次，**不重签订单签名** |
| 3.6 | 签名字段 | EIP-712 domain `predict.fun CTF Exchange / 1 / chainId(56\|97)`；`Order` 12 字段、`feeRateBps` 必须填入市场真实值；`salt` 用 CSPRNG，`< 2^31`；`expiration` = now+300s（GTD）或 0（GTC） |
| 3.7 | 合约选择 | `isNegRisk ? NEG_RISK_CTF_EXCHANGE : CTF_EXCHANGE`；`isYieldBearing=true` 走的是另一族合约 → **v1 直接拒绝并记日志**，不要静默用错合约 |
| 3.8 | 重试 | 签名在重试之外，重试重发同一 payload（靠 order hash 去重）；禁止在重试内重签 |
| 3.9 | 到期时间 | 列表接口无 `endsAt`，只能 `GET /v1/categories/{slug}` 取，按 categorySlug **批量+缓存（≥5min）**；F1 永不结算由 `tradingStatus`/`status` 兜底 |
| 3.10 | 状态码 | `tradingStatus ∈ {OPEN, MATCHING_NOT_ENABLED, CANCEL_ONLY, CLOSED}`、`status ∈ {REGISTERED, ...}`；只有 `OPEN` 才允许下单；`CANCEL_ONLY` 只能撤 |
| 3.11 | 链上准备 | 首次需 `USDT.approve(exchange, max)`；**BNB Chain 需要 BNB 付 gas**（PM 是 MATIC）→ 部署清单里必须写"给 PF 热钱包打 ~0.02 BNB" |
| 3.12 | 账户模式 | v1 用 **EOA 模式**（personal_sign），不用 Kernel 智能钱包：Python/Go 参考实现都只做 EOA，少一层签名包装，风险最低 |
| 3.13 | 流动性薄 | 实测单市场 `totalLiquidityUsd` 仅约 $90、asks 侧可能只有几十股 → 下单量必须被 `min(depth, MaxOrderUSD)` 夹住，否则一次就吃穿盘口并按滑点成交 |
| 3.14 | 费率 | `feeRateBps` 取自市场（实测统一 200），taker 收、maker 免；策略若挂 maker 单要注意 `CANCEL_ONLY`/`MATCHING_NOT_ENABLED` 状态下单无效 |

## 4. 前端改造

> 完整清单见 `docs/research/frontend-venue-coupling.md`（643 行，含逐文件行号）。`grep -rn venue frontend/src` → 0 命中，venue 概念在前端**完全不存在**。

### 4.1 前置决策：账户即场所（方案 A）

推荐 `Account` 绑定 `venue`：`dashboard`/`positions`/`orders` 天然按账户分组，**不需要任何聚合代码**，UI 工作量比"账户跨场所"少约 40%，且从结构上杜绝跨币种相加。代价是总览页要新增"按场所现金卡"与“per-venue 权益序列”。

### 4.2 必须先做的一步：拆文件

加 Tab 之前必须机械搬迁（否则每个新功能都要在 1000+ 行文件里改）：

| 文件 | 现状 | 拆成 |
|---|---|---|
| `frontend/src/views/system.tsx` | 1144 行 | `Section` / PM live / PM endpoint / proxy / password / index |
| `frontend/src/views/settings.tsx` | 576 行 | `schema.ts` + 按节拆组件 |
| `frontend/src/types.ts` | 353 行 | 分域文件 + `index.ts` 重导出（`@/types` 引用零改动） |
| `frontend/src/views/opportunities.tsx` | 511 行 | 表格 / 抽屉 / 状态 |

顺序：`types.ts` → `system.tsx` → `settings.tsx` → `opportunities.tsx`，每步单独提交、`tsc --noEmit` + 手测通过再进下一步（避免长命分支）。

### 4.3 唯一真相源与作用域

新增 `frontend/src/lib/venue.ts`（venue 元数据：label/currency/trade url/能力）与 `lib/venue-scope.tsx`（当前选中场所的 Context）。所有 venue 判断只允许查这两个文件。

**关键陷阱**：`frontend/src/hooks/usePolling.ts:22-28` 在 fetcher 身份变化时会**清空 `data`**。把 `venue` 放进 `useCallback` 依赖 → 每次点 chip 表格都闪成骨架屏。必须让 fetcher 保持稳定引用（把 venue 作为参数传入并在内部读取，或改用 `useRef` 保存最新 scope）。

### 4.4 逐视图改造

1. **设置页**：`settings.tsx:37-135,429-456` 现在是扁平 `Settings` + 单一 `PUT /settings` → 改 `Settings.venues: Record<VenueId, VenueSettings>`（后端兼容旧扁平结构）。PF 卡必填 `apiKey`/`privateKey`/`testnet`，可选 smart wallet；文案必须写明"API Key 在 predict.fun 账户页申请"、"这是 **BNB Chain** 的另一把钥匙，与 Polygon 私钥不共用"。`POST /accounts` 要带 `venue`（`settings.tsx:355-374`）。
2. **机会雷达**：`opportunities.tsx:64-85,208` 用 **裸 `token_id`** 作 Map 键 —— PM 的 `clobTokenIds` 与 PF 的 `onChainId` 都是十进制 ERC-1155 id 字符串，PF 是**小整数**，会让 PM 持仓在 PF 行上误亮"已买入"并显示错误成本价。必须改成 `key = venue + ':' + token_id`。同理 `orders.tsx:133-139` 与 `opportunities.tsx:362` 是**服务端分页**（`limit/offset`/`data.total`），前端本地按 venue 过滤会让"共 N 条"和"下一页"禁用点算错 → **venue 必须作为查询参数下推到后端**，不能前端过滤。
3. **持仓/权益**：`types.ts:34-53` 的 `Metrics` 无币种；`types.ts:68-71` 的 `value` 无单位；`dashboard.tsx:89` 写死 `USD`、`:131-132` 单序列、`:152` `total_return`。→ 按 venue 出多条序列 + 折算合计（显式标注"折算"）。
4. **订单**：`api.ts:170-174` 的 `createOrder` **没有** venue/order_type/limit_price（前端目前完全没有 LIMIT/MARKET 概念）。PF 是 `MARKET + slippageBps + isMinAmountOut`，PM 是 FAK 走卖单簿 —— 新增 `order_type` 并列字段（不要把 `type` 改名，`settle` 语义依赖它）。
5. **下单票据**：`opportunities` 抽屉里展示"本单将扣的手续费 / 最小单 $1 / tick 0.001"，这是唯一必须新增的交互。

### 4.5 静默算错点（不报错、数值看着正常，但结论错）

| # | 位置 | 问题 |
|---|---|---|
| 1 | `types.ts:37-46` + `dashboard.tsx:156,172` | 跨币种求和 `cash`/`equity`/`exposure`，前端无 FX 层（`format.ts` 全文无换算），却统一渲染成 `$` |
| 2 | `dashboard.tsx:89,129-132,152` | 净值曲线单序列；若混币，`total_return` 会因 USDT/USDC 溢价而显示"盈利" |
| 3 | `dashboard.tsx:168` | `fees_paid` 把 PM 类别费率与 PF `feeRateBps`（含 maker rebate，负费用）相加，两种口径都不是 |
| 4 | `opportunities.tsx:116-144,486,489` | `net_yield`/`fully_filled`/`available_notional` 在两个场所语义不同（PF 是 `slippageBps` 乐观估计），UI 却同构并排，用户会横比 |
| 5 | `opportunities.tsx:64-85,208` | 裸 `token_id` 做键（见 §4.4.2） |

顺带修掉的既有 bug：`dashboard.tsx:262` 把 `sell`/止损单标成「结算」（`orders.tsx:30-34` 是对的）；`settings.tsx:313` 绕过 `formatCurrency`；`settings.tsx:490-500` 把 0–1 概率标成 `unit="USD"`；`protocolBadge` 只对 v2 渲染，v1 行无标记。

### 4.6 前端需要后端新增的最小 API 面

| 能力 | 形状 |
|---|---|
| `GET /venues` | `{ items: [{ venue, label, currency, enabled, order_types, live_ready, live_error, healthy, last_scan_at }] }` |
| 列表 `?venue=` | `opportunities` / `positions` / `orders` / `scan` |
| `Settings.venues` | `Record<VenueId, VenueSettings>`（读旧结构兼容） |
| `SystemSettings.venues` | `Record<VenueId, { base_url, trade_url, testnet, live_environment, live_ready, live_error }>` |
| `POST /accounts { venue }` | 账户创建带场所 |

## 5. 分期与验收

切片编号沿用 `docs/research/backend-venue-coupling.md` §3（S0–S12），每个切片可独立提交、`go test ./...` 保持绿、PM 行为逐字节不变（靠 S0 的 golden fixture 守）。

| 里程碑 | 切片 | 内容 | 验收（必须满足） |
|---|---|---|---|
| **M0** 纯重构 | S0–S4 | 特征化测试 → `venue` 词表包 → `Engine` 改持 `venue.Registry`（先只注册 1 个）→ 引擎内 `(venue,id)` 命名空间 → `LiveExecutor`→`VenueExecutor` + `Instrument` | 无行为变化：同 HTTP 调用序列、同 DB 行、同 JSON；`go test ./...` 全绿；PM 小额实盘走通一次 |
| **M0.5** 地基 | S5–S8 | `SystemSettings.Venues` 双读双写 → `venue`/`currency` 列（**必须在两处表重建之后追加**）→ `FeeModel`/`CashReader` 抽象 → 金额在边界改 `big.Int`+`Decimals` | 旧库原地升级不丢列；`1e-9` 绝对 eps 换成 `epsilon(decimals)`；PM 数字一位不差（对 S0 fixture 断言） |
| **M1** 只读接入 | S9 + S10 最小面 | PF `ListOpen/Get/Venue`、category→`endsAt` join+cache、`Unsupported` 原因、`GET /venues`、`?venue=` | 能扫 ≥100 个 PF 市场；无 API Key 时优雅降级；PM 输出与关闭开关时逐字节相同；扫描日志看得到**扣费后**的 edge |
| **M2** testnet 交易 | S11 | PF 执行器（EIP-712、`x-api-key`+JWT、`big.Int` 金额、LIMIT/MARKET+slippageBps）、`(venue,mode)` 唯一放宽、per-venue 执行策略、扫描锁按场所隔离 | chainId 97 上 下单→成交→撤单→持仓回读 闭环；错误路径（<$1、tick 不对、余额不足、yield-bearing）有明确报错 |
| **M3** 主线小额 | S11 收尾 | 切主网，`MaxOrderUSDPerVenue=5`、`MaxExposurePerVenue=50`，dry-run→真实；BNB gas 就位 | 连续 50 单无重复下单/无签名错/无资产错配；链上 vs 本地对账一致 |
| **M4** 结算与 UI | S12 + 前端 | `Settler` 按 `position.Venue` 分派（PF 未实现则 **fail-closed 并露 LastError**）、前端按 §4 改造 | 无“永远不结算”的静默持仓；两场所权益曲线分开；全局敞口上限生效 |

**顺序禁忌**：S6（加列）不能早于 S2–S4（命名空间），否则会存在难以回填的歧义行；`migratePositionStatus`/`migrateOrderStatus` 会按显式列清单重建 `positions`/`orders`，提前加的 `venue` 列会被**静默丢弃**。

预估：M0≈1.5d、M0.5≈1.5d、M1≈1d、M2≈2d、M3≈0.5d、M4≈1.5d ≈ **8 人日**。其中前端拆分（§4.2）与 S1/S2 无耦合，可并行。

## 6. 与 `docs/multi-venue-architecture.md` 的差异

0. **该文档 §4.1 的行号引用 12 处全错**（例：它说 `engine.go:557-673` 是 `scan`，实为 `575-724`；`types.go:491` 是 `Opportunity`，实为 `527-555`；`sqlite.go:343` 是 `ensureColumn`，实为 `358-385`；`engine.go:471` 是"单实盘账户"，实为 `473-476`）。建议把该文档的"实施章节"替换为本文 + 两份审计，避免按错行号施工。
1. **不重写目录**：不落地 `domain/ports/adapters`（这些目录至今 0 个 `.go`），改为在现有包上加 `venue` 接口（理由见 §0.1）。
2. **到期时间**：文档说"需要 category join"，实测确认，且补充"必须缓存"，否则每轮扫描 = 每市场一次 HTTP。
3. **合约地址**：以 SDK 源码为准，且必须处理 `isYieldBearing` 的另一族合约（文档未涉及）。
4. **费率**：文档只提"taker 收费"，这里给出可计算式 `fee = feeRateBps × min(p,1−p) × shares` 及对低价策略的含义（2% of notional），这是策略侧必须改的地方。
5. **跨场所比价**：PF 市场自带 `polymarketConditionIds`（实测 100 个市场里 42 个非空），存在**精确 join 键**，不需要 LLM 模糊匹配；但本文**不把跨场所套利列入本期**（策略是单边捡币，套利要双腿原子性，成本收益不匹配）。

## 7. 风险与安全

- **私钥面**：新增一把 BNB Chain 热钱包私钥。必须与 PM 钱包隔离（不同助记词/不同机器可），单场所限额兜底。
- **供应链**：2026-10 的 40 万 USDT 事件源于**文档站被入侵**（官方口径）。所以合约地址不要从文档抄：要么从 SDK 源码常量取，要么在运行时校验（签名前比对 `chainId` 与目标合约白名单）。
- **重复下单**：PF 靠 order hash 去重，但我们的重试逻辑一旦重签就会双花 —— 代码层面用"签名产出物不可重入"的单元测试守住。
- **资产错配**：两种币不能互转，UI 若做"合计"必须显式标注为"折算估值"，不可用于风控计算之外的展示。
- **数据面**：PF 列表无到期时间、盘口只有一侧、深度极薄 —— 任何"我们以为的价差"都可能不存在，M1 的只读期就是用来验证这一点的。

## 8. 待决策（需要产品/资金侧确认）

1. PF 用 EOA 模式还是 Smart Wallet 模式？（建议 EOA，M2 定）
2. PF 的 API Key 现在能拿到吗？（这是 M2 的硬前置）
3. 单场所投入上限与单笔上限的具体数值（建议 5 / 50 USDT 起）
4. 是否需要在 PF 上挂 maker 单（享受 0 费但可能长期不成交）——影响 M2 的订单类型实现。

## 9. 实施结果与偏差（2026-11 回填）

代码位置：`backend/internal/venue`（契约层）、`backend/internal/predictfun`（适配器）、`backend/internal/engine`、`backend/internal/store`、`backend/internal/api`、`frontend/src/lib/venue.ts`。原来的 8 个空目录（`app/domain/platform/ports/execution/service/strategy/adapters`）已删除，没有落地六边形结构。

与本文设计一致的部分（不再重复）：
- `venue` 薄接口 + 注册表，`Engine` 持有 `*venue.Registry`，按场所遍历、逐场所错误隔离。
- 身份键 `venue + tokenID`；`venue`/`currency` 落到 SQLite 并在 SQL 层过滤（翻页前）。
- 费用模型按场所分派（PM 保持历史口径，PF 用 `rate × min(p,1−p) × shares`）。
- PF 侧：签名只做一次、重试复用同一签名、金额全程 `big.Int`、fail-closed（yield-bearing / 缺 tick / 缺到期 一律不交易）。
- 前端 chip 走服务端过滤、`usePolling` 用 `useRef` 稳定 fetcher、跨场所不求和、金额恒带币种。

实施中被现实修正的设计（**这些是本文原来写错或写漏的**）：
1. **列表接口的 `stats` 恒为 `null`**（本文假设有流动性字段）。若不补，PF 全场所流动性为 0，会被 `MinLiquidity` 整体过滤掉。实现改为对每个市场的 `/v1/markets/{id}/stats` 补拉（TTL 1 分钟、并发 8、单次扫描上限 250），并且**只填补空缺**：列表若已报出深度，就用列表的值，避免"只看卖一深度"把有买盘厚度的市场误判为不可交易。
2. **`outcomes[].bestBid/bestAsk` 形状不稳定**（数字 / 字符串 / `null` / `{price,size}` 对象都出现过，本文按单一形状设计）。已用自定义 `UnmarshalJSON` 兼容四种形状，遇到数组等未知形状仍然报错（不猜）。
3. **PF 的到期时间是 category 粒度**：同一 `categorySlug` 下所有市场共用 `endsAt`（实测 `which-party-will-win-the-house-in-2026` 全部为 2026-11-03）。因此 `days_to_end` 对 PF 是"分类到期"，不能当作单市场精确到期时间使用。
4. **凭据热更新是本轮新增需求**：凭据从环境变量变成网页可改之后，"保存了却要重启"会被当成保存失败。PF 客户端改为 `atomic.Pointer` 快照配置，`SetBaseURL` / `ApplyLiveSettings` 即时生效并清缓存；`/system/live/test?venue=predictfun` 用候选凭据试连后**恢复已存凭据**。`applyRuntimeSettings` 只在 `system.venues.predictfun` **存在**时下推凭据——条目不存在表示"继承进程环境"，用零值下推会把 env 里的 `testnet` 冲掉。
5. **`Settings.Venues` 的缺省语义**：条目不存在时 PM 启用、PF 停用；条目存在则按字面读。这样升级到一个带多场所的版本，不会静默开始交易新场所。
6. **`min_liquidity` 仍是全局值**（本文设计为逐场所）。PF 真实深度跨度很大（$90–$315k），逐场所流动性阈值是**已知待办**（记在 §8 之后，不阻塞本次交付）。
7. **只读场所也能报价**：行情注册与执行器注册是分开的，没有签名器时 `/opportunities` 与真实收益报价照常工作，只有下单会以 409 拒绝并说明原因。
8. **M4 已补齐（2026-11-03）**：PF 现在会自动结算（按公布的获胜结果映射 1/0，平局 0.5）并支持链上赎回；见 §12–§14。`merge`/`convert` 仍未做。**链上授权已有入口**（见第 10 条）。

## 10. 复审修复（同日回填）

一次独立复审（只读，覆盖引擎/适配器/API/前端）发现 4 个阻塞项与若干小问题，已全部处理：

| 阻塞项 | 现象 | 修复 |
| --- | --- | --- |
| 下单被拒后无本地记录 | `onSigned` 在 POST 成功后才回调；POST 失败时订单可能已成交，本地却只留 `external_id=""`，对账看不见 → **可能重复买入** | `onSigned` 提前到提交之前（与 Polymarket 执行器一致）；POST 失败先 `LookupOrder` 试恢复，否则以 `unknown` + 本地 hash 落库 |
| 手续费双记 | `LookupOrder` 把 fee 折进 `Cost`，引擎再加一次 `Fee` → 卖出可能算出 ≤0 而**永久锁死自动交易** | `Cost` 统一为 `shares*price` 毛额，fee 只在 `Fee` 字段 |
| 挂单被记成"被拒" | GTD 挂单无成交时 `shares==0` 且状态 `open` → 引擎按 rejected 记账，真实持仓存在 | 无成交一律 `unknown`（能被对账循环捡回）；部分成交同时撤掉剩余挂单 |
| 测试网部署被静默切到主网 | 网页保存任一字段都会把持久化的 `testnet=false` 下推，而 UI 徽标仍显示启动时的值 | `testnet` 改为三态（省略=继承环境，显式=切换）；凭据探活用 `ResetLiveSettings` 回到启动基线 |

其余修复：`Configured()` 不认链上状态 → 审批/探活前先判配置；探活未持交易锁 → 加锁；PF 忽略 `settings.SlippageBPS` → 仍用场所默认并在契约注明（`MaxSlippageBPS=200`）；纸张卖出手续费只按设置费率 → 优先用雷达报告的场所费率；对账价格回退顺序（`pricePerShare → implied → limit`）；`amountFilled` 小数被当成 0；`Settings/SystemSettings` 返回 map 拷贝；单场所扫描不再清空其它场所的雷达行；分类启发式纳入 `Market.Category`；`ReadOnly` 由注册状态推导；删除死代码（`RealFeeModelFor`、`FeeBPSOrDefault`）；订单原因文案改为按场所生成。

**两个只有在真机/真实类型下才暴露的 bug**（单测用 fake 执行器时全绿）：
1. PF `*Executor` 没有转发 `ApplyLiveSettings`/`SetBaseURL`，引擎的类型断言静默失败 → **凭据热更新在生产环境完全没生效**。现在执行器补齐 5 个方法并加了编译期断言。
2. `ApplyLiveSettings` 保存新私钥后 maker 仍钉在旧 signer（`signer != maker`）→ 实盘被 `ReadyError` 拒绝。新增 `makerImplicit` 记住"maker 是隐式跟随 signer 的"，换 key 时自动跟随。（`internal/engine/venue_predictfun_integration_test.go` 用**真实** predictfun 执行器断言这一行为。）

新增能力：`GET /system/live/approval?venue=` 与 `POST /system/live/approve?venue=`，前端 PF 凭据卡显示余额/授权额度/结果份额划转状态并提供「链上授权」按钮（真实链上交易，需 BNB gas）。授权前会拒绝下单，而不是在提交后才失败。

## 11. 第二轮复审（同日回填）

第二轮复审只审第一轮修复新增的切片，又发现 4 个阻塞项，均已修复并有回归测试：

1. **手续费折算必须有唯一归属**：`venue.Fill.Cost` 定为**毛额**，`Fee` 单列，引擎在把成交落成持仓/已实现盈亏时**只折算一次**（直接买入 `Cost += Fee`、直接卖出 `Cost -= Fee`、对账同一处理）。此前 Polymarket 的 `applyFee` 在成交上折了、对账路径又折一次，而 PF 两条路径都不折 → 同一笔成交在"直接成交"与"对账"下差一个手续费。测试：`TestLiveStopLossSellsThroughExecutor` 断言入场成本 98.3（毛额 98.2 + 费 0.1）、出场净额 74.8（毛额 74.9 − 费 0.1）。
2. **单场所扫描曾清空其它场所的雷达**：`mergeOpportunities` 原先用"扫到的场所数 ≥ 请求的场所数"推断是否为全量扫描，而带 `?venue=` 的扫描请求的正好是一个场所 → 两个数相等 → 直接整体替换。改为显式 `scoped` 标志，并让 `Opportunities().Venues` 报告雷达当前真正包含的场所。测试：`TestScopedScanKeepsOtherVenueRows`。
3. **测试网部署可能被静默切到主网**：前端把"继承环境(nil)"压成 `false` 并每次保存都发送；后端 `VenueSystemSettings.Merge` 又不继承 `Testnet`。现在前端 `testnet` 也是三态（未显式选择时**不下发**该字段，界面显示"跟随部署环境"），后端 Merge 继承，且 `VenueRuntime.testnet` 读执行器的**实时**网络而不是注册时的能力快照。
4. **切网时的端点保护是死代码**：切网先清空 `BaseURL`，随后又被传入值覆盖 → "主网 chainId + 测试网 REST"。现在切换网络时会忽略与当前端点相同的传入值（那一定是按旧网络解析出来的），显式给出的**不同**端点仍然生效。测试：`TestApplyLiveSettingsDropsEndpointPinnedToTheOldNetwork`。

其它一并处理：4xx（非 429）视为确定性拒单而不是 `unknown`（避免无谓冻结止损）；`sideIsSell` 在响应缺 `side` 时才回退（显式 `0`=买入要被尊重）；`EnsureApprovals` 授权后重读链上状态（否则 `ReadyError` 仍报未授权）；探活恢复失败写入系统错误而不是静默吞掉；`Market.Category` 参与分类启发式；`sort.SliceStable`；`VenueSystemSettings.Merge` 把脱敏占位符当"保留";清理死分支、`gofmt`。

**已知限制**（不阻塞交付，记入待办）：`min_liquidity` 仍是全局值；PF 结算/赎回未实现；PF 实盘只支持 EOA（smart wallet 会 fail-closed 拒绝）；对账中"token 已不在雷达"的订单会一直保持未对账状态并保守地阻止该账户自动交易，需要人工核对。

## 12. 联网核实（2026-11-03）：结算与赎回的真实规则

调研报告：`docs/research/predictfun-settlement.md`（550 行，结论按 [V] 已验证 / [I] 推断 / [?] 未知标注）。

- **权威来源是 OpenAPI 而不是文档站**：`https://api-testnet.predict.fun/docs` 内嵌完整 OpenAPI 3.0（110 paths / 364 schemas），主网同构；`docs.predict.fun`/`dev.predict.fun` 在 2026-10 的安全事件后已下线。主网 GET 需要 `x-api-key`（401），测试网 keyless，schema 共用。
- **`resolution` 不是枚举而是一个 outcome 对象**：真实载荷是 `{"name":"Up","indexSet":1,"onChainId":"5830…","positionId":"5830…","status":"WON",…}`，没有 `winningIndexSet`/`resolvedAt`/结算价。原先按字段名猜的 `Resolution{Status,WinningIndexSet,ResolvedAt}` 完全不存在——这类错误只能靠真机载荷发现。
- **`?status=` 只接受 `OPEN|RESOLVED`，分页参数是 `?first=`**：`?limit=` 被忽略且恒返回 20 条，很容易误判"平台上只有 20 个已结算市场"。
- **结算状态机**：`MarketStatus` = REGISTERED, PRICE_PROPOSED, PRICE_DISPUTED, PAUSED, UNPAUSED, RESOLVED, REMOVED；`PRICE_PROPOSED` **不是终态**（实测市场在提议后约 20 小时才正式结算），`PRICE_DISPUTED` 可能翻盘。所以只在 `RESOLVED` + `tradingStatus=CLOSED` 且"恰好一个 WON"时才给出 1/0。
- **平局真实存在**：市场 1388 两侧都 WON，链上 `payoutDenominator=2`、`payoutNumerators=[1,1]` → 每股 0.50。"取第一个 WON 当赢家"会把一半仓位记错。
- **赎回**：普通市场调 CTF `redeemPositions(USDT, parentCollectionId=0x0, conditionId, [indexSet])`（selector `0x01b7037c`，indexSet 必须取 `outcomes[].indexSet` 而不是名字顺序）；neg-risk 市场调 `NegRiskAdapter.redeemPositions(bytes32 conditionId, uint256[] amounts)`（selector `0xdbeccb23`，`amounts` 按结果槽位给值，Yes → `[amt,0]`、No → `[0,amt]`）。`convertPositions` 是独立的 NO 转换流程，不是赎回前置。
- **gas**：两个官方 SDK 都没有 relayer/gasless，赎回就是普通 EOA 交易，签名地址必须有 BNB。`POST /v1/gateway/authorize{,/redeem}` 只签授权、不代发交易。
- **授权**：普通赎回无需任何授权；neg-risk 的 redeem/merge/convert 需要 CTF 上 `setApprovalForAll(NEG_RISK_ADAPTER, true)`（已并入 `EnsureApprovals`）。
- 链上真相已用 `eth_call` 核对：`payoutDenominator`(`0xdd34de67`)/`payoutNumerators`(`0x0504c814`) 与 API 一致；`conditionId == keccak256(resolverAddress‖oracleQuestionId‖uint256(2))` 复算命中。
- **仍未解**：`positionId` 的推导公式（不要自己算 token id，直接用 `outcomes[].onChainId`）；`NegRiskAdapter.getResult` 的参数语义；交易所按长度还是 `signatureType` 分派代理钱包签名；交易所侧最小下单量（SDK 侧是 0.01 股）；主网与测试网载荷是否漂移（本仓库只在测试网真机验证过）。

## 13. 本轮实现（2026-11-03）

1. **逐场所 `min_liquidity`**：`Settings.venues[venue].min_liquidity`（0 = 继承全局）。PF 市场深度与 PM 差一个量级（实测 $90–$315k），单一阈值要么藏掉 PF 全部、要么放进 PM 噪声。
2. **自动结算**：`predictfun.Provider.mapMarket` 按 `resolution.indexSet` → `outcomes[].onChainId` → `status==WON` 三级回退判定赢家，赢家价格记 1、其余记 0；**两个及以上 WON 视为平局**，两侧都记 0.5；terminal 但没有赢家时写 `resolution_pending`，防止引擎拿最后成交价结算。`Capabilities.SupportsSettlement` 改为 `true`。
3. **链上赎回**：`venue.Redeemer` 契约 + `predictfun.OnChain.Redeem`（手写 ABI、gas 预检）+ `engine.RedeemPosition`（先落库再签名、幂等、持 `tradeMu`）+ `POST /accounts/:id/positions/:positionId/redeem` + 前端「赎回」按钮。
4. **neg-risk 前置授权**：`EnsureApprovals` 增加第三个 operator（NegRiskAdapter 在 CTF 上的 `setApprovalForAll`），且不给它多余的 USDT allowance。

## 14. 已知限制（本轮结束时）

| 限制 | 影响 | 说明 |
| --- | --- | --- |
| 代理钱包（Kernel）签名未实现 | 托管账户/智能钱包用户无法实盘 | 签名格式已查清（订单哈希外层包 Kernel 域 + `0x01‖ECDSA_VALIDATOR‖sig` 86 字节），但没有真机向量，宁可不做也不猜 |
| 赎回需自付 BNB gas | 无 gas 的账户赎回会失败 | 接口有余额预检与可操作文案；官方也无 relayer |
| `merge`/`convert` 未做 | NO 侧无法合并成抵押品提前释放资金 | 需要额外合约调用与仓位配对逻辑 |
| 主网未真机验证 | 主网载荷可能与测试网漂移 | 主网 GET 需要 `x-api-key`，本仓库只在测试网端到端跑过 |
| WebSocket 未接 | 盘口刷新依赖轮询（默认 30s） | `wss://ws.predict.fun/ws` 的 topic 与心跳已记录在 §3 |
| 平局/撤单的链上对账 | 平局按 0.5 关账，链上赎回按 0.5 赔付 | 需在平局时人工确认赎回金额 |
