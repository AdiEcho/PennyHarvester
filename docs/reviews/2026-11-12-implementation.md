# PennyHarvester 实施记录

基线：`abc13a3142247dab427af74ea45f27738c87a117`。本文记录本次按
[开发计划](./2026-11-12-development-plan.md) 落地的代码与验证证据；未列出的审查项仍按计划
保留，不把“部分完成”写成“全部完成”。

## 已完成

### 行情可信度与结算

- `market.DataHealth`、`Market.FetchedAt/Stale/StaleReason`：缓存命中不刷新
  `fetched_at`；上游失败回退旧快照时标记 stale 并记录原因。
- Gamma 校验价格范围、空/重复 token、outcome 名称；协议按 token 字段推断，未知协议拒绝。
- `finalResolutionData` 只接受明确的 `resolved`，空状态、proposed/disputed/pending 不结算。
- 扫描把 stale 传给止损、结算、手动下单与自动开仓：stale 行仍可见，但禁止新开仓和止损判断；
  stale 的已结算快照只更新标记、不入账。
- `SourceStatus.Health` 暴露各场所数据健康，Dashboard 使用实际来源状态。

### 账本、敞口与恢复

- `orders.event_id` 迁移、索引与 CRUD；legacy 数据库先加列再重建。
- 未确认买单按 `Amount + Fee` 预占敞口，按市场/事件阻断新开仓；未确认卖单也阻断同市场开仓。
- 实盘事件敞口上限；手动实盘下单先对账，失败则拒绝。
- 结算遇到未确认订单时冻结：不改持仓状态、不入账，等待人工对账。
- 迟到的卖/买成交保持 `unknown` 且不记账，提示人工核对。
- 批量行情缺少持仓市场时报告具体 market id，不静默跳过。
- 全部行情发现失败仍执行对账、刷新与止损，最后返回 `ErrUpstream`。
- 账本写入使用不随请求取消的上下文和 15s 超时。
- 赎回意向按 `position_id` 直接查询；查重、创建 pending 与广播在同一 `tradeMu` 临界区，
  并发点击不会广播两笔赎回。
- 赎回/授权错误携带交易哈希；广播或回执结果未知时订单保持 `unknown`，API 返回 `tx_hash`。
- 实盘本金初始化改为 compare-and-set（`baseline_equity = 0`），并发 Dashboard 不覆盖已提交本金。
- 配置应用失败或保存失败都会回滚全部已应用对象；回滚失败时记录阻断原因，下单、授权、赎回和自动开仓
  统一拒绝，直到重新保存成功或重启。

### 交易协议与费用

- Polymarket 卖单 `Fill.Cost` 恢复为毛额，费用单列，引擎只扣一次。
- Predict.fun 买入费用按份额计（collateral / price），卖出费用按计价币计；引擎按净份额记账。
- PF 终局状态要求 `resolved|settled|finalized`、恰好两个 outcome、WON 标签与 resolution 对象一致；
  缺少裁决的终局市场拒绝结算。
- PF 市场分页发送 `after` 游标并检测游标未前进。
- PF 赎回优先按 market id 读取元数据，再回退 token/condition。
- PF 买入/卖出明确捕获 `OrderRejectedError` 为 rejected；超时/传输错误仍走查询，查询不明为 unknown。
- PF 下单使用官方嵌套 DTO（`data.order/pricePerShare/strategy/slippageBps/isMinAmountOut`），
  `pricePerShare` 按 18 位整数发送；201 只代表接受，不再伪造份额或成本。
- PF 撤单使用 `data.ids`，逐单处理 removed/noop/rejected；rejected 不视为成功。
- PF 订单查询同时解析外层状态与内层签名订单；只有终态且成交证据完整才入账。
  按本订单所属 match leg 逐笔累计 VWAP，部分成交不补满，OPEN 不终结，缺证据保持 unknown。
- PF 费用按资产类型换算：买单份额费扣净份额，卖单现金费扣净现金；显式零费不再估算，缺费用保持 unknown。
- 链 ID 与配置网络不一致时拒绝使用 RPC；`PREDICTFUN_RPC_URL` 为空时使用所选网络的默认 RPC。
- PF 认证、市场缓存与余额缓存绑定配置 generation：切换钱包/网络后，旧请求的 token、
  分类结束时间、token 索引与余额都不能写回新配置。

### 配置与 Web

- 系统配置读取-合并-校验-保存进入同一 `tradeMu`；并发局部 patch 不丢字段。
- 平面 `data_source` / `live` 显式 patch 优先于 venue patch；venue-only patch 仍同步到运行时。
- 保存失败按已应用对象逆序回滚（proxy、数据源、PM 执行器、PF 执行器）；回滚失败返回明确错误。
- API Key 在 `Redacted`、`MergeSecret`、venue merge 与 SQLite 中按秘密处理；不再明文返回。
- 密码变更递增 revision，旧认证结果不能创建新会话；登录尝试在 bcrypt 前原子占用配额。
- 可信代理默认不信任；写请求校验同源 Origin / `Sec-Fetch-Site` 与 JSON Content-Type。
- 前端请求不把损坏 JSON 当成功；204/205 才允许空 body；AbortError 向上传播。
- 系统设置草稿、probe、钱包/Relayer 检测均绑定 revision；旧结果和旧 `finally` 不能清理新请求的 busy 状态。
- PF 测试网继承地址不再被前端填成显式主网；probe 使用候选 REST 地址并返回实际 `testnet`。
- 轮询使用请求代次，旧请求不能清理新请求状态或重复计时。
- 下单网络/超时错误显示“订单结果未知”并刷新持仓，而不是“订单未提交”。
- PF probe 返回实际 `testnet`；账户列表暴露 `live_testnet`，Dashboard 区分测试网与主网实盘。
- 收益文案改为“盘口估算净收益”，不再声称是最终真实收益。

## 验证

```text
cd backend
env -u PREDICTFUN_LIVE go test ./...
env -u PREDICTFUN_LIVE go test -race ./...
go vet ./...
cd ../frontend && npx tsc --noEmit && npm run build
```

以上均通过（前端构建只有既有的大 chunk 提示）。

### Polymarket 签名与对账

- 买单按最小价位把预算重算为份额，并逐档抬高签名限价，直到整数 maker/taker 比例仍能跨过卖一；
  解决 SDK 分币截断把 0.98 压成 0.9751 而被交易所拒绝的问题。
- 卖单每次签名前失效 SDK tick 缓存，tick 从 0.01 变为 0.001 后不会继续按旧档位签名；
  `ceilTick` 不再让 0.07/0.01 的浮点噪声多跳一档。
- `LookupOrder` 只按已确认成交逐笔累计份额与金额，并按逐笔价格计算非线性费用；
  不再用 `size_matched × price` 估算，也不再用 VWAP 代替逐笔费用。
- 成交分页有上限、游标必须前进、成交 ID 去重且数量必须与 `size_matched` 一致；
  未确认、失败、缺页、超限都返回 `unknown` 而不是部分入账。
- 模糊提交只走最终性查询；明确拒单在查询前返回 rejected。
- tick 变化、逐笔费用、分页、去重、数量不一致、404/传输错误、旧端点迟到余额响应均有确定性测试。
- 余额缓存绑定配置 generation；旧钱包/旧地址的迟到响应不能刷新新配置的缓存。
- 授权额度按实际结算的普通/负风险交易所读取；其他 spender 的大额授权不再掩盖目标交易所为 0。
- Relayer/CLOB 认证请求遇到重定向直接返回 3xx，不把 `POLY_*` 头转发到其他主机。


### 精度与能力隔离

- `venue.Scale` 改为按最短往返十进制字符串转 `big.Rat`，避免 `0.1` 的二进制噪声放大到 18 位。
- 新增 `ScaleDecimal`/`UnscaleDecimal`/`UnscaleRat`：用户输入的 18 位金额与完整余额可无损往返；
  超出请求精度的文本直接报错，不静默截断。
- `PriceDecimalsForTick` 从十进制字符串推导小数位：`0.025 → 3`、`0.0001 → 4`、`0.5 → 1`，
  不再因浮点比较少算一位。
- 场所注册表对 `OrderTypes` 做深拷贝，调用方修改返回值不再污染注册表。

### 链上操作与恢复

- 新增 `chain_operations` 持久化日志：账号、场所、chainId、from、nonce、tx hash、状态与赎回仓位；
  同一账号/场所的活跃授权、同一仓位的活跃赎回有部分唯一索引，重复提交返回 409。
- 适配器必须先通过 `onSigned` 落库签名哈希，再广播；回调失败不广播，回调前的 `intent` 超时转为 `unknown`。
- 赎回先写链上 intent，再写 UI 工单，再签名/广播；确认、回滚、未知分别落到 `confirmed`/`reverted`/`unknown`，
  未知保留 hash/from/nonce，绝不用新 nonce 自动重发。
- 授权批次逐笔记录；部分成功后重试从缺失额度继续，已确认行不重复发送。
- 扫描在拿 `tradeMu` 前用独立预算恢复链上操作；RPC 失败保持 unknown，不会把未确认当成功。
- `unknown` 会持续阻塞重试，需人工核对链上状态；这是有意的保守语义。

### 钱包验证与剩余账本修正

- 钱包识别返回显式 `verified`、`chain_id`、`owner`、`implementation`、`evidence`；
  只有 Polygon 137、已部署且工厂推导或 owner/signer 证据完整才算验证，未知形态不再默认 Deposit。
- 前端只有完整证据才允许保存代理实盘账户；不完整时显示“未验证，请核对”并阻止保存。
- 模拟止损优先使用本轮扫描的市场费率，仓位离开雷达后不再按账户默认费率少计费用。
- 年化起点只取第一笔 `filled` 自动买入，拒单/未决单不再拉长统计周期。
- 为未决订单增加 `(account_id, id) WHERE status IN ('pending','unknown')` 部分索引；2026-11-13 基准又增加
  `(account_id, id DESC)` 与 `(account_id, status, id DESC)`，消除订单列表的临时排序。
- 授权/赎回响应的写截止时间单独延长到请求时限之外，避免链上已成功但 60 秒 WriteTimeout 丢掉响应。

## 2026-11-13 收尾

- `engine.go` 3,679 → 434 行，按职责拆为 `settings.go` / `scan.go` / `orders.go` / `views.go` / `risk.go`；
  只移动函数体，AST 声明数 119→119，行为等价，全量测试通过。`store/sqlite.go` 1,339 → 494 行（基准修复后 496 行），
  拆为 accounts/orders/positions/settings/equity，纯移动。
- `unknown` 链上操作新增网页端核对入口：过滤/分页、审计字段、`unknown` 条件更新、API 400/404/409、
  桌面表格与移动卡片；确认/回滚不会重发或替换交易，赎回结果同步订单。
- 代理实盘保存改由服务端重新发现并校验 chain 137、funder、钱包类型与 owner/signer 证据；
  直接调用 API 无法写入未验证 Funder。发现结果按 signer/funder/type 短时缓存，空私钥不复用缓存。
- 离线测试隔离：联网 smoke 使用 `//go:build live`，`PREDICTFUN_LIVE` 必须为 `1`；
  CI 只编译 live 包，`make test-live` 显式联网。
- 前端接入 vitest + Testing Library + Playwright；覆盖钱包证据、API 合同、链上核对桌面流程与移动卡片，
  CI 增加浏览器 E2E。`make test` / `make test-e2e` 可复现。
- 新增只读 `ledger-export`（`mode=ro` + `query_only=1`），导出账户/订单/持仓/链上操作/权益快照，
  不读设置与凭据表、不调用 venue API、不修改数据库；对账流程见 `docs/ledger-export.md`。
- 基准报告见 `docs/reviews/2026-11-13-benchmarks.md`；10k 订单典型页 14.07→0.267 ms，深页
  20.24→0.794 ms，状态过滤 2.79→0.275 ms。

复现命令：

```bash
cd backend
env -u PREDICTFUN_LIVE go test -race -count=1 ./...
go test -tags live -run '^$' ./internal/predictfun
env -u PREDICTFUN_LIVE go test -run '^$' -bench . -benchmem -count=3 ./internal/engine ./internal/store
cd ../frontend
npm run typecheck && npm run test && npm run build && npm run test:e2e
```

## 仍未完成（外部验证门）

- Protocol V2 实盘提交：域、结构、地址已核实（见 `docs/reviews/2026-11-13-polymarket-v2-evidence.md`），
  但缺官方 V3 测试向量与 canary CLOB 接受证据，且协议从 2026-11-23 起分阶段激活；
  当前继续识别展示、实盘明确拒单。完成链上 `domainSeparator()` 复核与一笔 canary 签名往返后再接入。
- 真实账户对账/最小金额实盘：需要生产只读凭据、venue 流水与操作者确认；本次只提供只读导出，
  不修改生产库、不伪造对账结果。

生产数据库未修改；所有 schema 变更只通过代码内迁移，旧库在打开时增量升级。
