import { render, screen, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from '@/lib/api'
import { useAccount } from '@/lib/account-context'
import type { Account, Dashboard, Metrics } from '@/types'
import { DashboardView } from './dashboard'

vi.mock('@/lib/api', () => ({
  api: { dashboard: vi.fn(), orders: vi.fn() },
}))

vi.mock('@/lib/account-context', () => ({ useAccount: vi.fn() }))

const account: Account = {
  id: 1,
  name: '模拟账户',
  mode: 'paper',
  venue: 'polymarket',
  currency: 'USDC',
  is_default: true,
  auto_trade: true,
  baseline_equity: 1000,
  created_at: '2026-11-01T00:00:00Z',
  live_configured: false,
  live_ready: false,
  live_signer: '',
  live_maker: '',
  live_error: '',
}

const dashboard: Dashboard = {
  account,
  metrics: {
    mode: 'paper',
    venue: 'polymarket',
    currency: 'USDC',
    initial_bankroll: 1000,
    cash: 1001,
    wallet_cash: 0,
    wallet_cash_stale: false,
    equity: 1001,
    realized_pnl: 1,
    unrealized_pnl: 0,
    total_pnl: 1,
    fees_paid: 0,
    total_return: 0.001,
    open_exposure: 0,
    open_positions: 0,
    settled_positions: 1,
    split_positions: 0,
    stopped_positions: 0,
    win_rate: 1,
    auto_trade_started_at: '2026-11-01T00:00:00Z',
    annualized_return: 0.073,
    annualized_days: 5,
    total_orders: 1,
  },
  engine: { running: true, last_scan_at: null, last_error: '', scan_count: 1, mode: 'paper' },
  source: { name: '测试行情', last_sync_at: null },
  equity: [],
}

beforeEach(() => {
  vi.mocked(useAccount).mockReturnValue({
    accounts: [account],
    account,
    accountId: account.id,
    selectAccount: vi.fn(),
    refreshAccounts: vi.fn(),
    isLoading: false,
    error: null,
  })
  vi.mocked(api.orders).mockResolvedValue({ items: [], total: 0 })
})

async function renderMetrics(overrides: Partial<Metrics> = {}) {
  vi.mocked(api.dashboard).mockResolvedValue({
    ...dashboard,
    metrics: { ...dashboard.metrics, ...overrides },
  })
  render(<DashboardView onOpenOrders={vi.fn()} />)
  await screen.findByText('暂无资金曲线')
  const card = screen.getByText('预估年化收益率').closest<HTMLElement>('div.relative')
  if (!card) throw new Error('Annualized return card is missing')
  return within(card)
}

describe('estimated annualized return', () => {
  it('shows a five-day estimate without waiting for thirty days', async () => {
    const card = await renderMetrics()
    expect(card.getByText('+7.3%', { exact: true })).toBeVisible()
    expect(card.getByText(/按 5 天线性折算/)).toBeVisible()
    expect(card.queryByText(/满 30 天才折算/)).not.toBeInTheDocument()
  })

  it.each([
    { days: 0.25, period: '6 小时' },
    { days: 5 / (24 * 60), period: '5 分钟' },
    { days: 2 / (24 * 60 * 60), period: '2 秒' },
  ])('shows a sub-day estimate over $period', async ({ days, period }) => {
    const card = await renderMetrics({ annualized_days: days, annualized_return: 0.12 })
    expect(card.getByText('+12.0%', { exact: true })).toBeVisible()
    expect(card.getByText(new RegExp(`按 ${period}线性折算`))).toBeVisible()
  })

  it.each([
    { annualized_return: 0, value: '0.0%' },
    { annualized_return: -1, value: '-100.0%' },
  ])('shows a valid short-window return of $value', async ({ annualized_return, value }) => {
    const card = await renderMetrics({ annualized_return })
    expect(card.getByText(value, { exact: true })).toBeVisible()
  })

  it.each([365, 400])('preserves compound labeling at %s days', async (days) => {
    const card = await renderMetrics({ annualized_days: days, annualized_return: 0.08 })
    expect(card.getByText('+8.0%', { exact: true })).toBeVisible()
    expect(card.getByText(new RegExp(`按 ${days} 天复利折算`))).toBeVisible()
  })

  it('withholds the estimate before the first automatic buy', async () => {
    const card = await renderMetrics({ auto_trade_started_at: null, annualized_days: 0, annualized_return: 0 })
    expect(card.getByText('—', { exact: true })).toBeVisible()
    expect(card.getByText('尚未产生自动买入')).toBeVisible()
  })

  it.each([0, -1])('withholds the estimate for a non-positive window of %s days', async (days) => {
    const card = await renderMetrics({ annualized_days: days, annualized_return: 0 })
    expect(card.getByText('—', { exact: true })).toBeVisible()
    expect(card.getByText('等待运行时长累积')).toBeVisible()
    expect(card.queryByText(/满 30 天才折算/)).not.toBeInTheDocument()
  })
})
