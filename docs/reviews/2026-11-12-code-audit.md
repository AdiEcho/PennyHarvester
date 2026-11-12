# PennyHarvester 深度代码审查

- 审查基线：`abc13a3142247dab427af74ea45f27738c87a117`，`master`。
- 审查日期：2026-11-12。
- 目标：检查交易与账本逻辑、性能边界、数据真实性，并形成可执行的开发与重构计划。
- 变更范围：本次只新增审查与开发文档，不修改业务代码、依赖、运行配置或账户数据。
- 本文中的行号均对应上述提交；后续修复请以符号名和测试场景定位。

## 1. 方法与证据边界

本次采用模块级静态审查、现有测试与构建验证、隔离环境定向探针，以及官方资料交叉核对。代码范围包含启动配置、行情适配器、执行器、扫描与风控引擎、SQLite、HTTP API、前端与 CI。

没有访问生产数据库、真实私钥或账户流水，没有提交实盘订单、链上授权或赎回。不能据此证明某个账户历史盈亏正确、资金已经到账，或策略具备正期望。性能问题按明确代码路径分析；未经负载测量的吞吐量、延迟改善不写成实测结果。

证据等级：

- A：本地隔离探针或实际测试直接观察到的行为。
- B：可由源码和调用链直接确认的缺陷或边界。
- C：需要上游版本、真实响应或链上证据补充验证的条件性风险。
- 建议：尚未成为已观察故障的工程改进，不冒充缺陷。

优先级：

- P0：已证实、可直接造成严重资金或权限失控的阻断问题。
- P1：交易、账本、结果真实性或恢复能力存在实质风险，应在扩大实盘前修复。
- P2：受特定条件触发的正确性、性能、展示或维护性问题。
- P3：防御性强化、文档一致性及长期工程改进。

### 1.1 结论与优先顺序

现有项目具备场所/账户隔离、离线测试、发送前订单身份持久化、事务化落账等基础；主要缺口不在缺少更多策略指标，而在跨模块的事实语义：参考价与兑付、请求金额与实际成交、匹配与确认、gross与net、草稿与生效配置被混用。已执行的基线测试均通过，另有2个联网smoke明确跳过；现有测试仍未覆盖这些组合。

本次共列58个编号项：18个P1、31个P2、9个P3，未确认P0。编号是修复单元，不是独立事故数；若干项存在上下游依赖。未确认P0不等于可保证资金安全，也不把条件性源码风险说成生产事故。

| 模块 | P1 | P2 | P3 | 详情 |
| --- | ---: | ---: | ---: | --- |
| 行情/最终性 DAT | 2 | 2 | 0 | 第2节 |
| 引擎/账本 LED | 4 | 6 | 0 | 第6节 |
| Predict.fun PF | 6 | 4 | 0 | 第8节 |
| Web/API WEB | 3 | 12 | 5 | 第9节 |
| Polymarket PM | 3 | 7 | 0 | 第10节 |
| 通用接口 VEN | 0 | 0 | 3 | 第10节 |
| 文档 DOC | 0 | 0 | 1 | 第5节 |

优先处理四组完整边界，而非按文件大小重写：

1. 风险与账本：unknown不释放新增风险、未决退出不被参考结算越过；PM卖费只扣一次；PF净份额与现金遵循真实费用资产。LED-01/02已在隔离执行器中复现越限与收益偏差。
2. 成交事实：PF请求/响应修到官方结构，PM签名整数价格与盘口交叉，分页/失败/处理中成交不能冒充完整最终成交。PF严格fixture已观察错误结构及正数份额Cost=0。
3. 数据最终性：陈旧缓存不续期，closed或报价0.5不代表平局，PF结果不完整不补成兑付。部分数据应可辨，不伪装成健康空列表。
4. 身份与操作：flat配置A→B→清空必须真实应用，部分失败原子回滚，旧请求不污染新钱包；无密码也有跨站副作用防护，授权对象绑定已应用revision。

性能优化放在这些不变量之后：先建立代表性离线基准与预算，再去除无界历史查询/重复RPC、实施路由lazy，并以同条件复测判断收益；不以更快地产生错误事实为优化成果。具体拆分、验收、依赖及合并波次见 [开发与重构计划](./2026-11-12-development-plan.md)。

### 1.2 数据真实性逐层判定

| 对象 | 当前可成立的描述 | 不可据此推导的事实 | 主要缺口 |
| --- | --- | --- | --- |
| 扫描结果 | 本轮收到/缓存的候选市场 | 全市场完整、新鲜、可按参考价成交 | DAT-01/03、PF-07、LED-03 |
| 参考价/报价 | 参考估值或某时盘口估算 | 最终派彩、最终成交量/均价/净收益 | DAT-02、PF-04/08、WEB-06、PM-02 |
| 订单状态 | 接受、匹配、处理中、已确认需按源区分 | 看到正份额或size_matched就已完整到账 | PF-02、PM-03/05 |
| 手续费 | 金额等值、扣费资产、gross/net应分别保存 | 公式正确就表示份额/现金都正确 | PM-01/04、PF-03、LED-08 |
| 账面现金/收益 | 本程序追踪范围内的策略账本 | 钱包全部资产、可支取金额、策略真实历史回报 | LED-02/05/09；外部充值/其他交易未核对 |
| 余额/授权 | 指定网络/钱包/目标spender的观察值 | 配置切换后旧值仍属于当前身份 | PM-07/08、PF-06/09、WEB-03/11 |
| 赎回 | 当前成功路径等待成功receipt；intent/hash恢复、额外确认与资产变化核对尚不完整，本次未验证真实到账 | HTTP成功/失败直接等于已到账/未发送 | LED-06、PF-05/10、WEB-04/15 |
| UI状态 | 最新已接受查询和已应用配置的展示 | 草稿、旧请求或乐观override代表服务端事实 | WEB-02/05/07/12/13/14 |

因此不能仅凭现有Dashboard判定某真实账户盈亏准确。所需外部逐笔核对尚未执行；本次只提供缺陷证据与修复验证方法，不提供投资收益承诺。

## 2. 行情与结算数据审查

### DAT-01 / P1：陈旧 Gamma 缓存会被重新标成新鲜数据

证据：A。位置：`backend/internal/market/gamma.go:281-291`、`backend/internal/market/gamma.go:321-342`。

广域视图刷新失败后使用 `cachedView(cacheKey(view), 0)`，接受任意年龄的缓存；随后执行 `items, err = stale, nil`，再进入 `storeView`，将缓存时间改为当前时间。上游持续故障时，历史行情不仅无限存活，还会再获得 60 秒的新鲜窗口。

隔离探针预置 24 小时前缓存，并让全部四个视图返回 HTTP 503。实际结果为：返回该旧市场、`err=<nil>`、缓存写入时间距当前仅约 10 微秒、普通 TTL 查询命中。这里的问题不是允许展示旧数据，而是旧数据丢失年龄与失败状态。

下游已核实：`backend/internal/engine/engine.go:1532-1551` 接收这些 nil-error 数据，`backend/internal/engine/engine.go:1874-1875` 对列表中已有的持仓不再独立查询，`backend/internal/engine/engine.go:1629` 更新 lastSyncAt。因此旧缓存会传播到估值、止损触发、模拟成交和实盘候选选择；不能据此声称真实执行器一定按旧参考价成交。

修复方向：将成功获取时间与尝试刷新时间分开；降级返回不得刷新成功时间；引入最大陈旧期限、`stale` 与失败视图明细；陈旧数据只可用于带标记的展示，不可直接成为成交、止损或最终结算依据。

验收：冻结时钟；连续多次刷新失败；确保 `observed_at` 不变化、陈旧原因可见、超龄后停止作为交易依据，上游恢复后才转为 fresh。

### DAT-02 / P1：缺少明确结算状态仍可将 50/50 参考价当作最终平局兑付结果

证据：A。位置：`backend/internal/market/market.go:194-220`。

`finalResolutionData()` 允许 `status == "" || status == "resolved"`；因此 `closed=true`、两侧参考价为 0.5、但没有任何结算状态的市场，也会被 `SettlementPrice()` 接受并返回 0.5。关闭交易、暂时撤盘或价格恰好均衡本身不能替代最终兑付证据。

隔离探针实际观察：`closed=true, resolution_status empty, prices=0.5/0.5 => settle=true, payout=0.5`。

下游已核实：`backend/internal/engine/engine.go:1949-1978` 的 live/paper 账本共用该方法，只有 venue 能力门槛，没有额外 finality 检查。源码的“paper only”注释不能限制真实调用链。这里讨论本地账本结算，不把它等同于链上资金转移。

修复方向：将“停止交易”“平台已公布结果”“链上最终兑付”“已赎回到账”分开建模。缺失 finality 信息应为 unknown，保留未结算；兼容旧镜像必须有显式策略或第二证据来源，不应默认允许。对 0/1 结果也应用同一要求。

验收：关闭但未决的 0.5/0.5、1/0 都不得入已实现盈亏；proposed/disputed/unknown 状态不结算；明确最终平局才记 0.5。

### DAT-03 / P2：行情解析未验证数值与身份，坏行又被静默跳过

证据：A/B。位置：`backend/internal/market/gamma.go:604-668`、`backend/internal/market/gamma.go:377-380`、`backend/internal/market/gamma.go:437-439`、`backend/internal/market/market.go:173-189`。

`parseGammaMarket()` 校验数组长度与日期，但没有集中验证价格范围、token 非空/唯一、市场身份完整性等不变量。`Winner()` 对 `>=0.999` 和 `<=0.001` 的判断也没有先约束 `[0,1]`。

定向探针确认：`[1.2,-0.2]` 可解析成功，且在已关闭/已解决状态下返回胜方兑付 1。另一个探针让所有市场行不合法，`ListOpen()` 返回空列表且错误为 nil，无法区分“确实没有候选”与“上游 schema 已漂移”。分页后续失败被保留为部分结果是合理降级，但缺少 incomplete 元信息也会造成完整性误判。

修复方向：建立适配器输入不变量校验与坏行隔离；记录 `received/accepted/rejected` 数量及理由；保留成功行，但 API 明确给出 degraded/incomplete。不要为失败行补价格、流动性或结算值。数值校验应允许合法极端价格 0/1，不用“价格必须互补到某个极小误差”取代真实业务规则。

验收：负价、超 1 价格、空 ID、重复 token、空/坏日期、全坏行、第二页超时；均不能伪装成完整健康扫描。

### DAT-04 / P2：缺失协议版本时可选取 V2 标识，却失去 V2 识别

证据：A；生产触发依赖上游/镜像省略版本。位置：`backend/internal/market/gamma.go:685-697`、`backend/internal/market/gamma.go:664`、`backend/internal/market/market.go:117-119`。

当 `version` 为空、`clobTokenIds` 缺失而 `positionIds` 有值时，解析器返回后者，但 `Market.Version` 仍为空，`IsV2()` 返回 false。现有显式 v2 路径的保护不能自动覆盖这种兼容输入。

隔离探针实际观察：`token=yes-v2, IsV2=false`。这说明内部标识/协议信息不一致，不代表已观察到交易所接受了错误签名。

修复方向：来源为 `positionIds` 时保留协议来源，或把不能可靠识别的协议设为 unknown 并限制只读。禁止通过“version 不等于 v2”推导“必定可按 v1 签名”。

验收：显式 v1/v2、两字段同时存在、缺版本仅 positionIds、未知版本；只允许具有完整签名能力映射的 instrument 进入实盘。

## 3. 隔离探针记录

### 3.1 行情与终局证据

探针复制了 `backend/go.mod`、`backend/go.sum`、`backend/internal/market/` 与 `backend/internal/netproxy/` 到临时目录，在拷贝中追加临时测试，不改原仓库。

执行：`env -i PATH="$PATH" HOME="$HOME" TMPDIR="${TMPDIR:-/tmp}" go test ./internal/market -run '^TestAuditProbe' -v -count=1`。

| 探针 | 输入 | 观察结果 |
| --- | --- | --- |
| `TestAuditProbeStaleCacheRenewedAsFresh` | 24h 旧缓存 + 所有上游视图 503 | 返回旧行、无错误、TTL 重置 |
| `TestAuditProbeResolutionWithoutEvidence` | closed、缺结算状态、0.5/0.5 | 接受为兑付 0.5 |
| `TestAuditProbeOutOfRangePricesAccepted` | 已解决、1.2/-0.2 | 解析成功、接受为兑付 1 |
| `TestAuditProbeInvalidRowsBecomeSuccessfulEmptyScan` | 所有市场行无效 | 空列表、无错误 |
| `TestAuditProbeMissingVersionUsesV2IDsButNotV2Gate` | 仅 positionIds、缺 version | 获得 V2 标识、IsV2=false |

五个探针均成功复现上述当前行为，最后一次复核运行约 0.462 秒。探针 PASS 仅表示“缺陷前提得到观察”，不表示对应业务行为正确。临时目录完成后清理；表中场景应转为正式回归测试，并反转断言验证修复后的安全行为。

### 3.2 实盘风险准入与未决退出时序

后续在相同 HEAD 的临时 `git archive` 快照中复用现有 fakeProvider/fakeLive，使用内存 SQLite，新增临时测试；环境清空，HOME/缓存独立，sandbox 禁止全部网络及读写原工作区。没有调用真实执行器。

核心命令：`go test -race -count=1 -timeout=90s -run '^TestAuditRepro' -v ./internal/engine`；另设 `GOENV=off GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off GOWORK=off GOFLAGS=-mod=readonly`。结果退出码0，2个测试函数、4个子用例全部命中异常断言，没有跳过或 race 报告。墙钟15.52秒，包内1.950秒。

| 对应问题 | 可控时序 | 直接观察 |
| --- | --- | --- |
| LED-01：unknown 后再次手动买入 | MaxPosition/MaxExposure/MaxEventExposure 均150，连续两次同市场买100；两次 fake 返回 unknown+timeout；之后两单都返回 matched | 第二单确实调用 Buy；补账后两仓成本合计200，突破市场及总限额各50 |
| LED-01：手动事件限额 | MaxPosition150、MaxExposure1000、MaxEventExposure150；同 EventID 的两个市场分别买100，立即 matched | 两单均成功，EventID 正确保留；事件敞口200，突破限额50 |
| LED-02：全额未决卖出跨终局 | 100份成本98；价格跌至0.75触发 unknown 止损；下一轮 Lookup 失败但市场明确 resolved 且1/0；最后 Lookup 返回卖100份、gross75、fee0.10 | 提前参考结算100、收益+2；后到卖单变 rejected、未决归零；应为−23.10，收益高估25.10 |
| LED-02：部分未决卖出跨终局 | 同上，最后 Lookup 返回卖40份、gross30、fee0.10，其余60份胜出 | 仍参考结算100、收益+2；应为−8.10，收益高估10.10 |

LED-02 测试明确验证该市场 `SettlementPrice("one-yes") == (1, true)`，所以不依赖 DAT-02 的缺失终局证据问题。第二轮扫描的 Go error 为 nil，但 ScanSummary.Error 包含 `实盘账户 对账: 订单 pending-stop 对账失败: audit-lookup-unavailable`。第三轮订单原因是 `止损订单对应的持仓已不存在，请人工核对`，真实卖出的金额及手续费没有进入本地账本。

这里“真实卖出回报”指模拟执行器注入的成交事实，不是实际发生过生产交易。探针只动态确认 LED-01 的两个入口缺口与 LED-02 的全额/部分时序；EventID 恢复遗漏、查询失败 fail-open 等其余分支仍是源码证据。临时测试、快照、日志及缓存均已清理，原 HEAD 和业务代码未改变。

### 3.3 系统配置与 Predict.fun 严格契约

使用同一HEAD的临时快照、内存SQLite、API现有fixtureProvider/quoteLive与仅记录配置的runtime hook；PF仅调用Client请求与LookupOrder映射，预置假token、未调用认证/签名/RPC。env-i、独立HOME/缓存、禁下载，sandbox仅允许loopback，禁止读写原工作区及读取常见凭据目录。

核心命令：`go test -race -count=1 -timeout=90s -run '^TestAuditRepro(FlatPMConfig|PredictRequestContract|PredictNestedLookup)$' -v ./internal/api ./internal/predictfun`。3个测试函数、19个子测试全部通过异常断言；0失败/跳过，无race。墙钟18.29秒，API包1.829秒、PF包1.701秒；未重跑全库。

| 对应问题 | fixture与检查 | 观察结果 |
| --- | --- | --- |
| WEB-02 | flat data_source/live一起保存和分section保存；A→不同B→显式清空；核对PUT/GET、SQLite flat/venue、runtime hook | 每次200；首次A生效；B和清空后仍A；hook每次被调用但仍收到A的CLOB地址及fake凭据 |
| PF-01 create | 严格验证官方必填data.order/data.pricePerShare/data.strategy及类型 | 当前发送data:SignedOrder、根层提示字段、数字slippageBps，无strategy；本地fixture拒绝400；符合结构的对照请求201 |
| PF-01 cancel | 要求data.ids字符串数组 | 当前发送orderId，被本地fixture拒绝400；正确对照200；缺ids、空数组、101项、非字符串被拒绝 |
| PF-02 nested GET | 外层id/status/amount/amountFilled，内层order.makerAmount/takerAmount/side，无额外外层价格 | 四例found=true、error=nil、Shares为正，但Price=0、Cost=0、Status=unknown；ack的内层三个字段全空 |

PF-02四例分别为BUY MARKET全成交100、BUY MARKET部分40且FILLED、BUY LIMIT部分25且OPEN、SELL MARKET部分40且FILLED；委托均100份，使用18位缩放字符串。确认解码丢失嵌套字段，不代表修好该解码就能用委托金额恢复真实VWAP。

本地create错误原文：`Predict.fun POST /v1/orders 失败（HTTP 400）：{"message":"data.order required object; data.pricePerShare required string; data.strategy required MARKET|LIMIT","success":false}`。本地cancel错误原文：`Predict.fun POST /v1/orders/remove 失败（HTTP 400）：{"message":"data must be object","success":false}`。这些HTTP码和错误由本地契约fixture生成，不冒充生产响应。

未覆盖完整ContractOrder签名、cancel removed/noop/rejected业务处理、请求量兜底、matches/费用归集或浏览器。临时测试、日志及缓存已清理，原60个快照源码blob不变；原业务代码无差异。

### 3.4 Polymarket 费用、签名精度与成交分页

在目标HEAD的临时 `git archive` 快照中新增5个用例，使用固定SDK与严格loopback GET fixture；`env -i`、独立HOME/模块缓存、禁下载，sandbox拒绝非loopback网络及整个 `/Users` 的读写。没有启动应用服务或数据库，没有调用Buy/Sell提交路径，也没有 `/order` POST。

核心命令：`go test -race -count=1 -timeout=60s -run '^TestAuditP1' -v ./internal/live`。5个用例全部命中异常断言，退出码0，无race，包耗时4.210秒。环境Go1.27.3、darwin/arm64；本轮未重跑全库、vet、前端或其他模块。

| 用例 | 直接观察 | 边界 |
| --- | --- | --- |
| `TestAuditP1SellFeeGrossContract` | 100份×0.75回报映射为Cost=75；实际applySellFee后Fee=1.3125、Cost=73.6875；再按engine消费公式扣费得到72.375，少计1.3125 | 调用真实包内费用函数，再使用源码相同的消费算式；不是完整Engine/DB测试 |
| `TestAuditP1SellReconcileNetDifference` | 同一经济成交的CONFIRMED fixture使实际LookupOrder返回Cost=75、Fee=1.3125；按相同消费公式得到73.6875，与直接路径差一次手续费 | 验证对账映射及公式，不是实盘卖出 |
| `TestAuditP1BuySignedAmountsBelowAsk` | 预算2、ask0.98、tick0.01，实际planMarketBuy→固定SDK CreateOrder产生makerAmount=1990000、takerAmount=2040800；签名能恢复到合成测试地址 | 真实签名对象，不是仅复算计划；只签名不提交，不声称观察到生产拒单码 |
| `TestAuditP1TradesStopsBeforeSecondPage` | 第一页目标60份×0.98、第二页40份×0.985，均CONFIRMED且有next_cursor；LookupOrder未请求第二页，返回60份、Cost=58.8、Price=0.98，覆盖size_matched=100 | 对照读取同游标能取得第二页；完整集合应100份、98.2、均价0.982 |
| `TestAuditP1AllFailedFallsBackToLimitEstimate` | 订单size_matched=100、限价0.99、MATCHED；唯一目标trade为FAILED、100份×0.80；仍返回found=true、err=nil、Shares=100、Cost=99、Price=0.99、Fee=0.0693、Status=MATCHED | 确认全部FAILED仍回退为正数估值；未测试其他生命周期状态或去重 |

签名交叉条件的整数实测为 `1990000 × 100 = 199000000 < 2040800 × 98 = 199998400`，实际比例 `0.9751078008624069 < 0.98`。PM-01的直接路径净额72.375与补账净额73.6875是基于同一100份成交的函数/契约比较，不伪装成完整数据库落账或生产资产变化。

预检曾因过严sandbox规则出现 `Abort trap: 6`（退出134）；隔离脚本曾因ctypes声明及临时HOME中的Go telemetry目录退出1。修正临时profile/检查脚本、清空HOME并禁止写入后，业务测试前后隔离检查均通过。这些预检失败不是业务测试失败，也没有通过放开外网或读取真实凭据绕过。

快照原有63个文件哈希均未变；唯一临时测试、日志、独立缓存及整个临时目录均已删除。原工作区仅保留审查文档，HEAD和业务代码未变。PM-03其他状态生命周期、超时、重复trade ID等仍属于源码分析和待补回归，不升级为本轮动态验证。

## 4. 基线验证

验证在目标 HEAD 的临时快照中完成，复用已安装的前端依赖，不安装或升级依赖。进程使用清空的环境与临时 HOME；macOS sandbox 禁止非回环出站连接、写原工作区和模块缓存，以及读取原生产数据库、备份、`.env`。不启动长期服务。

| 检查 | 命令 | 结果 | 墙钟耗时 |
| --- | --- | --- | --- |
| 后端测试、竞态、语句覆盖率 | `go test -race -cover -count=1 -json ./...` | 退出码 0；254 个顶层测试通过，2 个跳过，0 个失败 | 34.27 秒 |
| 后端静态检查 | `go vet ./...` | 退出码 0，无诊断 | 8.45 秒 |
| 前端类型检查及生产构建 | `npm run build`，即 `tsc -b && vite build` | 退出码 0，有体积警告 | 11.62 秒 |

计入子测试后有 313 条 PASS 事件；没有观察到数据竞争报告。普通包级语句覆盖率如下，不是全项目加权覆盖率，也不是交易场景覆盖率：

| 包（相对 `backend/`） | 语句覆盖率 |
| --- | ---: |
| `cmd/server` | 0.0%，无测试文件 |
| `internal/config` | 0.0%，无测试文件 |
| `internal/api` | 68.9% |
| `internal/engine` | 73.6% |
| `internal/live` | 73.6% |
| `internal/market` | 71.5% |
| `internal/netproxy` | 81.4% |
| `internal/predictfun` | 59.1% |
| `internal/store` | 59.4% |
| `internal/venue` | 75.3% |

联网测试 `TestLiveProviderSmoke`、`TestLiveProviderSettlementSmoke` 明确跳过，原因是 `set PREDICTFUN_LIVE=1 to hit the real Predict.fun API`。实际 guard 为 `PREDICTFUN_LIVE` 是否非空，赋值 `0` 也会启用，因此离线测试必须彻底移除该变量。位置：`backend/internal/predictfun/live_smoke_test.go:18,65`。跳过不等于通过；本次没有验证真实 API、生产迁移或浏览器交互。

环境：macOS 27.0.1 arm64、Go 1.27.3、CGO 开启、Apple clang 17.0.0、Node 26.13.0、npm 11.20.0、TypeScript 5.7.3、Vite 6.4.5、React 19.3.1、Recharts 2.15.4。与 CI 的 Linux / Node 22、`go.mod` 声明的 Go 1.26.6 版本不同，需要继续保留 CI 原矩阵验证。

构建转换 2302 个模块；主要产物：

| 产物 | 构建体积 | gzip |
| --- | ---: | ---: |
| 主入口 JS | 587.20 kB | 173.84 kB |
| charts JS | 430.21 kB | 116.38 kB |
| CSS | 46.78 kB | 8.56 kB |

警告原文：`(!) Some chunks are larger than 500 kB after minification.` 调高警告阈值不是优化；应先测首次加载和主线程执行，再决定路由懒加载及依赖拆分。本次没有测量浏览器运行时性能。

代码规模基线：32 个 Go 生产文件、26 个 Go 测试文件、38 个前端 TS/TSX 文件；未发现前端常规 `.test.*` / `.spec.*` 文件。构建测试覆盖不了账户切换、并发请求、错误分支或交易按钮行为。

## 5. 文档与契约一致性

### DOC-01 / P3：历史设计、当前能力和真实财务语义混杂

证据：B。`README.md`、`.env.example`、`docs/venue-api-contract.md`、`docs/predictfun-integration-plan.md` 中既有后续实现回填，也有未撤销的旧限制。已见冲突包括：

- 每实例一个实盘账户与每场所一个实盘账户并存。
- “只 BUY、无卖出/赎回”与止损卖出、Predict.fun 链上赎回说明并存。
- “Predict.fun 不支持结算”与 `SupportsSettlement=true` 及自动结算说明并存。
- 旧“最多 500 个市场”描述未跟随多视图、事件尾盘扫描更新。
- Polymarket 的 USDC / pUSD、V1 / V2 限制混在同一当前能力描述中。
- 凭据空值、脱敏占位符、继承部署环境的描述不统一。
- API 契约/历史记录仍描述 PF“广播即成功、未等回执”，当前实现实际等待 receipt，并拒绝 status=0；当前问题是未知结果恢复、hash 保留及额外确认策略，不是完全不等回执。
- `docs/predictfun-integration-plan.md` 的“全部处理”“不阻塞交付”是历史实施记录，不是本次重新核验所有资金边界的结论。

影响：排障时无法确认系统是否真的支持某种操作，或把策略账面收益误认为可支取现金。文档已经声明的限制仍需根据实际后果分级，不因“已知限制”就排除审查。

建议保留历史研究但加版本与适用范围；当前能力由 API 能力表、网络/协议配置、测试夹具及明确的财务字段定义共同约束。禁止把“订单已匹配”“交易已广播”“平台已公布结果”“链上已确认”“钱包已到账”写成同义词。

## 6. 引擎、风控与账本

LED-01 的手动准入缺口与 LED-02 的全额/部分退出时序已由隔离测试直接确认，证据 A，详见第3.2节。其余为源码/调用链证据 B；没有把未执行的验收场景写成测试结果。

### LED-01 / P1：风险约束未覆盖所有实盘入口和恢复路径

位置：`backend/internal/engine/engine.go:2519-2565`、`backend/internal/engine/engine.go:2676-2719`、`backend/internal/engine/engine.go:2192-2193`、`backend/internal/engine/engine.go:2246-2249`、`backend/internal/engine/engine.go:2921-2928`。

手动 `PlaceOrder` 直接进入共用实盘买入，风险计算只统计已有 Position，没有预留 pending/unknown 订单成本；事件限额只在自动策略外层检查。未知买单恢复为 Position 时又遗漏 `EventID`，使后续事件敞口持续少算。另有 `backend/internal/engine/engine.go:2805-2807` 未决查询失败返回 `0, err`，而 `backend/internal/engine/engine.go:1644-1664` 仅记录错误、不因此阻断新增风险。

后果：上一笔可能成交但尚无持仓时，第二笔手动订单仍可提交；同事件不同市场的手动单可突破事件限额；通过对账恢复的仓位也可被自动限额漏算。钱包余额充足不代表风险限额得到遵守。

修复：把事件、市场、总敞口及未决状态统一放到共用准入；最小版本有未决就拒绝新增风险，允许继续交易则必须做持久化风险预留。恢复仓位保留 EventID；对已有空值设计可审计修复。未决读取失败必须 fail-closed。

验收：unknown 后再次手动买入；同事件两市场手动买入；补账后自动入场；未决查询失败而其他查询成功。现有 `backend/internal/engine/engine_test.go:1842` 与 `backend/internal/engine/engine_test.go:1244` 分别只覆盖单笔恢复、paper 自动事件限额。

### LED-02 / P1：未决卖单可被参考结算越过，后到真实成交永久漏账

位置：`backend/internal/engine/engine.go:1644-1654`、`backend/internal/engine/engine.go:1949-1978`、`backend/internal/engine/engine.go:2875-2886`。

止损卖单结果 unknown，下一轮查询仍失败但市场有合法终局数据时，`refreshPositions` 仍可按全部本地份额结算。之后查到真实卖出，因仓位已经关闭，对账将订单标成 rejected，原因是“止损订单对应的持仓已不存在，请人工核对”。真实卖出所得、手续费、成本分摊未入账，且订单不再属于未决集合。

隔离复现：100份、成本98，全额卖出gross75、fee0.10后市场胜出，本地仍记兑付100、收益+2，而非−23.10，高估25.10。部分卖40份、gross30、fee0.10、剩余60份胜出时，本地仍为+2，而正确值为−8.10，高估10.10。两个变体都已命中异常断言。这里是本地账本错记，不是链上重复兑付。

修复：未决退出冻结该仓位的不可逆结算；查明成交后只结算剩余份额。已关闭仓位后到真实成交必须进入待修复状态，不能标 rejected 就解除风险阻断。验收覆盖全额/部分成交、查询超时、跨终局、重启恢复；现有 `backend/internal/engine/engine_test.go:967` 未覆盖跨终局。

### LED-03 / P1：机会发现故障会跳过持仓维护，独立刷新预算不完整

位置：`backend/internal/engine/engine.go:1554-1558`、`backend/internal/engine/engine.go:228-242`、`backend/internal/engine/engine.go:1856`、`backend/internal/engine/engine.go:1889-1909`、`backend/internal/engine/engine.go:1978-1985`。

全部 ListOpen 失败会直接返回，跳过对账、持仓刷新和止损，即使按 ID 查询仍可用。venue 预算以固定 90 秒分配再截到父 context，较短预算下第一慢源可耗尽后续时间。行情补查虽然有脱离父取消的 context，但初始持仓读取和最终 SQL 写入仍用可能已过期的旧 context，额外获取的新价格可能无法落库。

另一方向：批量接口少返回某个 ID 且 error=nil 时，`backend/internal/engine/engine.go:1892-1905`、`backend/internal/engine/engine.go:1929-1933` 可将刷新当成功，留下旧估值继续参与判断。

修复：风险维护与机会发现分离；风险阶段拥有覆盖读取、请求、落账的完整有界生命周期，不依赖发现成功；逐 ID 检查批量完整性。未知估值显式标记且禁止新增风险。验收覆盖仅 ListOpen 失败、发现耗尽预算、batch 少行且 nil error；现有测试多用 Background 或会为缺行返回错误的 fake，未覆盖这些组合。

### LED-04 / P1：配置部分应用失败后，实际执行器与保存/展示配置分裂

位置：`backend/internal/engine/engine.go:527-555`、`backend/internal/engine/engine.go:814-850`。

应用顺序为 Polymarket 更新成功、再更新 Predict.fun；后者失败时 rollback 只恢复数据源 URL 和 proxy，没有恢复已经变化的 Polymarket 执行器。外层返回错误且不保存，UI/SQLite 仍是旧配置，实际下单却可能使用候选 key 或 CLOB URL。数据库保存失败后的恢复错误也被忽略。

修复：短期把所有已应用对象纳入逆序回滚，并暴露回滚失败、禁用受影响交易；中期先构造校验候选客户端，再一次切换完整配置 generation。验收用 fake PM 成功、PF 失败，以及存储失败/回滚失败，断言持久化、展示、实际签名器一致。

### LED-05 / P2：Dashboard 并发初始化可永久覆盖实盘本金

位置：`backend/internal/engine/engine.go:2994-3052`、`backend/internal/engine/engine.go:3125-3143`、`backend/internal/store/sqlite.go:776-779`。

Dashboard 不持交易锁，先读旧账户/持仓，再读钱包；baseline 更新为无条件 `UPDATE accounts SET baseline_equity = ? WHERE id = ?`。可发生：Dashboard 读到 baseline=0、持仓0；交易线程初始化1000并买入100；Dashboard 再读钱包900，按旧持仓0把 baseline 覆盖为900。之后现金、收益率及资金约束使用错误本金。

修复：初始化改条件写入，竞争失败后重读；用于首次计算的持仓与钱包必须一致且新鲜。不要直接让 Dashboard 拿非重入 tradeMu，因为扫描持锁也会经 snapshot 调用 Dashboard。验收用 channel/barrier 控制顺序，不依赖偶发竞态。`CashSnapshot.Stale` 是否被适配器返回需与对应模块证据结合，不擅自假设。

### LED-06 / P2：赎回防重检查非原子、限于最近 500 条，pending 无恢复

位置：`backend/internal/engine/engine.go:650-695`、`backend/internal/store/sqlite.go:1041-1047`。

检查和 pending 创建发生在 tradeMu 之前；两个并发请求都能通过再串行广播。只查近 500 条也会漏掉历史赎回。创建 pending 后崩溃会阻断重试；普通未决查询过滤 `external_id <> ''`，赎回没有 ExternalID，不会被恢复。tx hash 仅写 Reason，状态写入失败被忽略。

后果是重复 gas、操作永久卡住或状态丢失，不推断可重复领取兑付款。PF 执行器当前会等待成功 receipt，最长三分钟，明确拒绝 status=0；因此不能把引擎 filled 路径误写成“广播即成功”。但普通错误会把 intent 标 rejected，广播/回执结果未知的错误又可能丢失结构化 hash，见 PF-05。

修复为按 position 的原子占位/唯一约束、专门链上操作表与恢复状态；不能简单把 tx hash 填进 ExternalID，让 CLOB 对账去查链上交易。

验收：并发双请求、500+历史订单、pending 后崩溃、广播后写库失败、交易回滚/丢弃。现有 `backend/internal/engine/engine_test.go:2660` 仅测顺序重复。

### LED-07 / P2：Predict.fun API key 未纳入 SQLite 加密

位置：`backend/internal/store/sqlite.go:605-645`、`backend/internal/store/sqlite.go:527-588`、`backend/internal/engine/types.go:481-485`。

保存/加载加密 PrivateKey、APISecret、APIPassphrase，却遗漏 Predict.fun 用于 `x-api-key` 的 APIKey。因此数据库副本无需 `.key` 文件即可暴露该认证凭据。不推断仅凭 API key 就能转移资金。

修复：对 PF APIKey 增加现有 Encrypt/Decrypt 与旧明文升级路径，不混淆 PM APIKey 标识符语义。扩展 `backend/internal/store/system_settings_test.go:183-220`，将 APIKey 加入原始 payload 不得包含的秘密列表，覆盖重开与旧库迁移。

### LED-08 / P2：模拟止损离开雷达后丢失市场费率，退出费用少计

位置：`backend/internal/engine/engine.go:2371-2396`、`backend/internal/venue/fee.go:21-30`。

卖出费率从机会列表查找，触发止损的 token 往往已跌破 MinPrice、离开雷达，因而回退全局费率。PF 市场费率200bps、默认15bps、100股按0.75卖出且滑点0时，按给定市场费率的静态模型应计 `0.02×0.25×100=0.5`，回退仅计 `0.0015×0.25×100=0.0375`，为应计的7.5%，少计0.4625（92.5%），模拟收益相应多计0.4625。

修复：使用刷新后的市场 fee schedule；若需要入场回退，持久化模型版本/费率并标记来源，不依赖候选列表。验收入场后价格跌出雷达而费率高于默认的完整流程。现有 `backend/internal/engine/engine_test.go:2198` 只覆盖入场费用。

### LED-09 / P2：年化起点包含拒单、未决单，而非首次实际入场

位置：`backend/internal/store/sqlite.go:1023-1038`、`backend/internal/engine/types.go:1323-1326`、`backend/internal/engine/engine.go:3086-3093`。

`EarliestAutoBuyAt` 只过滤 auto/buy，未过滤 filled。仅有拒单也会显示已运行，早期拒单会拉长首次成交后的年化分母。修复先限定 filled 并明确创建时间/成交时间口径；若需要首次尝试指标另设字段。验收 rejected/pending/unknown-only 为 nil，以及先拒后成交按首次成交口径计算。现有 `backend/internal/engine/engine_test.go:761-839` 只有已成交样本。

### LED-10 / P2：扫描、快照重复读全历史，零未决查询也缺专用索引

位置：`backend/internal/engine/engine.go:2066`、`backend/internal/engine/engine.go:2138`、`backend/internal/engine/engine.go:2582`、`backend/internal/engine/engine.go:2174`、`backend/internal/engine/engine.go:2676`、`backend/internal/engine/engine.go:3151`、`backend/internal/store/sqlite.go:227-233`、`backend/internal/store/sqlite.go:1041-1047`。

自动循环先读全部持仓，每次买入再重读；K笔新单、H条历史可形成约 O(K×H) 重复加载。snapshot 经完整 Dashboard 还读取计数、500条旧快照。数据库单连接会放大慢读对落账/API的阻塞。未运行 EXPLAIN 或负载基准，不声称当前已超时。

修复顺序：未决查询部分索引；开放仓位/已实现汇总与“曾交易”查询分离；同一串行决策中复用风险汇总并随成交更新；快照用专用资金汇总。先测历史规模增长曲线，不先换库或扩大连接池。

建议索引需经真实迁移夹具和执行计划验证：

```sql
CREATE INDEX IF NOT EXISTS idx_orders_unresolved_account_id
ON orders(account_id, id)
WHERE status IN ('pending', 'unknown') AND external_id <> '';
```

### 已有保护与未升级为缺陷的边界

- 已存在签名后、提交前持久化 pending，自动路径通常会因 unknown 阻止新增风险；本次指出的是入口与错误路径缺口，不是系统完全无防护。
- 引擎约定 `Fill.Cost` 为 gross，买入加一次 fee、卖出减一次 fee。引擎自身算式符合该契约，但适配器不都遵守：PM 直接卖出先扣费造成组合后重复扣费，见 PM-01；PF 实际买费扣份额与当前表达冲突，见 PF-03。不能把单层正确推广为跨层费用正确。
- 部分卖出按比例分摊成本，后续结算累加剩余收益；订单和持仓 finalize 有事务与本地幂等保护，位置 `backend/internal/store/sqlite.go:1061-1085`、`backend/internal/store/sqlite.go:1127-1151`、`backend/internal/store/sqlite.go:1158-1178`。
- 账户编辑不覆盖 baseline，账户 venue/currency 隔离、对明确标为 `version=v2` 的 Protocol V2 市场拒单（不指 CTFExchangeV2/domain version `2`）、止损最小数量及避免已交易市场重入保护存在。
- 未证实的旧 schema+扩展列/trigger 组合不列为当前确定性迁移缺陷；当前引擎明确写入快照币种，不把 store 的默认币种分支误报为已触发串币。
- 未验证生产规模性能、真实账户盈亏或真实链上到账。

## 7. 官方契约交叉核对

检索日期：2026-11-12。以下是公开文档/规范证据，不是对生产账户或交易端点的测试。页面会变化，实施时应固定 OpenAPI/SDK 版本及夹具；版本不明的条目保持待验证。

| 官方来源 | 直接支持的事实 | 对本项目的约束 |
| --- | --- | --- |
| [Polymarket Market Details](https://docs.polymarket.com/market-data/market-details) | closed：“Market has resolved or been closed, so no further trading is possible.” | closed 不是只表示最终解析，不能独立证明派彩 |
| [Protocol V2 API Migration](https://docs.polymarket.com/migrate/polymarket-v2/api-integrations) | “For V2, `resolutionStatus` is `inactive`, `active`, or `resolved`.”；“For V1, keep using `umaResolutionStatus`.” | 结算状态字段跟随协议，缺字段不默认为 final |
| [Get trades](https://docs.polymarket.com/api-reference/trade/get-trades) | “Retrieves trades for the authenticated user. Returns paginated results.”；next_cursor 的 `LTE=` 表示结束 | `/data/trades` 必须按游标取全并验证覆盖，不能按第一页数量推断已完整 |
| [Place Orders](https://docs.polymarket.com/trading/place-orders) | “A limit order specifies the price at which you are willing to trade”；“Settlement happens asynchronously after an order matches…” | 限价不等于成交均价，matched 不等于链上确认 |
| [Polymarket Fees](https://docs.polymarket.com/trading/fees) | “Taker fees are calculated in USDC and vary based on the share price.”；`fee = C × feeRate × p × (1 - p)` | 费用公式和实际抵押品/扣费腿要分别按版本核实 |
| [Predict.fun REST OpenAPI](https://dev.predict.fun/openapi/rest.json) | OpenAPI 3.2.0、info.version 1.0.0；盘口基于 Yes 轴；resolution 是 Outcome 对象 | 不把 No 价格直接照搬 Yes；价格、统计金额、成交字段要分层 |
| [Predict.fun Manage positions](https://dev.predict.fun/trading/manage-positions) | “A binary market can also resolve 50-50, which pays 0.50 USDT per share of either outcome.”；“USDT on BNB Chain has 18 decimals.” | 平局不能强行取单一 winner；金额与数量精度要按18位处理 |

### 7.1 Polymarket 协议、抵押品、费用不可混为一个版本号

迁移页区分：CTF 路径使用 CTFExchangeV2、EIP-712 domain version `"2"`；Protocol V2 路径使用 ExchangeV3/PositionManager、domain version `"3"`。文案中的“V2”可能指不同层，不能仅按名称兼容。

迁移页明确：`CONDITIONAL-V2` 选择 V2 positions，`COLLATERAL` 选择 pUSD。该迁移以已有 pUSD + CTFExchangeV2 集成为起点，不能简单写成“全部 V1 都是 USDC，全部 V2 才是 pUSD”。Fees 页仍使用 USDC 名称，是需要保留并核对的官方口径差异。

Protocol V2 / ExchangeV3 的明确原文为：“Reconcile fills and fees separately: BUY fees add to collateral spend; SELL fees are deducted from proceeds.” 但本次资料不足以断言 CTFExchangeV2 BUY 费用是否仍扣份额。不能把历史“买费扣份额”当作所有当前协议的必然规则，也不能把 V2 的规则直接套用到旧路径。上线前需固定合约、抵押品 token、SDK版本并逐笔核对实际数量/扣费腿。

CLOB OpenAPI 为 3.1.0、info.version 1.0.0。成交状态存在 MATCHED、MINED、CONFIRMED、RETRYING、FAILED 等阶段；原始 API 与 SDK 名称映射需固定版本验证。每页示例100不是已核实的永久硬上限；历史保留期限和全量上限本轮未证实。成交均价应为 `Σ(price×size)/Σ(size)`，手续费独立归集，并确认归属的是当前订单。

### 7.2 Predict.fun 当前规范与历史研究有差异

本次公开 OpenAPI 抓取成功；当前 Outcome 要求 `onChainId`，未包含 `positionId`。历史 `docs/research/predictfun-settlement.md` 有不同的测试网字段观察，不能推断主网当前响应必定同构。

统计字段分别为 `totalLiquidityUsd`、`liquidity3cAskUsd`、`volumeTotalUsd`、`volume24hUsd`。`/orders/matches` 提供 `amountFilled`、`priceExecuted`、`transactionHash`、`executedAt`；“A match event describes a whole transaction”，因此还要明确具体 maker/taker/order 归属后再累计。

官方赎回按两个维度分流：

| 类型 | 合约 |
| --- | --- |
| 普通 | ConditionalTokens |
| 普通收益型 | YieldBearingConditionalTokens |
| Neg-risk | NegRiskAdapter |
| Neg-risk 收益型 | YieldBearingNegRiskAdapter |

普通路径赎回指定 outcome 的全部余额，neg-risk 传明确数量。这与“应用只跟踪一部分份额”的策略账本边界需区分。官方确认50/50兑付存在，但“两侧WON”的具体编码本轮只有历史测试网记录，不扩写成完整主网契约。SDK确认数、`tx.wait(1)`的固定版本行为也未重新核实。

链上管理本质是钱包发交易；广播哈希应记 submitted，成功 receipt、足够确认、资产变化和应用账本确认分开保存。当前 PF 已等待成功 receipt，不代表已实现额外确认数、重组恢复或资产变化核对。

## 8. Predict.fun 契约、成交与链上操作

PF-01请求结构与PF-02嵌套GET解码已由严格本地fixture确认，证据 A，详见第3.3节；这不是生产API验证。其余为源码与当前官方契约交叉证据 B；畸形/冲突响应场景不声称已在生产发生。主要官方依据为第7节 OpenAPI，以及 [下单](https://dev.predict.fun/trading/place-orders)、[订单生命周期](https://dev.predict.fun/trading/order-lifecycle)、[撤单](https://dev.predict.fun/trading/cancel-orders)、[费用](https://dev.predict.fun/trading/fees)。

### PF-01 / P1：创建和撤销订单 DTO 与当前公开契约不符

位置：`backend/internal/predictfun/client.go:404-408`、`backend/internal/predictfun/executor.go:643-648`、`backend/internal/predictfun/client.go:1479-1490`、`backend/internal/predictfun/executor.go:679-683`。

当前下单为 `{data: SignedOrder, pricePerShare, slippageBps, isMinAmountOut}`；官方要求 `{data:{order:ContractOrder, pricePerShare, strategy, slippageBps, isMinAmountOut}}`。`data.order/data.pricePerShare/data.strategy` 必填，`slippageBps` 为字符串。签名对象和 REST 包装不是同一种结构。

当前撤单为 `{orderId:...}`，官方要求 `{data:{ids:[...]}}`；结果 `removed/noop/rejected` 在顶层，当前忽略结果。即使只改对请求，也可能把 removal lock 的 rejected 当成功；部分成交后的撤单错误还被执行器丢弃。

后果：严格遵循当前契约的服务会拒绝请求；本地自洽 mock 无法证明真实互通。`backend/internal/predictfun/test_helpers_test.go:130-152` 接受当前 DTO；`backend/internal/predictfun/client_test.go:254-284` 反而断言旧撤单格式。

修复/验收：固定官方 schema，分离签名 DTO、提交 DTO、状态 DTO；校验必填层级和字符串类型；逐订单处理 removed/noop/rejected；ID/hash 端点不可混用。严格 fixture 拒绝旧结构，部分成交取消失败保持待恢复。

### PF-02 / P1：合法嵌套订单响应无法完成对账，兜底又用委托参数冒充成交

位置：`backend/internal/predictfun/client.go:414-434`、`backend/internal/predictfun/client.go:481-517`、`backend/internal/predictfun/client.go:1457-1473`、`backend/internal/predictfun/executor.go:717-803`。

官方 GET 为外层 `id/status/amountFilled`，内层 `order.makerAmount/takerAmount/side`。当前有外层 id 就不继续解析内层 order；官方外层也无 pricePerShare 或累计成交 USDT。结果可以得到正数 amountFilled，却没有价格/金额，LookupOrder 返回 unknown、Cost=0，真实成交无法入账。

独立地，`fillFromAck` 在缺少实际数量时回退请求份额，价格回退签名金额比例/限价。原始订单规模、最低接收量和限价都不是实际成交数量/VWAP。官方 MARKET 部分成交也可能终态 FILLED，不能因状态名补满原始数量。

修复：提交接受、订单状态、成交明细分层。按 `/v1/orders/matches` 的订单/fill hash 归属提取实际份额、成交额及费用，不累计整笔 match 的其他 maker；缺证据保持 unknown。LIMIT OPEN 的 amountFilled 可继续增长，解码修好后必须继续跟踪累计增量和剩余挂单，不能首次正数就永久 finalize。

验收：真实嵌套 GET、部分 FILLED、两档 VWAP、缺金额、嵌套 SELL、明确零费、后续增长的 OPEN。`backend/internal/predictfun/executor_test.go:71-137` 当前用只有 id/status=MATCHED 的 fake，并把请求估值断言为成交；该测试需改为证据不足不入账。

### PF-03 / P1：买入费用实际扣份额，当前账本却按额外 USDT 支出

位置：`backend/internal/predictfun/client.go:520-532`、`backend/internal/predictfun/executor.go:763-772`、`backend/internal/predictfun/executor.go:1006-1014`；消费端 `backend/internal/engine/engine.go:2766-2780`。

官方 Fees 明确：“Buy: you receive fewer shares. The fee is taken in shares, `fee / p` of them.” USDT 等值公式 `rateBps/10000 × min(p,1−p) × shares` 本身正确，但不能因此认为 USDT 实际支出增加。

按上述官方费用规则计算，非本轮成交实测：100份、p=0.5、200bps，应支出50 USDT、净得98份；当前适配器返回100份、gross50、fee1，引擎入账成100份、成本51。数量、成本、PnL、后续可卖/可赎数量同时偏离。PF-01/02 可能先阻断成交路径，但修请求并不会自动修费用资产。

另有 `FeePaid()>0` 把明确零费与缺值混同，maker 零费可能再次按 taker 模型估算。官方 match 的 `fee.type=SHARES/COLLATERAL`、`fee.amount` 应参与入账。

修复：扩展成交契约以保存费用资产、实际 collateral delta、净份额；保持跨场所兼容并逐场所验证，不能机械把所有 BUY 都改成同一种费用腿。验收50 USDT→98份、SELL净收入、maker零费及折扣；`backend/internal/predictfun/executor_test.go:120-127` 只核对费用等值，覆盖不足。

### PF-04 / P1：没有一致终局 verdict 时仍可拿报价当派彩

位置：`backend/internal/predictfun/provider.go:371-425`、`backend/internal/predictfun/provider.go:755-784`；下游 `backend/internal/market/market.go:194-222`。

找不到 winningOutcome 只在 ResolutionStatus 为空时设 pending。如果输入 status=RESOLVED、tradingStatus=CLOSED、resolution=null、两侧无WON/LOST，却保留约0.5报价，下游可识别为平局兑付；极端旧报价也可识别为1/0。反向地，找到 resolution/indexSet 或 WON 后直接升为 resolved，没有核对生命周期最终性或冲突证据。

这是异常/不完整响应下的 fail-open，不主张正常生产 API 必然产生组合。普通赢家和50/50映射已经实现，不能把历史“未实现”描述重复作为发现。

修复/验收：无一致 verdict 无条件不可结算；价格与派彩独立字段；RESOLVED+无verdict+0.5/0.5、极端旧报价、非终态与WON冲突均不入账。`backend/internal/predictfun/provider_test.go:257-272` 的 CLOSED+0.97/0.02 未覆盖危险组合。

### PF-05 / P1：发送或回执结果未知时丢失结构化交易 hash

位置：`backend/internal/predictfun/approvals.go:451-483`、`backend/internal/predictfun/redeem.go:247-251`、`backend/internal/predictfun/executor.go:361-375`。

签名后 hash 已确定，SendTransaction 出错却返回空 hash，无法判断节点是否已接受。广播成功而 receipt 查询失败时 sendCall 返回 hash，但上层 Redeem/Executor 再将结果清空；错误文本可能含 hash，不等于可机器恢复的交易身份。

当前实现确实等待 receipt、拒绝 status=0，最长三分钟；问题是未知阶段错误被当普通失败，加上 LED-06 可被标 rejected 并允许再次请求。重复 gas 与恢复丢失是风险，不是重复领取兑付。

修复：签名后广播前可靠登记 chain/from/nonce/hash，错误结果仍携带阶段和身份；unknown 由独立链上恢复器查证，不自动新 nonce 重发。验收节点接收后断开、receipt超时/取消、明确回滚、广播后写库失败。现有 approvals/redeem 测试以状态/编码为主，未覆盖这些 RPC 故障。

### PF-06 / P1：旧配置请求可重新污染新钱包/网络缓存

位置：`backend/internal/predictfun/client.go:900-911`、`backend/internal/predictfun/client.go:963-987`、`backend/internal/predictfun/client.go:1180-1221`、`backend/internal/predictfun/provider.go:151-161`、`backend/internal/predictfun/executor.go:163-173`、`backend/internal/predictfun/executor.go:917-949`。

时序：Collateral 固定钱包A的链客户端并暂停；切换B时重建客户端/清空缓存；A响应随后以当前时间回填 snapshot；B下一次得到A余额，Stale=false、error=nil。JWT、市场元数据、统计缓存有同类旧请求写回；endpoint/API key 与 bearer 签名流程也可能分别读取不同代配置。

这是逻辑竞争，不要求 Go race detector 报数据竞争。普通陈旧余额回退实际伴随非nil error，不与该“旧值重新当新值”场景混淆。

修复：不可变配置快照+generation；请求、签名、认证及缓存写回绑定同一代；旧代结果不得写回，提交边界复核。验收 barrier 精确控制 A查询→切B→A返回，再覆盖 JWT/元数据；`backend/internal/predictfun/client_reconfigure_test.go:191-235` 的并发 getter 不覆盖完整异步流程。

### PF-07 / P2：分页发送 cursor，而官方请求键为 after

位置：`backend/internal/predictfun/client.go:1264-1272`、`backend/internal/predictfun/provider.go:192-235`、`backend/internal/predictfun/provider.go:648-672`。

响应 cursor 应送回 `after`，当前发 `cursor=`。上游忽略未知参数时会反复读第一页（最多五次），去重后遗漏后续市场；拒绝参数时则第二页失败但部分结果可能仍无整体错误。未检测重复cursor。

修复/验收：改用after，验证游标前进并公开截断；两页fixture只对after返回第二页，覆盖重复cursor/空终页。`backend/internal/predictfun/client_test.go:16-63` 只验证接收到cursor，没有请求下一页。

### PF-08 / P2：Quote 与 Buy 的盘口规划不一致

位置：`backend/internal/predictfun/executor.go:505-540`、`backend/internal/predictfun/executor.go:862-910`。

静态算例：asks=0.50×1份、0.60×100份，预算10、tick0.01、maxPrice0.70。Quote 得 LimitPrice0.60、Shares16.666666、Notional约9.90、FullyFilled=true；Buy 只按bestAsk+2%得到边界0.51、最低份额约19.607843，而0.51内仅1份/0.50 USDT。报价足量不能约束实际订单。AvailableNotional 还累计限价外深度，不符合接口“限价内金额”语义。

修复：纯函数订单规划器统一预算、深度、限价、滑点、取整和min-out。验收至少两档盘口和限价外巨额深度；`backend/internal/predictfun/executor_test.go:402-439` 的“走深度”仅单档，不能证明一致性。算例不是生产成交实验。

### PF-09 / P2：测试网 REST/签名与主网 RPC 可以混用

位置：`backend/internal/config/config.go:73`、`backend/internal/predictfun/client.go:604-635`、`backend/internal/predictfun/client.go:845-858`、`backend/internal/predictfun/approvals.go:174-189`。

启动传入非空默认主网RPC，本模块只在RPCURL为空时选测试网默认；运行时切网只重置REST。可形成REST=testnet、签名chainID97、RPC实际56。链客户端只dial，不验证eth_chainId，余额、nonce、allowance和合约读取可能跨网。

正常节点会拒绝chainID不匹配的签名，不能表述成“必然向主网转账”。修复区分默认/显式RPC，默认随网络切换，任何新RPC首次使用验证chainID。验收RPC56+配置97在读余额和发送前明确失败；当前重配测试只验证REST和本地chainID。

### PF-10 / P2：重启后已结算仓位赎回依赖无法补齐的冷缓存

位置：`backend/internal/predictfun/executor.go:338-350`、`backend/internal/predictfun/provider.go:588-614`、`backend/internal/predictfun/provider.go:648-655`。

Redeem 依赖token→market metadata；冷缓存只通过status=OPEN扫描重建，已结算市场不在集合中。引擎已有MarketID却未传入按ID解析或预热，普通重试不改变查询集合。

修复：按持仓MarketID确定性加载赎回元数据，或持久化condition/indexSet/neg-risk/协议等必需信息，不全扫历史替代。验收空缓存、OPEN空、按ID可取resolved，保证无需广播也可验证解析成功；现有fixture通常已预热开放市场。

### PF 已有保护与保留事项

- EOA-only、明确拒绝yield-bearing、缺tick/fee元数据拒绝签名；domain区分链和exchange，签名前核对signer/chain。
- 签名hash在HTTP提交前交持久化回调；401/429复用相同payload，不盲重试5xx。
- 授权先查状态、跳过已满足项，覆盖NegRiskAdapter，同批次递增nonce；普通/neg-risk赎回有calldata测试。
- receipt status=0明确失败。额外确认数、重组恢复、到账核对仍需设计。
- 未证明EIP-712哈希算法错误；自身签名/恢复一致不替代官方SDK golden vector。当前onChainId与历史positionId材料冲突，保留版本边界。
- 未测真实生产API、RPC、成交或吞吐；所有修复场景除明确记录的探针外均为待执行验收。

## 9. Web、HTTP API 与操作真实性

WEB-02 flat配置往返已由隔离API测试直接确认，证据 A，详见第3.3节；其余为源码证据 B。没有执行浏览器攻击、真实账户操作或链上验证；对浏览器策略、跨站可达性等外部前提保持限定。

### WEB-01 / P1：无访问密码时，写 API 缺少独立跨站请求防护

位置：`backend/internal/api/router.go:23-35`、`backend/internal/api/router.go:48-65`、`backend/internal/api/auth.go:236-245`、`backend/internal/api/system.go:252-266`。

密码关闭时鉴权直接放行，未校验Origin、可信Host、Fetch Metadata或CSRF token。`POST /scan?execute=true`、`POST /system/live/approve?venue=predictfun` 无需请求体，跨站表单可表达；JSON handler也未先限制Content-Type。在浏览器允许向服务发请求且实盘已配置的条件下，恶意站点可能触发交易或链上授权。localhost绑定不等于完整CSRF防线；未验证各浏览器私网访问限制，不声称任意浏览器必然可利用。

修复：写请求统一校验来源/可信Host，不随密码开关禁用；JSON端点限制媒体类型，明确非浏览器客户端认证策略。验收外站Origin空体POST不得调用fake approver/执行器，text/plain配置请求拒绝，同源合法请求正常。密码开启时SameSite=Strict是已有保护。

### WEB-02 / P1：正常 UI 第二次保存 PM 配置可能无效却返回成功

位置：`backend/internal/api/system.go:41-82`、`backend/internal/engine/types.go:773-813`、`frontend/src/views/system.tsx:1202-1205`、`frontend/src/views/system.tsx:1231-1233`。

当前UI提交flat data_source/live。后端保留已有venues.polymarket，修改flat后调用SyncPolymarketVenue；其中Normalize先让旧venue block覆盖新flat值。因此保存A后改B或清空，可能仍用A并返回200。不是仅影响历史客户端；凭据轮换、endpoint切换、清空覆盖都受影响，掩码又降低可见性。

修复：明确patch优先级，把本次flat patch写到对应venue再规范化，逐步保留单一权威配置表示。验收API round-trip A→不同B→清空，检查持久化与实际runtime；现有 `backend/internal/api/system_auth_test.go:245-329` 主要测相同值/mask回传。

### WEB-03 / P1：授权按钮使用已应用配置，却位于可编辑草稿钱包/网络卡片

位置：`frontend/src/views/system.tsx:821-897`、`frontend/src/views/system.tsx:966-1017`、`backend/internal/api/system.go:252-266`。

保存主网钱包后，仅在草稿切测试网或换私钥，仍能点授权；POST只带venue，实际用旧runtime。授权状态effect只依赖runtime.configured，换钱包仍为true时不重查，旧授权状态继续显示。页面已有gas警告和部分runtime标识，不能写成完全无提示；问题是操作对象不与草稿/版本绑定。

修复：dirty、检测/保存中禁用副作用操作；确认框展示已应用chain、signer/maker及目标合约；请求携带expected revision，服务端拒绝过期配置。验收主网runtime+测试网dirty draft不发POST；保存换钱包后必重查授权。

### WEB-04 / P2：提交未知被显示为未提交，链上等待超过 HTTP 写超时

位置：`frontend/src/views/opportunities.tsx:381-383`、`frontend/src/views/opportunities.tsx:562`、`backend/internal/api/system.go:257-297`、`backend/cmd/server/main.go:85-91`。

订单响应丢失不代表未发送，但非Abort错误统一显示“订单未提交”，诱导重试。授权/赎回同步等待最长三分钟，而HTTP WriteTimeout60秒；60–180秒成功时可能无法回写响应，断连也不会撤销链上效果。

修复：明确拒绝与结果未知分开，未知先查询持久化operation/order身份；统一有界请求时限，长操作采用可查询状态而不是单纯延长所有超时。验收已处理但响应断开、fake慢成功、重复查询；ResponseRecorder不能覆盖传输层超时。

### WEB-05 / P2：过期检测、probe、保存响应可以覆盖新草稿

位置：`frontend/src/views/system.tsx:319-432`、`frontend/src/views/system.tsx:870-871`、`frontend/src/views/system.tsx:1111-1154`、`frontend/src/views/system.tsx:1314-1320`、`frontend/src/views/settings.tsx:743-746`。

detectWallet捕获A草稿，await后仍把A的signature_type/funder合并到最新B草稿；手动选择也可被覆盖。600ms debounce只取消未发定时器，未淘汰在途请求。PM/PF凭据、relayer、proxy probe同样可在编辑清空后迟到写回“通过”。保存响应也可替换提交后继续编辑的内容。

修复：草稿revision覆盖密钥、网络、endpoint、手动选择；所有异步结果只接受同代，结合AbortController；保存期间冻结section或按revision回填。验收A/B逆序返回、检测中手选、编辑后旧probe成功、保存后继续编辑。

### WEB-06 / P2：报价未绑定金额/时效，旧估算仍标真实净收益

位置：`frontend/src/views/opportunities.tsx:69-70`、`frontend/src/views/opportunities.tsx:300-325`、`frontend/src/views/opportunities.tsx:536-551`。

报价缓存仅venue:token，开框/改金额/刷新行情不失效，失败保留原值。先查1再改100仍显示旧净收益/深度；小字确有旧金额和成交差异说明，但醒目“真实净收益”不代表当前单真实结果。不能因此说服务端必按旧报价执行。

修复：query identity包含account、venue、token、金额、配置revision；金额变化立即失效，TTL和过期标识，淘汰旧请求；改称“盘口估算净收益”。验收1→100、过期、刷新失败、逆序响应。

### WEB-07 / P2：局部保存的读改写在锁外，仍会覆盖其他并发配置

位置：`backend/internal/api/system.go:41-82`、`backend/internal/api/router.go:389-411`、`backend/internal/engine/engine.go:527-555`。

handler锁外读完整配置并局部合并，随后传完整快照入写锁；两个客户端改不同section也能互相覆盖，甚至恢复旧凭据。扫描间隔保存同类。修复在同一写边界完成读/patch/验证/保存，或revision条件更新。验收barrier并发不相交patch都保留，冲突patch明确409/重试规则，不静默last-write旧快照。

### WEB-08 / P2：登录限速信任转发头，昂贵验证前也未预占额度

位置：`backend/internal/api/router.go:23-25`、`backend/internal/api/auth.go:205-227`、`backend/internal/api/auth.go:286-302`。

Gin v1.10.1默认信任所有代理，未配置trusted proxies却按ClientIP限速，同一来源轮换X-Forwarded-For可绕开五次/分钟。allow→bcrypt→recordFailure非原子，并发可在失败记录前全部进入验证。

修复：无代理关闭信任，有代理明确白名单；bcrypt前原子消耗额度并限制并发。验收同RemoteAddr变头及并发失败；`backend/internal/api/system_auth_test.go:145-160` 仅顺序同IP。

### WEB-09 / P2：旧密码验证可在改密撤销之后签发新会话

位置：`backend/internal/api/auth.go:94-107`、`backend/internal/api/auth.go:130-136`、`backend/internal/api/auth.go:166-175`、`backend/internal/api/auth.go:291-307`。

Verify复制旧hash后解锁bcrypt；改密更新hash并清sessions；旧Verify稍后成功，独立newSession仍能发七天会话。修复验证携带credential revision，签发时锁内复核。验收旧验证开始→改密完成→旧验证返回，必须拒绝签发；顺序改密撤销已有效，不泛化为撤销完全失效。

### WEB-10 / P2：GET/PUT system 将 PF API key 原值送回浏览器

位置：`backend/internal/api/system.go:125-135`、`backend/internal/engine/types.go:503-505`、`backend/internal/engine/types.go:698-709`。

PF复用PM脱敏逻辑未遮APIKey，但PF该字段是x-api-key认证凭据。api_key_configured已足够支持UI。与LED-07数据库明文是不同暴露面。

修复venue-aware redaction、mask round-trip及清空语义；`backend/internal/api/system_auth_test.go:408-413` 目前反而要求明文返回，改为响应不含原值。未发现私钥经此泄漏，也不推断API key单独可签名转账。

### WEB-11 / P2：测试网、草稿和当前生效地址混为一谈

位置：`frontend/src/components/status-badges.tsx:48-60`、`frontend/src/views/dashboard.tsx:150-165`、`frontend/src/views/opportunities.tsx:524-559`、`frontend/src/views/system.tsx:93-105`、`frontend/src/views/system.tsx:775`、`frontend/src/views/system.tsx:981`、`backend/internal/api/system.go:218-230`。

- 账户/下单只按live显示实盘/真实资金，未区分PF测试网。
- probe UI读probe.testnet，但成功API未返回该字段，测试网也显示主网。
- 缺省PF地址在前端填静态主网URL，stored override被称为当前生效；继承测试网时先展示错误，保存还可能将继承变主网显式覆盖。
- `backend/internal/engine/engine.go:734-737`、`backend/internal/engine/engine.go:876-904` 的PF testLive未使用候选REST地址验证，不能拿通过状态证明该地址可用。

修复统一runtime网络/effective endpoint事实模型，明确stored/inherited/draft，probe返回确定网络；测试testnet环境无URLoverride、测试网probe、未保存endpoint。

### WEB-12 / P2：解析失败被当成功，轮询缺请求代次

位置：`frontend/src/lib/api.ts:69-85`、`frontend/src/hooks/usePolling.ts:49-74`。

json/text的catch返回null，吞无效JSON、截断和读取body时AbortError；2xx被当T返回，可能清空数据或触发空值错误。轮询只看effect active，同一生命周期hide/show的旧finally仍可干扰新状态/计时器。

修复只有协议明确允许的无body才返回空，传播解析错误/AbortError，success/error/finally均核对request generation。验收headers成功但body abort/损坏、visibility交错。账户切换已有effect cleanup和remount，不误报普遍跨账户覆盖。

### WEB-13 / P2：旧列表被新筛选/页码重新标记，过期状态不完整

位置：`frontend/src/views/orders.tsx:88-147`、`frontend/src/views/orders.tsx:184-189`、`frontend/src/views/positions.tsx:74-82`、`frontend/src/views/positions.tsx:126`、`frontend/src/views/dashboard.tsx:296`。

page1成功→page2失败，旧100行仍被标为101–200；venue切换失败也会把旧结果套新标签。总量缩减导致offset越界时空分支隐藏分页器，无法直接上一页。Dashboard最近订单有旧数据后隐藏后续刷新错误。

修复数据绑定accepted query key/更新时间，各查询独立stale/error；越界纠正重取或保留返回入口。验收page2失败、pending数缩减、venue切换失败、最近订单连续失败。

### WEB-14 / P2：本地引擎开关 override 永久压过真实状态

位置：`frontend/src/views/settings.tsx:629-642`、`frontend/src/views/settings.tsx:769-791`。

engineOverride设置后从不清除，其他客户端改变运行状态，轮询新值仍被覆盖。修复更新查询缓存，或只保留到更新后一代权威查询返回。验收本地暂停→另一客户端启动→轮询，恢复显示实际状态。

### 较小交互缺陷与防御性修复

| 编号 | 级别 | 确认机制与位置 | 验收 |
| --- | --- | --- | --- |
| WEB-15 | P2 | `frontend/src/views/positions.tsx:84-106` 仅一个redeeming ID，A→B会解锁A，一项完成可清另一项，允许重复POST | pending-ID集合或账户串行，A→B→A不重复；不单凭UI推断链上重复广播 |
| WEB-16 | P3 | `frontend/src/views/settings.tsx:360`、`frontend/src/views/settings.tsx:521-538` 新live唯一性按当前账户venue而非newVenue判断 | PM已有live时仍可创建尚无live的PF，切venue协调mode |
| WEB-17 | P3 | `frontend/src/views/positions.tsx:47-52`、`frontend/src/views/positions.tsx:147-154` 移动赎回漏isLive，桌面有 | paper settled两布局都无入口，handler再guard；后端已拒绝paper，不是模拟可动真钱 |
| WEB-18 | P3 | `backend/internal/api/router.go:475-479`、`backend/internal/api/system.go:275-284` 无效赎回ID重复写两份JSON | 单层负责响应，json.Valid及完整单对象解码 |
| WEB-19 | P3 | `frontend/src/views/opportunities.tsx:284`、`frontend/src/views/opportunities.tsx:371-376` paper可点其他venue但POST不带行venue | 传venue并限制账户匹配；两个fake验证路由，不宣称已发生ID碰撞买错市场 |
| WEB-20 | P3 | `frontend/src/views/opportunities.tsx:501-516` 手动execute确认仅说买入，`backend/internal/engine/engine.go:1662-1680` 还可止损卖出 | 明示买入/止损；execute=false可对账/刷新/快照，未发现其发起新买卖 |

### 前端性能和已有保护

`frontend/src/App.tsx:5-10` 静态加载六页面，`frontend/vite.config.ts:13-15` 仅拆Recharts。结合第4节体积，先做页面lazy boundary，延迟系统/参数设置；收益需冷加载测量。useVenues每实例独立轮询可共享能力查询；缓存必须绑定正确作用域。fmtMoney已有Intl缓存，不凭感觉宣称格式化是瓶颈。opportunities GET读内存快照，不是每次轮询都扫上游；Dashboard含余额请求，不能把所有GET归类为廉价读。

现有保护应保留：account+view remount、live账户venue/ready检查、有限正金额、明确买入确认、钱包stale标记、年化估算与不足一天隐藏；口令哈希校验、随机session、七天期限、HttpOnly、SameSite=Strict、顺序改密撤销；API 1MiB body限制、分页/ID/venue/status校验、no-store；私钥/PM secret/passphrase/代理密码常规脱敏；赎回按账户查仓位并拒绝paper。

这些保护不覆盖上述竞态/错误语义。现有API覆盖率68.9%，前端无行为测试，不能以构建通过替代这些验收。

## 10. Polymarket live 与通用成交接口

PM-01费用函数与补账契约、PM-02实际签名整数、PM-03跨页遗漏及全部FAILED回退已由隔离用例确认，证据 A，详见第3.4节；不包含完整Engine落账或生产撮合验证。其余分支基于源码、固定依赖与官方资料，证据 B；纯算术例子不冒充实盘成交。固定SDK版本为 `github.com/Cyvadra/polymarket-clob-client@v0.0.0-20261028131959-03acabc9ed0b`，其文件行号以该版本为准。

### PM-01 / P1：直接卖出提前扣费，下游再扣一次

位置：`backend/internal/live/live.go:709-721`、`backend/internal/venue/venue.go:28-39`、`backend/internal/engine/engine.go:2470-2474`。

Fill.Cost契约为gross，但applySellFee设置Fee后还执行 `fill.Cost -= fee`，引擎再做Cost−Fee。100份按0.75卖出、r=0.07：gross75、fee1.3125，正确净73.6875，直接路径记72.375。交易所没有多收，本地现金/收益少记。LookupOrder返回gross，补账只扣一次，同交易因回报路径不同产生不同账本。

修复：适配器保持gross，不预扣；更新 `backend/internal/live/live_test.go:600-602` 当前固化Cost=75−fee的断言，增加真实适配器→引擎消费契约以及直接/补账结果一致性测试。单独证明engine算式正确不足以证明跨层正确。

### PM-02 / P1：SDK 独立截断现金与份额，使签名买价低于可成交盘口

位置：`backend/internal/live/live.go:538-545`、`backend/internal/live/live.go:810-829`；固定SDK `signing.go:145-160`。

计划先floor(budget/limitPrice,4位)，SDK又把现金截到美分、份额截四位，却不按新现金重算份额。预算2、ask0.98：计划2.0408份，签名makerAmount=1,990,000、takerAmount=2,040,800，实际买价1.99/2.0408=0.9751078008624069，低于0.98。Quote显示可成交不代表实际签名金额交叉。

固定提交的 [CTFExchangeV2 Trading.sol](https://github.com/Polymarket/ctf-exchange-v2/blob/ccc0596074f4dfd62c944fbca4de252893b82b4b/src/exchange/mixins/Trading.sol#L638-L656) 要求同token BUY/SELL交叉；整数比例同样失败。具体API错误码未验证。

修复：先确定可表示现金，再按精度形成一致整数maker/taker；Quote与签名共享计划，发送前整数校验交叉及用户最高价；需要修补/替换固定依赖构造逻辑，不靠显示值或float epsilon掩盖。`backend/internal/live/live_test.go:504-547` 已有同金额，但mock无条件成交，仅核对takerAmount。

### PM-03 / P1：对账把处理中、失败兜底或跨页不全成交当作最终事实

位置：`backend/internal/live/clob_l2.go:221-271`、`backend/internal/live/live.go:919-947`、`backend/internal/live/live.go:559-564`、`backend/internal/live/live.go:652-658`。

三个独立分支：

- tradesForOrder只排除短名FAILED，MATCHED/MINED/RETRYING/未知状态照计；当前OpenAPI还有TRADE_STATUS_*名称，前缀FAILED也不被过滤。处理中可能随后失败，不能等同CONFIRMED。
- LookupOrder忽略trades错误，回退size_matched×order.price；全部目标成交FAILED也可能回退正数估计。限价不是成交均价，匹配量不是已确认净持仓。
- 分页条件 `if found || ... { break }`，第一页一旦找到目标就停；同订单60份在页尾、40份在次页时只记60，还覆盖完整size_matched。没有trade ID去重、完整性或数量核对。

引擎看到正数fill即最终落账并停止后续跟踪，导致虚假/遗漏数量、错误成本永久化。FAK/FOK不等于长期挂单；这里指交易查询/链上结果仍可能变化或不全。

修复：无法证明完整且成功终态时保留pending/unknown；关联成交遍历、trade ID去重、累计数量核对，matched/confirmed/failed/complete独立。达到页数上限明确incomplete，不默认为完整。`backend/internal/live/live_test.go:279-292` 接受trades404后的限价估算；其他测试仅单页/短FAILED。

验收MATCHED→RETRYING→FAILED、MATCHED→CONFIRMED、全部FAILED但size_matched正数、60+40跨页、超时/404/页数上限/重复trade、前缀状态。两分钟order404只是本地启发式，官方资料未提供该可见性终态保证，不能凭时间自动认定绝无成交。

### PM-04 / P2：非线性费用用 VWAP 计算，偏离逐成交费用

位置：`backend/internal/live/live.go:609-610`、`backend/internal/live/live.go:699-713`、`backend/internal/live/live.go:929-941`；逐档Quote为 `backend/internal/live/live.go:1009-1032`。

成交汇总先VWAP再算 `r×shares×[p(1−p)]^e`，Quote却逐档。r0.07/e1，50份@0.2及50份@0.8：逐笔合计1.12，均价0.5计算1.75，差0.63，与舍入细节无关。修复优先实际fee，或逐成交公式/舍入；只有聚合值则标估算，不入最终实际费。验收精确双价格，而非仅Fee>0；官方舍入粒度仍需固定。

### PM-05 / P2：SDK 明确拒单误入 unknown

位置：`backend/internal/live/live.go:555-580`、`backend/internal/live/live.go:651-674`；固定SDK `orders.go:190-191`。

success=false时SDK同时返回response与OrderRejectedError；当前先处理任意submitErr并回查，404后变unknown，后面的!response.Success不可达，造成不必要阻断与查询。修复errors.As识别确定拒单类型；timeout仍unknown，不将全部4xx视为确定拒绝。验收success=false+order404直接rejected，超时保留未知。

### PM-06 / P2：计划与签名使用不同 tick，浮点上取整还可能跳一档

位置：`backend/internal/live/live.go:810-821`、`backend/internal/live/live.go:545`；固定SDK `market.go:68-88`。

计划用新盘口tick，CreateOrder用SDK永久缓存TickSize；已有InvalidateMarketMetadata未调用。缓存0.01、盘口0.001时计划0.983，签名仍按0.01验证，可持续失败直到重建。另 `math.Ceil(limitPrice/tick)*tick` 在0.07/0.01=7.000000000000001时把0.07抬0.08，可能错误拒绝最高价或放宽边界。

修复单一带版本metadata snapshot/显式失效，以整数tick或定点量化；验收BUY/SELL连续tick变化与0.07/0.14/0.28对齐值。不是只增加某一处epsilon，留下其他路径继续漂移。

### PM-07 / P2：最大 spender 授权不等于目标 exchange 授权

位置：`backend/internal/live/clob_l2.go:185-196`、`backend/internal/live/live.go:1144-1152`。

maxAllowance取map最大值，目标exchange0、旧/其他spender100也报告100；map明确零还可能被legacy scalar覆盖。预检误通过、交易失败，不代表链上绕过授权。修复保留spender map，按协议/资产/normal或neg-risk选择，区分zero/missing/error；仅map不存在时才用受限legacy兼容。`backend/internal/live/live_test.go:354-400` 当前反而断言最大值，应补目标spender场景。

### PM-08 / P2：切换配置后，迟到余额可回填新缓存

位置：`backend/internal/live/live.go:155-175`、`backend/internal/live/live.go:1077-1100`。

Dashboard发起A请求→切B并清缓存→A响应写cash/cashAt→B最多15秒读到A余额且Stale=false。配置/交易有tradeMu，不泛化报告并发换钱包下单；Dashboard未持该锁，故此路径独立可触发。普通stale回退伴随非nil error，与旧值伪新鲜不同。

修复有效配置generation，返回/写缓存前复核；验收两个本地server+barrier，不依赖随机race。与PF-06共享模式，但各适配器都需独立实现并验证。

### PM-09 / P2：跨主机重定向可转发自定义认证头

位置：`backend/internal/live/clob_l2.go:112-122`、`backend/internal/live/relayer.go:75-88`、`backend/internal/live/live.go:432-469`。

未设CheckRedirect，Go默认跨主机保护不包含POLY_API_KEY/POLY_PASSPHRASE/POLY_SIGNATURE/RELAYER_API_KEY；配置端点或代理返回跳转时会带给目标，还未阻止TLS降级。Relayer key可复用；没有通过这些头发送私钥/API secret，不扩大为私钥泄漏。

修复专用认证client禁跳转或严格同scheme/host，不修改共享client引竞态。验收两个本地server覆盖L2、Relayer、SDK内部请求和HTTPS降级政策，无需实际外传秘密。

### PM-10 / P2：钱包识别把通用代理形态当作已验证钱包

位置：`backend/internal/live/wallet.go:272-298`、`backend/internal/live/wallet.go:437-459`。

任意EIP-1167前缀判Proxy，EIP-7702/ERC-1967特征判Deposit，getOwners返回≥64字节判Safe却不解码owner/signer，最终不识别仍默认Deposit type3；RPC未查chainID。字节码形态不能证明实现合约、所有权或签名兼容性，可产生错误类型/funder和持续拒单；未证明可直接转走第三方资金。

修复candidate与verified分开；已知factory精确派生，其他地址核对implementation/owner/chain，无证据返回unverified。验收错误chain、无关minimal proxy、Safe不含signer/空owners、RPC失败；`backend/internal/live/wallet_test.go:177-202` 接受仅通用字节码的profile地址，不能证明归属。

### 通用精度与能力边界

| 编号 | 级别 | 位置与问题 | 最小改进 |
| --- | --- | --- | --- |
| VEN-01 | P3 | `backend/internal/venue/money.go:26-53` float64 Scale/Unscale不保任意18位金额；999999999999999999→1.0→1000000000000000000 | 签名/全余额保留raw整数或十进制字符串，float只做展示和近似策略；不把1wei例子放大为已发生资金事故 |
| VEN-02 | P3 | `backend/internal/venue/money.go:75-85` tick小数位函数对0.0001返回3、0.025返回2 | 从精确tick推导或拒绝不支持集合，补单测，实际调用支持范围另核实 |
| VEN-03 | P3 | `backend/internal/venue/venue.go:189`、`backend/internal/venue/venue.go:225-240` Capabilities.OrderTypes浅拷贝 | 复制slice；未见当前修改者，不建复杂不可变框架 |

`backend/internal/venue/venue.go:131-133` callback说明应写“签名后、发送前”，与现有保护一致；Fill/LookupOrder需表达完整性、终态、费用资产而非只扩充status字符串。`backend/internal/venue/caps.go:29` 明确SupportsCancel=false，不能把FAK/FOK恢复描述成通用挂单撤单系统。

钱包发现对候选重复串行getCode，见 `backend/internal/live/wallet.go:396-427`、`backend/internal/live/wallet.go:448-482`；每RPC12秒而流程无整体预算。先单次调用内缓存、整体deadline、有界并发，未测生产延迟，不先引长期全局缓存。

### PM 已有保护与外部事实边界

- 签名后发送前持久化callback，失败阻止发送；SDK不盲重试订单POST，保留未知签名ID。
- 配置验证私钥格式/L2三元组/EOA与funder约束；公共Quote剥离私钥和凭据。
- 配置变更清客户端/凭据/余额，凭据派生写回有一致性检查，缺口在余额等其他工作流。
- 费率请求失败/异常不默认为零，费率上界与响应大小/请求超时保护存在。
- 钱包发现本身不下单/授权/赎回，但Probe可派生/创建CLOB API credentials，不应写成纯本地检查。
- CTFExchangeV2 domain2不等于ProtocolV2/ExchangeV3 domain3。live参数无独立协议身份，DAT-04的缺version输入只能由更完整的能力门槛阻止。
- 当前COLLATERAL选择pUSD；USDC历史枚举不能替代抵押品地址/精度/协议身份。maker_address服务端精确谓词、状态字符串映射、费用舍入粒度、FOK SELL特殊最小量仍需固定版本夹具；未把CTF BUY扣股的旧推断升级为确认结论。
