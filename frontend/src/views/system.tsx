import { useCallback, useEffect, useState } from 'react'
import {
  AlertTriangle,
  CheckCircle2,
  Database,
  Globe2,
  KeyRound,
  LoaderCircle,
  LogOut,
  Network,
  RotateCcw,
  Save,
  ShieldCheck,
  ShieldOff,
  Wallet,
  Wifi,
  XCircle,
} from 'lucide-react'
import { api } from '@/lib/api'
import { cn } from '@/lib/utils'
import { useToast } from '@/components/toast'
import type {
  DataSourceSettings,
  LiveEnvironment,
  LiveProbeResult,
  LiveSettings,
  ProxyConfig,
  ProxyMode,
  ProxyTestResult,
  RelayerProbeResult,
  SystemSettings,
} from '@/types'
import { formatCurrency } from '@/lib/format'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogTitle } from '@/components/ui/dialog'
import { ErrorState, Skeleton } from '@/components/ui/feedback'
import { Input } from '@/components/ui/input'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { PageHeader } from '@/components/page-header'

const modeLabels: Record<ProxyMode, string> = {
  environment: '跟随环境变量',
  custom: '自定义代理',
  off: '直连（忽略环境变量）',
}

const REDACTED_SECRET = '******'
const PUBLIC_GAMMA_URL = 'https://gamma-api.polymarket.com'
const PUBLIC_CLOB_URL = 'https://clob.polymarket.com'

function Section({ icon: Icon, title, description, children, actions }: {
  icon: typeof Network
  title: string
  description: string
  children: React.ReactNode
  actions?: React.ReactNode
}) {
  return (
    <Card>
      <CardHeader className="border-b border-slate-100">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="flex items-start gap-3">
            <div className="rounded-xl bg-slate-100 p-2.5 text-slate-500"><Icon className="h-[18px] w-[18px]" /></div>
            <div>
              <CardTitle>{title}</CardTitle>
              <CardDescription className="mt-1">{description}</CardDescription>
            </div>
          </div>
          {actions}
        </div>
      </CardHeader>
      <CardContent className="pt-5 sm:pt-6">{children}</CardContent>
    </Card>
  )
}

function urlError(value: string): string | null {
  const trimmed = value.trim()
  if (!trimmed) return null
  try {
    const parsed = new URL(trimmed)
    if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') return '仅支持 http 或 https 地址。'
    if (!parsed.hostname) return '地址缺少主机名。'
    return null
  } catch {
    return '地址格式无效，请填写完整的 http(s) URL。'
  }
}

// The live configuration has two shapes. An EOA signs and holds funds with the
// same key; a Polymarket proxy/deposit wallet only lets the key sign, while the
// collateral lives in the funder address. The UI switches fields and hints on
// this mode instead of asking the operator to know signature type numbers.
function effectiveSignatureType(draft: LiveSettings, environment: LiveEnvironment): number {
  return draft.signature_type ?? environment.signature_type
}

function walletMode(draft: LiveSettings, environment: LiveEnvironment): 'eoa' | 'proxy' {
  return effectiveSignatureType(draft, environment) === 0 ? 'eoa' : 'proxy'
}

function liveValidation(draft: LiveSettings, environment: LiveEnvironment): string | null {
  const key = draft.private_key.trim()
  if (key && key !== REDACTED_SECRET && !/^0x[0-9a-fA-F]{64}$/.test(key)) {
    return '私钥必须是 0x 开头的 64 位十六进制字符串。'
  }
  const funder = draft.funder.trim()
  const effectiveFunder = funder || environment.funder.trim()
  if (funder && !/^0x[0-9a-fA-F]{40}$/.test(funder)) {
    return 'Funder 必须是有效的 0x 地址。'
  }
  if (walletMode(draft, environment) === 'eoa' && effectiveFunder) {
    return funder
      ? '自托管 EOA 模式下 Funder 必须留空。'
      : '自托管 EOA 模式要求 Funder 留空，但环境变量中仍配置了 Funder；请移除 POLYMARKET_FUNDER 后重启。'
  }
  if (walletMode(draft, environment) === 'proxy' && !effectiveFunder) {
    return '代理钱包模式需要填写 Funder（Polymarket 账户里的代理 / Deposit 钱包地址）。'
  }
  const credentials = [draft.api_key.trim(), draft.api_secret.trim(), draft.api_passphrase.trim()]
  const anySet = credentials.some(Boolean)
  const allSet = credentials.every(Boolean)
  if (anySet && !allSet) {
    return 'L2 API Key、Secret、Passphrase 必须同时填写或同时留空。若你只有 polymarket.com 生成的 Relayer API Key（UUID + 地址），请把这三项全部留空，系统会用私钥自动派生 CLOB 凭据。'
  }
  return null
}

function EndpointSection({ system, draft, dirty, saving, busy, onChange, onDiscard, onClear, onSave }: {
  system: SystemSettings
  draft: DataSourceSettings
  dirty: boolean
  saving: boolean
  busy: boolean
  onChange: (patch: Partial<DataSourceSettings>) => void
  onDiscard: () => void
  onClear: () => void
  onSave: () => void
}) {
  const runtime = system.runtime
  return (
    <Section
      icon={Database}
      title="行情与交易接口"
      description="覆盖 Polymarket 的公开行情（Gamma）与实盘交易（CLOB）地址。留空表示继续使用启动环境变量或官方默认地址。"
      actions={<Badge variant="success">已生效</Badge>}
    >
      <div className="grid gap-5 lg:grid-cols-2">
        <div>
          <label htmlFor="gamma-base-url" className="text-sm font-medium text-slate-700">Gamma 行情地址</label>
          <p className="mt-1 min-h-8 text-xs leading-4 text-slate-400">用于机会雷达与持仓结算。支持自建镜像或本地代理。</p>
          <Input
            id="gamma-base-url"
            value={draft.gamma_base_url}
            onChange={(event) => onChange({ gamma_base_url: event.target.value })}
            placeholder={runtime.gamma_default || PUBLIC_GAMMA_URL}
            spellCheck={false}
            autoComplete="off"
            className="mt-2 font-mono text-xs"
          />
          <p className="mt-1.5 text-[11px] leading-4 text-slate-400">当前生效：<code className="break-all rounded bg-slate-100 px-1">{runtime.gamma_base_url || runtime.gamma_default || PUBLIC_GAMMA_URL}</code></p>
        </div>
        <div>
          <label htmlFor="clob-base-url" className="text-sm font-medium text-slate-700">CLOB 交易地址</label>
          <p className="mt-1 min-h-8 text-xs leading-4 text-slate-400">用于实盘下单、手续费读取、余额查询与订单对账。</p>
          <Input
            id="clob-base-url"
            value={draft.clob_base_url}
            onChange={(event) => onChange({ clob_base_url: event.target.value })}
            placeholder={runtime.clob_default || PUBLIC_CLOB_URL}
            spellCheck={false}
            autoComplete="off"
            className="mt-2 font-mono text-xs"
          />
          <p className="mt-1.5 text-[11px] leading-4 text-slate-400">当前生效：<code className="break-all rounded bg-slate-100 px-1">{runtime.clob_base_url || runtime.clob_default || PUBLIC_CLOB_URL}</code></p>
        </div>
      </div>

      <div className="mt-5 flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-end">
        <p className="flex-1 text-xs leading-5 text-slate-400">保存后新的行情与交易请求立即使用新地址，无需重启。留空即可恢复环境变量或官方默认值。</p>
        <Button variant="secondary" onClick={onDiscard} disabled={!dirty || busy}><RotateCcw className="h-4 w-4" />放弃更改</Button>
        <Button variant="ghost" onClick={onClear} disabled={busy}>清空为默认值</Button>
        <Button variant="primary" onClick={onSave} loading={saving} disabled={!dirty || busy}><Save className="h-4 w-4" />保存接口设置</Button>
      </div>
    </Section>
  )
}

function LiveTradingSection({ system, draft, dirty, saving, busy, clobBaseURL, onChange, onReset, onSave }: {
  system: SystemSettings
  draft: LiveSettings
  dirty: boolean
  saving: boolean
  busy: boolean
  clobBaseURL: string
  onChange: (patch: Partial<LiveSettings>) => void
  onReset: () => void
  onSave: () => void
}) {
  const { toast } = useToast()
  const [testing, setTesting] = useState(false)
  const [probe, setProbe] = useState<LiveProbeResult | null>(null)
  const [relayerKey, setRelayerKey] = useState('')
  const [relayerAddress, setRelayerAddress] = useState('')
  const [relayerTesting, setRelayerTesting] = useState(false)
  const [relayerProbe, setRelayerProbe] = useState<RelayerProbeResult | null>(null)
  const [discoveringFunder, setDiscoveringFunder] = useState(false)
  // Any edit invalidates the previous result so a stale "passed" badge cannot
  // be mistaken for a check of the current form values.
  useEffect(() => {
    setProbe(null)
    setRelayerProbe(null)
  }, [draft])
  const runtime = system.runtime
  const environment = runtime.live_environment
  const statusVariant = runtime.live_ready ? 'success' : runtime.live_configured ? 'warning' : 'default'
  const statusLabel = runtime.live_ready ? '凭据本地校验通过' : runtime.live_configured ? '已配置但未就绪' : '未配置实盘凭据'
  const mode = walletMode(draft, environment)
  const proxyType = effectiveSignatureType(draft, environment) === 0 ? 3 : effectiveSignatureType(draft, environment)
  const clearCredentials = () => onChange({ api_key: '', api_secret: '', api_passphrase: '' })
  const environmentCredentialsSet = environment.api_key_set && environment.api_secret_set && environment.api_passphrase_set
  const environmentCredentialsPartial = (environment.api_key_set || environment.api_secret_set || environment.api_passphrase_set) && !environmentCredentialsSet

  const discoverFunder = async () => {
    setDiscoveringFunder(true)
    try {
      const result = await api.discoverFunder(draft, clobBaseURL)
      if (!result.ok) {
        toast({ title: '未能自动获取 Funder', description: result.error || '请检查私钥与 CLOB 地址。', variant: 'error' })
        return
      }
      const wallets = result.wallets ?? []
      if (wallets.length === 0) {
        toast({
          title: 'CLOB 未返回关联钱包',
          description: '请先在 polymarket.com 用这个私钥对应的钱包登录一次，让平台为它创建 Deposit 钱包；或从个人菜单复制地址后手动填入。',
          variant: 'info',
        })
        return
      }
      onChange({ funder: wallets[0] })
      toast({
        title: '已填入 Funder',
        description: wallets.length > 1 ? `共 ${wallets.length} 个关联钱包，已填入第一个：${wallets[0]}` : wallets[0],
        variant: 'success',
      })
    } catch (caught) {
      toast({ title: '查询失败', description: caught instanceof Error ? caught.message : '请稍后重试。', variant: 'error' })
    } finally {
      setDiscoveringFunder(false)
    }
  }

  const testRelayer = async () => {
    setRelayerTesting(true)
    setRelayerProbe(null)
    try {
      const result = await api.testRelayer(draft, clobBaseURL, relayerKey.trim(), relayerAddress.trim())
      setRelayerProbe(result)
      toast({
        title: result.ok ? 'Relayer 密钥有效' : 'Relayer 密钥检测失败',
        description: result.ok
          ? `签名者 ${result.address} · 该地址下共 ${result.key_count ?? 0} 把密钥`
          : result.error || '请检查密钥、签名者地址与网络出口。',
        variant: result.ok ? 'success' : 'error',
      })
    } catch (caught) {
      toast({ title: '检测失败', description: caught instanceof Error ? caught.message : '请稍后重试。', variant: 'error' })
    } finally {
      setRelayerTesting(false)
    }
  }

  const testCredentials = async () => {
    const validation = liveValidation(draft, environment)
    if (validation) {
      toast({ title: '无法检测', description: validation, variant: 'error' })
      return
    }
    setTesting(true)
    setProbe(null)
    try {
      const result = await api.testLive(draft, clobBaseURL)
      setProbe(result)
      toast({
        title: result.ok ? '实盘凭据可用' : '实盘凭据检测失败',
        description: result.ok
          ? `Signer ${result.signer} · 抵押余额 ${formatCurrency(result.cash ?? 0)}`
          : result.error || '请检查私钥、Funder、签名类型与网络出口。',
        variant: result.ok ? 'success' : 'error',
      })
    } catch (caught) {
      toast({ title: '检测失败', description: caught instanceof Error ? caught.message : '请稍后重试。', variant: 'error' })
    } finally {
      setTesting(false)
    }
  }

  return (
    <Section
      icon={Wallet}
      title="实盘交易凭据"
      description="可直接在网页中填写钱包私钥、Funder 与 L2 API 凭据。留空表示沿用启动环境变量；私钥与 Secret 在数据库中加密保存，读取时只返回掩码。"
      actions={<Badge variant={statusVariant}>{statusLabel}</Badge>}
    >
      <div className="rounded-xl border border-slate-200 bg-slate-50/60 p-4">
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-sm font-semibold text-slate-800">运行状态</span>
          <Badge variant={statusVariant}>{statusLabel}</Badge>
          {runtime.live_configured && <span className="text-xs text-slate-500">Signer / Maker 可在「参数设置」的账户卡片中查看。</span>}
        </div>
        {runtime.live_error && <p className="mt-2 break-words text-xs leading-5 text-rose-600">{runtime.live_error}</p>}
        <div className="mt-3 flex flex-wrap gap-2">
          {environment.private_key_set && <Badge variant="info">环境变量已提供私钥</Badge>}
          {environment.funder && <Badge variant="info">环境 Funder {environment.funder.slice(0, 6)}…{environment.funder.slice(-4)}</Badge>}
          {environmentCredentialsSet && <Badge variant="info">环境变量已提供 L2 凭据</Badge>}
          {environmentCredentialsPartial && <Badge variant="warning">环境变量 L2 凭据不完整</Badge>}
          {!environment.private_key_set && !environment.funder && !environment.api_key_set && !environment.api_secret_set && !environment.api_passphrase_set && <Badge variant="default">未检测到实盘环境变量</Badge>}
        </div>
      </div>

      <div className="mt-5">
        <p className="text-sm font-medium text-slate-700">钱包模式</p>
        <p className="mt-1 text-xs leading-4 text-slate-400">决定资金在哪里、以及 CLOB 如何校验签名。两种模式都需要私钥，仅用 Relayer API Key 无法下单。</p>
        <div className="mt-2 grid gap-3 sm:grid-cols-2">
          <button
            type="button"
            aria-pressed={mode === 'eoa'}
            onClick={() => onChange({ signature_type: 0, funder: '' })}
            className={cn(
              'rounded-xl border p-4 text-left transition',
              mode === 'eoa' ? 'border-emerald-300 bg-emerald-50/70 ring-1 ring-emerald-200' : 'border-slate-200 bg-white hover:border-slate-300',
            )}
          >
            <span className="flex items-center gap-2 text-sm font-semibold text-slate-800">
              <Wallet className="h-4 w-4 text-slate-500" />自托管 EOA
              {mode === 'eoa' && <CheckCircle2 className="ml-auto h-4 w-4 text-emerald-500" />}
            </span>
            <span className="mt-1 block text-xs leading-5 text-slate-500">私钥地址就是资金地址（Signer = Maker），不需要 Funder。链上赎回需自付 Polygon gas。</span>
          </button>
          <button
            type="button"
            aria-pressed={mode === 'proxy'}
            onClick={() => onChange({ signature_type: proxyType })}
            className={cn(
              'rounded-xl border p-4 text-left transition',
              mode === 'proxy' ? 'border-emerald-300 bg-emerald-50/70 ring-1 ring-emerald-200' : 'border-slate-200 bg-white hover:border-slate-300',
            )}
          >
            <span className="flex items-center gap-2 text-sm font-semibold text-slate-800">
              <ShieldCheck className="h-4 w-4 text-slate-500" />Polymarket 代理钱包
              {mode === 'proxy' && <CheckCircle2 className="ml-auto h-4 w-4 text-emerald-500" />}
            </span>
            <span className="mt-1 block text-xs leading-5 text-slate-500">私钥只负责签名，USDC 与持仓在 Funder（Proxy / Safe / Deposit）地址。下单仍由私钥签名后提交 CLOB。</span>
          </button>
        </div>
        <p className="mt-2 text-xs leading-5 text-slate-400">邮箱 / Magic 登录的账号私钥由 Polymarket 托管、无法导出，不能直接用于本系统实盘；请改用可导出私钥的钱包（步骤见 README「邮箱登录账号如何实盘」）。</p>
      </div>

      <div className="mt-5 grid gap-x-5 gap-y-5 lg:grid-cols-3">
        <div className={mode === 'proxy' ? 'lg:col-span-2' : 'lg:col-span-3'}>
          <label htmlFor="live-private-key" className="text-sm font-medium text-slate-700">钱包私钥</label>
          <p className="mt-1 min-h-8 text-xs leading-4 text-slate-400">
            {mode === 'eoa' ? '等同于钱包资产控制权。建议使用专用小额热钱包。' : '只用于签名，不直接持有资金；请使用控制该代理钱包的 EOA 私钥。'}
            留空则沿用环境变量或保持未配置。
          </p>
          <div className="mt-2 flex gap-2">
            <Input
              id="live-private-key"
              type="password"
              value={draft.private_key}
              onChange={(event) => onChange({ private_key: event.target.value })}
              onFocus={(event) => { if (event.currentTarget.value === REDACTED_SECRET) event.currentTarget.select() }}
              placeholder={environment.private_key_set ? '环境变量已提供，留空沿用' : '0x...'}
              spellCheck={false}
              autoComplete="off"
              className="font-mono text-xs"
            />
            {draft.private_key && (
              <Button variant="secondary" size="sm" onClick={() => onChange({ private_key: '' })}>清除</Button>
            )}
          </div>
          {draft.private_key === REDACTED_SECRET && <p className="mt-1.5 text-xs text-slate-400">已保存的私钥不会回显。直接输入新值会覆盖，点击「清除」可恢复环境变量。</p>}
        </div>

        {mode === 'proxy' && (
          <div>
            <label htmlFor="live-signature-type" className="text-sm font-medium text-slate-700">代理钱包类型</label>
            <p className="mt-1 min-h-8 text-xs leading-4 text-slate-400">1=旧版 Magic/Google Proxy，2=旧版 Safe（MetaMask 等），3=Deposit Wallet（2026-05 之后新注册账号默认）。环境变量默认 {environment.signature_type}。</p>
            <Select value={String(proxyType)} onValueChange={(value) => onChange({ signature_type: Number(value) })}>
              <SelectTrigger id="live-signature-type" className="mt-2">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="1">1 · Proxy（旧版 Magic / Google）</SelectItem>
                <SelectItem value="2">2 · Safe（旧版外部签名钱包）</SelectItem>
                <SelectItem value="3">3 · Deposit Wallet（当前默认）</SelectItem>
              </SelectContent>
            </Select>
          </div>
        )}

        {mode === 'proxy' ? (
          <div className="lg:col-span-3">
            <label htmlFor="live-funder" className="text-sm font-medium text-slate-700">Funder（代理 / Deposit 钱包地址）</label>
            <p className="mt-1 text-xs leading-4 text-slate-400">Polymarket 账户里显示的钱包地址，也是 USDC 与持仓所在地址。必须填写，且不能等于上面的私钥地址。</p>
            <div className="mt-2 flex gap-2">
              <Input
                id="live-funder"
                value={draft.funder}
                onChange={(event) => onChange({ funder: event.target.value })}
                placeholder={environment.funder || '0x...'}
                spellCheck={false}
                autoComplete="off"
                className="font-mono text-xs"
              />
              <Button variant="secondary" size="sm" onClick={() => void discoverFunder()} loading={discoveringFunder} disabled={busy}>自动获取</Button>
            </div>
            <p className="mt-1.5 text-xs leading-4 text-slate-400">「自动获取」向 CLOB 查询该私钥关联的 Proxy / Deposit 钱包，不会保存也不会下单。旧版 Proxy 钱包通常能查到；Deposit 钱包若查不到，请从 Polymarket 个人菜单复制。</p>
          </div>
        ) : (
          <div className="lg:col-span-3 rounded-xl border border-slate-200 bg-slate-50/60 p-3 text-xs leading-5 text-slate-500">
            自托管 EOA：资金、持仓与签名都在同一个地址上，Funder 必须留空。赎回需要自付 Polygon gas，本系统不代赎回。
          </div>
        )}
      </div>

      <div className="mt-6 border-t border-slate-100 pt-5">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div>
            <p className="text-sm font-semibold text-slate-800">CLOB L2 API 凭据（通常留空）</p>
            <p className="mt-1 text-xs leading-5 text-slate-400">三项必须同时填写或同时留空。留空时系统会用私钥自动派生。注意：polymarket.com「Settings → API Keys」里生成的是 <span className="font-medium text-slate-600">Relayer API Key</span>（一串 UUID + 地址），只用于免 gas 的中继交易，<span className="font-medium text-slate-600">不是</span>这里的 CLOB 凭据，请勿填入。</p>
          </div>
          <Button variant="ghost" size="sm" onClick={clearCredentials} disabled={!draft.api_key && !draft.api_secret && !draft.api_passphrase}>清除 L2 凭据</Button>
        </div>
        <div className="mt-4 grid gap-x-5 gap-y-4 lg:grid-cols-3">
          <div>
            <label htmlFor="live-api-key" className="text-sm font-medium text-slate-700">API Key</label>
            <Input
              id="live-api-key"
              value={draft.api_key}
              onChange={(event) => onChange({ api_key: event.target.value })}
              placeholder={environment.api_key_set ? '环境变量已提供' : '通常留空（自动派生）'}
              spellCheck={false}
              autoComplete="off"
              className="mt-2 font-mono text-xs"
            />
          </div>
          <div>
            <label htmlFor="live-api-secret" className="text-sm font-medium text-slate-700">API Secret</label>
            <Input
              id="live-api-secret"
              type="password"
              value={draft.api_secret}
              onChange={(event) => onChange({ api_secret: event.target.value })}
              onFocus={(event) => { if (event.currentTarget.value === REDACTED_SECRET) event.currentTarget.select() }}
              placeholder={environment.api_secret_set ? '环境变量已提供' : '通常留空（自动派生）'}
              spellCheck={false}
              autoComplete="off"
              className="mt-2 font-mono text-xs"
            />
          </div>
          <div>
            <label htmlFor="live-api-passphrase" className="text-sm font-medium text-slate-700">API Passphrase</label>
            <Input
              id="live-api-passphrase"
              type="password"
              value={draft.api_passphrase}
              onChange={(event) => onChange({ api_passphrase: event.target.value })}
              onFocus={(event) => { if (event.currentTarget.value === REDACTED_SECRET) event.currentTarget.select() }}
              placeholder={environment.api_passphrase_set ? '环境变量已提供' : '通常留空（自动派生）'}
              spellCheck={false}
              autoComplete="off"
              className="mt-2 font-mono text-xs"
            />
          </div>
        </div>
        <p className="mt-3 text-xs leading-5 text-slate-400">CLOB 的 Key / Secret / Passphrase 不会在 Polymarket 网页上展示，只能由私钥派生。Relayer API Key 的用途是经 relayer 免 gas 提交链上交易（赎回、转账等，仍需私钥签名），本系统当前不代赎回，因此无需填写；它的地址可用来核对下方检测结果里的 Signer。</p>
      </div>

      {mode === 'proxy' && (
        <div className="mt-6 border-t border-slate-100 pt-5">
          <div>
            <p className="text-sm font-semibold text-slate-800">Relayer API Key（可选，仅链上免 gas 操作）</p>
            <p className="mt-1 text-xs leading-5 text-slate-400">
              polymarket.com「Settings → API Keys」生成的 UUID 与签名者地址。它只负责鉴权：官方 <code className="rounded bg-slate-100 px-1">/submit</code> 请求体仍必须带 <code className="rounded bg-slate-100 px-1">signature</code> / <code className="rounded bg-slate-100 px-1">nonce</code>（由私钥签名），因此不能下单。本系统当前不代赎回，这里只做验证，不会保存。
            </p>
          </div>
          <div className="mt-3 grid gap-3 lg:grid-cols-2">
            <Input
              value={relayerKey}
              onChange={(event) => { setRelayerKey(event.target.value); setRelayerProbe(null) }}
              placeholder="01a18d94-7b45-7bc6-..."
              spellCheck={false}
              autoComplete="off"
              aria-label="Relayer API Key"
              className="font-mono text-xs"
            />
            <Input
              value={relayerAddress}
              onChange={(event) => { setRelayerAddress(event.target.value); setRelayerProbe(null) }}
              placeholder={probe?.signer || '0x...（签名者地址，不是 Funder）'}
              spellCheck={false}
              autoComplete="off"
              aria-label="Relayer 签名者地址"
              className="font-mono text-xs"
            />
          </div>
          <div className="mt-3 flex flex-wrap items-center gap-3">
            <Button variant="secondary" size="sm" onClick={() => void testRelayer()} loading={relayerTesting} disabled={busy || !relayerKey.trim() || !relayerAddress.trim()}>验证 Relayer 密钥</Button>
            <p className="text-xs leading-5 text-slate-400">地址必须等于私钥地址（Signer）；填成 Funder 会返回 401。</p>
          </div>
          {relayerProbe && (
            <div className={cn('mt-3 rounded-xl border px-4 py-3 text-xs leading-5', relayerProbe.ok ? 'border-emerald-200 bg-emerald-50 text-emerald-800' : 'border-rose-200 bg-rose-50 text-rose-700')} role="status">
              {relayerProbe.ok ? (
                <p className="break-words">Relayer 密钥有效 · 签名者 {relayerProbe.address} · 该地址下 {relayerProbe.key_count ?? 0} 把密钥 · Key {relayerProbe.api_key} · 耗时 {Math.round(relayerProbe.latency_ms ?? 0)} ms</p>
              ) : (
                <p className="break-words">{relayerProbe.error || '检测失败。'}</p>
              )}
            </div>
          )}
        </div>
      )}

      {probe && (
        <div className={cn('mt-4 rounded-xl border px-4 py-3 text-xs leading-5', probe.ok ? 'border-emerald-200 bg-emerald-50 text-emerald-800' : 'border-rose-200 bg-rose-50 text-rose-700')} role="status">
          {probe.ok ? (
            <div className="space-y-1">
              <p className="font-medium">凭据检测通过 · {probe.mode === 'eoa' ? '自托管 EOA（Signer = Maker）' : `代理钱包（类型 ${probe.signature_type}）`} · {probe.source === 'configured' ? '使用已填写的 L2 凭据' : '已由私钥派生 L2 凭据'} · 耗时 {Math.round(probe.latency_ms ?? 0)} ms</p>
              <p className="break-all font-mono">Signer {probe.signer}</p>
              <p className="break-all font-mono">Maker {probe.maker}</p>
              {probe.mode === 'proxy' && <p>代理钱包模式下 Maker 应等于你填写的 Funder；若不等，说明代理钱包类型或 Funder 填错。</p>}
              <p>API Key {probe.api_key || '—'} · 抵押余额 {formatCurrency(probe.cash ?? 0)} · 授权额度 {formatCurrency(probe.allowance ?? 0)}</p>
              {(probe.cash ?? 0) <= 0 && <p className="text-amber-700">抵押余额为 0：请确认 Funder 是该账户真实钱包地址，或该钱包尚未存入 USDC（余额不足时下单会被拒绝）。</p>}
            </div>
          ) : (
            <p className="break-words">{probe.error || '检测失败，请检查私钥、Funder、签名类型与网络出口。'}</p>
          )}
        </div>
      )}

      <div className="mt-5 flex items-start gap-2 rounded-xl border border-amber-200 bg-amber-50 p-3 text-xs leading-5 text-amber-900">
        <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0" />
        <span>私钥会以加密形式写入本机 SQLite。请同时保管数据库旁的 <code className="rounded bg-amber-100 px-1">&lt;数据库文件&gt;.key</code> 密钥文件；丢失密钥将无法解密已保存的凭据。通过远程网络访问时请务必使用 HTTPS/TLS 反向代理，避免凭据在传输过程中被截获。不要将数据库或密钥文件提交到 Git。</span>
      </div>

      <div className="mt-5 flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-end">
        <p className="flex-1 text-xs leading-5 text-slate-400">「检测凭据」只做只读验证（派生 L2 凭据 + 读取余额与授权），不会保存设置也不会下单。</p>
        <Button variant="secondary" onClick={() => void testCredentials()} loading={testing} disabled={busy}>检测凭据</Button>
        <Button variant="secondary" onClick={onReset} disabled={!dirty || busy}><RotateCcw className="h-4 w-4" />放弃更改</Button>
        <Button variant="primary" onClick={onSave} loading={saving} disabled={!dirty || busy}><Save className="h-4 w-4" />保存实盘设置</Button>
      </div>
    </Section>
  )
}

export function SystemView() {
  const { toast } = useToast()
  const [system, setSystem] = useState<SystemSettings | null>(null)
  const [proxyDraft, setProxyDraft] = useState<ProxyConfig | null>(null)
  const [proxyDirty, setProxyDirty] = useState(false)
  const [dataDraft, setDataDraft] = useState<DataSourceSettings | null>(null)
  const [dataDirty, setDataDirty] = useState(false)
  const [liveDraft, setLiveDraft] = useState<LiveSettings | null>(null)
  const [liveDirty, setLiveDirty] = useState(false)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [savingSection, setSavingSection] = useState<'proxy' | 'data' | 'live' | null>(null)
  const [testing, setTesting] = useState(false)
  const [testResult, setTestResult] = useState<ProxyTestResult | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const next = await api.system()
      setSystem(next)
      setProxyDraft((current) => (current && proxyDirty ? current : next.proxy))
      setDataDraft((current) => (current && dataDirty ? current : next.data_source))
      setLiveDraft((current) => (current && liveDirty ? current : next.live))
      setError('')
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : '系统设置加载失败。')
    } finally {
      setLoading(false)
    }
  }, [proxyDirty, dataDirty, liveDirty])

  useEffect(() => {
    void load()
    // Load once; later refreshes are explicit so unsaved edits are not lost.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const applySaved = (saved: SystemSettings, section: 'proxy' | 'data' | 'live') => {
    setSystem(saved)
    if (section === 'proxy' || !proxyDirty) setProxyDraft(saved.proxy)
    if (section === 'data' || !dataDirty) setDataDraft(saved.data_source)
    if (section === 'live' || !liveDirty) setLiveDraft(saved.live)
    if (section === 'proxy') setProxyDirty(false)
    if (section === 'data') setDataDirty(false)
    if (section === 'live') setLiveDirty(false)
  }

  const saveSection = async (
    section: 'proxy' | 'data' | 'live',
    body: { proxy?: ProxyConfig; data_source?: DataSourceSettings; live?: LiveSettings },
    successTitle: string,
    successDescription: string,
  ) => {
    setSavingSection(section)
    try {
      const saved = await api.updateSystem(body)
      applySaved(saved, section)
      toast({ title: successTitle, description: successDescription, variant: 'success' })
    } catch (caught) {
      toast({
        title: '设置保存失败',
        description: caught instanceof Error ? caught.message : '请稍后重试。',
        variant: 'error',
      })
    } finally {
      setSavingSection(null)
    }
  }

  const updateProxy = (patch: Partial<ProxyConfig>) => {
    setProxyDraft((current) => (current ? { ...current, ...patch } : current))
    setProxyDirty(true)
    setTestResult(null)
  }

  const saveProxy = () => {
    if (!proxyDraft) return
    void saveSection('proxy', { proxy: proxyDraft }, '代理设置已保存', '新的网络出口已立即应用到行情与实盘请求。')
  }

  const resetProxy = () => {
    if (!system) return
    setProxyDraft(system.proxy)
    setProxyDirty(false)
    setTestResult(null)
  }

  const updateData = (patch: Partial<DataSourceSettings>) => {
    setDataDraft((current) => (current ? { ...current, ...patch } : current))
    setDataDirty(true)
  }

  const saveData = () => {
    if (!dataDraft) return
    const gammaError = urlError(dataDraft.gamma_base_url)
    if (gammaError) {
      toast({ title: '接口设置未保存', description: `Gamma 行情地址：${gammaError}`, variant: 'error' })
      return
    }
    const clobError = urlError(dataDraft.clob_base_url)
    if (clobError) {
      toast({ title: '接口设置未保存', description: `CLOB 交易地址：${clobError}`, variant: 'error' })
      return
    }
    void saveSection('data', { data_source: dataDraft }, '接口设置已保存', '新的 Gamma / CLOB 地址已立即生效。')
  }

  const resetData = () => {
    if (!system) return
    setDataDraft(system.data_source)
    setDataDirty(false)
  }

  const clearData = () => {
    setDataDraft({ gamma_base_url: '', clob_base_url: '' })
    setDataDirty(true)
  }

  const updateLive = (patch: Partial<LiveSettings>) => {
    setLiveDraft((current) => (current ? { ...current, ...patch } : current))
    setLiveDirty(true)
  }

  const saveLive = () => {
    if (!liveDraft || !system) return
    const validation = liveValidation(liveDraft, system.runtime.live_environment)
    if (validation) {
      toast({ title: '实盘设置未保存', description: validation, variant: 'error' })
      return
    }
    void saveSection('live', { live: liveDraft }, '实盘设置已保存', 'CLOB 客户端已按新凭据重建。若状态未就绪，请查看上方错误提示。')
  }

  const resetLive = () => {
    if (!system) return
    setLiveDraft(system.live)
    setLiveDirty(false)
  }

  const testProxy = async () => {
    if (!proxyDraft) return
    setTesting(true)
    setTestResult(null)
    try {
      const result = await api.testProxy(proxyDraft)
      setTestResult(result)
      toast({
        title: result.ok ? '代理连接正常' : '代理连接失败',
        description: result.ok ? `耗时 ${result.latency_ms.toFixed(0)} ms · ${result.target}` : result.error || '请检查代理地址与上游网络。',
        variant: result.ok ? 'success' : 'error',
      })
    } catch (caught) {
      toast({ title: '代理测试失败', description: caught instanceof Error ? caught.message : '请稍后重试。', variant: 'error' })
    } finally {
      setTesting(false)
    }
  }

  const custom = proxyDraft?.mode === 'custom'

  return (
    <>
      <PageHeader
        eyebrow="SYSTEM & SECURITY"
        title="系统设置"
        description="集中配置 Polymarket 接口地址、实盘交易凭据、网络出口代理与网页访问密码。大多数设置保存在本机 SQLite，修改后立即生效，无需重启服务。"
        actions={<Button variant="secondary" size="sm" onClick={() => void load()} disabled={loading}><RotateCcw className="h-3.5 w-3.5" />重新读取</Button>}
      />

      {loading && !system ? (
        <Card><div className="grid gap-5 p-6 sm:grid-cols-2"><Skeleton className="h-28" /><Skeleton className="h-28" /><Skeleton className="h-28" /><Skeleton className="h-28" /></div></Card>
      ) : error && !system ? (
        <Card><ErrorState message={error} onRetry={() => void load()} /></Card>
      ) : (
        <div className="space-y-4">
          {error && <div className="rounded-xl border border-amber-200 bg-amber-50 px-4 py-3 text-xs text-amber-800">重新读取失败，当前显示的是上一次数据：{error}</div>}
          {system?.runtime.error && <div className="flex items-start gap-2 rounded-xl border border-rose-200 bg-rose-50 px-4 py-3 text-xs leading-5 text-rose-700"><AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0" /><span>启动时应用已保存的系统设置失败：{system.runtime.error}。请检查接口、代理或实盘凭据后重新保存。</span></div>}

          {system && dataDraft && (
            <EndpointSection
              system={system}
              draft={dataDraft}
              dirty={dataDirty}
              saving={savingSection === 'data'}
              busy={savingSection !== null}
              onChange={updateData}
              onDiscard={resetData}
              onClear={clearData}
              onSave={saveData}
            />
          )}

          {system && liveDraft && (
            <LiveTradingSection
              system={system}
              draft={liveDraft}
              dirty={liveDirty}
              saving={savingSection === 'live'}
              busy={savingSection !== null}
              clobBaseURL={dataDraft?.clob_base_url ?? ''}
              onChange={updateLive}
              onReset={resetLive}
              onSave={saveLive}
            />
          )}

          <Section
            icon={Globe2}
            title="网络出口代理"
            description="行情（Gamma）与实盘（CLOB）请求共用同一出口。修改后新的请求立即使用新代理。"
            actions={<Badge variant={custom ? 'info' : proxyDraft?.mode === 'off' ? 'warning' : 'default'}>{proxyDraft ? modeLabels[proxyDraft.mode] : '—'}</Badge>}
          >
            {proxyDraft && (
              <div className="space-y-5">
                <div className="grid gap-5 lg:grid-cols-2">
                  <div>
                    <label htmlFor="proxy-mode" className="text-sm font-medium text-slate-700">出口模式</label>
                    <p className="mt-1 text-xs leading-5 text-slate-400">
                      「跟随环境变量」读取进程的 HTTP_PROXY / HTTPS_PROXY；「自定义代理」使用下方地址；「直连」完全忽略环境变量。
                    </p>
                    <Select value={proxyDraft.mode} onValueChange={(value) => updateProxy({ mode: value as ProxyMode })}>
                      <SelectTrigger id="proxy-mode" className="mt-2">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="environment">跟随环境变量</SelectItem>
                        <SelectItem value="custom">自定义代理</SelectItem>
                        <SelectItem value="off">直连（忽略环境变量）</SelectItem>
                      </SelectContent>
                    </Select>
                  </div>
                  <div>
                    <label htmlFor="proxy-url" className="text-sm font-medium text-slate-700">代理地址</label>
                    <p className="mt-1 text-xs leading-5 text-slate-400">支持 http、https、socks5，需要在自定义模式下填写。密码字段在读取时会被掩码，未修改则原密码保留。</p>
                    <Input
                      id="proxy-url"
                      value={proxyDraft.url}
                      disabled={!custom}
                      onChange={(event) => updateProxy({ url: event.target.value })}
                      placeholder="http://127.0.0.1:7890"
                      spellCheck={false}
                      autoComplete="off"
                      className="mt-2 font-mono text-xs"
                    />
                  </div>
                  <div className="lg:col-span-2">
                    <label htmlFor="proxy-no-proxy" className="text-sm font-medium text-slate-700">不走代理的地址（NO_PROXY）</label>
                    <p className="mt-1 text-xs leading-5 text-slate-400">逗号分隔的主机或域名后缀，例如 <code className="rounded bg-slate-100 px-1">127.0.0.1,localhost,.local</code>。本机回环地址默认直连。</p>
                    <Input
                      id="proxy-no-proxy"
                      value={proxyDraft.no_proxy}
                      disabled={!custom}
                      onChange={(event) => updateProxy({ no_proxy: event.target.value })}
                      placeholder="127.0.0.1,localhost"
                      spellCheck={false}
                      autoComplete="off"
                      className="mt-2 font-mono text-xs"
                    />
                  </div>
                </div>

                {proxyDraft.mode === 'environment' && system && (
                  <div className="rounded-xl border border-slate-200 bg-slate-50 p-4 text-xs leading-5 text-slate-600">
                    <p className="font-medium text-slate-700">当前进程环境变量</p>
                    <div className="mt-2 flex flex-wrap gap-2">
                      <Badge variant={system.environment.http_proxy_set ? 'success' : 'default'}>HTTP_PROXY {system.environment.http_proxy_set ? '已设置' : '未设置'}</Badge>
                      <Badge variant={system.environment.https_proxy_set ? 'success' : 'default'}>HTTPS_PROXY {system.environment.https_proxy_set ? '已设置' : '未设置'}</Badge>
                      {system.environment.no_proxy && <Badge variant="info">NO_PROXY {system.environment.no_proxy}</Badge>}
                    </div>
                  </div>
                )}

                <div className={cn('rounded-xl border p-4', testResult ? (testResult.ok ? 'border-emerald-200 bg-emerald-50/60' : 'border-rose-200 bg-rose-50/60') : 'border-slate-200 bg-slate-50/60')}>
                  <div className="flex flex-wrap items-center justify-between gap-3">
                    <div className="flex items-start gap-2">
                      {testResult ? (
                        testResult.ok ? <CheckCircle2 className="mt-0.5 h-4 w-4 shrink-0 text-emerald-600" /> : <XCircle className="mt-0.5 h-4 w-4 shrink-0 text-rose-600" />
                      ) : (
                        <Wifi className="mt-0.5 h-4 w-4 shrink-0 text-slate-400" />
                      )}
                      <div className="text-xs leading-5 text-slate-600">
                        <p className="font-medium text-slate-800">
                          {testResult ? (testResult.ok ? `连接正常 · ${testResult.latency_ms.toFixed(0)} ms · HTTP ${testResult.status_code ?? '—'}` : '连接失败') : '连接测试'}
                        </p>
                        <p className="mt-0.5 break-words">
                          {testResult ? (testResult.error || `已通过代理访问 ${testResult.target}`) : `使用当前表单中的配置访问 ${system?.proxy_probe_url || 'Polymarket CLOB'}，不会保存设置。`}
                        </p>
                      </div>
                    </div>
                    <Button variant="secondary" size="sm" onClick={() => void testProxy()} loading={testing} disabled={!proxyDraft}>
                      {testing ? <LoaderCircle className="h-3.5 w-3.5 animate-spin" /> : <Wifi className="h-3.5 w-3.5" />}测试连接
                    </Button>
                  </div>
                </div>

                <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-end">
                  <p className="flex-1 text-xs leading-5 text-slate-400">保存后无需重启；未保存的修改只影响「测试连接」。</p>
                  <Button variant="secondary" onClick={resetProxy} disabled={!proxyDirty || savingSection !== null}><RotateCcw className="h-4 w-4" />放弃更改</Button>
                  <Button variant="primary" onClick={saveProxy} loading={savingSection === 'proxy'} disabled={!proxyDirty || savingSection !== null}><Save className="h-4 w-4" />保存代理设置</Button>
                </div>
              </div>
            )}
          </Section>

          <PasswordSection
            passwordSet={Boolean(system?.password_set)}
            onChanged={() => void load()}
          />

          <div className="flex items-start gap-2 text-xs leading-5 text-slate-400">
            <ShieldCheck className="mt-0.5 h-3.5 w-3.5 shrink-0" />
            <p>访问密码只保护网页与 API；实盘私钥与 L2 Secret 以 AES-GCM 加密保存在本机 SQLite，读取时只返回掩码。监听地址、数据库路径与前端静态目录仍属于启动参数，修改后需要重启服务。</p>
          </div>
        </div>
      )}
    </>
  )
}

function PasswordSection({ passwordSet, onChanged }: { passwordSet: boolean; onChanged: () => void }) {
  const { toast } = useToast()
  const [currentPassword, setCurrentPassword] = useState('')
  const [newPassword, setNewPassword] = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')
  const [saving, setSaving] = useState(false)
  const [disableOpen, setDisableOpen] = useState(false)
  const [disablePassword, setDisablePassword] = useState('')
  const [disabling, setDisabling] = useState(false)
  const [error, setError] = useState('')

  const submit = async () => {
    if (newPassword.length < 8) {
      setError('新密码至少 8 个字符。')
      return
    }
    if (newPassword !== confirmPassword) {
      setError('两次输入的新密码不一致。')
      return
    }
    if (passwordSet && !currentPassword) {
      setError('请输入当前密码。')
      return
    }
    setSaving(true)
    setError('')
    try {
      await api.changePassword({ current_password: currentPassword, new_password: newPassword })
      setCurrentPassword('')
      setNewPassword('')
      setConfirmPassword('')
      toast({ title: passwordSet ? '访问密码已更新' : '访问密码已启用', description: '其他浏览器/设备的登录会话已失效。', variant: 'success' })
      onChanged()
    } catch (caught) {
      const message = caught instanceof Error ? caught.message : '保存失败，请稍后重试。'
      setError(message)
      toast({ title: '密码修改失败', description: message, variant: 'error' })
    } finally {
      setSaving(false)
    }
  }

  const disable = async () => {
    if (!disablePassword) {
      toast({ title: '请输入当前密码', variant: 'error' })
      return
    }
    setDisabling(true)
    try {
      await api.disablePassword(disablePassword)
      setDisableOpen(false)
      setDisablePassword('')
      toast({ title: '已关闭访问密码', description: '现在任何能访问该端口的人都可以直接打开工作台。', variant: 'info' })
      onChanged()
    } catch (caught) {
      toast({ title: '关闭失败', description: caught instanceof Error ? caught.message : '请稍后重试。', variant: 'error' })
    } finally {
      setDisabling(false)
    }
  }

  return (
    <Section
      icon={KeyRound}
      title="网页访问密码"
      description="启用后，打开工作台与调用 API 都需要先登录。密码以 bcrypt 哈希保存在本机数据库。"
      actions={<Badge variant={passwordSet ? 'success' : 'default'}>{passwordSet ? '已启用' : '未启用'}</Badge>}
    >
      <div className="grid gap-x-5 gap-y-4 lg:grid-cols-3">
        <div>
          <label htmlFor="current-password" className="text-sm font-medium text-slate-700">当前密码</label>
          <p className="mt-1 text-xs leading-4 text-slate-400">{passwordSet ? '修改或关闭密码时需要验证。' : '当前未设置密码，可留空。'}</p>
          <Input
            id="current-password"
            type="password"
            autoComplete="current-password"
            value={currentPassword}
            disabled={!passwordSet}
            onChange={(event) => { setCurrentPassword(event.target.value); setError('') }}
            className="mt-2"
          />
        </div>
        <div>
          <label htmlFor="new-password" className="text-sm font-medium text-slate-700">新密码</label>
          <p className="mt-1 text-xs leading-4 text-slate-400">8–72 个字符，建议使用密码管理器生成。</p>
          <Input
            id="new-password"
            type="password"
            autoComplete="new-password"
            value={newPassword}
            onChange={(event) => { setNewPassword(event.target.value); setError('') }}
            className="mt-2"
          />
        </div>
        <div>
          <label htmlFor="confirm-password" className="text-sm font-medium text-slate-700">确认新密码</label>
          <p className="mt-1 text-xs leading-4 text-slate-400">再次输入以确认没有拼写错误。</p>
          <Input
            id="confirm-password"
            type="password"
            autoComplete="new-password"
            value={confirmPassword}
            onChange={(event) => { setConfirmPassword(event.target.value); setError('') }}
            className="mt-2"
          />
        </div>
      </div>

      {error && <p className="mt-3 text-xs leading-5 text-rose-600" role="alert">{error}</p>}

      <div className="mt-5 flex flex-col gap-2 sm:flex-row sm:items-center">
        <Button variant="primary" onClick={() => void submit()} loading={saving} disabled={!newPassword || !confirmPassword}>
          <ShieldCheck className="h-4 w-4" />{passwordSet ? '更新密码' : '启用访问密码'}
        </Button>
        {passwordSet && (
          <>
            <Button variant="ghost" className="text-rose-600 hover:bg-rose-50 hover:text-rose-700" onClick={() => setDisableOpen(true)}>
              <ShieldOff className="h-4 w-4" />关闭密码保护
            </Button>
            <Button variant="ghost" onClick={() => { void api.logout().finally(() => window.location.reload()) }}>
              <LogOut className="h-4 w-4" />退出登录
            </Button>
          </>
        )}
        <p className="flex-1 text-xs leading-5 text-slate-400 sm:text-right">修改成功后，当前浏览器保持登录，其他会话需要重新登录。</p>
      </div>

      {!passwordSet && (
        <div className="mt-4 flex items-start gap-2 rounded-xl border border-amber-200 bg-amber-50 p-3 text-xs leading-5 text-amber-900">
          <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0" />
          <span>后端默认仅监听 <code className="rounded bg-amber-100 px-1">127.0.0.1</code>，如果要通过局域网或公网访问，请务必先启用访问密码并配置 TLS 反向代理。</span>
        </div>
      )}

      <Dialog open={disableOpen} onOpenChange={setDisableOpen}>
        <DialogContent>
          <div className="mb-1 flex items-center gap-2"><Badge variant="warning"><ShieldOff className="h-3 w-3" />安全降级</Badge></div>
          <DialogTitle>关闭访问密码保护？</DialogTitle>
          <DialogDescription className="mt-2">关闭后，任何能访问该端口的客户端都可以直接读取账户、持仓并下真实订单。请确认该实例只暴露在受信任的本机环境。</DialogDescription>
          <div>
            <label htmlFor="disable-password" className="text-sm font-medium text-slate-700">当前密码</label>
            <Input
              id="disable-password"
              type="password"
              autoComplete="current-password"
              value={disablePassword}
              onChange={(event) => setDisablePassword(event.target.value)}
              className="mt-2"
            />
          </div>
          <div className="flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
            <Button variant="secondary" onClick={() => setDisableOpen(false)} disabled={disabling}>取消</Button>
            <Button variant="danger" onClick={() => void disable()} loading={disabling} disabled={!disablePassword}>确认关闭</Button>
          </div>
        </DialogContent>
      </Dialog>
    </Section>
  )
}
