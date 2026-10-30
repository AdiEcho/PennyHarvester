import { useCallback, useState } from 'react'
import {
  Activity,
  ArrowDownRight,
  ArrowUpRight,
  Banknote,
  ChartNoAxesCombined,
  ChevronRight,
  CircleDollarSign,
  Clock3,
  Database,
  Layers3,
  Radio,
  RefreshCw,
  ShieldAlert,
  Table2,
  TrendingUp,
  Wallet,
} from 'lucide-react'
import type { LucideIcon } from 'lucide-react'
import {
  Area,
  CartesianGrid,
  Line,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
  ComposedChart,
} from 'recharts'
import { api } from '@/lib/api'
import { useAccount } from '@/lib/account-context'
import { usePolling } from '@/hooks/usePolling'
import type { AccountMode, EquityPoint, Metrics } from '@/types'
import { formatAxisDate, formatAxisCurrency, formatCurrency, formatDate, formatFullDate, formatNumber, formatPercent, pnlClass } from '@/lib/format'
import { cn } from '@/lib/utils'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { EmptyState, ErrorState, Skeleton } from '@/components/ui/feedback'
import { InlineError, PageHeader } from '@/components/page-header'
import { OrderModeBadge, OrderStatusBadge } from '@/components/status-badges'

function StatCard({ label, value, detail, icon: Icon, accent, loading }: {
  label: string
  value: string
  detail: React.ReactNode
  icon: LucideIcon
  accent?: boolean
  loading: boolean
}) {
  return (
    <Card className="relative overflow-hidden p-5">
      <div className="flex items-start justify-between gap-3">
        <p className="text-sm font-medium text-slate-500">{label}</p>
        <div className={cn('rounded-xl p-2', accent ? 'bg-emerald-50 text-emerald-700' : 'bg-slate-100 text-slate-500')}>
          <Icon className="h-[18px] w-[18px]" strokeWidth={1.8} />
        </div>
      </div>
      {loading ? <Skeleton className="mt-3 h-8 w-36" /> : <p className="mt-2 text-[26px] font-semibold tracking-[-0.035em] text-slate-950 sm:text-[28px]">{value}</p>}
      <div className="mt-2 min-h-5 text-xs leading-5 text-slate-500">{loading ? <Skeleton className="h-4 w-28" /> : detail}</div>
    </Card>
  )
}

function EquityTooltip({ active, payload, mode }: { active?: boolean; payload?: Array<{ payload: EquityPoint }>; mode: AccountMode }) {
  if (!active || !payload?.length) return null
  const point = payload[0].payload
  return (
    <div className="min-w-[170px] rounded-xl border border-slate-200 bg-surface/95 p-3.5 shadow-float backdrop-blur">
      <p className="text-xs text-slate-500">{formatFullDate(point.time)}</p>
      <div className="mt-2 flex items-center justify-between gap-5">
        <span className="flex items-center gap-2 text-xs text-slate-500"><span className="h-0.5 w-4 rounded bg-[var(--series-equity)]" />{mode === 'live' ? '实盘权益' : '模拟权益'}</span>
        <span className="text-sm font-semibold text-slate-900">{formatCurrency(point.value)}</span>
      </div>
    </div>
  )
}

function EquityChart({ data, refreshing, mode }: { data: EquityPoint[]; refreshing: boolean; mode: AccountMode }) {
  const [view, setView] = useState<'chart' | 'table'>('chart')
  const label = mode === 'live' ? '实盘权益' : '模拟权益'

  return (
    <Card className="min-w-0 xl:col-span-2">
      <CardHeader className="flex-col gap-4 space-y-0 sm:flex-row sm:items-start sm:justify-between">
        <div>
          <CardTitle>权益曲线</CardTitle>
          <CardDescription className="mt-1">{label} · USD · {data.length} 个记录点</CardDescription>
        </div>
        <div className="flex shrink-0 self-start rounded-lg border border-slate-200 bg-slate-50 p-1" aria-label="权益数据展示方式">
          <button className={cn('flex h-7 items-center gap-1.5 whitespace-nowrap rounded-md px-2.5 text-xs transition', view === 'chart' ? 'bg-surface font-medium text-slate-800 shadow-sm' : 'text-slate-500 hover:text-slate-800')} onClick={() => setView('chart')} aria-pressed={view === 'chart'}>
            <ChartNoAxesCombined className="h-3.5 w-3.5" />图表
          </button>
          <button className={cn('flex h-7 items-center gap-1.5 whitespace-nowrap rounded-md px-2.5 text-xs transition', view === 'table' ? 'bg-surface font-medium text-slate-800 shadow-sm' : 'text-slate-500 hover:text-slate-800')} onClick={() => setView('table')} aria-pressed={view === 'table'}>
            <Table2 className="h-3.5 w-3.5" />数据
          </button>
        </div>
      </CardHeader>
      <CardContent>
        {data.length === 0 ? (
          <EmptyState compact icon={TrendingUp} title="暂无资金曲线" description="完成首次市场扫描或产生交易后，这里会自动绘出你的账户净值变化趋势。" />
        ) : view === 'table' ? (
          <div className="scrollbar-thin max-h-[310px] overflow-auto rounded-xl border border-slate-200">
            <table className="w-full text-left text-sm">
              <caption className="sr-only">{label}历史数据</caption>
              <thead className="sticky top-0 bg-slate-50 text-xs text-slate-500">
                <tr><th className="px-4 py-3 font-medium">记录时间</th><th className="px-4 py-3 text-right font-medium">{label}</th></tr>
              </thead>
              <tbody className="divide-y divide-slate-100">
                {[...data].reverse().map((point, index) => (
                  <tr key={`${point.time}-${index}`}><td className="px-4 py-3 text-slate-600">{formatFullDate(point.time)}</td><td className="px-4 py-3 text-right font-medium tabular-nums text-slate-900">{formatCurrency(point.value)}</td></tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <div className={cn('h-[310px] w-full transition-opacity', refreshing && 'opacity-60')} role="img" aria-label={`${label}随时间变化的折线图；也可切换为数据表查看`}>
            <ResponsiveContainer width="100%" height="100%">
              <ComposedChart data={data} margin={{ top: 12, right: 8, left: 0, bottom: 0 }}>
                <defs>
                  <linearGradient id="equityArea" x1="0" y1="0" x2="0" y2="1">
                    <stop offset="0%" stopColor="var(--series-equity)" stopOpacity={0.12} />
                    <stop offset="100%" stopColor="var(--series-equity)" stopOpacity={0.01} />
                  </linearGradient>
                </defs>
                <CartesianGrid vertical={false} stroke="var(--chart-grid)" strokeWidth={1} />
                <XAxis dataKey="time" tickFormatter={formatAxisDate} tickLine={false} axisLine={false} minTickGap={42} tickMargin={12} tick={{ fill: 'var(--chart-muted)', fontSize: 11 }} />
                <YAxis tickFormatter={formatAxisCurrency} tickLine={false} axisLine={false} width={82} tickCount={5} domain={['auto', 'auto']} tick={{ fill: 'var(--chart-muted)', fontSize: 11 }} />
                <Tooltip content={<EquityTooltip mode={mode} />} cursor={{ stroke: 'var(--chart-muted)', strokeWidth: 1 }} isAnimationActive={false} />
                <Area type="monotone" dataKey="value" stroke="none" fill="url(#equityArea)" isAnimationActive={false} />
                <Line type="monotone" dataKey="value" name={label} stroke="var(--series-equity)" strokeWidth={2} dot={data.length === 1 ? { r: 4, fill: 'var(--series-equity)', stroke: 'var(--chart-surface)', strokeWidth: 2 } : false} activeDot={{ r: 5, fill: 'var(--series-equity)', stroke: 'var(--chart-surface)', strokeWidth: 2 }} isAnimationActive={false} />
              </ComposedChart>
            </ResponsiveContainer>
          </div>
        )}
      </CardContent>
    </Card>
  )
}

function MetricsCards({ metrics, loading }: { metrics: Metrics | null; loading: boolean }) {
  const isLive = metrics?.mode === 'live'
  return (
    <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
      <StatCard
        label={isLive ? '实盘账户权益' : '模拟账户权益'}
        value={metrics ? formatCurrency(metrics.equity) : '—'}
        icon={CircleDollarSign}
        accent
        loading={loading}
        detail={metrics ? <span className={cn('inline-flex items-center gap-1 font-medium', pnlClass(metrics.total_return))}>{metrics.total_return >= 0 ? <ArrowUpRight className="h-3.5 w-3.5" /> : <ArrowDownRight className="h-3.5 w-3.5" />}{formatPercent(metrics.total_return, 2, true)} <span className="font-normal text-slate-400">较{isLive ? '初始基准' : '初始本金'}</span></span> : '正在加载账户数据'}
      />
      <StatCard
        label={isLive ? '策略可用资金' : '可用虚拟资金'}
        value={metrics ? formatCurrency(metrics.cash) : '—'}
        icon={Wallet}
        loading={loading}
        detail={metrics ? (isLive
          ? <span>钱包 USDC <span className="font-medium tabular-nums text-slate-700">{formatCurrency(metrics.wallet_cash)}</span>{metrics.wallet_cash_stale && <span className="text-amber-600"> · 暂显上次余额</span>}</span>
          : `初始本金 ${formatCurrency(metrics.initial_bankroll)}`) : '正在加载账户数据'}
      />
      <StatCard
        label="累计盈亏"
        value={metrics ? formatCurrency(metrics.total_pnl, true) : '—'}
        icon={Banknote}
        loading={loading}
        detail={metrics ? <span>已实现 <span className={pnlClass(metrics.realized_pnl)}>{formatCurrency(metrics.realized_pnl, true)}</span> · 浮动 <span className={pnlClass(metrics.unrealized_pnl)}>{formatCurrency(metrics.unrealized_pnl, true)}</span> · 手续费 <span className="font-medium tabular-nums text-slate-700">{formatCurrency(metrics.fees_paid)}</span></span> : '正在加载账户数据'}
      />
      <StatCard
        label="当前持仓投入"
        value={metrics ? formatCurrency(metrics.open_exposure) : '—'}
        icon={Layers3}
        loading={loading}
        detail={metrics ? `${formatNumber(metrics.open_positions, 0)} 个持有中仓位` : '正在加载账户数据'}
      />
    </div>
  )
}

export function DashboardView({ onOpenOrders }: { onOpenOrders: () => void }) {
  const { account, accountId, isLoading: accountLoading } = useAccount()
  const fetchDashboard = useCallback((signal: AbortSignal) => api.dashboard(accountId, signal), [accountId])
  const fetchOrders = useCallback((signal: AbortSignal) => api.orders(accountId, 'all', 6, 0, signal), [accountId])
  const dashboard = usePolling(fetchDashboard, { enabled: Boolean(accountId) })
  const orders = usePolling(fetchOrders, { enabled: Boolean(accountId) })
  const data = dashboard.data
  const mode: AccountMode = account?.mode || 'paper'
  const isLive = mode === 'live'
  const loading = accountLoading || (Boolean(accountId) && dashboard.isLoading)

  return (
    <>
      <PageHeader
        eyebrow={isLive ? 'LIVE PORTFOLIO' : 'PAPER PORTFOLIO'}
        title={`${account?.name || '账户'}总览`}
        description={isLive
          ? '实时掌握实盘资金、持仓盈亏与系统状态。当前账户的买单会直接通过真实钱包提交到 Polymarket。'
          : '使用独立虚拟资金同步真实行情，零成本验证策略表现，不会发出任何真实订单。'}
        actions={<Button variant="secondary" onClick={() => { dashboard.refresh(); orders.refresh() }} disabled={dashboard.isRefreshing}><RefreshCw className={cn('h-4 w-4', dashboard.isRefreshing && 'animate-spin')} />刷新数据</Button>}
      />
      {dashboard.error && data && <InlineError message={dashboard.error} onRetry={dashboard.refresh} />}
      {dashboard.error && !data && <div className="mb-5"><InlineError message={dashboard.error} onRetry={dashboard.refresh} hasData={false} /></div>}

      <MetricsCards metrics={data?.metrics || null} loading={loading} />

      <div className="mt-4 grid gap-4 xl:grid-cols-3">
        {loading ? <Card className="xl:col-span-2"><div className="p-6"><Skeleton className="h-5 w-32" /><Skeleton className="mt-8 h-[260px] w-full" /></div></Card> : data ? <EquityChart data={data.equity} refreshing={dashboard.isRefreshing} mode={mode} /> : <Card className="xl:col-span-2"><ErrorState message={dashboard.error || '暂无总览数据。'} onRetry={dashboard.refresh} /></Card>}

        <Card>
          <CardHeader className="flex-row items-start justify-between space-y-0">
            <div><CardTitle>系统状态</CardTitle><CardDescription className="mt-1">后台自动扫描与行情连接情况</CardDescription></div>
            {data && <Badge variant={data.engine.running ? 'success' : 'default'}><span className={cn('h-1.5 w-1.5 rounded-full', data.engine.running ? 'bg-emerald-500' : 'bg-slate-400')} />{data.engine.running ? '运行中' : '已暂停'}</Badge>}
          </CardHeader>
          <CardContent>
            {loading ? <div className="space-y-4"><Skeleton className="h-16 w-full" /><Skeleton className="h-16 w-full" /><Skeleton className="h-16 w-full" /></div> : data ? (
              <div className="space-y-1">
                <div className="flex items-center gap-3 rounded-xl bg-slate-50 p-3.5">
                  <div className={cn('rounded-lg p-2', data.engine.running ? 'bg-emerald-100 text-emerald-700' : 'bg-slate-200 text-slate-500')}><Activity className="h-4 w-4" /></div>
                  <div className="min-w-0 flex-1"><p className="text-xs text-slate-500">市场扫描引擎</p><p className="mt-0.5 text-sm font-medium text-slate-800">{data.engine.running ? '自动扫描已开启' : '自动扫描已暂停'}</p></div>
                  <span className="text-xs tabular-nums text-slate-500">已扫 {formatNumber(data.engine.scan_count, 0)} 次</span>
                </div>
                <div className="flex items-center gap-3 rounded-xl p-3.5">
                  <div className={cn('rounded-lg p-2', isLive ? 'bg-amber-50 text-amber-700' : 'bg-blue-50 text-blue-600')}>{isLive ? <Radio className="h-4 w-4" /> : <Database className="h-4 w-4" />}</div>
                  <div className="min-w-0 flex-1"><p className="text-xs text-slate-500">{isLive ? '实盘钱包' : '当前账户'}</p><p className="mt-0.5 truncate text-sm font-medium text-slate-800" title={data.account.live_maker || data.account.live_signer || undefined}>{isLive ? (data.account.live_ready ? (data.account.live_maker || data.account.live_signer || '已就绪') : data.account.live_configured ? '配置待完善' : '尚未配置私钥') : data.account.name}</p></div>
                  {isLive && <Badge variant={data.account.live_ready ? 'success' : data.account.live_configured ? 'warning' : 'danger'}>{data.account.live_ready ? '已就绪' : data.account.live_configured ? '待完善' : '未配置'}</Badge>}
                </div>
                <div className="flex items-center gap-3 rounded-xl p-3.5">
                  <div className="rounded-lg bg-blue-50 p-2 text-blue-600"><Database className="h-4 w-4" /></div>
                  <div className="min-w-0 flex-1"><p className="text-xs text-slate-500">行情数据源</p><p className="mt-0.5 truncate text-sm font-medium text-slate-800" title={data.source.name}>{data.source.name || '默认行情源'}</p></div>
                </div>
                <dl className="space-y-3 border-t border-slate-100 px-1 pt-4 text-xs">
                  <div className="flex items-center justify-between gap-4"><dt className="flex items-center gap-1.5 text-slate-500"><Clock3 className="h-3.5 w-3.5" />最近扫描</dt><dd className="text-slate-700">{formatDate(data.engine.last_scan_at)}</dd></div>
                  <div className="flex items-center justify-between gap-4"><dt className="flex items-center gap-1.5 text-slate-500"><RefreshCw className="h-3.5 w-3.5" />最近同步</dt><dd className="text-slate-700">{formatDate(data.source.last_sync_at)}</dd></div>
                  <div className="flex items-center justify-between gap-4"><dt className="text-slate-500">已结算 / 胜率</dt><dd className="text-slate-700">{formatNumber(data.metrics.settled_positions, 0)} 笔{data.metrics.split_positions > 0 ? `（含 ${formatNumber(data.metrics.split_positions, 0)} 笔平局）` : ''} / {data.metrics.settled_positions - data.metrics.split_positions > 0 ? formatPercent(data.metrics.win_rate, 1) : '—'}</dd></div>
                </dl>
                {isLive && !data.account.live_ready && (
                  <div className="mt-4 flex items-start gap-2 rounded-lg border border-amber-200 bg-amber-50 p-3 text-xs leading-5 text-amber-800">
                    <ShieldAlert className="mt-0.5 h-3.5 w-3.5 shrink-0" />
                    <span>{data.account.live_configured ? <>实盘配置待完善：{data.account.live_error || '请检查钱包、Funder 与 API 凭据设置。'}</> : <>实盘尚未配置：在「系统设置 → 实盘交易凭据」中填写私钥，或设置 <code className="rounded bg-amber-100 px-1">POLYMARKET_PRIVATE_KEY</code> 后重启服务。</>}</span>
                  </div>
                )}
                {data.account.live_error && data.account.live_ready && <div className="mt-4 break-words rounded-lg border border-rose-100 bg-rose-50 p-3 text-xs leading-5 text-rose-700"><span className="font-medium">实盘错误：</span>{data.account.live_error}</div>}
                {data.engine.last_error && <div className="mt-4 break-words rounded-lg border border-rose-100 bg-rose-50 p-3 text-xs leading-5 text-rose-700"><span className="font-medium">引擎错误：</span>{data.engine.last_error}</div>}
              </div>
            ) : <ErrorState compact message={dashboard.error || '暂无状态数据。'} onRetry={dashboard.refresh} />}
          </CardContent>
        </Card>
      </div>

      <Card className="mt-4 overflow-hidden">
        <CardHeader className="flex-row items-center justify-between space-y-0">
          <div><CardTitle>最近订单</CardTitle><CardDescription className="mt-1">{isLive ? '最近的实盘买入与到期结算动态' : '最近的模拟买入与到期结算动态'}</CardDescription></div>
          <Button variant="ghost" size="sm" onClick={onOpenOrders}>查看全部<ChevronRight className="h-3.5 w-3.5" /></Button>
        </CardHeader>
        {orders.isLoading ? <div className="space-y-3 px-6 pb-6">{[1, 2, 3].map((item) => <Skeleton key={item} className="h-12 w-full" />)}</div> : orders.error && !orders.data ? <ErrorState compact message={orders.error} onRetry={orders.refresh} /> : !orders.data?.items.length ? <EmptyState compact title="还没有交易记录" description={isLive ? '去「机会雷达」挑选市场下单，或开启自动交易后，订单记录就会显示在这里。' : '去「机会雷达」试下一笔模拟单，或开启自动交易后，订单记录就会显示在这里。'} /> : (
          <>
            <div className="hidden overflow-x-auto sm:block">
              <table className="w-full min-w-[720px] text-left text-sm">
                <thead className="border-y border-slate-100 bg-slate-50/70 text-xs text-slate-500"><tr><th className="px-6 py-3 font-medium">市场 / 方向</th><th className="px-4 py-3 font-medium">类型</th><th className="px-4 py-3 text-right font-medium">金额</th><th className="px-4 py-3 font-medium">状态</th><th className="px-6 py-3 text-right font-medium">时间</th></tr></thead>
                <tbody className="divide-y divide-slate-100">
                  {orders.data.items.map((order) => <tr key={order.id} className="hover:bg-slate-50/60"><td className="max-w-[340px] px-6 py-3.5"><p className="truncate font-medium text-slate-800" title={order.question}>{order.question}</p><p className="mt-1 text-xs text-slate-500">{order.outcome} · {order.type === 'buy' ? (isLive ? '实盘买入' : '模拟买入') : '结算'}</p></td><td className="px-4 py-3.5"><OrderModeBadge mode={mode} kind={order.kind} /></td><td className="px-4 py-3.5 text-right font-medium tabular-nums text-slate-800">{formatCurrency(order.amount)}</td><td className="px-4 py-3.5"><OrderStatusBadge status={order.status} /></td><td className="px-6 py-3.5 text-right text-xs text-slate-500">{formatDate(order.created_at)}</td></tr>)}
                </tbody>
              </table>
            </div>
            <div className="divide-y divide-slate-100 sm:hidden">
              {orders.data.items.map((order) => <div key={order.id} className="px-5 py-4"><div className="flex items-start justify-between gap-3"><div className="min-w-0"><p className="line-clamp-2 text-sm font-medium leading-5 text-slate-800">{order.question}</p><p className="mt-1 text-xs text-slate-500">{order.outcome} · {order.type === 'buy' ? (isLive ? '实盘买入' : '模拟买入') : '结算'}</p></div><span className="shrink-0 text-sm font-semibold tabular-nums text-slate-800">{formatCurrency(order.amount)}</span></div><div className="mt-3 flex flex-wrap items-center gap-2"><OrderModeBadge mode={mode} kind={order.kind} /><OrderStatusBadge status={order.status} /><span className="ml-auto text-[11px] text-slate-400">{formatDate(order.created_at)}</span></div></div>)}
            </div>
          </>
        )}
      </Card>
    </>
  )
}
