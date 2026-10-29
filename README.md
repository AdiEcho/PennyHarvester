# PennyHarvester

> Don't chase 100x. Harvest the last cent.

一个围绕 Polymarket **0.95–0.99 高价格二元结果**构建的策略执行系统。后端使用 Go/Gin，前端使用 React/Vite/TypeScript 与 shadcn 风格组件，SQLite 持久化账户、持仓、订单和权益快照。

系统默认使用**实盘账户**：配置钱包凭据后，订单会真实提交到 Polymarket CLOB。另有一个独立的**模拟账户**，使用虚拟资金在本地记账，用于策略测试，不会发送任何真实订单。

> **风险提示：实盘会使用真实资金。** 0.99 不等于无风险；高价结果失败时可能损失接近全部本金。请先使用模拟账户验证策略，再用最小金额测试实盘链路。

## 账户模型

- **实盘账户（默认）**：`mode=live`。通过服务端环境变量中的私钥派生 CLOB API 凭据，使用 EIP-712 签名提交真实订单。每个实例只允许一个实盘账户，且不可删除。
- **模拟账户**：`mode=paper`。订单只写入本地 SQLite，按可配置滑点、费用和风控规则模拟成交。可以创建多个；删除模拟账户时会一并清除其本地持仓、订单和权益记录。
- 两个账户的持仓、订单和权益曲线完全隔离。
- 自动交易按账户独立开启。实盘自动交易默认关闭，开启时需要二次确认，并要求已配置实盘凭据。
- 每个实例首次启动会自动创建「实盘账户」和「模拟账户」，默认账户为实盘。

## 已实现

- Polymarket Gamma 公开只读行情，支持 JSON 数组和 JSON 编码数组字段
- 行情不可用时保持持仓不变并明确报错，不会用占位数据替代真实行情
- 二元市场扫描：参考价、到期天数、流动性过滤
- **实盘执行**：Polymarket CLOB V2 订单签名、FAK 市价买入、成交回报、CLOB 订单号审计
- **实盘可靠性**：提交前持久化待确认订单；超时/未知订单在后续扫描中按 CLOB 状态对账，避免真实成交丢失
- **实盘手续费**：按 CLOB `FeeSchedule` 计算 taker fee，并计入持仓成本、现金和敞口
- **模拟执行**：可配置滑点、费用、单笔金额、单市场/总敞口和现金约束
- 自动实盘/模拟买入、手动买入、持仓盯市、0/1 结算及 50/50 分摊退款
- 已实现/未实现盈亏、胜率、策略可用现金、钱包 USDC、总敞口与权益曲线
- 手动扫描默认只刷新行情，不会触发自动下单；后台引擎按账户开关执行自动交易
- 机会雷达、持仓、订单日志、账户管理、策略设置、引擎启停和数据源状态
- SQLite 持久化、优雅停机、请求体限制、安全响应头和本地监听默认值
- 后端单元/API 测试、竞态检测及前端生产构建

## 策略口径

设公开参考价为 `p`，订单名义金额为 `A`。

**模拟账户**（滑点 `s`、费用 `fee_bps`）：

```text
模拟成交价 p' = p × (1 + s)
模拟份额     q = A / p'
模拟费用     f = A × fee_bps / 10000
模拟成本     c = A + f
```

**实盘账户**：

```text
订单类型   FAK（立即成交，剩余取消）
买入金额   A USD
成交价格   由 Polymarket 当前订单簿决定
成交份额   以 CLOB 返回的 takingAmount 为准
名义成本   以 CLOB 返回的 makingAmount 为准
实盘费用   fee = FeeSchedule.Rate × shares × (p × (1-p))^Exponent
实际成本   名义成本 + 实盘费用
```

如果计入滑点后的模拟价格超过 `max_price`，模拟订单会被拒绝。实盘会在提交前检查盘口价格是否超过 `max_price`，并用预估手续费预留现金和敞口。界面中的年化值是**简单年化理论值**，不是复利预测，也不是收益承诺。

模拟账户的 `initial_bankroll` 只在创建账户时写入该账户的基准权益；之后修改全局默认值不会重算已有账户的历史现金或收益率。

Paper 费用使用固定 `fee_bps` 近似，不复制 CLOB 的 `p × (1-p)` 费率公式；需要更接近实盘时应手动提高 `fee_bps`。

默认参数：

| 参数 | 默认值 |
|---|---:|
| 参考价区间 | 0.95–0.99 |
| 最长到期时间 | 30 天 |
| 最低流动性 | 10,000 USD |
| 单笔金额 | 100 USD |
| 单市场/总敞口 | 250 / 1,000 USD |
| 模拟初始资金 | 10,000 USD（仅新建模拟账户） |
| Paper 滑点 | 20 BPS |
| Paper 费用 | 0 BPS（建议按目标市场费率配置） |
| 实盘手续费 | 按市场 `FeeSchedule` 自动计算 |
| 扫描间隔 | 60 秒 |
| 默认账户 | 实盘 |
| 自动交易 | 关闭（按账户开启） |

## 快速开始

要求：Go 1.26.6+、Node.js 20.19+（或 22.12+）、npm。

### 1. 启动后端

```bash
cd backend
go run ./cmd/server
```

默认监听 `127.0.0.1:8080`，读取 `https://gamma-api.polymarket.com`。首次扫描可能需要几秒。

> 不配置 `POLYMARKET_PRIVATE_KEY` 时，默认实盘账户会显示「实盘未配置」，所有实盘下单会被明确拒绝，不会静默回退到模拟账户。此时可以直接切换到「模拟账户」测试策略。

### 2. 配置实盘交易（可选）

实盘凭据只从环境变量读取，不会写入数据库或前端：

```bash
export POLYMARKET_PRIVATE_KEY=0x...          # 必填：签名私钥
export POLYMARKET_SIGNATURE_TYPE=0           # 0=EOA，1=Proxy，2=Safe，3=Deposit/Poly1271
export POLYMARKET_FUNDER=0x...               # 非 EOA 签名类型必填；EOA 模式必须留空
# export POLYMARKET_API_KEY=...              # 可选：已有 CLOB L2 凭据时直接使用
# export POLYMARKET_API_SECRET=...
# export POLYMARKET_API_PASSPHRASE=...
```

未提供 API 凭据时，服务会在首次实盘操作时自动从私钥派生 CLOB 凭据。私钥应放在进程管理器、系统密钥环或专用密钥文件中，不要提交到 Git。

> 实盘账户的策略「可用现金」= 首次同步的钱包 USDC 基准 + 已实现盈亏 − 未平仓成本。界面同时显示真实钱包 USDC 余额。该口径假设这个钱包专用于本策略；如果钱包还有其他手动持仓，策略资金口径会偏离真实余额。实盘账户的 `maker` 可能是 EOA signer，也可能是 `POLYMARKET_FUNDER` 指向的代理/Safe 钱包；设置页会分别显示。EOA 模式不要设置 `POLYMARKET_FUNDER`。

### 3. 启动前端

另开终端：

```bash
cd frontend
npm install
npm run dev
```

访问 <http://localhost:5173>。Vite 将 `/api` 代理到 `127.0.0.1:8080`。如需指向其他后端，可设置 `VITE_API_BASE_URL`（默认 `/api/v1`）。

也可以从仓库根目录使用 `make dev-backend` 和 `make dev-frontend`。

### 4. 一键构建并运行

从仓库根目录执行：

```bash
make run
```

该命令会先安装缺失的前端依赖、构建最新的前端与后端二进制，然后以单端口方式启动。访问 <http://127.0.0.1:8080>。

可覆盖默认配置：

```bash
ADDR=127.0.0.1:9000 DATABASE_PATH=./data/research.db make run
```

手动执行等价步骤：

```bash
cd frontend && npm run build
cd ../backend
WEB_DIR=../frontend/dist go run ./cmd/server
```

## 配置

后端仅通过环境变量配置：

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `ADDR` | `127.0.0.1:8080` | HTTP 监听地址 |
| `DATABASE_PATH` | `./data/pennyharvester.db` | SQLite 文件 |
| `GAMMA_BASE_URL` | Gamma 公共地址 | 公共行情 API 地址 |
| `WEB_DIR` | 空 | 可选的前端构建目录 |
| `POLYMARKET_PRIVATE_KEY` | 空 | 实盘签名私钥；为空则实盘不可用 |
| `POLYMARKET_SIGNATURE_TYPE` | `0` | `0` EOA、`1` Proxy、`2` Safe、`3` Deposit/Poly1271 |
| `POLYMARKET_FUNDER` | 空 | 非 EOA 签名类型对应的钱包地址；EOA 模式必须留空；兼容 `POLYMARKET_MAKER_ADDRESS` |
| `POLYMARKET_CLOB_URL` | `https://clob.polymarket.com` | CLOB API 地址 |
| `POLYMARKET_API_KEY` | 空 | 可选，已有 L2 API Key |
| `POLYMARKET_API_SECRET` | 空 | 可选，已有 L2 API Secret |
| `POLYMARKET_API_PASSPHRASE` | 空 | 可选，已有 L2 API Passphrase |

示例见 [.env.example](.env.example)。项目不自动加载 `.env`，请通过 shell 或进程管理器注入。

> API 默认无认证。不要直接暴露到公网；如需远程部署，应自行增加 TLS、认证、授权和访问控制。私钥权限等同于钱包控制权，务必使用专用小额钱包并限制主机访问。

## API

统一前缀：`/api/v1`。除账户列表外，账户相关接口都接受 `account_id` 查询参数；省略时使用默认账户。

| 方法 | 路径 | 用途 |
|---|---|---|
| GET | `/health` | 服务健康、默认账户模式、实盘配置与就绪状态 |
| GET | `/accounts` | 账户列表（含实盘配置状态、signer 与 maker） |
| POST | `/accounts` | 创建账户，Body: `{"name":"...","mode":"paper"}` |
| PATCH | `/accounts/:id` | 更新名称、自动交易或默认账户 |
| DELETE | `/accounts/:id` | 删除模拟账户，并级联删除其持仓、订单和权益记录 |
| GET | `/dashboard?account_id=1` | 指标、引擎、数据源、权益快照 |
| GET | `/opportunities` | 最近一次成功扫描的机会 |
| POST | `/scan` | 默认只刷新行情、持仓和权益，不自动下单；`?execute=true` 时同时执行自动交易 |
| GET | `/positions?account_id=1&status=open` | 持仓，支持 `all/open/won/lost/split` |
| GET | `/orders?account_id=1&status=all&limit=100&offset=0` | 订单日志；`status` 支持 `all/filled/settled/rejected/pending/unknown/unresolved` |
| GET / PUT | `/settings` | 读取/更新全局策略与风控参数 |
| POST | `/engine/toggle` | 启停后台扫描，Body: `{"running":false}` |
| POST | `/accounts/:id/orders` | 创建实盘或模拟买入订单 |

订单示例：

```json
{
  "market_id": "123",
  "outcome": "Yes",
  "token_id": "public-reference-token-id",
  "amount": 100
}
```

错误统一返回 `{"error":"..."}`。行情失败返回 502，参数或风控拒绝返回 400，实盘未配置返回 503，账户冲突返回 409。

## 数据库迁移

启动时会自动迁移旧 SQLite 库：为 `positions` 增加 `kind` 列与 `split` 状态，为 `orders` 增加 `pending/unknown` 状态，并为旧模拟账户补写一次基准权益。迁移在事务中执行，失败会回滚；建议升级前备份 `pennyharvester.db`。

## 验证

```bash
cd backend
go test -race ./...
go vet ./...

cd ../frontend
npm run build
```

或从根目录运行 `make test`、`make build`。

## 边界与风险

- **实盘会真实动用资金。** 自动交易默认关闭；开启前请先用模拟账户和最小实盘金额验证。
- **0.99 不等于无风险。** 买入高价结果的最大收益有限，但失败时可能损失接近全部本金。
- Gamma 的 `outcomePrices` 是公开参考数据，不是可成交的订单簿卖价。点差、深度和真实费用可能显著改变结果。
- 实盘成交价由 CLOB 订单簿决定，可能与界面上的指示价格不同；FAK 可能部分成交或完全不成交。
- 实盘下单前需要在 Polymarket 完成 USDC 授权；系统会读取 allowance 并拒绝授权额度不足的订单。
- 实盘手续费由 CLOB 按市场费率收取，并可能随市场规则变化；本系统按 `FeeSchedule` 估算和记账，最终以钱包实际扣款为准。
- 实盘订单可能因超时、网络中断或 CLOB 异步提交而进入 `unknown` 状态。系统会持久化待确认订单并在后续扫描中对账，但在对账完成前不会继续为该账户自动下单。
- 实盘持仓的结算按公开 0/1 或 50/50 结果在本地记账。链上奖金领取、跨系统资金对账不在本系统范围内；结算后的奖金可能仍需在 Polymarket 手动领取。
- 50/50 分摊结算在持仓中标记为 `split`（分摊退款），不计入胜率分子或分母。
- 模拟账户的绩效不是历史回测，也不证明策略具有正期望。
- 市场可能延期、争议、取消或按特殊规则结算。公开结算数据异常时，持仓会保持未结算并显示错误，而不是猜测结果。
- 单次扫描最多读取 500 个开放市场；引擎使用本地 SQLite，未设计为多实例部署。多个实例各自使用同一钱包会造成重复下单。
- 使用 Polymarket 数据或交易时，应遵守适用的服务条款和地区规定。

## License

PolyForm Noncommercial 1.0.0，详见 [LICENSE](LICENSE)。
