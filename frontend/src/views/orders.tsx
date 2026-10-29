import { useCallback, useState } from 'react'
import { BookOpenCheck, Bot, CalendarDays, RefreshCw, UserRound } from 'lucide-react'
import { api } from '@/lib/api'
import { useAccount } from '@/lib/account-context'
import { usePolling } from '@/hooks/usePolling'
import type { Order, OrderStatusFilter } from '@/types'
import { formatCurrency, formatDate, formatNumber, formatPrice } from '@/lib/format'
import { cn } from '@/lib/utils'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { EmptyState, ErrorState, LoadingBlock } from '@/components/ui/feedback'
import { InlineError, PageHeader } from '@/components/page-header'
import { OrderModeBadge, OrderStatusBadge } from '@/components/status-badges'

const PAGE_SIZE = 100

const filters: { value: OrderStatusFilter; label: string }[] = [
  { value: 'all', label: '全部订单' },
  { value: 'filled', label: '已成交' },
  { value: 'settled', label: '已结算' },
  { value: 'unresolved', label: '待对账' },
  { value: 'rejected', label: '已拒绝' },
]

function OrderCard({ order, isLive }: { order: Order; isLive: boolean }) {
  return (
    <Card className="p-4">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <p className="text-xs text-slate-400">订单 #{order.id} · {order.kind === 'auto' ? '自动策略' : '手动确认'}</p>
          <h3 className="mt-1.5 text-sm font-medium leading-6 text-slate-900">{order.question}</h3>
        </div>
        <OrderStatusBadge status={order.status} />
      </div>
      <div className="mt-3 flex flex-wrap items-center gap-2 text-xs">
        <span className="rounded-md bg-blue-50 px-2 py-1 font-medium text-blue-700">{order.outcome}</span>
        <span className="text-slate-500">{order.type === 'buy' ? (isLive ? '实盘买入' : '模拟买入') : '结算'}</span>
        <OrderModeBadge mode={isLive ? 'live' : 'paper'} kind={order.kind} />
      </div>
      <div className="mt-4 grid grid-cols-3 gap-3 border-t border-slate-100 pt-4">
        <div><p className="text-[11px] text-slate-400">金额</p><p className="mt-1 text-sm font-semibold tabular-nums text-slate-800">{formatCurrency(order.amount)}</p></div>
        <div><p className="text-[11px] text-slate-400">价格</p><p className="mt-1 text-sm tabular-nums text-slate-700">{formatPrice(order.price)}</p></div>
        <div><p className="text-[11px] text-slate-400">份额</p><p className="mt-1 text-sm tabular-nums text-slate-700">{formatNumber(order.shares, 4)}</p></div>
      </div>
      <div className="mt-3 flex items-start justify-between gap-3 text-[11px]">
        <div>
          <p className={cn('leading-5', order.status === 'rejected' ? 'text-rose-600' : order.status === 'unknown' || order.status === 'pending' ? 'text-amber-700' : 'text-slate-500')}>{order.reason || '无附加说明'}</p>
          {order.external_id && <p className="mt-1 break-all font-mono text-[10px] text-slate-400">CLOB {order.external_id}</p>}
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
  const fetchOrders = useCallback(
    (signal: AbortSignal) => api.orders(accountId, status, PAGE_SIZE, offset, signal),
    [accountId, status, offset],
  )
  const { data, error, isLoading, isRefreshing, refresh } = usePolling(fetchOrders, { enabled: Boolean(accountId) })
  const items = data?.items || []

  return (
    <>
      <PageHeader eyebrow={isLive ? 'LIVE ORDER LOG' : 'PAPER ORDER LOG'} title="订单记录" description={isLive ? '审计手动与自动实盘订单，包括 Polymarket CLOB 订单号、成交、结算以及风控拒绝原因。' : '审计手动与自动模拟订单，包括成交、结算以及风控拒绝原因。'} actions={<Button variant="secondary" onClick={refresh} disabled={isRefreshing}><RefreshCw className={cn('h-4 w-4', isRefreshing && 'animate-spin')} />刷新记录</Button>} />
      {error && data && <InlineError message={error} onRetry={refresh} />}
      <div className="mb-4 flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div className="flex w-full gap-1 overflow-x-auto rounded-xl border border-slate-200 bg-surface p-1 sm:w-auto" role="tablist" aria-label="订单状态筛选">
          {filters.map((filter) => <button key={filter.value} role="tab" aria-selected={status === filter.value} onClick={() => { setStatus(filter.value); setOffset(0) }} className={cn('h-8 flex-1 whitespace-nowrap rounded-lg px-3 text-xs font-medium transition sm:flex-none', status === filter.value ? 'bg-navy text-white shadow-sm' : 'text-slate-500 hover:bg-slate-50 hover:text-slate-800')}>{filter.label}</button>)}
        </div>
        <p className="text-xs text-slate-500">显示 <span className="font-semibold tabular-nums text-slate-800">{formatNumber(items.length, 0)}</span> 条{data ? ` / 共 ${formatNumber(data.total, 0)} 条` : ''}</p>
      </div>

      {accountLoading || isLoading ? <Card><LoadingBlock rows={6} /></Card> : error && !data ? <Card><ErrorState message={error} onRetry={refresh} /></Card> : !items.length ? (
        <Card>
          <EmptyState
            icon={BookOpenCheck}
            title={status === 'all' ? '暂无订单记录' : '此状态下暂无订单'}
            description={status === 'all' ? (isLive ? '手动实盘买入和自动策略订单都会完整记录在这里，并附 CLOB 订单号。' : '手动模拟买入和自动策略订单都会完整记录在这里。') : '切换其他状态，或查看全部订单。'}
            action={status !== 'all' ? <Button variant="secondary" onClick={() => { setStatus('all'); setOffset(0) }}>查看全部订单</Button> : undefined}
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
                  {items.map((order) => (
                    <tr key={order.id} className="align-top hover:bg-slate-50/60">
                      <td className="max-w-[360px] px-5 py-4">
                        <p className="line-clamp-2 font-medium leading-5 text-slate-800" title={order.question}>{order.question}</p>
                        <div className="mt-1.5 flex items-center gap-2 text-xs"><span className="font-medium text-blue-700">{order.outcome}</span><span className="text-slate-300">·</span><span className="text-slate-400">#{order.id}</span>{order.position_id !== null && <span className="text-slate-400">持仓 #{order.position_id}</span>}</div>
                      </td>
                      <td className="px-4 py-4">
                        <p className="flex items-center gap-1.5 text-slate-700">{order.kind === 'auto' ? <Bot className="h-3.5 w-3.5 text-slate-400" /> : <UserRound className="h-3.5 w-3.5 text-slate-400" />}{order.kind === 'auto' ? '自动策略' : '手动确认'}</p>
                        <p className="mt-1.5 text-xs text-slate-400">{order.type === 'buy' ? (isLive ? '实盘买入' : '模拟买入') : '结算'}</p>
                      </td>
                      <td className="px-4 py-4 text-right font-medium tabular-nums text-slate-800">{formatCurrency(order.amount)}</td>
                      <td className="px-4 py-4 text-right"><p className="tabular-nums text-slate-700">{formatPrice(order.price)}</p><p className="mt-1 text-xs tabular-nums text-slate-400">{formatNumber(order.shares, 4)} 份</p></td>
                      <td className="max-w-[230px] px-4 py-4">
                        <OrderStatusBadge status={order.status} />
                        <p className={cn('mt-2 line-clamp-2 text-xs leading-5', order.status === 'rejected' ? 'text-rose-600' : order.status === 'unknown' || order.status === 'pending' ? 'text-amber-700' : 'text-slate-500')} title={order.reason}>{order.reason || '无附加说明'}</p>
                        {order.external_id && <p className="mt-1 break-all font-mono text-[10px] text-slate-400">CLOB {order.external_id}</p>}
                      </td>
                      <td className="px-5 py-4 text-right"><span className="inline-flex items-center gap-1.5 text-xs text-slate-500"><CalendarDays className="h-3.5 w-3.5 text-slate-400" />{formatDate(order.created_at)}</span></td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </Card>
          <div className="grid gap-3 sm:grid-cols-2 lg:hidden">{items.map((order) => <OrderCard key={order.id} order={order} isLive={isLive} />)}</div>
          {data && data.total > PAGE_SIZE && (
            <div className="mt-4 flex flex-col items-center justify-between gap-3 text-xs text-slate-400 sm:flex-row">
              <p>当前显示第 {formatNumber(offset + 1, 0)}–{formatNumber(offset + items.length, 0)} 条，共 {formatNumber(data.total, 0)} 条记录</p>
              <div className="flex gap-2">
                <Button variant="secondary" size="sm" disabled={offset === 0 || isRefreshing} onClick={() => setOffset(Math.max(0, offset - PAGE_SIZE))}>上一页</Button>
                <Button variant="secondary" size="sm" disabled={offset + items.length >= data.total || isRefreshing} onClick={() => setOffset(offset + PAGE_SIZE)}>下一页</Button>
              </div>
            </div>
          )}
        </>
      )}
    </>
  )
}
