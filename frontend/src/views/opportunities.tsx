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
import type { Opportunity, OpportunityQuote } from '@/types'
import { formatCompactCurrency, formatCurrency, formatFullDate, formatNumber, formatPercent, formatPrice, formatPriceChange } from '@/lib/format'
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

function MarketLink({ url, compact = false }: { url: string; compact?: boolean }) {
  if (!/^https?:\/\//i.test(url)) return null
  return (
    <a href={url} target="_blank" rel="noreferrer" className={cn('inline-flex items-center justify-center gap-1.5 rounded-lg text-slate-400 transition hover:bg-slate-100 hover:text-slate-800 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-emerald-600', compact ? 'h-8 w-8' : 'px-2 py-1.5 text-xs')} aria-label="在新窗口打开 Polymarket 市场" title="查看公开市场">
      <ExternalLink className="h-3.5 w-3.5" />{!compact && '市场'}
    </a>
  )
}

interface QuoteState {
  quote?: OpportunityQuote
  error?: string
  loading?: boolean
}

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

function RealYield({ state, onQuote }: { state?: QuoteState; onQuote: () => void }) {
  const quote = state?.quote
  if (state?.loading && !quote) {
    return <span className="inline-flex items-center gap-1.5 text-xs text-slate-500"><RefreshCw className="h-3.5 w-3.5 animate-spin" />读取盘口…</span>
  }
  if (!quote) {
    return (
      <div className="flex flex-col items-end gap-1">
        <Button variant="ghost" size="sm" onClick={onQuote} title="读取 Polymarket 实时订单簿与 FeeSchedule，按参数设置中的单笔金额估算"><Zap className="h-3.5 w-3.5" />获取真实收益</Button>
        {state?.error && <span className="line-clamp-2 max-w-[210px] text-right text-[11px] leading-4 text-rose-600" title={state.error}>{state.error}</span>}
      </div>
    )
  }
  return (
    <div className="flex flex-col items-end gap-0.5 text-right">
      <span className={cn('font-semibold tabular-nums', quote.net_yield > 0 ? 'text-emerald-700' : 'text-rose-700')}>{formatPercent(quote.net_yield, 2, true)}</span>
      <span className="text-[11px] tabular-nums text-slate-500" title={`限价 ${formatPrice(quote.limit_price)} · 盘口可用 ${formatCurrency(quote.available_notional)}`}>成交均价 {formatPrice(quote.fill_price)} · 费 {formatCurrency(quote.fee)}</span>
      <span className="flex items-center gap-1 text-[11px] tabular-nums text-slate-400">
        {formatCurrency(quote.amount)} · {quoteAge(quote.quoted_at)}
        <button type="button" onClick={onQuote} disabled={state?.loading} className="rounded p-0.5 text-slate-400 transition hover:bg-slate-100 hover:text-slate-700 disabled:opacity-50" title="重新读取真实盘口与手续费" aria-label="刷新真实收益">
          <RefreshCw className={cn('h-3 w-3', state?.loading && 'animate-spin')} />
        </button>
      </span>
      {quote.exceeds_max_price && <span className="text-[11px] leading-4 text-rose-600">超出最高买入价，实盘会拒单</span>}
      {!quote.fully_filled && <span className="text-[11px] leading-4 text-amber-600">深度不足，仅可成交 {formatCompactCurrency(quote.available_notional)}</span>}
      {state?.error && <span className="line-clamp-2 max-w-[210px] text-right text-[11px] leading-4 text-rose-600" title={state.error}>{state.error}</span>}
    </div>
  )
}

function OpportunityCard({ item, onBuy, onQuote, quote, orderSize, isLive, language }: { item: Opportunity; onBuy: (item: Opportunity) => void; onQuote: () => void; quote?: QuoteState; orderSize: number | null; isLive: boolean; language: MarketLanguage }) {
  return (
    <Card className="p-4">
      <div className="flex items-start gap-3">
        <div className="min-w-0 flex-1">
          <div className="mb-2 flex flex-wrap items-center gap-1.5"><Badge variant="info">{marketOutcome(item, language)}</Badge><Badge variant="default">{categoryLabel(item.category)}</Badge>{item.sports_game && <Badge variant="warning" title="体育赛事盘口可能包含加时、延长赛或点球决胜，临近结算时价格波动较大">赛事 · 加时风险</Badge>}<span className="text-[11px] text-slate-400">#{item.market_id.slice(0, 10)}</span></div>
          <h3 className="text-sm font-medium leading-6 text-slate-900">{marketQuestion(item, language)}</h3>
        </div>
        <MarketLink url={marketURL(item.url, language)} compact />
      </div>
      <div className="mt-4 grid grid-cols-2 gap-3 rounded-xl bg-slate-50 p-3">
        <div><p className="text-[11px] text-slate-500">指示价格</p><p className="mt-1 text-sm font-semibold tabular-nums text-slate-900">{formatPrice(item.price)}</p>{item.price_change_24h != null && <p className={cn('mt-0.5 text-[11px] tabular-nums', Math.abs(item.price_change_24h) >= 0.05 ? 'text-amber-600' : 'text-slate-400')}>24h {formatPriceChange(item.price_change_24h)}</p>}</div>
        <div><p className="text-[11px] text-slate-500">毛收益</p><p className="mt-1 text-sm font-semibold tabular-nums text-emerald-700">{formatPercent(item.gross_yield, 2, true)}</p></div>
      </div>
      {item.sports_game && <p className="mt-3 flex items-start gap-1.5 rounded-xl border border-amber-200 bg-amber-50 px-3 py-2 text-[11px] leading-4 text-amber-800"><ShieldAlert className="mt-0.5 h-3.5 w-3.5 shrink-0" />体育赛事盘口可能包含加时、延长赛或点球决胜，临近结算时价格波动较大。</p>}
      <div className="mt-3 flex items-center justify-between gap-3 rounded-xl border border-slate-200 p-3">
        <div>
          <p className="text-[11px] text-slate-500">真实收益（含手续费）</p>
          <p className="text-[10px] text-slate-400">按单笔 {orderSize ? formatCurrency(orderSize) : '金额'} · 实时盘口 + FeeSchedule</p>
        </div>
        <RealYield state={quote} onQuote={onQuote} />
      </div>
      <div className="mt-3 flex items-center justify-between gap-3">
        <div className="flex flex-wrap gap-x-3 gap-y-1 text-[11px] text-slate-500"><span>流动性 {formatCompactCurrency(item.liquidity)}</span><span>{formatNumber(item.days_to_end, 1)} 天 · {formatFullDate(item.end_date)} 到期</span></div>
        <Button size="sm" variant={isLive ? 'danger' : 'primary'} onClick={() => onBuy(item)}>{isLive ? <Radio className="h-3.5 w-3.5" /> : <FlaskConical className="h-3.5 w-3.5" />}{isLive ? '实盘买入' : '模拟买入'}</Button>
      </div>
    </Card>
  )
}

export function OpportunitiesView({ onOrderCreated }: { onOrderCreated: () => void }) {
  const { account } = useAccount()
  const isLive = account?.mode === 'live'
  const liveReady = !isLive || Boolean(account?.live_ready)
  const [query, setQuery] = useState('')
  const [marketLanguage, setMarketLanguage] = useState<MarketLanguage>(readMarketLanguage)
  const [category, setCategory] = useState<CategoryFilter>('non_sports')
  const [sportsPolicy, setSportsPolicy] = useState('exclude_games')
  const categoryTouched = useRef(false)
  const [selected, setSelected] = useState<Opportunity | null>(null)
  const [amount, setAmount] = useState('')
  const [confirmed, setConfirmed] = useState(false)
  const [scanning, setScanning] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const [orderError, setOrderError] = useState('')
  const [quotes, setQuotes] = useState<Record<string, QuoteState>>({})
  const [orderSize, setOrderSize] = useState<number | null>(null)
  const scanController = useRef<AbortController | null>(null)
  const orderController = useRef<AbortController | null>(null)
  const quoteControllers = useRef<Map<string, AbortController>>(new Map())
  const { toast } = useToast()
  const fetchOpportunities = useCallback((signal: AbortSignal) => api.opportunities(signal), [])
  const { data, error, isLoading, isRefreshing, refresh } = usePolling(fetchOpportunities)

  useEffect(() => () => {
    scanController.current?.abort()
    orderController.current?.abort()
    quoteControllers.current.forEach((controller) => controller.abort())
    quoteControllers.current.clear()
  }, [])

  useEffect(() => {
    persistMarketLanguage(marketLanguage)
  }, [marketLanguage])

  // The real-yield button quotes the configured single-order size. Show it so
  // the amount is not mistaken for a hardcoded value; it is edited in 参数设置.
  // The same request supplies the default radar category. A manual category
  // selection must not be overwritten by a later poll.
  useEffect(() => {
    const controller = new AbortController()
    api.settings(controller.signal).then((settings) => {
      setOrderSize(settings.order_size)
      setSportsPolicy(settings.sports_policy)
      if (!categoryTouched.current) setCategory(normalizeCategoryFilter(settings.default_category))
    }).catch(() => {})
    return () => controller.abort()
  }, [])

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
  const selectedQuote = selected ? quotes[selected.token_id]?.quote : undefined

  const fetchQuote = async (item: Opportunity, quoteAmount?: number) => {
    quoteControllers.current.get(item.token_id)?.abort()
    const controller = new AbortController()
    quoteControllers.current.set(item.token_id, controller)
    setQuotes((current) => ({ ...current, [item.token_id]: { ...current[item.token_id], loading: true, error: undefined } }))
    try {
      const quote = await api.quoteOpportunity(item.token_id, quoteAmount, controller.signal)
      if (!quoteAmount) setOrderSize(quote.amount)
      setQuotes((current) => ({ ...current, [item.token_id]: { quote } }))
    } catch (caught) {
      if (caught instanceof DOMException && caught.name === 'AbortError') return
      setQuotes((current) => ({ ...current, [item.token_id]: { ...current[item.token_id], loading: false, error: caught instanceof Error ? caught.message : '获取真实收益失败，请重试。' } }))
    } finally {
      if (quoteControllers.current.get(item.token_id) === controller) quoteControllers.current.delete(item.token_id)
    }
  }

  const openBuy = (item: Opportunity) => {
    setSelected(item)
    setAmount('')
    setConfirmed(false)
    setOrderError('')
  }

  const scan = async () => {
    scanController.current?.abort()
    const controller = new AbortController()
    scanController.current = controller
    setScanning(true)
    try {
      const result = await api.scan(controller.signal)
      toast({ title: '扫描完成', description: `本次返回 ${formatNumber(result.total, 0)} 个候选机会；仅刷新行情，未自动下单。`, variant: 'success' })
      refresh()
    } catch (caught) {
      if (caught instanceof DOMException && caught.name === 'AbortError') return
      toast({ title: '扫描失败', description: caught instanceof Error ? caught.message : '请稍后重试。', variant: 'error' })
    } finally {
      if (scanController.current === controller) setScanning(false)
    }
  }

  const submitOrder = async () => {
    if (!selected || !validAmount || !confirmed) return
    orderController.current?.abort()
    const controller = new AbortController()
    orderController.current = controller
    setSubmitting(true)
    setOrderError('')
    try {
      if (!account) throw new Error('尚未选择账户')
      const position = await api.createOrder(account.id, {
        market_id: selected.market_id,
        outcome: selected.outcome,
        token_id: selected.token_id,
        amount: numericAmount,
      }, controller.signal)
      toast({ title: isLive ? '实盘买入已成交' : '模拟买入已成交', description: `已创建${isLive ? '实盘' : '模拟'}持仓 #${position.id}，成交份额 ${formatNumber(position.shares, 4)}。`, variant: 'success' })
      setSelected(null)
      onOrderCreated()
    } catch (caught) {
      if (caught instanceof DOMException && caught.name === 'AbortError') return
      setOrderError(caught instanceof Error ? caught.message : (isLive ? '实盘订单提交失败，请重试。' : '模拟订单提交失败，请重试。'))
    } finally {
      if (orderController.current === controller) setSubmitting(false)
    }
  }

  return (
    <>
      <PageHeader eyebrow={isLive ? 'LIVE OPPORTUNITY RADAR' : 'OPPORTUNITY RADAR'} title="机会雷达" description={isLive ? '筛选公开市场价格偏离。点击实盘买入会把订单提交到 Polymarket CLOB，请务必确认金额与盘口深度。' : '筛选公开市场的价格偏离，以理论收益辅助研究，不构成结算承诺。'} actions={<><MarketLanguageSelect value={marketLanguage} onChange={setMarketLanguage} /><Button variant="primary" onClick={scan} loading={scanning}><Radar className={cn('h-4 w-4', !scanning && 'text-emerald-200 dark:text-emerald-950')} />{scanning ? '正在扫描…' : '立即扫描'}</Button></>} />
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
            <Button variant="ghost" size="sm" onClick={refresh} disabled={isRefreshing}><RefreshCw className={cn('h-3.5 w-3.5', isRefreshing && 'animate-spin')} />刷新</Button>
          </div>
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
          <span>真实收益 = 实时盘口成交价 + 真实手续费，按「参数设置 → 单笔金额」{orderSize ? `（${formatCurrency(orderSize)}）` : ''}估算</span>
          <span>到期时间按本地时区显示</span>
          <span>立即扫描仅刷新行情，不会自动下单</span>
          <span>默认分类来自「参数设置」；体育赛事盘口默认按加时/波动风险过滤</span>
          <span>{marketLanguage === 'zh' ? '市场问题与方向优先使用 Polymarket 中文文案' : '市场问题与方向显示 Polymarket 英文原文'}</span>
          {data && <span className="sm:ml-auto">来源 {data.source} · 扫描于 {formatFullDate(data.scanned_at)}</span>}
        </div>
      </Card>

      {isLoading ? <Card><LoadingBlock rows={6} /></Card> : error && !data ? <Card><ErrorState message={error} onRetry={refresh} /></Card> : !data?.items.length ? (
        <Card><EmptyState icon={Binoculars} title="雷达暂未发现机会" description="当前筛选条件下没有候选市场。你可以重新扫描，或在参数设置中调整筛选阈值。" action={<Button variant="secondary" onClick={scan} loading={scanning}><Radar className="h-4 w-4" />重新扫描</Button>} /></Card>
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
                  <tr><th className="px-5 py-3.5 font-medium">市场 / 方向</th><th className="px-4 py-3.5 text-right font-medium">指示价格</th><th className="px-4 py-3.5 text-right font-medium">毛收益</th><th className="px-4 py-3.5 text-right font-medium">真实收益<p className="mt-0.5 text-[10px] font-normal text-slate-400">按单笔 {orderSize ? formatCurrency(orderSize) : '金额'}</p></th><th className="px-4 py-3.5 text-right font-medium">流动性</th><th className="px-4 py-3.5 text-right font-medium">到期</th><th className="px-5 py-3.5 text-right font-medium">操作</th></tr>
                </thead>
                <tbody className={cn('divide-y divide-slate-100 transition-opacity', isRefreshing && 'opacity-60')}>
                  {filtered.map((item) => (
                    <tr key={`${item.market_id}-${item.token_id}`} className="group hover:bg-slate-50/70">
                      <td className="max-w-[390px] px-5 py-4"><div className="flex items-start gap-3"><div className="mt-0.5 rounded-lg bg-slate-100 p-2 text-slate-400 group-hover:bg-surface group-hover:text-emerald-600"><Sparkles className="h-4 w-4" /></div><div className="min-w-0"><p className="line-clamp-2 font-medium leading-5 text-slate-800" title={marketQuestion(item, marketLanguage)}>{marketQuestion(item, marketLanguage)}</p><div className="mt-1.5 flex flex-wrap items-center gap-1.5"><Badge variant="info" className="px-2 py-0.5 text-[11px]">{marketOutcome(item, marketLanguage)}</Badge><Badge variant="default" className="px-2 py-0.5 text-[11px]">{categoryLabel(item.category)}</Badge>{item.sports_game && <Badge variant="warning" className="px-2 py-0.5 text-[11px]" title="体育赛事盘口可能包含加时、延长赛或点球决胜，临近结算时价格波动较大">赛事 · 加时风险</Badge>}<span className="text-[11px] text-slate-400">{item.slug || item.market_id.slice(0, 12)}</span></div></div></div></td>
                      <td className="px-4 py-4 text-right"><div className="font-semibold tabular-nums text-slate-800">{formatPrice(item.price)}</div>{item.price_change_24h != null && <p className={cn('mt-1 text-[11px] tabular-nums', Math.abs(item.price_change_24h) >= 0.05 ? 'text-amber-600' : 'text-slate-400')}>24h {formatPriceChange(item.price_change_24h)}</p>}</td>
                      <td className="px-4 py-4 text-right font-medium tabular-nums text-emerald-700">{formatPercent(item.gross_yield, 2, true)}</td>
                      <td className="px-4 py-4 text-right"><RealYield state={quotes[item.token_id]} onQuote={() => fetchQuote(item)} /></td>
                      <td className="px-4 py-4 text-right tabular-nums text-slate-600">{formatCompactCurrency(item.liquidity)}</td>
                      <td className="px-4 py-4 text-right"><span className="tabular-nums text-slate-700">{formatNumber(item.days_to_end, 1)} 天</span><p className="mt-1 whitespace-nowrap text-[11px] tabular-nums text-slate-400">{formatFullDate(item.end_date)}</p></td>
                      <td className="px-5 py-4"><div className="flex items-center justify-end gap-1"><MarketLink url={marketURL(item.url, marketLanguage)} compact /><Button size="sm" variant="secondary" className={isLive ? 'text-rose-700 group-hover:border-rose-200 group-hover:bg-rose-50 group-hover:text-rose-800' : 'group-hover:border-emerald-200 group-hover:bg-emerald-50 group-hover:text-emerald-800'} onClick={() => openBuy(item)}>{isLive ? <Radio className="h-3.5 w-3.5" /> : <FlaskConical className="h-3.5 w-3.5" />}{isLive ? '实盘买入' : '模拟买入'}</Button></div></td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </Card>
          <div className="grid gap-3 sm:grid-cols-2 lg:hidden">{filtered.map((item) => <OpportunityCard key={`${item.market_id}-${item.token_id}`} item={item} onBuy={openBuy} onQuote={() => fetchQuote(item)} quote={quotes[item.token_id]} orderSize={orderSize} isLive={isLive} language={marketLanguage} />)}</div>
        </>
      )}

      <Dialog open={selected !== null} onOpenChange={(open) => { if (!open && !submitting) setSelected(null) }}>
        <DialogContent>
          {selected && <>
            <div>
              <div className="mb-3 flex items-center gap-2"><Badge variant={isLive ? 'danger' : 'success'}>{isLive ? <Radio className="h-3 w-3" /> : <FlaskConical className="h-3 w-3" />}{isLive ? '真实资金' : '虚拟资金'}</Badge><span className="text-xs text-slate-400">{isLive ? '订单将提交至 Polymarket CLOB' : '不会发出真实订单'}</span></div>
              <DialogTitle>{isLive ? '确认实盘买入' : '确认模拟买入'}</DialogTitle>
              <DialogDescription className="mt-2">{isLive ? '按下确认后，系统会立即向 Polymarket 提交真实买单。成交价由当前订单簿决定，可能与下方指示价格不同。' : '仅在 PennyHarvester 内记录虚拟头寸，用于验证策略与风控规则。'}</DialogDescription>
            </div>
            <div className="rounded-xl border border-slate-200 bg-slate-50 p-4">
              <p className="pr-4 text-sm font-medium leading-6 text-slate-900">{marketQuestion(selected, marketLanguage)}</p>
              <div className="mt-3 flex flex-wrap items-center gap-2"><Badge variant="info">{marketOutcome(selected, marketLanguage)}</Badge><Badge variant="default">{categoryLabel(selected.category)}</Badge>{selected.sports_game && <Badge variant="warning" title="体育赛事盘口可能包含加时、延长赛或点球决胜，临近结算时价格波动较大">赛事 · 加时风险</Badge>}<span className="text-xs text-slate-500">指示价格 <span className="font-semibold text-slate-800">{formatPrice(selected.price)}</span></span><span className="text-xs text-slate-400">·</span><span className="text-xs text-slate-500">理论毛收益 <span className="text-emerald-700">{formatPercent(selected.gross_yield, 2, true)}</span></span></div>
            </div>
            {selected.sports_game && <div className="flex items-start gap-2 rounded-xl border border-amber-200 bg-amber-50 p-3 text-xs leading-5 text-amber-800" role="alert"><ShieldAlert className="mt-0.5 h-4 w-4 shrink-0" /><span><span className="font-medium">体育赛事盘口：</span>可能包含加时、延长赛或点球决胜，临近结算时价格波动明显。请确认你已理解该风险。</span></div>}
            <div>
              <div className="mb-2 flex items-center justify-between"><label htmlFor="paper-amount" className="text-sm font-medium text-slate-700">{isLive ? '实盘投入金额' : '投入虚拟金额'}</label><span className="text-xs text-slate-400">USD</span></div>
              <div className="relative"><CircleDollarSign className="absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-slate-400" /><Input id="paper-amount" type="number" min="0.01" step="any" inputMode="decimal" autoFocus value={amount} onChange={(event) => setAmount(event.target.value)} className="h-11 pl-9 text-base" placeholder="0.00" aria-describedby="amount-help" /></div>
              <div className="mt-2 flex items-center justify-between gap-4 text-xs" id="amount-help"><span className="text-slate-500">预计份额（{isLive ? '按指示价估算，实际以盘口为准' : '未计费用与滑点'}）</span><span className="font-medium tabular-nums text-slate-700">{validAmount && selected.price > 0 ? formatNumber(numericAmount / selected.price, 4) : '—'}</span></div>
              {amount && !validAmount && <p className="mt-2 text-xs text-rose-600">请输入大于 0 的有效金额。</p>}
            </div>
            <div className="rounded-xl border border-emerald-200 bg-emerald-50/70 p-3.5">
              <div className="flex flex-wrap items-center justify-between gap-2">
                <span className="text-xs font-medium text-emerald-900">真实净收益（实盘盘口 + FeeSchedule）</span>
                <button type="button" onClick={() => fetchQuote(selected, validAmount ? numericAmount : undefined)} disabled={quotes[selected.token_id]?.loading} className="rounded-md px-1.5 py-0.5 text-[11px] font-medium text-emerald-800 underline-offset-2 transition hover:bg-emerald-100 hover:underline disabled:opacity-50">
                  {quotes[selected.token_id]?.loading ? '读取中…' : `按 ${validAmount ? formatCurrency(numericAmount) : orderSize ? `单笔 ${formatCurrency(orderSize)}` : '单笔金额'} 获取`}
                </button>
              </div>
              {selectedQuote ? <>
                <div className="mt-1.5 flex items-baseline justify-between gap-3"><span className="text-sm font-semibold tabular-nums text-emerald-800">{formatPercent(selectedQuote.net_yield, 2, true)}</span><span className="text-[11px] tabular-nums text-emerald-800/80">净利 {formatCurrency(selectedQuote.net_profit)}</span></div>
                <p className="mt-1 text-[11px] leading-5 text-emerald-800/80">按 {formatCurrency(selectedQuote.amount)} · 成交均价 {formatPrice(selectedQuote.fill_price)} · 限价 {formatPrice(selectedQuote.limit_price)} · 手续费 {formatCurrency(selectedQuote.fee)} · 总成本 {formatCurrency(selectedQuote.total_cost)}</p>
                {selectedQuote.exceeds_max_price && <p className="mt-1 text-[11px] leading-5 text-rose-700">盘口成交价超过最高买入价 {formatPrice(selectedQuote.max_price)}，实盘会被拒单。</p>}
                {!selectedQuote.fully_filled && <p className="mt-1 text-[11px] leading-5 text-amber-700">盘口深度仅 {formatCompactCurrency(selectedQuote.available_notional)}，无法一次成交该金额。</p>}
                {!isLive && <p className="mt-1 text-[11px] leading-5 text-emerald-800/60">模拟账户仍按参数设置中的滑点与费率记账，此处仅供对照实盘口径。</p>}
              </> : <p className="mt-1 text-[11px] leading-5 text-emerald-800/70">读取 Polymarket 实时订单簿与 taker 手续费后显示，不会提交订单。</p>}
              {quotes[selected.token_id]?.error && <p className="mt-1 break-words text-[11px] leading-5 text-rose-700">报价失败：{quotes[selected.token_id]?.error}</p>}
            </div>
            <RiskNotice compact />
            <label className="flex cursor-pointer items-start gap-3 rounded-xl border border-slate-200 p-3.5 transition hover:bg-slate-50">
              <input type="checkbox" checked={confirmed} onChange={(event) => setConfirmed(event.target.checked)} className="mt-0.5 h-4 w-4 rounded border-slate-300 accent-emerald-600 dark:accent-emerald-500" />
              <span className="text-xs leading-5 text-slate-600">{isLive ? <><span className="font-medium text-rose-800">我确认这是真实资金实盘买入。</span> 我理解订单会立即提交到 Polymarket，且高价结果失败时可能损失大部分本金。</> : <><span className="font-medium text-slate-800">我确认这是模拟买入。</span> 仅使用虚拟资金；当前价格不保证最终结算。</>}</span>
            </label>
            {!liveReady && <div className="flex items-start gap-2 rounded-xl border border-rose-200 bg-rose-50 p-3 text-sm leading-6 text-rose-700" role="alert"><ShieldAlert className="mt-0.5 h-4 w-4 shrink-0" /><span><span className="font-medium">{account?.live_configured ? '实盘凭据未就绪。' : '实盘凭据未配置。'}</span> {account?.live_configured ? (account.live_error || '请检查 Funder、CLOB 凭据与 USDC 授权。') : '可在「系统设置 → 实盘交易凭据」中填写私钥，或设置 POLYMARKET_PRIVATE_KEY 后重启服务。'}</span></div>}
            {orderError && <div className="break-words rounded-xl border border-rose-200 bg-rose-50 p-3 text-sm leading-6 text-rose-700" role="alert"><span className="font-medium">订单未提交：</span>{orderError}</div>}
            <div className="flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
              <Button variant="secondary" onClick={() => setSelected(null)} disabled={submitting}>取消</Button>
              <Button variant={isLive ? 'danger' : 'primary'} onClick={submitOrder} loading={submitting} disabled={!validAmount || !confirmed || !liveReady}><CheckCircle2 className="h-4 w-4" />{submitting ? (isLive ? '正在提交实盘订单…' : '正在记录…') : `${isLive ? '确认实盘买入' : '确认模拟买入'}${validAmount ? ` ${formatCurrency(numericAmount)}` : ''}`}</Button>
            </div>
            <div className="-mt-2 flex items-center justify-center gap-1.5 text-[11px] text-slate-400"><CalendarDays className="h-3 w-3" />市场到期：{formatFullDate(selected.end_date)}（本地时间）<ArrowUpRight className="h-3 w-3" /></div>
          </>}
        </DialogContent>
      </Dialog>
    </>
  )
}
