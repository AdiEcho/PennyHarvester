# 2026-11-13 收尾开发计划

基线：`03cb140`。目标是把 `docs/reviews/2026-11-12-development-plan.md` 中仍未完成、且可在离线环境验证的开发项收口。真实账户对账与 Protocol V2 实盘提交需要外部凭据/官方向量或激活后的 canary 验证，不作为本次可伪造的“完成”。

状态（2026-11-13）：CMP-01/02/03/04/05/06/07 已完成并验证；独立复审提出的高危/中危问题已修复。V2 证据见 `docs/reviews/2026-11-13-polymarket-v2-evidence.md`，实盘仍明确拒单。

## 波次

| 波次 | 任务 | 验收 |
| --- | --- | --- |
| 1 | CMP-01 链上操作人工核对：仓储过滤/分页、`unknown → confirmed/reverted` 审计、API、前端列表与核对弹窗 | 迁移可重复执行；非 `unknown` 不得改写（同结果幂等）；确认必须有哈希；赎回结果同步订单；API 400/404/409；前端可完成一次核对 |
| 1 | CMP-02 代理钱包服务端校验：保存前由服务端重新发现并核对 Polygon 137、funder、签名类型、owner/signer 证据；成功结果短时缓存 | 直接调 API 无法写入未验证 Funder；缓存不跨 funder/type；EOA 不触发网络 |
| 2 | CMP-03 离线测试隔离：live smoke 使用 `//go:build live`，环境变量 `0` 不再触发 | 普通 `go test ./...` 不编译联网测试；`make test-live` 显式运行 |
| 2 | CMP-04 前端行为测试与浏览器 E2E：API 错误/取消语义、钱包证据、链上操作核对；确定性 mock API | `npm run test`、`npm run test:e2e` 通过；CI 接入 |
| 3 | CMP-05 模块拆分：按职责移动 `engine.go` 顶层声明，行为等价；风险准入/账本查询提取边界 | 外部 API 不变；全量测试通过；`engine.go` 只保留装配与稳定入口 |
| 3 | CMP-06 代表性基准：多市场、多账户、长订单历史、单场所失败、429/延迟；记录扫描分段、查询与内存 | 基准可复现；报告给出机器、命令、数字；只做有数字支撑的低风险优化 |
| 4 | CMP-07 文档与对账导出：README 测试/运维入口、只读账本导出、实现状态、基准与残余边界 | 文档与代码一致；导出不写库、不含密钥；不把未验证能力写成已支持 |

## 外部验证门

- Protocol V2（ExchangeV3、EIP-712 domain v3、pUSD/PositionManager）：域、结构、地址已核实并记录；仍缺官方 V3 测试向量与 canary CLOB 接受证据，且协议分阶段激活（2026-11-23 起）。在完成链上 `domainSeparator()` 复核与一笔 canary 签名往返前，继续识别展示、实盘拒单；不猜签名域。
- 真实账户对账/最小金额实盘：需要生产只读凭据与操作者确认。代码侧只提供可审计导出/差异工具，不修改生产库。

## 证据包

每个任务附：变更文件、触发用例、失败/通过断言、实际命令与结果、未验证边界。所有数据库测试使用临时/内存 SQLite；不触碰 `backend/data/`。


## 收口证据

| 项 | 结果 |
| --- | --- |
| CMP-01 链上核对 | 仓储迁移/过滤/分页、`unknown` 条件更新、审计字段、API 400/404/409、前端桌面/移动端核对流程与 E2E 已通过；账户筛选增加“全部账户 / 场所级”入口，`account_id=0` 的授权记录可见可核对；人工核对与赎回订单状态在同一 SQLite 事务内提交，失败时链上记录回滚为 `unknown` 以保留重试路径；原交易哈希只读，哈希更正写入核对说明 |
| CMP-02 钱包校验 | 保存前服务端重新发现并校验 chain 137、funder、类型、owner/signer；直接 PUT 未验证 Funder 返回 400 且不落库；缓存按 signer/funder/type 绑定，空私钥不复用缓存 |
| CMP-03 离线隔离 | `//go:build live`，`PREDICTFUN_LIVE` 必须精确为 `1`；CI 只编译 live 包，`make test-live` 显式联网 |
| CMP-04 前端/E2E | vitest 17 项、Playwright 4 项（工作台 smoke、桌面链上核对、移动端卡片、场所级授权全账户核对）；CI 接入 |
| CMP-05 模块拆分 | `engine.go` 3,679 → 434 行，拆为 settings/scan/orders/views/risk；仅移动，AST 声明 119→119、函数体逐字节一致；`store/sqlite.go` 1,339 → 494 行（基准修复后 496 行），拆为 accounts/orders/positions/settings/equity；全量测试通过 |
| CMP-06 基准 | `docs/reviews/2026-11-13-benchmarks.md`：200 市场单/双账户、单场所失败、10k 订单典型/深页/状态过滤、2k 持仓；按查询计划新增两个订单覆盖索引，典型页 14.07→0.267 ms、深页 20.24→0.794 ms、状态过滤 2.79→0.275 ms，分配量不变；429 重试与扫描超时由现有确定性测试覆盖，不计 ns/op |
| CMP-07 文档/导出 | `backend/cmd/ledger-export` 只读导出 + `docs/ledger-export.md`；README 增加测试、live、E2E、导出入口；V2 边界说明保持一致 |
