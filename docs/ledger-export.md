# 本地账本导出（ledger-export）

`ledger-export` 是一个只读的本地对账工具。它把 SQLite 中的账户、订单、持仓、链上操作和权益快照导出为版本化 JSON，供人工或脚本与交易所/预测市场导出的流水进行核对。

它**不会**调用任何 venue API，**不会**修改数据库，也**不会**修复或补齐账本数据。发现差异后应记录到独立的对账记录中，再由人工确认处理方式。

## 用法

在 `backend/` 目录下运行：

```bash
go run ./cmd/ledger-export \
  -db data/pennyharvester.db \
  -out ledger-export.json \
  -pretty
```

参数：

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-db` | `data/pennyharvester.db` | SQLite 数据库路径，始终以只读方式打开 |
| `-account` | `0` | 只导出指定账户 id；`0` 表示导出全部账户；账户不存在时命令报错 |
| `-out` | 空 | 输出文件；为空时输出到 stdout。写文件使用临时文件 + rename 原子替换 |
| `-pretty` | `false` | 使用两空格缩进；默认输出紧凑 JSON |

示例：

```bash
# 导出全部账户到 stdout
go run ./cmd/ledger-export

# 只导出账户 2，并写入文件
go run ./cmd/ledger-export -account 2 -out account-2.json -pretty
```

成功时 stderr 保持为空；指定 `-out` 时 stdout 也为空，便于安全地重定向或由脚本调用。

## 输出结构

顶层字段：

- `schema_version`：当前为 `1`
- `generated_at`：UTC 生成时间
- `database`：数据库文件名（不含目录）
- `accounts`：按账户 id 升序排列的数组

每个账户包含身份/模式/venue/currency/默认账户标记、`baseline_equity`、创建时间，以及按时间（同一时间按 id）升序排列的：

- `orders`：`external_id`、`status`、`amount`、`price`、`shares`、`fee`、`reason`、`created_at` 等订单字段
- `positions`：`status`、`shares`、`cost`、`entry_price`、`current_price`、`fee`、开平仓时间，以及数据库中存储的已实现/未实现 PnL 字段
- `chain_operations`：`from_address`、`nonce`、`tx_hash`、`status`、`reason`，以及 `resolved_by`、`resolution_note`、`resolved_at` 等处理审计字段
- `equity_snapshots`：`time`、`value`

每个账户还包含 `status_counts`，只统计该账户下各表的状态数量，不计算现金、总 PnL 或其他派生指标。

导出只读取账本表，不读取 `settings`、`system_settings`、`account_settings`、`access_password` 等设置/凭据表，因此不会包含私钥、API secret 或密码哈希。

如果数据库来自较旧的版本，缺少可选的链上审计列或 `orders.external_id` 等列时，导出会通过 `PRAGMA table_info` 自动跳过不存在的列，仍输出固定字段名（缺失列使用对应的零值、空字符串或 null）。

## 对账流程

1. **导出本地账本**：使用 `-out` 生成带时间戳的 JSON，例如 `ledger-2026-11-13.json`。
2. **取得 venue 流水**：从交易所/预测市场后台导出同一时间范围的成交、结算、充值/提现或链上交易记录。`ledger-export` 不会代你调用 venue API。
3. **按身份匹配**：
   - 订单优先用 `orders.external_id` 与 venue 订单号匹配；
   - 链上操作优先用 `chain_operations.tx_hash`（以及 `chain_id`、`nonce`、`from_address`）匹配；
   - 没有外部 id 的订单再结合 `market_id`、`token_id`、方向、金额和时间窗口人工匹配。
4. **逐项比较**：状态、成交金额/价格/份额、手续费、时间戳、结算结果，以及链上操作的状态和人工处理审计字段。权益快照只用于辅助核对时间点余额，不代表 venue 的正式账单。
5. **记录差异**：把无法匹配、状态不一致、金额/费用不一致、链上失败但本地已结算等情况写入独立对账记录，附上本地 id、venue id/`tx_hash`、差异说明和处理人。
6. **人工确认后再处理**：本工具不修复数据。需要修正账本时，应由人工确认后通过正常业务流程处理，并保留变更记录。

只读性说明：命令使用 `mode=ro` 与 `PRAGMA query_only=1` 打开数据库，并在导出前后保持源数据库文件不变；测试会校验这一点。请勿把导出文件写回数据库路径。

导出内部使用一个只读事务，账户、订单、持仓、链上操作和权益快照来自同一个 SQLite 快照，不会读到跨表不一致的中间状态。若服务正在写入，最新数据仍可能只在 WAL 中；需要固定证据时先停止服务，或把 `pennyharvester.db`、`pennyharvester.db-wal`、`pennyharvester.db-shm` 三个文件一起复制到同一目录再导出。只复制主库文件会漏掉未检查点的数据。
