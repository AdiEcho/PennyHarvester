# 加密市场入场价优化开发文档

日期：2026-11-27  
触发案例：Bitcoin Up or Down 5分钟市场，Down 结果以 0.310 入场，最终 -2.06 USDC。

---

## 1. 问题定义

### 1.1 现象

实盘以 0.310 入场 "Down" 结果，市场已定价上涨概率 69%，Down 以亏损结算。0.310 不是意外滑点，而是系统在市场共识明显偏向另一侧时仍然执行了入场。

### 1.2 根本原因

三个相互叠加的结构性缺陷：

**R1 — 价格使用扫描时参考价，不使用实时盘口**

`findOpportunity`（`types.go:1432`）将 `outcome.Price`（最近一次扫描得到的 Gamma 参考价）存入 `Opportunity.Price`。`executeLiveBuy`（`orders.go`）以这个价格估算手续费（`orders.go:997`），但提交给 CLOB 的限价是 `settings.MaxPrice`，买单实际成交价来自 CLOB 撮合。

默认扫描间隔 60s，加密市场入场窗口也是最后 60s（`CryptoEntryWindowSeconds: 60`）。在触发入场决策和实际提交订单之间，5分钟比特币价格可以剧烈移动，系统对此没有任何校验。

**R2 — 无方向性置信度门槛**

`findOpportunity`（`types.go:1464`）只检查 `settings.MinPrice ≤ outcome.Price ≤ settings.MaxPrice`。对于默认值（`0.95–0.99`），0.310 会被拒绝。但若用户为加密方向性市场调低了 `MinPrice`，系统就会在胜率低于 50% 时仍然入场——没有任何"不入逆势场"的保护。

**R3 — 无实时下单前验证**

`executeLiveBuy` 在调用 `placeLiveBuy` 之前不查询当前盘口。系统已有 `QuoteOpportunity` 路径（它走真实 CLOB ask 侧并计算 `LimitPrice`），但该路径仅用于前端预览，实盘入场不经过它。

---

## 2. 代码路径与关键行

```
engine/scan.go           → scan() → autoLiveTrade()
engine/orders.go:997     → exec.EstimateBuyFee(ctx, conditionID, amount, opportunity.Price)
engine/orders.go:1040    → placeLiveBuy(ctx, exec, ..., amount, settings.MaxPrice, onSigned)
engine/types.go:1432     → Price: outcome.Price        ← 扫描时参考价，非实时买一价
engine/types.go:1464     → outcome.Price < settings.MinPrice || > settings.MaxPrice
engine/orders.go:321     → fillPrice = opportunity.Price * (1 + SlippageBPS/10000)  ← 模拟路径
```

加密入场窗口路径：
```
engine/orders.go:248-256 → autoEntryWindow()
  if category == Crypto → CryptoEntryWindowSeconds (default 60s)
  withinAutoEntryWindow() 仅检查 remaining ∈ (0, window]
```

---

## 3. 优化方案

### 3.1 O1：实盘入场前实时盘口验证（最高优先级）

**目标**：在 `executeLiveBuy` 调用 `placeLiveBuy` 之前，用实时 CLOB 询价校验当前可成交价，若超过可接受偏差则拒单。

**实现要点**

在 `executeLiveBuy`（`engine/orders.go`）的风险检查通过之后、`placeLiveBuy` 调用之前，插入实时询价逻辑：

```go
// 建议插入位置：engine/orders.go，executeLiveBuy 中风险 switch 之后

quote, quoteErr := exec.QuoteBuy(ctx, opportunity.TokenID, opportunity.ConditionID, amount)
if quoteErr != nil {
    // 询价失败视为盘口不可达，拒单而非继续
    return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount,
        "实盘下单前盘口询价失败: "+quoteErr.Error(), "")
}
if quote.LimitPrice > settings.MaxPrice {
    return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount,
        fmt.Sprintf("实时盘口最差成交价 %.4f 超过 max_price %.4f，拒单",
            quote.LimitPrice, settings.MaxPrice), "")
}
// 可选：拒绝参考价与实时价偏差过大的情况
if priceDrift := quote.LimitPrice - opportunity.Price; priceDrift > settings.MaxPriceDriftBPS/10000 {
    return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount,
        fmt.Sprintf("实时价较扫描参考价漂移 %.4f，超过允许偏差，拒单", priceDrift), "")
}
```

需要在 `venue.Executor` 接口或 `venue.Quoter` 子接口中添加 `QuoteBuy`，或复用已有的 `OpportunityQuote` 路径（`engine/engine.go` 中的 `QuoteOpportunity`）。

新增设置字段（`engine/types.go` `Settings` struct）：

```go
// MaxPriceDriftBPS 是允许实时盘口买一价相对扫描参考价的最大正向偏差（单位 BPS）。
// 0 表示禁用漂移检查，仅依靠 MaxPrice 拦截。
// 建议初始值 50（0.5%），即参考价 0.31 时拒绝实时价 > 0.3155 的入场。
MaxPriceDriftBPS float64 `json:"max_price_drift_bps"`
```

**验收**
- 扫描参考价 0.31，实时盘口 0.32，MaxPrice=0.99，MaxPriceDriftBPS=50 → 拒单，记录原因
- 扫描参考价 0.31，实时盘口 0.311，同配置 → 通过
- `QuoteBuy` 超时 → 拒单，不继续提交
- 拒单记录写入 orders 表，reason 包含实时价与参考价

---

### 3.2 O2：加密市场方向性置信度门槛

**目标**：加密方向性市场（5分钟 Up/Down 类）在当前胜率已低于 50% 时，拒绝入场。

这类市场的价格从 0 到 1 均有意义，0.310 的 Down 意味着市场认为上涨概率达 69%，此时入场 Down 是逆势下注。`MinPrice` 设计为尾部策略筛选器，不适合做方向性过滤。

**实现要点**

在 `findOpportunity`（`types.go`）中，对加密市场增加额外检查：

```go
// 建议插入位置：types.go findOpportunity()，当前 MinPrice/MaxPrice 检查之后

if m.EffectiveVenue().IsCryptoPricingMarket() || m.Category == string(market.CategoryCrypto) {
    if outcome.Price < settings.CryptoMinConfidence {
        return Opportunity{}, fmt.Errorf("%w: 加密方向性市场胜率 %.4f 低于最低置信度 %.4f",
            ErrInvalidOrder, outcome.Price, settings.CryptoMinConfidence)
    }
}
```

新增设置字段：

```go
// CryptoMinConfidence 是加密方向性市场的最低入场置信度。
// 0.5 表示只在当前结果胜率 ≥ 50% 时入场。0 禁用。
// 对常规尾部策略不影响（那些市场已被 MinPrice=0.95 覆盖）。
CryptoMinConfidence float64 `json:"crypto_min_confidence"`
```

默认值建议 `0.50`，可在设置页配置。

也可以更简单地在现有框架内处理：为加密市场单独设置 `MinPrice=0.50`，通过 `Venues` 覆盖或按 category 的分层参数实现，不引入新字段。两种方式等价，前者语义更明确。

**验收**
- Down=0.310（Up=0.69），CryptoMinConfidence=0.50 → `findOpportunity` 返回 ErrInvalidOrder
- Down=0.55，CryptoMinConfidence=0.50 → 通过
- 普通政治/体育尾部市场 price=0.97 不受影响

---

### 3.3 O3：记录扫描到下单的价格时效

**目标**：在实盘订单日志中记录 `opportunity.FetchedAt` 与下单时间的差值，让运营者可见"以多旧的价格做的决策"。

**实现要点**

`Order` struct 中增加（或在 `Reason` 字段拼接）价格时效信息：

```go
// 建议在 executeLiveBuy 的 onSigned 回调中，向 Order.Reason 附加时效信息
priceStaleness := time.Since(opportunity.FetchedAt)
reason := fmt.Sprintf("实盘订单已签名，等待成交回报（参考价时效 %ds）",
    int(priceStaleness.Seconds()))
```

也可在 `Order` struct 中增加 `PriceAge int64 json:"price_age_ms"` 字段持久化到数据库，支持后续统计分析。

**验收**
- 扫描时间 T，下单时间 T+45s，日志/DB 记录 price_age≈45s
- 前端订单详情页可展示该字段

---

### 3.4 O4：加密入场窗口策略评估

**现状**：`CryptoEntryWindowSeconds=60`，5分钟市场的最后1分钟。这段时间市场共识最强，价格最确定，但对于方向已明确的市场，入场时机已过。

**可选方向**（非立即实现，需配合回测）：

- 将加密入场窗口改为"开盘后第 N 秒内"而非"收盘前 N 秒内"，捕捉价格尚未充分发现时的机会
- 增加 `CryptoEntryOpenWindowSeconds`（开盘入场窗口）与 `CryptoEntryCloseWindowSeconds`（收盘入场窗口）两个独立参数
- 或完全禁用自动入场，改为手动在开盘时下单

这个优化依赖对历史成交数据的回测，不应在没有数据支持的情况下盲目调整。

---

## 4. 实施优先级

| 优先级 | 编号 | 说明 | 风险 |
|--------|------|------|------|
| P0 | O1 | 实盘入场前实时盘口验证 | 增加一次网络请求，需设置合理超时（建议 2s）；询价失败时拒单而非降级通过 |
| P0 | O2 | 加密市场方向性置信度门槛 | 纯过滤逻辑，无副作用；降低入场频率，可能错过少数高置信度机会 |
| P1 | O3 | 价格时效日志 | 仅记录，不改变决策；需数据库 schema 变更或复用 Reason 字段 |
| P2 | O4 | 入场窗口策略评估 | 需要回测依据，不建议仅凭直觉调整 |

---

## 5. 受影响文件

```
backend/internal/engine/types.go      — Settings struct 新增字段，DefaultSettings，Validate，findOpportunity
backend/internal/engine/orders.go     — executeLiveBuy 插入实时询价逻辑
backend/internal/venue/venue.go       — Executor 接口或新增 Quoter 子接口
backend/internal/store/sqlite.go      — orders 表 schema（如果持久化 price_age）
```

---

## 6. 不在本文档范围内

本文档聚焦入场价格质量。以下相关但独立的问题由现有开发计划覆盖：

- 行情缓存降级与 `stale` 保护（DAT-01，`2026-11-12-development-plan.md`）
- 未决订单风险准入（LED-01）
- Quote 路径与 order plan 精度（PM-02/PF-08）
