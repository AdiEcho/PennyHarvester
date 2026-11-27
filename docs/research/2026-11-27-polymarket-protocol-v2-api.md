# Polymarket Protocol V2：API 交易可行性与实现方案

调研日期：2026-11-27。外部依据为官方迁移文档、合约源码、官方 SDK 源码，以及当日对 `clob.polymarket.com` / `gamma-api.polymarket.com` 的只读实测；仓库依据为 2026-11-13 证据档案 `docs/reviews/2026-11-13-polymarket-v2-evidence.md` 与本次集成点盘点。

结论：V2 可以走现有 CLOB API 交易。端点、认证、L2 头、`POST /order` body 结构都不变，唯一必须换的是 EIP-712 签名域（version `"3"` + ExchangeV3）以及抵押/仓位授权路径。Go 侧需要自己实现签名，官方 Go 库不提供 V3。

## 1. 三层 "V2" 必须先分清

| 层 | 生效时间 | 交易合约 | Order 结构 | 域版本 | 抵押品 |
| --- | --- | --- | --- | --- | --- |
| CLOB V1（CTF Exchange） | 2026-04-27 前，已退役 | `0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E` / neg-risk `0xC5d563A3…f80a` | 12 字段 | `"1"` | USDC.e |
| CLOB V2（CTF Exchange V2） | 2026-04-28 起 | `0xE111180000d2663C0091e4f400237545B87B996B` / neg-risk `0xe2222d279d744050d28e00520010520000310F59` | 11 字段 | `"2"` | pUSD |
| Protocol V2（ExchangeV3） | 2026-11-23 起分阶段 | `0xe3333700cA9d93003F00f0F71f8515005F6c00Aa`（单一交易所，无 neg-risk 变体） | 同 11 字段 | `"3"` | pUSD + PositionManager |

Gamma 的 `version:"v2"` 指第三层；SDK 内部 `exchangeVersion: "v2"` 指第二层。仓库现在的实盘签的是第二层（domain version `"2"`，`venue.go:61-62` 的 `0xE111…` / `0xe222…`），不是 `"1"`。之前把现有签名说成 v1 是错的。

## 2. 结论与实测证据

- V2 市场可以用现有 `POST /order` 交易，没有新端点、没有新 header、order body 没有 version 字段。交易所由 `tokenId` 隐含路由。
- `GET /version` 返回 `{"version":2}`，这是 CLOB 版本，不是 Gamma 市场协议版本。签名版本必须按市场的 Gamma `version` 决定，不能读 `/version`。
- V2 市场的 Gamma payload 可能同时有 `clobTokenIds` 和 `positionIds`，必须读 `version` 选择，不能靠字段存在性判断。
- `GET /book?token_id=<positionId>` 返回真实盘口，响应尾部带 `"version":"v2"`；CTF 市场响应不含该字段。
- `GET /clob-markets/<conditionId>` 对 V2 返回 `{"mts":0.01,"mos":5,"tbf":1000,"mbf":1000,"itode":true,"fd":{"r":0.01,"e":1,"to":true},"v":"v2"}`，其中 `v` 是协议版本，`fd` 是费率参数；现有 `FeeSchedule`（读 `fd`）可直接复用。
- `GET /balance-allowance?asset_type=CONDITIONAL-V2&token_id=<posid>` 返回 401 而非 400，说明服务端接受该 asset type；已发布 OpenAPI 的 enum 仍只有 `COLLATERAL|CONDITIONAL`，规范滞后。
- V2 全部市场类型共用一个 ExchangeV3，Binary/NegRisk/Combinatorial 模块按 Position ID 路由；V2 永不走 neg-risk 交易所。

## 3. 合约与域参数（Polygon 137）

| 角色 | 地址 |
| --- | --- |
| ExchangeV3（域 verifyingContract） | `0xe3333700cA9d93003F00f0F71f8515005F6c00Aa` |
| PositionManager（ERC-1155 仓位账本） | `0x006F54F7f9A22e0000CC2AB60031000000ae9fEF` |
| Router（split/merge/redeem） | `0x12121212006e4CD160D18e3f00711DA5c3372600` |
| pUSD（ERC-20，6 位小数） | `0xC011a7E12a19f7B1f670d46F03B03f3342E82DFB` |
| AutoRedeemer | `0xa1200000d0002264C9a1698e001292D00E1b00af` |
| CollateralOnramp（USDC.e→pUSD） | `0x93070a847efEf7F70739046A929D47a521F5B8ee` |
| CTF（未变） | `0x4D97DCd97eC945f40cF65F87097ACe5EA0476045` |

三个订单域，name 均为 `Polymarket CTF Exchange`，chainId 均为 137：

| 市场 | version | verifyingContract |
| --- | --- | --- |
| Gamma `version:"v2"` | `"3"` | `0xe3333700cA9d93003F00f0F71f8515005F6c00Aa` |
| CTF `negRisk:false` | `"2"` | `0xE111180000d2663C0091e4f400237545B87B996B` |
| CTF `negRisk:true` | `"2"` | `0xe2222d279d744050d28e00520010520000310F59` |

Order 类型（V2 与 V3 相同）：

```
Order(uint256 salt,address maker,address signer,uint256 tokenId,uint256 makerAmount,
      uint256 takerAmount,uint8 side,uint8 signatureType,uint256 timestamp,
      bytes32 metadata,bytes32 builder)
```

- `expiration` 只在 wire body，不参与签名；GTC 用 `"0"`，GTD 必须至少 3 分钟后且会提前 60 秒生效。
- `side` 签名用 0/1，body 用 `"BUY"/"SELL"`；`signatureType` 0/1/2/3 语义不变。
- `timestamp` 为 Unix 毫秒；`metadata`、`builder` 为 bytes32，可全零。
- POLY_1271（type 3）需要 `TypedDataSign` 包装：外层是订单域（V3 下 version `"3"`），内层 `DepositWallet` 域 version `"1"`、verifyingContract 为 signer。
- 预期 `ExchangeV3.domainSeparator()` = `0x466c63910185bbd55e8679264200c4e0abdcbb0c6264eb3d41d13326022e095b`（Amoy 为 `0xdf8b5fd2…`），未链上复核。

## 4. 资金路径

- BUY：`pUSD.approve(ExchangeV3, amount)`，额度覆盖本金加费用；签名 maker 为资金持有人。
- SELL：`PositionManager.setApprovalForAll(ExchangeV3, true)`；旧 CTF 授权不适用。
- 余额：pUSD 用 `asset_type=COLLATERAL`；V2 仓位用 `asset_type=CONDITIONAL-V2&token_id=<posid>`，先 `GET /balance-allowance/update` 再读 `/balance-allowance`，从 `allowances[ExchangeV3]` 取授权。
- 手续费：maker 0，taker `C×feeRate×p×(1-p)`，费率来自市场 `fd`；CLOB V2 起费用在撮合时决定，签名里没有 feeRate。
- 最小单与 tick 来自市场：V2 样例 `mts=0.01`、`mos=5`，`itode=true` 表示 250ms taker delay。

## 5. SDK 支持

| 语言 | 包 | V3 |
| --- | --- | --- |
| TS | `@polymarket/clob-client-v2` 1.3.1 / `@polymarket/client` 0.20.1 | 支持；`resolveOrderRouting()` 见 positionID 即置 `exchangeVersion=3` 且强制 `negRisk=false` |
| Python | `polymarket-client` ≥0.12.0 / `py-clob-client-v2` 1.2.2+ | changelog 2026-11-12 只声明 Amoy 80002 |
| Rust | `polymarket_client_sdk_v2` 0.9.1+ | 同上，Amoy 口径 |
| Go | `github.com/polymarket/go-order-utils` | 不支持：main 仍是 version `"1"` + 12 字段，PR #24 自 2026-04 起未合并；无官方 Go CLOB 客户端 |

所以 Go 侧必须自签。现用第三方 `github.com/Cyvadra/polymarket-clob-client` 的 `CreateOrder` 把域、版本、合约全写死为 CLOB V2（`signing.go:23-27,200-209`），不能用于 V3；但它的 `SubmitSignedOrder` 只消费已签结构（`orders.go:168-205`），可原样复用 L2 认证、重试、拒绝错误与响应解析。

## 6. 本仓库集成点

平仓路径：

- `backend/internal/engine/orders.go:920-928`：V2 拒单，替换为按 `Version` 路由。
- `backend/internal/engine/orders.go:433-437`：自动实盘静默跳过 V2，替换为路由。
- `backend/internal/engine/orders.go:998`：唯一签/发单点，现调 `exec.Buy(tokenID, conditionID, …)`，接口缺协议身份。
- `backend/internal/engine/orders.go:637,655`：止损卖出，同样缺协议身份。
- `backend/internal/engine/orders.go:462,977`：预交易授权守卫，只认两个 CTF spender。

执行器：

- `backend/internal/live/live.go:522-618` `Buy`：`planMarketBuy` 定预算/份额，`CreateOrder` 签名，`SubmitSignedOrder` 提交，`lookupAmbiguousSubmit` 对账。
- `backend/internal/live/live.go:620-694` `Sell`：FOK 全量卖出。
- `backend/internal/live/live.go:715-731` `EstimateBuyFee`：走 `FeeSchedule`，V2 的 `fd` 可复用。
- `backend/internal/live/live.go:1196-1243` `collateralBalance`：读 `balance` 与 `allowances` map。
- `backend/internal/live/clob_l2.go:287-340` `tradesForOrder`：按 `market` + `maker_address` 翻页，未用将被弃用的 `asset_id` 过滤。

模型与接口：

- `backend/internal/venue/venue.go:155-177` `Executor` 接口：`Buy/Sell` 只有 tokenID/conditionID，没有 `Version`/`NegRisk`。
- `backend/internal/venue/venue.go:60-87`：CTF spender 常量与 `AllowanceFor(negRisk)`。
- `backend/internal/market/gamma.go:957-989`：已按 `version` 选 `positionIds`/`clobTokenIds`。
- `backend/internal/engine/types.go:1016-1077`：`Opportunity` 已带 `Version`、`TokenID`（V2 时是 positionId）、`NegRisk`。
- `backend/internal/predictfun/sign.go:121-160`：仓库内 EIP-712 参考实现，机制可复用，但字段集与 name/version 是 Predict.fun 的。
- 前端 `frontend/src/views/opportunities.tsx:185-198` 与 `frontend/src/lib/venue.ts:41-71`：V2 只有展示徽标，无能力开关。

## 7. 实现方案

### 波 0：外部证据（不改代码）

1. 用 `POLYGON_RPC_URL` 对 ExchangeV3 `eth_call domainSeparator()`，与预期常量比对；不一致则停。
2. 用 TS SDK 1.3.1 + 临时私钥构造一笔 V3 订单，导出 typed data、digest、signature；本地 recover 出 signer。
3. 对真实 V2 市场抓一份 `/book` 与 `/clob-markets`，确认 `version:"v2"`、`v:"v2"`、`fd` 字段。
4. 只读确认 maker 的 pUSD→ExchangeV3 与 PositionManager→ExchangeV3 授权状态。

### 波 1：离线签名包

新增 `backend/internal/polymarket/order`（纯函数，无网络）：

- 常量：name、version `"3"`、chainId 137、ExchangeV3、PositionManager、pUSD。
- `type Order` 11 字段；`func Sign(key, domain, order) (Signed, error)`，返回 digest 作 orderID。
- 金额与精度完全对齐 SDK `orderAmounts`：6 位小数；BUY FAK/FOK 时 pUSD 向下取 0.01、份额向下取 0.0001；其余方向 pUSD 取 0.0001、份额取 0.01。
- `expiration` 只在 wire。
- type 3 的 `TypedDataSign` 包装，先按 V3 外层 + 内层 `DepositWallet/1` 实现，用波 0 的 TS 向量校验。
- 单测：hard-coded domain separator、digest、recover、wire JSON golden、金额边界。

不引入官方 Go 库；用 `go-ethereum/apitypes`（仓库已有）。

### 波 2：执行器接线

- `venue.Executor` 的 `Buy/Sell` 增加协议上下文（推荐 `venue.Instrument{TokenID, ConditionID, Version string; NegRisk bool}`），同步改 `predictfun` 与调用点。避免用可选接口旁路，否则路由分散。
- `live.Buy`：`Version=="v2"` 时走本地签名，`client.SubmitSignedOrder` 提交，得到 `SignedOrderV2` 同构结构；`v1` 保持 `CreateOrder`。对账、拒绝、模糊提交逻辑复用。
- `live.Sell`：V2 用 PositionManager 余额与 V3 签名；份额来源先按本地持仓，成交后按 `CONDITIONAL-V2` 刷新。
- `EstimateBuyFee`、`planMarketBuy`、`LookupOrder` 不改。
- 授权守卫：`venue` 增 `PolymarketExchangeV3`、`PolymarketPositionManager`；`AllowanceFor` 改为接收协议（v2 选 ExchangeV3，v1 选 CTF/neg-risk）。注意 spender key 大小写归一。

### 波 3：授权与余额

- 新增 Polymarket 的 `Approver`（`venue/approval.go:12-47`），用 `live/wallet.go` 的 JSON-RPC 客户端发 `approve` / `setApprovalForAll`，接入现有 `engine/chain_operations.go` 的记录与人工核对流程。
- BUY 走 pUSD approve；SELL 走 PositionManager setApprovalForAll；兑换/拆分另做，不在本期。
- `/balance-allowance/update` 在授权后与下单前刷新；`CONDITIONAL-V2` 读仓位。

### 波 4：引擎与界面

- 删除 `orders.go:920-928` 的 V2 拒单与 `orders.go:433-437` 的跳过；改为 `v1`/`v2` 路由，未知版本继续拒。
- 自动实盘对 V2 走同一风控：敞口、事件上限、现金、授权上限都不变。
- 前端：`venue.ts` 增加 `supportsProtocolV2` 能力位；徽标文案从“实盘不支持”改为协议标识；未通过波 0 前保持拒单并保留现有文案。
- 文档：README `:61-62`、`:487-488` 与 `.env.example` 同步；新增开关 `POLYMARKET_PROTOCOL_V2` 默认关闭，便于灰度。

### 波 5：验收

离线：

- 域 separator、digest、signature 与 TS SDK 逐字节一致；recover 出同一 signer。
- fake CLOB 断言 `POST /order` body 无 version 字段、`tokenId` 为 positionId、签名 recover 正确；V1 回归不变。
- 金额取整、tick、最小单、余额不足、授权不足、模糊提交、对账均有测试。

链上/联调（需人工与真实资金）：

- `domainSeparator()` 复核。
- 只读确认 pUSD、PositionManager 授权。
- canary V2 市场最小金额 BUY，确认 `/data/orders`、`/data/trades` 状态到 CONFIRMED；再做一次最小 SELL。
- 通过后才允许自动实盘；否则保持开关关闭。

## 8. 风险与未决

1. 官方未发布 V3 测试向量；TS SDK 实现与 Go 自签必须逐字节对齐后才可发单。
2. ExchangeV3 `domainSeparator()` 尚未链上核验。
3. type 3 的 `TypedDataSign` 内层域版本在仓库档案与 SDK 之间存在分歧（外层 V3、内层 1），必须用官方向量确认；未确认前可先只支持 0/1/2。
4. Python/Rust 主网 V3 签名是否放开未声明；TS 1.3.1 已支持。Go 自签不依赖 SDK，但 CLOB 是否接受只能靠 canary 证明。
5. V2 费用、250ms taker delay、非二元市场的细节未实测。
6. 2026-12-14 CLOB 查询弃用：`/data/orders` 用 `asset_id` 时必须带 `market`；本仓库 `tradesForOrder` 已带 `market`，但上线前需复查所有查询。
7. Gamma/CLOB OpenAPI 落后于实现（`resolutionStatus`、`v`、`CONDITIONAL-V2` 未声明），字段以实测为准，解析保持宽松。

## 9. 依赖与工作量

- 波 1 约 1 天，纯离线可测，是主要新增代码。
- 波 2 约 1 天，接口改动波及 `predictfun` 与测试。
- 波 3 约 1 天，链上交易发送与核对接入。
- 波 4 约半天，前端与文档。
- 波 5 依赖外部资金与 canary 市场，无法离线完成。

不能把波 5 的结论写成“已支持”，除非拿到真实成交回执。

## 10. 实现状态（2026-11-27 同日落地）

已实现的波次与证据：

- **波 1（离线签名包）**：`backend/internal/polymarket/order/`。`DomainFor` 按 Gamma 版本选域（v2 → version `"3"` + ExchangeV3，v1 → `"2"` + CTF/neg-risk），`Amounts` 复刻 SDK 的买卖金额取整，type 3 走完整 Poly1271 envelope。测试覆盖：独立 Python（eth-account 0.13.4）向量逐字节比对、与现有 CLOB SDK 在 V2 域上的摘要/签名逐字节比对、Poly1271 结构自检。
- **域分隔符链上核验**：`eth_call` `domainSeparator()`（selector `0xf698da25`）于 `0xe333…00Aa` 返回 `0x466c63910185bbd55e8679264200c4e0abdcbb0c6264eb3d41d13326022e095b`，与文档预期一致；ExchangeV3 / PositionManager / pUSD 均有代码。
- **波 2（执行器路由）**：`venue.ProtocolExecutor` 为可选扩展，`Position.ProtocolVersion` 持久化开仓版本，`live.BuyProtocol/SellProtocol` 本地签 ExchangeV3 订单后仍用 SDK `SubmitSignedOrder` 提交；wire body 由 fake CLOB 测试断言：无 version 字段、`tokenId` 为 positionId、FAK/FOK、签名可恢复。
- **波 3（授权）**：本仓库不代发授权交易，沿用 Polymarket 网页完成 pUSD → ExchangeV3、仓位 → PositionManager；引擎在下单前读 CLOB 的 allowances 映射，V2 固定检查 ExchangeV3，缺授权直接拒单。
- **波 4（前端/文档）**：徽标改为「协议 V2 · ExchangeV3」，不再声称实盘不支持；README 与 `.env.example` 增加 `POLYMARKET_PROTOCOL_V2`（默认 `true`，设 `false` 恢复拒单）。
- **波 5（canary）未完成**：没有真实资金成交回执。因此文档与 UI 都不写“已验证可成交”，只写“已实现并通过离线/链上域校验”。

与第 7 节原计划的差异：

1. 未改造 `venue.Executor` 的 `Buy/Sell` 签名，而是新增可选接口，避免波及 `predictfun` 的 18 处调用点。
2. 卖出协议版本从持仓行读取，而不是重新查 Gamma；这样离线止损也不会签错域。
3. 风控仍按单一 `POLYMARKET_PROTOCOL_V2` 开关，不按市场灰度；V2 授权不足时由额度检查拒单。

风险条目更新：第 2 条（域分隔符未核验）已关闭；第 1、3、4、5 条仍开放，其中第 4 条只能由 canary 关闭。
