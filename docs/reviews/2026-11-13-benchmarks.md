# 2026-11-13 离线性能基准报告

本报告记录重构完成后的离线基准。初版只新增 `_test.go` 与本文档，不修改生产代码；随后依据本文的查询计划，为订单列表补了两个覆盖索引，并用同一基准复测。索引是本次唯一的生产代码变更，复测数字单列在订单表内。基准不访问网络、真实凭据或 `backend/data/` 生产库。

## 环境

| 项目 | 值 |
| --- | --- |
| 机器 | Mac mini (Mac16,10)，Apple M4，10 核（4 性能核 + 6 能效核），16 GB 内存 |
| 系统 | macOS 27.0.1，Darwin Kernel 27.0.0，arm64 |
| Go | go1.27.3 darwin/arm64 |
| 数据库 | 临时目录中的 SQLite 文件（`modernc.org/sqlite`），WAL，`SetMaxOpenConns(1)` |

单机、未固定 CPU 频率、未隔离后台进程。下面的数字只用于同一台机器上的相对比较，不是生产容量承诺。

## 命令

```bash
cd backend
gofmt -w internal/engine/benchmarks_test.go internal/store/benchmarks_test.go
go vet ./internal/engine ./internal/store
env -u PREDICTFUN_LIVE go test -run '^$' -bench . -benchmem -count=3 ./internal/engine ./internal/store
```

补充验证：

```bash
cd backend
go test ./internal/engine ./internal/store
```

结果：`go vet` 无输出；`gofmt -l` 无差异；基准 3 次全部 `PASS`；两个包现有测试全部通过。基准进程从未读取 `PREDICTFUN_LIVE`，也没有发出网络请求。

## 引擎扫描基准

文件：`backend/internal/engine/benchmarks_test.go`（`package engine`）。

`package engine` 不能导入 `pennyharvester/backend/internal/store`（`store` 反过来导入 `engine`，会形成测试编译环），而现有 `engine_test.go` 中的 `newTestEngineWithRegistry` 等辅助函数属于 `package engine_test`。因此这三个基准在文件内使用最小化的内存仓储、静态行情源和只读场所，不依赖生产数据库或网络。仓储只保留每个账户最新的一条权益快照，扫描重复执行时状态有界；SQLite 持久化成本由下文的 store 基准单独测量。

固定场景：

- 200 个确定性二元市场，ID 为 `bench-market-000` 至 `bench-market-199`。
- Yes 价格 0.960–0.989，No 价格为补价；流动性 20,000–44,500，成交量 50,000，到期时间 2–21 天后，`Active=true`，未设置 `acceptingOrders`（按可交易处理）。
- 默认策略：价格区间 0.95–0.99，最短流动性 10,000，到期上限 30 天，Polymarket 启用。因此每个市场恰好产生一个 Yes 机会，共 200 条。
- 走 `ScanOnly`：解析账户与策略、拉取市场、过滤/分类、合并雷达、刷新持仓（空）、为每个账户写权益快照；不执行止损或自动下单。
- `BenchmarkScanManyMarkets`：1 个 paper 账户，初始资金 10,000。
- `BenchmarkScanTwoAccounts`：2 个 paper 账户，市场集合完全相同，逐账户过滤并各写一条快照。
- `BenchmarkScanVenueFailure`：行情源 `ListOpen` 直接返回错误。扫描仍执行账户维护与快照，最终返回可被 `errors.Is(err, ErrUpstream)` 识别的错误；基准断言错误类型与“每次扫描一条快照”，失败路径不会被基准当成成功。

| 基准 | 场景 | ns/op（中位数） | 3 次范围 (ns/op) | B/op | allocs/op |
| --- | --- | ---: | ---: | ---: | ---: |
| `BenchmarkScanManyMarkets` | 1 账户 / 200 市场 | 1,308,074（1.31 ms） | 1,304,500–1,414,088 | 1,273,847（约 1.21 MiB） | 3,522 |
| `BenchmarkScanTwoAccounts` | 2 账户 / 200 市场 | 2,488,555（2.49 ms） | 2,483,303–2,548,523 | 1,682,996（约 1.60 MiB） | 5,751 |
| `BenchmarkScanVenueFailure` | 1 账户 / 行情源报错 | 2,386（2.39 µs） | 2,353–2,529 | 4,026（约 3.93 KiB） | 43 |

解读：

- 200 个市场的一次完整 `ScanOnly` 约 1.31 ms。两账户为 2.49 ms，约为单账户的 1.90 倍；市场只拉取一次，增量主要来自逐账户过滤、雷达视图和快照，而不是重复网络或重复解压。
- 两账户的分配量从约 1.21 MiB 增至 1.60 MiB，`allocs/op` 从 3,522 增至 5,751，和“每个账户各保留一份机会视图”的结构一致。
- 单场所失败路径约 2.39 µs、43 次分配，因为它不产生任何市场行；账户维护仍然执行，说明失败路径没有在基准里被提前短路。该数字不代表多场所下其他场所继续成功时的成本，也不包含重试/退避策略。
- 三次运行的 ns/op 波动约为：多市场 8.4%、两账户 2.6%、失败路径 7.5%。分配字节数三次仅相差数百至两千字节，`allocs/op` 三次完全一致，适合作为回归基线。

## 订单与持仓查询基准

文件：`backend/internal/store/benchmarks_test.go`（`package store`）。

固定场景：

- 每个基准函数在 `b.TempDir()` 中新建 SQLite 文件，执行真实迁移，得到默认的 live 与 paper 两个账户。
- 订单历史共 10,000 条：主账户 9,900 条，第二账户 100 条。主账户每 10 条有 1 条 `rejected`（共 990 条），其余为 `filled`；`created_at` 从 2026-01-01 起按分钟递增，金额 100、价格 0.96、Yes token。
- `ListOrders` 的典型页为 `limit=100, offset=0`；深页为 `limit=100, offset=9000`（主账户剩余 900 条中的前 100 条）；状态过滤为 `status=rejected, limit=100, offset=0`。
- `ListPositions` 使用独立数据库：前述两个账户，0 条订单，主账户 2,000 条 `open` 持仓；查询为 `status=open`。
- 基准循环只读；每个查询首次调用时校验返回行数（状态过滤还校验每行状态），后续迭代不再做断言。

| 基准 | 场景 | 优化前 ns/op | 优化后 ns/op（中位数） | 3 次范围 (ns/op) | B/op | allocs/op |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| `BenchmarkListOrders/TypicalPage` | 100 条 / offset 0 | 14,072,313（14.07 ms） | 266,735（0.267 ms） | 265,764–274,010 | 167,346（约 163 KiB） | 4,341 |
| `BenchmarkListOrders/DeepPage` | 100 条 / offset 9000 | 20,240,101（20.24 ms） | 794,325（0.794 ms） | 788,599–795,967 | 167,362（约 163 KiB） | 4,343 |
| `BenchmarkListOrders/StatusFiltered` | `rejected` / 100 条 / offset 0 | 2,787,180（2.79 ms） | 275,361（0.275 ms） | 274,833–280,146 | 167,698（约 164 KiB） | 4,345 |
| `BenchmarkListPositions` | 2,000 条 open 持仓 | 6,715,979（6.72 ms） | 6,682,133（6.68 ms） | 6,642,724–6,740,379 | 4,718,614（约 4.50 MiB） | 99,798 |

解读：

- 优化前典型订单页约 14.07 ms，深页约 20.24 ms（约为典型页的 1.44 倍）。深页的额外成本不只是多扫 9,000 行，还包括每次都把账户的匹配行整体排序。典型页与状态过滤页三次运行波动分别约 0.7% 和 0.5%，深页约 1.35%，数字稳定。
- 优化前状态过滤页约 2.79 ms，明显快于典型页：参与排序的只有 990 条 `rejected` 行，而不是 9,900 条。它说明长历史页面的主要成本是“找到匹配行 + 排序”，不是返回 100 行本身。
- 依据查询计划新增 `idx_orders_account_id(account_id, id DESC)` 与 `idx_orders_account_status_id(account_id, status, id DESC)` 后，典型页降至 0.267 ms（约 52.8 倍），深页降至 0.794 ms（约 25.5 倍），状态过滤页降至 0.275 ms（约 10.1 倍）；分配量不变，说明收益来自消除临时排序而不是减少返回行。深页仍随 offset 线性扫描索引，但不再排序 9,900 行。
- 2,000 条持仓的列表读取约 6.72 ms、4.72 MiB、99,798 次分配（约 50 次/行），主要来自多列扫描、字符串构造和每行时间解析；本次没有为它增加索引或缓存，复测数字与优化前基本一致。

### 查询计划

优化前，在模式完全相同的临时 SQLite 库上（10,000 条订单、与迁移一致的索引），三个订单查询的计划一致：

```text
QUERY PLAN
|--SEARCH orders USING INDEX idx_orders_account_event (account_id=?)
`--USE TEMP B-TREE FOR ORDER BY
```

典型页、深页和状态过滤都出现 `USE TEMP B-TREE FOR ORDER BY`。查询按 `id DESC` 排序，而迁移创建的 `idx_orders_account_created` 按 `created_at DESC` 排序；SQLite 选择了 `idx_orders_account_event` 作为 `account_id` 过滤入口，然后对匹配行做临时排序。

新增两个索引后，计划变为：

```text
-- 典型页 / 深页
`--SEARCH orders USING COVERING INDEX idx_orders_account_id (account_id=?)

-- 状态过滤
`--SEARCH orders USING COVERING INDEX idx_orders_account_status_id (account_id=? AND status=?)
```

临时 B-TREE 消失，`id DESC` 由索引顺序直接满足。生产库未执行 `ANALYZE`，实际计划可能随数据分布变化；复测在相同临时库上确认了上述计划，且返回行数断言仍然通过。

## 已知边界

- 单机测量：Mac mini M4 / 16 GB / macOS 27.0.1 / Go 1.27.3；未固定 CPU、未做容器隔离，其他进程和热节流会影响绝对值。
- 引擎基准使用内存仓储与静态行情源，不包含真实 HTTP 请求、JSON 解析、限流、TLS、DNS、翻译接口和事件标签接口；200 个市场也不等于生产上游一次返回的全部市场。
- 引擎基准没有持仓和未确认订单，因此只覆盖空仓扫描的持仓刷新/快照分支；下单、止损、实盘对账不在本次范围内。
- 为保持重复迭代有界，内存仓储只保留每个账户最新快照，不模拟 SQLite 的追加写入和文件增长。
- Store 基准使用临时文件 SQLite、单连接、短文本字段，不含并发读写、WAL 检查点压力、磁盘满/慢盘或百万级历史；10,000 条订单是可重复的“长历史”样本，不代表生产上限。
- 所有数字来自 `-count=3` 的中位数；未做统计显著性检验，也不应直接外推为多用户并发吞吐。
- 库存查询基准暴露的临时排序已用两个覆盖索引修复；除此之外没有修改查询 SQL、扫描逻辑、缓存、配置或前端。`ScanVenueFailure` 只覆盖行情源立即报错；429 的确定性覆盖在 `backend/internal/predictfun/client_test.go:207-246`（`Retry-After` 后重试、连续 429 只重试一次即失败），扫描超时/10 秒节奏的覆盖在 `backend/internal/engine/engine_test.go:451`，两者不产出 ns/op，不混入本表。

## 结论

重构后的扫描与存储读取路径可在离线、确定性、无凭据条件下重复测量。当前基线下：单账户 200 市场扫描约 1.31 ms，两账户约 2.49 ms；行情源完全失败时错误路径约 2.39 µs；10,000 条订单的典型页从 14.07 ms 降至 0.267 ms，深页从 20.24 ms 降至 0.794 ms，状态过滤页从 2.79 ms 降至 0.275 ms。索引修复只针对已观测到的临时排序，持仓读取与扫描分配热点仍保留为后续有数据支撑的候选。
