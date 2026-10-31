import { useCallback, useState, type ReactNode } from 'react'
import {
  Activity,
  Binoculars,
  BookOpenCheck,
  CircleDollarSign,
  Cog,
  FlaskConical,
  LayoutDashboard,
  Menu,
  Radio,
  Settings2,
  ShieldAlert,
  ShieldCheck,
  WalletCards,
} from 'lucide-react'
import type { LucideIcon } from 'lucide-react'
import { api } from '@/lib/api'
import { useAccount } from '@/lib/account-context'
import { usePolling } from '@/hooks/usePolling'
import { cn } from '@/lib/utils'
import { Badge } from '@/components/ui/badge'
import { Dialog, DialogContent, DialogDescription, DialogTitle } from '@/components/ui/dialog'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { RiskNotice } from '@/components/risk-notice'
import { ModeBadge } from '@/components/status-badges'
import { ThemeToggle } from '@/components/theme-toggle'

export type View = 'dashboard' | 'opportunities' | 'positions' | 'orders' | 'settings' | 'system'

const navigation: { id: View; label: string; icon: LucideIcon; section?: string }[] = [
  { id: 'dashboard', label: '账户总览', icon: LayoutDashboard, section: '工作台' },
  { id: 'opportunities', label: '机会雷达', icon: Binoculars },
  { id: 'positions', label: '我的持仓', icon: WalletCards },
  { id: 'orders', label: '订单记录', icon: BookOpenCheck },
  { id: 'settings', label: '参数设置', icon: Settings2, section: '系统' },
  { id: 'system', label: '系统设置', icon: Cog },
]

const viewNames: Record<View, string> = {
  dashboard: '账户总览',
  opportunities: '机会雷达',
  positions: '我的持仓',
  orders: '订单记录',
  settings: '参数设置',
  system: '系统设置',
}

function Brand() {
  return (
    <div className="flex items-center gap-3">
      <div className="relative flex h-10 w-10 shrink-0 items-center justify-center rounded-xl bg-brand-lime text-navy shadow-[0_0_24px_rgba(199,243,107,0.12)]">
        <CircleDollarSign className="h-6 w-6" strokeWidth={1.8} />
        <span className="absolute -right-0.5 -top-0.5 h-2.5 w-2.5 rounded-full border-2 border-navy bg-emerald-400" />
      </div>
      <div>
        <div className="text-[15px] font-semibold tracking-[-0.02em] text-white">PennyHarvester</div>
        <div className="mt-0.5 text-[11px] tracking-wide text-slate-400">Polymarket 尾盘策略工作台</div>
      </div>
    </div>
  )
}

function Navigation({ view, onNavigate }: { view: View; onNavigate: (view: View) => void }) {
  const { account } = useAccount()
  const isLive = account?.mode === 'live'
  return (
    <div className="flex h-full flex-col bg-navy">
      <div className="flex h-[76px] items-center border-b border-white/[0.07] px-5"><Brand /></div>
      <nav className="flex-1 space-y-1 overflow-y-auto px-3 py-5" aria-label="主导航">
        {navigation.map((item) => {
          const Icon = item.icon
          const selected = view === item.id
          return (
            <div key={item.id}>
              {item.section && <p className="mb-2 mt-2 px-3 text-[10px] font-semibold uppercase tracking-[0.18em] text-slate-500">{item.section}</p>}
              <button
                type="button"
                onClick={() => onNavigate(item.id)}
                aria-current={selected ? 'page' : undefined}
                className={cn(
                  'group flex h-11 w-full items-center gap-3 rounded-xl px-3 text-sm transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand-lime',
                  selected ? 'bg-white/[0.09] font-medium text-white' : 'text-slate-400 hover:bg-white/[0.05] hover:text-white',
                )}
              >
                <Icon className={cn('h-[18px] w-[18px]', selected ? 'text-brand-lime' : 'text-slate-500 group-hover:text-white/80')} strokeWidth={1.8} />
                <span>{item.label}</span>
                {selected && <span className="ml-auto h-1.5 w-1.5 rounded-full bg-brand-lime" />}
              </button>
            </div>
          )
        })}
      </nav>
      <div className="p-4">
        <div className={cn('rounded-2xl border p-4', isLive ? 'border-amber-400/20 bg-amber-400/[0.07]' : 'border-white/[0.08] bg-white/[0.045]')}>
          <div className={cn('flex items-center gap-2', isLive ? 'text-amber-300' : 'text-brand-lime')}>
            {isLive ? <Radio className="h-4 w-4" /> : <FlaskConical className="h-4 w-4" />}
            <span className="text-xs font-semibold">{isLive ? '实盘交易模式' : '模拟演练模式'}</span>
          </div>
          <p className="mt-2 text-[11px] leading-5 text-slate-400">
            {isLive ? <>直接连接 Polymarket 真实盘口<br />使用真实 pUSD 下单交易</> : <>同步 Polymarket 实时公开行情<br />使用虚拟资金，不会真实下单</>}
          </p>
          <div className="mt-3 flex items-center gap-1.5 text-[10px] text-slate-500">
            {isLive ? <ShieldAlert className="h-3.5 w-3.5" /> : <ShieldCheck className="h-3.5 w-3.5" />}
            {isLive ? '注意资金安全，建议先小额试跑' : '零成本验证策略，不构成投资建议'}
          </div>
        </div>
      </div>
    </div>
  )
}

function AccountSwitcher() {
  const { accounts, account, selectAccount, isLoading } = useAccount()
  return (
    <Select
      value={account ? String(account.id) : 'none'}
      onValueChange={(value) => {
        if (value !== 'none') selectAccount(Number(value))
      }}
      disabled={isLoading || accounts.length === 0}
    >
      <SelectTrigger
        className="h-9 w-[132px] gap-1.5 border-slate-200 bg-surface px-2.5 text-xs font-medium text-slate-700 shadow-sm sm:w-[220px]"
        aria-label="切换账户"
      >
        <SelectValue placeholder="无账户" className="min-w-0 flex-1 truncate text-left" />
      </SelectTrigger>
      <SelectContent align="end">
        {accounts.length === 0 ? (
          <SelectItem value="none" disabled>无账户</SelectItem>
        ) : accounts.map((item) => (
          <SelectItem key={item.id} value={String(item.id)}>
            {item.name} · {item.mode === 'live' ? '实盘' : '模拟'}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  )
}

export function AppShell({ view, onNavigate, children }: { view: View; onNavigate: (view: View) => void; children: ReactNode }) {
  const [mobileOpen, setMobileOpen] = useState(false)
  const fetchHealth = useCallback((signal: AbortSignal) => api.health(signal), [])
  const { data: health, error: healthError } = usePolling(fetchHealth, { interval: 30000 })
  const { account } = useAccount()

  const navigate = (next: View) => {
    onNavigate(next)
    setMobileOpen(false)
  }

  const connected = health?.status === 'ok' && !healthError
  const isLive = account?.mode === 'live'

  return (
    <div className="min-h-screen bg-canvas text-ink">
      <aside className="fixed inset-y-0 left-0 z-30 hidden w-[248px] border-r border-white/[0.06] lg:block">
        <Navigation view={view} onNavigate={navigate} />
      </aside>

      <Dialog open={mobileOpen} onOpenChange={setMobileOpen}>
        <DialogContent className="left-0 top-0 h-full max-h-none w-[280px] max-w-[85vw] -translate-x-0 -translate-y-0 rounded-none border-0 bg-navy p-0 text-white [&>button]:z-10 [&>button]:text-slate-400 [&>button]:hover:bg-white/10 [&>button]:hover:text-white">
          <DialogTitle className="sr-only">导航菜单</DialogTitle>
          <DialogDescription className="sr-only">选择要查看的页面</DialogDescription>
          <Navigation view={view} onNavigate={navigate} />
        </DialogContent>
      </Dialog>

      <div className="lg:pl-[248px]">
        <header className="sticky top-0 z-20 flex h-[68px] items-center border-b border-white/10 bg-navy px-4 text-white lg:h-[76px] lg:border-slate-200/80 lg:bg-surface/90 lg:px-8 lg:text-slate-900 lg:backdrop-blur-xl">
          <button className="mr-3 rounded-lg p-2 text-white/80 hover:bg-white/10 hover:text-white lg:hidden" onClick={() => setMobileOpen(true)} aria-label="打开导航菜单">
            <Menu className="h-5 w-5" />
          </button>
          <div className="min-w-0 lg:hidden">
            <div className="truncate text-sm font-semibold">{viewNames[view]}</div>
            <div className="mt-0.5 truncate text-[10px] text-slate-400">尾盘策略工作台</div>
          </div>
          <div className="hidden items-center gap-2 text-sm lg:flex">
            <span className="text-slate-400">{isLive ? '实盘交易' : '模拟演练'}</span>
            <span className="text-slate-300">/</span>
            <span className="font-medium text-slate-800">{viewNames[view]}</span>
          </div>
          <div className="ml-auto flex items-center gap-2 sm:gap-3">
            <div className="hidden items-center gap-2 text-xs text-slate-500 xl:flex" title={healthError || undefined}>
              <span className={cn('h-2 w-2 rounded-full', connected ? 'bg-emerald-500' : healthError ? 'bg-rose-500' : 'animate-pulse bg-slate-300')} />
              {connected ? '服务在线' : healthError ? '连接已断开' : '正在连接…'}
            </div>
            <div className="hidden h-5 w-px bg-slate-200 xl:block" />
            <ThemeToggle />
            <AccountSwitcher />
            {account && <ModeBadge mode={account.mode} className="hidden sm:inline-flex" />}
            {isLive && account && !account.live_ready && (
              <Badge variant={account.live_configured ? 'warning' : 'danger'} className="hidden lg:inline-flex"><ShieldAlert className="h-3 w-3" /> {account.live_configured ? '实盘未就绪' : '实盘未配置'}</Badge>
            )}
          </div>
        </header>

        <main className="mx-auto w-full max-w-[1600px] px-4 py-6 sm:px-6 sm:py-8 lg:px-8 lg:py-9">
          {account?.mode === 'live' && !account.live_ready && (
            <div className="mb-5 flex items-start gap-3 rounded-xl border border-rose-200 bg-rose-50 px-4 py-3 text-sm text-rose-800">
              <ShieldAlert className="mt-0.5 h-4 w-4 shrink-0" />
              <div>
                <p className="font-semibold">{account.live_configured ? '实盘账户还差一步即可就绪' : '实盘账户尚未配置钱包私钥'}</p>
                <p className="mt-1 text-xs leading-5 text-rose-700">
                  {account.live_configured ? <>{account.live_error || '请检查代理钱包地址（POLYMARKET_FUNDER）、API 凭据以及 Polymarket 上的 pUSD 授权状态。'}</> : <>尚未配置实盘私钥。可在「系统设置 → 实盘交易凭据」中填写，或设置 <code className="rounded bg-rose-100 px-1 py-0.5">POLYMARKET_PRIVATE_KEY</code> 后重启服务。你也可以先在右上角切换到「模拟账户」体验策略。</>}
                </p>
              </div>
            </div>
          )}
          <div key={`${account?.id ?? 'none'}-${view}`} className="animate-fade-up">{children}</div>
          <footer className="mt-8 border-t border-slate-200/70 pt-5">
            <RiskNotice />
            <div className="mt-4 flex flex-col gap-1 text-[11px] text-slate-400 sm:flex-row sm:items-center sm:justify-between">
              <span>{isLive ? 'PennyHarvester · Polymarket 实盘交易模式' : 'PennyHarvester · Polymarket 模拟演练模式'}</span>
              <span className="flex items-center gap-1.5"><Activity className="h-3 w-3" /> 页面开启时每 20 秒自动同步最新数据</span>
            </div>
          </footer>
        </main>
      </div>
    </div>
  )
}
