import type {
  Account,
  AccountMode,
  AccountsResponse,
  AuthStatus,
  Dashboard,
  DataSourceSettings,
  Engine,
  FunderDiscoveryResult,
  Health,
  LiveProbeResult,
  LiveSettings,
  OpportunitiesResponse,
  OrderStatusFilter,
  OrdersResponse,
  OpportunityQuote,
  Position,
  PositionStatus,
  PositionsResponse,
  ProxyConfig,
  ProxyTestResult,
  RelayerProbeResult,
  Settings,
  SystemSettings,
  Venue,
  VenuesResponse,
  VenueLiveSettings,
  VenueSystemPatch,
  VenueApprovalResponse,
  VenueRedemption,
} from '@/types'

const API_BASE = import.meta.env.VITE_API_BASE_URL || '/api/v1'

export class ApiError extends Error {
  status: number

  constructor(message: string, status: number) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

interface RequestOptions {
  method?: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE'
  body?: unknown
  signal?: AbortSignal
}

async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  let response: Response
  try {
    response = await fetch(`${API_BASE}${path}`, {
      method: options.method || 'GET',
      credentials: 'same-origin',
      headers: {
        Accept: 'application/json',
        ...(options.body !== undefined ? { 'Content-Type': 'application/json' } : {}),
      },
      body: options.body !== undefined ? JSON.stringify(options.body) : undefined,
      signal: options.signal,
    })
  } catch (error) {
    if (error instanceof DOMException && error.name === 'AbortError') throw error
    throw new ApiError('无法连接后端服务，请确认服务已启动后重试。', 0)
  }

  const contentType = response.headers.get('content-type') || ''
  const payload = contentType.includes('application/json')
    ? await response.json().catch(() => null)
    : await response.text().catch(() => '')

  if (!response.ok) {
    if (response.status === 401 && !path.startsWith('/auth/')) {
      window.dispatchEvent(new Event('penny:unauthorized'))
    }
    const message =
      payload && typeof payload === 'object' && 'error' in payload && typeof payload.error === 'string'
        ? payload.error
        : `请求失败（HTTP ${response.status}），请稍后重试。`
    throw new ApiError(message, response.status)
  }

  return payload as T
}

function withAccount(path: string, accountId?: number): string {
  if (!accountId) return path
  return `${path}${path.includes('?') ? '&' : '?'}account_id=${accountId}`
}

// A venue filter is only appended when one is selected. The UI's "all" scope
// maps to null, so the request stays untouched and the backend returns both
// venues; the literal "all" is never sent as a venue value.
function withVenue(path: string, venue?: Venue | null): string {
  if (!venue) return path
  return `${path}${path.includes('?') ? '&' : '?'}venue=${encodeURIComponent(venue)}`
}

export const api = {
  health: (signal?: AbortSignal) => request<Health>('/health', { signal }),
  accounts: (signal?: AbortSignal) => request<AccountsResponse>('/accounts', { signal }),
  createAccount: (body: { name: string; mode: AccountMode; venue?: Venue; initial_bankroll?: number }, signal?: AbortSignal) =>
    request<Account>('/accounts', { method: 'POST', body, signal }),
  updateAccount: (id: number, body: { name?: string; auto_trade?: boolean; is_default?: boolean }, signal?: AbortSignal) =>
    request<Account>(`/accounts/${id}`, { method: 'PATCH', body, signal }),
  deleteAccount: (id: number, signal?: AbortSignal) =>
    request<void>(`/accounts/${id}`, { method: 'DELETE', signal }),
  dashboard: (accountId: number | undefined, signal?: AbortSignal) =>
    request<Dashboard>(withAccount('/dashboard', accountId), { signal }),
  getVenues: (signal?: AbortSignal) =>
    request<VenuesResponse>('/venues', { signal }).then((response) => response?.venues ?? []),
  opportunities: (venue?: Venue | null, signal?: AbortSignal, accountId?: number) =>
    request<OpportunitiesResponse>(withVenue(withAccount('/opportunities', accountId), venue), { signal }),
  quoteOpportunity: (tokenId: string, amount?: number, venue?: Venue | null, signal?: AbortSignal, accountId?: number) =>
    request<OpportunityQuote>(withAccount('/opportunities/quote', accountId), {
      method: 'POST',
      body: {
        token_id: tokenId,
        ...(amount && amount > 0 ? { amount } : {}),
        // Token ids are venue-local, so the quote must resolve inside one venue.
        ...(venue ? { venue } : {}),
      },
      signal,
    }),
  positions: (
    accountId: number | undefined,
    status: PositionStatus | 'all',
    venue?: Venue | null,
    signal?: AbortSignal,
  ) => request<PositionsResponse>(withVenue(withAccount(`/positions?status=${status}`, accountId), venue), { signal }),
  orders: (
    accountId: number | undefined,
    status: OrderStatusFilter = 'all',
    limit = 100,
    offset = 0,
    venue?: Venue | null,
    signal?: AbortSignal,
  ) => request<OrdersResponse>(withVenue(withAccount(`/orders?status=${status}&limit=${limit}&offset=${offset}`, accountId), venue), { signal }),
  settings: (signal?: AbortSignal, accountId?: number) =>
    request<Settings>(withAccount('/settings', accountId), { signal }),
  updateSettings: (settings: Settings, signal?: AbortSignal, accountId?: number) =>
    request<Settings>(withAccount('/settings', accountId), { method: 'PUT', body: settings, signal }),
  updateScanInterval: (seconds: number, signal?: AbortSignal) =>
    request<{ scan_interval_seconds: number }>('/settings/scan-interval', {
      method: 'PUT',
      body: { scan_interval_seconds: seconds },
      signal,
    }),
  authStatus: (signal?: AbortSignal) => request<AuthStatus>('/auth/status', { signal }),
  login: (password: string, signal?: AbortSignal) =>
    request<void>('/auth/login', { method: 'POST', body: { password }, signal }),
  logout: (signal?: AbortSignal) => request<void>('/auth/logout', { method: 'POST', signal }),
  changePassword: (body: { current_password: string; new_password: string }, signal?: AbortSignal) =>
    request<void>('/auth/password', { method: 'POST', body, signal }),
  disablePassword: (currentPassword: string, signal?: AbortSignal) =>
    request<void>('/auth/password/disable', { method: 'POST', body: { current_password: currentPassword }, signal }),
  system: (signal?: AbortSignal) => request<SystemSettings>('/system', { signal }),
  updateSystem: (
    body: {
      proxy?: ProxyConfig
      data_source?: DataSourceSettings
      live?: LiveSettings
      venues?: VenueSystemPatch
    },
    signal?: AbortSignal,
  ) => request<SystemSettings>('/system', { method: 'PUT', body, signal }),
  testProxy: (proxy: ProxyConfig, signal?: AbortSignal) =>
    request<ProxyTestResult>('/system/proxy/test', { method: 'POST', body: { proxy }, signal }),
  testLive: (live: LiveSettings | VenueLiveSettings, clobBaseURL: string, venue?: Venue | null, signal?: AbortSignal) =>
    request<LiveProbeResult>(withVenue('/system/live/test', venue), {
      method: 'POST',
      body: { live, clob_base_url: clobBaseURL },
      signal,
    }),
  /** 只读读取链上授权状态（不发交易）。 */
  redeemPosition: (accountId: number, positionId: number, signal?: AbortSignal) =>
    request<VenueRedemption>(`/accounts/${accountId}/positions/${positionId}/redeem`, {
      method: 'POST',
      signal,
    }),
  getVenueApproval: (venue: Venue, signal?: AbortSignal) =>
    request<VenueApprovalResponse>(withVenue('/system/live/approval', venue), { signal }),
  /**
   * 发送缺失的授权交易。这是真的链上交易：钱包需要 BNB 支付 gas，
   * 因此只在用户显式点击时调用。
   */
  approveVenue: (venue: Venue, signal?: AbortSignal) =>
    request<VenueApprovalResponse>(withVenue('/system/live/approve', venue), { method: 'POST', signal }),
  testRelayer: (
    live: LiveSettings | VenueLiveSettings,
    clobBaseURL: string,
    relayerAPIKey: string,
    relayerAddress: string,
    venue?: Venue | null,
    signal?: AbortSignal,
  ) =>
    request<RelayerProbeResult>(withVenue('/system/relayer/test', venue), {
      method: 'POST',
      body: {
        live,
        clob_base_url: clobBaseURL,
        relayer_api_key: relayerAPIKey,
        relayer_address: relayerAddress,
      },
      signal,
    }),
  detectWallet: (live: LiveSettings, clobBaseURL: string, signal?: AbortSignal) =>
    request<FunderDiscoveryResult>('/system/live/funder', {
      method: 'POST',
      body: { live, clob_base_url: clobBaseURL },
      signal,
    }),
  toggleEngine: (running: boolean, signal?: AbortSignal) =>
    request<Engine>('/engine/toggle', { method: 'POST', body: { running }, signal }),
  scan: (signal?: AbortSignal, execute = false, accountId?: number, venue?: Venue | null) => {
    const params = new URLSearchParams()
    if (execute) params.set('execute', 'true')
    if (accountId) params.set('account_id', String(accountId))
    if (venue) params.set('venue', venue)
    const query = params.toString()
    return request<OpportunitiesResponse>(`/scan${query ? `?${query}` : ''}`, { method: 'POST', signal })
  },
  createOrder: (
    accountId: number,
    body: { market_id: string; outcome: string; token_id: string; amount: number },
    signal?: AbortSignal,
  ) => request<Position>(`/accounts/${accountId}/orders`, { method: 'POST', body, signal }),
}
