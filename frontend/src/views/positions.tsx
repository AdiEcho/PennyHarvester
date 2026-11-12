import { useCallback, useEffect, useRef, useState } from 'react'
import { BriefcaseBusiness, CalendarDays, Clock3, RefreshCw, WalletCards } from 'lucide-react'
import { api } from '@/lib/api'
import { useAccount } from '@/lib/account-context'
import { usePolling } from '@/hooks/usePolling'
import { useVenues } from '@/hooks/useVenues'
import type { Position, PositionStatus, PositionsResponse } from '@/types'
import { formatDate, formatFullDate, formatNumber, formatPrice, pnlClass } from '@/lib/format'
import { fmtMoney, settlementHintOf, venueMeta, venueOf } from '@/lib/venue'
import { cn } from '@/lib/utils'
import { useToast } from '@/components/toast'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { EmptyState, ErrorState, LoadingBlock } from '@/components/ui/feedback'
import { InlineError, PageHeader } from '@/components/page-header'
import { PositionStatusBadge, VenueChip } from '@/components/status-badges'
import { VenueFilter, useVenueScope, type VenueScope } from '@/components/venue-filter'

interface PositionsSnapshot {
  response: PositionsResponse
  query: { status: PositionStatus | 'all'; scope: VenueScope }
  observedAt: number
}

const tabs: { value: PositionStatus | 'all'; label: string }[] = [
  { value: 'all', label: '全部' },
  { value: 'open', label: '持有中' },
  { value: 'won', label: '已获胜' },
  { value: 'split', label: '分摊退款' },
  { value: 'sold', label: '已止损' },
  { value: 'lost', label: '未获胜' },
]

function positionPnl(position: Position) {
  return position.status === 'open' ? position.unrealized_pnl : position.realized_pnl
}

function PositionCard({
  position,
  isLive,
  canRedeem,
  redeeming,
  onRedeem,
}: {
  position: Position
  isLive: boolean
  canRedeem: boolean
  redeeming: boolean
  onRedeem: (position: Position) => void
}) {
  const pnl = positionPnl(position)
  const venue = venueOf(position)
  const redeemable = canRedeem && (position.status === 'won' || position.status === 'lost' || position.status === 'split')
  return (
    <Card className="p-4">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0"><p className="flex flex-wrap items-center gap-1.5 text-xs text-slate-400"><VenueChip venue={venue} />{venueMeta(venue).label} {isLive ? '实盘头寸' : '模拟头寸'} #{position.id}</p><h3 className="mt-1.5 text-sm font-medium leading-6 text-slate-900">{position.question}</h3></div>
        <div className="flex shrink-0 flex-col items-end gap-2"><PositionStatusBadge status={position.status} exitKind={position.exit_kind} endDate={position.end_date} />{redeemable && <Button variant="secondary" onClick={() => onRedeem(position)} disabled={redeeming}>{redeeming ? '赎回中…' : '赎回'}</Button>}</div>
      </div>
      <div className="mt-3 flex items-center gap-2 text-xs text-slate-500"><span className="rounded-md bg-blue-50 px-2 py-1 font-medium text-blue-700">{position.outcome}</span><span>{formatNumber(position.shares, 4)} 份</span><span>·</span><span>投入 {fmtMoney(position.cost, venue)}</span></div>
      <div className="mt-4 grid grid-cols-3 gap-3 border-t border-slate-100 pt-4">
        <div><p className="text-[11px] text-slate-400">入场价格</p><p className="mt-1 text-sm font-medium tabular-nums text-slate-800">{formatPrice(position.entry_price)}</p></div>
        <div><p className="text-[11px] text-slate-400">当前价格</p><p className="mt-1 text-sm font-medium tabular-nums text-slate-800">{formatPrice(position.current_price)}</p></div>
        <div className="text-right"><p className="text-[11px] text-slate-400">{position.status === 'open' ? '未实现损益' : '已实现损益'}</p><p className={cn('mt-1 text-sm font-semibold tabular-nums', pnlClass(pnl))}>{fmtMoney(pnl, venue, { signed: true })}</p></div>
      </div>
      <div className="mt-3 flex flex-wrap items-center gap-x-4 gap-y-1 text-[11px] text-slate-400">
        <span className="flex items-center gap-1.5"><CalendarDays className="h-3 w-3" />{position.status === 'open' ? '开仓' : position.exit_kind === 'stop' ? '止损' : '平仓'} {formatDate(position.status === 'open' ? position.opened_at : position.closed_at)}</span>
        <span className="flex items-center gap-1.5" title={formatFullDate(position.end_date)}><Clock3 className="h-3 w-3" />收盘 {formatDate(position.end_date)}</span>
      </div>
    </Card>
  )
}

export function PositionsView() {
  const [status, setStatus] = useState<PositionStatus | 'all'>('open')
  const { account, accountId, isLoading: accountLoading } = useAccount()
  const isLive = account?.mode === 'live'
  const accountVenue = venueOf(account)
  const { venues, capabilityOf } = useVenues()
  const { scope, scopeRef, setScope } = useVenueScope(venues)
  // Stable fetcher (see usePolling): the venue lives in a ref, the scope change
  // triggers refresh() so the table keeps showing rows while the filtered page
  // loads instead of falling back to skeletons.
  const fetchPositions = useCallback(
    async (signal: AbortSignal): Promise<PositionsSnapshot> => {
      const query = { status, scope: scopeRef.current }
      const response = await api.positions(accountId, query.status, query.scope === 'all' ? null : query.scope, signal)
      return { response, query, observedAt: Date.now() }
    },
    [accountId, status, scopeRef],
  )
  const { data: snapshot, error, isLoading, isRefreshing, refresh } = usePolling(fetchPositions, { enabled: Boolean(accountId) })
  const { toast } = useToast()
  // A single id unlocked every other row when two redemptions overlapped, and
  // the first completion cleared the second row's busy state. Track each
  // on-chain attempt independently.
  const [redeeming, setRedeeming] = useState<Set<number>>(() => new Set())
  const redeemingRef = useRef<Set<number>>(new Set())
  // Redemption is a live, on-chain action: a simulated position has no shares to
  // burn, and each venue advertises whether it implements claiming at all.
  const canRedeemVenue = useCallback(
    (venue: ReturnType<typeof venueOf>) => Boolean(capabilityOf(venue)?.supports_redeem),
    [capabilityOf],
  )
  const handleRedeem = useCallback(
    async (position: Position) => {
      const positionVenue = venueOf(position)
      const settled = position.status === 'won' || position.status === 'lost' || position.status === 'split'
      // Redemption is on-chain and only valid for the account's own venue. The
      // row button is hidden in the other cases, but a stale click or a
      // keyboard activation must not bypass the same checks.
      if (!accountId || !isLive || accountVenue !== positionVenue || !canRedeemVenue(positionVenue) || !settled || redeemingRef.current.has(position.id)) return
      redeemingRef.current.add(position.id)
      setRedeeming((current) => new Set(current).add(position.id))
      try {
        const result = await api.redeemPosition(accountId, position.id)
        toast({
          title: '赎回交易已提交',
          description: `交易 ${result.tx_hash}（${result.method}）。链上确认后抵押品会回到钱包，需自付 BNB gas。`,
          variant: 'success',
        })
        refresh()
      } catch (caught) {
        toast({ title: '赎回失败', description: caught instanceof Error ? caught.message : '请稍后重试。', variant: 'error' })
      } finally {
        redeemingRef.current.delete(position.id)
        setRedeeming((current) => { const next = new Set(current); next.delete(position.id); return next })
      }
    },
    [accountId, accountVenue, canRedeemVenue, isLive, refresh, toast],
  )
  const lastScope = useRef<typeof scope>(scope)
  useEffect(() => {
    if (lastScope.current === scope) return
    lastScope.current = scope
    refresh()
  }, [scope, refresh])
  const accepted = snapshot?.query
  const items = snapshot?.response.items || []
  const statusText = (value: PositionStatus | 'all') => value === 'open' ? '未平仓' : value === 'all' ? '' : value === 'sold' ? '已止损' : '已结算'
  const scopeText = (value: VenueScope) => value === 'all' ? venueMeta(accountVenue).label : venueMeta(value).label
  const staleLabel = snapshot && accepted ? `${statusText(accepted.status)}${isLive ? '实盘' : '模拟'}头寸 · ${scopeText(accepted.scope)} · 观察于 ${formatFullDate(new Date(snapshot.observedAt).toISOString())}` : ''
  const shownStatus = accepted?.status ?? status
  const shownScope = accepted?.scope ?? scope

  return (
    <>
      <PageHeader eyebrow={isLive ? 'LIVE POSITIONS' : 'PAPER POSITIONS'} title={isLive ? '实盘持仓' : '模拟持仓'} description={isLive ? `追踪实盘头寸的价格变化与结算结果。${settlementHintOf(accountVenue)}` : '追踪虚拟头寸的价格变化与结算结果，分别观察未实现和已实现损益。'} actions={<Button variant="secondary" onClick={refresh} disabled={isRefreshing}><RefreshCw className={cn('h-4 w-4', isRefreshing && 'animate-spin')} />刷新持仓</Button>} />
      {error && snapshot && <InlineError message={`${error}（当前展示 ${staleLabel}）`} onRetry={refresh} />}
      <div className="mb-4 flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div className="flex w-full gap-1 overflow-x-auto rounded-xl border border-slate-200 bg-surface p-1 sm:w-auto" role="tablist" aria-label="持仓状态筛选">
          {tabs.map((tab) => <button key={tab.value} role="tab" aria-selected={status === tab.value} onClick={() => setStatus(tab.value)} className={cn('h-8 flex-1 whitespace-nowrap rounded-lg px-4 text-xs font-medium transition sm:flex-none', status === tab.value ? 'bg-navy text-white shadow-sm' : 'text-slate-500 hover:bg-slate-50 hover:text-slate-800')}>{tab.label}</button>)}
        </div>
        <p className="text-xs text-slate-500">{snapshot ? <><span className="font-semibold tabular-nums text-slate-800">{formatNumber(snapshot.response.total, 0)}</span> 个{staleLabel}</> : '正在读取头寸…'}</p>
      </div>
      <div className="mb-4">
        <VenueFilter available={venues} value={scope} onChange={setScope} />
      </div>

      {accountLoading || isLoading ? <Card><LoadingBlock rows={5} /></Card> : error && !snapshot ? <Card><ErrorState message={error} onRetry={refresh} /></Card> : !items.length ? (
        <Card><EmptyState icon={shownStatus === 'open' ? WalletCards : BriefcaseBusiness} title={shownStatus === 'open' ? '暂无未平仓头寸' : '此状态下暂无持仓'} description={`${scopeText(shownScope)}：${shownStatus === 'open' ? (isLive ? '前往机会雷达，确认一笔实盘买入后即可开始观察。' : '前往机会雷达，确认一笔虚拟买入后即可开始观察。') : (isLive ? '切换其他状态查看，或等待实盘引擎产生新的记录。' : '切换其他状态查看，或等待模拟引擎产生新的记录。')}`} /></Card>
      ) : (
        <>
          <Card className="hidden overflow-hidden lg:block">
            <div className="overflow-x-auto"><table className="w-full min-w-[1080px] text-left text-sm">
              <thead className="border-b border-slate-100 bg-slate-50/70 text-xs text-slate-500"><tr><th className="px-5 py-3.5 font-medium">市场 / 方向</th><th className="px-4 py-3.5 font-medium">状态</th><th className="px-4 py-3.5 text-right font-medium">入场 / 当前</th><th className="px-4 py-3.5 text-right font-medium">份额</th><th className="px-4 py-3.5 text-right font-medium">投入成本</th><th className="px-4 py-3.5 text-right font-medium">损益</th><th className="px-5 py-3.5 text-right font-medium">时间 / 收盘</th></tr></thead>
              <tbody className={cn('divide-y divide-slate-100 transition-opacity', isRefreshing && 'opacity-60')}>{items.map((position) => {
                const pnl = positionPnl(position)
                const venue = venueOf(position)
                const canRedeemRow = isLive && accountVenue === venue && canRedeemVenue(venue) && (position.status === 'won' || position.status === 'lost' || position.status === 'split')
                return <tr key={position.id} className="hover:bg-slate-50/60"><td className="max-w-[360px] px-5 py-4"><p className="line-clamp-2 font-medium leading-5 text-slate-800" title={position.question}>{position.question}</p><div className="mt-1.5 flex items-center gap-2 text-xs"><VenueChip venue={venue} /><span className="font-medium text-blue-700">{position.outcome}</span><span className="text-slate-300">·</span><span className="text-slate-400">#{position.id}</span></div></td><td className="px-4 py-4"><PositionStatusBadge status={position.status} exitKind={position.exit_kind} endDate={position.end_date} /></td><td className="px-4 py-4 text-right tabular-nums text-slate-700"><span>{formatPrice(position.entry_price)}</span><span className="mx-1.5 text-slate-300">→</span><span>{formatPrice(position.current_price)}</span></td><td className="px-4 py-4 text-right tabular-nums text-slate-600">{formatNumber(position.shares, 4)}</td><td className="px-4 py-4 text-right"><p className="font-medium tabular-nums text-slate-800">{fmtMoney(position.cost, venue)}</p><p className="mt-1 text-[11px] text-slate-400">费用 {fmtMoney(position.fee, venue)}</p></td><td className="px-4 py-4 text-right"><p className={cn('font-semibold tabular-nums', pnlClass(pnl))}>{fmtMoney(pnl, venue, { signed: true })}</p><p className="mt-1 text-[11px] text-slate-400">{position.status === 'open' ? '未实现' : '已实现'}</p></td><td className="px-5 py-4 text-right text-xs text-slate-500"><p>{position.status === 'open' ? '开仓' : position.exit_kind === 'stop' ? '止损' : '结算'} {formatDate(position.status === 'open' ? position.opened_at : position.closed_at)}</p><p className="mt-1 text-[11px] text-slate-400" title={formatFullDate(position.end_date)}>收盘 {formatDate(position.end_date)}</p>{canRedeemRow && <Button className="mt-2" variant="secondary" onClick={() => handleRedeem(position)} disabled={redeeming.has(position.id)}>{redeeming.has(position.id) ? '赎回中…' : '赎回'}</Button>}</td></tr>
              })}</tbody>
            </table></div>
          </Card>
          <div className="grid gap-3 sm:grid-cols-2 lg:hidden">{items.map((position) => (
            <PositionCard
              key={position.id}
              position={position}
              isLive={isLive}
              canRedeem={isLive && accountVenue === venueOf(position) && canRedeemVenue(venueOf(position))}
              redeeming={redeeming.has(position.id)}
              onRedeem={handleRedeem}
            />
          ))}</div>
        </>
      )}
    </>
  )
}
