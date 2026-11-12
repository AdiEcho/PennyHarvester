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

## 仍未完成

计划中的跨模块重构与低优先级项未在本轮全部展开：

- PF-05：授权/赎回的更完整持久化状态机（当前已有 intent、tx hash 与 unknown 状态）。
- WEB-06：报价已按账户与金额失效，但配置 revision 尚未进入缓存 key。
- LED-06：pending 赎回崩溃恢复仍需独立操作表与人工核对入口。
- 性能、查询与模块拆分按计划第 4/7 节后置。

生产数据库未修改；所有 schema 变更只通过代码内迁移，旧库在打开时增量升级。
