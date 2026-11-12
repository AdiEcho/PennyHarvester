import { useCallback, useEffect, useRef, useState } from 'react'
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
import { useVenues } from '@/hooks/useVenues'
import type {
  DataSourceSettings,
  FunderDiscoveryResult,
  LiveEnvironment,
  LiveProbeResult,
  LiveSettings,
  ProxyConfig,
  ProxyMode,
  ProxyTestResult,
  RelayerProbeResult,
  SystemSettings,
  SystemVenueRuntime,
  Venue,
  VenueApproval,
  VenueLiveSettings,
  VenueSystemPatch,
} from '@/types'
import { fmtMoney, VENUES, venueLabel, venueMeta } from '@/lib/venue'
import { assessDetectedWallet, type WalletVerification } from '@/lib/wallet-verification'
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
import { VenueChip } from '@/components/status-badges'

const modeLabels: Record<ProxyMode, string> = {
  environment: '跟随环境变量',
  custom: '自定义代理',
  off: '直连（忽略环境变量）',
}

const REDACTED_SECRET = '******'
const walletSourceLabels: Record<string, string> = {
  'polymarket-profile': 'Polymarket 公开资料',
  'create2-derivation': '链上 CREATE2 推导',
  'clob-proxy-wallets': 'CLOB 关联钱包接口',
}

// Save keys double as the concurrency control for the per-venue cards: saving
// Predict.fun credentials must not reset the Polymarket draft.
type SaveKey = 'proxy' | `data:${Venue}` | `live:${Venue}`

// One PUT body shape for every card; omitted sections keep their stored value.
interface SystemPatch {
  proxy?: ProxyConfig
  data_source?: DataSourceSettings
  live?: LiveSettings
  venues?: VenueSystemPatch
}

interface PredictFunDraft {
  base_url: string
  // Three-state, mirroring the API: undefined means "inherit the deployment
  // environment" and must not be sent. Collapsing it to false would move a
  // PREDICTFUN_TESTNET=1 deployment onto BNB mainnet on the next save.
  testnet?: boolean
  api_key: string
  private_key: string
  smart_wallet: string
  // Secrets are write-only: GET returns "" plus a configured flag, never a value.
  api_key_configured: boolean
  private_key_configured: boolean
  smart_wallet_configured: boolean
}

function toPredictFunDraft(system: SystemSettings): PredictFunDraft {
  const loaded = system.venues?.predictfun
  return {
    // Keep an inherited endpoint empty. Filling the mainnet default here would
    // turn a testnet deployment into an explicit mainnet override on save.
    base_url: loaded?.data_source?.base_url || '',
    testnet: loaded?.live?.testnet ?? loaded?.data_source?.testnet,
    api_key: '',
    private_key: '',
    smart_wallet: '',
    // Flags live on the venue entry; the live-level keys are tolerated too.
    api_key_configured: Boolean(loaded?.api_key_configured || loaded?.live?.api_key_configured || loaded?.live?.api_key),
    private_key_configured: Boolean(loaded?.private_key_configured || loaded?.live?.private_key_configured || loaded?.live?.private_key),
    smart_wallet_configured: Boolean(loaded?.smart_wallet_configured || loaded?.live?.smart_wallet_configured || loaded?.live?.smart_wallet),
  }
}

// Falls back to the flat runtime fields (which always describe Polymarket) when
// the backend has not shipped runtime.venues[] yet.
function venueRuntime(system: SystemSettings, venue: Venue): SystemVenueRuntime {
  const fromApi = system.runtime.venues?.find((item) => item.venue === venue)
  if (fromApi) return fromApi
  if (venue === 'polymarket') {
    return {
      venue,
      configured: system.runtime.live_configured,
      ready: system.runtime.live_ready,
      signer: '',
      maker: system.runtime.live_environment?.funder || '',
      error: system.runtime.live_error,
      testnet: false,
    }
  }
  return {
    venue,
    configured: false,
    ready: false,
    signer: '',
    maker: '',
    error: '',
    testnet: Boolean(system.venues?.predictfun?.live?.testnet),
  }
}

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

function effectiveFunder(draft: LiveSettings, environment: LiveEnvironment): string {
  return (draft.funder.trim() || environment.funder.trim()).toLowerCase()
}

// Dirty checks compare the editable draft with the applied draft, never with
// stored plaintext secrets (the browser only receives masks or blanks). A
// blank secret that arrived blank therefore stays clean; clearing a mask or
// typing a new value is still an edit.
function liveDraftDirty(draft: LiveSettings, applied: LiveSettings, environment: LiveEnvironment): boolean {
  return effectiveSignatureType(draft, environment) !== effectiveSignatureType(applied, environment)
    || effectiveFunder(draft, environment) !== effectiveFunder(applied, environment)
    || draft.private_key !== applied.private_key
    || draft.api_key !== applied.api_key
    || draft.api_secret !== applied.api_secret
    || draft.api_passphrase !== applied.api_passphrase
}

function predictFunLiveDraftDirty(draft: PredictFunDraft, applied: PredictFunDraft): boolean {
  return draft.testnet !== applied.testnet
    || draft.private_key !== applied.private_key
    || draft.api_key !== applied.api_key
    || draft.smart_wallet !== applied.smart_wallet
}

function endpointDraftDirty(draft: DataSourceSettings, applied: DataSourceSettings): boolean {
  return draft.gamma_base_url !== applied.gamma_base_url
    || draft.clob_base_url !== applied.clob_base_url
}

function proxyDraftDirty(draft: ProxyConfig, applied: ProxyConfig): boolean {
  return draft.mode !== applied.mode
    || draft.url !== applied.url
    || draft.no_proxy !== applied.no_proxy
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
  const defaults = venueMeta('polymarket').defaults
  return (
    <Section
      icon={Database}
      title="Polymarket 行情与交易接口"
      description="覆盖 Polymarket 的公开行情（Gamma）与实盘交易（CLOB）地址。留空表示继续使用启动环境变量或官方默认地址。"
      actions={<div className="flex items-center gap-2"><VenueChip venue="polymarket" full /><Badge variant="success">已生效</Badge></div>}
    >
      <div className="grid gap-5 lg:grid-cols-2">
        <div>
          <label htmlFor="gamma-base-url" className="text-sm font-medium text-slate-700">Gamma 行情地址</label>
          <p className="mt-1 min-h-8 text-xs leading-4 text-slate-400">用于机会雷达与持仓结算。支持自建镜像或本地代理。</p>
          <Input
            id="gamma-base-url"
            value={draft.gamma_base_url}
            onChange={(event) => onChange({ gamma_base_url: event.target.value })}
            placeholder={runtime.gamma_default || defaults.gammaBaseURL}
            spellCheck={false}
            autoComplete="off"
            className="mt-2 font-mono text-xs"
          />
          <p className="mt-1.5 text-[11px] leading-4 text-slate-400">当前生效：<code className="break-all rounded bg-slate-100 px-1">{runtime.gamma_base_url || runtime.gamma_default || defaults.gammaBaseURL}</code></p>
        </div>
        <div>
          <label htmlFor="clob-base-url" className="text-sm font-medium text-slate-700">CLOB 交易地址</label>
          <p className="mt-1 min-h-8 text-xs leading-4 text-slate-400">用于实盘下单、手续费读取、余额查询与订单对账。</p>
          <Input
            id="clob-base-url"
            value={draft.clob_base_url}
            onChange={(event) => onChange({ clob_base_url: event.target.value })}
            placeholder={runtime.clob_default || defaults.clobBaseURL}
            spellCheck={false}
            autoComplete="off"
            className="mt-2 font-mono text-xs"
          />
          <p className="mt-1.5 text-[11px] leading-4 text-slate-400">当前生效：<code className="break-all rounded bg-slate-100 px-1">{runtime.clob_base_url || runtime.clob_default || defaults.clobBaseURL}</code></p>
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

function LiveTradingSection({ system, draft, dirty, saving, busy, clobBaseURL, revisionRef, onDraftEdit, onChange, onReset, onSave, onWalletCheck }: {
  system: SystemSettings
  draft: LiveSettings
  dirty: boolean
  saving: boolean
  busy: boolean
  clobBaseURL: string
  revisionRef: { current: number }
  onDraftEdit: () => void
  onChange: (patch: Partial<LiveSettings>) => void
  onReset: () => void
  onSave: () => void
  onWalletCheck: (check: WalletVerification) => void
}) {
  // A stale request may finish after a newer one started. Revision suppresses
  // its data, but its finally must not clear the newer request's busy state.
  const detectionSeq = useRef(0)
  const relayerSeq = useRef(0)
  const probeSeq = useRef(0)
  const { toast } = useToast()
  const [testing, setTesting] = useState(false)
  const [probe, setProbe] = useState<LiveProbeResult | null>(null)
  const [relayerKey, setRelayerKey] = useState('')
  const [relayerAddress, setRelayerAddress] = useState('')
  const [relayerTesting, setRelayerTesting] = useState(false)
  const [relayerProbe, setRelayerProbe] = useState<RelayerProbeResult | null>(null)
  const [detectingWallet, setDetectingWallet] = useState(false)
  const [detection, setDetection] = useState<FunderDiscoveryResult | null>(null)
  // A manual choice of wallet mode wins over auto-detection until the private
  // key changes; otherwise typing a key would immediately undo the operator's
  // "self-custody EOA" selection.
  const [manualMode, setManualMode] = useState(false)
  const detectedKeyRef = useRef('')
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
  const walletCheck = assessDetectedWallet(
    detection,
    mode,
    effectiveFunder(draft, environment),
    effectiveSignatureType(draft, environment),
  )
  // The parent owns the save action, so it needs the same conservative
  // verification result that this panel displays.
  useEffect(() => {
    onWalletCheck(walletCheck)
  }, [onWalletCheck, walletCheck.canSubmit, walletCheck.label, walletCheck.reason, walletCheck.status])
  const clearCredentials = () => onChange({ api_key: '', api_secret: '', api_passphrase: '' })
  const environmentCredentialsSet = environment.api_key_set && environment.api_secret_set && environment.api_passphrase_set
  const environmentCredentialsPartial = (environment.api_key_set || environment.api_secret_set || environment.api_passphrase_set) && !environmentCredentialsSet

  // detectWallet resolves the Polymarket account wallet (funder) and its type
  // (EOA / Proxy / Safe / Deposit) from the private key. The backend tries the
  // public profile lookup first and falls back to CREATE2 derivation, so the
  // operator never has to know the signature-type number.
  const detectWallet = async (options: { silent?: boolean } = {}) => {
    const key = draft.private_key.trim()
    // An empty field is fine when the startup environment supplied the key: the
    // backend resolves it before querying.
    if (!key && !environment.private_key_set) {
      if (!options.silent) {
        toast({ title: '无法自动识别', description: '请先填写钱包私钥，或先保存一次私钥让系统记住它。', variant: 'error' })
      }
      return
    }
    setDetectingWallet(true)
    const revision = revisionRef.current
    const attempt = ++detectionSeq.current
    try {
      const result = await api.detectWallet(draft, clobBaseURL)
      // A newer edit supersedes this detection; applying its wallet type or
      // funder would overwrite the operator's current draft.
      if (revisionRef.current !== revision) return
      setDetection(result)
      if (!result.ok) {
        if (!options.silent) {
          toast({
            title: '未能自动识别钱包',
            description: result.error || '请检查私钥、网络出口与 Polymarket 接口地址。',
            variant: 'error',
          })
        }
        return
      }
      const nextType = result.wallet_type
      const nextFunder = result.funder ?? ''
      const currentType = draft.signature_type ?? environment.signature_type
      const currentFunder = draft.funder.trim()
      const resultCheck = assessDetectedWallet(result, nextType === 0 ? 'eoa' : 'proxy', nextFunder || currentFunder, nextType ?? currentType)
      // Only touch the form when something actually differs, so an automatic
      // check on page load cannot mark the section dirty. An unresolved wallet
      // type is not replaced with a guessed Deposit (3); the detection remains
      // a candidate until the backend supplies explicit evidence.
      if (nextType != null && (nextType !== currentType || nextFunder.toLowerCase() !== currentFunder.toLowerCase())) {
        onChange({ signature_type: nextType, funder: nextFunder })
      }
      if (!options.silent) {
        toast({
          title: resultCheck.status === 'verified' ? `钱包已验证：${result.wallet_type_name || '代理钱包'}` : resultCheck.status === 'not_required' ? '自托管 EOA 已识别' : '检测到候选钱包（未验证，请核对）',
          description: resultCheck.status === 'verified'
            ? `${nextFunder}${result.deployed === false ? '（尚未在链上部署）' : ''}`
            : resultCheck.status === 'not_required'
              ? '该私钥没有关联的 Polymarket 账户钱包，Funder 留空。'
              : `${nextFunder || '未返回地址'}。${resultCheck.reason}`,
          variant: resultCheck.status === 'verified' || resultCheck.status === 'not_required' ? 'success' : 'info',
        })
      }
    } catch (caught) {
      if (revisionRef.current !== revision) return
      if (!options.silent) {
        toast({ title: '检测失败', description: caught instanceof Error ? caught.message : '请稍后重试。', variant: 'error' })
      }
    } finally {
      if (attempt === detectionSeq.current) setDetectingWallet(false)
    }
  }

  // Auto-detect once per private key: a freshly pasted key immediately fills
  // both the wallet type and the funder. Two other cases are covered as well:
  // the masked placeholder (the backend restores the stored key) and an empty
  // field when the startup environment supplied the key.
  const privateKeyValue = draft.private_key.trim()
  const usingEnvironmentKey = privateKeyValue === '' && environment.private_key_set
  const privateKeyDetectable = usingEnvironmentKey
    || privateKeyValue === REDACTED_SECRET
    || /^0x[0-9a-fA-F]{64}$/.test(privateKeyValue)
  const detectionKey = usingEnvironmentKey ? '__environment__' : privateKeyValue
  const previousKeyRef = useRef(detectionKey)
  // Editing the key re-enables auto-detection even after a manual mode choice,
  // and invalidates any previous signer/ownership evidence immediately. Until
  // the new key is detected and verified, the save action stays blocked.
  useEffect(() => {
    if (previousKeyRef.current === detectionKey) return
    previousKeyRef.current = detectionKey
    setDetection(null)
    setManualMode(false)
  }, [detectionKey])
  useEffect(() => {
    if (!privateKeyDetectable) {
      detectedKeyRef.current = ''
      return
    }
    if (manualMode || detectedKeyRef.current === detectionKey) return
    const timer = window.setTimeout(() => {
      detectedKeyRef.current = detectionKey
      void detectWallet({ silent: true })
    }, 600)
    return () => window.clearTimeout(timer)
    // detectWallet is intentionally excluded: it closes over the same draft.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [detectionKey, privateKeyDetectable, manualMode])

  const testRelayer = async () => {
    setRelayerTesting(true)
    setRelayerProbe(null)
    const revision = revisionRef.current
    const attempt = ++relayerSeq.current
    try {
      const result = await api.testRelayer(draft, clobBaseURL, relayerKey.trim(), relayerAddress.trim(), 'polymarket')
      if (revisionRef.current !== revision) return
      setRelayerProbe(result)
      toast({
        title: result.ok ? 'Relayer 密钥有效' : 'Relayer 密钥检测失败',
        description: result.ok
          ? `签名者 ${result.address} · 该地址下共 ${result.key_count ?? 0} 把密钥`
          : result.error || '请检查密钥、签名者地址与网络出口。',
        variant: result.ok ? 'success' : 'error',
      })
    } catch (caught) {
      if (revisionRef.current !== revision) return
      toast({ title: '检测失败', description: caught instanceof Error ? caught.message : '请稍后重试。', variant: 'error' })
    } finally {
      if (attempt === relayerSeq.current) setRelayerTesting(false)
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
    const revision = revisionRef.current
    const attempt = ++probeSeq.current
    try {
      const result = await api.testLive(draft, clobBaseURL, 'polymarket')
      if (revisionRef.current !== revision) return
      setProbe(result)
      toast({
        title: result.ok ? '实盘凭据可用' : '实盘凭据检测失败',
        description: result.ok
          ? `Signer ${result.signer} · 抵押余额 ${fmtMoney(result.cash ?? 0, 'polymarket')}`
          : result.error || '请检查私钥、Funder、签名类型与网络出口。',
        variant: result.ok ? 'success' : 'error',
      })
    } catch (caught) {
      if (revisionRef.current !== revision) return
      toast({ title: '检测失败', description: caught instanceof Error ? caught.message : '请稍后重试。', variant: 'error' })
    } finally {
      if (attempt === probeSeq.current) setTesting(false)
    }
  }

  return (
    <Section
      icon={Wallet}
      title="Polymarket 实盘交易凭据"
      description="可直接在网页中填写钱包私钥、Funder 与 L2 API 凭据。留空表示沿用启动环境变量；私钥与 Secret 在数据库中加密保存，读取时只返回掩码。"
      actions={<div className="flex items-center gap-2"><VenueChip venue="polymarket" full /><Badge variant={statusVariant}>{statusLabel}</Badge></div>}
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
        <p className="mt-1 text-xs leading-4 text-slate-400">决定资金在哪里、以及 CLOB 如何校验签名。填入私钥后系统会自动识别该钱包属于自托管 EOA 还是哪种 Polymarket 代理钱包，并填好 Funder；你也可以手动选择覆盖识别结果。</p>
        <div className="mt-2 grid gap-3 sm:grid-cols-2">
          <button
            type="button"
            aria-pressed={mode === 'eoa'}
            onClick={() => { setManualMode(true); onChange({ signature_type: 0, funder: '' }) }}
            className={cn(
              'rounded-xl border p-4 text-left transition',
              mode === 'eoa' ? 'border-emerald-300 bg-emerald-50/70 ring-1 ring-emerald-200' : 'border-slate-200 bg-surface hover:border-slate-300',
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
            onClick={() => { setManualMode(true); onChange({ signature_type: proxyType }) }}
            className={cn(
              'rounded-xl border p-4 text-left transition',
              mode === 'proxy' ? 'border-emerald-300 bg-emerald-50/70 ring-1 ring-emerald-200' : 'border-slate-200 bg-surface hover:border-slate-300',
            )}
          >
            <span className="flex items-center gap-2 text-sm font-semibold text-slate-800">
              <ShieldCheck className="h-4 w-4 text-slate-500" />Polymarket 代理钱包
              {mode === 'proxy' && <CheckCircle2 className="ml-auto h-4 w-4 text-emerald-500" />}
            </span>
            <span className="mt-1 block text-xs leading-5 text-slate-500">私钥只负责签名，pUSD 与持仓在 Funder（Proxy / Safe / Deposit）地址。下单仍由私钥签名后提交 CLOB。</span>
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
              className="min-w-0 flex-1 font-mono text-xs"
            />
            <Button variant="secondary" size="sm" onClick={() => void detectWallet()} loading={detectingWallet} disabled={busy}>自动识别</Button>
            {draft.private_key && (
              <Button variant="secondary" size="sm" onClick={() => onChange({ private_key: '' })}>清除</Button>
            )}
          </div>
          <p className="mt-1.5 text-xs leading-4 text-slate-400">填入完整私钥后会自动识别候选钱包类型（EOA / Proxy / Safe / Deposit）并填好 Funder，也可点「自动识别」重试。字节码形态只是候选线索；未验证的候选钱包不能保存为实盘配置。</p>
          {draft.private_key === REDACTED_SECRET && <p className="mt-1.5 text-xs text-slate-400">已保存的私钥不会回显。直接输入新值会覆盖，点击「清除」可恢复环境变量。</p>}
        </div>

        {mode === 'proxy' && (
          <div>
            <label htmlFor="live-signature-type" className="text-sm font-medium text-slate-700">代理钱包类型</label>
            <p className="mt-1 min-h-8 text-xs leading-4 text-slate-400">1=旧版 Magic/Google Proxy，2=旧版 Safe（MetaMask 等），3=Deposit Wallet（2026-05 之后新注册账号默认）。环境变量默认 {environment.signature_type}。</p>
            <Select value={String(proxyType)} onValueChange={(value) => { setManualMode(true); onChange({ signature_type: Number(value) }) }}>
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
            <p className="mt-1 text-xs leading-4 text-slate-400">Polymarket 账户里显示的钱包地址，也是 pUSD 与持仓所在地址。必须填写，且不能等于上面的私钥地址。</p>
            <div className="mt-2 flex gap-2">
              <Input
                id="live-funder"
                value={draft.funder}
                onChange={(event) => { setManualMode(true); onChange({ funder: event.target.value }) }}
                placeholder={environment.funder || '0x...'}
                spellCheck={false}
                autoComplete="off"
                className="font-mono text-xs"
              />
              <Button variant="secondary" size="sm" onClick={() => void detectWallet()} loading={detectingWallet} disabled={busy}>自动检测</Button>
            </div>
            <p className="mt-1.5 text-xs leading-4 text-slate-400">「自动检测」先用 Polymarket 公开资料把私钥地址解析成账户钱包，再读取链上字节码作为候选线索；查不到时用 CREATE2 推导旧版 Proxy / Safe 地址做链上校验。候选是否属于当前私钥、实现合约与 owner 必须由后端显式核对后才能保存；检测只读，不会保存、派生 L2 凭据或下单。链上校验使用 <code className="break-all rounded bg-slate-100 px-1">{environment.polygon_rpc_url || 'https://polygon-bor-rpc.publicnode.com'}</code>（可用环境变量 POLYGON_RPC_URL 覆盖）。</p>
          </div>
        ) : (
          <div className="lg:col-span-3 rounded-xl border border-slate-200 bg-slate-50/60 p-3 text-xs leading-5 text-slate-500">
            自托管 EOA：资金、持仓与签名都在同一个地址上，Funder 必须留空。赎回需要自付 Polygon gas，本系统不代赎回。
          </div>
        )}
      </div>

      {!detection && mode === 'proxy' && (
        <div className="mt-4 rounded-xl border border-amber-200 bg-amber-50 px-4 py-3 text-xs leading-5 text-amber-900" role="status">
          <p className="font-medium">代理钱包未验证，请核对</p>
          <p className="mt-1">请先自动检测钱包。后端必须返回链、已知实现合约、解码后的 owner 与 signer 证据后，才能保存实盘配置。</p>
        </div>
      )}

      {detection && (
        <div className={cn(
          'mt-4 rounded-xl border px-4 py-3 text-xs leading-5',
          detection.ok
            ? walletCheck.status === 'verified' || walletCheck.status === 'not_required'
              ? 'border-emerald-200 bg-emerald-50 text-emerald-800'
              : 'border-amber-200 bg-amber-50 text-amber-900'
            : mode === 'eoa'
              ? 'border-slate-200 bg-slate-50 text-slate-600'
              : 'border-amber-200 bg-amber-50 text-amber-900',
        )} role="status">
          {detection.ok ? (
            <div className="space-y-1">
              <p className="font-medium">
                {walletCheck.status === 'verified'
                  ? `钱包已验证：${detection.wallet_type_name || '代理钱包'}`
                  : walletCheck.status === 'not_required'
                    ? '自托管 EOA：无需验证代理钱包'
                    : `候选钱包（${walletCheck.label}）：${detection.wallet_type_name || '未知类型'} · 来源 ${walletSourceLabels[detection.source ?? ''] || detection.source || '—'}${detection.deployed === false ? ' · 链上尚未部署' : ''}`}
              </p>
              {walletCheck.status !== 'verified' && walletCheck.status !== 'not_required' && <p>{walletCheck.reason}</p>}
              {walletCheck.status === 'verified' && <p>Polygon 137 · implementation 与 owner/signer 已核对。</p>}
              {detection.signer && <p className="break-all font-mono">Signer {detection.signer}</p>}
              {detection.funder
                ? <p className="break-all font-mono">Funder {detection.funder}</p>
                : <p>该私钥没有关联的 Polymarket 账户钱包，按自托管 EOA 处理（Funder 留空）。</p>}
              {(detection.candidates ?? []).length > 1 && (
                <p>共 {(detection.candidates ?? []).length} 个关联钱包；其余：{(detection.candidates ?? []).slice(1).map((candidate) => `${candidate.address.slice(0, 6)}…${candidate.address.slice(-4)}（${candidate.wallet_type_name}）`).join('、')}</p>
              )}
              {(detection.notes ?? []).map((note) => <p key={note} className="text-amber-700">{note}</p>)}
            </div>
          ) : (
            <div className="space-y-1">
              <p className="font-medium">
                {mode === 'eoa' ? '该私钥没有关联的 Polymarket 账户钱包' : '未能自动识别钱包（未验证，请核对）'}
              </p>
              {mode === 'eoa' && <p>自托管 EOA 模式下这属于正常情况：资金与签名都在私钥地址上，Funder 留空即可。</p>}
              <p className="break-words">{detection.error || '请检查私钥、网络出口与 Polymarket 接口地址。'}</p>
              {(detection.notes ?? []).map((note) => <p key={note} className="break-words">{note}</p>)}
            </div>
          )}
        </div>
      )}

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
              onChange={(event) => { onDraftEdit(); setRelayerKey(event.target.value); setRelayerProbe(null) }}
              placeholder="01a18d94-7b45-7bc6-..."
              spellCheck={false}
              autoComplete="off"
              aria-label="Relayer API Key"
              className="font-mono text-xs"
            />
            <Input
              value={relayerAddress}
              onChange={(event) => { onDraftEdit(); setRelayerAddress(event.target.value); setRelayerProbe(null) }}
              placeholder={probe?.signer || '0x...（签名者地址，不是 Funder）'}
              spellCheck={false}
              autoComplete="off"
              aria-label="Relayer 签名者地址"
              className="font-mono text-xs"
            />
          </div>
          <div className="mt-3 flex flex-wrap items-center gap-3">
            <Button variant="secondary" size="sm" onClick={() => void testRelayer()} loading={relayerTesting} disabled={busy || !relayerKey.trim() || !relayerAddress.trim()}>验证 Relayer 密钥</Button>
            <p className="text-xs leading-5 text-slate-400">地址必须等于私钥地址（Signer）；填成 Funder 会返回 401。检测会带上 venue=polymarket。</p>
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
              <p>API Key {probe.api_key || '—'} · 抵押余额 {fmtMoney(probe.cash ?? 0, 'polymarket')} · 授权额度 {fmtMoney(probe.allowance ?? 0, 'polymarket')}</p>
              {(probe.cash ?? 0) <= 0 && <p className="text-amber-700">抵押余额为 0：请确认 Funder 是该账户真实钱包地址，或该钱包尚未存入 pUSD（余额不足时下单会被拒绝）。</p>}
            </div>
          ) : (
            <div className="space-y-1">
              <p className="break-words">{probe.error || '检测失败，请检查私钥、Funder、签名类型与网络出口。'}</p>
              <p className="text-rose-600">若提示签名或钱包不匹配，先点上方「自动识别」让系统重新判断钱包类型与 Funder，再重新检测。</p>
            </div>
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

function PredictFunEndpointSection({ draft, dirty, saving, busy, runtime, onChange, onDiscard, onSave }: {
  draft: PredictFunDraft
  dirty: boolean
  saving: boolean
  busy: boolean
  runtime: SystemVenueRuntime
  onChange: (patch: Partial<PredictFunDraft>) => void
  onDiscard: () => void
  onSave: () => void
}) {
  const meta = venueMeta('predictfun')
  return (
    <Section
      icon={Database}
      title="Predict.fun 行情与交易接口"
      description="Predict.fun 的行情与交易共用同一个 API 主机。留空表示使用官方默认地址；自建代理时填写完整 http(s) 地址。"
      actions={<div className="flex items-center gap-2"><VenueChip venue="predictfun" full />{runtime.testnet && <Badge variant="warning">测试网</Badge>}</div>}
    >
      <div className="grid gap-5 lg:grid-cols-2">
        <div>
          <label htmlFor="predictfun-base-url" className="text-sm font-medium text-slate-700">Base URL</label>
          <p className="mt-1 min-h-8 text-xs leading-4 text-slate-400">用于 {meta.label} 的市场列表、盘口与下单。</p>
          <Input
            id="predictfun-base-url"
            value={draft.base_url}
            onChange={(event) => onChange({ base_url: event.target.value })}
            placeholder={draft.testnet ? '留空使用测试网默认地址' : meta.defaults.baseURL}
            spellCheck={false}
            autoComplete="off"
            className="mt-2 font-mono text-xs"
          />
          <p className="mt-1.5 text-[11px] leading-4 text-slate-400">当前生效：<code className="break-all rounded bg-slate-100 px-1">{draft.base_url || (draft.testnet ? '测试网默认地址（继承部署环境）' : `主网默认 ${meta.defaults.baseURL}`)}</code></p>
        </div>
        <div className="rounded-xl border border-slate-200 bg-slate-50/60 p-4 text-xs leading-5 text-slate-500">
          <p className="font-medium text-slate-700">与 Polymarket 的差异</p>
          <ul className="mt-2 list-disc space-y-1 pl-4">
            <li>计价币为 {meta.currency}（{meta.quoteDecimals} 位精度），不与 USDC 相加或换算。</li>
            <li>订单类型为 {meta.orderTypes.join(' / ')}，没有 FOK/FAK。</li>
            <li>引擎不支持该场所的自动结算：到期持仓保持「持有中」，需自行赎回。</li>
            <li>实盘私钥是独立的 EVM 私钥，不可与 Polymarket 私钥复用。</li>
          </ul>
        </div>
      </div>

      <div className="mt-5 flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-end">
        <p className="flex-1 text-xs leading-5 text-slate-400">保存后新的请求立即使用新地址，无需重启。留空保存会保持已保存的地址；要回到官方默认地址，请直接填入上面的默认值。</p>
        <Button variant="secondary" onClick={onDiscard} disabled={!dirty || busy}><RotateCcw className="h-4 w-4" />放弃更改</Button>
        <Button variant="primary" onClick={onSave} loading={saving} disabled={!dirty || busy}><Save className="h-4 w-4" />保存接口设置</Button>
      </div>
    </Section>
  )
}

function PredictFunLiveSection({ draft, dirty, saving, busy, runtime, baseURL, settlementSupported, configurationDirty, revisionRef, onChange, onReset, onSave }: {
  draft: PredictFunDraft
  dirty: boolean
  saving: boolean
  busy: boolean
  runtime: SystemVenueRuntime
  baseURL: string
  settlementSupported: boolean
  configurationDirty: boolean
  revisionRef: { current: number }
  onChange: (patch: Partial<PredictFunDraft>) => void
  onReset: () => void
  onSave: () => void
}) {
  const probeSeq = useRef(0)
  const { toast } = useToast()
  const meta = venueMeta('predictfun')
  const [testing, setTesting] = useState(false)
  const [probe, setProbe] = useState<LiveProbeResult | null>(null)
  const [approval, setApproval] = useState<VenueApproval | null>(null)
  const [approving, setApproving] = useState(false)
  // Any edit invalidates the previous result.
  useEffect(() => { setProbe(null) }, [draft])

  // Allowances live on chain, not in the saved settings, so they are read on
  // mount. A venue that is not configured has nothing to report and the card
  // stays hidden; the read is skipped entirely in that case. Re-read whenever
  // the applied wallet or network changes, otherwise an allowance from the
  // previous identity would stay on screen.
  useEffect(() => {
    if (!runtime.configured) { setApproval(null); return }
    let alive = true
    api.getVenueApproval('predictfun')
      .then((result) => { if (alive) setApproval(result.approval) })
      .catch(() => { if (alive) setApproval(null) })
    return () => { alive = false }
  }, [runtime.configured, runtime.ready, runtime.signer, runtime.maker, runtime.testnet])

  // Approval acts on the applied runtime, not on the draft. Keep the button
  // disabled until the current draft is saved and the applied config is ready.
  const approvalBlockedReason = approving
    ? '授权提交中'
    : busy
      ? '配置保存中'
      : testing
        ? '凭据检测中'
        : configurationDirty
          ? '先保存配置后再授权'
          : !runtime.configured
            ? '尚未配置实盘凭据'
            : !runtime.ready
              ? '实盘凭据未就绪'
              : ''

  const grantApprovals = async () => {
    if (approvalBlockedReason) return
    setApproving(true)
    try {
      const result = await api.approveVenue('predictfun')
      setApproval(result.approval)
      toast({
        title: result.approval.ready ? '链上授权完成' : '授权已提交',
        description: result.approval.ready
          ? '现在可以在 Predict.fun 实盘下单。'
          : result.approval.missing || '交易已发出，等待确认后重新读取。',
        variant: result.approval.ready ? 'success' : 'info',
      })
    } catch (caught) {
      toast({ title: '链上授权失败', description: caught instanceof Error ? caught.message : '请检查 RPC、私钥与 BNB 余额。', variant: 'error' })
    } finally {
      setApproving(false)
    }
  }
  const statusVariant = runtime.ready ? 'success' : runtime.configured ? 'warning' : 'default'
  const statusLabel = runtime.ready ? '凭据本地校验通过' : runtime.configured ? '已配置但未就绪' : '未配置实盘凭据'
  // Effective network: the explicit choice when the operator made one, otherwise
  // whatever the deployment env resolved to.
  const testnet = draft.testnet ?? runtime.testnet

  const testCredentials = async () => {
    const key = draft.private_key.trim()
    if (key && !/^0x[0-9a-fA-F]{64}$/.test(key)) {
      toast({ title: '无法检测', description: '私钥必须是 0x 开头的 64 位十六进制字符串。', variant: 'error' })
      return
    }
    setTesting(true)
    setProbe(null)
    const revision = revisionRef.current
    const attempt = ++probeSeq.current
    try {
      // Empty secrets mean "keep the stored value", exactly like the PUT body.
      const live: VenueLiveSettings = {
        private_key: draft.private_key,
        api_key: draft.api_key,
        smart_wallet: draft.smart_wallet,
        ...(draft.testnet === undefined ? {} : { testnet: draft.testnet }),
      }
      const result = await api.testLive(live, baseURL, 'predictfun')
      if (revisionRef.current !== revision) return
      setProbe(result)
      toast({
        title: result.ok ? 'Predict.fun 凭据可用' : 'Predict.fun 凭据检测失败',
        description: result.ok
          ? `Signer ${result.signer || '—'} · 余额 ${fmtMoney(result.cash ?? 0, 'predictfun')}`
          : result.error || '请检查私钥、API Key、Smart Wallet 与网络出口。',
        variant: result.ok ? 'success' : 'error',
      })
    } catch (caught) {
      if (revisionRef.current !== revision) return
      toast({ title: '检测失败', description: caught instanceof Error ? caught.message : '请稍后重试。', variant: 'error' })
    } finally {
      if (attempt === probeSeq.current) setTesting(false)
    }
  }

  return (
    <Section
      icon={Wallet}
      title="Predict.fun 实盘交易凭据"
      description={`Predict.fun 使用独立的 EVM 私钥与 x-api-key 鉴权，资金在 Smart Wallet（BNB Chain 上的 ${venueMeta('predictfun').currency}）。密钥字段为只写：读取时不回显，留空表示保持已保存值。`}
      actions={<div className="flex items-center gap-2"><VenueChip venue="predictfun" full /><Badge variant={statusVariant}>{statusLabel}</Badge>{testnet && <Badge variant="warning">测试网</Badge>}</div>}
    >
      <div className="rounded-xl border border-slate-200 bg-slate-50/60 p-4">
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-sm font-semibold text-slate-800">运行状态</span>
          <Badge variant={statusVariant}>{statusLabel}</Badge>
          {runtime.testnet && <Badge variant="warning">测试网 TESTNET</Badge>}
          {!settlementSupported && <Badge variant="default">不自动结算</Badge>}
        </div>
        {runtime.error && <p className="mt-2 break-words text-xs leading-5 text-rose-600">{runtime.error}</p>}
        <dl className="mt-3 grid gap-2 text-xs sm:grid-cols-2">
          <div className="flex justify-between gap-3 sm:block"><dt className="text-slate-500">Signer</dt><dd className="break-all font-mono text-[11px] text-slate-700">{runtime.signer || '—'}</dd></div>
          <div className="flex justify-between gap-3 sm:block"><dt className="text-slate-500">Maker / Smart Wallet</dt><dd className="break-all font-mono text-[11px] text-slate-700">{runtime.maker || '—'}</dd></div>
        </dl>
        {!settlementSupported && <p className="mt-3 text-xs leading-5 text-amber-700">该场所暂不支持引擎结算（supports_settlement: false）：持仓到期后保持「持有中」，不要期待自动结算或自动赎回。</p>}
        {settlementSupported && <p className="mt-3 text-xs leading-5 text-slate-500">引擎会按场所公布的获胜结果自动关账（账本损益立即结算），但链上份额仍需你在 Predict.fun 手动赎回，赎回交易需要自行支付 BNB gas；本版本不代发赎回交易。</p>}
      </div>

      <div className="mt-5 grid gap-x-5 gap-y-5 lg:grid-cols-2">
        <div>
          <label htmlFor="predictfun-api-key" className="text-sm font-medium text-slate-700">API Key</label>
          <p className="mt-1 text-xs leading-4 text-slate-400">随请求以 x-api-key 头发送。{draft.api_key_configured ? '已配置，输入新值会覆盖。' : '尚未配置。'}</p>
          <div className="mt-2 flex items-center gap-2">
            <Input
              id="predictfun-api-key"
              type="password"
              value={draft.api_key}
              onChange={(event) => onChange({ api_key: event.target.value })}
              placeholder={draft.api_key_configured ? '已配置（留空保持不变）' : '尚未配置'}
              spellCheck={false}
              autoComplete="off"
              className="font-mono text-xs"
            />
            {draft.api_key_configured && <Badge variant="success">已配置</Badge>}
          </div>
        </div>
        <div>
          <label htmlFor="predictfun-smart-wallet" className="text-sm font-medium text-slate-700">Smart Wallet</label>
          <p className="mt-1 text-xs leading-4 text-slate-400">持有资金的账户地址；EOA 模式下留空（资金就在私钥地址上）。{draft.smart_wallet_configured ? '已配置。' : ''}</p>
          <div className="mt-2 flex items-center gap-2">
            <Input
              id="predictfun-smart-wallet"
              value={draft.smart_wallet}
              onChange={(event) => onChange({ smart_wallet: event.target.value })}
              placeholder={draft.smart_wallet_configured ? '已配置（留空保持不变）' : runtime.maker || '0x...'}
              spellCheck={false}
              autoComplete="off"
              className="font-mono text-xs"
            />
            {draft.smart_wallet_configured && <Badge variant="success">已配置</Badge>}
          </div>
        </div>
        <div className="lg:col-span-2">
          <label htmlFor="predictfun-private-key" className="text-sm font-medium text-slate-700">钱包私钥（独立 EVM 私钥）</label>
          <p className="mt-1 text-xs leading-4 text-slate-400">不要复用 Polymarket 的私钥。私钥只用于签名，建议使用专用小额热钱包。{draft.private_key_configured ? '已配置，输入新值会覆盖。' : '尚未配置。'}</p>
          <div className="mt-2 flex items-center gap-2">
            <Input
              id="predictfun-private-key"
              type="password"
              value={draft.private_key}
              onChange={(event) => onChange({ private_key: event.target.value })}
              placeholder={draft.private_key_configured ? '已配置（留空保持不变）' : '0x...'}
              spellCheck={false}
              autoComplete="off"
              className="font-mono text-xs"
            />
            {draft.private_key_configured && <Badge variant="success">已配置</Badge>}
          </div>
        </div>
      </div>

      <div className="mt-6 flex flex-col gap-4 rounded-xl border border-slate-200 bg-slate-50 p-4 sm:flex-row sm:items-center sm:justify-between">
        <div className="flex items-start gap-3">
          <div className={cn('rounded-xl p-2.5', testnet ? 'bg-amber-50 text-amber-600' : 'bg-surface text-slate-400')}><Network className="h-5 w-5" /></div>
          <div>
            <p className="flex flex-wrap items-center gap-2 text-sm font-semibold text-slate-800">测试网（Testnet）{testnet && <Badge variant="warning">测试网</Badge>}</p>
            <p className="mt-1 text-xs leading-5 text-slate-500">开启后请求发往 Predict.fun 测试网，余额与持仓都是测试资产；关闭后为 BNB Chain 主网真实资金。切换后请重新检测凭据。</p>
            <p className="mt-1 text-xs leading-5 text-slate-400">{draft.testnet === undefined ? `当前跟随部署环境（${runtime.testnet ? '测试网' : '主网'}）；开关一次即可固定为显式设置。` : '已显式指定网络，保存后对下一次下单生效。'}</p>
          </div>
        </div>
        <div className="flex items-center gap-3 sm:flex-col sm:items-end">
          <Switch checked={testnet} onCheckedChange={(checked) => onChange({ testnet: checked })} aria-label="切换 Predict.fun 测试网" />
          <span className="text-xs text-slate-400">{testnet ? '测试网' : '主网'}</span>
        </div>
      </div>

      {probe && (
        <div className={cn('mt-4 rounded-xl border px-4 py-3 text-xs leading-5', probe.ok ? 'border-emerald-200 bg-emerald-50 text-emerald-800' : 'border-rose-200 bg-rose-50 text-rose-700')} role="status">
          {probe.ok ? (
            <div className="space-y-1">
              <p className="font-medium">凭据检测通过 · {probe.testnet ? '测试网' : '主网'} · 耗时 {Math.round(probe.latency_ms ?? 0)} ms</p>
              <p className="break-all font-mono">Signer {probe.signer || '—'}</p>
              <p className="break-all font-mono">Maker {probe.maker || '—'}</p>
              <p>API Key {probe.api_key || '—'} · 余额 {fmtMoney(probe.cash ?? 0, 'predictfun', { digits: 4 })}</p>
              {(probe.cash ?? 0) <= 0 && <p className="text-amber-700">余额为 0：请确认 Smart Wallet 地址正确，且钱包内有 {meta.currency} 与 BNB 手续费。</p>}
            </div>
          ) : (
            <p className="break-words">{probe.error || '检测失败，请检查私钥、API Key、Smart Wallet 与网络出口。'}</p>
          )}
        </div>
      )}

      <div className="mt-5 flex items-start gap-2 rounded-xl border border-amber-200 bg-amber-50 p-3 text-xs leading-5 text-amber-900">
        <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0" />
        <span>密钥写入本机 SQLite（AES-GCM 加密），读取时只返回「已配置」标记，绝不回显。Predict.fun 的私钥与 Polymarket 私钥互相独立，泄漏任意一个都会导致对应账户资金被盗。</span>
      </div>

      <div className="mt-5 flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-end">
      {approval && (
        <div className={cn('mt-4 rounded-xl border px-4 py-3 text-xs leading-5', approval.ready ? 'border-emerald-200 bg-emerald-50 text-emerald-800' : 'border-amber-200 bg-amber-50 text-amber-900')} role="status">
          <p className="font-medium">
            链上授权 · {approval.ready ? '已就绪' : '未完成'}
          </p>
          <p className="mt-1">
            余额 {fmtMoney(approval.balance, 'predictfun', { digits: 4 })} · 已授权 {fmtMoney(approval.allowance, 'predictfun', { digits: 4 })} · 结果份额划转 {approval.shares_ready ? '已授权' : '未授权'}
          </p>
          {!approval.ready && (
            <p className="mt-1 text-amber-800">
              {approval.missing || '需要授权 USDT 与结果份额后才能实盘交易。'}授权是一个真实的链上交易，钱包需要 BNB 支付 gas。
            </p>
          )}
        </div>
      )}

        <p className="flex-1 text-xs leading-5 text-slate-400">「检测凭据」只做只读验证（读取余额与账户状态），不会保存设置也不会下单；请求会带上 venue=predictfun。</p>
        {approval && !approval.ready && (
          <>
            {approvalBlockedReason && (
              <p className="text-xs leading-5 text-amber-700 sm:text-right" role="status">{approvalBlockedReason}</p>
            )}
            <Button variant="secondary" onClick={() => void grantApprovals()} loading={approving} disabled={Boolean(approvalBlockedReason)}>
              <ShieldCheck className="h-4 w-4" />链上授权
            </Button>
          </>
        )}
        <Button variant="secondary" onClick={() => void testCredentials()} loading={testing} disabled={busy}>检测凭据</Button>
        <Button variant="secondary" onClick={onReset} disabled={!dirty || busy}><RotateCcw className="h-4 w-4" />放弃更改</Button>
        <Button variant="primary" onClick={onSave} loading={saving} disabled={!dirty || busy}><Save className="h-4 w-4" />保存实盘设置</Button>
      </div>
    </Section>
  )
}

function VenueRuntimeCard({ system, capabilityOf }: { system: SystemSettings; capabilityOf: ReturnType<typeof useVenues>['capabilityOf'] }) {
  return (
    <Section icon={Network} title="场所运行状态" description="逐场所读取后端 runtime.venues[]：凭据是否就绪、结算能力与测试网状态。一个场所异常不影响另一个。">
      <div className="grid gap-3 lg:grid-cols-2">
        {VENUES.map((venue) => {
          const runtime = venueRuntime(system, venue)
          const meta = venueMeta(venue)
          const capability = capabilityOf(venue)
          const testnet = runtime.testnet || Boolean(capability?.testnet)
          const settlement = capability?.supports_settlement ?? meta.supportsSettlement
          const readOnly = capability?.read_only ?? false
          return (
            <div key={venue} className="rounded-xl border border-slate-200 p-4">
              <div className="flex flex-wrap items-center gap-2">
                <VenueChip venue={venue} full />
                <Badge variant={runtime.ready ? 'success' : runtime.configured ? 'warning' : 'default'}>
                  {runtime.ready ? '已就绪' : runtime.configured ? '配置待完善' : '未配置'}
                </Badge>
                {testnet && <Badge variant="warning">测试网</Badge>}
                {readOnly && <Badge variant="danger">只读</Badge>}
                {!settlement && <Badge variant="default">不自动结算</Badge>}
                {capability?.order_types?.length ? <Badge variant="info">{capability.order_types.join(' / ')}</Badge> : <Badge variant="info">{meta.orderTypes.join(' / ')}</Badge>}
              </div>
              <dl className="mt-3 space-y-1.5 text-xs">
                <div className="flex justify-between gap-3"><dt className="text-slate-500">收付币种</dt><dd className="font-medium text-slate-700">{meta.currency} · {meta.quoteDecimals} 位精度</dd></div>
                <div className="flex justify-between gap-3"><dt className="text-slate-500">Signer</dt><dd className="max-w-[220px] truncate font-mono text-[11px] text-slate-600" title={runtime.signer}>{runtime.signer || '—'}</dd></div>
                <div className="flex justify-between gap-3"><dt className="text-slate-500">Maker</dt><dd className="max-w-[220px] truncate font-mono text-[11px] text-slate-600" title={runtime.maker}>{runtime.maker || '—'}</dd></div>
                <div className="flex justify-between gap-3"><dt className="text-slate-500">结算能力</dt><dd className="text-slate-700">{settlement ? '引擎可自动结算' : '保持持有中，需自行赎回'}</dd></div>
                {capability && <div className="flex justify-between gap-3"><dt className="text-slate-500">最小下单额</dt><dd className="text-slate-700">{fmtMoney(capability.min_notional, venue)}</dd></div>}
                {capability && capability.min_sell_shares > 0 && <div className="flex justify-between gap-3"><dt className="text-slate-500">最小卖单</dt><dd className="text-slate-700">{capability.min_sell_shares} 份</dd></div>}
              </dl>
              {runtime.error && <p className="mt-2 break-words text-xs leading-5 text-rose-600">{runtime.error}</p>}
            </div>
          )
        })}
      </div>
    </Section>
  )
}

export function SystemView() {
  const { toast } = useToast()
  const [tab, setTab] = useState<Venue>('polymarket')
  const [system, setSystem] = useState<SystemSettings | null>(null)
  const [proxyDraft, setProxyDraft] = useState<ProxyConfig | null>(null)
  const [pmData, setPmData] = useState<DataSourceSettings | null>(null)
  const [pmLive, setPmLive] = useState<LiveSettings | null>(null)
  const [pfDraft, setPfDraft] = useState<PredictFunDraft | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [savingSection, setSavingSection] = useState<SaveKey | null>(null)
  const [liveWalletCheck, setLiveWalletCheck] = useState<WalletVerification | null>(null)
  const [testing, setTesting] = useState(false)
  const [testResult, setTestResult] = useState<ProxyTestResult | null>(null)
  const { capabilityOf } = useVenues()

  // Monotonic draft revision. Every editable-field change increments it, so an
  // async response can tell whether the draft it started from still exists.
  const revisionRef = useRef(0)
  const bumpRevision = useCallback(() => { revisionRef.current += 1 }, [])

  // Dirty state is derived from the draft and the last applied settings, so a
  // save can reset the baseline just by replacing `system`. Never compare
  // secret masks to stored plaintext: the applied draft only has masks/blanks.
  const proxyDirty = Boolean(system && proxyDraft && proxyDraftDirty(proxyDraft, system.proxy))
  const pmDataDirty = Boolean(system && pmData && endpointDraftDirty(pmData, system.data_source))
  const pmLiveDirty = Boolean(system && pmLive && liveDraftDirty(pmLive, system.live, system.runtime.live_environment))
  const pfApplied = system ? toPredictFunDraft(system) : null
  const pfDataDirty = Boolean(pfDraft && pfApplied && pfDraft.base_url !== pfApplied.base_url)
  const pfLiveDirty = Boolean(pfDraft && pfApplied && predictFunLiveDraftDirty(pfDraft, pfApplied))

  const load = useCallback(async () => {
    const revision = revisionRef.current
    setLoading(true)
    try {
      const next = await api.system()
      // A reload that raced an edit must not replace the newer draft.
      if (revisionRef.current !== revision) {
        setError('')
        return
      }
      setSystem(next)
      setProxyDraft((current) => (current && proxyDirty ? current : next.proxy))
      setPmData((current) => (current && pmDataDirty ? current : next.data_source))
      setPmLive((current) => (current && pmLiveDirty ? current : next.live))
      setPfDraft((current) => (current && (pfDataDirty || pfLiveDirty) ? current : toPredictFunDraft(next)))
      setError('')
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : '系统设置加载失败。')
    } finally {
      setLoading(false)
    }
  }, [proxyDirty, pmDataDirty, pmLiveDirty, pfDataDirty, pfLiveDirty])

  useEffect(() => {
    void load()
    // Load once; later refreshes are explicit so unsaved edits are not lost.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // Write only the saved draft; the caller sets `system` (the applied baseline)
  // even when this save became stale, but stale responses must not touch fields
  // edited after the request started. Bumping the revision invalidates probes
  // that were started against the pre-save draft.
  const applySavedDraft = (saved: SystemSettings, key: SaveKey) => {
    bumpRevision()
    if (key === 'proxy') {
      setProxyDraft(saved.proxy)
      return
    }
    if (key === 'data:polymarket') {
      setPmData(saved.data_source)
      return
    }
    if (key === 'live:polymarket') {
      setPmLive(saved.live)
      return
    }
    const savedVenue = saved.venues?.predictfun
    setPfDraft((current) => {
      if (!current) return current
      if (key === 'data:predictfun') {
        return { ...current, base_url: savedVenue?.data_source?.base_url ?? current.base_url }
      }
      // Secrets are write-only: clear the inputs and rely on the configured flags.
      return {
        ...current,
        testnet: savedVenue?.live?.testnet ?? current.testnet,
        api_key: '',
        private_key: '',
        smart_wallet: '',
        api_key_configured: Boolean(savedVenue?.api_key_configured || savedVenue?.live?.api_key_configured) || current.api_key_configured || Boolean(current.api_key),
        private_key_configured: Boolean(savedVenue?.private_key_configured || savedVenue?.live?.private_key_configured) || current.private_key_configured || Boolean(current.private_key),
        smart_wallet_configured: Boolean(savedVenue?.smart_wallet_configured || savedVenue?.live?.smart_wallet_configured) || current.smart_wallet_configured || Boolean(current.smart_wallet),
      }
    })
  }

  const saveSection = async (key: SaveKey, body: SystemPatch, successTitle: string, successDescription: string) => {
    const revision = revisionRef.current
    setSavingSection(key)
    try {
      const saved = await api.updateSystem(body)
      // The server applied the save either way; refresh the runtime baseline.
      setSystem(saved)
      if (revisionRef.current !== revision) return
      applySavedDraft(saved, key)
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
    bumpRevision()
    setProxyDraft((current) => (current ? { ...current, ...patch } : current))
    setTestResult(null)
  }

  const saveProxy = () => {
    if (!proxyDraft) return
    void saveSection('proxy', { proxy: proxyDraft }, '代理设置已保存', '新的网络出口已立即应用到各场所的行情与实盘请求。')
  }

  const resetProxy = () => {
    if (!system) return
    bumpRevision()
    setProxyDraft(system.proxy)
    setTestResult(null)
  }

  const updateData = (patch: Partial<DataSourceSettings>) => {
    bumpRevision()
    setPmData((current) => (current ? { ...current, ...patch } : current))
  }

  const saveData = () => {
    if (!pmData) return
    const gammaError = urlError(pmData.gamma_base_url)
    if (gammaError) {
      toast({ title: '接口设置未保存', description: `Gamma 行情地址：${gammaError}`, variant: 'error' })
      return
    }
    const clobError = urlError(pmData.clob_base_url)
    if (clobError) {
      toast({ title: '接口设置未保存', description: `CLOB 交易地址：${clobError}`, variant: 'error' })
      return
    }
    // Polymarket stays on the flat fields: they are the contract's authoritative
    // view and the backend mirrors them into venues.polymarket. Sending the venue
    // entry too would make an emptied field merge back to its previous value.
    void saveSection('data:polymarket', { data_source: pmData }, '接口设置已保存', '新的 Gamma / CLOB 地址已立即生效。')
  }

  const resetData = () => {
    if (!system) return
    bumpRevision()
    setPmData(system.data_source)
  }

  const clearData = () => {
    bumpRevision()
    setPmData({ gamma_base_url: '', clob_base_url: '' })
  }

  const updateLive = (patch: Partial<LiveSettings>) => {
    bumpRevision()
    setPmLive((current) => (current ? { ...current, ...patch } : current))
  }

  const saveLive = () => {
    if (!pmLive || !system) return
    const validation = liveValidation(pmLive, system.runtime.live_environment)
    if (validation) {
      toast({ title: '实盘设置未保存', description: validation, variant: 'error' })
      return
    }
    // Detection is only a candidate until the backend returns chain,
    // implementation, owner and signer evidence. Saving an unverified proxy
    // wallet would make bytecode shape look like proof of ownership.
    if (walletMode(pmLive, system.runtime.live_environment) === 'proxy' && liveWalletCheck?.status !== 'verified') {
      toast({
        title: '实盘设置未保存：代理钱包未验证，请核对',
        description: liveWalletCheck?.reason || '请先自动检测钱包，并等待后端返回链、implementation、owner 与 signer 证据。',
        variant: 'error',
      })
      return
    }
    // Flat `live` only, for the same reason as the endpoint card: clearing a
    // credential has to clear it, and the backend dual-writes the venue entry.
    void saveSection('live:polymarket', { live: pmLive }, '实盘设置已保存', 'Polymarket CLOB 客户端已按新凭据重建。若状态未就绪，请查看上方错误提示。')
  }

  const resetLive = () => {
    if (!system) return
    bumpRevision()
    setPmLive(system.live)
  }

  const updatePfData = (patch: Partial<PredictFunDraft>) => {
    bumpRevision()
    setPfDraft((current) => (current ? { ...current, ...patch } : current))
  }

  const updatePfLive = (patch: Partial<PredictFunDraft>) => {
    bumpRevision()
    setPfDraft((current) => (current ? { ...current, ...patch } : current))
  }

  const savePfData = () => {
    if (!pfDraft) return
    const baseError = urlError(pfDraft.base_url)
    if (baseError) {
      toast({ title: '接口设置未保存', description: `Base URL：${baseError}`, variant: 'error' })
      return
    }
    void saveSection('data:predictfun', { venues: { predictfun: { data_source: { base_url: pfDraft.base_url } } } }, 'Predict.fun 接口已保存', '新的 API 地址已立即生效。')
  }

  const resetPfData = () => {
    if (!system) return
    bumpRevision()
    const loaded = toPredictFunDraft(system)
    setPfDraft((current) => (current ? { ...current, base_url: loaded.base_url } : current))
  }

  const resetPfLive = () => {
    if (!system) return
    bumpRevision()
    const loaded = toPredictFunDraft(system)
    setPfDraft((current) => (current ? {
      ...current,
      testnet: loaded.testnet,
      api_key: '',
      private_key: '',
      smart_wallet: '',
      api_key_configured: loaded.api_key_configured,
      private_key_configured: loaded.private_key_configured,
      smart_wallet_configured: loaded.smart_wallet_configured,
    } : current))
  }

  const savePfLive = () => {
    if (!pfDraft) return
    const key = pfDraft.private_key.trim()
    if (key && !/^0x[0-9a-fA-F]{64}$/.test(key)) {
      toast({ title: '实盘设置未保存', description: '私钥必须是 0x 开头的 64 位十六进制字符串。', variant: 'error' })
      return
    }
    const wallet = pfDraft.smart_wallet.trim()
    if (wallet && !/^0x[0-9a-fA-F]{40}$/.test(wallet)) {
      toast({ title: '实盘设置未保存', description: 'Smart Wallet 必须是有效的 0x 地址（EOA 模式请留空）。', variant: 'error' })
      return
    }
    // Empty strings keep the stored secret (MergeSecret semantics).
    void saveSection('live:predictfun', {
      venues: {
        predictfun: {
          live: {
            api_key: pfDraft.api_key,
            private_key: pfDraft.private_key,
            smart_wallet: pfDraft.smart_wallet,
            // Omitted when inherited, so saving an unrelated field cannot flip
            // the deployment onto another chain.
            ...(pfDraft.testnet === undefined ? {} : { testnet: pfDraft.testnet }),
          },
        },
      },
    }, '实盘设置已保存', 'Predict.fun 客户端已按新凭据重建。若状态未就绪，请查看运行状态中的错误提示。')
  }

  const testProxy = async () => {
    if (!proxyDraft) return
    setTesting(true)
    setTestResult(null)
    const revision = revisionRef.current
    try {
      const result = await api.testProxy(proxyDraft)
      if (revisionRef.current !== revision) return
      setTestResult(result)
      toast({
        title: result.ok ? '代理连接正常' : '代理连接失败',
        description: result.ok ? `耗时 ${result.latency_ms.toFixed(0)} ms · ${result.target}` : result.error || '请检查代理地址与上游网络。',
        variant: result.ok ? 'success' : 'error',
      })
    } catch (caught) {
      if (revisionRef.current !== revision) return
      toast({ title: '代理测试失败', description: caught instanceof Error ? caught.message : '请稍后重试。', variant: 'error' })
    } finally {
      setTesting(false)
    }
  }

  const custom = proxyDraft?.mode === 'custom'
  const tabs: TabItem<Venue>[] = VENUES.map((venue) => ({
    value: venue,
    label: venueLabel(venue),
    hint: venueMeta(venue).currency,
  }))
  const pfCapability = capabilityOf('predictfun')

  return (
    <>
      <PageHeader
        eyebrow="SYSTEM & SECURITY"
        title="系统设置"
        description="集中配置各场所的接口地址与实盘交易凭据、网络出口代理与网页访问密码。大多数设置保存在本机 SQLite，修改后立即生效，无需重启服务。"
        actions={<Button variant="secondary" size="sm" onClick={() => void load()} disabled={loading}><RotateCcw className="h-3.5 w-3.5" />重新读取</Button>}
      />

      {loading && !system ? (
        <Card><div className="grid gap-5 p-6 sm:grid-cols-2"><Skeleton className="h-28" /><Skeleton className="h-28" /><Skeleton className="h-28" /><Skeleton className="h-28" /></div></Card>
      ) : error && !system ? (
        <Card><ErrorState message={error} onRetry={() => void load()} /></Card>
      ) : (
        <div className="space-y-4">
          {error && <div className="rounded-xl border border-amber-200 bg-amber-50 px-4 py-3 text-xs text-amber-800">重新读取失败，当前显示的是上一次数据：{error}</div>}
          {system?.runtime.error && <div className="flex items-start gap-2 rounded-xl border border-rose-200 bg-rose-50 px-4 py-3 text-xs leading-5 text-rose-700"><AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0" /><span>后端运行时提示：{system.runtime.error}。若这是启动设置错误，请检查接口、代理或实盘凭据后重新保存。</span></div>}

          {system && <VenueRuntimeCard system={system} capabilityOf={capabilityOf} />}

          <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
            <TabBar items={tabs} value={tab} onChange={setTab} label="场所" />
            <p className="text-xs text-slate-500">接口地址与实盘凭据按场所分别保存；代理与访问密码为全局设置。</p>
          </div>

          <div role="tabpanel" aria-label={venueLabel(tab)} className="space-y-4">
          {tab === 'polymarket' ? (
            <>
              {system && pmData && (
                <EndpointSection
                  system={system}
                  draft={pmData}
                  dirty={pmDataDirty}
                  saving={savingSection === 'data:polymarket'}
                  busy={savingSection !== null}
                  onChange={updateData}
                  onDiscard={resetData}
                  onClear={clearData}
                  onSave={saveData}
                />
              )}
              {system && pmLive && (
                <LiveTradingSection
                  system={system}
                  draft={pmLive}
                  dirty={pmLiveDirty}
                  saving={savingSection === 'live:polymarket'}
                  busy={savingSection !== null}
                  clobBaseURL={pmData?.clob_base_url ?? ''}
                  revisionRef={revisionRef}
                  onDraftEdit={bumpRevision}
                  onChange={updateLive}
                  onReset={resetLive}
                  onSave={saveLive}
                  onWalletCheck={setLiveWalletCheck}
                />
              )}
            </>
          ) : (
            system && pfDraft && (
              <>
                <PredictFunEndpointSection
                  draft={pfDraft}
                  dirty={pfDataDirty}
                  saving={savingSection === 'data:predictfun'}
                  busy={savingSection !== null}
                  runtime={venueRuntime(system, 'predictfun')}
                  onChange={updatePfData}
                  onDiscard={resetPfData}
                  onSave={savePfData}
                />
                <PredictFunLiveSection
                  draft={pfDraft}
                  dirty={pfLiveDirty}
                  saving={savingSection === 'live:predictfun'}
                  busy={savingSection !== null}
                  runtime={venueRuntime(system, 'predictfun')}
                  baseURL={pfDraft.base_url}
                  settlementSupported={pfCapability?.supports_settlement ?? venueMeta('predictfun').supportsSettlement}
                  configurationDirty={pfDataDirty || pfLiveDirty}
                  revisionRef={revisionRef}
                  onChange={updatePfLive}
                  onReset={resetPfLive}
                  onSave={savePfLive}
                />
              </>
            )
          )}
          </div>

          <Section
            icon={Globe2}
            title="网络出口代理"
            description="所有场所的行情与交易请求共用同一出口。修改后新的请求立即使用新代理。"
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
                          {testResult ? (testResult.error || `已通过代理访问 ${testResult.target}`) : `使用当前表单中的配置访问 ${system?.proxy_probe_url || '行情接口'}（所有场所共用该出口），不会保存设置。`}
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
            <p>访问密码只保护网页与 API；各场所的实盘私钥与密钥以 AES-GCM 加密保存在本机 SQLite，读取时只返回掩码或「已配置」标记。监听地址、数据库路径与前端静态目录仍属于启动参数，修改后需要重启服务。</p>
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
