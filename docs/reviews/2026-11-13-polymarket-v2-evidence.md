# Polymarket Protocol V2 调研结论（2026-11-13）

结论：V3 签名的域、结构、地址与参考实现已公开，代码可实现；但真实 V2 订单提交要等 2026-11-23 起的分阶段上线，且仍缺官方向量/测试网流程。当前系统继续“识别展示、实盘拒单”，不猜签名域。

## 合约（Polygon 137）

| 角色 | 地址 |
| --- | --- |
| ExchangeV3 代理 | `0xe3333700cA9d93003F00f0F71f8515005F6c00Aa` |
| PositionManager 代理 | `0x006F54F7f9A22e0000CC2AB60031000000ae9fEF` |
| pUSD | `0xC011a7E12a19f7B1f670d46F03B03f3342E82DFB` |
| CTF（未变） | `0x4D97DCd97eC945f40cF65F87097ACe5EA0476045` |
| CTF Exchange V2 | `0xE111180000d2663C0091e4f400237545B87B996B` |
| Neg Risk Exchange V2 | `0xe2222d279d744050d28e00520010520000310F59` |
| Router 代理 | `0x12121212006e4CD160D18e3f00711DA5c3372600` |
| CollateralOnramp | `0x93070a847efEf7F70739046A929D47a521F5B8ee` |
| CollateralOfframp | `0x2957922Eb93258b93368531d39fAcCA3B4dC5854` |
| USDC.e | `0x2791Bca1f2de4661ED88A30C99A7a9449Aa84174` |

只按代理地址集成；文档中的 implementation 地址已过期，代理曾于 2026-10-28 升级。

## EIP-712

- V2 市场 `version:"v2"`：domain `{ name: "Polymarket CTF Exchange", version: "3", chainId: 137, verifyingContract: ExchangeV3 }`。
- 旧市场 `version:"v1"`：domain version `"2"`，verifyingContract 为对应 CTF Exchange / Neg Risk Exchange。
- Order 签名字段（11 个）：`salt, maker, signer, tokenId, makerAmount, takerAmount, side, signatureType, timestamp, metadata, builder`。
- `expiration` 只在 POST /order wire body 中，不参与签名。
- `timestamp` 为 Unix 毫秒；`side` 0=BUY/1=SELL；签名类型 0/1/2/3 不变。类型 3 外层 `TypedDataSign` 的 domain 在 V3 下 version 也为 `"3"`。
- ExchangeV3 的 `domainSeparator()` 应为 `0x466c63910185bbd55e8679264200c4e0abdcbb0c6264eb3d41d13326022e095b`（Amoy 为 `0xdf8b5fd2...`）。上线前用 `eth_call` 复核。

## 抵押品与仓位

- 订单以 pUSD 计价（6 位小数），费用也以 pUSD 收取；pUSD 由 USDC.e 经 Onramp 包装。
- BUY：`pUSD.approve(ExchangeV3, amount)`，额度要覆盖本金加费用。
- SELL：`PositionManager.setApprovalForAll(ExchangeV3, true)`；旧的 CTF 授权不适用。
- V2 仓位 ID 是 PositionManager 的 ERC-1155 tokenId，按不透明十进制字符串处理。Gamma `version:"v2"` 时用 `positionIds`（数组），`version:"v1"` 时用 `clobTokenIds`（JSON 字符串）；两者不可混用。
- 拆分/合并/赎回走 Router；拆分需 pUSD 授权 Router，合并/赎回需 PositionManager 授权 Router。

## 上线时间

- CLOB V2 已于 2026-04-28 上线；旧 V1 SDK/签名不再支持。
- Protocol V2（ExchangeV3 + PositionManager）分阶段：2026-11-23 Phase 1、11-24 Phase 2、12-01 Phase 3。现有 CTF 市场继续使用旧合约，双路径必须按市场 `version` 路由。
- 第三方“11-09 已上线”的说法与官方排期冲突，不采用。

## 未解缺口

1. 官方未发布 V3 测试向量；第三方只有 V2 domain 的摘要，不能验证 V3。
2. 官方文档没有 V2 测试网/Amoy pUSD 流程；Amoy ExchangeV3 只出现在 SDK 源码/PR 中。
3. GitHub `polymarket-v2-external/docs/exchange.md` 的 domain 仍写旧值，与链上合约矛盾；以链上 `domainSeparator()` 为准。
4. 未验证“Relayer 只接受无限额度授权”的第三方说法。

## 上线前最低外部证据

1. 对 ExchangeV3 执行 `eth_call domainSeparator()`，与预期常量一致。
2. 用官方 SDK 构造一笔 V3 订单，本地 recover 出 signer，并在 canary V2 市场确认 CLOB 接受。
3. 抓一份 Gamma `version:"v2"` 且 `positionIds` 非空的真实 payload，锁定路由。
4. 链上确认 maker 的 pUSD→ExchangeV3、PositionManager→ExchangeV3（及 Router）授权。
5. 固定 SDK 版本；优先统一 SDK `@polymarket/client` ≥0.8.0，`@polymarket/clob-client-v2` 已弃用。

在上述 1–2 完成前，本仓库不发送 V2 订单；`version:"v2"` 仅展示并明确拒绝实盘下单。
