import type { Currency, Venue } from '@/lib/venue'

export type { Currency, Venue }

export type AccountMode = 'live' | 'paper'

// VenueCapabilities mirrors GET /venues: what the venue can do at runtime.
export interface VenueCapabilities {
  venue: Venue
  label: string
  currency: Currency
  quote_decimals: number
  min_notional: number
  // min_sell_shares is the smallest sell the venue accepts, in shares. A buy
  // may be sized in money, but an exit is a limit order floored by shares, so a
  // position below this count cannot be sold (0 = no floor).
  min_sell_shares: number
  order_types: string[]
  max_slippage_bps: number
  supports_cancel: boolean
  supports_settlement: boolean
  // supports_redeem is true when settled shares can be burned back into
  // collateral on chain. Polymarket claims through its own UI, so it is false.
  supports_redeem: boolean
  read_only: boolean
  testnet: boolean
}

export interface VenuesResponse {
  venues: VenueCapabilities[]
}

// VenueRedemption is the response of POST /accounts/:id/positions/:positionId/redeem.
// tx_hash is the only durable handle: the transaction is already on chain.
export interface VenueRedemption {
  venue: Venue
  tx_hash: string
  method: string
}

export interface Account {
  id: number
  name: string
  mode: AccountMode
  /** 每个账户都属于一个场所；缺省按 Polymarket 解释。 */
  venue: Venue
  currency: Currency
  is_default: boolean
  auto_trade: boolean
  baseline_equity: number
  created_at: string
  live_configured: boolean
  live_ready: boolean
  live_signer: string
  live_maker: string
  live_error: string
  /** Predict.fun 测试网实盘；Polymarket 与模拟账户为 false。 */
  live_testnet?: boolean
}

export interface AccountsResponse {
  items: Account[]
  total: number
}

export interface Health {
  status: 'ok' | string
  service: string
  mode: AccountMode | string
  default_account_mode: AccountMode | string
  live_configured: boolean
  live_ready: boolean
  live_error: string
  time: string
}

export interface Metrics {
  mode: AccountMode
  /** 指标只覆盖单个场所（账户隔离），因此可以安全展示。 */
  venue: Venue
  currency: Currency
  initial_bankroll: number
  cash: number
  wallet_cash: number
  wallet_cash_stale: boolean
  equity: number
  realized_pnl: number
  unrealized_pnl: number
  total_pnl: number
  fees_paid: number
  total_return: number
  open_exposure: number
  open_positions: number
  settled_positions: number
  split_positions: number
  stopped_positions: number
  win_rate: number
  // auto_trade_started_at is the account's first automatic buy: the start of the
  // strategy window that annualized_return is extrapolated over.
  auto_trade_started_at: string | null
  annualized_return: number
  annualized_days: number
  total_orders: number
}

export interface Engine {
  running: boolean
  last_scan_at: string | null
  last_error: string
  scan_count: number
  mode: AccountMode | string
}

export interface Source {
  name: string
  last_sync_at: string | null
}

export interface EquityPoint {
  time: string
  value: number
  /** 旧快照可能没有场所；渲染时用账户的场所兜底。 */
  venue?: Venue
  currency?: Currency
}

export interface Dashboard {
  account: Account
  metrics: Metrics
  engine: Engine
  source: Source
  equity: EquityPoint[]
}

export interface Opportunity {
  venue: Venue
  currency: Currency
  market_id: string
  condition_id: string
  question: string
  question_zh?: string
  group_item_title?: string
  event_title_zh?: string
  slug: string
  outcome: string
  outcome_zh?: string
  token_id: string
  price: number
  liquidity: number
  volume: number
  end_date: string
  days_to_end: number
  gross_yield: number
  url: string
  category: string
  sports_game: boolean
  overtime_risk: boolean
  price_change_24h?: number | null
  /** 仅 Polymarket 有协议版本：v2 市场走 positionId + ExchangeV3，实盘暂不支持 */
  version?: string | null
}

// ScanAutoTrade reports what a manual scan did for the requesting account.
// orders_placed counts new automatic entries; skipped_held counts candidates
// that already had a position record and were not bought again.
export interface ScanAutoTrade {
  account_id: number
  execute: boolean
  orders_placed: number
  skipped_held: number
  error?: string
}

export interface OpportunitiesResponse {
  items: Opportunity[]
  total: number
  scanned_at: string | null
  source: string
  auto_trade?: ScanAutoTrade
  /** 本次结果涉及的场所（服务端 venue 过滤之后）。 */
  venues?: Venue[]
}

// OpportunityQuote is a real-data estimate: the ask book and taker fee come
// from Polymarket's CLOB, not from the Gamma reference price.
export interface OpportunityQuote {
  token_id: string
  condition_id: string
  amount: number
  best_ask: number
  limit_price: number
  fill_price: number
  shares: number
  notional: number
  fee_rate: number
  fee: number
  total_cost: number
  payout: number
  net_profit: number
  net_yield: number
  gross_yield: number
  available_notional: number
  fully_filled: boolean
  exceeds_max_price: boolean
  max_price: number
  quoted_at: string
}

export type PositionStatus = 'open' | 'won' | 'lost' | 'split' | 'sold'

export interface Position {
  id: number
  account_id: number
  venue: Venue
  currency: Currency
  market_id: string
  condition_id: string
  question: string
  outcome: string
  token_id: string
  side: 'BUY'
  kind: 'manual' | 'auto'
  /** 空字符串表示普通持仓；stop 表示由止损提前平仓 */
  exit_kind: string
  entry_price: number
  current_price: number
  shares: number
  cost: number
  fee: number
  status: PositionStatus
  opened_at: string
  closed_at: string | null
  end_date: string
  realized_pnl: number
  unrealized_pnl: number
}

export interface PositionsResponse {
  items: Position[]
  total: number
}

export type OrderStatus = 'filled' | 'rejected' | 'settled' | 'pending' | 'unknown'
export type OrderStatusFilter = OrderStatus | 'all' | 'unresolved'

export interface Order {
  id: number
  account_id: number
  venue: Venue
  currency: Currency
  position_id: number | null
  market_id: string
  question: string
  outcome: string
  token_id: string
  kind: 'manual' | 'auto'
  type: 'buy' | 'sell' | 'settle'
  /** 空字符串表示买入/结算；stop 表示止损卖出 */
  action: string
  amount: number
  price: number
  shares: number
  fee: number
  status: OrderStatus
  reason: string
  external_id: string
  created_at: string
}

export interface OrdersResponse {
  items: Order[]
  total: number
}

// VenueSettings overrides the global Settings for one venue. Zero/absent
// numeric values inherit the global value; disabled venues are neither scanned
// nor traded.
export interface VenueSettings {
  enabled: boolean
  /** 0 / omitted = inherit the global min_liquidity. */
  min_liquidity: number
  order_size: number
  max_position: number
  max_exposure: number
  fee_bps: number
}

export interface Settings {
  min_price: number
  max_price: number
  max_days: number
  min_liquidity: number
  order_size: number
  max_position: number
  max_exposure: number
  /** 0 = 不限；同一事件下全部未平仓头寸的合计成本上限。 */
  max_event_exposure: number
  scan_interval_seconds: number
  slippage_bps: number
  fee_bps: number
  initial_bankroll: number
  default_category: string
  sports_policy: string
  max_price_change_24h: number
  entry_window_minutes: number
  sports_entry_window_minutes: number
  /** 加密分类的尾盘窗口，单位秒；优先于上面的分钟窗口。 */
  crypto_entry_window_seconds: number
  stop_loss_enabled: boolean
  stop_loss_ratio: number
  /** 服务端管理，前端只读并原样回传。 */
  fee_model_version?: number
  /** 逐场所覆写；缺省字段继承上面的全局值。 */
  venues?: Record<Venue, VenueSettings>
}

export type ProxyMode = 'environment' | 'custom' | 'off'

export interface ProxyConfig {
  mode: ProxyMode
  url: string
  no_proxy: string
}

export interface DataSourceSettings {
  gamma_base_url: string
  clob_base_url: string
}

// Per-venue system settings (contract §5). Polymarket keeps the flat gamma/clob
// pair; Predict.fun uses one base_url for quotes and trading.
export interface VenueDataSourceSettings {
  gamma_base_url?: string
  clob_base_url?: string
  base_url?: string
  testnet?: boolean
}

export interface VenueLiveSettings {
  private_key?: string
  funder?: string
  signature_type?: number | null
  api_key?: string
  api_secret?: string
  api_passphrase?: string
  smart_wallet?: string
  testnet?: boolean
  // Secrets are write-only: GET returns "" plus a configured flag.
  private_key_configured?: boolean
  funder_configured?: boolean
  api_key_configured?: boolean
  api_secret_configured?: boolean
  api_passphrase_configured?: boolean
  smart_wallet_configured?: boolean
}

export interface VenueSystemSettings {
  data_source: VenueDataSourceSettings
  live: VenueLiveSettings
  // GET /system reports the write-only secrets as flags on the venue entry.
  data_source_configured?: boolean
  live_configured?: boolean
  private_key_configured?: boolean
  api_key_configured?: boolean
  smart_wallet_configured?: boolean
}

// PUT /system accepts a per-venue patch: omitted sections keep their stored value.
export type VenueSystemPatch = Partial<Record<Venue, Partial<VenueSystemSettings>>>

export interface LiveSettings {
  private_key: string
  funder: string
  signature_type: number | null
  api_key: string
  api_secret: string
  api_passphrase: string
}

// Predict.fun live credentials: an independent EVM key (never the Polymarket
// one) plus the API key used for x-api-key auth. smart_wallet holds the funds
// and may be empty in EOA mode.
export interface PredictFunSettings {
  base_url: string
  api_key: string
  private_key: string
  smart_wallet: string
  testnet: boolean
  private_key_configured?: boolean
  api_key_configured?: boolean
  smart_wallet_configured?: boolean
}

export interface LiveProbeResult {
  ok: boolean
  error?: string
  signer?: string
  maker?: string
  api_key?: string
  source?: string
  mode?: 'eoa' | 'proxy'
  signature_type?: number
  cash?: number
  allowance?: number
  latency_ms?: number
  /** 探针面向的场所，决定余额的币种。 */
  venue?: Venue
  currency?: Currency
  testnet?: boolean
}

export interface RelayerProbeResult {
  ok: boolean
  error?: string
  signer?: string
  address?: string
  api_key?: string
  key_count?: number
  latency_ms?: number
}

export interface WalletCandidate {
  address: string
  wallet_type: number
  wallet_type_name: string
  deployed: boolean
  source: string
}

export interface FunderDiscoveryResult {
  ok: boolean
  error?: string
  signer?: string
  funder?: string
  wallet_type?: number
  wallet_type_name?: string
  deployed?: boolean
  source?: string
  wallets?: string[]
  candidates?: WalletCandidate[]
  notes?: string[]
}

export interface LiveEnvironment {
  clob_base_url: string
  polygon_rpc_url?: string
  private_key_set: boolean
  funder: string
  signature_type: number
  api_key_set: boolean
  api_secret_set: boolean
  api_passphrase_set: boolean
}

/** 链上授权状态：Predict.fun 必须授权 USDT 与 ERC1155 才能实盘交易。 */
export interface VenueApproval {
  venue: Venue
  collateral: string
  balance: number
  allowance: number
  shares_ready: boolean
  ready: boolean
  missing?: string
}

export interface VenueApprovalResponse {
  venue: string
  approval: VenueApproval
}

export interface SystemRuntime {
  gamma_base_url: string
  clob_base_url: string
  gamma_default: string
  clob_default: string
  live_environment: LiveEnvironment
  live_configured: boolean
  live_ready: boolean
  live_error: string
  error: string
  /** 逐场所运行状态；缺省时用扁平字段代表 Polymarket。 */
  venues?: SystemVenueRuntime[]
}

export interface SystemVenueRuntime {
  venue: Venue
  configured: boolean
  ready: boolean
  signer: string
  maker: string
  error: string
  testnet: boolean
}

export interface SystemEnvironment {
  http_proxy_set: boolean
  https_proxy_set: boolean
  no_proxy: string
}

export interface SystemSettings {
  proxy: ProxyConfig
  data_source: DataSourceSettings
  live: LiveSettings
  runtime: SystemRuntime
  password_set: boolean
  proxy_probe_url: string
  environment: SystemEnvironment
  /** 逐场所数据源与凭据；扁平字段等于 Polymarket（旧后端兼容）。 */
  venues?: Partial<Record<Venue, VenueSystemSettings>>
}

export interface ProxyTestResult {
  ok: boolean
  target: string
  mode: ProxyMode
  latency_ms: number
  status_code?: number
  error?: string
}

export interface AuthStatus {
  required: boolean
  authenticated: boolean
}
