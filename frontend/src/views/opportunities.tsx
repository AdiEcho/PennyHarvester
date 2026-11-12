import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  ArrowUpRight,
  Binoculars,
  CalendarDays,
  CheckCircle2,
  CircleDollarSign,
  ExternalLink,
  Filter,
  FlaskConical,
  Radar,
  Radio,
  RefreshCw,
  Search,
  ShieldAlert,
  Sparkles,
  X,
  Zap,
} from 'lucide-react'
import { api } from '@/lib/api'
import { useAccount } from '@/lib/account-context'
import { usePolling } from '@/hooks/usePolling'
import { useVenues } from '@/hooks/useVenues'
import { readSettingsRevision } from '@/lib/settings-revision'
import type { Opportunity, OpportunityQuote, Position, Settings } from '@/types'
import { formatFullDate, formatNumber, formatPercent, formatPrice, formatPriceChange } from '@/lib/format'
import { currencyOf, fmtMoney, protocolVersionOf, venueMeta, venueOf, venueOrderSize, VENUES, type Venue } from '@/lib/venue'
import { categoryCounts, categoryFilters, categoryLabel, matchesCategory, normalizeCategoryFilter, type CategoryFilter } from '@/lib/category'
import { marketOutcome, marketQuestion, marketURL, persistMarketLanguage, readMarketLanguage, type MarketLanguage } from '@/lib/market-language'
import { cn } from '@/lib/utils'
import { useToast } from '@/components/toast'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogTitle } from '@/components/ui/dialog'
import { EmptyState, ErrorState, LoadingBlock } from '@/components/ui/feedback'
import { Input } from '@/components/ui/input'
import { InlineError, PageHeader } from '@/components/page-header'
import { MarketLanguageSelect } from '@/components/market-language-select'
import { RiskNotice } from '@/components/risk-notice'
import { VenueChip } from '@/components/status-badges'
import { VenueFilter, useVenueScope, type VenueScope } from '@/components/venue-filter'

function MarketLink({ url, compact = false }: { url: string; compact?: boolean }) {
  if (!/^https?:\/\//i.test(url)) return null
  return (
    <a href={url} target="_blank" rel="noreferrer" className={cn('inline-flex items-center justify-center gap-1.5 rounded-lg text-slate-400 transition hover:bg-slate-100 hover:text-slate-800 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-emerald-600', compact ? 'h-8 w-8' : 'px-2 py-1.5 text-xs')} aria-label="在新窗口打开市场" title="查看公开市场">
      <ExternalLink className="h-3.5 w-3.5" />{!compact && '市场'}
    </a>
  )
}

interface QuoteState {
  quote?: OpportunityQuote
  error?: string
  loading?: boolean
}

interface HeldPosition {
  shares: number
  cost: number
  entryPrice: number
  count: number
  venue: Venue
}

// Both venues identify outcomes with decimal ERC-1155 ids, and the id spaces
// overlap: a Polymarket position must never light up a Predict.fun row. Every
// token-keyed map is therefore keyed by `${venue}:${token_id}`.
function tokenKey(value: { venue?: string; token_id: string }): string {
  return `${venueOf(value)}:${value.token_id}`
}

// A quote is only valid for the account, market outcome, amount and applied
// settings it was requested for. Including all of them in the cache key makes
// an amount edit a cache miss immediately instead of relying on a later stale
// comparison to hide the old numbers.
function quoteKey(
  value: { venue?: string; token_id: string; outcome: string },
  accountId: number | undefined,
  amount: number,
  configRevision: number,
): string {
  return `${accountId ?? 'none'}|${configRevision}|${tokenKey(value)}|${value.outcome}|${amount > 0 ? amount : 'default'}`
}

// The radar marks markets this account already holds. Manual and automatic
// fills can land on the same token more than once, so the marker shows the
// share-weighted entry price instead of whichever fill came last.
function heldPositionsByToken(positions: Position[]): Map<string, HeldPosition> {
  const groups = new Map<string, { venue: Venue; shares: number; cost: number; entryValue: number; count: number }>()
  for (const position of positions) {
    if (position.status !== 'open' || !(position.shares > 0)) continue
    const venue = venueOf(position)
    const key = tokenKey({ venue, token_id: position.token_id })
    const group = groups.get(key) || { venue, shares: 0, cost: 0, entryValue: 0, count: 0 }
    group.shares += position.shares
    group.cost += position.cost
    group.entryValue += position.entry_price * position.shares
    group.count += 1
    groups.set(key, group)
  }
  const result = new Map<string, HeldPosition>()
  groups.forEach((group, key) => {
    result.set(key, {
      venue: group.venue,
      shares: group.shares,
      cost: group.cost,
      entryPrice: group.entryValue / group.shares,
      count: group.count,
    })
  })
  return result
}

function heldTitle(held: HeldPosition) {
  return `已买入 ${formatNumber(held.shares, 4)} 份 · 成本 ${fmtMoney(held.cost, held.venue)}${held.count > 1 ? ` · 共 ${held.count} 笔` : ''}`
}

function HeldBadge({ held, className }: { held?: HeldPosition; className?: string }) {
  if (!held) return null
  return (
    <Badge variant="success" className={className} title={heldTitle(held)}>
      <CheckCircle2 className="h-3 w-3" />已买入
    </Badge>
  )
}

function HeldPrice({ held }: { held?: HeldPosition }) {
  if (!held) return null
  return <p className="mt-1 text-[11px] font-medium tabular-nums text-emerald-700" title={heldTitle(held)}>买入价 {formatPrice(held.entryPrice)}</p>
}

const QUOTE_TTL_MS = 60_000

function quoteAge(quotedAt: string) {
  const time = new Date(quotedAt).getTime()
  if (!Number.isFinite(time)) return ''
  const seconds = Math.max(0, Math.round((Date.now() - time) / 1000))
  if (seconds < 5) return '刚刚'
  if (seconds < 60) return `${seconds} 秒前`
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes} 分钟前`
  return `${Math.floor(minutes / 60)} 小时前`
}

function quoteIsStale(quote: OpportunityQuote): boolean {
  const time = new Date(quote.quoted_at).getTime()
  return !Number.isFinite(time) || Date.now() - time > QUOTE_TTL_MS
}

function RealYield({ state, venue, onQuote }: { state?: QuoteState; venue: Venue; onQuote: () => void }) {
  const quote = state?.quote
  const meta = venueMeta(venue)
  if (state?.loading && !quote) {
    return <span className="inline-flex items-center gap-1.5 text-xs text-slate-500"><RefreshCw className="h-3.5 w-3.5 animate-spin" />读取盘口…</span>
  }
  if (!quote) {
    return (
      <div className="flex flex-col items-end gap-1">
        <Button variant="ghost" size="sm" onClick={onQuote} title={`读取 ${meta.label} 实时订单簿与 taker 手续费，按参数设置中的 ${meta.label} 单笔金额估算盘口净收益`}><Zap className="h-3.5 w-3.5" />获取盘口估算</Button>
        {state?.error && <span className="line-clamp-2 max-w-[210px] text-right text-[11px] leading-4 text-rose-600" title={state.error}>{state.error}</span>}
      </div>
    )
  }
  const stale = quoteIsStale(quote)
  return (
    <div className="flex flex-col items-end gap-0.5 text-right">
      <span className={cn('font-semibold tabular-nums', stale ? 'text-amber-700' : quote.net_yield > 0 ? 'text-emerald-700' : 'text-rose-700')}>{formatPercent(quote.net_yield, 2, true)}</span>
      <span className="text-[11px] tabular-nums text-slate-500" title={`限价 ${formatPrice(quote.limit_price)} · 盘口可用 ${fmtMoney(quote.available_notional, venue)}`}>成交均价 {formatPrice(quote.fill_price)} · 费 {fmtMoney(quote.fee, venue)}</span>
      <span className="flex items-center gap-1 text-[11px] tabular-nums text-slate-400">
        {fmtMoney(quote.amount, venue)} · {quoteAge(quote.quoted_at)}{stale && ' · 已过期'}
        <button type="button" onClick={onQuote} disabled={state?.loading} className="rounded p-0.5 text-slate-400 transition hover:bg-slate-100 hover:text-slate-700 disabled:opacity-50" title="重新读取实时盘口与手续费" aria-label="刷新盘口估算">
          <RefreshCw className={cn('h-3 w-3', state?.loading && 'animate-spin')} />
        </button>
      </span>
      {stale && <span className="text-[11px] leading-4 text-amber-700" role="status">报价已超过 1 分钟，仅作历史参考；请刷新</span>}
      {quote.exceeds_max_price && <span className="text-[11px] leading-4 text-rose-600">超出最高买入价，实盘会拒单</span>}
      {!quote.fully_filled && <span className="text-[11px] leading-4 text-amber-600">深度不足，仅可成交 {fmtMoney(quote.available_notional, venue, { compact: true })}</span>}
      {state?.error && <span className="line-clamp-2 max-w-[210px] text-right text-[11px] leading-4 text-rose-600" title={state.error}>{state.error}</span>}
    </div>
  )
}

// ProtocolBadge marks Polymarket Protocol V2 markets. They quote themselves
// through positionIds and settle on ExchangeV3, which the live executor cannot
// sign for yet, so a live order is refused with an explicit reason while paper
// accounts still rehearse on them. Predict.fun has no protocol versions.
function ProtocolBadge({ item }: { item: Opportunity }) {
  if (protocolVersionOf(item) !== 'v2') return null
  return (
    <Badge
      variant="danger"
      title="Polymarket Protocol V2 市场（positionId + ExchangeV3）：模拟账户可正常演练，实盘下单会被明确拒绝"
    >
      协议 V2 · 实盘不支持
    </Badge>
  )
}

function OpportunityCard({ item, held, onBuy, onQuote, quote, orderSize, isLive, liveReady, venueHint, language }: { item: Opportunity; held?: HeldPosition; onBuy: (item: Opportunity) => void; onQuote: () => void; quote?: QuoteState; orderSize: number; isLive: boolean; liveReady: boolean; venueHint: string; language: MarketLanguage }) {
  const venue = venueOf(item)
  const meta = venueMeta(venue)
  return (
    <Card className="p-4">
      <div className="flex items-start gap-3">
        <div className="min-w-0 flex-1">
          <div className="mb-2 flex flex-wrap items-center gap-1.5"><VenueChip venue={venue} /><Badge variant="info">{marketOutcome(item, language)}</Badge><Badge variant="default">{categoryLabel(item.category)}</Badge><HeldBadge held={held} />{item.sports_game && <Badge variant="warning" title="体育赛事盘口可能包含加时、延长赛或点球决胜，临近结算时价格波动较大">赛事 · 加时风险</Badge>}<ProtocolBadge item={item} /><span className="text-[11px] text-slate-400">#{item.market_id.slice(0, 10)}</span></div>
          <h3 className="text-sm font-medium leading-6 text-slate-900">{marketQuestion(item, language)}</h3>
        </div>
        <MarketLink url={marketURL(item.url, language)} compact />
      </div>
      <div className="mt-4 grid grid-cols-2 gap-3 rounded-xl bg-slate-50 p-3">
        <div><p className="text-[11px] text-slate-500">指示价格</p><p className="mt-1 text-sm font-semibold tabular-nums text-slate-900">{formatPrice(item.price)}</p>{item.price_change_24h != null && <p className={cn('mt-0.5 text-[11px] tabular-nums', Math.abs(item.price_change_24h) >= 0.05 ? 'text-amber-600' : 'text-slate-400')}>24h {formatPriceChange(item.price_change_24h)}</p>}<HeldPrice held={held} /></div>
        <div><p className="text-[11px] text-slate-500">毛收益</p><p className="mt-1 text-sm font-semibold tabular-nums text-emerald-700">{formatPercent(item.gross_yield, 2, true)}</p></div>
      </div>
      {item.sports_game && <p className="mt-3 flex items-start gap-1.5 rounded-xl border border-amber-200 bg-amber-50 px-3 py-2 text-[11px] leading-4 text-amber-800"><ShieldAlert className="mt-0.5 h-3.5 w-3.5 shrink-0" />体育赛事盘口可能包含加时、延长赛或点球决胜，临近结算时价格波动较大。</p>}
      <div className="mt-3 flex items-center justify-between gap-3 rounded-xl border border-slate-200 p-3">
        <div>
          <p className="text-[11px] text-slate-500">盘口估算净收益（含手续费）</p>
          <p className="text-[10px] text-slate-400">按单笔 {orderSize > 0 ? fmtMoney(orderSize, venue) : '金额'} · {meta.label} 实时盘口</p>
        </div>
        <RealYield state={quote} venue={venue} onQuote={onQuote} />
      </div>
      <div className="mt-3 flex items-center justify-between gap-3">
        <div className="flex flex-wrap gap-x-3 gap-y-1 text-[11px] text-slate-500"><span>流动性 {fmtMoney(item.liquidity, venue, { compact: true })}</span><span>{formatNumber(item.days_to_end, 1)} 天 · {formatFullDate(item.end_date)} 到期</span></div>
        <Button size="sm" variant={isLive ? 'danger' : 'primary'} onClick={() => onBuy(item)} disabled={!liveReady} title={venueHint || undefined}>{isLive ? <Radio className="h-3.5 w-3.5" /> : <FlaskConical className="h-3.5 w-3.5" />}{isLive ? '实盘买入' : '模拟买入'}</Button>
      </div>
    </Card>
  )
}

export function OpportunitiesView({ onOrderCreated }: { onOrderCreated: () => void }) {
  const { account, accountId } = useAccount()
  const isLive = account?.mode === 'live'
  const accountVenue = venueOf(account)
  const [query, setQuery] = useState('')
  const [marketLanguage, setMarketLanguage] = useState<MarketLanguage>(readMarketLanguage)
  const [category, setCategory] = useState<CategoryFilter>('non_sports')
  const [sportsPolicy, setSportsPolicy] = useState('exclude_games')
  const categoryTouched = useRef(false)
  const [selected, setSelected] = useState<Opportunity | null>(null)
  const [amount, setAmount] = useState('')
  const [confirmed, setConfirmed] = useState(false)
  const [scanning, setScanning] = useState(false)
  const [scanPrompt, setScanPrompt] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const [orderError, setOrderError] = useState('')
  const [orderErrorUnknown, setOrderErrorUnknown] = useState(false)
  const [quotes, setQuotes] = useState<Record<string, QuoteState>>({})
  const [settings, setSettings] = useState<Settings | null>(null)
  const [settingsRevision, setSettingsRevision] = useState(() => readSettingsRevision(accountId))
  const scanController = useRef<AbortController | null>(null)
  const orderController = useRef<AbortController | null>(null)
  const quoteControllers = useRef<Map<string, AbortController>>(new Map())
  const { toast } = useToast()
  const { venues } = useVenues()
  const { scope, scopeRef, setScope } = useVenueScope(venues)
  // The fetcher must keep a stable identity: usePolling drops the current rows
  // when the fetcher changes, which would flash skeletons on every chip click.
  // The scope is read from a ref and a change triggers refresh() instead, so
  // the previous page stays visible (dimmed) until the new one arrives.
  const fetchOpportunities = useCallback(
    (signal: AbortSignal) => api.opportunities(scopeRef.current === 'all' ? null : scopeRef.current, signal, accountId),
    [scopeRef, accountId],
  )
  const { data, error, isLoading, isRefreshing, refresh } = usePolling(fetchOpportunities)
  const lastScope = useRef<VenueScope>(scope)
  useEffect(() => {
    if (lastScope.current === scope) return
    lastScope.current = scope
    refresh()
  }, [scope, refresh])
  // Open positions for the selected account drive the 「已买入」 markers. They
  // are polled separately so auto-trade fills show up without a radar scan.
  const fetchHeldPositions = useCallback((signal: AbortSignal) => api.positions(accountId, 'open', null, signal), [accountId])
  const { data: heldData, refresh: refreshHeld } = usePolling(fetchHeldPositions, { enabled: Boolean(accountId) })
  const heldByToken = useMemo(() => heldPositionsByToken(heldData?.items || []), [heldData])

  useEffect(() => () => {
    scanController.current?.abort()
    orderController.current?.abort()
    quoteControllers.current.forEach((controller) => controller.abort())
    quoteControllers.current.clear()
  }, [])

  // Quotes are account-scoped: a quote computed for one wallet's size or
  // venue must not survive an account switch.
  useEffect(() => {
    quoteControllers.current.forEach((controller) => controller.abort())
    quoteControllers.current.clear()
    setQuotes({})
    setSettingsRevision(readSettingsRevision(accountId))
    setSelected(null)
    setAmount('')
    setConfirmed(false)
    setOrderError('')
    setOrderErrorUnknown(false)
  }, [accountId])

  useEffect(() => {
    persistMarketLanguage(marketLanguage)
  }, [marketLanguage])

  // The real-yield button quotes the account's configured single-order size, and
  // the same request supplies the account's default radar category. A manual
  // category selection must not be overwritten by a later poll.
  useEffect(() => {
    const controller = new AbortController()
    api.settings(controller.signal, accountId).then((next) => {
      setSettings(next)
      setSportsPolicy(next.sports_policy)
      if (!categoryTouched.current) setCategory(normalizeCategoryFilter(next.default_category))
    }).catch(() => {})
    return () => controller.abort()
  }, [accountId])

  const orderSizeFor = useCallback((venue: Venue) => venueOrderSize(settings, venue), [settings])
  const scopeLabel = scope === 'all' ? VENUES.map((venue) => venueMeta(venue).label).join(' + ') : venueMeta(scope).label
  const scopedOrderSize = scope === 'all' ? 0 : orderSizeFor(scope)
  // A quote is only meaningful for the account's own venue, live or paper.
  // Paper accounts must not submit a Polymarket row to a Predict.fun account
  // (the backend rejects venue mismatches, but the UI should not offer it).
  const venueBlockedReason = (venue: Venue) => {
    if (accountVenue !== venue) return `当前账户属于 ${venueMeta(accountVenue).label}，不能向 ${venueMeta(venue).label} 下单`
    if (isLive && !account?.live_ready) return `${venueMeta(venue).label} 实盘凭据未就绪`
    return ''
  }
  const venueReady = (venue: Venue) => !venueBlockedReason(venue)

  const allItems = useMemo(() => data?.items || [], [data])
  const counts = useMemo(() => categoryCounts(allItems), [allItems])
  const filtered = useMemo(() => {
    const keyword = query.trim().toLocaleLowerCase()
    return allItems.filter((item) => {
      if (!matchesCategory(item, category)) return false
      if (!keyword) return true
      return [item.question, item.question_zh || '', item.event_title_zh || '', item.group_item_title || '', item.outcome, item.outcome_zh || '', item.slug, item.market_id].some((value) => value.toLocaleLowerCase().includes(keyword))
    })
  }, [allItems, query, category])

  const numericAmount = Number(amount)
  const validAmount = amount.trim() !== '' && Number.isFinite(numericAmount) && numericAmount > 0
  const selectedVenue = selected ? venueOf(selected) : accountVenue
  // The selected quote is looked up by the full request identity. When the
  // amount or settings revision changes this becomes a different key and the
  // old quote disappears immediately, even before a new request starts.
  const selectedQuoteAmount = validAmount ? numericAmount : orderSizeFor(selectedVenue)
  const selectedQuoteState = selected ? quotes[quoteKey(selected, accountId, selectedQuoteAmount, settingsRevision)] : undefined
  const selectedQuote = selectedQuoteState?.quote
  const selectedHeld = selected ? heldByToken.get(tokenKey(selected)) : undefined
  const quoteTimeStale = Boolean(selectedQuote && quoteIsStale(selectedQuote))

  const fetchQuote = async (item: Opportunity, quoteAmount?: number) => {
    const targetAmount = quoteAmount && quoteAmount > 0 ? quoteAmount : 0
    const key = quoteKey(item, accountId, targetAmount, settingsRevision)
    quoteControllers.current.get(key)?.abort()
    const controller = new AbortController()
    quoteControllers.current.set(key, controller)
    setQuotes((current) => ({ ...current, [key]: { ...current[key], loading: true, error: undefined } }))
    try {
      const quote = await api.quoteOpportunity(item.token_id, targetAmount || undefined, venueOf(item), controller.signal, accountId)
      // Aborting is advisory in some runtimes. Ignore a response when a newer
      // request (or an account switch) has already replaced this controller.
      if (quoteControllers.current.get(key) !== controller) return
      setQuotes((current) => ({ ...current, [key]: { quote } }))
    } catch (caught) {
      if (caught instanceof DOMException && caught.name === 'AbortError') return
      if (quoteControllers.current.get(key) !== controller) return
      setQuotes((current) => ({ ...current, [key]: { loading: false, error: caught instanceof Error ? caught.message : '获取盘口估算失败，请重试。' } }))
    } finally {
      if (quoteControllers.current.get(key) === controller) quoteControllers.current.delete(key)
    }
  }

  const openBuy = (item: Opportunity) => {
    setSelected(item)
    setAmount('')
    setConfirmed(false)
    setOrderError('')
    setOrderErrorUnknown(false)
  }

  const runScan = async (execute: boolean) => {
    setScanPrompt(false)
    scanController.current?.abort()
    const controller = new AbortController()
    scanController.current = controller
    setScanning(true)
    try {
      const result = await api.scan(controller.signal, execute, accountId, scope === 'all' ? null : scope)
      if (execute) {
        const summary = result.auto_trade
        const placed = summary?.orders_placed ?? 0
        const skipped = summary?.skipped_held ?? 0
        const details = [
          `本次返回 ${formatNumber(result.total, 0)} 个候选机会（${scopeLabel}）`,
          '已先执行止损保护（触发时会提交止损卖出）',
          placed > 0 ? `已对 ${placed} 个未持仓盘口新增买入` : '没有符合条件的新盘口，本次未新增买入',
          skipped > 0 ? `跳过 ${skipped} 个已有持仓记录的盘口买入` : '',
        ].filter(Boolean).join('；')
        toast({
          title: summary?.error ? '扫描完成，但自动交易出现错误' : placed > 0 ? '扫描完成并执行了自动交易' : '扫描完成，本次无新增买入',
          description: summary?.error ? `${details}。${summary.error}` : `${details}。`,
          variant: summary?.error ? 'error' : placed > 0 ? 'success' : 'info',
        })
      } else {
        toast({ title: '扫描完成', description: `本次返回 ${formatNumber(result.total, 0)} 个候选机会（${scopeLabel}）；本次仅刷新行情，未执行自动交易（也不会触发止损卖出）。`, variant: 'success' })
      }
      refresh()
      refreshHeld()
    } catch (caught) {
      if (caught instanceof DOMException && caught.name === 'AbortError') return
      toast({ title: '扫描失败', description: caught instanceof Error ? caught.message : '请稍后重试。', variant: 'error' })
    } finally {
      if (scanController.current === controller) setScanning(false)
    }
  }

  const submitOrder = async () => {
    if (!selected || !validAmount || !confirmed) return
    const blocked = venueBlockedReason(selectedVenue)
    if (blocked) {
      setOrderError(blocked)
      return
    }
    orderController.current?.abort()
    const controller = new AbortController()
    orderController.current = controller
    setSubmitting(true)
    setOrderError('')
    setOrderErrorUnknown(false)
    try {
      if (!account) throw new Error('尚未选择账户')
      const position = await api.createOrder(account.id, {
        market_id: selected.market_id,
        outcome: selected.outcome,
        token_id: selected.token_id,
        amount: numericAmount,
        venue: selectedVenue,
      }, controller.signal)
      toast({ title: isLive ? '实盘买入已成交' : '模拟买入已成交', description: `已创建${isLive ? '实盘' : '模拟'}持仓 #${position.id}，成交份额 ${formatNumber(position.shares, 4)}。`, variant: 'success' })
      setSelected(null)
      refreshHeld()
      onOrderCreated()
    } catch (caught) {
      if (caught instanceof DOMException && caught.name === 'AbortError') return
      // A lost response is not a rejected order. Network failures and explicit
      // unknown outcomes must prompt a position refresh before any retry;
      // otherwise the operator can double-submit a live order.
      const message = caught instanceof Error ? caught.message : ''
      const unknown = caught instanceof TypeError || /timeout|timed out|未知|network|failed to fetch/i.test(message)
      setOrderErrorUnknown(unknown)
      if (unknown) refreshHeld()
      setOrderError(message || (isLive ? '实盘订单提交失败，请重试。' : '模拟订单提交失败，请重试。'))
    } finally {
      if (orderController.current === controller) setSubmitting(false)
    }
  }

  // A live account can only auto-trade its own venue, even when the radar shows both.
  const scopeMatchesAccount = scope === 'all' || scope === accountVenue
  const sources = data?.venues?.length ? data.venues : venues

  return (
    <>
      <PageHeader eyebrow={isLive ? 'LIVE OPPORTUNITY RADAR' : 'OPPORTUNITY RADAR'} title="机会雷达" description={isLive ? `筛选公开市场价格偏离。点击实盘买入会把订单提交到 ${venueMeta(accountVenue).label}，请务必确认金额与盘口深度。` : '筛选公开市场的价格偏离，以理论收益辅助研究，不构成结算承诺。'} actions={<><MarketLanguageSelect value={marketLanguage} onChange={setMarketLanguage} /><Button variant="primary" onClick={() => setScanPrompt(true)} loading={scanning}><Radar className={cn('h-4 w-4', !scanning && 'text-emerald-200 dark:text-emerald-950')} />{scanning ? '正在扫描…' : '立即扫描'}</Button></>} />
      {error && data && <InlineError message={error} onRetry={refresh} />}

      <Card className="mb-4 p-4 sm:p-5">
        <div className="flex flex-col gap-4 md:flex-row md:items-center">
          <div className="relative flex-1">
            <Search className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-slate-400" />
            <Input value={query} onChange={(event) => setQuery(event.target.value)} className="pl-9 pr-9" placeholder="搜索市场问题、方向或 ID…" aria-label="搜索机会" />
            {query && <button onClick={() => setQuery('')} className="absolute right-2.5 top-1/2 -translate-y-1/2 rounded p-1 text-slate-400 hover:bg-slate-100 hover:text-slate-700" aria-label="清除搜索"><X className="h-3.5 w-3.5" /></button>}
          </div>
          <div className="flex flex-wrap items-center justify-between gap-3 md:justify-end">
            <p className="text-xs text-slate-500" aria-live="polite"><span className="font-semibold tabular-nums text-slate-800">{formatNumber(filtered.length, 0)}</span> 个匹配结果{(query || category !== 'all') && data ? <span> / 共 {formatNumber(data.total, 0)} 个</span> : null}</p>
            <Button variant="ghost" size="sm" onClick={() => { refresh(); refreshHeld() }} disabled={isRefreshing}><RefreshCw className={cn('h-3.5 w-3.5', isRefreshing && 'animate-spin')} />刷新</Button>
          </div>
        </div>
        <div className="mt-3 border-t border-slate-100 pt-3">
          <VenueFilter available={venues} value={scope} onChange={setScope} />
        </div>
        <div className="mt-3 flex flex-wrap items-center gap-1.5 border-t border-slate-100 pt-3">
          <span className="mr-1 inline-flex items-center gap-1 text-[11px] font-medium text-slate-400"><Filter className="h-3.5 w-3.5" />分类</span>
          {categoryFilters.map((option) => {
            const active = category === option.value
            return (
              <button
                key={option.value}
                type="button"
                onClick={() => { categoryTouched.current = true; setCategory(option.value) }}
                aria-pressed={active}
                className={cn('inline-flex items-center gap-1.5 rounded-full border px-3 py-1 text-xs transition', active ? 'border-emerald-200 bg-emerald-50 font-medium text-emerald-800' : 'border-slate-200 bg-surface text-slate-600 hover:border-slate-300 hover:bg-slate-50')}
              >
                {option.label}
                <span className={cn('tabular-nums text-[10px]', active ? 'text-emerald-600' : 'text-slate-400')}>{formatNumber(counts[option.value], 0)}</span>
              </button>
            )
          })}
        </div>
        {category === 'sports' && sportsPolicy !== 'allow' && (
          <p className="mt-2 text-[11px] leading-4 text-amber-700">当前体育市场策略已排除体育赛事盘口，这里只显示未被排除的体育预测市场；可在「参数设置 → 市场筛选」中调整。</p>
        )}
        <div className="mt-3 flex flex-wrap items-center gap-x-4 gap-y-1 border-t border-slate-100 pt-3 text-[11px] text-slate-400">
          <span>毛收益 = (1 − 价格) / 价格（未计手续费）</span>
          <span>盘口估算净收益 = 实时盘口成交价 + 真实手续费，按「参数设置 → 场所单笔金额」{scopedOrderSize > 0 ? `（${fmtMoney(scopedOrderSize, scope === 'all' ? accountVenue : scope)}）` : '（逐场所）'}估算，非最终结算收益</span>
          <span>到期时间按本地时区显示</span>
          <span>立即扫描会先询问是否执行自动交易（含止损卖出）；自动买入只处理当前账户尚无持仓记录的盘口</span>
          <span>默认分类来自「参数设置」；体育赛事盘口默认按加时/波动风险过滤</span>
          <span>{marketLanguage === 'zh' ? '市场问题与方向优先使用中文文案' : '市场问题与方向显示英文原文'}</span>
          {data && <span className="sm:ml-auto">来源 {data.source}{sources.length > 1 ? ` · ${sources.map((venue) => venueMeta(venue).label).join(' + ')}` : ''} · 扫描于 {formatFullDate(data.scanned_at)}</span>}
        </div>
      </Card>

      {isLive && !venueReady(accountVenue) && (
        <div className="mb-4 flex items-start gap-2 rounded-xl border border-rose-200 bg-rose-50 p-3 text-xs leading-6 text-rose-700" role="alert">
          <ShieldAlert className="mt-0.5 h-4 w-4 shrink-0" />
          <span><span className="font-medium">{venueMeta(accountVenue).label} 实盘凭据未就绪。</span> {account?.live_configured ? (account.live_error || '请检查钱包、Funder 与 API 凭据。') : `可在「系统设置 → 实盘交易凭据」中完成配置，或设置 ${venueMeta(accountVenue).envHint} 后重启服务。`} 仅有凭据就绪的场所可以提交实盘买单。</span>
        </div>
      )}

      {isLoading ? <Card><LoadingBlock rows={6} /></Card> : error && !data ? <Card><ErrorState message={error} onRetry={refresh} /></Card> : !data?.items.length ? (
        <Card><EmptyState icon={Binoculars} title="雷达暂未发现机会" description={`当前筛选条件下没有候选市场（场所：${scopeLabel}）。你可以重新扫描，或在参数设置中调整筛选阈值。`} action={<Button variant="secondary" onClick={() => setScanPrompt(true)} loading={scanning}><Radar className="h-4 w-4" />重新扫描</Button>} /></Card>
      ) : filtered.length === 0 ? (
        query.trim() ? (
          <Card><EmptyState icon={Search} title="没有匹配的市场" description={`没有找到与“${query}”相关的机会，请尝试其他关键词。`} action={<Button variant="secondary" onClick={() => setQuery('')}>清除搜索</Button>} /></Card>
        ) : (
          <Card><EmptyState icon={Filter} title="该分类暂无可交易机会" description={category === 'sports' && sportsPolicy !== 'allow' ? '体育赛事盘口默认按加时与临场波动风险过滤。可在「参数设置 → 市场筛选」中调整体育市场策略，或切换到其他分类。' : '当前分类下没有符合价格、流动性与波动上限的候选市场。可切换分类，或在参数设置中调整筛选条件。'} action={<Button variant="secondary" onClick={() => { categoryTouched.current = true; setCategory(category === 'non_sports' ? 'all' : 'non_sports') }}>{category === 'non_sports' ? '查看全部机会' : '查看非体育机会'}</Button>} /></Card>
        )
      ) : (
        <>
          <Card className="hidden overflow-hidden lg:block">
            <div className="overflow-x-auto">
              <table className="w-full min-w-[1080px] text-left text-sm">
                <thead className="border-b border-slate-100 bg-slate-50/70 text-xs text-slate-500">
                  <tr><th className="px-5 py-3.5 font-medium">市场 / 方向</th><th className="px-4 py-3.5 text-right font-medium">指示价格</th><th className="px-4 py-3.5 text-right font-medium">毛收益</th><th className="px-4 py-3.5 text-right font-medium">盘口估算净收益<p className="mt-0.5 text-[10px] font-normal text-slate-400">按单笔 {scopedOrderSize > 0 ? fmtMoney(scopedOrderSize, scope === 'all' ? accountVenue : scope) : '场所金额'} · 非最终收益</p></th><th className="px-4 py-3.5 text-right font-medium">流动性</th><th className="px-4 py-3.5 text-right font-medium">到期</th><th className="px-5 py-3.5 text-right font-medium">操作</th></tr>
                </thead>
                <tbody className={cn('divide-y divide-slate-100 transition-opacity', isRefreshing && 'opacity-60')}>
                  {filtered.map((item) => {
                    const venue = venueOf(item)
                    const held = heldByToken.get(tokenKey(item))
                    const blocked = venueBlockedReason(venue)
                    const ready = !blocked
                    const quoteAmount = orderSizeFor(venue)
                    return (
                      <tr key={`${venue}:${item.market_id}:${item.token_id}`} className="group hover:bg-slate-50/70">
                        <td className="max-w-[390px] px-5 py-4"><div className="flex items-start gap-3"><div className="mt-0.5 rounded-lg bg-slate-100 p-2 text-slate-400 group-hover:bg-surface group-hover:text-emerald-600"><Sparkles className="h-4 w-4" /></div><div className="min-w-0"><p className="line-clamp-2 font-medium leading-5 text-slate-800" title={marketQuestion(item, marketLanguage)}>{marketQuestion(item, marketLanguage)}</p><div className="mt-1.5 flex flex-wrap items-center gap-1.5"><VenueChip venue={venue} className="px-2 py-0.5 text-[11px]" /><Badge variant="info" className="px-2 py-0.5 text-[11px]">{marketOutcome(item, marketLanguage)}</Badge><Badge variant="default" className="px-2 py-0.5 text-[11px]">{categoryLabel(item.category)}</Badge><HeldBadge held={held} className="px-2 py-0.5 text-[11px]" />{item.sports_game && <Badge variant="warning" className="px-2 py-0.5 text-[11px]" title="体育赛事盘口可能包含加时、延长赛或点球决胜，临近结算时价格波动较大">赛事 · 加时风险</Badge>}<ProtocolBadge item={item} /><span className="text-[11px] text-slate-400">{item.slug || item.market_id.slice(0, 12)}</span></div></div></div></td>
                        <td className="px-4 py-4 text-right"><div className="font-semibold tabular-nums text-slate-800">{formatPrice(item.price)}</div>{item.price_change_24h != null && <p className={cn('mt-1 text-[11px] tabular-nums', Math.abs(item.price_change_24h) >= 0.05 ? 'text-amber-600' : 'text-slate-400')}>24h {formatPriceChange(item.price_change_24h)}</p>}<HeldPrice held={held} /></td>
                        <td className="px-4 py-4 text-right font-medium tabular-nums text-emerald-700">{formatPercent(item.gross_yield, 2, true)}</td>
                        <td className="px-4 py-4 text-right"><RealYield state={quotes[quoteKey(item, accountId, quoteAmount, settingsRevision)]} venue={venue} onQuote={() => fetchQuote(item, quoteAmount || undefined)} /></td>
                        <td className="px-4 py-4 text-right tabular-nums text-slate-600">{fmtMoney(item.liquidity, venue, { compact: true })}</td>
                        <td className="px-4 py-4 text-right"><span className="tabular-nums text-slate-700">{formatNumber(item.days_to_end, 1)} 天</span><p className="mt-1 whitespace-nowrap text-[11px] tabular-nums text-slate-400">{formatFullDate(item.end_date)}</p></td>
                        <td className="px-5 py-4"><div className="flex items-center justify-end gap-1"><MarketLink url={marketURL(item.url, marketLanguage)} compact /><Button size="sm" variant="secondary" className={isLive ? 'text-rose-700 group-hover:border-rose-200 group-hover:bg-rose-50 group-hover:text-rose-800' : 'group-hover:border-emerald-200 group-hover:bg-emerald-50 group-hover:text-emerald-800'} onClick={() => openBuy(item)} disabled={!ready} title={blocked || undefined}>{isLive ? <Radio className="h-3.5 w-3.5" /> : <FlaskConical className="h-3.5 w-3.5" />}{isLive ? '实盘买入' : '模拟买入'}</Button></div></td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>
          </Card>
          <div className="grid gap-3 sm:grid-cols-2 lg:hidden">{filtered.map((item) => <OpportunityCard key={`${venueOf(item)}:${item.market_id}:${item.token_id}`} item={item} held={heldByToken.get(tokenKey(item))} onBuy={openBuy} onQuote={() => fetchQuote(item, orderSizeFor(venueOf(item)) || undefined)} quote={quotes[quoteKey(item, accountId, orderSizeFor(venueOf(item)), settingsRevision)]} orderSize={orderSizeFor(venueOf(item))} isLive={isLive} liveReady={venueReady(venueOf(item))} venueHint={venueBlockedReason(venueOf(item))} language={marketLanguage} />)}</div>
        </>
      )}

      <Dialog open={scanPrompt} onOpenChange={(open) => { if (!open && !scanning) setScanPrompt(false) }}>
        <DialogContent className="max-w-md">
          <div>
            <div className="mb-3 flex flex-wrap items-center gap-2">
              <Badge variant={isLive ? 'danger' : 'success'}>{isLive ? <Radio className="h-3 w-3" /> : <FlaskConical className="h-3 w-3" />}{isLive ? '实盘账户' : '模拟账户'}</Badge>
              <VenueChip venue={accountVenue} full />
              <span className="text-xs text-slate-400">{account?.name || '当前账户'}</span>
            </div>
            <DialogTitle>立即扫描：是否执行自动交易？</DialogTitle>
            <DialogDescription className="mt-2">扫描会拉取最新公开行情、刷新持仓盯市并记录净值。自动交易会先检查止损（可能卖出或减仓已有持仓），再按规则买入本轮出现的新机会。是否执行？</DialogDescription>
          </div>
          <div className="rounded-xl border border-slate-200 bg-slate-50 p-4 text-xs leading-5 text-slate-600">
            <p><span className="font-medium text-slate-800">本次扫描场所：</span>{scopeLabel}（按当前场所筛选）。扫描全部场所时会逐场所执行。</p>
            <p className="mt-1"><span className="font-medium text-slate-800">自动交易范围：</span>先对当前账户执行止损保护（触发时卖出或减仓已有持仓），再仅对尚无持仓记录的新盘口买入；已结算或已止损卖出的盘口不再买入。</p>
            <p className="mt-1"><span className="font-medium text-slate-800">仍需通过：</span>单笔金额{orderSizeFor(accountVenue) > 0 ? `（${fmtMoney(orderSizeFor(accountVenue), accountVenue)}）` : ''}、单市场与总敞口上限、尾盘下单窗口、未决订单熔断及实盘凭据检查。</p>
            {isLive && <p className="mt-1 font-medium text-rose-700">实盘账户会向 {venueMeta(accountVenue).label} 提交真实买单或止损卖单，并动用真实 {venueMeta(accountVenue).collateral}。</p>}
            {isLive && !scopeMatchesAccount && <p className="mt-1 text-amber-700">当前账户属于 {venueMeta(accountVenue).label}，无法在 {venueMeta(scope).label} 范围内执行自动交易；可切换场所筛选，或本次只扫描行情。</p>}
          </div>
          {isLive && !venueReady(accountVenue) && <div className="flex items-start gap-2 rounded-xl border border-rose-200 bg-rose-50 p-3 text-sm leading-6 text-rose-700" role="alert"><ShieldAlert className="mt-0.5 h-4 w-4 shrink-0" /><span>{account?.live_configured ? '实盘凭据未就绪，本次只能刷新行情。' : '实盘凭据未配置，本次只能刷新行情。'} 请先在「系统设置 → 实盘交易凭据」中完成 {venueMeta(accountVenue).label} 的配置。</span></div>}
          <div className="flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
            <Button variant="secondary" onClick={() => runScan(false)} disabled={scanning}><Radar className="h-4 w-4" />仅扫描行情</Button>
            <Button variant={isLive ? 'danger' : 'primary'} onClick={() => runScan(true)} loading={scanning} disabled={!accountId || !venueReady(accountVenue) || !scopeMatchesAccount}><Zap className="h-4 w-4" />扫描并执行自动交易</Button>
          </div>
          <p className="-mt-2 text-center text-[11px] leading-5 text-slate-400">自动交易（含止损卖出）只对本次扫描与当前账户生效，会忽略该账户的后台自动交易开关与引擎暂停状态。</p>
        </DialogContent>
      </Dialog>

      <Dialog open={selected !== null} onOpenChange={(open) => { if (!open && !submitting) setSelected(null) }}>
        <DialogContent>
          {selected && <>
            <div>
              <div className="mb-3 flex flex-wrap items-center gap-2"><Badge variant={isLive ? 'danger' : 'success'}>{isLive ? <Radio className="h-3 w-3" /> : <FlaskConical className="h-3 w-3" />}{isLive ? '真实资金' : '虚拟资金'}</Badge><VenueChip venue={selectedVenue} full /><span className="text-xs text-slate-400">{isLive ? `订单将提交至 ${venueMeta(selectedVenue).label}（${venueMeta(selectedVenue).orderTypes.join(' / ')}）` : '不会发出真实订单'}</span></div>
              <DialogTitle>{isLive ? '确认实盘买入' : '确认模拟买入'}</DialogTitle>
              <DialogDescription className="mt-2">{isLive ? `按下确认后，系统会立即向 ${venueMeta(selectedVenue).label} 提交真实买单。成交价由当前订单簿决定，可能与下方指示价格不同。` : '仅在 PennyHarvester 内记录虚拟头寸，用于验证策略与风控规则。'}</DialogDescription>
            </div>
            <div className="rounded-xl border border-slate-200 bg-slate-50 p-4">
              <p className="pr-4 text-sm font-medium leading-6 text-slate-900">{marketQuestion(selected, marketLanguage)}</p>
              <div className="mt-3 flex flex-wrap items-center gap-2"><VenueChip venue={selectedVenue} /><Badge variant="info">{marketOutcome(selected, marketLanguage)}</Badge><Badge variant="default">{categoryLabel(selected.category)}</Badge>{selected.sports_game && <Badge variant="warning" title="体育赛事盘口可能包含加时、延长赛或点球决胜，临近结算时价格波动较大">赛事 · 加时风险</Badge>}<ProtocolBadge item={selected} /><span className="text-xs text-slate-500">指示价格 <span className="font-semibold text-slate-800">{formatPrice(selected.price)}</span></span><span className="text-xs text-slate-400">·</span><span className="text-xs text-slate-500">理论毛收益 <span className="text-emerald-700">{formatPercent(selected.gross_yield, 2, true)}</span></span></div>
            </div>
            {selectedHeld && <div className="flex items-start gap-2 rounded-xl border border-emerald-200 bg-emerald-50 p-3 text-xs leading-5 text-emerald-800" role="status"><CheckCircle2 className="mt-0.5 h-4 w-4 shrink-0" /><span><span className="font-medium">已买入：</span>买入价 {formatPrice(selectedHeld.entryPrice)} · {formatNumber(selectedHeld.shares, 4)} 份 · 成本 {fmtMoney(selectedHeld.cost, selectedHeld.venue)}{selectedHeld.count > 1 ? `（${selectedHeld.count} 笔）` : ''}。再次买入会增加该头寸。</span></div>}
            {selected.sports_game && <div className="flex items-start gap-2 rounded-xl border border-amber-200 bg-amber-50 p-3 text-xs leading-5 text-amber-800" role="alert"><ShieldAlert className="mt-0.5 h-4 w-4 shrink-0" /><span><span className="font-medium">体育赛事盘口：</span>可能包含加时、延长赛或点球决胜，临近结算时价格波动明显。请确认你已理解该风险。</span></div>}
            <div>
              <div className="mb-2 flex items-center justify-between"><label htmlFor="paper-amount" className="text-sm font-medium text-slate-700">{isLive ? '实盘投入金额' : '投入虚拟金额'}</label><span className="text-xs text-slate-400">{currencyOf(selectedVenue)}</span></div>
              <div className="relative"><CircleDollarSign className="absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-slate-400" /><Input id="paper-amount" type="number" min="0.01" step="any" inputMode="decimal" autoFocus value={amount} onChange={(event) => setAmount(event.target.value)} className="h-11 pl-9 text-base" placeholder="0.00" aria-describedby="amount-help" /></div>
              <div className="mt-2 flex items-center justify-between gap-4 text-xs" id="amount-help"><span className="text-slate-500">预计份额（{isLive ? '按指示价估算，实际以盘口为准' : '未计费用与滑点'}）</span><span className="font-medium tabular-nums text-slate-700">{validAmount && selected.price > 0 ? formatNumber(numericAmount / selected.price, 4) : '—'}</span></div>
              {amount && !validAmount && <p className="mt-2 text-xs text-rose-600">请输入大于 0 的有效金额。</p>}
            </div>
            <div className="rounded-xl border border-emerald-200 bg-emerald-50/70 p-3.5">
              <div className="flex flex-wrap items-center justify-between gap-2">
                <span className="text-xs font-medium text-emerald-900">盘口估算净收益（{venueMeta(selectedVenue).label} 盘口 + 手续费，随金额变化刷新）</span>
                <button type="button" onClick={() => fetchQuote(selected, validAmount ? numericAmount : (orderSizeFor(selectedVenue) || undefined))} disabled={selectedQuoteState?.loading} className="rounded-md px-1.5 py-0.5 text-[11px] font-medium text-emerald-800 underline-offset-2 transition hover:bg-emerald-100 hover:underline disabled:opacity-50">
                  {selectedQuoteState?.loading ? '读取中…' : `按 ${validAmount ? fmtMoney(numericAmount, selectedVenue) : orderSizeFor(selectedVenue) > 0 ? `单笔 ${fmtMoney(orderSizeFor(selectedVenue), selectedVenue)}` : '单笔金额'} 获取`}
                </button>
              </div>
              {selectedQuote ? <>
                <div className="mt-1.5 flex items-baseline justify-between gap-3"><span className="text-sm font-semibold tabular-nums text-emerald-800">{formatPercent(selectedQuote.net_yield, 2, true)}</span><span className="text-[11px] tabular-nums text-emerald-800/80">估算净收益 {fmtMoney(selectedQuote.net_profit, selectedVenue)}（非最终收益）</span></div>
                <p className="mt-1 text-[11px] leading-5 text-emerald-800/80">按 {fmtMoney(selectedQuote.amount, selectedVenue)} · 成交均价 {formatPrice(selectedQuote.fill_price)} · 限价 {formatPrice(selectedQuote.limit_price)} · 手续费 {fmtMoney(selectedQuote.fee, selectedVenue)} · 总成本 {fmtMoney(selectedQuote.total_cost, selectedVenue)}</p>
                {selectedQuote.exceeds_max_price && <p className="mt-1 text-[11px] leading-5 text-rose-700">盘口成交价超过最高买入价 {formatPrice(selectedQuote.max_price)}，实盘会被拒单。</p>}
                {!selectedQuote.fully_filled && <p className="mt-1 text-[11px] leading-5 text-amber-700">盘口深度仅 {fmtMoney(selectedQuote.available_notional, selectedVenue)}，无法一次成交该金额。</p>}
                {quoteTimeStale && <p className="mt-1 text-[11px] leading-5 text-amber-700" role="status">报价已超过 1 分钟，盘口可能变化；请重新获取。</p>}
                {!isLive && <p className="mt-1 text-[11px] leading-5 text-emerald-800/60">模拟账户仍按参数设置中的滑点与费率记账，此处仅供对照实盘口径。</p>}
              </> : selectedQuoteState?.loading ? <p className="mt-1 text-[11px] leading-5 text-emerald-800/70">正在读取当前金额的盘口估算…</p>
                : <p className="mt-1 text-[11px] leading-5 text-emerald-800/70">当前金额尚未获取报价；读取 {venueMeta(selectedVenue).label} 实时盘口与手续费后显示，不会提交订单。</p>}
              {selectedQuoteState?.error && <p className="mt-1 break-words text-[11px] leading-5 text-rose-700">报价失败：{selectedQuoteState.error}</p>}
            </div>
            <RiskNotice compact />
            <label className="flex cursor-pointer items-start gap-3 rounded-xl border border-slate-200 p-3.5 transition hover:bg-slate-50">
              <input type="checkbox" checked={confirmed} onChange={(event) => setConfirmed(event.target.checked)} className="mt-0.5 h-4 w-4 rounded border-slate-300 accent-emerald-600 dark:accent-emerald-500" />
              <span className="text-xs leading-5 text-slate-600">{isLive ? <><span className="font-medium text-rose-800">我确认这是真实资金实盘买入。</span> 我理解订单会立即提交到 {venueMeta(selectedVenue).label}，使用 {currencyOf(selectedVenue)} 结算，且高价结果失败时可能损失大部分本金。</> : <><span className="font-medium text-slate-800">我确认这是模拟买入。</span> 仅使用虚拟资金；当前价格不保证最终结算。</>}</span>
            </label>
            {venueBlockedReason(selectedVenue) && <div className="flex items-start gap-2 rounded-xl border border-rose-200 bg-rose-50 p-3 text-sm leading-6 text-rose-700" role="alert"><ShieldAlert className="mt-0.5 h-4 w-4 shrink-0" /><span>{venueBlockedReason(selectedVenue)}{isLive && <> {account?.live_configured ? (account.live_error || `请检查 ${venueMeta(selectedVenue).label} 的钱包、Funder 与 API 凭据。`) : `可在「系统设置 → 实盘交易凭据」中填写 ${venueMeta(selectedVenue).label} 的私钥，或设置 ${venueMeta(selectedVenue).envHint} 后重启服务。`}</>}</span></div>}
            {orderError && <div className="break-words rounded-xl border border-rose-200 bg-rose-50 p-3 text-sm leading-6 text-rose-700" role="alert"><span className="font-medium">{orderErrorUnknown ? '订单结果未知，请先刷新持仓核对：' : '订单未提交：'}</span>{orderError}</div>}
            <div className="flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
              <Button variant="secondary" onClick={() => setSelected(null)} disabled={submitting}>取消</Button>
              <Button variant={isLive ? 'danger' : 'primary'} onClick={submitOrder} loading={submitting} disabled={!validAmount || !confirmed || !venueReady(selectedVenue)}><CheckCircle2 className="h-4 w-4" />{submitting ? (isLive ? '正在提交实盘订单…' : '正在记录…') : `${isLive ? '确认实盘买入' : '确认模拟买入'}${validAmount ? ` ${fmtMoney(numericAmount, selectedVenue)}` : ''}`}</Button>
            </div>
            <div className="-mt-2 flex items-center justify-center gap-1.5 text-[11px] text-slate-400"><CalendarDays className="h-3 w-3" />市场到期：{formatFullDate(selected.end_date)}（本地时间）<ArrowUpRight className="h-3 w-3" /></div>
          </>}
        </DialogContent>
      </Dialog>
    </>
  )
}
