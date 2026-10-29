import { useCallback, useEffect, useRef, useState } from 'react'
import {
  Activity,
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
import type { Account, AccountMode, Engine, Settings } from '@/types'
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
import { PageHeader } from '@/components/page-header'
import { ModeBadge } from '@/components/status-badges'

const numericKeys = ['min_price', 'max_price', 'max_days', 'min_liquidity', 'order_size', 'max_position', 'max_exposure', 'scan_interval_seconds', 'slippage_bps', 'fee_bps', 'initial_bankroll'] as const
type NumericKey = typeof numericKeys[number]
type Draft = Record<NumericKey, string>
type FieldErrors = Partial<Record<NumericKey, string>>

function toDraft(settings: Settings): Draft {
  return {
    min_price: String(settings.min_price),
    max_price: String(settings.max_price),
    max_days: String(settings.max_days),
    min_liquidity: String(settings.min_liquidity),
    order_size: String(settings.order_size),
    max_position: String(settings.max_position),
    max_exposure: String(settings.max_exposure),
    scan_interval_seconds: String(settings.scan_interval_seconds),
    slippage_bps: String(settings.slippage_bps),
    fee_bps: String(settings.fee_bps),
    initial_bankroll: String(settings.initial_bankroll),
  }
}

function toSettings(draft: Draft): Settings {
  return {
    min_price: Number(draft.min_price),
    max_price: Number(draft.max_price),
    max_days: Number(draft.max_days),
    min_liquidity: Number(draft.min_liquidity),
    order_size: Number(draft.order_size),
    max_position: Number(draft.max_position),
    max_exposure: Number(draft.max_exposure),
    scan_interval_seconds: Number(draft.scan_interval_seconds),
    slippage_bps: Number(draft.slippage_bps),
    fee_bps: Number(draft.fee_bps),
    initial_bankroll: Number(draft.initial_bankroll),
  }
}

function validate(settings: Settings): FieldErrors {
  const errors: FieldErrors = {}
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
  if (settings.max_exposure < settings.order_size) errors.max_exposure = '总敞口上限不能低于单笔金额。'
  if (!Number.isInteger(settings.scan_interval_seconds) || settings.scan_interval_seconds < 10 || settings.scan_interval_seconds > 86400) errors.scan_interval_seconds = '请输入 10–86400 之间的整数秒数。'
  if (settings.slippage_bps < 0 || settings.slippage_bps > 1000) errors.slippage_bps = '滑点范围为 0–1000 bps。'
  if (settings.fee_bps < 0 || settings.fee_bps > 1000) errors.fee_bps = '费用范围为 0–1000 bps。'
  if (settings.initial_bankroll <= 0) errors.initial_bankroll = '模拟初始资金必须大于 0。'
  return errors
}

function NumberField({ id, label, description, value, unit, error, min, max, step = 'any', onChange }: {
  id: NumericKey
  label: string
  description: string
  value: string
  unit: string
  error?: string
  min?: number
  max?: number
  step?: string
  onChange: (value: string) => void
}) {
  return (
    <div>
      <label htmlFor={id} className="text-sm font-medium text-slate-700">{label}</label>
      <p className="mt-1 min-h-8 text-xs leading-4 text-slate-400">{description}</p>
      <div className="relative mt-2">
        <Input id={id} type="number" inputMode="decimal" value={value} min={min} max={max} step={step} onChange={(event) => onChange(event.target.value)} aria-invalid={Boolean(error)} aria-describedby={error ? `${id}-error` : undefined} className={cn('pr-16 tabular-nums', error && 'border-rose-400 focus:border-rose-500 focus:ring-rose-500/10')} />
        <span className="pointer-events-none absolute right-3 top-1/2 -translate-y-1/2 text-xs text-slate-400">{unit}</span>
      </div>
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

function AccountSection({ account, accounts }: { account: Account; accounts: Account[] }) {
  const { refreshAccounts, selectAccount } = useAccount()
  const { toast } = useToast()
  const [name, setName] = useState(account.name)
  const [saving, setSaving] = useState(false)
  const [autoDialog, setAutoDialog] = useState(false)
  const [createOpen, setCreateOpen] = useState(false)
  const [newName, setNewName] = useState('')
  const [newMode, setNewMode] = useState<AccountMode>('paper')
  const [creating, setCreating] = useState(false)
  const isLive = account.mode === 'live'
  const hasLive = accounts.some((item) => item.mode === 'live')

  useEffect(() => { setName(account.name) }, [account.id, account.name])

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
    setCreating(true)
    try {
      const created = await api.createAccount({ name: newName.trim(), mode: newMode })
      refreshAccounts()
      selectAccount(created.id)
      setCreateOpen(false)
      setNewName('')
      toast({ title: '账户已创建', description: `${created.name} · ${created.mode === 'live' ? '实盘' : '模拟'}`, variant: 'success' })
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
              <CardDescription className="mt-1">实盘与模拟是完全隔离的账户。自动交易按账户单独开启。</CardDescription>
            </div>
          </div>
          <ModeBadge mode={account.mode} />
        </div>
      </CardHeader>
      <CardContent className="space-y-5 pt-5 sm:pt-6">
        <div className="grid gap-4 lg:grid-cols-2">
          <div className="rounded-xl border border-slate-200 p-4">
            <div className="flex items-center justify-between gap-3">
              <div><p className="text-sm font-semibold text-slate-800">{account.name}</p><p className="mt-0.5 text-xs text-slate-400">账户 #{account.id} · 创建于 {account.created_at.slice(0, 10)}</p></div>
              {account.is_default && <Badge variant="info"><Star className="h-3 w-3" /> 默认</Badge>}
            </div>
            <div className="mt-4 flex gap-2">
              <Input value={name} onChange={(event) => setName(event.target.value)} aria-label="账户名称" className="h-9" />
              <Button variant="secondary" size="sm" onClick={() => void save({ name: name.trim() }, '账户名称已保存')} disabled={saving || !name.trim() || name.trim() === account.name}><Save className="h-3.5 w-3.5" />保存</Button>
            </div>
            <div className="mt-3 flex flex-wrap gap-2">
              {!account.is_default && <Button variant="ghost" size="sm" onClick={() => void save({ is_default: true }, '默认账户已切换')} disabled={saving}><Star className="h-3.5 w-3.5" />设为默认</Button>}
              {!isLive && !account.is_default && <Button variant="ghost" size="sm" className="text-rose-600 hover:bg-rose-50 hover:text-rose-700" onClick={() => void removeAccount()} disabled={saving}><Trash2 className="h-3.5 w-3.5" />删除</Button>}
              <Button variant="ghost" size="sm" onClick={() => setCreateOpen(true)}><Plus className="h-3.5 w-3.5" />新建账户</Button>
            </div>
          </div>

          <div className={cn('rounded-xl border p-4', isLive ? 'border-amber-200 bg-amber-50/50' : 'border-emerald-100 bg-emerald-50/50')}>
            <div className="flex items-center justify-between gap-3">
              <div><p className="text-sm font-semibold text-slate-800">{isLive ? '实盘自动交易' : '模拟自动交易'}</p><p className="mt-0.5 text-xs leading-5 text-slate-500">{isLive ? '开启后，引擎将按策略参数提交真实买单。' : '开启后，引擎将按策略参数创建虚拟买单。'}</p></div>
              <Switch checked={account.auto_trade} onCheckedChange={toggleAuto} disabled={saving || (isLive && !account.live_configured)} aria-label={isLive ? '实盘自动交易' : '模拟自动交易'} />
            </div>
            <dl className="mt-4 space-y-2 text-xs">
              <div className="flex justify-between gap-3"><dt className="text-slate-500">账户模式</dt><dd className="font-medium text-slate-800">{isLive ? '真实资金 · CLOB' : '虚拟资金 · 本地撮合'}</dd></div>
              <div className="flex justify-between gap-3"><dt className="text-slate-500">基准权益</dt><dd className="font-medium tabular-nums text-slate-800">{account.baseline_equity > 0 ? `$${account.baseline_equity.toFixed(2)}` : '待首次余额同步'}</dd></div>
              {isLive && <div className="flex justify-between gap-3"><dt className="text-slate-500">交易钱包</dt><dd className="max-w-[220px] truncate font-mono text-[11px] text-slate-700" title={account.live_signer}>{account.live_signer || '未配置'}</dd></div>}
            </dl>
            {isLive && !account.live_configured && (
              <div className="mt-3 flex items-start gap-2 rounded-lg border border-rose-200 bg-surface/70 p-3 text-xs leading-5 text-rose-700">
                <ShieldAlert className="mt-0.5 h-3.5 w-3.5 shrink-0" />
                <span>未配置 <code className="rounded bg-rose-100 px-1">POLYMARKET_PRIVATE_KEY</code>，实盘下单会被拒绝。可先使用模拟账户测试策略。</span>
              </div>
            )}
            {isLive && account.live_error && <p className="mt-2 break-words text-xs leading-5 text-rose-600">实盘错误：{account.live_error}</p>}
          </div>
        </div>

        {isLive && account.auto_trade && (
          <div className="flex items-start gap-2 rounded-xl border border-rose-200 bg-rose-50 p-3 text-xs leading-5 text-rose-800">
            <ShieldAlert className="mt-0.5 h-4 w-4 shrink-0" />
            <span><span className="font-semibold">实盘自动交易已开启。</span> 引擎会持续提交真实资金订单。建议先用小金额与严格敞口上限验证。</span>
          </div>
        )}

        <div className="rounded-xl border border-slate-200 bg-slate-50/60 p-3 text-xs leading-5 text-slate-500">
          <Info className="mr-1 inline h-3.5 w-3.5 align-[-2px]" />
          实盘账户使用服务端环境变量中的钱包和 CLOB 凭据；模拟账户只写本地数据库。实盘账户不可删除，每个实例只允许一个实盘账户。
        </div>
      </CardContent>

      <Dialog open={autoDialog} onOpenChange={setAutoDialog}>
        <DialogContent>
          <div className="mb-3 flex items-center gap-2"><Badge variant="danger"><ShieldAlert className="h-3 w-3" />真实资金</Badge></div>
          <DialogTitle>开启实盘自动交易？</DialogTitle>
          <DialogDescription className="mt-2">开启后，扫描引擎会按照当前策略参数自动提交 Polymarket 买单，不再逐笔人工确认。请确认单笔金额、单市场敞口和总敞口上限。</DialogDescription>
          <div className="rounded-xl border border-rose-200 bg-rose-50 p-4 text-sm leading-6 text-rose-800">
            自动交易会持续运行，直到你手动关闭或暂停扫描引擎。单笔金额与敞口上限以「参数设置」中的风控参数为准。
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
          <DialogDescription className="mt-2">模拟账户使用独立虚拟资金；实盘账户复用服务端钱包凭据，每个实例只能有一个。</DialogDescription>
          <div>
            <label htmlFor="new-account-name" className="text-sm font-medium text-slate-700">账户名称</label>
            <Input id="new-account-name" value={newName} onChange={(event) => setNewName(event.target.value)} placeholder="例如：策略验证 A" className="mt-2" autoFocus />
          </div>
          <div>
            <label htmlFor="new-account-mode" className="text-sm font-medium text-slate-700">账户模式</label>
            <Select value={newMode} onValueChange={(value) => setNewMode(value as AccountMode)}>
              <SelectTrigger id="new-account-mode" className="mt-2">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="paper">模拟账户（虚拟资金）</SelectItem>
                <SelectItem value="live" disabled={hasLive}>实盘账户（真实资金）{hasLive ? ' · 已存在' : ''}</SelectItem>
              </SelectContent>
            </Select>
          </div>
          <div className="flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
            <Button variant="secondary" onClick={() => setCreateOpen(false)} disabled={creating}>取消</Button>
            <Button variant="primary" onClick={() => void createAccount()} loading={creating}>创建账户</Button>
          </div>
        </DialogContent>
      </Dialog>
    </Card>
  )
}

export function SettingsView() {
  const { account, accounts } = useAccount()
  const [draft, setDraft] = useState<Draft | null>(null)
  const [dirty, setDirty] = useState(false)
  const [errors, setErrors] = useState<FieldErrors>({})
  const [saving, setSaving] = useState(false)
  const [toggling, setToggling] = useState(false)
  const [engineOverride, setEngineOverride] = useState<Engine | null>(null)
  const saveController = useRef<AbortController | null>(null)
  const latestSettings = useRef<Settings | null>(null)
  const { toast } = useToast()
  const fetchSettings = useCallback((signal: AbortSignal) => api.settings(signal), [])
  const fetchDashboard = useCallback((signal: AbortSignal) => api.dashboard(account?.id, signal), [account?.id])
  const settingsQuery = usePolling(fetchSettings, { interval: 30000 })
  const dashboardQuery = usePolling(fetchDashboard, { enabled: Boolean(account?.id) })
  const engine = engineOverride || dashboardQuery.data?.engine || null
  const isLive = account?.mode === 'live'

  useEffect(() => {
    if (!settingsQuery.data) return
    latestSettings.current = settingsQuery.data
    if (!dirty) setDraft(toDraft(settingsQuery.data))
  }, [settingsQuery.data])

  useEffect(() => () => saveController.current?.abort(), [])

  const update = (key: NumericKey, value: string) => {
    setDraft((current) => current ? { ...current, [key]: value } : current)
    setDirty(true)
    setErrors((current) => ({ ...current, [key]: undefined }))
  }

  const reset = () => {
    if (!latestSettings.current) return
    setDraft(toDraft(latestSettings.current))
    setErrors({})
    setDirty(false)
  }

  const save = async () => {
    if (!draft) return
    const payload = toSettings(draft)
    const validation = validate(payload)
    setErrors(validation)
    if (Object.keys(validation).length > 0) {
      toast({ title: '请先修正参数', description: '部分字段不符合约束，请查看红色提示。', variant: 'error' })
      return
    }
    saveController.current?.abort()
    const controller = new AbortController()
    saveController.current = controller
    setSaving(true)
    try {
      const saved = await api.updateSettings(payload, controller.signal)
      latestSettings.current = saved
      setDraft(toDraft(saved))
      setDirty(false)
      setErrors({})
      toast({ title: '参数已保存', description: '新配置已提交，后续扫描与风控将使用该配置。', variant: 'success' })
      settingsQuery.refresh()
    } catch (caught) {
      if (caught instanceof DOMException && caught.name === 'AbortError') return
      toast({ title: '保存失败', description: caught instanceof Error ? caught.message : '请稍后重试。', variant: 'error' })
    } finally {
      if (saveController.current === controller) setSaving(false)
    }
  }

  const toggleEngine = async (running: boolean) => {
    setToggling(true)
    try {
      const next = await api.toggleEngine(running)
      setEngineOverride(next)
      toast({ title: running ? '扫描引擎已启动' : '扫描引擎已暂停', description: running ? '将按设置间隔扫描公共行情，并按账户开关决定是否自动交易。' : '不再自动扫描，已有持仓保持不变。', variant: 'success' })
      dashboardQuery.refresh()
    } catch (caught) {
      toast({ title: '引擎状态切换失败', description: caught instanceof Error ? caught.message : '请稍后重试。', variant: 'error' })
    } finally {
      setToggling(false)
    }
  }

  return (
    <>
      <PageHeader eyebrow={isLive ? 'LIVE CONFIGURATION' : 'PAPER CONFIGURATION'} title="参数设置" description={isLive ? '配置公共行情筛选、实盘风控与账户级自动交易。实盘成交价由 Polymarket 订单簿决定。' : '配置公共行情筛选、虚拟资金与 Paper 成交参数。市场筛选与风控对实盘、模拟账户同时生效。'} />

      <Card className="mb-4 overflow-hidden border-navy bg-navy text-white">
        <div className="flex flex-col gap-5 p-5 sm:flex-row sm:items-center sm:p-6">
          <div className={cn('rounded-2xl p-3', engine?.running ? 'bg-brand-lime text-navy' : 'bg-white/10 text-slate-400')}><Activity className="h-6 w-6" /></div>
          <div className="min-w-0 flex-1"><div className="flex flex-wrap items-center gap-2"><h2 className="text-base font-semibold">扫描引擎</h2>{engine && <Badge variant={engine.running ? 'lime' : 'navy'}>{engine.running ? '运行中' : '已暂停'}</Badge>}</div><p className="mt-1.5 text-sm leading-6 text-slate-400">控制公共行情扫描；是否自动下单由各账户的「自动交易」开关独立决定。</p>{engine?.last_error && <p className="mt-2 break-words text-xs leading-5 text-rose-300">最近错误：{engine.last_error}</p>}</div>
          <div className="flex items-center gap-3 sm:flex-col sm:items-end"><Switch checked={engine?.running || false} onCheckedChange={toggleEngine} disabled={!engine || toggling || dashboardQuery.isLoading} aria-label="启动或暂停扫描引擎" className="data-[state=checked]:bg-emerald-500" /><span className="text-xs text-slate-400">{toggling ? '正在切换…' : engine?.running ? '点击暂停' : '点击启动'}</span></div>
        </div>
        {dashboardQuery.error && !engine && <div className="break-words border-t border-white/10 bg-white/5 px-5 py-3 text-xs text-rose-300">{dashboardQuery.error}</div>}
      </Card>

      {account && <div className="mb-4"><AccountSection key={account.id} account={account} accounts={accounts} /></div>}

      {settingsQuery.isLoading ? <Card><div className="grid gap-5 p-6 sm:grid-cols-2"><Skeleton className="h-24" /><Skeleton className="h-24" /><Skeleton className="h-24" /><Skeleton className="h-24" /></div></Card> : settingsQuery.error && !settingsQuery.data ? <Card><ErrorState message={settingsQuery.error} onRetry={settingsQuery.refresh} /></Card> : !draft ? <Card><div className="grid gap-5 p-6 sm:grid-cols-2"><Skeleton className="h-24" /><Skeleton className="h-24" /><Skeleton className="h-24" /><Skeleton className="h-24" /></div></Card> : (
        <form id="settings-form" onSubmit={(event) => { event.preventDefault(); void save() }} className="space-y-4">
          {settingsQuery.error && <div className="rounded-xl border border-amber-200 bg-amber-50 px-4 py-3 text-xs text-amber-800">后台刷新失败，当前表单仍可编辑：{settingsQuery.error}</div>}
          <SettingsSection icon={SlidersHorizontal} title="市场筛选" description="只保留符合价格、到期时间与流动性要求的公开市场候选。实盘与模拟共用同一套筛选条件。">
            <div className="grid gap-x-5 gap-y-6 sm:grid-cols-2 xl:grid-cols-4">
              <NumberField id="min_price" label="最低指示价格" description="必须大于 0，且不高于最高价格。" value={draft.min_price} unit="USD" min={0} max={1} error={errors.min_price} onChange={(value) => update('min_price', value)} />
              <NumberField id="max_price" label="最高指示价格" description="必须小于 1，且不低于最低价格。" value={draft.max_price} unit="USD" min={0} max={1} error={errors.max_price} onChange={(value) => update('max_price', value)} />
              <NumberField id="max_days" label="最长到期时间" description="筛选 1–3650 天内到期的市场。" value={draft.max_days} unit="天" min={1} max={3650} step="1" error={errors.max_days} onChange={(value) => update('max_days', value)} />
              <NumberField id="min_liquidity" label="最低流动性" description="低于该值的市场不会进入雷达。" value={draft.min_liquidity} unit="USD" min={0} error={errors.min_liquidity} onChange={(value) => update('min_liquidity', value)} />
            </div>
          </SettingsSection>

          <SettingsSection icon={ShieldCheck} title="资金与风控" description="风控上限同时作用于实盘与模拟账户；初始资金仅用于模拟账户。">
            <div className="grid gap-x-5 gap-y-6 sm:grid-cols-2 xl:grid-cols-4">
              <NumberField id="initial_bankroll" label="模拟初始资金" description="模拟账户的基准资金，不是真实现金余额。实盘基准由钱包余额自动初始化。" value={draft.initial_bankroll} unit="USD" min={0} error={errors.initial_bankroll} onChange={(value) => update('initial_bankroll', value)} />
              <NumberField id="order_size" label="单笔金额" description="每次自动买入的目标名义金额。" value={draft.order_size} unit="USD" min={0} error={errors.order_size} onChange={(value) => update('order_size', value)} />
              <NumberField id="max_position" label="单市场持仓上限" description="不得低于单笔金额。" value={draft.max_position} unit="USD" min={0} error={errors.max_position} onChange={(value) => update('max_position', value)} />
              <NumberField id="max_exposure" label="总敞口上限" description="全部未平仓头寸的风险预算上限。" value={draft.max_exposure} unit="USD" min={0} error={errors.max_exposure} onChange={(value) => update('max_exposure', value)} />
            </div>
          </SettingsSection>

          <SettingsSection icon={TimerReset} title="扫描与模拟成交" description="调整扫描频率，以及模拟成交时计入的滑点和费用。实盘成交价以 CLOB 订单簿为准。">
            <div className="grid gap-x-5 gap-y-6 sm:grid-cols-2 xl:grid-cols-3">
              <NumberField id="scan_interval_seconds" label="扫描间隔" description="允许 10–86400 秒，需为整数。" value={draft.scan_interval_seconds} unit="秒" min={10} max={86400} step="1" error={errors.scan_interval_seconds} onChange={(value) => update('scan_interval_seconds', value)} />
              <NumberField id="slippage_bps" label="Paper 滑点" description="0–1000 bps；仅影响模拟成交。" value={draft.slippage_bps} unit="bps" min={0} max={1000} error={errors.slippage_bps} onChange={(value) => update('slippage_bps', value)} />
              <NumberField id="fee_bps" label="Paper 费用" description="0–1000 bps；仅用于模拟核算。" value={draft.fee_bps} unit="bps" min={0} max={1000} error={errors.fee_bps} onChange={(value) => update('fee_bps', value)} />
            </div>
            <div className="mt-6 flex items-start gap-3 rounded-xl border border-slate-200 bg-slate-50 p-4">
              <div className="rounded-xl bg-surface p-2.5 text-slate-500"><UserCog className="h-5 w-5" /></div>
              <div className="flex-1"><p className="text-sm font-semibold text-slate-800">自动交易开关已移到账户管理</p><p className="mt-1 text-xs leading-5 text-slate-500">实盘与模拟账户可以分别开启自动交易。实盘账户开启时需要二次确认。</p></div>
            </div>
          </SettingsSection>

          <div className="sticky bottom-4 z-10 flex flex-col gap-3 rounded-2xl border border-slate-200 bg-surface/95 p-4 shadow-float backdrop-blur sm:flex-row sm:items-center">
            <div className="flex flex-1 items-start gap-2 text-xs leading-5 text-slate-500"><Info className="mt-0.5 h-4 w-4 shrink-0 text-slate-400" /><span>{dirty ? '有尚未保存的更改。保存前请确认风控上限。' : '当前参数已与后端同步。'} 筛选与风控参数对实盘、模拟账户同时生效。</span></div>
            <div className="flex gap-2"><Button type="button" variant="secondary" onClick={reset} disabled={!dirty || saving}><RotateCcw className="h-4 w-4" />放弃更改</Button><Button type="submit" variant="primary" loading={saving} disabled={!dirty}>{saving ? '正在保存…' : <><Save className="h-4 w-4" />保存参数</>}</Button></div>
          </div>
        </form>
      )}

      <div className="mt-5 flex items-start gap-2 text-xs leading-5 text-slate-400"><Gauge className="mt-0.5 h-3.5 w-3.5 shrink-0" /><p>{isLive ? '实盘建议先使用最小金额验证钱包授权、盘口深度与成交回报，再逐步提高敞口。' : '建议先使用保守上限验证策略。模拟成交规则无法复现真实市场的流动性、排队与价格冲击。'}</p></div>
    </>
  )
}
