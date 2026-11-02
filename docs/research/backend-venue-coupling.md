# Backend venue-coupling audit — Polymarket → +predict.fun

Read-only audit of the Go/Gin + SQLite backend at commit `dae30a3` ("✨ feat(扫描)：手动扫描支持按次选择自动下单"). No code was modified. Line numbers are valid for that commit and were verified with `grep -n` / `awk NR` against the working tree.

Scope: `backend/cmd/server`, `backend/internal/{config,api,engine,market,live,store,netproxy}`. Frontend was only inspected where the Go types leak into it (noted in §5).

Legend for the "why it blocks" column: **HARD** = cannot work at all for a second venue without changing this; **STRUCTURAL** = works for one venue only, needs a registry/namespace; **COSMETIC** = message/label, no behavioral block.

---

## 0. Executive summary — the eight things that actually stop a second venue

1. **Single-valued wiring.** `Engine.provider market.Provider` and `Engine.live LiveExecutor` are scalars (`engine.go:19-20`), injected as single arguments (`engine.go:40`, `main.go:57-70`). There is no venue key anywhere in the call path.
2. **The executor interface is Polymarket's fee-schedule API.** `LiveExecutor.Buy/Sell/LookupOrder/EstimateBuyFee/Quote` all take `conditionID` because that is the key for `client.FeeSchedule(ctx, conditionID)` (`types.go:769-788`, `live.go:520/621/728/762/888`).
3. **Identity is a bare string.** Scans build `map[string]market.Market` keyed by Gamma `market.ID` (`engine.go:595-597`), reconciliation/refresh look up by `order.MarketID` / `position.MarketID` (`engine.go:751/1467`), and auto-trade de-dup uses `orderedMarkets[opportunity.MarketID]` (`engine.go:852-861`, `engine.go:928-937`). Two venues can collide in every one of these maps.
4. **Money is `float64` and collateral is hard-coded 6-decimal.** `collateralUnits` divides by `1_000_000` (`clob_l2.go:177`) and `collateralFromSDK` does the same (`live.go:1126`). PF USDT is 18-decimal. No `big.Rat`/`decimal` exists in the backend (only `big.Int` for CREATE2 in `wallet.go:140-141`).
5. **No venue/currency in the schema.** `positions`/`orders`/`equity_snapshots` have `account_id` but no venue and no currency (`sqlite.go:79-127`); every query filters only on `account_id`.
6. **Settings are one venue.** `SystemSettings{Proxy, DataSource, Live}` (`types.go:217-222`), `DataSourceSettings{GammaBaseURL, CLOBBaseURL}` (`types.go:226-229`), `LiveSettings{PrivateKey, Funder, SignatureType, APIKey, APISecret, APIPassphrase}` (`types.go:273-280`). The probe URL is a compile-time constant `https://clob.polymarket.com/time` (`system.go:20`).
7. **Settlement is derived from Gamma outcome prices.** `Market.Winner()` / `Market.SettlementPrice()` require `Closed`, exactly two outcomes, and `outcome.Price` at 0.999/0.001 or 0.499–0.501 split (`market.go:71-123`). PF settles through its own resolver and exposes no such price field.
8. **One live account per process.** `CreateAccount` rejects a second live account (`engine.go:473-476`), and live trading always routes to the same `e.live`.

Secondary but real: the whole scan is serialized under one `tradeMu` with a 90 s deadline (`engine.go:577`, `engine.go:108`), so "add a venue" also means "add per-venue isolation" or one slow upstream breaks both.

---

## 1. Coupling inventory (a): file:line | current code | why it blocks a second venue | suggested minimal change

### 1.1 Process wiring and configuration

| file:line | current code | why it blocks a second venue | suggested minimal change |
|---|---|---|---|
| `backend/cmd/server/main.go:57` | `provider := market.NewGammaClient(cfg.GammaBaseURL, proxyManager)` | One provider instance, one Gamma base URL; no way to select by venue. **HARD** | Build `map[venue.ID]market.Provider` and pass it to `engine.New`; keep the Gamma client under `venue.Polymarket`. |
| `backend/cmd/server/main.go:58-69` | `liveExecutor := live.New(live.Config{PrivateKey: cfg.PolymarketPrivateKey, …})` | One executor, one credential set, one chain. **HARD** | Construct a PF executor from a separate `live.Config`/`PredictFunConfig`; put both in an executor registry. |
| `backend/cmd/server/main.go:70` | `strategyEngine, err := engine.New(ctx, repository, provider, liveExecutor, proxyManager)` | The constructor signature encodes "one provider + one live executor". **HARD** | `engine.New(ctx, repo, venues *venue.Registry, proxy)`. |
| `backend/internal/config/config.go:24-31` | `PolymarketPrivateKey`, `PolymarketFunder`, `PolymarketSignatureType`, `PolymarketCLOBURL`, `PolygonRPCURL`, `PolymarketAPIKey/Secret/Passphrase` | The entire env surface is Polymarket-named; no `PREDICTFUN_*`, no `BSC_RPC_URL`, no chain id. **HARD** | Add `PredictFunAPIKey`, `PredictFunPrivateKey`, `PredictFunBaseURL`, `PredictFunTestnet`, `BSCRPCURL`; keep existing fields untouched. |
| `backend/internal/config/config.go:38` | `GammaBaseURL: env("GAMMA_BASE_URL", "https://gamma-api.polymarket.com")` | The only market-data URL in config is Gamma. | Add `PREDICTFUN_BASE_URL` (default testnet) and pass it to the PF provider. |
| `backend/internal/config/config.go:44` | `PolymarketCLOBURL: env("POLYMARKET_CLOB_URL", "https://clob.polymarket.com")` | Trading URL is a single global. | Same, per-venue settings replace this for PF. |
| `backend/internal/config/config.go:47` | `PolygonRPCURL: envAny([]string{"POLYGON_RPC_URL", "POLYMARKET_RPC_URL"}, "https://polygon-bor-rpc.publicnode.com")` | Chain/RPC is hard-wired Polygon; PF needs BSC (chainId 56). | Add `BSC_RPC_URL`; make RPC a per-venue capability. |
| `backend/internal/config/config.go:34-52` | `func FromEnv() Config { … }` | Flat struct, no `map[venue]…`. | Keep flat struct as the Polymarket section; add a second struct. |

### 1.2 Engine struct, lifecycle, and venue selection

| file:line | current code | why it blocks a second venue | suggested minimal change |
|---|---|---|---|
| `backend/internal/engine/engine.go:19` | `provider market.Provider` | Single public-data source; every `e.provider` call is venue-less. **HARD** | Replace with `venues *venue.Registry` (providers keyed by `venue.ID`). |
| `backend/internal/engine/engine.go:20` | `live     LiveExecutor` | Single money executor; every `e.live` call is venue-less. **HARD** | Same registry; `Registry.executor(instrument)`. |
| `backend/internal/engine/engine.go:40` | `func New(ctx context.Context, repo Repository, provider market.Provider, live LiveExecutor, proxy *netproxy.Manager) (*Engine, error)` | Constructor cannot express N venues. **HARD** | Take a registry; keep a single-venue convenience wrapper so existing tests keep compiling. |
| `backend/internal/engine/engine.go:108-109` | `scanCtx, cancel := context.WithTimeout(ctx, 90*time.Second)` / `_ = e.Scan(scanCtx)` | One deadline for all venues: a hung PF upstream kills the PM scan. **STRUCTURAL** | Per-venue child context + `errgroup`/`WaitGroup`; keep one lock acquisition. |
| `backend/internal/engine/engine.go:112-113` | `settings := e.Settings()` / `timer := time.NewTimer(time.Duration(settings.ScanIntervalSeconds) * time.Second)` | One scan cadence/one settings blob for all venues. | Keep global cadence for v1; move per-venue knobs into `VenueSettings` later. |
| `backend/internal/engine/engine.go:193-195` | `if reloader, ok := e.live.(interface{ ReloadProxy() }); ok && reloader != nil { reloader.ReloadProxy() }` | Type-asserts the single executor after a proxy change. | Loop over executors in the registry. |
| `backend/internal/engine/engine.go:208` | `probe, ok := e.live.(liveProbeRuntime)` | Credential probe is bound to one executor type. | `engine.ProbeLive(venue, …)` → registry lookup. |
| `backend/internal/engine/engine.go:222` | `probe, ok := e.live.(relayerProbeRuntime)` | Same for relayer probe (PM-only concept). | Keep PM-only; dispatch by venue and return "unsupported" for PF. |
| `backend/internal/engine/engine.go:239` | `discovery, ok := e.live.(walletDiscovery)` | Same for wallet discovery (PM CREATE2/proxy/funder model). | Same. |
| `backend/internal/engine/engine.go:255-263` | `provider, hasProvider := e.provider.(dataSourceRuntime)` … `provider.SetBaseURL(settings.DataSource.GammaBaseURL)` | Runtime endpoint override assumes exactly one provider whose base URL is "Gamma". **STRUCTURAL** | Per-venue `SetBaseURL` loop; PM keeps the same call order for rollback. |
| `backend/internal/engine/engine.go:264-274` | `updater, hasLive := e.live.(liveSettingsRuntime)` … `updater.UpdateSettings(settings.Live, settings.DataSource.CLOBBaseURL)` | Runtime credential override assumes exactly one executor and one CLOB URL. **STRUCTURAL** | Per-venue settings update with the same "apply before save, rollback on failure" semantics. |
| `backend/internal/engine/engine.go:282-290` | `runtime.GammaBaseURL = provider.BaseURL()` / `runtime.CLOBBaseURL = live.EffectiveCLOBURL()` | `SystemRuntime` DTO has only two URL slots. **STRUCTURAL** | Add `venues: []VenueRuntime` while keeping the two legacy fields populated from PM. |
| `backend/internal/engine/engine.go:327` | `return OpportunityList{… Source: e.provider.Name()}` | The radar's `source` is a single string (`"Polymarket Gamma"`). | Add `venue` per item; keep `source` as an aggregate label. |
| `backend/internal/engine/engine.go:419-445` | `decorateAccount` calls `e.live.Configured/Signer/Maker/ReadyError` for every live account | Every live account is assumed to be on the same venue/executor. **STRUCTURAL** | Add `Account.Venue` and look up the executor for it. |
| `backend/internal/engine/engine.go:447-454` | `LiveStatus()` asserts `e.live != nil` then `e.live.Configured()` | Reports a single live status. | `LiveStatus(venue)` or a map; `/health` can keep PM. |
| `backend/internal/engine/engine.go:473-476` | `if mode == AccountModeLive && existing.Mode == AccountModeLive { return Account{}, fmt.Errorf("%w: 每个实例只允许一个实盘账户", ErrAccountConflict) }` | Prevents a second live account, hence a second venue's live account. **HARD** | Reject only when `(venue, mode)` already exists. |
| `backend/internal/engine/engine.go:510-515` | `if *autoTrade && account.Mode == AccountModeLive { if e.live == nil \|\| !e.live.Configured() { … } }` | Enabling auto-trade checks the single executor. | Check `executorFor(account.Venue)`. |
| `backend/internal/engine/engine.go:535-551` | `DeleteAccount`: `if account.Mode == AccountModeLive { return … "实盘账户不能删除" }` | Live-account singleton policy leaks into delete. | Decide per venue; no code change needed if per-venue live accounts are allowed, but the guard stays as-is for PM. |
| `backend/internal/engine/engine.go:1685` | `source := SourceStatus{Name: e.provider.Name(), LastSyncAt: e.lastSyncAt}` | Dashboard reports one source. | Aggregate or add `sources []SourceStatus`. |

### 1.3 Scan → opportunity discovery

| file:line | current code | why it blocks a second venue | suggested minimal change |
|---|---|---|---|
| `backend/internal/engine/engine.go:587` | `markets, err := e.provider.ListOpen(ctx, settings.MaxDays)` | One upstream call supplies the whole scan; any error aborts everything (`engine.go:590-592`). **HARD** | Loop over enabled venues, each with its own error bucket and health record. |
| `backend/internal/engine/engine.go:595-597` | `byID := make(map[string]market.Market, len(markets))` … `byID[item.ID] = item` | Map key is the raw market id with no venue prefix. **HARD** | Key by `venue.ID + "\x00" + marketID` (or a struct key) and thread the venue through. |
| `backend/internal/engine/engine.go:599` | `opportunity, candidateErr := findOpportunity(item, outcome.Name, outcome.TokenID, settings, now)` | Passes only the market object; no venue, no per-venue settings. | `findOpportunity(venue, item, outcome, venueSettings, now)`. |
| `backend/internal/engine/engine.go:608` | `opportunities = e.classifyOpportunities(ctx, opportunities, byID, settings)` | Classification is tag-based (PM tags) but also calls `e.provider` for the tag fallback. **STRUCTURAL** | Pass the venue's provider/tags into classification. |
| `backend/internal/engine/engine.go:616-621` | `// Polymarket exposes Chinese question and outcome labels through the Gamma locale parameter.` … `if localizedProvider, ok := e.provider.(market.LocalizedProvider); ok` | Locale enrichment is a Gamma-only optional interface; PF has no such parameter. **STRUCTURAL** | Gate on `capabilities.Localized`; PF provider simply does not implement it (already optional), but the provider lookup must be per venue. |
| `backend/internal/engine/engine.go:624-627` | `if !seen[opportunity.MarketID] { seen[opportunity.MarketID] = true; ids = append(ids, opportunity.MarketID) }` | De-dup key is market id only. | `venue + marketID`. |
| `backend/internal/engine/engine.go:636` | `localizedItem, ok := byID[opportunities[i].MarketID]` | Localized result map keyed by market id only. | Same. |
| `backend/internal/engine/engine.go:656-658` | `e.opportunities = opportunities` / `e.lastSyncAt = timePointer(now)` | One flat radar slice; no venue per row to filter on. | Add `Opportunity.Venue`; keep the slice flat. |
| `backend/internal/engine/engine.go:577` | `e.tradeMu.Lock()` at the top of `scan` | Serializes scanning and all order placement for every account and (future) every venue. **STRUCTURAL** | Keep the lock for order placement, release it for the read-only scan phase, or use a per-venue lock. |
| `backend/internal/engine/engine.go:675-688` | `if account.Mode == AccountModeLive { remaining, err := e.reconcileLiveOrders(ctx, account, byID) }` then `e.refreshPositions(ctx, account.ID, byID)` | Per-account processing assumes the account's venue is the only venue. | Derive venue from `account.Venue` (and later from each position). |
| `backend/internal/engine/engine.go:301` (`LastError`) / `:738-742` | `e.lastError = message` | A single error string for the whole engine. | Per-venue health map, keep `lastError` aggregate. |

### 1.4 Opportunity shape and hard-coded market assumptions

| file:line | current code | why it blocks a second venue | suggested minimal change |
|---|---|---|---|
| `backend/internal/engine/types.go:527-555` | `type Opportunity struct { MarketID string; ConditionID string; … TokenID string; Price float64; Liquidity float64; EndDate time.Time; … Version string }` | No `Venue`, no `Currency`, no `OrderTypes`/`Unsupported`. `ConditionID` and `TokenID` are PM names. **HARD** | Add `Venue`, `Currency`, `Unsupported`; keep existing JSON fields byte-identical for PM. |
| `backend/internal/engine/types.go:842-851` | `opportunityFor`: `URL: m.URL(), … Version: m.Version` | Radar links are built by the market type; PF has no polymarket.com URL. | Provider fills `Market.URL`; `opportunityFor` copies it. |
| `backend/internal/engine/types.go:854-856` | `if !m.Active \|\| m.Closed \|\| m.EndDate.IsZero() \|\| !m.EndDate.After(now) \|\| m.EndDate.After(now.AddDate(0, 0, settings.MaxDays))` | `Active`/`Closed`/`EndDate` are Gamma semantics. PF has `tradingStatus`/`status` and **no per-market end date** (see §5). **HARD** | Move these gates into a per-venue `Market.TradeReady()`/`Market.EndDate` contract; PF provider must synthesize/fail-closed on missing `endsAt`. |
| `backend/internal/engine/types.go:858-860` | `if !m.TradeReady() { … "市场已暂停接受订单（acceptingOrders=false）" }` | `TradeReady()` reads Gamma `acceptingOrders` (`market.go:54-56`). | Keep the method; PF provider maps `tradingStatus` into `AcceptingOrders` or a venue-neutral field. |
| `backend/internal/engine/types.go:861-863` | `if len(m.Outcomes) != 2 \|\| m.Liquidity < settings.MinLiquidity` | Binary-only and dollar-liquidity assumptions shared by all venues; PF v1 is binary-only anyway, but its liquidity unit must be declared. | Keep binary gate; make `MinLiquidity` per venue/currency. |
| `backend/internal/engine/types.go:864-867` | `if settings.MaxPriceChange24h > 0 && m.PriceChange24h != nil && math.Abs(*m.PriceChange24h) > settings.MaxPriceChange24h+1e-9` | `PriceChange24h` comes from Gamma `oneDayPriceChange` (`gamma.go:117`, `gamma.go:386`); PF may not supply it (nil disables the guard — a silent behavior difference). | Add `Capabilities.Has24hChange`; PF provider leaves it nil and the UI shows "not available". |
| `backend/internal/engine/types.go:875-877` | `if outcome.Price < settings.MinPrice \|\| outcome.Price > settings.MaxPrice` | Price window itself is venue-neutral (0–1), but the settings are single-valued. | Per-venue `Settings`. |
| `backend/internal/engine/types.go:557-560` | `func (o Opportunity) IsV2() bool { return strings.EqualFold(strings.TrimSpace(o.Version), "v2") }` | "v2" is a Polymarket protocol concept. On a PF opportunity `Version` would always be empty and the check silently passes. | Replace with `capabilities.SupportsInstrument(opportunity)` (PM `v2` → false). |
| `backend/internal/market/market.go:17-42` | `type Market struct { ID; ConditionID; … Version string; AcceptingOrders *bool }` | PM-shaped fields (`ConditionID`, `Version`, `AcceptingOrders`, `ResolutionStatus`) but no venue/currency/URL. **HARD** | Add `Venue`, `URL`, `OutcomeCount`, `Unsupported`; keep existing fields. |
| `backend/internal/market/market.go:58-63` | `func (m Market) URL() string { if m.Slug == "" { return "" }; return "https://polymarket.com/event/" + m.Slug }` | Hard-coded polymarket.com URL is copied into `Opportunity.URL` and rendered by the radar. **HARD** | Provider-populated `Market.URL`; delete the URL builder. |
| `backend/internal/market/market.go:44-48` | `func (m Market) IsV2() bool { return strings.EqualFold(strings.TrimSpace(m.Version), "v2") }` | PM-only protocol flag, consumed by `Opportunity.IsV2` and two engine refusal paths. | Keep for PM; move the refusal decision to a capability check. |
| `backend/internal/market/market.go:54-56` | `func (m Market) TradeReady() bool { return m.AcceptingOrders == nil \|\| *m.AcceptingOrders }` | Encodes Gamma's `acceptingOrders` semantics; a PF market that is paused would look trade-ready unless the provider maps the field. | Provider maps `tradingStatus` → `AcceptingOrders` (or a new neutral field). |
| `backend/internal/market/market.go:71-96` | `func (m Market) Winner() (Outcome, bool)` — requires `finalResolutionData()`, and `outcome.Price >= 0.999` / `> 0.001` | Settlement is inferred from **Gamma's last traded price**, not from an on-chain resolution. PF has `resolverAddress`/`oracleQuestionId` and no price-based settlement. **HARD** | Introduce a `Settler`/`Resolution` provider interface; PM implementation keeps exactly this logic. |
| `backend/internal/market/market.go:98-117` | `SettlementPrice`: 50/50 detection via `m.Outcomes[0].Price >= 0.499 && … <= 0.501` | Same price-inference assumption; also indexes `Outcomes[0]`/`[1]` directly (binary-only). | Same; PF must return `(0,false)` until a real resolver adapter exists. |
| `backend/internal/market/market.go:119-123` | `func (m Market) finalResolutionData() bool { if !m.Closed \|\| len(m.Outcomes) != 2 { return false }; status := strings.ToLower(strings.TrimSpace(m.ResolutionStatus)); return status == "" \|\| status == "resolved" }` | Accepts empty status as resolved; PF status vocabulary differs (`tradingStatus`/`status`). | Move into the PM settler. |
| `backend/internal/market/market.go:127-131` | `type Provider interface { ListOpen(ctx, maxDays) ([]Market, error); Get(ctx, id) (Market, error); Name() string }` | No `Venue()`; also no way to express currency/capabilities. **HARD** | Add `Venue() venue.ID` (and optionally `Capabilities()`). |
| `backend/internal/market/gamma.go:92` | `func (g *GammaClient) Name() string { return "Polymarket Gamma" }` | Name doubles as the UI's "source" label and as the only identity of the provider. | Add `Venue()` and keep `Name()` unchanged. |
| `backend/internal/market/gamma.go:138-180` | `ListOpen` reads three hard-coded views: `{order: "endDate", ascending: "true"}`, `{order: "volume24hr"}`, `{order: "liquidity"}`, 2 pages each | Pagination/order strategy is Gamma-specific; a PF provider with cursor + category join cannot reuse this. | Contract stays `ListOpen(maxDays)`; implementation is per venue. |
| `backend/internal/market/gamma.go:187-221` | `listMarkets` builds `closed`, `limit`, `order`, `ascending`, `include_tag`, `end_date_min`, `end_date_max`, `after_cursor` | Server-side `end_date_min/max` filtering is a Gamma feature; PF end dates live on the category and need a client-side join + cache. | Keep in Gamma client. |
| `backend/internal/market/gamma.go:227-260` | `MarketsLocale` uses the Gamma `locale` query parameter | PM-only localization. | Already an optional interface; only call it for the PM provider. |
| `backend/internal/market/gamma.go:275-306` | `EventTags` uses `/events?...` and Gamma tag objects | PM-only category source. | Optional interface already; keep for PM. |
| `backend/internal/market/gamma.go:399-417` | `gammaOutcomeIDs`: `case "v2": return decodeOutcomeIDs(raw.PositionIDs, "positionIds")` | PM protocol version decides which id is the tradeable token; no venue-neutral "outcome id" contract. | Keep inside Gamma parser; document `Outcome.TokenID` as "venue outcome id". |
| `backend/internal/market/category.go:57-128` | `var categoryBySlug = map[string]Category{"sports": …, "politics": …, "crypto": …}` | PM tag taxonomy. PF has its own `categorySlug` vocabulary; unknown slugs fall through to `CategoryOther` (silent). | Keep the PM map; let a provider supply a category mapping, or accept `other` for PF v1. |
| `backend/internal/market/category.go:141-177` | `func Classify(m Market, tags []Tag) Classification` | Uses `GameStartTime`, question/slug regexes — generic enough, but tags come from PM. | No change needed for v1. |
| `backend/internal/engine/category.go:15-67` | `classifyOpportunities`: `if provider, ok := e.provider.(market.EventTagProvider); ok { … provider.EventTags(ctx, eventIDs) }` | Tag fallback uses the single provider. | Pass the venue provider. |

### 1.5 Live executor interface and Polymarket execution semantics

| file:line | current code | why it blocks a second venue | suggested minimal change |
|---|---|---|---|
| `backend/internal/engine/types.go:769-788` | `type LiveExecutor interface { Configured; Signer; Maker; ReadyError; Buy(ctx, tokenID, conditionID string, amount, maxPrice float64, onSigned …); Sell(ctx, tokenID, conditionID, shares, onSigned); LookupOrder(ctx, orderID, conditionID); EstimateBuyFee(ctx, conditionID, amount, price); Quote(ctx, tokenID, conditionID, amount); Collateral(ctx) }` | Five of the ten methods take Polymarket identifiers (`Buy`, `Sell`, `LookupOrder`, `EstimateBuyFee`, `Quote`), and `Collateral` returns pUSD by convention. **HARD** | Introduce `venue.Instrument` and `VenueExecutor`; keep `LiveExecutor` as an alias/PM adapter so tests compile. |
| `backend/internal/engine/types.go:774-777` | `// … conditionID is required to load the market's fee schedule.` / `Buy(ctx context.Context, tokenID, conditionID string, amount, maxPrice float64, onSigned func(orderID string) error) (LiveFill, error)` | Fee schedule lookup is the reason `conditionID` is in the interface; PF's fee is a per-market `feeRateBps`. **HARD** | `Buy(ctx, in venue.Instrument, req BuyRequest, onSigned …)`; PM adapter reads `in.ConditionID` exactly as today. |
| `backend/internal/engine/types.go:781` | `Sell(ctx context.Context, tokenID, conditionID string, shares float64, onSigned func(orderID string) error) (LiveFill, error)` | Same; PF "sell" is a LIMIT/MARKET order on the opposite side. | Same. |
| `backend/internal/engine/types.go:782` | `LookupOrder(ctx context.Context, orderID, conditionID string) (LiveFill, bool, error)` | Reconciliation needs `conditionID` to filter `/data/trades?market=` (`clob_l2.go:227-228`). | Same. |
| `backend/internal/engine/types.go:783` | `EstimateBuyFee(ctx context.Context, conditionID string, amount, price float64) (float64, error)` | Fee is looked up by PM condition id. | `EstimateBuyFee(ctx, in venue.Instrument, amount, price)`. |
| `backend/internal/engine/types.go:786` | `Quote(ctx context.Context, tokenID, conditionID string, amount float64) (LiveQuote, error)` | Quote mixes PM order-book + fee schedule. | `Quote(ctx, in venue.Instrument, amount)`. |
| `backend/internal/engine/types.go:787` | `Collateral(ctx context.Context) (CashSnapshot, error)` | One collateral currency implied. | Per-venue executor returns its own currency; `CashSnapshot` gains `Currency`. |
| `backend/internal/engine/types.go:760-765` | `type CashSnapshot struct { Amount float64; Allowance float64; At time.Time; Stale bool }` | No currency; `Amount` is pUSD by convention (comment at `types.go:757-759`). | Add `Currency venue.QuoteCurrency` (keep zero value = USDC/pUSD for PM). |
| `backend/internal/engine/types.go:748-756` | `// For a buy, Cost is the gross notional plus the taker fee. For a sell, Cost is the gross proceeds minus the taker fee.` | Fee-on-top vs fee-deducted is a PM convention; PF may charge in a different unit/side. | Keep `LiveFill` but document it as "venue-adjusted net"; PF executor normalizes. |
| `backend/internal/engine/types.go:794-804` | `type LiveQuote struct { BestAsk; LimitPrice; FillPrice; Shares; Notional; Fee; FeeRate; AvailableNotional; FullyFilled }` | `LimitPrice`/`FullyFilled` are FAK-walk concepts; PF MARKET uses `slippageBps`/`isMinAmountOut`. | Add `OrderType` / `SlippageBps` fields, or normalize inside the PF executor. |
| `backend/internal/live/live.go:512-533` | `schedule, err := client.FeeSchedule(ctx, conditionID)` … `order, err := client.MarketBuyOrder(ctx, tokenID, amount, clobclient.OrderTypeFAK)` … `if maxPrice > 0 && order.Price > maxPrice+1e-9` | Hard-coded PM SDK + FAK + tick/limit semantics. **HARD** | New PM adapter keeps these exact calls; PF executor implements its own EIP-712 order build. |
| `backend/internal/live/live.go:613-629` | `schedule, err := client.FeeSchedule(ctx, conditionID)` … `client.MarketSellOrder(ctx, tokenID, shares, clobclient.OrderTypeFOK)` | FOK choice is a PM stop-loss design decision; PF only has LIMIT/MARKET. | Keep PM adapter; PF stop-loss must be an explicit MARKET with `slippageBps`. |
| `backend/internal/live/live.go:717-732` | `client.FeeSchedule(ctx, conditionID)` … `return feeFromSchedule(schedule, amount/price, price)` | Fee model = PM schedule. | Per-venue fee strategy. |
| `backend/internal/live/live.go:742-767` | `client.OrderBook(ctx, tokenID)` … `schedule, err := client.FeeSchedule(ctx, conditionID)` | Order book + fee schedule both PM CLOB. | Per-venue quote. |
| `backend/internal/live/live.go:853-899` | `LookupOrder` → `client.Order`, `e.tradesForOrder(ctx, orderID, conditionID)`, `client.FeeSchedule(ctx, conditionID)` | Reconciliation is PM CLOB-specific. | `LookupOrder(ctx, externalID, in)`; PM adapter unchanged. |
| `backend/internal/live/live.go:903-911` | `const maxFeeRate = 0.10` with comment `// The schedule must come from /clob-markets/{condition_id}` | PM fee cap + endpoint assumption. | Move into PM fee model. |
| `backend/internal/live/live.go:930-960` | `schedule.Fee(formatFloat(shares), formatFloat(price), true)` | Fee math delegated to the PM SDK's `FeeSchedule`. PF is `feeRateBps/10000 × min(p,1−p) × shares`. | `FeeModel` interface: PM keeps `schedule.Fee`; PF implements the formula. |
| `backend/internal/live/live.go:962-986` | `feeFromLevels(schedule, levels)` | Per-level PM fee. | PM-only. |
| `backend/internal/live/live.go:996-1010` | `applyFee` adds fee to `Cost`, comment references `docs.polymarket.com/trading/fees` | Buy-side fee convention. | Keep in PM adapter; PF normalizes its own fill. |
| `backend/internal/live/live.go:425-438` | `sdkCfg := clobclient.Config{PrivateKey: …, MakerAddress: makerAddress, SignatureType: clobclient.SignatureType(cfg.SignatureType), Host: cfg.CLOBURL, Timeout: 20*time.Second, QPS: 10}` | PM SDK config; `SignatureType`/`MakerAddress` are PM proxy-wallet concepts. | Keep PM; PF has its own client. |
| `backend/internal/live/live.go:1065-1106` | `collateralBalance`: `query := url.Values{"asset_type": {"COLLATERAL"}, "signature_type": {strconv.Itoa(e.signatureType())}}` then `e.l2Get(ctx, "/balance-allowance", …)` | PM CLOB account endpoint; PF has no L2 HMAC and reads USDT balance on-chain. | Per-venue `Collateral` implementation. |
| `backend/internal/live/live.go:1110-1126` | `return units / 1_000_000, allowanceUnits / 1_000_000, nil` | Hard 6-decimal assumption. **HARD for 18-decimal USDT.** | `Capabilities.QuoteDecimals`-driven conversion; `big.Int` base units at the boundary. |
| `backend/internal/live/clob_l2.go:38` | `const defaultCLOBURL = "https://clob.polymarket.com"` | Default host baked into the L2 helper. | Keep in PM package. |
| `backend/internal/live/clob_l2.go:90-94` | `privateKey, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(cfg.PrivateKey), "0x"))` then `base := strings.TrimRight(strings.TrimSpace(cfg.CLOBURL), "/")` | L2 auth = PM private key + CLOB URL. | PM-only. |
| `backend/internal/live/clob_l2.go:112-116` | `request.Header.Set("POLY_ADDRESS", …)`, `POLY_SIGNATURE`, `POLY_TIMESTAMP`, `POLY_API_KEY`, `POLY_PASSPHRASE` | PM L2 HMAC header set. PF uses `x-api-key` + JWT. | PM-only. |
| `backend/internal/live/clob_l2.go:165-177` | `// balances and allowances in 6-decimal base units.` … `return units / 1_000_000, nil` | **HARD**: 6-decimal hardcode. | Parameterize decimals; use `big.Rat`/`big.Int`. |
| `backend/internal/live/clob_l2.go:221-228` | `func (e *Executor) tradesForOrder(ctx, orderID, conditionID string)` … `query.Set("market", conditionID)` | Reconciliation filtered by PM condition id. | Per-venue `LookupOrder`. |
| `backend/internal/live/wallet.go:44-59` | `safeFactoryAddress`, `proxyFactoryAddress`, `depositFactoryAddress`, `depositImplementation`, `safeInitCodeHash`, `proxyInitCodeHash`, `profilePath = "/public-profile"`, `defaultPolygonRPC = "https://polygon-bor-rpc.publicnode.com"` | PM wallet model + Polygon chain constants. **HARD** (unusable on BSC). | Move into a PM wallet package; PF gets its own wallet/EOA model. |
| `backend/internal/live/wallet.go:99-128` | `DeriveSafeAddress`, `DeriveProxyAddress`, `DeriveDepositAddress` (CREATE2 with PM factory salts) | PM-specific CREATE2 derivation. | PM-only; PF has EOA or Kernel smart account. |
| `backend/internal/live/wallet.go:184-191` | `func newRPCClient(cfg Config) *rpcClient { endpoint := effectiveRPC(cfg.PolygonRPC) … }` | One RPC endpoint, implicitly Polygon. | Per-venue RPC config. |
| `backend/internal/live/relayer.go:21-23` | `const defaultRelayerURL = "https://relayer-v2.polymarket.com"` / `const relayerKeysPath = "/relayer/api/keys"` | PM gasless relayer; PF has no equivalent in the current design. | PM-only; keep the probe behind a PM capability. |
| `backend/internal/live/relayer.go:41-129` | `ProbeRelayer` reads/sets `RELAYER_API_KEY` / `RELAYER_API_KEY_ADDRESS` | PM credential shape. | PM-only. |

### 1.6 Engine execution paths (buy / sell / stop-loss / reconcile)

| file:line | current code | why it blocks a second venue | suggested minimal change |
|---|---|---|---|
| `backend/internal/engine/engine.go:813-818` | `func (e *Engine) autoTrade(…)` → `if account.Mode == AccountModeLive { return e.autoLiveTrade(…) }; return e.autoPaperTrade(…)` | Dispatch is by account mode only; the venue is never consulted. **HARD** | `executor := e.venues.Executor(account.Venue)`; same split for paper. |
| `backend/internal/engine/engine.go:852-861` | `orderedMarkets := make(map[string]bool, len(positions))` … `orderedMarkets[position.MarketID] = true` | De-dup key is market id only → PM and PF ids collide, blocking a legit second-venue entry (or, worse, silently skipping it). **HARD** | Key by `(venue, marketID)`; store venue on the position. |
| `backend/internal/engine/engine.go:884-886` | `fee := settings.OrderSize * settings.FeeBPS / 10000` / `fillPrice := opportunity.Price * (1 + settings.SlippageBPS/10000)` | Paper simulation uses a single PM-calibrated `FeeBPS`/`SlippageBPS` (`types.go:136-142`). | Per-venue paper fee/slippage. |
| `backend/internal/engine/engine.go:919-921` | `return outcome, fmt.Errorf("%w: 实盘自动交易已开启，但未配置 POLYMARKET_PRIVATE_KEY", ErrLiveUnavailable)` | Error text is PM-specific (cosmetic) but also asserts the single executor. | Message keyed by venue. |
| `backend/internal/engine/engine.go:947-949` | `snapshot, err := e.live.Collateral(ctx)` … `"读取实盘 pUSD 余额失败"` | Cash is pUSD; PF needs USDT. | `executorFor(opportunity.Venue).Collateral`. |
| `backend/internal/engine/engine.go:965-967` | `if opportunity.ConditionID == "" { continue }` | Auto-trade silently requires a PM condition id. A PF opportunity would be skipped. **HARD** | Require `Instrument.OutcomeID`/`Instrument.MarketID`; PM keeps the `ConditionID` check. |
| `backend/internal/engine/engine.go:968-973` | `if opportunity.IsV2() { … continue }` | PM protocol refusal embedded in generic auto-trade loop. | Capability gate. |
| `backend/internal/engine/engine.go:975` | `estimatedFee, feeErr := e.live.EstimateBuyFee(ctx, opportunity.ConditionID, budget, opportunity.Price)` | Fee lookup by PM condition id. | `executor.EstimateBuyFee(ctx, instrument, …)`. |
| `backend/internal/engine/engine.go:983-992` | `totalExposure+cost > settings.MaxExposure+1e-9` … `if snapshot.Allowance > 0 && cost > snapshot.Allowance+1e-9 { break }` | Risk limits are global (not per venue/currency) and allowance is pUSD. | Per-venue risk buckets; `CashSnapshot.Currency`. |
| `backend/internal/engine/engine.go:995` | `position, err := e.executeLiveBuy(ctx, account, opportunity, "auto", budget)` | Executor not selected per opportunity. | Pass the resolved executor. |
| `backend/internal/engine/engine.go:1013-1045` | `runStopLoss`: comment `// Polymarket has no native stop order` … `if account.Mode == AccountModeLive { _, sellErr = e.executeLiveSell(ctx, account, position, reason) }` | Stop-loss always uses the single live executor; PF LIMIT/MARKET sell semantics differ. **STRUCTURAL** | Resolve executor by `position`'s venue; PF maps stop to MARKET + `slippageBps`. |
| `backend/internal/engine/engine.go:1121-1126` | `if position.ConditionID == "" \|\| position.TokenID == "" { … "缺少 condition_id 或 token_id，无法提交止损卖单" }` / `e.live.Sell(ctx, position.TokenID, position.ConditionID, …)` | Sell requires PM ids. **HARD** | `Sell(ctx, instrument, shares, …)`. |
| `backend/internal/engine/engine.go:1168` | `applySellFill(position, fill, "stop", "Polymarket CLOB 止损成交", now)` | PM-specific reason written to the ledger (cosmetic, but appears in the orders UI). | Venue label from capabilities. |
| `backend/internal/engine/engine.go:1296-1299` | `if e.live == nil \|\| !e.live.Configured() { … "实盘未配置：请设置 POLYMARKET_PRIVATE_KEY 后重启服务" }` | Single executor guard. | `executorFor(account.Venue)`. |
| `backend/internal/engine/engine.go:1305-1308` | `if opportunity.ConditionID == "" { … "缺少 condition_id，无法加载实盘手续费" }` | Condition id is mandatory for every venue. **HARD** | Require `instrument` fields; PM adapter derives `conditionID`. |
| `backend/internal/engine/engine.go:1309-1317` | `if opportunity.IsV2() { … "该市场属于 Polymarket Protocol V2（positionId + ExchangeV3），当前实盘执行器只支持 CTF（v1）市场…" }` | PM-specific refusal in the generic buy path. | Capability flag `SupportsInstrument`. |
| `backend/internal/engine/engine.go:1337-1359` | `snapshot, cashErr := e.live.Collateral(ctx)` … `"实盘 pUSD 授权额度不足，请先在 Polymarket 完成授权"` … exposure checks | pUSD cash/allowance and global limits. | Per-venue cash + per-venue limits. |
| `backend/internal/engine/engine.go:1342` | `estimatedFee, feeErr := e.live.EstimateBuyFee(ctx, opportunity.ConditionID, amount, opportunity.Price)` | Same as above. | Instrument-based fee. |
| `backend/internal/engine/engine.go:1364` | `fill, err := e.live.Buy(ctx, opportunity.TokenID, opportunity.ConditionID, amount, settings.MaxPrice, func(orderID string) error { … CreateOrder(…) })` | Buy signature + `settings.MaxPrice` (global) + persistence callback. | `Buy(ctx, instrument, BuyRequest{Amount, MaxPrice, OrderType}, onSigned)`. |
| `backend/internal/engine/engine.go:1427` | `Reason: "Polymarket CLOB 实盘成交"` (defensive fallback when `onSigned` did not fire) | PM label in ledger. | Venue label. |
| `backend/internal/engine/engine.go:1438-1480` | `reconcileLiveOrders` → `item, ok := markets[order.MarketID]` … `e.provider.Get(ctx, order.MarketID)` … `e.live.LookupOrder(ctx, order.ExternalID, item.ConditionID)` | Market lookup and executor are single-venue; an order's venue is not stored. **HARD** | Store `Order.Venue`; use it for provider + executor. |
| `backend/internal/engine/engine.go:1509` | `applySellFill(position, fill, "stop", "Polymarket CLOB 止损成交（对账）", …)` | PM label. | Venue label. |
| `backend/internal/engine/engine.go:1539-1541` | `position := Position{… ConditionID: item.ConditionID, …}` on reconcile | Reconstructed position has no venue. | Copy `item.Venue` / `order.Venue`. |
| `backend/internal/engine/engine.go:334-391` | `QuoteOpportunity`: `quote, err := e.live.Quote(ctx, target.TokenID, target.ConditionID, amount)` | Radar quote always uses the single executor; also `e.opportunities` lookup by token only (`engine.go:349-357`). | Look up `(venue, token)`; dispatch executor. |

### 1.7 Paper path and settlement (no live executor involved)

| file:line | current code | why it blocks a second venue | suggested minimal change |
|---|---|---|---|
| `backend/internal/engine/engine.go:744-764` | `item, ok := markets[position.MarketID]` … `item, err = e.provider.Get(ctx, position.MarketID)` … `item.SettlementPrice(position.TokenID)` | Settlement/marking assumes the single provider can resolve any position's market by raw id. **HARD** | `providerFor(position.Venue).Get(...)`; settlement via per-venue settler. |
| `backend/internal/engine/engine.go:764-794` | `if settlementPrice, resolved := item.SettlementPrice(position.TokenID); resolved { … "won"/"split"/"lost" … Reason: fmt.Sprintf("依据公开结算数据按 %.4f 结算", settlementPrice) }` | Settlement is inferred from Gamma price, not from the venue's resolver. PF positions would never settle (or would settle wrongly). **HARD** | `Settler.Resolution(ctx, instrument) (price, ok, error)`; PM implementation = today's `SettlementPrice`. |
| `backend/internal/engine/engine.go:796-799` | `unrealized := outcome.Price*position.Shares - position.Cost` / `e.repo.UpdateMark(ctx, position.ID, outcome.Price, unrealized)` | Mark price is the Gamma `outcome.Price`, not a per-venue best bid or mark. | Provider returns a mark price consistent with its own book. |
| `backend/internal/engine/engine.go:1234-1294` | `executePaperBuy` uses `settings.FeeBPS`, `settings.SlippageBPS`, `settings.MaxPrice`, `settings.MaxExposure`, `settings.MaxPosition` | All single-valued. | Per-venue settings struct. |
| `backend/internal/engine/engine.go:1094-1110` | `executePaperSell` same globals. | Same. | Same. |
| `backend/internal/engine/engine.go:1597-1694` | `Dashboard`: one `Metrics` with `Cash`, `Equity`, `FeesPaid`, no currency | USDC and USDT would be summed if both venues shared an account. **HARD for correctness of PnL.** | Per-venue (or per-currency) metrics; keep legacy totals for PM-only accounts. |
| `backend/internal/engine/engine.go:1717-1729` | `snapshot` → `AddSnapshot(ctx, account.ID, EquityPoint{…})` | Equity curve is per account, not per venue/currency. | Add `venue`/`currency` on the point (store column). |

### 1.8 Store / schema

| file:line | current code | why it blocks a second venue | suggested minimal change |
|---|---|---|---|
| `backend/internal/store/sqlite.go:79-101` | `CREATE TABLE IF NOT EXISTS positions ( … market_id TEXT NOT NULL, condition_id TEXT NOT NULL, … token_id TEXT NOT NULL, … )` | No `venue`, no `currency`; `condition_id NOT NULL` assumes every position has a PM condition. **HARD** | `ensureColumn("positions","venue","TEXT NOT NULL DEFAULT 'polymarket'")` + `currency`; keep `condition_id` (write `''` for PF). |
| `backend/internal/store/sqlite.go:102-121` | `CREATE TABLE IF NOT EXISTS orders ( … market_id, token_id, … external_id … )` | No `venue`; `external_id` is a PM CLOB order id. | `ensureColumn("orders","venue", …)`. |
| `backend/internal/store/sqlite.go:122-127` | `CREATE TABLE IF NOT EXISTS equity_snapshots ( account_id, time, value )` | No venue/currency; USDT and USDC series would mix. | `ensureColumn("equity_snapshots","venue"/"currency", …)`. |
| `backend/internal/store/sqlite.go:64-73` | `CREATE TABLE IF NOT EXISTS settings ( id … payload TEXT … )` / same for `system_settings` | **Not a blocker** — payload is opaque JSON, so `venues` can be added without DDL. | Keep; only `engine.SystemSettings` needs the new field. |
| `backend/internal/store/sqlite.go:358-385` | `func (s *SQLite) ensureColumn(ctx context.Context, table, column, definition string) error { … ALTER TABLE %s ADD COLUMN %s %s }` | Generic and reusable — the doc's mechanism is correct, only its line reference is wrong (see §5). | Reuse as-is for venue/currency columns. |
| `backend/internal/store/sqlite.go:141-176` | `ensureColumn` calls: `positions/account_id`, `orders/account_id`, `orders/external_id`, `positions/kind`, `equity_snapshots/account_id`, `positions/exit_kind`, `orders/fee`, `orders/action`, followed by `migratePositionStatus` (161) and `migrateOrderStatus` (174) | This is the only migration block; new columns must be appended after both rebuilds, otherwise the rebuild's `INSERT … SELECT` drops them. | Append three `venue` columns (+ optional `currency`) after line 176 (after `migrateOrderStatus`). |
| `backend/internal/store/sqlite.go:177-181` | `CREATE INDEX IF NOT EXISTS idx_positions_account_status ON positions(account_id, status); … idx_orders_account_created …` | Indexes do not include venue; a `WHERE venue = ?` filter will scan. | Add `(account_id, venue, status)` indexes, keep old ones for compatibility. |
| `backend/internal/store/sqlite.go:685-704` | `insertPosition` column list starts `account_id, market_id, condition_id, …` | Insert has no venue. | Add `venue`/`currency` columns. |
| `backend/internal/store/sqlite.go:741-753` | `insertOrder` column list | Same. | Same. |
| `backend/internal/store/sqlite.go:892-920` | `FinalizeLiveBuy`: `UPDATE orders SET … reason = ?, …` with `"Polymarket CLOB 实盘成交"` | The **store layer writes a venue-specific reason string**. | Pass the reason from the engine (or make it venue-neutral). |
| `backend/internal/store/sqlite.go:958-982` | `FinalizeLiveSell` with `"Polymarket CLOB 止损成交"` | Same. | Same. |
| `backend/internal/store/sqlite.go:755-810` | `ListPositions(ctx, accountID, status)` — `WHERE account_id = ?` only | No venue filter; per-venue dashboards/positions cannot be scoped. | Add optional venue filter (empty = all). |
| `backend/internal/store/sqlite.go:878-885` | `ListUnresolvedOrders(ctx, accountID)` — `WHERE account_id = ? AND status IN ('pending','unknown')` | Reconciliation cannot know which venue/executor an order belongs to. | Include `venue` in the row and group by it. |
| `backend/internal/store/sqlite.go:103-119` | `type TEXT CHECK(type IN ('buy','settle'))`, `kind CHECK(kind IN ('manual','auto'))` | `type` has no `sell` — stop-loss sells are stored as `type='settle'`, `action='stop'` (`engine.go:1085-1090`). PF LIMIT/maker orders would need a new `type`/status vocabulary, which requires a table rebuild (`migrateOrderStatus` pattern). **STRUCTURAL** | Defer: map PF LIMIT/MARKET onto existing `type`/`action`/`status` values for v1, or add `migrateOrderType` like `migrateOrderStatus` (`sqlite.go:255-298`). |
| `backend/internal/store/sqlite.go:82-83` | `market_id TEXT NOT NULL, condition_id TEXT NOT NULL` | `condition_id` is `NOT NULL` — PF has a condition id too, but the semantics differ; a venue that has none cannot insert. | Keep `NOT NULL`; providers always supply a condition id string (PF has `conditionId`). Note in the adapter contract. |

### 1.9 HTTP API

| file:line | current code | why it blocks a second venue | suggested minimal change |
|---|---|---|---|
| `backend/internal/api/router.go:29-59` | Route block: `/accounts`, `/dashboard`, `/opportunities`, `/opportunities/quote`, `/scan`, `/positions`, `/orders`, `/settings`, `/system…`, `/accounts/:id/orders`, `/paper/orders` | No `?venue=`, no `GET /venues`. Every market/position/order handler is implicitly "the venue". **HARD** | Add optional `?venue=` (default polymarket) and `GET /venues`; keep existing routes/response shape. |
| `backend/internal/api/router.go:44` | `protected.GET("/opportunities", h.opportunities)` → `h.engine.Opportunities()` | Returns the flat, single-source list. | Filter by `?venue=`; keep field `source` populated. |
| `backend/internal/api/router.go:47-48` | `protected.GET("/positions", h.positions)` / `protected.GET("/orders", h.orders)` | Only `account_id` + `status`; no venue. | Add optional `venue` query, pass to repo. |
| `backend/internal/api/router.go:86-98` | `health` returns `live_configured/live_ready/live_error` (singular) | Single live status. | Add `venues: [{id, configured, ready, error}]`, keep legacy fields. |
| `backend/internal/api/system.go:20` | `const proxyProbeURL = "https://clob.polymarket.com/time"` | Hard-coded PM probe target; also a fixed host for SSRF safety. **HARD for a PF connectivity test.** | Per-venue probe URL from `Capabilities`; keep the constant as PM's. |
| `backend/internal/api/system.go:26-31` | `var request struct { Proxy *netproxy.Config; DataSource *engine.DataSourceSettings; Live *engine.LiveSettings }` | Request body has no venue dimension. | Add `Venues map[string]PlaceVenueSettings` as an optional field (partial update semantics preserved). |
| `backend/internal/api/system.go:68-80` | Response: `"data_source": settings.DataSource, "live": settings.Live.Redacted(), "runtime": h.engine.SystemRuntime(), …` | Response shape is single-venue. | Add `"venues"`, keep the old keys mirrored from PM. |
| `backend/internal/api/system.go:85-133` | `testProxy` uses `proxyProbeURL` | Only tests reachability of the PM host. | Accept a venue, choose its probe URL (whitelist per venue, not user-supplied). |
| `backend/internal/api/system.go:135-170` | `testLive` → `h.engine.ProbeLive(ctx, request.Live, request.CLOBBaseURL)` | PM-only credential probe; PF needs `x-api-key`/JWT/EIP-712 checks. | `ProbeLive(venue, …)`, return "unsupported" for unimplemented venues. |
| `backend/internal/api/system.go:172-206` | `testRelayer` | PM relayer only. | Keep PM-only, add venue to the route. |
| `backend/internal/api/system.go:208-249` | `discoverFunder` → `h.engine.DiscoverWallet(ctx, request.Live, engine.WalletEndpoints{CLOB: …, Gamma: …})` | PM wallet discovery (Gamma `/public-profile` + Polygon CREATE2). PF has no funder concept. | Keep PM-only; return "unsupported" if `venue=predictfun`. |
| `backend/internal/api/router.go:173-177` | `func (h *Handler) opportunities(c *gin.Context) { c.JSON(http.StatusOK, h.engine.Opportunities()) }` | No filter and no error path to report per-venue health. | Add venue + health to the payload. |

---

## 1b. Money / precision inventory — "does anything prevent 18-decimal USDT?"

**No.** The backend has no fixed-point money type. Summary of every money-touching site:

| file:line | code | implication |
|---|---|---|
| `backend/internal/engine/types.go:538-540` | `Price float64`, `Liquidity float64`, `Volume float64` | Market data is float. Fine for a 0–1 price; not for wei amounts. |
| `backend/internal/engine/types.go:594-619` | `Position`: `EntryPrice, CurrentPrice, Shares, Cost, Fee, RealizedPNL, UnrealizedPNL float64` | Ledger is float; no currency tag. |
| `backend/internal/engine/types.go:621-645` | `Order`: `Amount, Price, Shares, Fee float64` | Same. |
| `backend/internal/engine/types.go:748-765` | `LiveFill{Cost, Price, Fee, Shares float64}`, `CashSnapshot{Amount, Allowance float64}` | Executor boundary is float. |
| `backend/internal/store/sqlite.go:90-94` | `entry_price REAL`, `current_price REAL`, `shares REAL`, `cost REAL`, `fee REAL` | SQLite stores IEEE-754 doubles. |
| `backend/internal/store/sqlite.go:99-100`, `113-116`, `126` | `realized_pnl REAL`, `unrealized_pnl REAL`, `amount REAL`, `price REAL`, `shares REAL`, `fee REAL`, `equity_snapshots.value REAL` | Same. |
| `backend/internal/live/clob_l2.go:164-177` | `// balances and allowances in 6-decimal base units.` … `return units / 1_000_000, nil` | **Hard-codes 6 decimals.** With PF USDT (18) this is wrong by 1e12. |
| `backend/internal/live/live.go:1126` | `return units / 1_000_000, allowanceUnits / 1_000_000, nil` | Same hardcode in the SDK fallback. |
| `backend/internal/live/live.go:811-812` | `const shareScale = 10_000` / `shares := math.Floor((budget/limitPrice)*shareScale) / shareScale` | Share quantization is 4 dp; PF order sizes are ERC-1155 units and may need more (or fewer, depending on decimals). |
| `backend/internal/live/live.go:836` | `fillPrice := math.Round((notional/shares)*1e6) / 1e6` | Fill price rounded to 6 dp (PM tick convention). |
| `backend/internal/live/live.go:1162-1167` | `func parseAmount(raw string) float64 { value, err := strconv.ParseFloat(...); if err != nil { return 0 }; return value }` | A parse failure is silently 0 — for an 18-decimal string this can under-report rather than error. |
| `backend/internal/live/live.go:930-960` | `schedule.Fee(formatFloat(shares), formatFloat(price), true)` | Fee is computed in PM SDK float/string space. |
| `backend/internal/live/live.go:717-732` | `return feeFromSchedule(schedule, amount/price, price)` | Notional→shares conversion in float. |
| `backend/internal/engine/types.go:136-142` | `const DefaultFeeBPS = 15` with comment "approximates Polymarket's taker fee" | Paper fees are PM-calibrated; PF's `feeRateBps=200` with `min(p,1−p)` would be wrong. |
| `backend/internal/engine/engine.go:884`, `1100`, `1236` | `fee := … settings.FeeBPS / 10000` | Same. |
| `backend/internal/engine/engine.go:890/893/896/983/986/989/992/1031/1074/1263-1267/1353-1359` | `+1e-9` / `<= 1e-9` comparisons | Absolute epsilon on currency amounts; at 18-decimal base units a wei is 1e-18, so these epsilons are meaningless or harmful depending on the unit used. |
| `backend/internal/live/live.go:531/816/845/947/982` | `+1e-9` comparisons on price/notional/fee | Same class of issue. |
| `backend/go.mod:5-10` | requires `polymarket-clob-client`, `go-ethereum`, `gin`, `sqlite` — **no decimal library** | Nothing to build 18-dp money on today. |
| `backend/internal/live/wallet.go:140-141` | `prefix := new(big.Int).SetBytes(...)` | `math/big` is used only for CREATE2 init-code hashing, not money. |

**Conclusion for the refactor:** the 18-decimal requirement is not a "tweak `1_000_000` to `1e18`" fix. It requires (i) parsing amounts as decimal strings into `big.Int` base units at the PF client boundary, (ii) keeping float64 only for display/DB columns (or migrating to integer base units), and (iii) replacing absolute `1e-9` epsilons with unit-aware comparisons. `collateralUnits` / `collateralFromSDK` / `shareScale` / `*1e6` are the concrete blockers to change.

Fee computation sites (complete list): `live.go:698 applySellFee`, `live.go:717 EstimateBuyFee`, `live.go:930 feeFromSchedule`, `live.go:962 feeFromLevels`, `live.go:996 applyFee`, `live.go:915 feeRate`, `engine.go:884/1100/1236` (paper), `types.go:142 DefaultFeeBPS`. All of them are Polymarket's `shares × rate × p × (1−p)` model; PF's `feeRateBps/10000 × min(p,1−p) × shares` is a different formula and fee source (market object, not `/clob-markets`).

---

## 2. (b) Real call graph: scan → opportunity → auto order → settlement

Names are exact identifiers; `→` is a direct call. Line numbers in parentheses.

### 2.1 Background scan

```
Engine.Start (engine.go:85)
└─ go e.run (engine.go:104)
   ├─ e.isRunning (engine.go:107)
   ├─ context.WithTimeout(ctx, 90s) (engine.go:108)
   ├─ e.Scan (engine.go:109 → 553)
   │  └─ e.scan(ctx, ScanOptions{Execute:true}) (554 → 575)
   │     ├─ e.tradeMu.Lock() (577)                        ← serializes ALL venues/accounts
   │     ├─ e.Settings() (586)
   │     ├─ e.provider.ListOpen(ctx, settings.MaxDays) (587)
   │     │  └─ GammaClient.ListOpen (gamma.go:138)
   │     │     ├─ listMarkets(order=endDate) (187)
   │     │     │  └─ getJSON("/markets/keyset?…") (308)
   │     │     ├─ listMarkets(order=volume24hr) (187)
   │     │     └─ listMarkets(order=liquidity) (187)
   │     │        └─ parseGammaMarket (332) → gammaOutcomeIDs (399)
   │     ├─ e.recordScan(now, err) (589)
   │     ├─ for each market, for each outcome:
   │     │  └─ findOpportunity(item, outcome.Name, outcome.TokenID, settings, now) (599 → types.go:854)
   │     │     └─ opportunityFor (types.go:842)  ← URL = m.URL() (market.go:58)
   │     ├─ e.classifyOpportunities(ctx, opportunities, byID, settings) (608 → category.go:15)
   │     │  ├─ e.provider.(market.EventTagProvider).EventTags (category.go:17 → gamma.go:275)
   │     │  └─ market.Classify(item, tags) (category.go:43 → market/category.go:141)
   │     ├─ sort.Slice by EndDate/GrossYield (609-614)
   │     ├─ e.provider.(market.LocalizedProvider).MarketsLocale(ctx, ids, "zh") (621-630 → gamma.go:227)
   │     ├─ e.opportunities = opportunities; e.lastSyncAt = now (656-658)
   │     ├─ e.repo.ListAccounts (661)
   │     └─ for each account (668):
   │        ├─ if live: e.reconcileLiveOrders(ctx, account, byID) (677 → 1438)
   │        │  ├─ e.repo.ListUnresolvedOrders (1439 → sqlite.go:878)
   │        │  ├─ markets[order.MarketID] or e.provider.Get (1467-1475)
   │        │  ├─ e.live.LookupOrder(ctx, order.ExternalID, item.ConditionID) (1476 → live.go:853)
   │        │  │  ├─ client.Order(orderID) (861)
   │        │  │  ├─ e.tradesForOrder(orderID, conditionID) (882 → clob_l2.go:221 → l2Get /data/trades)
   │        │  │  └─ client.FeeSchedule(conditionID) (888 → live.go:930 feeFromSchedule)
   │        │  ├─ buy: fill.Cost += fill.Fee (1523); repo.FinalizeLiveBuy (1545 → sqlite.go:892)
   │        │  └─ stop: fill.Cost -= fill.Fee (1508); applySellFill (1509); repo.FinalizeLiveSell (1515 → sqlite.go:958)
   │        ├─ e.refreshPositions(ctx, account.ID, byID) (683 → 744)      ← SETTLEMENT (2.3)
   │        ├─ if execute && scoped && (running||manual):
   │        │  ├─ e.runStopLoss(ctx, account, settings) (702 → 1017)     ← STOP-LOSS (2.4)
   │        │  └─ e.autoTrade(ctx, account, opportunities, settings) (708 → 813)
   │        │     ├─ account.Mode==live → e.autoLiveTrade (815 → 917)     ← AUTO ENTRY (2.2)
   │        │     └─ else → e.autoPaperTrade (817 → 843)
   │        └─ e.snapshot(ctx, account) (717 → 1717)
   │           └─ e.Dashboard (1597) → e.repo.AddSnapshot (sqlite.go:1011)
   └─ timer = settings.ScanIntervalSeconds (113)
```

### 2.2 Auto entry (live) — the exact hard-coded hop chain

```
e.autoLiveTrade (817/917)
├─ e.live.Configured() / e.live.ReadyError() (918-923)     ← single executor
├─ e.repo.ListPositions(account.ID,"all") (925)
├─ orderedMarkets[position.MarketID] = true (935-941)       ← key has NO venue
├─ e.live.Collateral(ctx) (947 → live.go:1028)              ← pUSD balance/allowance
│  └─ e.collateralBalance (1065) → e.l2Get("/balance-allowance") (1075)
│     └─ collateralUnits (clob_l2.go:165) → /1_000_000
└─ for each opportunity (953):
   ├─ orderedMarkets[opportunity.MarketID] (956)
   ├─ withinAutoEntryWindow(opportunity, settings, now) (962 → 834)  ← EndDate-based
   ├─ opportunity.ConditionID == "" → skip (965)
   ├─ opportunity.IsV2() → skip (968)
   ├─ e.live.EstimateBuyFee(ctx, opportunity.ConditionID, budget, price) (975 → live.go:717)
   │  └─ client.FeeSchedule(conditionID) (728)
   ├─ exposure/cash/allowance checks (983-992)
   └─ e.executeLiveBuy(ctx, account, opportunity, "auto", budget) (995 → 1296)
      ├─ e.live.Configured/ReadyError (1297-1304)
      ├─ opportunity.ConditionID == "" → reject (1305)
      ├─ opportunity.IsV2() → reject (1309-1317)
      ├─ e.repo.ListPositions (1319)
      ├─ e.ensureLiveBaseline (1334 → 1696) → e.live.Collateral (1700)
      ├─ e.live.Collateral (1337)
      ├─ e.live.EstimateBuyFee (1342)
      ├─ risk checks incl. snapshot.Allowance (1353-1359)
      └─ e.live.Buy(ctx, opportunity.TokenID, opportunity.ConditionID, amount, settings.MaxPrice, onSigned) (1364 → live.go:512)
         ├─ client.FeeSchedule(ctx, conditionID) (520)
         ├─ client.MarketBuyOrder(ctx, tokenID, amount, OrderTypeFAK) (524)
         ├─ order.Builder = e.builderCode (530)
         ├─ maxPrice check (531)
         ├─ client.CreateOrder(ctx, order) (534)              ← EIP-712 sign
         ├─ onSigned(signed.OrderID) (539) → engine callback:
         │  └─ e.repo.CreateOrder(Order{Status:"pending", ExternalID: orderID}) (1365-1370)
         ├─ client.SubmitSignedOrder(ctx, signed, OrderTypeFAK) (543)
         ├─ fill.Cost = parseAmount(MakingAmount); fill.Shares = parseAmount(TakingAmount) (572-573)
         └─ applyFee(schedule, &fill) (live.go:996)            ← cost += fee
      └─ on success: e.repo.FinalizeLiveBuy(pending.ID, position, fill) (1415 → sqlite.go:892)
```

### 2.3 Settlement (paper and live positions alike)

```
e.refreshPositions (engine.go:744)
├─ e.repo.ListPositions(accountID, "open") (745 → sqlite.go:755)
└─ for each position:
   ├─ markets[position.MarketID] or e.provider.Get(ctx, position.MarketID) (751-757)
   ├─ item.OutcomeByToken(position.TokenID) (759 → market.go:65)
   ├─ if item.SettlementPrice(position.TokenID) resolves (764 → market.go:98):
   │  ├─ finalResolutionData: m.Closed && len(Outcomes)==2 && resolutionStatus in {"", "resolved"} (market.go:119)
   │  ├─ split if both outcomes 0.499..0.501 (market.go:105-106)
   │  ├─ winner if one outcome price >= 0.999 and the other <= 0.001 (market.go:83-92)
   │  ├─ position.Status = "won" | "split" | "lost" (771-776)
   │  ├─ position.RealizedPNL += payout - position.Cost (780)
   │  └─ e.repo.SettlePosition(position, Order{Type:"settle", Status:"settled"}) (791 → sqlite.go:989)
   └─ else mark-to-market:
      └─ e.repo.UpdateMark(position.ID, outcome.Price, outcome.Price*shares - cost) (797 → sqlite.go:984)
```

Note: this settlement path never consults the live executor. For a PF position it will call the PM provider with a PF market id, fail `OutcomeByToken`, and the position stays open forever (`engine.go:759-762` returns an error per position).

### 2.4 Stop-loss

```
e.runStopLoss (engine.go:1017)
├─ settings.StopLossEnabled (1021)
├─ e.repo.ListPositions(account.ID,"open") (1022)
└─ for each position with shares/entry/current > 0:
   ├─ trigger = EntryPrice * settings.StopLossRatio (1030)
   ├─ CurrentPrice > trigger+1e-9 → continue (1031)
   └─ account.Mode==live ? e.executeLiveSell (1039 → 1112) : e.executePaperSell (1041 → 1094)
      executeLiveSell:
      ├─ e.live.Configured/ReadyError (1113-1119)
      ├─ position.ConditionID/TokenID required (1121)
      ├─ e.live.Sell(ctx, position.TokenID, position.ConditionID, shares, onSigned) (1126 → live.go:613)
      │  ├─ client.FeeSchedule(conditionID) (621)
      │  ├─ client.MarketSellOrder(tokenID, shares, OrderTypeFOK) (625)
      │  ├─ client.CreateOrder (630) → onSigned → repo.CreateOrder(pending, action="stop") (1127-1133)
      │  └─ applySellFee(schedule,&fill) (live.go:698) → fill.Cost -= fee
      └─ applySellFill (1168 → 1056) → repo.FinalizeLiveSell (1173 → sqlite.go:958)
          (partial fill keeps position open; full fill → status "lost", ExitKind "stop")
```

---

## 3. (c) Ordered refactor slices that keep Polymarket behavior byte-identical

Each slice is independently shippable and testable with the existing `go test ./...` suite. "PM identical" means: same HTTP calls in the same order, same DB rows/values, same JSON.

**S0 — characterization tests (no production change).**
Capture golden fixtures for: `GammaClient.ListOpen` merge/dedup, `findOpportunity` accept/reject reasons, `LiveExecutor.Buy/Sell/Quote/LookupOrder` request shape against a `httptest` CLOB, and the exact `orders.reason` strings (`Polymarket CLOB 实盘成交`, `Polymarket CLOB 止损成交`). These become the regression oracle for every later slice. Files: `internal/engine/engine_test.go`, `internal/live/live_test.go`, `internal/store/sqlite_test.go`.

**S1 — add `backend/internal/venue` vocabulary (additive only).**
`type ID string` with `Polymarket`/`PredictFun`; `Instrument{Venue, MarketID, OutcomeID, ConditionID}`; `Capabilities{Venue, Label, Currency, QuoteDecimals, MinNotional, OrderTypes, EndDateGranularity, BinaryOnly}`; `QuoteCurrency`. No behavior change; nothing imports it yet except new tests.
*Note:* `internal/adapters/predictfun/testdata` already exists as an empty scaffold (see §6).

**S2 — `market.Provider.Venue()` + `engine.Engine` registry, one entry.**
Add `Venue() venue.ID` to `Provider` (`market.go:127-131`), implement `return venue.Polymarket` in `GammaClient`. Change `Engine{provider, live}` to `Engine{venues *venue.Registry}` with exactly one registered provider/executor and accessor methods `providerFor(venue)`/`executorFor(venue)` that ignore the argument and return the single entry. Every existing `e.provider`/`e.live` call site (the list in §1.2/§1.4) becomes `e.providerFor(venue.Polymarket)`. **No HTTP/DB/JSON change** — verified by S0 goldens.
Files: `market/market.go`, `market/gamma.go`, `engine/engine.go`, `engine/types.go` (repo interface untouched), `cmd/server/main.go`, `engine_test.go`, `api/router_test.go` fixtures.

**S3 — namespace identity inside the engine (no schema change).**
Introduce an internal key `type marketKey struct{ venue venue.ID; id string }`; replace `map[string]market.Market` (`engine.go:595`), the localized `byID` (`engine.go:631`), `orderedMarkets`/`heldMarkets`/`skippedMarkets`/`marketExposure` (`engine.go:852-861`, `engine.go:928-941`) with keyed maps. Add `Opportunity.Venue` and `Opportunity.Currency` (JSON additive; frontend ignores unknown fields). DB `market_id` keeps the raw PM id so rows are unchanged.
**Behavior-risk to test:** de-dup semantics are today "one order per market id"; with a single venue the key is equivalent, so tests must pass unchanged.

**S4 — `LiveExecutor` → `VenueExecutor` with `Instrument`.**
New interface (as in the architecture doc §2.3), plus a thin PM adapter whose methods re-derive the exact old arguments: `in.ConditionID` for fee/`LookupOrder`, `in.OutcomeID` for `tokenID`. Keep `type LiveExecutor = VenueExecutor` (or an unexported adapter) so `engine_test.go:71-131` fakes compile with a one-line method-set addition. Engine call sites change only in argument construction; the executed CLOB calls stay byte-identical (S0 golden).
`BuyRequest{Amount, MaxPrice, OrderType, SlippageBps}` and `Fill`/`Quote`/`Cash` shapes: keep `LiveFill`, `LiveQuote`, `CashSnapshot` names to avoid touching the store (`FinalizeLiveBuy/FinalizeLiveSell` signature at `sqlite.go:892/958`).

**S5 — `SystemSettings` gains `Venues` with dual-read/dual-write.**
Add `Venues map[venue.ID]VenueSettings` to `SystemSettings` (`types.go:217`). On load: if `Venues` is empty, synthesize `Venues["polymarket"]` from `DataSource`/`Live`; on save: mirror `Venues["polymarket"]` back into `DataSource`/`Live` so an older build/row still works. `Validate()` rejects unknown venue ids. Secrets: reuse `store/secrets.go` `enc:v1:` (`secrets.go:20`) and the `******` merge rule (`types.go:348-361`, `netproxy.RedactedSecret`).
No API break: `writeSystemSettings` still returns `data_source`/`live` (`system.go:68-80`) and additionally `venues`.

**S6 — store: `venue`/`currency` columns, defaulted, additive.**
Append to `migrate` **after both table rebuilds** (i.e. after `migrateOrderStatus` returns at `sqlite.go:176`):
`ensureColumn("positions","venue","TEXT NOT NULL DEFAULT 'polymarket'")`, same for `orders`, `equity_snapshots`, plus `currency TEXT NOT NULL DEFAULT 'USDC'` on positions/snapshots. Add indexes `(account_id, venue, status)`. Read paths scan the new columns into `Position.Venue`/`Order.Venue`; write paths write them (PM writes `"polymarket"`/`"USDC"`). Queries keep `WHERE account_id = ?` (no new filter) so PM behavior is unchanged; add optional `venue` parameter only in S10.
Ordering is load-bearing: `migratePositionStatus` (`sqlite.go:161`) and `migrateOrderStatus` (`sqlite.go:174`) reconstruct their tables with an explicit column list, so a `venue` column added *before* them would be silently dropped on databases that still need the rebuild. Placing them after line 176 avoids touching the rebuild logic entirely; the alternative is to add `venue` to both `CREATE TABLE …_new`/`INSERT … SELECT` lists. Test with a pre-existing DB fixture either way.

**S7 — fee and cash abstractions.**
`type FeeModel interface { Estimate(ctx, in Instrument, amount, price float64) (float64, error); ForFill(...) }` with `pmFeeModel` wrapping `client.FeeSchedule`+`feeFromSchedule` (identical math) and `type CashReader interface { Collateral(ctx) (Cash, error) }`. Engine calls `executor.EstimateBuyFee` unchanged in S7; only the implementation is extracted. This is the prerequisite for PF's `feeRateBps` model.

**S8 — money precision at the boundary (PM unchanged).**
Introduce `type Amount struct{ Units *big.Int; Decimals int }` (or `big.Rat`) used only inside executors and provider parsers. PM: `Decimals=6`, conversion helpers reproduce today's `/1_000_000` exactly, and the JSON/DB output remains float64 with the same values (add a test asserting `formatFloat` round-trip for the S0 fixtures). Replace `1e-9` absolute epsilons with `epsilon(decimals)`.
This slice makes 18-decimal USDT representable without changing any PM number.

**S9 — PF read-only provider (feature-flagged, no trading).**
`provider.PredictFun` implementing `ListOpen/Get/Name/Venue`; category→`endsAt` join with cache; `Unsupported` reasons; provider-populated `Market.URL`. Engine scan loops the registry, merges by `(venue, marketID)`, isolates errors per venue. PM output must remain identical when the PF flag is off (goldens).
At this point `Opportunity.Venue/Currency` are populated and the radar is multi-venue, still paper-only.

**S10 — API/venue surface.**
`GET /venues` (capabilities + health), optional `?venue=` on `/opportunities`, `/positions`, `/orders`, `/dashboard`; `/system` gains `venues`; `/system/proxy/test`, `/system/live/test` take a venue and pick a whitelisted probe URL (PM keeps `https://clob.polymarket.com/time`). Existing default behavior: no `?venue=` → `polymarket`.

**S11 — PF executor (EIP-712, LIMIT/MARKET) + per-venue execution policy.**
`VenueExecutor` implementation with `Instrument`, `x-api-key`+JWT auth, `big.Int` order amounts, `LIMIT`/`MARKET` only, `slippageBps`. `ExecutionPolicy` per venue in `Settings` (`types.go:61` block) — PM default remains `Taker/FAK`, `MaxPrice` from global settings. Stop-loss maps to PF `MARKET + slippageBps` (no FOK).
Per-venue live account: relax `engine.go:473-476` to `(venue, mode)` uniqueness and add `Account.Venue` (needs an `accounts.venue` column via the same `ensureColumn` pattern).

**S12 — per-venue settlement.**
`type Settler interface { Resolution(ctx, Instrument) (price float64, ok bool, err error) }`; `pmSettler` = `Market.Winner`/`SettlementPrice` verbatim; `pfSettler` reads its resolver/oracle and **fails closed** until implemented. `refreshPositions` dispatches by `position.Venue`; a PF position with no settler stays open with a visible `LastError` instead of silently never settling.

Do **not** reorder S6 before S2–S4: schema changes without namespaced identity produce ambiguous rows that are hard to backfill.

---

## 4. (d) `docs/multi-venue-architecture.md` §4.1 — verified errors and stale claims

The doc's §4.1 table (`docs/multi-venue-architecture.md:280-300`) was checked line by line against the current tree. Claims that are correct: `market.Provider` is a real interface; `Engine.provider`/`Engine.live` are single-valued; `LiveExecutor` hard-codes `conditionID` for the fee schedule; the three tables have no venue column; `settings.payload`/`system_settings.payload` are JSON text. The following are **wrong or stale**:

| doc location | doc claim | actual code | verdict |
|---|---|---|---|
| `multi-venue-architecture.md:293` | `engine/engine.go:557-673` is `scan` | `scan` is `engine.go:575-724`. Line 557 is the blank line after `Scan`; `Scan` itself is `553-556`. | **Wrong line range.** Use `575-724`. |
| `multi-venue-architecture.md:294` | `engine/engine.go:754/907` are `autoTrade`/`PlaceOrder` | `autoTrade` is `engine.go:813-818`; `autoLiveTrade` `917-1009`; `PlaceOrder` `1206-1232`. Line 754 is inside `refreshPositions`; 907 is inside `autoLiveTrade`. | **Wrong lines.** There is no function at either line. |
| `multi-venue-architecture.md:295` | `engine/engine.go:1011-1019` is the PM V2 refusal | The V2 **skip** in auto-trade is `engine.go:968-973`; the explicit refusal (with the user-facing message) is `engine.go:1309-1317`. Line 1011 is the `runStopLoss` doc comment. | **Wrong lines.** Also there are *two* V2 paths, not one. |
| `multi-venue-architecture.md:296` | `engine/types.go:491` is `Opportunity` | `Opportunity` is `types.go:527-555`. Line 491 is `type WalletEndpoints struct`. | **Wrong line.** |
| `multi-venue-architecture.md:297`, `:156` | `engine/types.go:695` is `LiveExecutor` | `LiveExecutor` is `types.go:769-788`. Line 695 is the `Equity []EquityPoint` field of `Dashboard`. | **Wrong line** (cited twice). |
| `multi-venue-architecture.md:231` | idempotent `ensureColumn` is `sqlite.go:343` | `ensureColumn` is `sqlite.go:358-385`. Line 343 is inside `seedAccounts`. | **Wrong line.** Mechanism itself is correct. |
| `multi-venue-architecture.md:274` | "每个实例只允许一个实盘账户" is `engine.go:471` | The check is `engine.go:473-476`; the string `"每个实例只允许一个实盘账户"` is at `engine.go:475`. Line 471 is `return Account{}, err` in the `ListAccounts` error branch. | **Off by 4 lines.** |
| `multi-venue-architecture.md:244` | `SystemSettings` is `engine/types.go:181` | `SystemSettings` is `types.go:217-222`. Line 181 is inside `Settings.Validate()` (closing brace of the `MaxExposure` check). | **Wrong line.** |
| `multi-venue-architecture.md:285` | "删除 `:58` 的 polymarket.com 硬编码" | `func (m Market) URL()` starts at `market.go:58`, but the hard-coded literal `"https://polymarket.com/event/"` is at `market.go:62`. | **Half right** — the function is at 58, the string to delete is at 62. |
| `multi-venue-architecture.md:285` | `market/market.go:11-154` is the `Market` type | File is 154 lines; `Outcome` starts at 11, `Market` is `17-42`. | **Misleading range** (it is the whole file, not the type). |
| `multi-venue-architecture.md:298` | routes are `router.go:24-60`; add `?venue=` to "all market/position/order routes" | Route registrations are `router.go:29-59` inside `NewRouter` (`21-65`); line 24 is `h := &Handler{…}`. Also there is no separate market route group: opportunities `44`, quote `45`, positions `47`, orders `48`, place `58`. | **Range slightly wrong**; the inventory of "all routes" must include `/opportunities/quote` (45) and `/scan` (46). |
| `multi-venue-architecture.md:299` | `api/system.go:30-74` is request+response; probe URL `system.go:21` | Request struct is `system.go:27-31`; response is `system.go:68-80`. `proxyProbeURL` is `system.go:20` (comment `18-19`). | **Both line refs wrong** (30 is one field; 21 is blank). |
| `multi-venue-architecture.md:300` | `config.go:34-51` | `FromEnv` is `config.go:34-52` (closing brace 52). The env names to add are absent — confirmed (`config.go:24-31`, `38-50`). | **Essentially correct**, off by one at the end; env-var claim correct. |
| `multi-venue-architecture.md:139` | "`market.Provider` 已经是真接口，**只需加两件事**" (add `Venue()` and keep `Name()`) | The engine additionally type-asserts three optional behaviours on the provider: `dataSourceRuntime` (`engine.go:255`, implemented by `GammaClient.SetBaseURL/BaseURL/DefaultBaseURL` at `gamma.go:46-83`), `market.EventTagProvider` (`engine.go:621`/`category.go:17`), `market.LocalizedProvider` (`engine.go:621`). A second provider must either implement `dataSourceRuntime` or accept that `SystemRuntime()` returns empty URL fields (`engine.go:282-285`). | **Stale/incomplete** — it is more than two things. |
| `multi-venue-architecture.md:150` | "不要删字段（会破坏 3 个包的测试与前端类型）" | `market.Market` is referenced by exactly two non-`market` packages in Go: `engine` (`engine.go`, `category.go`, `types.go`, tests) and `api` (tests only, `router_test.go:22-52`). The frontend has its own hand-written DTOs (`frontend/src/types.ts:82-130`), not generated from Go structs. | **Wrong count and wrong reason** — 2 Go packages + hand-maintained frontend types. |
| `multi-venue-architecture.md:90-119` (§2.2 `Capabilities`) | proposes `SupportsCancel`, `MakerFeeFree`, `EndDateGranularity`, `BinaryOnly` | None of these exist today; `Settings` (`types.go:61-99`) has no venue/capability fields and `Market` (`market.go:17-42`) has no `Unsupported`/`OutcomeCount`. | **Correctly forward-looking** (not stale), but it is a proposal, not a description. |
| `multi-venue-architecture.md:139` code block | `type Provider interface { Venue() venue.ID; ListOpen(...); Get(...); Name() string }` | Current interface is exactly `ListOpen/Get/Name` (`market.go:127-131`). Adding `Venue()` is the only signature change needed there. | **Correct.** |
| `multi-venue-architecture.md:194` | "现状 `engine.go:557 scan()` 一次 `provider.ListOpen` 拿全部市场" | The single `ListOpen` call is `engine.go:587`; `scan` is `575`. The claim "one upstream failure kills the round" is true (`engine.go:590-592` returns `ErrUpstream`). | **Behavioral claim correct, line wrong.** |
| `multi-venue-architecture.md:231-241` | "三张表加 venue 列，全部走现有幂等 `ensureColumn`，不重建表" | `ensureColumn` is indeed idempotent (`sqlite.go:358-385`) and additive. However `positions`/`orders` are **already rebuilt** by `migratePositionStatus` (`sqlite.go:201-253`) and `migrateOrderStatus` (`sqlite.go:255-298`), so any venue column added before those rebuilds must also be added to the rebuild `CREATE TABLE`/`INSERT … SELECT` lists, or it will be dropped on old databases. | **Incomplete** — "no rebuild" is only true for databases that already have the latest CHECK constraints. |
| `multi-venue-architecture.md:231` | `equity_snapshots` needs `venue` **and** `currency` | `equity_snapshots` is `(account_id, time, value)` (`sqlite.go:122-127`); currency is not proposed as a column there in the doc's table (it proposes `positions.currency` only). Since the dashboard sums a single `value`, a PF USDT point and a PM USDC point are indistinguishable without it. | **Design gap** in the doc, not a code error. |
| `multi-venue-architecture.md:274` | "模拟账户的 `FeeBPS`/`SlippageBPS` 也提升为 per-venue" | Correctly identifies the problem: both live in the single `Settings` (`types.go:70-71`) and are used at `engine.go:884-886`, `1095-1100`, `1236-1238`. | **Correct.** |
| `multi-venue-architecture.md:299` | "探测端点按场所选择探测 URL（现在写死 `clob.polymarket.com/time`）" | Correct observation (`system.go:20`), wrong line (21). Also note the probe URL is intentionally a compile-time constant to prevent SSRF from the browser (`system.go:18-19`), so "per venue" must mean a server-side whitelist, not a user-supplied URL. | **Correct observation, missing constraint.** |
| `multi-venue-architecture.md:292` | `engine/engine.go:17-40` → `provider`/`live` to `*venue.Registry` | `Engine` struct is `17-38`; `New` is `40-83`. The two fields are `19-20`. | **Correct enough.** |
| `multi-venue-architecture.md:286` | `gamma.go:92` `Name()` | Exactly `func (g *GammaClient) Name() string { return "Polymarket Gamma" }` at line 92. | **Correct.** |
| `multi-venue-architecture.md:231` (`sqlite.go:64-73`) | settings/system_settings payloads are JSON text | `CREATE TABLE settings` is `64-68`, `system_settings` `69-73`. | **Correct.** |

Additional stale facts outside §4.1 worth flagging while sizing the work:

1. **§0.4 / §3.1 say the three tables have no venue column — true — but the doc does not mention that `orders.type` has a `CHECK(type IN ('buy','settle'))`** (`sqlite.go:111`). PF LIMIT/MARKET orders need either a mapping onto `buy`/`settle`+`action` or a new `migrateOrderType` rebuild (pattern at `sqlite.go:255`).
2. **§3.1 says `positions.condition_id` can stay**; it is `TEXT NOT NULL` (`sqlite.go:83`). PF does expose `conditionId`, so this is workable, but the adapter must always write a non-empty string or inserts fail.
3. **§2.3's `VenueExecutor` example keeps `BuyRequest`/`Fill`/`Quote`/`Cash` as new names**, while the store methods `FinalizeLiveBuy`/`FinalizeLiveSell` take `engine.LiveFill` (`sqlite.go:892/958`). Renaming the types forces a store change that the doc's slice list does not mention.
4. **§2.3 says "保留 `LiveExecutor` 作为别名，避免测试与 `types.go:695` 引用全部改"** — the alias is a good idea, but the referenced line is wrong (§ above) and the fake in `engine_test.go:71-131` implements the ten-method interface plus `ReloadProxy`, so adding `Venue()`/`Instrument` breaks it unless the alias/adaptor keeps the old method set.
5. **The doc's `market.Market` plan says "`TokenID` → 保留为数据库列名"**, but `token_id` is used in queries/indexes at `sqlite.go:179` and in `OutcomeByToken` (`market.go:65-72`); keeping the column name is fine, but the semantic change to "venue outcome id" must also cover `positions.token_id` and `orders.token_id` (`sqlite.go:86/109`), which the doc does not list.

---

## 5. Cross-cutting notes that do not fit a single line

1. **`e.provider.Name()` is user-visible.** `OpportunityList.Source` (`engine.go:327`) and `Dashboard.Source.Name` (`engine.go:1685`) render `"Polymarket Gamma"`. A registry must keep a stable aggregate label or the UI regresses.
2. **`Settings.Validate()` rejects `MaxPrice >= 1`** (`types.go:162-165`), which is correct for both venues (prices are probabilities). No change needed.
3. **`MaxDays` is applied inside the provider** (`ListOpen(ctx, settings.MaxDays)`) and again in `findOpportunity` (`types.go:855`). PF has no per-market end date, so `maxDays` semantics must be defined by the PF provider (likely: exclude markets with unknown `endsAt`), not by the engine.
4. **Time zone / time parsing is Gamma-specific**: `parseGammaTime` (`gamma.go:427-445`) accepts several PM layouts; PF timestamps must be parsed in its own client.
5. **`applyRuntimeSettings` order matters** (`engine.go:248-276`): proxy → provider URL → live settings, with rollback. A registry must preserve that order per venue and roll back every venue on a partial failure.
6. **`tradeMu` is also taken by `PlaceOrder`** (`engine.go:1207`) and `UpdateSystemSettings` (`engine.go:179`), so a long PF scan would delay a manual PM order. Per-venue locks or a shorter critical section are a prerequisite for "independent venues".
7. **`live.New` normalizes and validates Polymarket-only fields** (`live.go:70-81`, `validateEffectiveConfig` `live.go:320-378`): `SignatureType` 0..3 and the "EOA must not set Funder" rule. PF's `signatureType` enum (EOA / POLY_PROXY / POLY_GNOSIS_SAFE per the doc's Appendix C) is a different domain and must not reuse this validator.
8. **`netproxy` is venue-neutral** (`netproxy.go:20-22` mentions Polymarket only in a comment). It can be shared as-is; the only coupling is the `Proxy *netproxy.Manager` field in `live.Config` (`live.go:44-46`) and `engine.proxy`.
9. **`api/auth.go` is venue-neutral** (password/bcrypt only); no changes needed.
10. **Cosmetic PM strings that will surface in a multi-venue UI** (safe to defer, but they are the visible "one venue" tells): `engine.go:919`, `1299`, `1356`, `1427`, `1509`, `store/sqlite.go:905/970`, `market.go:62`, `api/system.go:20`, `live.go:135/385/564/658`.

---

## 6. Pre-existing scaffolding (informational)

`backend/internal/` contains empty, untracked directories that suggest an earlier aborted layering attempt:

```
internal/adapters/polymarket/        (empty)
internal/adapters/predictfun/testdata (empty)
internal/app, domain, platform, ports, service   (empty)
internal/execution/paper             (empty)
internal/strategy/tail               (empty)
```

None of these are in `git ls-files`; no code imports them. The refactor can either use `internal/venue` (as the architecture doc proposes) or revive `internal/adapters/{polymarket,predictfun}`. The latter is a cleaner fit for the existing `internal/live` split (PM wallet/relayer/clob-l2 code is Polymarket-specific and should move rather than be generalized).

---

## 7. One-line answer to "how big is this?"

Counting only rows marked `**HARD**` in §1: 31 sites across 10 files (`engine.go` 14, `market.go` 4, `types.go` 4, `main.go` 3, and one each in `config.go`, `api/router.go`, `live.go`, `clob_l2.go`, `wallet.go`, `sqlite.go`). One more row (`api/system.go:20`, the hard-coded probe URL) is marked `**HARD for a PF connectivity test**` and is included in the count qualitatively. The refactor is not a client addition; it is three coupled changes: (1) venue-keyed identity through engine + store, (2) an `Instrument`-based executor/provider contract, (3) a decimal-safe money boundary. Everything else (probe URLs, labels, settings UI) is mechanical once those three exist.
