import { useCallback, useEffect, useRef, useState } from 'react'
import { BookOpenCheck, Bot, CalendarDays, RefreshCw, ShieldAlert, UserRound } from 'lucide-react'
import { api } from '@/lib/api'
import { useAccount } from '@/lib/account-context'
import { usePolling } from '@/hooks/usePolling'
import { useVenues } from '@/hooks/useVenues'
import type { Order, OrderStatusFilter, OrdersResponse } from '@/types'
import { formatDate, formatFullDate, formatNumber, formatPrice } from '@/lib/format'
import { fmtMoney, venueMeta, venueOf } from '@/lib/venue'
import { cn } from '@/lib/utils'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { EmptyState, ErrorState, LoadingBlock } from '@/components/ui/feedback'
import { InlineError, PageHeader } from '@/components/page-header'
import { OrderModeBadge, OrderStatusBadge, VenueChip } from '@/components/status-badges'
import { VenueFilter, useVenueScope, type VenueScope } from '@/components/venue-filter'

const PAGE_SIZE = 100

interface OrdersSnapshot {
  response: OrdersResponse
  query: { status: OrderStatusFilter; offset: number; scope: VenueScope }
  observedAt: number
}

const filters: { value: OrderStatusFilter; label: string }[] = [
  { value: 'all', label: '全部订单' },
  { value: 'filled', label: '已成交' },
  { value: 'settled', label: '已结算' },
  { value: 'unresolved', label: '待对账' },
  { value: 'rejected', label: '已拒绝' },
]

function orderKindLabel(order: Order) {
  if (order.action === 'stop') return '止损保护'
  return order.kind === 'auto' ? '自动策略' : '手动确认'
}

// Shared with the dashboard preview: a stop-loss sell (or any sell) is a sell,
// never a settlement. Settlement orders are the only ones labelled 结算.
export function orderTypeLabel(order: Pick<Order, 'type' | 'action'>, isLive: boolean) {
  // A redemption is bookkeeping on a settled position (the engine stores it as a
  // settle action named "redeem"), so it must not read as a second settlement.
  if (order.action === 'redeem') return '链上赎回'
  if (order.action === 'stop') return isLive ? '实盘止损卖出' : '模拟止损卖出'
  if (order.action === 'sell' || order.type === 'sell') return isLive ? '实盘卖出' : '模拟卖出'
  if (order.type === 'buy') return isLive ? '实盘买入' : '模拟买入'
  return '结算'
}

function OrderCard({ order, isLive }: { order: Order; isLive: boolean }) {
  const venue = venueOf(order)
  return (
    <Card className="p-4">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <p className="flex flex-wrap items-center gap-1.5 text-xs text-slate-400"><VenueChip venue={venue} />订单 #{order.id} · {orderKindLabel(order)}</p>
          <h3 className="mt-1.5 text-sm font-medium leading-6 text-slate-900">{order.question}</h3>
        </div>
        <OrderStatusBadge status={order.status} />
      </div>
      <div className="mt-3 flex flex-wrap items-center gap-2 text-xs">
        <span className="rounded-md bg-blue-50 px-2 py-1 font-medium text-blue-700">{order.outcome}</span>
        <span className="text-slate-500">{orderTypeLabel(order, isLive)}</span>
        <OrderModeBadge mode={isLive ? 'live' : 'paper'} kind={order.action === 'stop' ? 'stop' : order.kind} />
      </div>
      <div className="mt-4 grid grid-cols-3 gap-3 border-t border-slate-100 pt-4">
        <div><p className="text-[11px] text-slate-400">金额</p><p className="mt-1 text-sm font-semibold tabular-nums text-slate-800">{fmtMoney(order.amount, venue)}</p>{order.fee > 0 && <p className="mt-0.5 text-[10px] text-slate-400">含费用 {fmtMoney(order.fee, venue)}</p>}</div>
        <div><p className="text-[11px] text-slate-400">价格</p><p className="mt-1 text-sm tabular-nums text-slate-700">{formatPrice(order.price)}</p></div>
        <div><p className="text-[11px] text-slate-400">份额</p><p className="mt-1 text-sm tabular-nums text-slate-700">{formatNumber(order.shares, 4)}</p></div>
      </div>
      <div className="mt-3 flex items-start justify-between gap-3 text-[11px]">
        <div>
          <p className={cn('leading-5', order.status === 'rejected' ? 'text-rose-600' : order.status === 'unknown' || order.status === 'pending' ? 'text-amber-700' : 'text-slate-500')}>{order.reason || '无附加说明'}</p>
          {order.external_id && <p className="mt-1 break-all font-mono text-[10px] text-slate-400">{venueMeta(venue).externalIdLabel} {order.external_id}</p>}
        </div>
        <span className="shrink-0 text-slate-400">{formatDate(order.created_at)}</span>
      </div>
    </Card>
  )
}

export function OrdersView() {
  const [status, setStatus] = useState<OrderStatusFilter>('all')
  const [offset, setOffset] = useState(0)
  const { account, accountId, isLoading: accountLoading } = useAccount()
  const isLive = account?.mode === 'live'
  const { venues } = useVenues()
  const { scope, scopeRef, setScope } = useVenueScope(venues)
  // The venue filter must run on the server: paging and `data.total` are both
  // server-side, so a client-side filter would make the counter and the "next
  // page" boundary wrong. The query lives in refs and a change re-issues it via
  // refresh(), keeping the fetcher identity stable so rows are not cleared.
  const statusRef = useRef(status)
  const offsetRef = useRef(offset)
  const fetchOrders = useCallback(
    async (signal: AbortSignal): Promise<OrdersSnapshot> => {
      const query = { status: statusRef.current, offset: offsetRef.current, scope: scopeRef.current }
      const response = await api.orders(accountId, query.status, PAGE_SIZE, query.offset, query.scope === 'all' ? null : query.scope, signal)
      return { response, query, observedAt: Date.now() }
    },
    [accountId, scopeRef],
  )
  const { data: snapshot, error, isLoading, isRefreshing, refresh } = usePolling(fetchOrders, { enabled: Boolean(accountId) })
  const appliedQuery = useRef({ status, offset, scope })
  useEffect(() => {
    const previous = appliedQuery.current
    if (previous.status === status && previous.offset === offset && previous.scope === scope) return
    // Totals and page boundaries are per venue scope, so a venue change starts
    // at the first page. The ref is updated synchronously to avoid a second
    // fetch from the following render.
    if (previous.scope !== scope && offset !== 0) {
      offsetRef.current = 0
      appliedQuery.current = { status, offset: 0, scope }
      setOffset(0)
      refresh()
      return
    }
    appliedQuery.current = { status, offset, scope }
    refresh()
  }, [status, offset, scope, refresh])
  const changeStatus = (next: OrderStatusFilter) => {
    statusRef.current = next
    offsetRef.current = 0
    setStatus(next)
    setOffset(0)
  }
  const changeOffset = (next: number) => {
    // A failed page change leaves the old accepted snapshot on screen while the
    // requested offset stays pending. Clicking that same target must retry it
    // instead of being swallowed by an identical setState.
    if (offsetRef.current === next) {
      refresh()
      return
    }
    offsetRef.current = next
    setOffset(next)
  }
  const accepted = snapshot?.query
  const shownStatus = accepted?.status ?? status
  const shownScope = accepted?.scope ?? scope
  const items = snapshot?.response.items || []
  const outOfRange = Boolean(snapshot && accepted && accepted.offset > 0 && accepted.offset >= snapshot.response.total)
  const statusLabel = (value: OrderStatusFilter) => filters.find((filter) => filter.value === value)?.label || value
  const scopeLabel = (value: VenueScope) => value === 'all' ? '全部场所' : venueMeta(value).label
  const staleLabel = snapshot ? `${statusLabel(snapshot.query.status)} · ${scopeLabel(snapshot.query.scope)} · 观察于 ${formatFullDate(new Date(snapshot.observedAt).toISOString())}` : ''

  return (
    <>
      <PageHeader eyebrow={isLive ? 'LIVE ORDER LOG' : 'PAPER ORDER LOG'} title="订单记录" description={isLive ? '审计手动与自动实盘订单，包括各场所订单号、成交、结算以及风控拒绝原因。' : '审计手动与自动模拟订单，包括成交、结算以及风控拒绝原因。'} actions={<Button variant="secondary" onClick={refresh} disabled={isRefreshing}><RefreshCw className={cn('h-4 w-4', isRefreshing && 'animate-spin')} />刷新记录</Button>} />
      {error && snapshot && <InlineError message={`${error}（当前展示 ${staleLabel}）`} onRetry={refresh} />}
      <div className="mb-4 flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div className="flex w-full gap-1 overflow-x-auto rounded-xl border border-slate-200 bg-surface p-1 sm:w-auto" role="tablist" aria-label="订单状态筛选">
          {filters.map((filter) => <button key={filter.value} role="tab" aria-selected={status === filter.value} onClick={() => changeStatus(filter.value)} className={cn('h-8 flex-1 whitespace-nowrap rounded-lg px-3 text-xs font-medium transition sm:flex-none', status === filter.value ? 'bg-navy text-white shadow-sm' : 'text-slate-500 hover:bg-slate-50 hover:text-slate-800')}>{filter.label}</button>)}
        </div>
        <p className="text-xs text-slate-500">显示 <span className="font-semibold tabular-nums text-slate-800">{formatNumber(items.length, 0)}</span> 条{snapshot ? ` / 共 ${formatNumber(snapshot.response.total, 0)} 条` : ''}{snapshot && <span className="ml-1 text-slate-400">（{staleLabel}）</span>}</p>
      </div>
      <div className="mb-4">
        <VenueFilter available={venues} value={scope} onChange={setScope} />
      </div>

      {accountLoading || isLoading ? <Card><LoadingBlock rows={6} /></Card> : error && !snapshot ? <Card><ErrorState message={error} onRetry={refresh} /></Card> : outOfRange && snapshot && accepted ? (
        <Card>
          <EmptyState
            icon={BookOpenCheck}
            title="当前页码超出订单范围"
            description={`${statusLabel(accepted.status)} · ${scopeLabel(accepted.scope)} 共 ${formatNumber(snapshot.response.total, 0)} 条，当前页从第 ${formatNumber(accepted.offset + 1, 0)} 条开始。`}
            action={<Button variant="secondary" onClick={() => changeOffset(Math.max(0, accepted.offset - PAGE_SIZE))}>返回上一页</Button>}
          />
        </Card>
      ) : !items.length ? (
        <Card>
          <EmptyState
            icon={BookOpenCheck}
            title={shownStatus === 'all' ? '暂无订单记录' : '此状态下暂无订单'}
            description={shownStatus === 'all' ? (isLive ? `手动实盘买入和自动策略订单都会完整记录在这里，并附${venueMeta(shownScope === 'all' ? account?.venue : shownScope).externalIdLabel} 订单号。` : '手动模拟买入和自动策略订单都会完整记录在这里。') : '切换其他状态，或查看全部订单。'}
            action={shownStatus !== 'all' ? <Button variant="secondary" onClick={() => changeStatus('all')}>查看全部订单</Button> : undefined}
          />
        </Card>
      ) : (
        <>
          <Card className="hidden overflow-hidden lg:block">
            <div className="overflow-x-auto">
              <table className="w-full min-w-[1120px] text-left text-sm">
                <thead className="border-b border-slate-100 bg-slate-50/70 text-xs text-slate-500">
                  <tr><th className="px-5 py-3.5 font-medium">订单 / 市场</th><th className="px-4 py-3.5 font-medium">来源 / 类型</th><th className="px-4 py-3.5 text-right font-medium">金额</th><th className="px-4 py-3.5 text-right font-medium">价格 / 份额</th><th className="px-4 py-3.5 font-medium">状态 / 原因</th><th className="px-5 py-3.5 text-right font-medium">创建时间</th></tr>
                </thead>
                <tbody className={cn('divide-y divide-slate-100 transition-opacity', isRefreshing && 'opacity-60')}>
                  {items.map((order) => {
                    const venue = venueOf(order)
                    return (
                      <tr key={order.id} className="align-top hover:bg-slate-50/60">
                        <td className="max-w-[360px] px-5 py-4">
                          <p className="line-clamp-2 font-medium leading-5 text-slate-800" title={order.question}>{order.question}</p>
                          <div className="mt-1.5 flex items-center gap-2 text-xs"><VenueChip venue={venue} /><span className="font-medium text-blue-700">{order.outcome}</span><span className="text-slate-300">·</span><span className="text-slate-400">#{order.id}</span>{order.position_id !== null && <span className="text-slate-400">持仓 #{order.position_id}</span>}</div>
                        </td>
                        <td className="px-4 py-4">
                          <p className="flex items-center gap-1.5 text-slate-700">{order.action === 'stop' ? <ShieldAlert className="h-3.5 w-3.5 text-amber-500" /> : order.kind === 'auto' ? <Bot className="h-3.5 w-3.5 text-slate-400" /> : <UserRound className="h-3.5 w-3.5 text-slate-400" />}{orderKindLabel(order)}</p>
                          <p className="mt-1.5 text-xs text-slate-400">{orderTypeLabel(order, isLive)}</p>
                        </td>
                        <td className="px-4 py-4 text-right"><p className="font-medium tabular-nums text-slate-800">{fmtMoney(order.amount, venue)}</p>{order.fee > 0 && <p className="mt-1 text-[11px] text-slate-400">含费用 {fmtMoney(order.fee, venue)}</p>}</td>
                        <td className="px-4 py-4 text-right"><p className="tabular-nums text-slate-700">{formatPrice(order.price)}</p><p className="mt-1 text-xs tabular-nums text-slate-400">{formatNumber(order.shares, 4)} 份</p></td>
                        <td className="max-w-[230px] px-4 py-4">
                          <OrderStatusBadge status={order.status} />
                          <p className={cn('mt-2 line-clamp-2 text-xs leading-5', order.status === 'rejected' ? 'text-rose-600' : order.status === 'unknown' || order.status === 'pending' ? 'text-amber-700' : 'text-slate-500')} title={order.reason}>{order.reason || '无附加说明'}</p>
                          {order.external_id && <p className="mt-1 break-all font-mono text-[10px] text-slate-400">{venueMeta(venue).externalIdLabel} {order.external_id}</p>}
                        </td>
                        <td className="px-5 py-4 text-right"><span className="inline-flex items-center gap-1.5 text-xs text-slate-500"><CalendarDays className="h-3.5 w-3.5 text-slate-400" />{formatDate(order.created_at)}</span></td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>
          </Card>
          <div className="grid gap-3 sm:grid-cols-2 lg:hidden">{items.map((order) => <OrderCard key={order.id} order={order} isLive={isLive} />)}</div>
          {snapshot && snapshot.response.total > PAGE_SIZE && accepted && (
            <div className="mt-4 flex flex-col items-center justify-between gap-3 text-xs text-slate-400 sm:flex-row">
              <p>当前显示第 {formatNumber(accepted.offset + 1, 0)}–{formatNumber(accepted.offset + items.length, 0)} 条，共 {formatNumber(snapshot.response.total, 0)} 条记录 <span className="text-slate-300">·</span> {staleLabel}</p>
              <div className="flex gap-2">
                <Button variant="secondary" size="sm" disabled={accepted.offset === 0 || isRefreshing} onClick={() => changeOffset(Math.max(0, accepted.offset - PAGE_SIZE))}>上一页</Button>
                <Button variant="secondary" size="sm" disabled={accepted.offset + items.length >= snapshot.response.total || isRefreshing} onClick={() => changeOffset(accepted.offset + PAGE_SIZE)}>下一页</Button>
              </div>
            </div>
          )}
        </>
      )}
    </>
  )
}
