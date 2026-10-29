import { useCallback, useState } from 'react'
import { BriefcaseBusiness, CalendarDays, RefreshCw, WalletCards } from 'lucide-react'
import { api } from '@/lib/api'
import { useAccount } from '@/lib/account-context'
import { usePolling } from '@/hooks/usePolling'
import type { Position, PositionStatus } from '@/types'
import { formatCurrency, formatDate, formatFullDate, formatNumber, formatPrice, pnlClass } from '@/lib/format'
import { cn } from '@/lib/utils'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { EmptyState, ErrorState, LoadingBlock } from '@/components/ui/feedback'
import { InlineError, PageHeader } from '@/components/page-header'
import { PositionStatusBadge } from '@/components/status-badges'

const tabs: { value: PositionStatus | 'all'; label: string }[] = [
  { value: 'all', label: '全部' },
  { value: 'open', label: '持有中' },
  { value: 'won', label: '已获胜' },
  { value: 'lost', label: '未获胜' },
]

function positionPnl(position: Position) {
  return position.status === 'open' ? position.unrealized_pnl : position.realized_pnl
}

function PositionCard({ position, isLive }: { position: Position; isLive: boolean }) {
  const pnl = positionPnl(position)
  return (
    <Card className="p-4">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0"><p className="text-xs text-slate-400">{isLive ? '实盘头寸' : '模拟头寸'} #{position.id}</p><h3 className="mt-1.5 text-sm font-medium leading-6 text-slate-900">{position.question}</h3></div>
        <PositionStatusBadge status={position.status} />
      </div>
      <div className="mt-3 flex items-center gap-2 text-xs text-slate-500"><span className="rounded-md bg-blue-50 px-2 py-1 font-medium text-blue-700">{position.outcome}</span><span>{formatNumber(position.shares, 4)} 份</span><span>·</span><span>投入 {formatCurrency(position.cost)}</span></div>
      <div className="mt-4 grid grid-cols-3 gap-3 border-t border-slate-100 pt-4">
        <div><p className="text-[11px] text-slate-400">入场价格</p><p className="mt-1 text-sm font-medium tabular-nums text-slate-800">{formatPrice(position.entry_price)}</p></div>
        <div><p className="text-[11px] text-slate-400">当前价格</p><p className="mt-1 text-sm font-medium tabular-nums text-slate-800">{formatPrice(position.current_price)}</p></div>
        <div className="text-right"><p className="text-[11px] text-slate-400">{position.status === 'open' ? '未实现损益' : '已实现损益'}</p><p className={cn('mt-1 text-sm font-semibold tabular-nums', pnlClass(pnl))}>{formatCurrency(pnl, true)}</p></div>
      </div>
      <div className="mt-3 flex items-center gap-1.5 text-[11px] text-slate-400"><CalendarDays className="h-3 w-3" />{position.status === 'open' ? '到期' : '平仓'} {formatDate(position.status === 'open' ? position.end_date : position.closed_at)}</div>
    </Card>
  )
}

export function PositionsView() {
  const [status, setStatus] = useState<PositionStatus | 'all'>('open')
  const { account, accountId, isLoading: accountLoading } = useAccount()
  const isLive = account?.mode === 'live'
  const fetchPositions = useCallback((signal: AbortSignal) => api.positions(accountId, status, signal), [accountId, status])
  const { data, error, isLoading, isRefreshing, refresh } = usePolling(fetchPositions, { enabled: Boolean(accountId) })

  return (
    <>
      <PageHeader eyebrow={isLive ? 'LIVE POSITIONS' : 'PAPER POSITIONS'} title={isLive ? '实盘持仓' : '模拟持仓'} description={isLive ? '追踪实盘头寸的价格变化与结算结果。结算损益为链上可领取权益，需在 Polymarket 手动领取。' : '追踪虚拟头寸的价格变化与结算结果，分别观察未实现和已实现损益。'} actions={<Button variant="secondary" onClick={refresh} disabled={isRefreshing}><RefreshCw className={cn('h-4 w-4', isRefreshing && 'animate-spin')} />刷新持仓</Button>} />
      {error && data && <InlineError message={error} onRetry={refresh} />}
      <div className="mb-4 flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div className="flex w-full gap-1 overflow-x-auto rounded-xl border border-slate-200 bg-surface p-1 sm:w-auto" role="tablist" aria-label="持仓状态筛选">
          {tabs.map((tab) => <button key={tab.value} role="tab" aria-selected={status === tab.value} onClick={() => setStatus(tab.value)} className={cn('h-8 flex-1 whitespace-nowrap rounded-lg px-4 text-xs font-medium transition sm:flex-none', status === tab.value ? 'bg-navy text-white shadow-sm' : 'text-slate-500 hover:bg-slate-50 hover:text-slate-800')}>{tab.label}</button>)}
        </div>
        <p className="text-xs text-slate-500">{data ? <><span className="font-semibold tabular-nums text-slate-800">{formatNumber(data.total, 0)}</span> 个{status === 'open' ? '未平仓' : status === 'all' ? '' : '已结算'}{isLive ? '实盘' : '模拟'}头寸</> : '正在读取头寸…'}</p>
      </div>

      {accountLoading || isLoading ? <Card><LoadingBlock rows={5} /></Card> : error && !data ? <Card><ErrorState message={error} onRetry={refresh} /></Card> : !data?.items.length ? (
        <Card><EmptyState icon={status === 'open' ? WalletCards : BriefcaseBusiness} title={status === 'open' ? '暂无未平仓头寸' : '此状态下暂无持仓'} description={status === 'open' ? (isLive ? '前往机会雷达，确认一笔实盘买入后即可开始观察。' : '前往机会雷达，确认一笔虚拟买入后即可开始观察。') : (isLive ? '切换其他状态查看，或等待实盘引擎产生新的记录。' : '切换其他状态查看，或等待模拟引擎产生新的记录。')} /></Card>
      ) : (
        <>
          <Card className="hidden overflow-hidden lg:block">
            <div className="overflow-x-auto"><table className="w-full min-w-[1080px] text-left text-sm">
              <thead className="border-b border-slate-100 bg-slate-50/70 text-xs text-slate-500"><tr><th className="px-5 py-3.5 font-medium">市场 / 方向</th><th className="px-4 py-3.5 font-medium">状态</th><th className="px-4 py-3.5 text-right font-medium">入场 / 当前</th><th className="px-4 py-3.5 text-right font-medium">份额</th><th className="px-4 py-3.5 text-right font-medium">投入成本</th><th className="px-4 py-3.5 text-right font-medium">损益</th><th className="px-5 py-3.5 text-right font-medium">时间</th></tr></thead>
              <tbody className={cn('divide-y divide-slate-100 transition-opacity', isRefreshing && 'opacity-60')}>{data.items.map((position) => {
                const pnl = positionPnl(position)
                return <tr key={position.id} className="hover:bg-slate-50/60"><td className="max-w-[360px] px-5 py-4"><p className="line-clamp-2 font-medium leading-5 text-slate-800" title={position.question}>{position.question}</p><div className="mt-1.5 flex items-center gap-2 text-xs"><span className="font-medium text-blue-700">{position.outcome}</span><span className="text-slate-300">·</span><span className="text-slate-400">#{position.id}</span></div></td><td className="px-4 py-4"><PositionStatusBadge status={position.status} /></td><td className="px-4 py-4 text-right tabular-nums text-slate-700"><span>{formatPrice(position.entry_price)}</span><span className="mx-1.5 text-slate-300">→</span><span>{formatPrice(position.current_price)}</span></td><td className="px-4 py-4 text-right tabular-nums text-slate-600">{formatNumber(position.shares, 4)}</td><td className="px-4 py-4 text-right"><p className="font-medium tabular-nums text-slate-800">{formatCurrency(position.cost)}</p><p className="mt-1 text-[11px] text-slate-400">费用 {formatCurrency(position.fee)}</p></td><td className="px-4 py-4 text-right"><p className={cn('font-semibold tabular-nums', pnlClass(pnl))}>{formatCurrency(pnl, true)}</p><p className="mt-1 text-[11px] text-slate-400">{position.status === 'open' ? '未实现' : '已实现'}</p></td><td className="px-5 py-4 text-right text-xs text-slate-500"><p>{position.status === 'open' ? '开仓' : '结算'} {formatDate(position.status === 'open' ? position.opened_at : position.closed_at)}</p><p className="mt-1 text-[11px] text-slate-400" title={formatFullDate(position.end_date)}>到期 {formatDate(position.end_date, false)}</p></td></tr>
              })}</tbody>
            </table></div>
          </Card>
          <div className="grid gap-3 sm:grid-cols-2 lg:hidden">{data.items.map((position) => <PositionCard key={position.id} position={position} isLive={isLive} />)}</div>
        </>
      )}
    </>
  )
}
