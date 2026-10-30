# PennyHarvester

> Don't chase 100x. Harvest the last cent.

**PennyHarvester** 是一个围绕 Polymarket **0.95–0.99 高胜率尾盘二元市场**构建的策略执行与模拟演练工作台。它不追逐高赔率的远期博弈，而是聚焦于临近结算、结果趋于明朗的二元事件，通过价格区间、到期时间、盘口流动性与多级敞口风控，系统化地筛选并捕捉「最后几美分」的尾盘收益。

后端采用 **Go / Gin** 编写，前端基于 **React / Vite / TypeScript** 与 shadcn 风格组件构建，使用 **SQLite** 在本地持久化账户、持仓、订单流水与净值快照，支持前后端一键打包、单端口运行。

> **风险提示：价格 0.99 也不代表稳赚不赔。**
> 系统默认打开**实盘账户**，配置钱包凭据后买单会直接提交至 Polymarket CLOB 并动用真实资金；一旦尾盘事件爆冷反转，可能损失绝大部分本金。建议先用内置的**模拟账户**零成本验证策略思路，再以最小金额测试实盘链路。

---

## 双轨账户模型

系统内置「实盘」与「模拟」两套完全隔离的账本，首次启动时会自动创建默认账户：

- **实盘账户（默认，`mode=live`）**
  通过网页「系统设置」或服务端环境变量提供的私钥派生 CLOB L2 API 凭据，使用 EIP-712 签名向 Polymarket 提交真实订单。每个实例仅允许存在一个实盘账户，且不可删除。若未配置私钥，实盘下单会被明确拦截并提示原因，**绝不会静默回退到模拟盘**，避免误判交易状态。
- **模拟账户（`mode=paper`）**
  实时同步 Polymarket 公开行情，但使用虚拟资金在本地 SQLite 中记账，按可配置的滑点、手续费与风控规则模拟撮合，不会发出任何真实订单。支持创建多个模拟账户以对比不同参数方案；删除模拟账户时会一并清理其持仓、订单和净值记录。
- **独立隔离与安全开关**
  不同账户的持仓、订单流水与权益曲线完全隔离。自动交易按账户独立控制；其中实盘自动交易默认关闭，开启时需经过二次确认，且要求实盘钱包与 API 凭据已就绪。

---

## 核心功能

### 1. 机会雷达与行情过滤
- **公开行情接入**：对接 Polymarket Gamma 只读行情接口，兼容标准 JSON 数组与 JSON 字符串编码数组字段。
- **中文市场文案**：机会雷达提供「中文 / English」市场语言选择，默认中文。中文优先读取 Polymarket Gamma 的 `locale=zh` 翻译；部分体育盘口 Polymarket 只翻译赛事标题而不翻译具体问题，此时会自动组合中文赛事标题、盘口类型与常见结果（如「总分 / 让分 / 准确比分」「是 / 否」「大 / 小」），避免整列回退为英文。切换语言不会改变工作台界面语言。
- **多维条件筛选**：针对二元市场（Binary Markets），按参考价格区间、剩余到期天数、最低流动性自动过滤尾盘机会。
- **真实收益报价**：雷达每行可点击「获取真实收益」，后端读取该 token 的 CLOB 实时卖盘与市场 `FeeSchedule`，按参数设置中的单笔金额逐档计算成交均价、taker 手续费、净成本与净收益率；同时标记盘口深度不足或成交限价超过 `max_price`（实盘会拒单）。报价只读、不会下单，未配置实盘私钥也可使用；模拟账户仍按自身滑点与固定费率记账。
- **行情异常保护**：当上游行情不可用或结算数据异常时，系统会保持现有持仓状态不变并明确报错，绝不使用占位数据或猜测结果替代真实行情。

### 2. 实盘执行与防丢单机制
- **CLOB 原生交易**：支持 Polymarket CLOB V2 EIP-712 订单签名、FAK（Fill-and-Kill，立即成交剩余取消）市价买入、真实成交回报解析与 CLOB 订单号审计。
- **预写日志与自动对账**：实盘订单在签名后、提交前会先持久化为 `pending` 状态；若因网络抖动、超时或异步撮合导致结果未知（`unknown`），后续扫描会自动向 CLOB 查询对账。**在未决订单完成对账前，系统会暂停该账户的自动下单**，防止真实成交丢失或重复建仓。
- **真实费率核算**：自动读取目标市场的 CLOB `FeeSchedule` 计算 Taker 手续费，并在下单前预留相应现金与敞口额度，成交后计入持仓成本。

### 3. 模拟演练与全周期记账
- **高仿真本地撮合**：支持自定义模拟滑点、固定费率、单笔下单金额，以及单市场/总敞口与可用现金约束。
- **持仓全生命周期**：完整支持自动/手动买入、实时盯市（Mark-to-Market）、到期 `0/1` 结算以及 `50/50` 平局退款（`split`）。
- **多维绩效指标**：实时核算已实现盈亏、浮动盈亏、胜率（自动剔除平局退款样本）、策略可用资金、钱包真实 USDC 余额、当前持仓投入与历史净值曲线。

### 4. 可视化工作台与工程底座
- **六大核心视图**：账户总览、机会雷达、我的持仓、订单记录、参数设置、系统设置，支持深浅色主题切换与响应式布局。
- **扫描与下单解耦**：手动点击「刷新行情/扫描」默认只更新行情、持仓盯市与净值快照，不会触发自动买单；后台扫描引擎则按各账户独立的自动交易开关执行策略。
- **开箱即用与安全默认**：内置 SQLite 事务级自动迁移、优雅停机、请求体大小限制、安全响应头，默认仅监听本地回环地址（`127.0.0.1`），并通过 Go 竞态检测（`-race`）与前后端构建测试。

### 5. 系统设置（接口、实盘凭据、网络代理与访问密码）
- **行情与交易接口**：在「系统设置」页直接修改 Gamma 行情地址与 CLOB 交易地址，支持自建镜像或本地代理。保存后新的行情与交易请求立即使用新地址，无需重启；留空则继承环境变量或官方默认值。
- **网页配置实盘凭据**：钱包私钥、签名类型、Funder 以及可选的 CLOB L2 API Key / Secret / Passphrase 都可以在网页中填写。网页值优先于启动环境变量；私钥与 L2 Secret 使用 AES-256-GCM 加密保存在 SQLite，读取时只返回 `******` 掩码，前端原样回传时后端自动还原。
- **网络出口代理**：在「系统设置」页统一配置后端访问 Polymarket 的出口，支持「跟随环境变量（`HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY`）」「自定义代理（http / https / socks5，支持 `NO_PROXY` 白名单）」「直连（忽略环境变量）」三种模式。修改后立即应用到 Gamma 行情与 CLOB 实盘请求，无需重启；配置保存在本机 SQLite 的 `system_settings` 表。
- **代理连通性测试**：保存前可用「测试连接」按钮通过当前表单配置探测 `https://clob.polymarket.com/time`，返回状态码与耗时；测试不会保存设置。
- **代理凭据保护**：代理地址中的密码在读取时以 `******` 掩码返回，前端原样回传时后端会自动还原，避免误清空密钥。
- **网页访问密码**：可选启用（默认关闭，保持本地无认证的默认行为）。启用后所有 API 与工作台都需要登录，密码以 bcrypt 哈希保存在 `access_password` 表，登录会话为 `HttpOnly` + `SameSite=Strict` Cookie，有效期 7 天；连续输错 5 次会被临时限流。
- **密码修改与关闭**：系统设置页支持校验当前密码后修改或关闭保护；修改密码会自动使其他浏览器会话失效。

---

## 策略与核算口径

设市场公开参考价为 `p`，单笔订单名义买入金额为 `A`。

### 模拟账户（`mode=paper`）
设配置滑点为 `s`（`slippage_bps / 10000`），手续费率为 `fee_bps`：

```text
模拟成交价  p' = p × (1 + s)
模拟份额    q  = A / p'
模拟手续费  f  = A × fee_bps / 10000
模拟总成本  c  = A + f
```

- 若计入滑点后的模拟成交价 `p'` 超过策略设定的最高买入价 `max_price`，该笔模拟订单会被直接拒绝。
- Polymarket 的实盘 taker 费为 `fee = 份额 × 类别费率 × p × (1 − p)`，折算成名义金额占比即 `类别费率 × (1 − p)`。因此理论等效费率为：

  ```text
  fee_bps ≈ 类别费率 × (1 − p) × 10000
  ```

  | 成交价 | 类别费率 0.04 | 0.05 | 0.07 |
  |---|---:|---:|---:|
  | 0.95 | 20 bps | 25 bps | 35 bps |
  | 0.97 | 12 bps | 15 bps | 21 bps |
  | 0.98 | 8 bps | 10 bps | 14 bps |
  | 0.99 | 4 bps | 5 bps | 7 bps |

- 默认 `fee_bps = 15`，对应 `p≈0.97`、类别费率 `0.05`，在本策略的 0.95–0.99 区间内整体偏保守。旧版本写入的默认 `0` 会在首次加载时一次性升级为 15；升级后如需 0 费（例如只做地缘政治这类免手续费市场）可以再手动改回。
- 模拟账户仍采用固定 `fee_bps` 近似，不复刻 CLOB 的非线性费率曲线；要更贴近某个具体价位，按上表换算后填入即可。
- `initial_bankroll`（模拟初始本金）仅在**新建模拟账户**时写入该账户的基准权益；后续修改全局默认值不会重算已有账户的历史现金或累计收益率。

### 实盘账户（`mode=live`）

```text
订单类型    FAK（立即成交，剩余未成交部分立即取消）
买入金额    A USD
成交价格    由 Polymarket CLOB 当前真实订单簿决定
成交份额    以 CLOB 返回的 takingAmount 为准
名义成本    以 CLOB 返回的 makingAmount 为准
实盘手续费  fee = FeeSchedule.Rate × shares × (p × (1 - p))^Exponent
实际总成本  名义成本 + 实盘手续费
```

- 实盘提交前会检查盘口价格是否超过 `max_price`，并按预估手续费提前校验和预留可用资金与风险敞口。
- 手续费在成交时按 USDC 从钱包额外扣除，**不会减少成交份额**；系统把费用累加进持仓 `Cost`，并同时记录在 `positions.fee` 与 `orders.fee`，因此已实现/未实现盈亏与「累计盈亏」卡片都已含费。
- 预估或计算失败时**不会按 0 费记账**：费率超出 0–0.10、费用超过名义金额 10%、或读取 `FeeSchedule` 失败，都会拒绝下单/将对账中的订单保持待确认状态。
- 费率口径（2026-10-30 核对）：[Fees](https://docs.polymarket.com/trading/fees) 列为 taker-only、按类别费率、5 位小数向下取整；[Resolution](https://docs.polymarket.com/concepts/resolution) 与 [Manage Positions](https://docs.polymarket.com/trading/positions/manage) 明确获胜份额按 1:1 兑付、赎回不收取费用。

### 真实收益报价（雷达）

雷达上的「毛收益」只按参考价计算、未计手续费。点击「获取真实收益」后，后端会用 CLOB 实时卖盘和真实费率给出一份只读估算（不签名、不下单）：

```text
限价      = ceil(触达的最差卖价 / tick) × tick   （与实盘 FAK 订单一致）
份额      = floor(单笔金额 / 限价 × 1e4) / 1e4
名义成本  = Σ(每档成交份额 × 该档卖价)          （逐档成交，不是全按最差价）
手续费    = Σ(每档份额 × Rate × (该档价格 × (1 − 该档价格))^Exponent)
净收益率  = (份额 − 名义成本 − 手续费) / (名义成本 + 手续费)
```

- 报价按参数设置中的「单笔金额」计算；在买入弹窗中也可按当前输入金额重新获取。
- `限价 > max_price` 时会在雷达上标红：即使成交均价更低，实盘订单仍会被风控拒绝。
- 卖盘深度不足以覆盖单笔金额时会标记「深度不足」，并按可用深度给出参考值。
- 报价是快照，盘口与费率会变化；界面显示报价时间，可随时点击刷新。

### 默认策略参数

| 参数 | 默认值 | 说明 |
|---|---:|---|
| 参考价区间 | `0.95 – 0.99` | 仅筛选价格落在该区间内的二元结果 |
| 最长到期时间 | `30 天` | 仅筛选距离结算不超过该天数的市场 |
| 最低流动性 | `10,000 USD` | 过滤盘口深度过浅的市场 |
| 单笔金额 | `100 USD` | 每次买入的名义下单金额 |
| 单市场 / 总敞口上限 | `250 / 1,000 USD` | 单一市场最大投入与账户总持仓投入上限 |
| 模拟初始资金 | `10,000 USD` | 仅对新创建的模拟账户生效 |
| 模拟滑点 (`slippage_bps`) | `20 BPS` | 模拟买入时的价格上浮比例（0.20%） |
| 模拟费用 (`fee_bps`) | `15 BPS` | 按名义金额计，≈ `类别费率 × (1 − p)`；见上表换算 |
| 实盘手续费 | 自动计算 | 按市场 `FeeSchedule` 动态读取并核算 |
| 扫描间隔 | `60 秒` | 后台引擎自动轮询行情的周期 |
| 默认账户 / 自动交易 | `实盘 / 关闭` | 自动交易默认关闭，需按账户单独开启 |

---

## 快速开始

**环境要求**：Go `1.26.6+`、Node.js `20.19+`（或 `22.12+`）、npm。

### 方式一：一键构建并单端口运行（推荐体验）

从仓库根目录执行：

```bash
make run
```

该命令会自动安装前端依赖、编译前后端生产产物，并由 Go 服务在单端口统一托管页面与 API。启动后直接访问 <http://127.0.0.1:8080>。

如需自定义监听端口或数据库路径：

```bash
ADDR=127.0.0.1:9000 DATABASE_PATH=./data/research.db make run
```

等价的手动构建步骤：

```bash
cd frontend && npm run build
cd ../backend
WEB_DIR=../frontend/dist go run ./cmd/server
```

### 方式二：前后端分离启动（适合开发调试）

**1. 启动后端**

```bash
cd backend
go run ./cmd/server
```

默认监听 `127.0.0.1:8080`，并从 `https://gamma-api.polymarket.com` 同步公开行情（首次扫描通常需要几秒钟）。也可以在仓库根目录执行 `make dev-backend`。

> **提示**：未配置 `POLYMARKET_PRIVATE_KEY` 时，默认实盘账户会显示「实盘未配置」并拒绝任何实盘买单。你可以直接在页面右上角切换到「模拟账户」体验完整功能。

**2. 启动前端**

另开一个终端窗口：

```bash
cd frontend
npm install
npm run dev
```

打开浏览器访问 <http://localhost:5173>（也可在根目录执行 `make dev-frontend`）。开发模式下 Vite 会自动将 `/api` 请求代理至 `127.0.0.1:8080`；如需连接其他后端地址，可配置环境变量 `VITE_API_BASE_URL`（默认值为 `/api/v1`）。

### 配置实盘交易（可选）

推荐直接在网页「系统设置 → 实盘交易凭据」中填写，保存后立即重建 CLOB 客户端，无需重启。也可以继续使用服务端环境变量；网页中留空的字段会继承环境变量：

```bash
export POLYMARKET_PRIVATE_KEY=0x...          # 可选：钱包签名私钥
export POLYMARKET_SIGNATURE_TYPE=0           # 0=EOA，1=Proxy，2=Safe，3=Deposit/Poly1271
export POLYMARKET_FUNDER=0x...               # 非 EOA 模式必填；EOA 模式必须留空
# export POLYMARKET_API_KEY=...              # 可选：复用已有的 CLOB L2 凭据（不是 Relayer API Key）
# export POLYMARKET_API_SECRET=...
# export POLYMARKET_API_PASSPHRASE=...
```

- **不要用 Relayer API Key 当 CLOB 凭据**：在 polymarket.com「Settings → API Keys」创建的是一对 **Relayer API Key**（一串 UUID）与**地址**，它只用于经 relayer 免 gas 提交链上交易（赎回等），不能用于 CLOB 下单。CLOB 的 L2 凭据是 `apiKey` / `secret` / `passphrase` 三元组，Polymarket 网页不会展示它，只能由钱包私钥通过 L1 签名向 `POST /auth/api-key` 或 `GET /auth/derive-api-key` 创建/派生。因此本系统推荐把网页中的三项 L2 字段**全部留空**，由私钥自动派生；只有你在外部脚本里另行创建并希望复用时才需要填写。Relayer 密钥里的地址是签名 EOA 地址，可用它核对「检测凭据」结果中的 Signer 是否一致（Funder 则是 Polymarket 账户的 Proxy / Deposit 钱包地址，两者不同）。
- **Relayer 不是一种登录方式（官方接口已确认）**：[`POST /submit`](https://docs.polymarket.com/api-reference/relayer/submit-a-transaction) 的请求体必填 `from`、`proxyWallet`、`nonce`、`signature`、`signatureParams`、`type`；`RELAYER_API_KEY` 与 `RELAYER_API_KEY_ADDRESS` 两个请求头只负责鉴权，其中 `RELAYER_API_KEY_ADDRESS` 必须等于密钥所有者（即签名者 EOA）地址。也就是说 relayer 只代付 gas 并转发**已签名**的钱包批次，任何模式下都离不开私钥，本系统不存在「仅用账号登录即可交易」的路径。
- **Relayer 密钥验证**：代理钱包模式下可在网页填写 Relayer API Key（UUID）与签名者地址，用 `POST /system/relayer/test` 调用 `GET /relayer/api/keys` 校验：先本地比对签名者地址与私钥地址，再确认该地址的密钥列表包含这把 Key。检测不保存密钥、不提交任何交易；密钥目前仅用于将来的免 gas 赎回（本系统暂不代赎回）。

#### 两种钱包模式

网页「钱包模式」选择器对应两套字段要求，后端用 `signature_type` 区分（`0` = EOA，`1/2/3` = 代理钱包）：

| 模式 | 适用账户 | 私钥作用 | Funder | 赎回 gas |
| --- | --- | --- | --- | --- |
| **自托管 EOA**（`0`） | 自己持有的钱包，资金就在该地址 | 签名 + 持有资金（Signer = Maker） | **必须留空** | 自付 POL |
| **Polymarket 代理钱包**（`1`/`2`/`3`） | polymarket.com 注册的 Proxy / Safe / Deposit 钱包 | 仅签名（控制该钱包的 EOA） | **必填**，且不能等于私钥地址 | 可经 relayer 免 gas（本系统暂不代赎回） |

- `1` = 旧版 Magic/Google Proxy，`2` = 旧版外部签名钱包的 Safe，`3` = Deposit Wallet（2026-05 之后新注册账号的默认类型）。
- **邮箱 / Magic 登录的账号无法导出私钥**（由 Polymarket 托管），因此不能直接用于本系统实盘；需要另用一个能导出私钥的钱包，或把资金转到由自己私钥控制的地址。
- 保存前会做本地校验：EOA 模式带 Funder、代理模式缺 Funder、Funder 等于私钥地址都会直接拒绝，避免订单签给一个没有余额的地址。

#### 邮箱登录账号如何实盘

邮箱 / Google 登录的 Polymarket 账号由 Magic 托管私钥（MPC），网页上的下单是 Magic 在浏览器内完成签名，**私钥无法导出**。本系统需要本地签名，所以无法直接用这类账号下单。可行路径：

1. **迁移到可导出私钥的钱包（推荐）**
   - 新建或导入一个 MetaMask / Rabby 等钱包（记下私钥）。
   - 在 polymarket.com（邮箱账号）里把 USDC 提现到这个新地址，提现由 Magic 在浏览器签名，无需导出私钥。
   - 用新钱包登录一次 polymarket.com，让平台为它部署 Deposit 钱包（类型 `3`）。
   - 在本系统选「Polymarket 代理钱包」，填新钱包私钥，点 Funder 旁的「自动获取」（或从个人菜单复制 Deposit 钱包地址），类型选 `3`。
   - 不想用 Polymarket 智能钱包也可以选「自托管 EOA」：USDC 直接在新地址上，但需自行完成 USDC 授权（自付 Polygon gas）。
2. **Session Key（官方 beta，仅 Deposit 钱包）**：Deposit Wallet 所有者可以授权一个独立 EOA 签名者代为交易（可按 venue 限定、180 天过期、不能提现），参见 [Session Keys](https://docs.polymarket.com/trading/session-keys)。但授权本身仍需 owner 私钥签名 + Builder API Key + relayer，且订单需要 session-key 签名包装，本系统当前 SDK 未实现，暂不支持。
3. **不要尝试“导出” Magic 私钥**：没有公开接口，也违反平台条款。

> 迁移后是新的钱包 / 新账户，旧账号的持仓与余额需在网页里自行赎回、提现；本系统不代赎回。

- **网页凭据的存储方式**：私钥、L2 Secret 与 Passphrase 以 AES-256-GCM 加密写入 SQLite；密钥文件默认位于数据库旁（`<DATABASE_PATH>.key`，权限 `0600`）。请将数据库与密钥文件一同备份；丢失密钥将无法解密已保存的凭据。网页与 API 永远不会返回明文私钥。
- 若未手动提供 L2 API 凭据，系统会在首次执行实盘操作时自动通过私钥派生。
- **保存前先验证**：网页「系统设置 → 实盘交易凭据」提供「检测凭据」按钮（`POST /system/live/test`），它会用当前表单（未保存也可）派生 L2 凭据并读取抵押余额与授权额度，返回 Signer / Maker / 掩码后的 API Key。该检测只读、不落库、不下单，可用于确认私钥与 Funder、签名类型是否匹配。
- **开发者码（Builder Code）已内置**：`backend/internal/config/config.go` 中硬编码了 `BuilderCode`（及仅供识别的 `BuilderAddress`），实盘订单在签名前会写入该 `builder` 字段，用于把成交归因到对应 builder 档案并参与分佣。该值为公开标识，不是密钥，也不需要环境变量配置。
- 请使用专用的小额热钱包，**切勿将私钥提交至 Git 仓库**；网页保存的私钥也应按同等敏感级别保护数据库与密钥文件。
- **实盘资金口径说明**：实盘账户的「策略可用资金」计算公式为 `首次同步的钱包 USDC 基准 + 已实现盈亏 − 当前未平仓成本`，同时界面会并列展示链上真实的「钱包 USDC 余额」。该核算方式假设该钱包专用于本策略；若同一钱包内还有你在 Polymarket 手动交易的其他仓位，策略账面可用资金会与钱包真实余额产生偏差。
- **Signer 与 Maker 说明**：`Signer` 是私钥对应的地址，`Maker` 是持仓与资金所在地址。自托管 EOA 模式下两者相同；代理钱包模式下 `Maker` 等于 `POLYMARKET_FUNDER`。参数设置页与「检测凭据」结果会分别展示两者。

---

## 环境变量配置

`ADDR`、`DATABASE_PATH`、`WEB_DIR` 属于启动参数；其余运行时选项都可以在页面「系统设置」中维护并保存在本机 SQLite。下表中的 `GAMMA_BASE_URL`、`POLYMARKET_*` 与代理变量是**可选的环境变量回退值**：网页中保存了非空值时优先使用网页值，留空时继承环境变量或内置默认值（配置示例参见 [.env.example](.env.example)）。

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `ADDR` | `127.0.0.1:8080` | HTTP 服务监听地址（启动参数，修改需重启） |
| `DATABASE_PATH` | `./data/pennyharvester.db` | SQLite 数据库文件路径（启动参数，修改需重启） |
| `GAMMA_BASE_URL` | `https://gamma-api.polymarket.com` | 公共行情地址的默认值；可在网页「系统设置」中覆盖 |
| `WEB_DIR` | 空 | 前端静态产物目录（启动参数；配置后由后端同端口托管页面） |
| `POLYMARKET_PRIVATE_KEY` | 空 | 实盘签名私钥的可选回退值；也可在网页中加密保存 |
| `POLYMARKET_SIGNATURE_TYPE` | `0` | 签名类型的可选回退值：`0` EOA、`1` Proxy、`2` Safe、`3` Deposit/Poly1271 |
| `POLYMARKET_FUNDER` | 空 | 非 EOA 签名类型对应的代理/Safe 钱包地址的可选回退值；EOA 模式必须留空（兼容旧变量名 `POLYMARKET_MAKER_ADDRESS`） |
| `POLYMARKET_CLOB_URL` | `https://clob.polymarket.com` | CLOB 交易地址的默认值；可在网页「系统设置」中覆盖 |
| `POLYMARKET_API_KEY` | 空 | 可选，复用已有的 CLOB L2 API Key（**不是** polymarket.com 设置页的 Relayer API Key）；也可在网页中填写 |
| `POLYMARKET_API_SECRET` | 空 | 可选，复用已有的 CLOB L2 API Secret；也可在网页中加密保存 |
| `POLYMARKET_API_PASSPHRASE` | 空 | 可选，复用已有的 CLOB L2 API Passphrase；也可在网页中加密保存 |
| `HTTP_PROXY` / `HTTPS_PROXY` | 空 | 仅当代理模式为「跟随环境变量」（默认）时生效；由 Go 标准库在首次请求时读取并缓存 |
| `NO_PROXY` | 空 | 仅当代理模式为「跟随环境变量」时生效，逗号分隔的主机/域名列表 |

> **安全提醒**：本项目不会自动加载 `.env` 文件，请通过 Shell 或进程管理器注入环境变量。后端默认不设身份认证，**请勿直接暴露在公网环境**；如需远程访问，请先在「系统设置」中启用访问密码，并自行配置 TLS 反向代理与网络访问控制。私钥等同于钱包资产控制权，请务必使用专用的小额热钱包并严格限制宿主机权限。网页保存的实盘私钥与 L2 Secret 会加密写入 SQLite，但数据库旁的 `<DATABASE_PATH>.key` 密钥文件仍是敏感文件，请与数据库一同保护。

> **代理说明**：出口代理的默认模式为「跟随环境变量」，与旧版本行为一致；进程启动时读取一次 `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY`（Go 标准库会缓存该环境）。若需要在不重启的情况下切换出口，请在「系统设置」中选择「自定义代理」或「直连」。

---

## HTTP API 概览

所有接口统一使用 `/api/v1` 前缀。除获取账户列表外，与账户相关的接口均支持通过 `?account_id=` 指定目标账户（省略时默认操作当前默认账户）。

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/health` | 服务健康检查、默认账户模式、实盘配置与就绪状态 |
| `GET` | `/accounts` | 获取所有账户列表（含实盘就绪状态、`signer` 与 `maker` 地址） |
| `POST` | `/accounts` | 创建新账户，请求体：`{"name":"...","mode":"paper"}` |
| `PATCH` | `/accounts/:id` | 修改账户名称、自动交易开关或设为默认账户 |
| `DELETE` | `/accounts/:id` | 删除指定模拟账户（级联清除其持仓、订单与净值记录） |
| `GET` | `/dashboard?account_id=1` | 获取账户核心指标、引擎状态、数据源状态与净值曲线快照 |
| `GET` | `/opportunities` | 获取最近一次成功扫描筛选出的市场机会列表；每项同时返回原始 `question` / `outcome`、Polymarket 中文翻译 `question_zh` / `outcome_zh`，以及中文赛事标题 `event_title_zh` 和盘口短标题 `group_item_title`，用于体育盘口的中文兜底展示 |
| `POST` | `/scan` | 手动触发扫描：默认仅刷新行情、持仓盯市与净值，不自动下单；附加 `?execute=true` 时同步执行自动交易 |
| `GET` | `/positions?account_id=1&status=open` | 查询持仓列表，`status` 支持 `all / open / won / lost / split` |
| `GET` | `/orders?account_id=1&status=all&limit=100&offset=0` | 查询订单记录，`status` 支持 `all / filled / settled / rejected / pending / unknown / unresolved` |
| `GET / PUT` | `/settings` | 读取或更新全局策略筛选条件与风控参数 |
| `GET / PUT` | `/system` | 读取或更新系统设置（Gamma / CLOB 地址、实盘凭据、网络出口代理）；私钥、L2 Secret、Passphrase 与代理密码在响应中自动掩码。请求体支持按分区部分更新：`proxy`、`data_source`、`live` 可单独提交；提交 `live` 时请带上该分区的完整字段，掩码值会自动还原为已保存的密钥 |
| `POST` | `/system/proxy/test` | 使用请求体中的代理配置探测 `https://clob.polymarket.com/time`，不保存设置 |
| `POST` | `/system/live/test` | 用请求体中的实盘凭据（`live` 与 `clob_base_url`，掩码值自动还原）做只读验证：派生 L2 凭据并读取余额/授权，返回 Signer、Maker、掩码 API Key 与耗时；不保存设置、不下单 |
| `POST` | `/system/relayer/test` | 验证 Relayer API Key：`relayer_api_key` + `relayer_address`（需与私钥地址一致），调用 `GET /relayer/api/keys` 确认密钥有效；不保存、不提交交易 |
| `POST` | `/system/live/funder` | 用私钥做 L1 鉴权，向 CLOB 查询该签名地址关联的 Proxy / Deposit 钱包，返回 `wallets`；不保存设置 |
| `GET` | `/auth/status` | 查询是否需要登录与当前会话是否有效（无需登录即可访问） |
| `POST` | `/auth/login` | 使用访问密码登录，成功后在 `ph_session` Cookie 中下发会话 |
| `POST` | `/auth/logout` | 退出登录并吊销当前会话 |
| `POST` | `/auth/password` | 首次设置或修改访问密码（需提供当前密码），成功后重新下发会话 |
| `POST` | `/auth/password/disable` | 校验当前密码后关闭访问密码保护 |
| `POST` | `/engine/toggle` | 启停后台定时扫描引擎，请求体：`{"running":false}` |
| `POST` | `/accounts/:id/orders` | 在指定账户下提交一笔实盘或模拟买入订单 |

**手动下单请求示例**：

```json
{
  "market_id": "123",
  "outcome": "Yes",
  "token_id": "public-reference-token-id",
  "amount": 100
}
```

**错误响应规范**：所有错误统一返回 `{"error":"..."}`。其中上游行情异常返回 `502`，参数校验或风控拒绝返回 `400`，实盘未配置或未就绪返回 `503`，账户状态或模式冲突返回 `409`。

---

## 数据库迁移与测试验证

### 自动数据库迁移
服务启动时会自动检测并升级旧版 SQLite 数据库结构：包括为 `positions` 表补充 `kind` 字段与 `split` 状态、为 `orders` 表补充 `pending / unknown` 状态、为存量模拟账户补写基准权益，以及创建 `system_settings`（网络代理配置）与 `access_password`（访问密码哈希）两张新表。所有迁移均在数据库事务内执行，若失败会自动回滚；建议在版本升级前先备份 `pennyharvester.db`。

### 运行测试与构建检查

在仓库根目录可直接运行：

```bash
make test
make build
```

或分步执行后端竞态测试、静态检查与前端生产构建：

```bash
cd backend
go test -race ./...
go vet ./...

cd ../frontend
npm run build
```

---

## 边界说明与风险揭示

在使用本系统进行策略研究或实盘交易前，请充分了解以下系统边界与市场风险：

- **真实资金风险**：实盘模式会直接动用钱包内的真实 USDC。自动交易默认关闭，开启前请务必先通过模拟账户和最小实盘金额验证完整链路。
- **0.99 并非无风险套利**：高价格二元结果的潜在收益空间极其有限（1%–5%），但一旦尾部风险发生、结果反转归零，将面临接近全部本金的损失。
- **公开参考价 ≠ 可成交盘口价**：Gamma 接口返回的 `outcomePrices` 仅代表公开参考价，不等于订单簿上的实际卖一价（Best Ask）。买卖点差、盘口深度不足与手续费都可能显著影响最终收益。
- **实盘成交不确定性**：实盘采用 FAK 订单，实际成交价格由 CLOB 实时订单簿决定，可能与页面显示的参考价存在偏差，也可能出现部分成交或因深度不足而未成交。
- **USDC 授权（Allowance）要求**：实盘下单前需先在 Polymarket 完成 USDC 合约授权。系统每次下单前会检查链上 Allowance，并直接拒绝授权额度不足的订单。
- **手续费记账口径**：实盘 taker 费在成交时按市场 `FeeSchedule` 计算并计入成本，maker 为 0；费率异常会拒单而不是按 0 费记账。模拟账户使用固定 `fee_bps` 近似（默认 15 BPS）。页面「累计盈亏」卡片单独展示已付手续费合计。Polymarket 的费率由平台设定并可能调整，最终资金变动仍以链上钱包实际扣款为准。
- **赎回不收费，但 gas 视钱包类型而定**：结算时获胜份额按 1:1 兑付，Polymarket 不收取赎回费；通过 Deposit/Proxy 钱包经 relayer 赎回不消耗 gas，而自托管 EOA（`POLYMARKET_SIGNATURE_TYPE=0`）赎回需自付 Polygon gas，本系统不记账该项成本。
- **未决订单熔断保护**：实盘订单若遇网络中断、超时或 CLOB 异步撮合，会进入 `unknown`（待核对）状态。系统已持久化该订单并会在后续扫描中自动对账，但在核对完成前不会继续为该账户自动下单。
- **出口代理与行情可用性**：代理链路不稳定会直接影响行情扫描与实盘下单；若出现 `502` 与「公开行情不可用」，请先在「系统设置」中测试代理连通性。代理只改变网络出口，不改变策略与风控判定。
- **本地结算记账 vs 链上领奖**：本系统只发送买入（BUY）指令，不提供卖出/平仓，也不会自动赎回头寸。实盘持仓到期后，系统会根据公开的 `0/1` 或 `50/50` 结果在本地更新结算状态与盈亏，但**不包含链上奖金赎回（Redeem）与跨系统资金对账**——获胜头寸的奖金仍需前往 Polymarket 官方界面手动领取。赎回本身不收取协议费，但自托管 EOA 需自付 Polygon gas（本系统不记账）。
- **平局退款（`split`）处理**：若市场按 `50/50` 分摊结算，持仓会标记为 `split`（平局退款），该笔交易不计入胜率的分子或分母。
- **模拟表现不代表实盘收益**：模拟账户仅基于实时行情做简化规则记账，既非历史回测，也无法复现真实订单簿的排队、流动性枯竭与价格冲击，不证明策略具有正数学期望。
- **特殊市场事件**：预测市场可能出现延期、规则争议、取消或特殊结算。当公开结算数据存在歧义或异常时，系统会保持持仓为未结算状态并提示错误，而不会强行猜测结算结果。
- **单机单实例约束**：单次扫描最多拉取 500 个开放市场；系统基于本地 SQLite 运行，未针对多实例并发部署设计。**切勿在多个实例上同时配置同一个实盘钱包**，否则会导致风控失效与重复下单。
- **合规性要求**：使用 Polymarket 行情数据或参与交易时，请自行确认并遵守所在司法管辖区的法律法规及平台服务条款。

---

## License

本项目采用 PolyForm Noncommercial 1.0.0 许可协议，详见 [LICENSE](LICENSE)。
