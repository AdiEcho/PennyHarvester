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
  Position,
  PositionStatus,
  PositionsResponse,
  ProxyConfig,
  ProxyTestResult,
  RelayerProbeResult,
  Settings,
  SystemSettings,
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

export const api = {
  health: (signal?: AbortSignal) => request<Health>('/health', { signal }),
  accounts: (signal?: AbortSignal) => request<AccountsResponse>('/accounts', { signal }),
  createAccount: (body: { name: string; mode: AccountMode }, signal?: AbortSignal) =>
    request<Account>('/accounts', { method: 'POST', body, signal }),
  updateAccount: (id: number, body: { name?: string; auto_trade?: boolean; is_default?: boolean }, signal?: AbortSignal) =>
    request<Account>(`/accounts/${id}`, { method: 'PATCH', body, signal }),
  deleteAccount: (id: number, signal?: AbortSignal) =>
    request<void>(`/accounts/${id}`, { method: 'DELETE', signal }),
  dashboard: (accountId: number | undefined, signal?: AbortSignal) =>
    request<Dashboard>(withAccount('/dashboard', accountId), { signal }),
  opportunities: (signal?: AbortSignal) => request<OpportunitiesResponse>('/opportunities', { signal }),
  positions: (accountId: number | undefined, status: PositionStatus | 'all', signal?: AbortSignal) =>
    request<PositionsResponse>(withAccount(`/positions?status=${status}`, accountId), { signal }),
  orders: (
    accountId: number | undefined,
    status: OrderStatusFilter = 'all',
    limit = 100,
    offset = 0,
    signal?: AbortSignal,
  ) => request<OrdersResponse>(withAccount(`/orders?status=${status}&limit=${limit}&offset=${offset}`, accountId), { signal }),
  settings: (signal?: AbortSignal) => request<Settings>('/settings', { signal }),
  updateSettings: (settings: Settings, signal?: AbortSignal) =>
    request<Settings>('/settings', { method: 'PUT', body: settings, signal }),
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
    body: { proxy?: ProxyConfig; data_source?: DataSourceSettings; live?: LiveSettings },
    signal?: AbortSignal,
  ) => request<SystemSettings>('/system', { method: 'PUT', body, signal }),
  testProxy: (proxy: ProxyConfig, signal?: AbortSignal) =>
    request<ProxyTestResult>('/system/proxy/test', { method: 'POST', body: { proxy }, signal }),
  testLive: (live: LiveSettings, clobBaseURL: string, signal?: AbortSignal) =>
    request<LiveProbeResult>('/system/live/test', {
      method: 'POST',
      body: { live, clob_base_url: clobBaseURL },
      signal,
    }),
  testRelayer: (
    live: LiveSettings,
    clobBaseURL: string,
    relayerAPIKey: string,
    relayerAddress: string,
    signal?: AbortSignal,
  ) =>
    request<RelayerProbeResult>('/system/relayer/test', {
      method: 'POST',
      body: {
        live,
        clob_base_url: clobBaseURL,
        relayer_api_key: relayerAPIKey,
        relayer_address: relayerAddress,
      },
      signal,
    }),
  discoverFunder: (live: LiveSettings, clobBaseURL: string, signal?: AbortSignal) =>
    request<FunderDiscoveryResult>('/system/live/funder', {
      method: 'POST',
      body: { live, clob_base_url: clobBaseURL },
      signal,
    }),
  toggleEngine: (running: boolean, signal?: AbortSignal) =>
    request<Engine>('/engine/toggle', { method: 'POST', body: { running }, signal }),
  scan: (signal?: AbortSignal, execute = false) =>
    request<OpportunitiesResponse>(`/scan${execute ? '?execute=true' : ''}`, { method: 'POST', signal }),
  createOrder: (
    accountId: number,
    body: { market_id: string; outcome: string; token_id: string; amount: number },
    signal?: AbortSignal,
  ) => request<Position>(`/accounts/${accountId}/orders`, { method: 'POST', body, signal }),
}
