import { useEffect, useState } from 'react'
import { FlaskConical, Info, Plus, Radio, Save, ShieldAlert, Star, Trash2 } from 'lucide-react'
import { api } from '@/lib/api'
import { useAccount } from '@/lib/account-context'
import { useToast } from '@/components/toast'
import type { Account, AccountMode, Venue } from '@/types'
import { fmtMoney, VENUES, venueLabel, venueMeta, venueOf } from '@/lib/venue'
import { cn } from '@/lib/utils'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import { ModeBadge, VenueChip } from '@/components/status-badges'
import { NumberField } from '@/components/ui/form-field'

export function AccountSection({ account, accounts }: { account: Account; accounts: Account[] }) {
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
  // Uniqueness is per target venue, not the currently selected account's venue.
  // A live Predict.fun account may be created while the selected account is a
  // live Polymarket one; only an existing live account on the target blocks it.
  const liveVenues = new Set(accounts.filter((item) => item.mode === 'live').map(venueOf))
  const hasLive = liveVenues.has(newVenue)
  const changeNewVenue = (next: Venue) => {
    setNewVenue(next)
    // Keep the mode valid when the operator switches to a venue that already
    // has a live account, instead of leaving a disabled "live" option selected.
    if (liveVenues.has(next)) setNewMode('paper')
  }

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
    if (newMode === 'live' && hasLive) {
      toast({ title: '该场所已有实盘账户', description: `${venueMeta(newVenue).label} 只能有一个实盘账户。`, variant: 'error' })
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
              <CardDescription className="mt-1">实盘与模拟是完全隔离的账户，且每个账户只属于一个场所。后台自动交易按账户单独开启；雷达「立即扫描」也可按次选择执行自动交易（含止损卖出）。</CardDescription>
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
              <Button variant="ghost" size="sm" onClick={() => { setNewBankroll(''); setCreateOpen(true) }}><Plus className="h-3.5 w-3.5" />新建账户</Button>
            </div>
          </div>

          <div className={cn('rounded-xl border p-4', isLive ? 'border-amber-200 bg-amber-50/50' : 'border-emerald-100 bg-emerald-50/50')}>
            <div className="flex items-center justify-between gap-3">
              <div><p className="text-sm font-semibold text-slate-800">{isLive ? '实盘自动交易' : '模拟自动交易'}</p><p className="mt-0.5 text-xs leading-5 text-slate-500">{isLive ? `开启后，后台扫描引擎将按策略参数向 ${meta.label} 提交真实买单。` : '开启后，后台扫描引擎将按策略参数创建虚拟买单。'}</p></div>
              <Switch checked={account.auto_trade} onCheckedChange={toggleAuto} disabled={saving || (isLive && !account.live_ready && !account.auto_trade)} aria-label={isLive ? '实盘自动交易' : '模拟自动交易'} />
            </div>
            <dl className="mt-4 space-y-2 text-xs">
              <div className="flex justify-between gap-3"><dt className="text-slate-500">账户模式</dt><dd className="font-medium text-slate-800">{isLive ? `真实资金 · ${meta.label} · ${meta.orderTypes.join(' / ')}` : `虚拟资金 · ${meta.label} · 本地撮合`}</dd></div>
              <div className="flex justify-between gap-3"><dt className="text-slate-500">{isLive ? '基准权益' : '模拟基准权益'}</dt><dd className="font-medium tabular-nums text-slate-800">{account.baseline_equity > 0 ? fmtMoney(account.baseline_equity, venue) : isLive ? '待首次余额同步' : '未设置'}</dd></div>
              {isLive && <div className="flex justify-between gap-3"><dt className="text-slate-500">交易钱包 (Maker)</dt><dd className="max-w-[220px] truncate font-mono text-[11px] text-slate-700" title={account.live_maker || account.live_signer}>{account.live_maker || account.live_signer || '未配置'}</dd></div>}
              {isLive && account.live_maker && account.live_signer && account.live_maker.toLowerCase() !== account.live_signer.toLowerCase() && <div className="flex justify-between gap-3"><dt className="text-slate-500">签名地址</dt><dd className="max-w-[220px] truncate font-mono text-[11px] text-slate-500" title={account.live_signer}>{account.live_signer}</dd></div>}
            </dl>
            {!isLive && account.baseline_equity <= 0 && (
              <p className="mt-3 rounded-lg border border-amber-200 bg-amber-50 p-3 text-xs leading-5 text-amber-800">该模拟账户没有初始资金，无法下单。请删除后重新创建，并在创建时填写初始资金。</p>
            )}
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
            <Select value={newVenue} onValueChange={(value) => changeNewVenue(value as Venue)}>
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
              label="模拟初始资金（必填）"
              description={`写入该账户的基准权益，必须大于 0，单位 ${venueMeta(newVenue).currency}；创建后保持不变，不会随策略参数修改而重算。`}
              value={newBankroll}
              unit={venueMeta(newVenue).currency}
              min={0}
              placeholder="请输入初始资金"
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
