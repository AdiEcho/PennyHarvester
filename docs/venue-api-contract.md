# 多场所 API 契约（v1）

> 状态：已定案，前端与后端按本文实现。任何一方需要改动，先改本文。
> 目标：让 Polymarket 与 Predict.fun 在同一个 UI 里共存，且**任何跨币种数字都不存在求和路径**。

## 0. 身份原则

| 概念 | 规则 |
| --- | --- |
| 场所 | `venue ∈ {"polymarket","predictfun"}`，缺省/空 = `polymarket`（历史行为） |
| 币种 | Polymarket=`USDC`（6 位小数），Predict.fun=`USDT`（18 位小数） |
| 市场标识 | `market_id`、`condition_id`、`token_id` 均为**场所内局部标识**，跨场所不可比较、不可去重 |
| 账户 | 一个账户属于一个场所（`account.venue`），因此余额、权益、暴露天然按场所隔离 |
| 求和 | 只允许在**同一 venue 且同一 currency** 内求和；UI 与 API 都不得跨场所汇总 |

## 1. `GET /api/v1/venues`

```json
{ "venues": [ {
  "venue": "polymarket",
  "label": "Polymarket",
  "currency": "USDC",
  "quote_decimals": 6,
  "min_notional": 1,
  "min_sell_shares": 5,
  "order_types": ["FAK", "FOK"],
  "max_slippage_bps": 100,
  "supports_cancel": false,
  "supports_settlement": true,
  "supports_redeem": false,
  "read_only": false,
  "testnet": false
} ] }
```

- `read_only`：该场所只能只读扫描，不能下单。由注册状态决定：没有交易执行器的场所恒为 `true`（后端在注册时推导，不由调用方声明）。
- `supports_settlement`：引擎能否用参考数据结算持仓。两个场所都为 `true`，但证据来源不同：Polymarket 走自己的 Gamma 解析结果，Predict.fun 把公布的获胜结果映射成 1/0（平局 0.5/0.5）。
- `supports_redeem`：能否对已结算仓位提交链上赎回交易。只有 Predict.fun 为 `true`（Polymarket 是链上 CTF 仓位但本系统不做赎回，靠 Polymarket 自己的 UI/自动结算）；前端据此决定是否在已结算仓位上显示「赎回」按钮。
- `quote_decimals`：抵押品小数位。前端**不需要**用它做运算，只用于文案提示（"18 位精度"）。
- 顺序即展示顺序（Polymarket 在前）。

## 2. 列表端点统一支持 `?venue=`

`GET /opportunities`、`GET /positions`、`GET /orders` 都接受可选 `venue` 参数：

- 缺省 / 空串 / `all` → 不过滤（`all` 只在 UI 内部使用，后端按缺省处理）。
- 非法值 → `400 {"error":"未知场所"}`（不要静默忽略）。
- 过滤必须发生在**分页之前**（服务端过滤），否则 `total` 与翻页会算错。

`GET /opportunities` 响应追加 `venues`：

```json
{ "items": [ { "...": "原有字段", "venue": "predictfun", "currency": "USDT" } ],
  "total": 128, "scanned_at": "2026-11-02T10:00:00Z", "source": "…",
  "venues": ["polymarket", "predictfun"] }
```

`Opportunity`、`Position`、`Order`、`Metrics`、`Account` 均追加：

```json
{ "venue": "polymarket", "currency": "USDC" }
```

## 3. 账户

`POST /api/v1/accounts`

```json
{ "name": "Predict.fun 实盘", "mode": "live", "venue": "predictfun", "auto_trade": false }
```

- `venue` 缺省 = `polymarket`。
- 实盘账户的唯一性从「全局一个」放宽为「**每场所一个**」：允许同时存在一个 Polymarket 实盘账户和一个 Predict.fun 实盘账户。
- 账户列表项追加 `venue`、`currency`、`live_configured`、`live_ready`、`live_error`（沿用现有字段名，逐场所取值）。

## 4. 策略设置 `GET/PUT /api/v1/settings`

在现有扁平 `Settings` 上追加 `venues`，**全局值仍是缺省值**：

```json
{
  "min_price": 0.02, "max_price": 0.20, "order_size": 5.0,
  "venues": {
    "polymarket": { "enabled": true,  "order_size": 5.0, "max_position": 25.0, "max_exposure": 80.0, "fee_bps": 15 },
    "predictfun": { "enabled": false, "order_size": 3.0, "max_position": 15.0, "max_exposure": 45.0, "fee_bps": 200 }
  }
}
```

- 缺省字段（0 或省略）= 继承全局值。`enabled=false` 的场所不参与扫描与下单。
- `fee_bps` 是该场所纸面费率的覆写；场所自身在行情里报告的费率优先（Predict.fun 报告 200）。
- 校验：`order_size > 0`、`max_position >= order_size`、`max_exposure >= max_position`、`fee_bps ∈ [0,1000]`；违反 → `400`。
- 旧客户端只发扁平字段时，`venues` 保持原值不变（读-改-写，不整段覆盖）。

## 5. 系统设置 `GET/PUT /api/v1/system`

追加 `venues`，与扁平字段**双读**（扁平 = Polymarket，兼容旧前端/旧库）：

```json
{
  "data_source": { "gamma_base_url": "…", "clob_base_url": "…" },
  "live": { "private_key": "", "funder": "", "signature_type": 0, "api_key": "", "api_secret": "", "api_passphrase": "" },
  "venues": {
    "polymarket": { "data_source": { "gamma_base_url": "…", "clob_base_url": "…" }, "live": { "…": "同上" } },
    "predictfun": {
      "data_source": { "base_url": "https://api.predict.fun" },
      "live": { "api_key": "", "private_key": "", "smart_wallet": "", "testnet": false }
    }
  }
}
```

- 密钥字段在 `GET` 中一律返回 `""`，另有布尔 `*_configured` 表示是否已保存（沿用现有 `Redacted()` 的语义，绝不回显）。
- `PUT` 中省略或空串 = 保持不变（沿用 `MergeSecret` 语义）。
- Predict.fun 的 `private_key` 是**独立的 EVM 私钥**，不得与 Polymarket 私钥复用；`smart_wallet` 是持有资金的账户地址（EOA 模式下留空）。

`GET /api/v1/system` 的 `runtime` 追加逐场所状态：

```json
{ "runtime": { "venues": [ { "venue": "predictfun", "configured": true, "ready": false,
  "signer": "0x…", "maker": "0x…", "error": "缺少 BNB 手续费", "testnet": true } ] } }
```

## 6. 错误语义

| 场景 | HTTP | body |
| --- | --- | --- |
| 未知 venue 参数 | 400 | `{"error":"未知场所"}` |
| 账户 venue 与机会 venue 不一致下单 | 400 | `{"error":"该机会不属于此账户的场所"}` |
| 场所只读但尝试下单 | 409 | `{"error":"该场所当前只读"}` |
| 场所未配置实盘凭据 | 409 | `{"error":"<venue> 实盘未配置：…"}` |

## 7. 前端必须遵守的渲染规则

1. 每个数字旁边都有场所标识（chip 或列），**不出现无场所归属的金额**。
2. 跨场所"合计"不渲染。若需要总览，只显示逐场所卡片。
3. 机会行"已买入"标记以 `venue + token_id` 为键（两场所的 outcome 标识都是十进制字符串，单独用 token_id 会串号）。
4. 场所筛选变化时不得清空表格（避免骨架屏闪烁）：查询函数需用稳定引用。
5. 费率的展示单位是 bps，但 Predict.fun 实际收费是 `rate × min(p,1-p) × shares`，UI 不要把它显示成"名义额的 2%"这类会误导的换算。

## 8. 实施补充（2026-11 回填）

实现时补齐的约定，按重要性排序：

1. **`venues.predictfun.live.funder` 与 `smart_wallet` 等价**。场所凭据块内嵌了 Polymarket 的 `LiveSettings`，所以 `funder` 字段对非 Polymarket 场所也会出现在 JSON 里；`Normalize()` 会在 `smart_wallet` 为空时用 `funder` 填充，二者都有时以 `smart_wallet` 为准。这样按 Polymarket 习惯填 `funder` 的客户端也不会静默丢失 maker 地址。`signature_type` / `api_secret` / `api_passphrase` 对 Predict.fun 无意义，写入会被忽略。
2. **凭据下推只在条目存在时发生**。`PUT /system` 里**没有** `venues.predictfun` 块时，引擎不会调用执行器的 `ApplyLiveSettings`，凭据保持进程环境变量注入的值。这条是为了让 `PREDICTFUN_*` 环境变量在"没在网页里配过"时继续有效——用零值下推会把 env 里的 `testnet` 冲成 `false`（主网）。
3. **`POST /api/v1/system/live/test?venue=predictfun`** 的 body 是 `{"venues":{"predictfun":{"live":{…},"testnet":true}}}`：用候选凭据试连（`/v1/auth` 登录 + 链上读余额），**成功后把执行器恢复为已保存凭据**，绝不留下未保存的配置。响应沿用 `LiveProbe`：`signer` / `maker` / `api_key`（掩码）/ `source:"chain"` / `mode:"live"` / `cash` / `allowance` / `latency_ms`。
4. **`GET /venues` 的 `supports_settlement=false` 是可渲染的事实**：持仓里属于该场所且已到期的市场不会被自动结算，运行时错误里也不会出现结算失败（引擎按能力分派，不做无效轮询）。前端应显示"该场所暂不支持自动结算"而不是"结算中"。
5. **只读注册是合法的**：`registry.Register(caps, data, nil)` 表示该场所只有行情没有交易。`POST /scan` 会扫描它，`/opportunities` 会展示它，下单返回 `409 {"error":"该场所当前只读"}`。
6. **`?venue=` 允许的取值**是注册表里的场所（当前 `polymarket` / `predictfun`），加上空串与 `all`。注册表之外的字符串（即使 `market.VenueOrder` 里有）返回 `400 {"error":"未知场所"}`。

## 9. 链上授权（Predict.fun 实盘前置条件）

Predict.fun 的订单由交易所合约拉取资金，因此下单前必须存在两笔授权：
USDT 对交易所（含 neg-risk 交易所）的 ERC20 allowance，以及
ConditionalTokens 对交易所的 ERC1155 `setApprovalForAll`。Polymarket 通过代理
钱包完成这一步，因此不需要该端点。

```
GET  /api/v1/system/live/approval?venue=predictfun   # 只读，不发交易
POST /api/v1/system/live/approve?venue=predictfun    # 发送缺失的授权交易
```

两者返回同一结构：

```json
{ "venue": "predictfun", "approval": {
  "venue": "predictfun",
  "collateral": "USDT",
  "balance": 12.5,
  "allowance": 0,
  "shares_ready": false,
  "ready": false,
  "missing": "尚未授权交易所划转结果份额（ERC1155 setApprovalForAll）"
} }
```

- `balance`/`allowance` 已按 USDT 的 18 位小数换算成人类可读金额。
- `missing` 只在 `ready=false` 时出现，前端直接展示，不再自己拼句子。
- 错误：未知场所 / 该场所无需授权（Polymarket）/ 该场所未配置实盘凭据 → `400`；
  其余链上失败（RPC、gas、nonce）→ `502`。
- `POST` 是真实链上交易，钱包需要 BNB 支付 gas；它持有引擎的交易互斥锁，
  不会与签名中的订单交错。前端只在用户点击时调用，绝不自动触发。
- 授权完成后无需重启：执行器在下次读取余额时拿到新的 allowance。

## 10. 链上赎回（Predict.fun）

```
POST /api/v1/accounts/:id/positions/:positionId/redeem
```

无请求体。响应：

```json
{ "venue": "predictfun", "tx_hash": "0x…", "method": "ctf" }
```

`method` 为 `ctf`（普通市场，`CTF.redeemPositions`）或 `neg-risk`（neg-risk 市场，`NegRiskAdapter.redeemPositions`），只用于展示与排障。

前置条件（全部由后端 `engine.RedeemPosition` 校验，前端只需要如实展示错误）：

| 情况 | 状态码 | 错误文案 |
| --- | --- | --- |
|账户不存在|404|`账户不存在: …`|
|模拟账户|400|`…模拟账户的仓位没有链上份额，无需赎回`|
|仓位不属于该账户|400|`仓位不存在: …`|
|仓位未结算（`open`）|400|`…只有已结算的仓位可以赎回（当前状态 open）`|
|场所未声明 `supports_redeem`|409|`…<场所> 不支持链上赎回`|
|场所无实盘执行器 / 未配置凭据|409|`…只读运行，无法执行链上赎回`|
|同一仓位已提交过赎回|400|`…该仓位已经提交过赎回（工单 12）`|
|BNB 余额不足 gas|502|`…赎回需要在 BSC 上支付 gas，但签名地址 0x… 的 BNB 余额不足（当前 0 wei），请先充值`|
|neg-risk 未授权 NegRiskAdapter|502|`…neg-risk 市场赎回需要先授权 NegRiskAdapter 划转结果份额，请在「系统设置 → Predict.fun」执行链上授权`|

行为约定：

1. **先落库再签名**：赎回写一条 `type=settle`、`action=redeem` 的订单（`status=pending`，`reason` 里带工单号），交易提交后改为 `filled` 并把 `tx_hash` 写进 `reason`；失败改为 `rejected` 并写入链上错误。前端在订单列表里看到「链上赎回」。
2. **幂等**：同一仓位只要存在 `pending`/`filled` 的赎回记录就拒绝重发（链上重复赎回只会烧 gas，所以这层只是省钱，不是防双付）。
3. **串行化**：赎回与交易共用引擎的 `tradeMu`，不会与同一时刻的买入/止损交错。
4. **不等待回执**：接口返回的是**已提交**的交易哈希。链上确认由 BNB 网络完成，钱包余额不会立刻变化；前端文案必须写"已提交"，不能写"已到账"。
5. **不自动重试**：赎回失败不会自动重发（gas 与 nonce 的语义交给用户判断）。
