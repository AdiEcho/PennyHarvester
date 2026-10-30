export type AccountMode = 'live' | 'paper'

export interface Account {
  id: number
  name: string
  mode: AccountMode
  is_default: boolean
  auto_trade: boolean
  baseline_equity: number
  created_at: string
  live_configured: boolean
  live_ready: boolean
  live_signer: string
  live_maker: string
  live_error: string
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
  win_rate: number
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
}

export interface Dashboard {
  account: Account
  metrics: Metrics
  engine: Engine
  source: Source
  equity: EquityPoint[]
}

export interface Opportunity {
  market_id: string
  condition_id: string
  question: string
  question_zh?: string
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
}

export interface OpportunitiesResponse {
  items: Opportunity[]
  total: number
  scanned_at: string | null
  source: string
}

export type PositionStatus = 'open' | 'won' | 'lost' | 'split'

export interface Position {
  id: number
  account_id: number
  market_id: string
  condition_id: string
  question: string
  outcome: string
  token_id: string
  side: 'BUY'
  kind: 'manual' | 'auto'
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
  position_id: number | null
  market_id: string
  question: string
  outcome: string
  token_id: string
  kind: 'manual' | 'auto'
  type: 'buy' | 'settle'
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

export interface Settings {
  min_price: number
  max_price: number
  max_days: number
  min_liquidity: number
  order_size: number
  max_position: number
  max_exposure: number
  scan_interval_seconds: number
  slippage_bps: number
  fee_bps: number
  initial_bankroll: number
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

export interface LiveSettings {
  private_key: string
  funder: string
  signature_type: number | null
  api_key: string
  api_secret: string
  api_passphrase: string
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

export interface FunderDiscoveryResult {
  ok: boolean
  error?: string
  wallets?: string[]
}

export interface LiveEnvironment {
  clob_base_url: string
  private_key_set: boolean
  funder: string
  signature_type: number
  api_key_set: boolean
  api_secret_set: boolean
  api_passphrase_set: boolean
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
