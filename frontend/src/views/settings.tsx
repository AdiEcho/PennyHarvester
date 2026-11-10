import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  Activity,
  Clock,
  FlaskConical,
  Gauge,
  Info,
  Plus,
  Radio,
  RotateCcw,
  Save,
  ShieldAlert,
  ShieldCheck,
  SlidersHorizontal,
  Star,
  TimerReset,
  Trash2,
  UserCog,
} from 'lucide-react'
import { api } from '@/lib/api'
import { useAccount } from '@/lib/account-context'
import { usePolling } from '@/hooks/usePolling'
import type { Account, AccountMode, Engine, Settings, Venue, VenueSettings } from '@/types'
import type { Currency } from '@/lib/venue'
import { fmtMoney, VENUES, venueLabel, venueMeta, venueOf } from '@/lib/venue'
import { cn } from '@/lib/utils'
import { useToast } from '@/components/toast'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogTitle } from '@/components/ui/dialog'
import { ErrorState, Skeleton } from '@/components/ui/feedback'
import { Input } from '@/components/ui/input'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import { TabBar, type TabItem } from '@/components/ui/tab-bar'
import { PageHeader } from '@/components/page-header'
import { ModeBadge, VenueChip } from '@/components/status-badges'

const numericKeys = ['min_price', 'max_price', 'max_days', 'min_liquidity', 'order_size', 'max_position', 'max_exposure', 'max_event_exposure', 'scan_interval_seconds', 'slippage_bps', 'fee_bps', 'initial_bankroll', 'max_price_change_24h', 'entry_window_minutes', 'sports_entry_window_minutes', 'crypto_entry_window_seconds', 'stop_loss_ratio'] as const
const selectKeys = ['default_category', 'sports_policy'] as const
type NumericKey = typeof numericKeys[number]
type SelectKey = typeof selectKeys[number]
type SharedDraft = Record<NumericKey, string> & Record<SelectKey, string> & { stop_loss_enabled: boolean }

const venueNumericKeys = ['min_liquidity', 'order_size', 'max_position', 'max_exposure', 'fee_bps'] as const
type VenueNumericKey = typeof venueNumericKeys[number]
type VenueDraft = Record<VenueNumericKey, string> & { enabled: boolean }

interface Draft {
  shared: SharedDraft
  venues: Record<Venue, VenueDraft>
}

// Error keys are either a shared numeric key or `${venue}.${venueKey}` so each
// tab only shows the errors that belong to it.
type FieldErrors = Record<string, string | undefined>
type SettingsTab = 'shared' | Venue

const defaultCategoryOptions = [
  { value: 'non_sports', label: '非体育（推荐）' },
  { value: 'all', label: '全部分类' },
  { value: 'politics', label: '政治' },
  { value: 'crypto', label: '加密' },
  { value: 'finance', label: '金融' },
  { value: 'tech', label: '科技' },
  { value: 'culture', label: '文化' },
  { value: 'sports', label: '体育' },
  { value: 'other', label: '其他' },
]

const sportsPolicyOptions = [
  { value: 'exclude_games', label: '排除体育赛事盘口（推荐）' },
  { value: 'exclude_all', label: '排除全部体育分类' },
  { value: 'allow', label: '保留体育市场并标记风险' },
]

function toVenueDraft(venue: Venue, value?: VenueSettings): VenueDraft {
  // 0 / missing means "inherit the global value", so it maps to an empty field
  // whose placeholder shows the inherited number. A venue with no stored entry
  // keeps its platform default (Predict.fun stays disabled until enabled here).
  return {
    enabled: value?.enabled ?? venueMeta(venue).enabledByDefault,
    min_liquidity: value?.min_liquidity ? String(value.min_liquidity) : '',
    order_size: value?.order_size ? String(value.order_size) : '',
    max_position: value?.max_position ? String(value.max_position) : '',
    max_exposure: value?.max_exposure ? String(value.max_exposure) : '',
    fee_bps: value?.fee_bps ? String(value.fee_bps) : '',
  }
}

function toDraft(settings: Settings): Draft {
  return {
    shared: {
      min_price: String(settings.min_price),
      max_price: String(settings.max_price),
      max_days: String(settings.max_days),
      min_liquidity: String(settings.min_liquidity),
      order_size: String(settings.order_size),
      max_position: String(settings.max_position),
      max_exposure: String(settings.max_exposure),
      max_event_exposure: String(settings.max_event_exposure ?? 0),
      scan_interval_seconds: String(settings.scan_interval_seconds),
      slippage_bps: String(settings.slippage_bps),
      fee_bps: String(settings.fee_bps),
      initial_bankroll: String(settings.initial_bankroll),
      default_category: settings.default_category,
      sports_policy: settings.sports_policy,
      max_price_change_24h: String(settings.max_price_change_24h),
      entry_window_minutes: String(settings.entry_window_minutes),
      sports_entry_window_minutes: String(settings.sports_entry_window_minutes),
      crypto_entry_window_seconds: String(settings.crypto_entry_window_seconds),
      stop_loss_ratio: String(settings.stop_loss_ratio),
      stop_loss_enabled: settings.stop_loss_enabled,
    },
    venues: {
      polymarket: toVenueDraft('polymarket', settings.venues?.polymarket),
      predictfun: toVenueDraft('predictfun', settings.venues?.predictfun),
    },
  }
}

function toShared(draft: SharedDraft) {
  return {
    min_price: Number(draft.min_price),
    max_price: Number(draft.max_price),
    max_days: Number(draft.max_days),
    min_liquidity: Number(draft.min_liquidity),
    order_size: Number(draft.order_size),
    max_position: Number(draft.max_position),
    max_exposure: Number(draft.max_exposure),
    max_event_exposure: Number(draft.max_event_exposure),
    scan_interval_seconds: Number(draft.scan_interval_seconds),
    slippage_bps: Number(draft.slippage_bps),
    fee_bps: Number(draft.fee_bps),
    initial_bankroll: Number(draft.initial_bankroll),
    default_category: draft.default_category,
    sports_policy: draft.sports_policy,
    max_price_change_24h: Number(draft.max_price_change_24h),
    entry_window_minutes: Number(draft.entry_window_minutes),
    sports_entry_window_minutes: Number(draft.sports_entry_window_minutes),
    crypto_entry_window_seconds: Number(draft.crypto_entry_window_seconds),
    stop_loss_ratio: Number(draft.stop_loss_ratio),
    stop_loss_enabled: draft.stop_loss_enabled,
  }
}

/** Empty field = inherit (0), per the API contract. */
function numberOrZero(value: string): number {
  return value.trim() === '' ? 0 : Number(value)
}

/**
 * Builds the PUT body from the loaded object, never from a fresh literal: fields
 * the form does not render (new backend fields, sibling venue values) survive.
 */
function toPayload(base: Settings, draft: Draft): Settings {
  const venues = {} as Record<Venue, VenueSettings>
  for (const venue of VENUES) {
    const loaded = base.venues?.[venue]
    const current = draft.venues[venue]
    venues[venue] = {
      ...(loaded ?? { enabled: venueMeta(venue).enabledByDefault, min_liquidity: 0, order_size: 0, max_position: 0, max_exposure: 0, fee_bps: 0 }),
      enabled: current.enabled,
      min_liquidity: numberOrZero(current.min_liquidity),
      order_size: numberOrZero(current.order_size),
      max_position: numberOrZero(current.max_position),
      max_exposure: numberOrZero(current.max_exposure),
      fee_bps: numberOrZero(current.fee_bps),
    }
  }
  return { ...base, ...toShared(draft.shared), venues }
}

// Cross-field rules compare *effective* values (override or inherited) so an
// inherited single-order size cannot silently exceed its venue caps.
function validateShared(shared: SharedDraft): FieldErrors {
  const errors: FieldErrors = {}
  const settings = toShared(shared)
  for (const key of numericKeys) {
    if (!Number.isFinite(settings[key])) errors[key] = '请输入有效数字。'
  }
  if (settings.min_price <= 0) errors.min_price = '最低价格必须大于 0。'
  if (settings.max_price >= 1) errors.max_price = '最高价格必须小于 1。'
  if (settings.min_price > settings.max_price) {
    errors.min_price = '最低价格不能高于最高价格。'
    errors.max_price = '最高价格不能低于最低价格。'
  }
  if (!Number.isInteger(settings.max_days) || settings.max_days < 1 || settings.max_days > 3650) errors.max_days = '请输入 1–3650 之间的整数天数。'
  if (settings.min_liquidity < 0) errors.min_liquidity = '最低流动性不能小于 0。'
  if (settings.order_size <= 0) errors.order_size = '单笔金额必须大于 0。'
  if (settings.max_position < settings.order_size) errors.max_position = '单市场持仓上限不能低于单笔金额。'
  if (settings.max_exposure < settings.max_position) errors.max_exposure = '总敞口上限不能低于单市场持仓上限。'
  if (settings.max_event_exposure < 0) errors.max_event_exposure = '单事件敞口上限不能小于 0（0 表示不限）。'
  else if (settings.max_event_exposure > 0 && settings.max_event_exposure < settings.order_size) errors.max_event_exposure = '单事件敞口上限不能低于单笔金额（0 表示不限）。'
  if (!Number.isInteger(settings.scan_interval_seconds) || settings.scan_interval_seconds < 10 || settings.scan_interval_seconds > 86400) errors.scan_interval_seconds = '请输入 10–86400 之间的整数秒数。'
  if (settings.slippage_bps < 0 || settings.slippage_bps > 1000) errors.slippage_bps = '滑点范围为 0–1000 bps。'
  if (settings.fee_bps < 0 || settings.fee_bps > 1000) errors.fee_bps = '费用范围为 0–1000 bps。'
  if (settings.initial_bankroll <= 0) errors.initial_bankroll = '模拟初始资金必须大于 0。'
  if (!defaultCategoryOptions.some((option) => option.value === settings.default_category)) errors.default_category = '请选择有效的默认分类。'
  if (!sportsPolicyOptions.some((option) => option.value === settings.sports_policy)) errors.sports_policy = '请选择有效的体育市场策略。'
  if (settings.max_price_change_24h < 0 || settings.max_price_change_24h > 1) errors.max_price_change_24h = '请输入 0–1 之间的数值（0 表示不限制）。'
  if (!Number.isInteger(settings.entry_window_minutes) || settings.entry_window_minutes < 0 || settings.entry_window_minutes > 1440) errors.entry_window_minutes = '请输入 0–1440 之间的整数分钟数。'
  if (!Number.isInteger(settings.sports_entry_window_minutes) || settings.sports_entry_window_minutes < 0 || settings.sports_entry_window_minutes > 1440) errors.sports_entry_window_minutes = '请输入 0–1440 之间的整数分钟数。'
  if (!Number.isInteger(settings.crypto_entry_window_seconds) || settings.crypto_entry_window_seconds < 0 || settings.crypto_entry_window_seconds > 86400) errors.crypto_entry_window_seconds = '请输入 0–86400 之间的整数秒数。'
  if (settings.stop_loss_ratio <= 0 || settings.stop_loss_ratio >= 1) errors.stop_loss_ratio = '请输入 0–1 之间的小数，例如 0.70。'
  return errors
}

/** One venue whose effective order size could not produce a sellable position. */
interface StopLossFloorViolation {
  venue: Venue
  label: string
  currency: Currency
  floor: number
  orderSize: number
}

/**
 * Venues where the stop-loss would promise an exit the exchange refuses. A buy
 * is sized in money, but the exit is a limit order the venue floors by shares,
 * so a per-venue order size below that floor always lands under it (a share
 * costs less than one currency unit). The backend refuses to enable the
 * stop-loss in that state; the form mirrors it so the reason is visible before
 * the save round-trip.
 */
function stopLossFloorViolations(draft: Draft): StopLossFloorViolation[] {
  if (!draft.shared.stop_loss_enabled) return []
  const shared = toShared(draft.shared)
  const violations: StopLossFloorViolation[] = []
  for (const venue of VENUES) {
    const meta = venueMeta(venue)
    if (!meta.minSellShares) continue
    if (!draft.venues[venue].enabled) continue
    const orderSize = numberOrZero(draft.venues[venue].order_size) || shared.order_size
    if (orderSize + 1e-9 >= meta.minSellShares) continue
    violations.push({ venue, label: meta.label, currency: meta.currency, floor: meta.minSellShares, orderSize })
  }
  return violations
}

function validate(draft: Draft): FieldErrors {
  const errors = validateShared(draft.shared)
  const shared = toShared(draft.shared)
  for (const venue of VENUES) {
    const current = draft.venues[venue]
    const key = (field: VenueNumericKey) => `${venue}.${field}`
    for (const field of venueNumericKeys) {
      if (current[field].trim() !== '' && !Number.isFinite(Number(current[field]))) {
        errors[key(field)] = '请输入有效数字；留空表示继承账户策略值。'
      }
    }
    const orderSize = numberOrZero(current.order_size) || shared.order_size
    const maxPosition = numberOrZero(current.max_position) || shared.max_position
    const maxExposure = numberOrZero(current.max_exposure) || shared.max_exposure
    const feeBps = numberOrZero(current.fee_bps)
    if (current.order_size.trim() !== '' && orderSize <= 0) errors[key('order_size')] = '单笔金额必须大于 0。'
    if (maxPosition < orderSize) errors[key('max_position')] = `单市场持仓上限不能低于单笔金额（当前生效 ${orderSize}）。`
    if (maxExposure < maxPosition) errors[key('max_exposure')] = `总敞口上限不能低于单市场持仓上限（当前生效 ${maxPosition}）。`
    if ((current.fee_bps.trim() !== '' && (feeBps < 0 || feeBps > 1000)) || feeBps > 1000) errors[key('fee_bps')] = '费用范围为 0–1000 bps（留空表示继承账户策略值）。'
    if (current.min_liquidity.trim() !== '' && numberOrZero(current.min_liquidity) < 0) errors[key('min_liquidity')] = '最低流动性不能小于 0。'
  }
  // The stop-loss sells a whole position as a limit order, and every venue floors
  // such an order by shares. An order size in money below that floor buys a
  // position the bot can never exit, so the backend refuses to enable the
  // stop-loss then; mirror the rule here so the reason lands on the field.
  for (const violation of stopLossFloorViolations(draft)) {
    const message = `开启自动止损需要${violation.label}单笔金额不低于 ${violation.floor} ${violation.currency}（${violation.label} 卖单最小 ${violation.floor} 份，单笔金额更低就买不到能卖出的份额）。`
    if (draft.venues[violation.venue].order_size.trim() !== '') errors[`${violation.venue}.order_size`] = message
    else errors.order_size = message
  }
  return errors
}

function NumberField({ id, label, description, value, unit, error, min, max, step = 'any', placeholder, onChange }: {
  id: string
  label: string
  description: string
  value: string
  unit: string
  error?: string
  min?: number
  max?: number
  step?: string
  placeholder?: string
  onChange: (value: string) => void
}) {
  return (
    <div>
      <label htmlFor={id} className="text-sm font-medium text-slate-700">{label}</label>
      <p className="mt-1 min-h-8 text-xs leading-4 text-slate-400">{description}</p>
      <div className="relative mt-2">
        <Input id={id} type="number" inputMode="decimal" value={value} min={min} max={max} step={step} placeholder={placeholder} onChange={(event) => onChange(event.target.value)} aria-invalid={Boolean(error)} aria-describedby={error ? `${id}-error` : undefined} className={cn('pr-20 tabular-nums', error && 'border-rose-400 focus:border-rose-500 focus:ring-rose-500/10')} />
        <span className="pointer-events-none absolute right-3 top-1/2 -translate-y-1/2 text-xs text-slate-400">{unit}</span>
      </div>
      {error && <p id={`${id}-error`} className="mt-1.5 text-xs leading-4 text-rose-600" role="alert">{error}</p>}
    </div>
  )
}

function SelectField({ id, label, description, value, options, error, onChange }: {
  id: SelectKey
  label: string
  description: string
  value: string
  options: { value: string; label: string }[]
  error?: string
  onChange: (value: string) => void
}) {
  return (
    <div>
      <label htmlFor={id} className="text-sm font-medium text-slate-700">{label}</label>
      <p className="mt-1 min-h-8 text-xs leading-4 text-slate-400">{description}</p>
      <Select value={value} onValueChange={onChange}>
        <SelectTrigger id={id} className={cn('mt-2', error && 'border-rose-400 focus:border-rose-500 focus:ring-rose-500/10')} aria-invalid={Boolean(error)} aria-describedby={error ? `${id}-error` : undefined}>
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          {options.map((option) => <SelectItem key={option.value} value={option.value}>{option.label}</SelectItem>)}
        </SelectContent>
      </Select>
      {error && <p id={`${id}-error`} className="mt-1.5 text-xs leading-4 text-rose-600" role="alert">{error}</p>}
    </div>
  )
}

function SettingsSection({ icon: Icon, title, description, children }: {
  icon: typeof SlidersHorizontal
  title: string
  description: string
  children: React.ReactNode
}) {
  return (
    <Card>
      <CardHeader className="border-b border-slate-100">
        <div className="flex items-start gap-3"><div className="rounded-xl bg-slate-100 p-2.5 text-slate-500"><Icon className="h-[18px] w-[18px]" /></div><div><CardTitle>{title}</CardTitle><CardDescription className="mt-1">{description}</CardDescription></div></div>
      </CardHeader>
      <CardContent className="pt-5 sm:pt-6">{children}</CardContent>
    </Card>
  )
}

function AccountSection({ account, accounts, defaultBankroll }: { account: Account; accounts: Account[]; defaultBankroll: string }) {
  const { refreshAccounts, selectAccount } = useAccount()
  const { toast } = useToast()
  const [name, setName] = useState(account.name)
  const [saving, setSaving] = useState(false)
  const [autoDialog, setAutoDialog] = useState(false)
  const [createOpen, setCreateOpen] = useState(false)
  const [newName, setNewName] = useState('')
  const [newMode, setNewMode] = useState<AccountMode>('paper')
  const [newVenue, setNewVenue] = useState<Venue>(venueOf(account))
  const [newBankroll, setNewBankroll] = useState('')
  const [creating, setCreating] = useState(false)
  const isLive = account.mode === 'live'
  const venue = venueOf(account)
  const meta = venueMeta(venue)
  // The uniqueness rule is per venue: one live Polymarket account and one live
  // Predict.fun account may coexist.
  const hasLive = accounts.some((item) => item.mode === 'live' && venueOf(item) === venue)

  useEffect(() => { setName(account.name) }, [account.id, account.name])
  useEffect(() => { setNewVenue(venueOf(account)) }, [account.id, account.venue])

  const save = async (patch: { name?: string; auto_trade?: boolean; is_default?: boolean }, message: string) => {
    setSaving(true)
    try {
      await api.updateAccount(account.id, patch)
      refreshAccounts()
      toast({ title: message, variant: 'success' })
    } catch (caught) {
      toast({ title: '账户更新失败', description: caught instanceof Error ? caught.message : '请稍后重试。', variant: 'error' })
    } finally {
      setSaving(false)
    }
  }

  const toggleAuto = (checked: boolean) => {
    if (checked && isLive) {
      setAutoDialog(true)
      return
    }
    void save({ auto_trade: checked }, checked ? '自动交易已开启' : '自动交易已关闭')
  }

  const createAccount = async () => {
    if (!newName.trim()) {
      toast({ title: '请输入账户名称', variant: 'error' })
      return
    }
    const bankroll = Number(newBankroll)
    if (newMode === 'paper' && (newBankroll.trim() === '' || !Number.isFinite(bankroll) || bankroll <= 0)) {
      toast({ title: '请填写有效的初始资金', description: '模拟账户的初始资金必须大于 0，单位为新账户所属场所的计价币。', variant: 'error' })
      return
    }
    setCreating(true)
    try {
      const created = await api.createAccount({
        name: newName.trim(),
        mode: newMode,
        venue: newVenue,
        ...(newMode === 'paper' ? { initial_bankroll: bankroll } : {}),
      })
      refreshAccounts()
      selectAccount(created.id)
      setCreateOpen(false)
      setNewName('')
      toast({ title: '账户已创建', description: `${created.name} · ${venueLabel(venueOf(created))} · ${created.mode === 'live' ? '实盘' : '模拟'}${created.mode === 'paper' ? ` · 初始资金 ${fmtMoney(created.baseline_equity, venueOf(created))}` : ''}`, variant: 'success' })
    } catch (caught) {
      toast({ title: '创建账户失败', description: caught instanceof Error ? caught.message : '请稍后重试。', variant: 'error' })
    } finally {
      setCreating(false)
    }
  }

  const removeAccount = async () => {
    if (!window.confirm(`确认删除模拟账户「${account.name}」？该账户的持仓、订单和权益记录会一并删除。`)) return
    setSaving(true)
    try {
      await api.deleteAccount(account.id)
      refreshAccounts()
      toast({ title: '账户已删除', variant: 'success' })
    } catch (caught) {
      toast({ title: '删除失败', description: caught instanceof Error ? caught.message : '请稍后重试。', variant: 'error' })
    } finally {
      setSaving(false)
    }
  }

  return (
    <Card>
      <CardHeader className="border-b border-slate-100">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="flex items-start gap-3">
            <div className={cn('rounded-xl p-2.5', isLive ? 'bg-amber-50 text-amber-700' : 'bg-emerald-50 text-emerald-700')}>{isLive ? <Radio className="h-[18px] w-[18px]" /> : <FlaskConical className="h-[18px] w-[18px]" />}</div>
            <div>
              <CardTitle>账户管理</CardTitle>
              <CardDescription className="mt-1">实盘与模拟是完全隔离的账户，且每个账户只属于一个场所。后台自动交易按账户单独开启；雷达「立即扫描」也可按次选择自动下单。</CardDescription>
            </div>
          </div>
          <div className="flex flex-wrap items-center gap-2"><VenueChip venue={venue} full /><ModeBadge mode={account.mode} /></div>
        </div>
      </CardHeader>
      <CardContent className="space-y-5 pt-5 sm:pt-6">
        <div className="grid gap-4 lg:grid-cols-2">
          <div className="rounded-xl border border-slate-200 p-4">
            <div className="flex items-center justify-between gap-3">
              <div><p className="text-sm font-semibold text-slate-800">{account.name}</p><p className="mt-0.5 text-xs text-slate-400">账户 #{account.id} · {meta.label} · 创建于 {account.created_at.slice(0, 10)}</p></div>
              {account.is_default && <Badge variant="info"><Star className="h-3 w-3" /> 默认</Badge>}
            </div>
            <div className="mt-4 flex gap-2">
              <Input value={name} onChange={(event) => setName(event.target.value)} aria-label="账户名称" className="h-9" />
              <Button variant="secondary" size="sm" onClick={() => void save({ name: name.trim() }, '账户名称已保存')} disabled={saving || !name.trim() || name.trim() === account.name}><Save className="h-3.5 w-3.5" />保存</Button>
            </div>
            <div className="mt-3 flex flex-wrap gap-2">
              {!account.is_default && <Button variant="ghost" size="sm" onClick={() => void save({ is_default: true }, '默认账户已切换')} disabled={saving}><Star className="h-3.5 w-3.5" />设为默认</Button>}
              {!isLive && !account.is_default && <Button variant="ghost" size="sm" className="text-rose-600 hover:bg-rose-50 hover:text-rose-700" onClick={() => void removeAccount()} disabled={saving}><Trash2 className="h-3.5 w-3.5" />删除</Button>}
              <Button variant="ghost" size="sm" onClick={() => { setNewBankroll(defaultBankroll); setCreateOpen(true) }}><Plus className="h-3.5 w-3.5" />新建账户</Button>
            </div>
          </div>

          <div className={cn('rounded-xl border p-4', isLive ? 'border-amber-200 bg-amber-50/50' : 'border-emerald-100 bg-emerald-50/50')}>
            <div className="flex items-center justify-between gap-3">
              <div><p className="text-sm font-semibold text-slate-800">{isLive ? '实盘自动交易' : '模拟自动交易'}</p><p className="mt-0.5 text-xs leading-5 text-slate-500">{isLive ? `开启后，后台扫描引擎将按策略参数向 ${meta.label} 提交真实买单。` : '开启后，后台扫描引擎将按策略参数创建虚拟买单。'}</p></div>
              <Switch checked={account.auto_trade} onCheckedChange={toggleAuto} disabled={saving || (isLive && !account.live_ready && !account.auto_trade)} aria-label={isLive ? '实盘自动交易' : '模拟自动交易'} />
            </div>
            <dl className="mt-4 space-y-2 text-xs">
              <div className="flex justify-between gap-3"><dt className="text-slate-500">账户模式</dt><dd className="font-medium text-slate-800">{isLive ? `真实资金 · ${meta.label} · ${meta.orderTypes.join(' / ')}` : `虚拟资金 · ${meta.label} · 本地撮合`}</dd></div>
              <div className="flex justify-between gap-3"><dt className="text-slate-500">{isLive ? '基准权益' : '模拟基准权益'}</dt><dd className="font-medium tabular-nums text-slate-800">{account.baseline_equity > 0 ? fmtMoney(account.baseline_equity, venue) : isLive ? '待首次余额同步' : '待初始化'}</dd></div>
              {isLive && <div className="flex justify-between gap-3"><dt className="text-slate-500">交易钱包 (Maker)</dt><dd className="max-w-[220px] truncate font-mono text-[11px] text-slate-700" title={account.live_maker || account.live_signer}>{account.live_maker || account.live_signer || '未配置'}</dd></div>}
              {isLive && account.live_maker && account.live_signer && account.live_maker.toLowerCase() !== account.live_signer.toLowerCase() && <div className="flex justify-between gap-3"><dt className="text-slate-500">签名地址</dt><dd className="max-w-[220px] truncate font-mono text-[11px] text-slate-500" title={account.live_signer}>{account.live_signer}</dd></div>}
            </dl>
            {isLive && !account.live_ready && (
              <div className="mt-3 flex items-start gap-2 rounded-lg border border-rose-200 bg-surface/70 p-3 text-xs leading-5 text-rose-700">
                <ShieldAlert className="mt-0.5 h-3.5 w-3.5 shrink-0" />
                <span>{account.live_configured ? <>实盘凭据未就绪：{account.live_error || `请检查 ${meta.label} 的钱包、Funder、API 凭据与 ${meta.collateral} 授权。`}</> : <>尚未配置 {meta.label} 实盘私钥。可在「系统设置 → 实盘交易凭据」中填写，或设置 <code className="rounded bg-rose-100 px-1">{meta.envHint}</code> 后重启服务。可先使用模拟账户测试策略。</>}</span>
              </div>
            )}
            {isLive && account.live_error && <p className="mt-2 break-words text-xs leading-5 text-rose-600">实盘错误：{account.live_error}</p>}
          </div>
        </div>

        {isLive && account.auto_trade && (
          <div className="flex items-start gap-2 rounded-xl border border-rose-200 bg-rose-50 p-3 text-xs leading-5 text-rose-800">
            <ShieldAlert className="mt-0.5 h-4 w-4 shrink-0" />
            <span><span className="font-semibold">实盘自动交易已开启。</span> 引擎会持续向 {meta.label} 提交真实资金订单。建议先用小金额与严格敞口上限验证。</span>
          </div>
        )}

        <div className="rounded-xl border border-slate-200 bg-slate-50/60 p-3 text-xs leading-5 text-slate-500">
          <Info className="mr-1 inline h-3.5 w-3.5 align-[-2px]" />
          实盘账户使用「系统设置」或服务端环境变量中的钱包与 API 凭据，下单前需完成 {meta.label} 的资金授权（Polymarket 为 {meta.collateral}，Predict.fun 为 {meta.currency}）；模拟账户只写本地数据库。实盘账户不可删除，每个场所只允许一个实盘账户。
        </div>
      </CardContent>

      <Dialog open={autoDialog} onOpenChange={setAutoDialog}>
        <DialogContent>
          <div className="mb-3 flex items-center gap-2"><Badge variant="danger"><ShieldAlert className="h-3 w-3" />真实资金</Badge><VenueChip venue={venue} full /></div>
          <DialogTitle>开启实盘自动交易？</DialogTitle>
          <DialogDescription className="mt-2">开启后，扫描引擎会按照当前策略参数自动向 {meta.label} 提交买单，不再逐笔人工确认。请确认单笔金额、单市场敞口和总敞口上限。</DialogDescription>
          <div className="rounded-xl border border-rose-200 bg-rose-50 p-4 text-sm leading-6 text-rose-800">
            自动交易会持续运行，直到你手动关闭或暂停扫描引擎。单笔金额与敞口上限以「参数设置」中 {meta.label} 的场所参数为准。
          </div>
          <div className="flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
            <Button variant="secondary" onClick={() => setAutoDialog(false)}>取消</Button>
            <Button variant="danger" onClick={() => { setAutoDialog(false); void save({ auto_trade: true }, '实盘自动交易已开启') }}><ShieldAlert className="h-4 w-4" />我确认开启实盘自动交易</Button>
          </div>
        </DialogContent>
      </Dialog>

      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent>
          <DialogTitle>新建账户</DialogTitle>
          <DialogDescription className="mt-2">模拟账户使用独立虚拟资金；实盘账户复用服务端钱包凭据，每个场所只能有一个实盘账户。</DialogDescription>
          <div>
            <label htmlFor="new-account-name" className="text-sm font-medium text-slate-700">账户名称</label>
            <Input id="new-account-name" value={newName} onChange={(event) => setNewName(event.target.value)} placeholder="例如：策略验证 A" className="mt-2" autoFocus />
          </div>
          <div>
            <label htmlFor="new-account-venue" className="text-sm font-medium text-slate-700">所属场所</label>
            <Select value={newVenue} onValueChange={(value) => setNewVenue(value as Venue)}>
              <SelectTrigger id="new-account-venue" className="mt-2">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {VENUES.map((item) => <SelectItem key={item} value={item}>{venueLabel(item)}（{venueMeta(item).currency}）</SelectItem>)}
              </SelectContent>
            </Select>
          </div>
          <div>
            <label htmlFor="new-account-mode" className="text-sm font-medium text-slate-700">账户模式</label>
            <Select value={newMode} onValueChange={(value) => setNewMode(value as AccountMode)}>
              <SelectTrigger id="new-account-mode" className="mt-2">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="paper">模拟账户（虚拟资金）</SelectItem>
                <SelectItem value="live" disabled={hasLive}>实盘账户（真实资金）{hasLive ? ' · 该场所已存在' : ''}</SelectItem>
              </SelectContent>
            </Select>
          </div>
          {newMode === 'paper' && (
            <NumberField
              id="new-account-bankroll"
              label="模拟初始资金"
              description={`写入该账户的基准权益，单位 ${venueMeta(newVenue).currency}；创建后不会随策略页的默认值变化而重算。`}
              value={newBankroll}
              unit={venueMeta(newVenue).currency}
              min={0}
              onChange={setNewBankroll}
            />
          )}
          <div className="flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
            <Button variant="secondary" onClick={() => setCreateOpen(false)} disabled={creating}>取消</Button>
            <Button variant="primary" onClick={() => void createAccount()} loading={creating}>创建账户</Button>
          </div>
        </DialogContent>
      </Dialog>
    </Card>
  )
}

function VenueSettingsSection({ venue, draft, shared, errors, onChange }: {
  venue: Venue
  draft: VenueDraft
  shared: SharedDraft
  errors: FieldErrors
  onChange: (patch: Partial<VenueDraft>) => void
}) {
  const meta = venueMeta(venue)
  const inherited = {
    min_liquidity: Number(shared.min_liquidity),
    order_size: Number(shared.order_size),
    max_position: Number(shared.max_position),
    max_exposure: Number(shared.max_exposure),
    fee_bps: Number(shared.fee_bps),
  }
  const placeholder = (field: VenueNumericKey) => (Number.isFinite(inherited[field]) ? `继承账户策略 ${inherited[field]}` : '继承账户策略值')
  const inheritLiquidity = Number.isFinite(inherited.min_liquidity) ? inherited.min_liquidity.toLocaleString() : '账户策略值'
  return (
    <SettingsSection
      icon={ShieldCheck}
      title={`${meta.label} 场所参数`}
      description={`${meta.label} 以 ${meta.currency} 计价（${meta.quoteDecimals} 位精度），订单类型 ${meta.orderTypes.join(' / ')}。留空的数字表示继承当前账户「策略」页的值。`}
    >
      <div className="flex flex-col gap-4 rounded-xl border border-slate-200 bg-slate-50 p-4 sm:flex-row sm:items-center sm:justify-between">
        <div className="flex items-start gap-3">
          <div className={cn('rounded-xl p-2.5', draft.enabled ? 'bg-emerald-50 text-emerald-700' : 'bg-surface text-slate-400')}><ShieldCheck className="h-5 w-5" /></div>
          <div>
            <p className="flex flex-wrap items-center gap-2 text-sm font-semibold text-slate-800">{meta.label} 参与扫描与下单<VenueChip venue={venue} /></p>
            <p className="mt-1 text-xs leading-5 text-slate-500">关闭后该场所不参与扫描、自动下单与手动下单；已有持仓与订单仍可查看。另一个场所不受影响。</p>
          </div>
        </div>
        <div className="flex items-center gap-3 sm:flex-col sm:items-end">
          <Switch checked={draft.enabled} onCheckedChange={(checked) => onChange({ enabled: checked })} aria-label={`启用 ${meta.label}`} />
          <span className="text-xs text-slate-400">{draft.enabled ? '已启用' : '已停用'}</span>
        </div>
      </div>

      <div className="mt-6 grid gap-x-5 gap-y-6 sm:grid-cols-2 xl:grid-cols-4">
        <NumberField id={`${venue}-min-liquidity`} label="最低流动性" description={venue === 'predictfun' ? `低于此值的市场不进入雷达。Predict.fun 的盘口深度通常远低于 Polymarket（实测 $90–$315k），建议设一个更低的门槛。留空继承账户策略值（当前 ${inheritLiquidity}）。` : `低于此值的市场不进入雷达。留空继承账户策略值（当前 ${inheritLiquidity}）。`} value={draft.min_liquidity} unit={meta.currency} min={0} placeholder={placeholder('min_liquidity')} error={errors[`${venue}.min_liquidity`]} onChange={(value) => onChange({ min_liquidity: value })} />
        <NumberField id={`${venue}-order-size`} label="单笔金额" description={`该场所每次自动买入的目标名义金额。雷达「真实收益」也按此金额报价。单位 ${meta.currency}。`} value={draft.order_size} unit={meta.currency} min={0} placeholder={placeholder('order_size')} error={errors[`${venue}.order_size`]} onChange={(value) => onChange({ order_size: value })} />
        <NumberField id={`${venue}-max-position`} label="单市场持仓上限" description={`不得低于该场所生效的单笔金额。单位 ${meta.currency}。`} value={draft.max_position} unit={meta.currency} min={0} placeholder={placeholder('max_position')} error={errors[`${venue}.max_position`]} onChange={(value) => onChange({ max_position: value })} />
        <NumberField id={`${venue}-max-exposure`} label="总敞口上限" description={`该场所全部未平仓头寸的风险预算上限，不得低于单市场上限。单位 ${meta.currency}，绝不与其他场所相加。`} value={draft.max_exposure} unit={meta.currency} min={0} placeholder={placeholder('max_exposure')} error={errors[`${venue}.max_exposure`]} onChange={(value) => onChange({ max_exposure: value })} />
        <NumberField id={`${venue}-fee-bps`} label="Paper 费用" description={venue === 'predictfun' ? '0–1000 bps，仅用于模拟成交；Predict.fun 实际按 费率 × min(p,1−p) × 份额 收取，maker 0 费。留空继承账户策略值。' : '0–1000 bps，仅用于模拟成交；留空继承账户策略值。'} value={draft.fee_bps} unit="bps" min={0} max={1000} placeholder={placeholder('fee_bps')} error={errors[`${venue}.fee_bps`]} onChange={(value) => onChange({ fee_bps: value })} />
      </div>
      <div className="mt-6 flex items-start gap-3 rounded-xl border border-slate-200 bg-slate-50 p-4">
        <div className="rounded-xl bg-surface p-2.5 text-slate-500"><Info className="h-5 w-5" /></div>
        <div className="flex-1">
          <p className="text-sm font-semibold text-slate-800">{meta.label} 的结算与费用口径</p>
          <p className="mt-1 text-xs leading-5 text-slate-500">
            {meta.supportsSettlement ? '引擎可用参考数据结算持仓。' : '引擎不会自动结算该场所的持仓：到期后持仓保持「持有中」，需要你在平台内自行赎回。'}
            {' '}手续费以场所行情报告的费率为准（该字段是纸面费用的覆写），不要把它换算成「名义额的百分比」来比较不同场所。
          </p>
        </div>
      </div>
    </SettingsSection>
  )
}

export function SettingsView() {
  const { account, accounts } = useAccount()
  const [tab, setTab] = useState<SettingsTab>('shared')
  const [draft, setDraft] = useState<Draft | null>(null)
  const [dirty, setDirty] = useState(false)
  const [errors, setErrors] = useState<FieldErrors>({})
  const [saving, setSaving] = useState(false)
  const [toggling, setToggling] = useState(false)
  const [engineOverride, setEngineOverride] = useState<Engine | null>(null)
  const saveController = useRef<AbortController | null>(null)
  const latestSettings = useRef<Settings | null>(null)
  const draftAccount = useRef<number | undefined>(undefined)
  const { toast } = useToast()
  // Settings are stored per account: both the read and the write carry the
  // selected account id so switching accounts never mixes their strategies.
  // The global template is intentionally never loaded here: editing it would
  // arm the stop-loss for every account that has not saved its own block.
  const fetchSettings = useCallback((signal: AbortSignal) => api.settings(signal, account?.id), [account?.id])
  const fetchDashboard = useCallback((signal: AbortSignal) => api.dashboard(account?.id, signal), [account?.id])
  const settingsQuery = usePolling(fetchSettings, { interval: 30000, enabled: Boolean(account?.id) })
  const dashboardQuery = usePolling(fetchDashboard, { enabled: Boolean(account?.id) })
  const engine = engineOverride || dashboardQuery.data?.engine || null
  const isLive = account?.mode === 'live'
  const accountVenue = venueOf(account)

  // usePolling clears its data when the fetcher identity changes, so an
  // account switch drops the previous account's settings and rebuilds the form
  // from the newly loaded block. Unsaved edits are discarded on purpose.
  useEffect(() => {
    if (draftAccount.current === account?.id) return
    draftAccount.current = account?.id
    latestSettings.current = null
    setDraft(null)
    setDirty(false)
    setErrors({})
    setTab('shared')
  }, [account?.id])

  useEffect(() => {
    if (!settingsQuery.data) return
    latestSettings.current = settingsQuery.data
    if (!dirty) setDraft(toDraft(settingsQuery.data))
  }, [settingsQuery.data, dirty])

  useEffect(() => () => saveController.current?.abort(), [])

  const tabs = useMemo<TabItem<SettingsTab>[]>(() => [
    { value: 'shared', label: '策略', icon: SlidersHorizontal },
    ...VENUES.map((venue) => ({ value: venue as SettingsTab, label: venueMeta(venue).label, hint: venueMeta(venue).currency })),
  ], [])

  const update = (key: NumericKey | SelectKey, value: string) => {
    setDraft((current) => current ? { ...current, shared: { ...current.shared, [key]: value } } : current)
    setDirty(true)
    setErrors((current) => ({ ...current, [key]: undefined }))
  }

  const updateStopLoss = (enabled: boolean) => {
    setDraft((current) => current ? { ...current, shared: { ...current.shared, stop_loss_enabled: enabled } } : current)
    setDirty(true)
  }

  const updateVenue = (venue: Venue, patch: Partial<VenueDraft>) => {
    setDraft((current) => current ? { ...current, venues: { ...current.venues, [venue]: { ...current.venues[venue], ...patch } } } : current)
    setDirty(true)
    setErrors((current) => {
      const next = { ...current }
      for (const key of Object.keys(patch)) delete next[`${venue}.${key}`]
      return next
    })
  }

  const reset = () => {
    if (!latestSettings.current) return
    setDraft(toDraft(latestSettings.current))
    setErrors({})
    setDirty(false)
  }

  const save = async () => {
    if (!draft) return
    // A write without an account id lands on the global template, which would
    // change the stop-loss of every account that never saved its own block.
    if (!account?.id) {
      toast({ title: '请先选择账户', description: '参数按账户保存，账户信息尚未载入完成。', variant: 'error' })
      return
    }
    const validation = validate(draft)
    setErrors(validation)
    const errorKeys = Object.keys(validation)
    if (errorKeys.length > 0) {
      // The error may live on another tab; switch to it so the red hint is visible.
      const tabHasError = errorKeys.some((key) => (tab === 'shared' ? !key.includes('.') : key.startsWith(`${tab}.`)))
      if (!tabHasError) {
        const venueError = VENUES.find((venue) => errorKeys.some((key) => key.startsWith(`${venue}.`)))
        setTab(venueError ?? 'shared')
      }
      toast({ title: '请先修正参数', description: '部分字段不符合约束，请查看红色提示（策略与场所页签）。', variant: 'error' })
      return
    }
    const base = latestSettings.current
    if (!base) {
      toast({ title: '保存失败', description: '尚未读取到后端参数，请稍后重试。', variant: 'error' })
      return
    }
    // One PUT carries the account's strategy fields plus both venues, built from
    // the loaded object so unrendered fields are never dropped.
    const payload = toPayload(base, draft)
    // Scanning is process-wide. The account settings endpoint deliberately
    // ignores this field, so a changed cadence needs its own global update.
    const scanInterval = Number(draft.shared.scan_interval_seconds)
    const scanChanged = scanInterval !== base.scan_interval_seconds
    saveController.current?.abort()
    const controller = new AbortController()
    saveController.current = controller
    setSaving(true)
    let scanSaved = false
    try {
      if (scanChanged) {
        await api.updateScanInterval(scanInterval, controller.signal)
        scanSaved = true
      }
      const saved = await api.updateSettings(payload, controller.signal, account?.id)
      latestSettings.current = saved
      setDraft(toDraft(saved))
      setDirty(false)
      setErrors({})
      toast({
        title: '参数已保存',
        description: scanChanged
          ? `当前账户${account ? `「${account.name}」` : ''}的策略与两个场所参数已提交；扫描间隔已更新为全局 ${scanInterval} 秒。`
          : `当前账户${account ? `「${account.name}」` : ''}的策略与两个场所参数已提交；其他账户不受影响。`,
        variant: 'success',
      })
      settingsQuery.refresh()
    } catch (caught) {
      if (caught instanceof DOMException && caught.name === 'AbortError') return
      const message = caught instanceof Error ? caught.message : '请稍后重试。'
      toast({
        title: '保存失败',
        description: scanSaved ? `扫描间隔已改为 ${scanInterval} 秒，但账户参数未保存：${message}` : message,
        variant: 'error',
      })
    } finally {
      if (saveController.current === controller) setSaving(false)
    }
  }

  const toggleEngine = async (running: boolean) => {
    setToggling(true)
    try {
      const next = await api.toggleEngine(running)
      setEngineOverride(next)
      toast({ title: running ? '扫描引擎已启动' : '扫描引擎已暂停', description: running ? '将按设置间隔扫描各已启用场所；后台自动下单按账户开关执行。' : '后台不再定时扫描；雷达「立即扫描」仍可手动触发。已有持仓保持不变。', variant: 'success' })
      dashboardQuery.refresh()
    } catch (caught) {
      toast({ title: '引擎状态切换失败', description: caught instanceof Error ? caught.message : '请稍后重试。', variant: 'error' })
    } finally {
      setToggling(false)
    }
  }

  return (
    <>
      <PageHeader eyebrow={isLive ? 'LIVE CONFIGURATION' : 'PAPER CONFIGURATION'} title="参数设置" description={account ? `配置当前账户「${account.name}」的${isLive ? '行情筛选、实盘风控与自动交易参数' : '行情筛选、虚拟资金与 Paper 成交参数'}；每个账户保存自己的策略，互不影响。实盘成交价由所选场所（${venueLabel(accountVenue)}）的订单簿决定。` : '正在载入账户信息…'} />

      <Card className="mb-4 overflow-hidden border-navy bg-navy text-white">
        <div className="flex flex-col gap-5 p-5 sm:flex-row sm:items-center sm:p-6">
          <div className={cn('rounded-2xl p-3', engine?.running ? 'bg-brand-lime text-navy' : 'bg-white/10 text-slate-400')}><Activity className="h-6 w-6" /></div>
          <div className="min-w-0 flex-1"><div className="flex flex-wrap items-center gap-2"><h2 className="text-base font-semibold">扫描引擎</h2>{engine && <Badge variant={engine.running ? 'lime' : 'navy'}>{engine.running ? '运行中' : '已暂停'}</Badge>}</div><p className="mt-1.5 text-sm leading-6 text-slate-400">控制后台定时扫描；后台自动下单由各账户的「自动交易」开关独立决定，雷达「立即扫描」可按次选择自动下单。</p>{engine?.last_error && <p className="mt-2 break-words text-xs leading-5 text-rose-300">最近错误：{engine.last_error}</p>}</div>
          <div className="flex items-center gap-3 sm:flex-col sm:items-end"><Switch checked={engine?.running || false} onCheckedChange={toggleEngine} disabled={!engine || toggling || dashboardQuery.isLoading} aria-label="启动或暂停扫描引擎" className="data-[state=checked]:bg-emerald-500" /><span className="text-xs text-slate-400">{toggling ? '正在切换…' : engine?.running ? '点击暂停' : '点击启动'}</span></div>
        </div>
        {dashboardQuery.error && !engine && <div className="break-words border-t border-white/10 bg-white/5 px-5 py-3 text-xs text-rose-300">{dashboardQuery.error}</div>}
      </Card>

      {account && <div className="mb-4"><AccountSection key={account.id} account={account} accounts={accounts} defaultBankroll={settingsQuery.data ? String(settingsQuery.data.initial_bankroll) : ''} /></div>}

      {settingsQuery.isLoading ? <Card><div className="grid gap-5 p-6 sm:grid-cols-2"><Skeleton className="h-24" /><Skeleton className="h-24" /><Skeleton className="h-24" /><Skeleton className="h-24" /></div></Card> : settingsQuery.error && !settingsQuery.data ? <Card><ErrorState message={settingsQuery.error} onRetry={settingsQuery.refresh} /></Card> : !draft ? <Card><div className="grid gap-5 p-6 sm:grid-cols-2"><Skeleton className="h-24" /><Skeleton className="h-24" /><Skeleton className="h-24" /><Skeleton className="h-24" /></div></Card> : (
        <form id="settings-form" onSubmit={(event) => { event.preventDefault(); void save() }} className="space-y-4">
          {settingsQuery.error && <div className="rounded-xl border border-amber-200 bg-amber-50 px-4 py-3 text-xs text-amber-800">后台刷新失败，当前表单仍可编辑：{settingsQuery.error}</div>}
          <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
            <TabBar items={tabs} value={tab} onChange={setTab} label="参数分类" />
            <p className="text-xs text-slate-500">「策略」是当前账户的筛选与风控参数；场所页签只覆写单笔金额、敞口与费用。</p>
          </div>

          <div role="tabpanel" aria-label={tab === 'shared' ? '策略参数' : `${venueMeta(tab).label} 场所参数`} className="space-y-4">
          {tab === 'shared' ? (
            <>
              <SettingsSection icon={SlidersHorizontal} title="市场筛选" description="只保留符合价格、到期时间、流动性与分类要求的公开市场候选。筛选条件属于当前账户；默认分类只决定雷达打开时的分类。">
                <div className="grid gap-x-5 gap-y-6 sm:grid-cols-2 xl:grid-cols-4">
                  <NumberField id="min_price" label="最低指示价格" description="概率（0–1），必须大于 0，且不高于最高价格。" value={draft.shared.min_price} unit="0–1" min={0} max={1} error={errors.min_price} onChange={(value) => update('min_price', value)} />
                  <NumberField id="max_price" label="最高指示价格" description="概率（0–1），必须小于 1，且不低于最低价格。" value={draft.shared.max_price} unit="0–1" min={0} max={1} error={errors.max_price} onChange={(value) => update('max_price', value)} />
                  <NumberField id="max_days" label="最长到期时间" description="筛选 1–3650 天内到期的市场。" value={draft.shared.max_days} unit="天" min={1} max={3650} step="1" error={errors.max_days} onChange={(value) => update('max_days', value)} />
                  <NumberField id="min_liquidity" label="最低流动性" description="低于该值的市场不会进入雷达；单位随各场所的计价币（USDC / USDT）。" value={draft.shared.min_liquidity} unit="场所币" min={0} error={errors.min_liquidity} onChange={(value) => update('min_liquidity', value)} />
                </div>
                <div className="mt-6 grid gap-x-5 gap-y-6 sm:grid-cols-2 xl:grid-cols-3">
                  <SelectField id="default_category" label="雷达默认分类" description="机会雷达打开时默认选中的分类；只影响展示，不改变自动交易策略。" value={draft.shared.default_category} options={defaultCategoryOptions} error={errors.default_category} onChange={(value) => update('default_category', value)} />
                  <SelectField id="sports_policy" label="体育市场策略" description="体育赛事盘口可能包含加时、延长赛或点球决胜，临场波动大。默认排除有开赛时间的赛事盘口，保留赛季冠军等长期体育预测。" value={draft.shared.sports_policy} options={sportsPolicyOptions} error={errors.sports_policy} onChange={(value) => update('sports_policy', value)} />
                  <NumberField id="max_price_change_24h" label="24 小时价格波动上限" description="概率变动（0–1）的绝对值超过该值时跳过该市场；0 表示不限制。" value={draft.shared.max_price_change_24h} unit="0–1" min={0} max={1} error={errors.max_price_change_24h} onChange={(value) => update('max_price_change_24h', value)} />
                </div>
                {draft.shared.default_category === 'sports' && draft.shared.sports_policy !== 'allow' && (
                  <div className="mt-6 rounded-xl border border-amber-200 bg-amber-50 px-4 py-3 text-xs leading-5 text-amber-800">当前体育市场策略会排除体育盘口，但雷达默认分类是「体育」，打开雷达时可能为空。建议把默认分类改为「非体育」，或把体育策略改为「保留并标记风险」。</div>
                )}
                <div className="mt-6 flex items-start gap-3 rounded-xl border border-slate-200 bg-slate-50 p-4">
                  <div className="rounded-xl bg-surface p-2.5 text-slate-500"><Info className="h-5 w-5" /></div>
                  <div className="flex-1"><p className="text-sm font-semibold text-slate-800">为什么默认排除体育赛事盘口？</p><p className="mt-1 text-xs leading-5 text-slate-500">足球加时、篮球加时、冰球加时、棒球延长局和电竞决胜局都会让临近结算的价格重新波动。0.95–0.99 的尾盘策略更适合结果已基本确定、且不会因加时改写的市场。自动交易同样使用该策略；若希望完全避开体育，请选择「排除全部体育分类」。</p></div>
                </div>
              </SettingsSection>

              <SettingsSection icon={ShieldCheck} title="资金与风控（当前账户）" description="这里的金额属于当前账户，单位随场所变化（Polymarket = USDC、Predict.fun = USDT）。需要不同的单笔金额或敞口时，在对应场所页签填写覆写值；其他账户不受影响。">
                <div className="grid gap-x-5 gap-y-6 sm:grid-cols-2 xl:grid-cols-4">
                  <NumberField id="initial_bankroll" label="新建模拟账户默认资金" description="只作为「新建账户」弹窗的预填值；弹窗里填写的金额直接写入新账户的基准权益，已有基准权益的账户不会因修改此值而重算历史。" value={draft.shared.initial_bankroll} unit="场所币" min={0} error={errors.initial_bankroll} onChange={(value) => update('initial_bankroll', value)} />
                  <NumberField id="order_size" label="单笔金额（默认）" description="每次自动买入的目标名义金额；机会雷达的「真实收益」也按该场所的金额报价。" value={draft.shared.order_size} unit="场所币" min={0} error={errors.order_size} onChange={(value) => update('order_size', value)} />
                  <NumberField id="max_position" label="单市场持仓上限（默认）" description="不得低于单笔金额。" value={draft.shared.max_position} unit="场所币" min={0} error={errors.max_position} onChange={(value) => update('max_position', value)} />
                  <NumberField id="max_exposure" label="总敞口上限（默认）" description="单个场所全部未平仓头寸的风险预算上限；不得低于单市场持仓上限。绝不跨场所合并。" value={draft.shared.max_exposure} unit="场所币" min={0} error={errors.max_exposure} onChange={(value) => update('max_exposure', value)} />
                  <NumberField id="max_event_exposure" label="单事件敞口上限（默认）" description="同一事件下全部市场的合计成本上限。一场选举或一场比赛的所有盘口共用同一个结果，逐个买入等于同一注押很多次；0 表示不限。" value={draft.shared.max_event_exposure} unit="场所币" min={0} error={errors.max_event_exposure} onChange={(value) => update('max_event_exposure', value)} />
                </div>
              </SettingsSection>

              <SettingsSection icon={Clock} title="尾盘下单窗口" description="自动买入只会在市场收盘前的窗口内执行；窗口外只保留候选，不会下单。手动买入不受限制。">
                <div className="grid gap-x-5 gap-y-6 sm:grid-cols-2 xl:grid-cols-3">
                  <NumberField id="entry_window_minutes" label="非体育类下单窗口" description="距收盘多少分钟开始允许自动买入；0 表示不限制。" value={draft.shared.entry_window_minutes} unit="分钟" min={0} max={1440} step="1" error={errors.entry_window_minutes} onChange={(value) => update('entry_window_minutes', value)} />
                  <NumberField id="sports_entry_window_minutes" label="体育类下单窗口" description="体育分类使用更短的窗口，避开临场加时与赔率剧烈波动；0 表示不限制。" value={draft.shared.sports_entry_window_minutes} unit="分钟" min={0} max={1440} step="1" error={errors.sports_entry_window_minutes} onChange={(value) => update('sports_entry_window_minutes', value)} />
                  <NumberField id="crypto_entry_window_seconds" label="加密类下单窗口" description="加密分类按秒计时，优先于上面的分钟窗口；默认 60 秒。0 表示不限制。" value={draft.shared.crypto_entry_window_seconds} unit="秒" min={0} max={86400} step="1" error={errors.crypto_entry_window_seconds} onChange={(value) => update('crypto_entry_window_seconds', value)} />
                </div>
                {draft.shared.crypto_entry_window_seconds !== '' && Number(draft.shared.crypto_entry_window_seconds) > 0 && Number(draft.shared.crypto_entry_window_seconds) <= Number(draft.shared.scan_interval_seconds) && (
                  <div className="mt-6 rounded-xl border border-amber-200 bg-amber-50 px-4 py-3 text-xs leading-5 text-amber-800">加密类下单窗口（{draft.shared.crypto_entry_window_seconds} 秒）不长于扫描间隔（{draft.shared.scan_interval_seconds} 秒），后台扫描可能整轮错过该窗口。请把扫描间隔调到窗口长度以内，或改用「立即扫描」。</div>
                )}
                <div className="mt-6 flex items-start gap-3 rounded-xl border border-slate-200 bg-slate-50 p-4">
                  <div className="rounded-xl bg-surface p-2.5 text-slate-500"><Info className="h-5 w-5" /></div>
                  <div className="flex-1"><p className="text-sm font-semibold text-slate-800">每个盘口只会自动下单一次</p><p className="mt-1 text-xs leading-5 text-slate-500">自动买入成功后会把该盘口标记为已下单；之后无论持仓是否已结算、盘口是否重新出现，都不会再次自动买入。窗口以市场到期时间为准，实际成交时点还受扫描间隔影响，可能晚于窗口起点最多一个扫描周期。</p></div>
                </div>
              </SettingsSection>

              <SettingsSection icon={ShieldAlert} title="止损保护" description={`各场所都没有交易所原生止损单。开启后，扫描引擎会在每次扫描时检查当前账户${account ? `「${account.name}」` : ''}未平仓头寸的参考价，跌到入场价的止损比例时自动提交市价卖出。止损开关与止损线随该账户保存，其他账户各自独立。`}>
                <div className="flex flex-col gap-4 rounded-xl border border-slate-200 bg-slate-50 p-4 sm:flex-row sm:items-center sm:justify-between">
                  <div className="flex items-start gap-3">
                    <div className={cn('rounded-xl p-2.5', draft.shared.stop_loss_enabled ? 'bg-amber-50 text-amber-600' : 'bg-surface text-slate-400')}><ShieldAlert className="h-5 w-5" /></div>
                    <div><p className="text-sm font-semibold text-slate-800">自动止损</p><p className="mt-1 text-xs leading-5 text-slate-500">只对当前账户生效：扫描引擎运行时检查该账户的未平仓头寸，不要求开启自动交易，其他账户的止损开关不受影响；自动交易开关只控制新的自动买入。</p></div>
                  </div>
                  <div className="flex items-center gap-3 sm:flex-col sm:items-end">
                    <Switch checked={draft.shared.stop_loss_enabled} onCheckedChange={updateStopLoss} aria-label="开启或关闭自动止损" />
                    <span className="text-xs text-slate-400">{draft.shared.stop_loss_enabled ? '已开启' : '已关闭'}</span>
                  </div>
                </div>
                <div className="mt-6 grid gap-x-5 gap-y-6 sm:grid-cols-2">
                  <NumberField id="stop_loss_ratio" label="止损线（入场价比例）" description="默认 0.70；例如 0.70 表示当前参考价跌到平均入场价的 70% 时卖出，不是绝对价格 0.70。" value={draft.shared.stop_loss_ratio} unit="× 入场价" min={0} max={1} step="0.01" error={errors.stop_loss_ratio} onChange={(value) => update('stop_loss_ratio', value)} />
                </div>
                {stopLossFloorViolations(draft).map((violation) => (
                  <div key={violation.venue} className="mt-6 rounded-xl border border-rose-200 bg-rose-50 px-4 py-3 text-xs leading-5 text-rose-800">
                    无法开启自动止损：{violation.label} 的卖单最小 {violation.floor} 份，当前单笔金额 {violation.orderSize} {violation.currency} 买不到这么多份额，仓位只能持有到结算。请把{violation.label}的单笔金额调到 {violation.floor} {violation.currency} 或以上。
                  </div>
                ))}
                <div className="mt-6 flex items-start gap-3 rounded-xl border border-amber-200 bg-amber-50 p-4">
                  <div className="rounded-xl bg-surface p-2.5 text-amber-600"><Info className="h-5 w-5" /></div>
                  <div className="flex-1"><p className="text-sm font-semibold text-amber-900">止损不是保证成交价</p><p className="mt-1 text-xs leading-5 text-amber-800">止损触发依赖扫描间隔（暂停扫描引擎时不会检查），行情跳空或盘口深度不足时可能卖不出去，或成交价明显低于止损线。Polymarket 的止损卖单使用 FOK 立即成交或全部取消；Predict.fun 使用 MARKET 单并受滑点上限约束。未成交时会在下一次扫描重试。请先用模拟账户验证。</p></div>
                </div>
              </SettingsSection>

              <SettingsSection icon={TimerReset} title="扫描与模拟成交" description="扫描间隔控制后台引擎的全局节奏（所有账户共用）；滑点与费用属于当前账户，实盘成交价以各场所订单簿为准。">
                <div className="grid gap-x-5 gap-y-6 sm:grid-cols-2 xl:grid-cols-3">
                  <NumberField id="scan_interval_seconds" label="扫描间隔（全局）" description="所有账户共用同一后台扫描节奏，允许 10–86400 秒，需为整数。" value={draft.shared.scan_interval_seconds} unit="秒" min={10} max={86400} step="1" error={errors.scan_interval_seconds} onChange={(value) => update('scan_interval_seconds', value)} />
                  <NumberField id="slippage_bps" label="Paper 滑点" description="0–1000 bps；仅影响模拟成交。" value={draft.shared.slippage_bps} unit="bps" min={0} max={1000} error={errors.slippage_bps} onChange={(value) => update('slippage_bps', value)} />
                  <NumberField id="fee_bps" label="Paper 费用（默认）" description="0–1000 bps。Polymarket 等效费率 ≈ 类别费率 × (1 − 价格)；Predict.fun 按 费率 × min(p,1−p) 计，maker 0 费。可在场所页签覆写。" value={draft.shared.fee_bps} unit="bps" min={0} max={1000} error={errors.fee_bps} onChange={(value) => update('fee_bps', value)} />
                </div>
                {draft.shared.fee_bps !== '' && Number(draft.shared.fee_bps) === 0 && (
                  <div className="mt-6 rounded-xl border border-amber-200 bg-amber-50 px-4 py-3 text-xs leading-5 text-amber-800">模拟费用为 0 时，模拟收益会系统性高估。Polymarket 对多数类别收取 taker 费，Predict.fun 实测约 200 bps × min(p,1−p)，建议按场所分别设置。</div>
                )}
                <div className="mt-6 flex items-start gap-3 rounded-xl border border-slate-200 bg-slate-50 p-4">
                  <div className="rounded-xl bg-surface p-2.5 text-slate-500"><UserCog className="h-5 w-5" /></div>
                  <div className="flex-1"><p className="text-sm font-semibold text-slate-800">自动交易开关已移到账户管理</p><p className="mt-1 text-xs leading-5 text-slate-500">实盘与模拟账户可以分别开启后台自动交易。雷达「立即扫描」可按次选择是否自动下单；实盘账户开启后台自动交易时需要二次确认。</p></div>
                </div>
              </SettingsSection>
            </>
          ) : (
            <VenueSettingsSection venue={tab} draft={draft.venues[tab]} shared={draft.shared} errors={errors} onChange={(patch) => updateVenue(tab, patch)} />
          )}
          </div>

          <div className="sticky bottom-4 z-10 flex flex-col gap-3 rounded-2xl border border-slate-200 bg-surface/95 p-4 shadow-float backdrop-blur sm:flex-row sm:items-center">
            <div className="flex flex-1 items-start gap-2 text-xs leading-5 text-slate-500"><Info className="mt-0.5 h-4 w-4 shrink-0 text-slate-400" /><span>{dirty ? `有尚未保存的更改${tab === 'shared' ? '（策略页）' : `（${venueMeta(tab).label} 页）`}，保存会同时提交当前账户的策略与两个场所的参数。` : '当前账户的参数已与后端同步。'} 扫描间隔为全局；其余策略与场所参数按账户独立保存。</span></div>
            <div className="flex gap-2"><Button type="button" variant="secondary" onClick={reset} disabled={!dirty || saving}><RotateCcw className="h-4 w-4" />放弃更改</Button><Button type="submit" variant="primary" loading={saving} disabled={!dirty}>{saving ? '正在保存…' : <><Save className="h-4 w-4" />保存参数</>}</Button></div>
          </div>
        </form>
      )}

      <div className="mt-5 flex items-start gap-2 text-xs leading-5 text-slate-400"><Gauge className="mt-0.5 h-3.5 w-3.5 shrink-0" /><p>{isLive ? `实盘建议先使用最小金额验证 ${venueMeta(accountVenue).label} 的钱包授权、盘口深度与成交回报，再逐步提高敞口。` : '建议先使用保守上限验证策略。模拟成交规则无法复现真实市场的流动性、排队与价格冲击。'}</p></div>
    </>
  )
}
