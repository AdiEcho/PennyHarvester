import { useCallback, useEffect, useMemo, useRef, useState, type FormEvent } from 'react'
import {
  AlertTriangle,
  CircleCheck,
  CircleX,
  Clock3,
  ExternalLink,
  Link2,
  PenLine,
  Radio,
  RefreshCw,
  ShieldAlert,
  ShieldCheck,
} from 'lucide-react'
import { api } from '@/lib/api'
import { useAccount } from '@/lib/account-context'
import { usePolling } from '@/hooks/usePolling'
import { useVenues } from '@/hooks/useVenues'
import type {
  ChainOperation,
  ChainOperationKind,
  ChainOperationStatus,
  ChainOperationStatusFilter,
  ChainOperationsResponse,
  ResolveChainOperationBody,
} from '@/types'
import { formatDate, formatFullDate, formatNumber } from '@/lib/format'
import { venueMeta, venueOf } from '@/lib/venue'
import { cn } from '@/lib/utils'
import { useToast } from '@/components/toast'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Card } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogTitle } from '@/components/ui/dialog'
import { Input, Label } from '@/components/ui/input'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { EmptyState, ErrorState, LoadingBlock } from '@/components/ui/feedback'
import { InlineError, PageHeader } from '@/components/page-header'
import { VenueChip } from '@/components/status-badges'
import { VenueFilter, useVenueScope, type VenueScope } from '@/components/venue-filter'

const PAGE_SIZE = 50

const statusFilters: { value: ChainOperationStatusFilter; label: string }[] = [
  { value: 'unknown', label: '待核对' },
  { value: 'intent', label: '待签名' },
  { value: 'signed', label: '已签名' },
  { value: 'broadcast', label: '已广播' },
  { value: 'confirmed', label: '已确认' },
  { value: 'reverted', label: '已回滚' },
  { value: 'all', label: '全部状态' },
]

const resolveStatuses: { value: 'confirmed' | 'reverted'; label: string }[] = [
  { value: 'confirmed', label: '已确认（链上成功）' },
  { value: 'reverted', label: '已回滚（链上失败）' },
]

const explorerTxBases: Partial<Record<number, string>> = {
  137: 'https://polygonscan.com/tx/',
  56: 'https://bscscan.com/tx/',
  97: 'https://testnet.bscscan.com/tx/',
}

function statusLabel(status: ChainOperationStatus | ChainOperationStatusFilter) {
  return statusFilters.find((filter) => filter.value === status)?.label || status
}

function scopeLabel(scope: VenueScope) {
  return scope === 'all' ? '全部场所' : venueMeta(scope).label
}

function kindLabel(kind: ChainOperationKind) {
  return kind === 'approve' ? '授权' : '赎回'
}

function chainLabel(chainId: number) {
  if (chainId === 137) return 'Polygon'
  if (chainId === 56) return 'BNB Chain'
  if (chainId === 97) return 'BNB 测试网'
  return `链 ${chainId}`
}

function explorerTxURL(chainId: number, hash: string | null | undefined) {
  const base = explorerTxBases[chainId]
  const trimmed = hash?.trim()
  return base && trimmed ? `${base}${trimmed}` : null
}

function ChainOperationStatusBadge({ status }: { status: ChainOperationStatus }) {
  if (status === 'intent')
    return (
      <Badge className="whitespace-nowrap">
        <Clock3 className="h-3 w-3" /> 待签名
      </Badge>
    )
  if (status === 'signed')
    return (
      <Badge variant="info" className="whitespace-nowrap">
        <PenLine className="h-3 w-3" /> 已签名
      </Badge>
    )
  if (status === 'broadcast')
    return (
      <Badge variant="info" className="whitespace-nowrap">
        <Radio className="h-3 w-3" /> 已广播
      </Badge>
    )
  if (status === 'unknown')
    return (
      <Badge variant="warning" className="whitespace-nowrap">
        <AlertTriangle className="h-3 w-3" /> 待核对
      </Badge>
    )
  if (status === 'confirmed')
    return (
      <Badge variant="success" className="whitespace-nowrap">
        <CircleCheck className="h-3 w-3" /> 已确认
      </Badge>
    )
  return (
    <Badge variant="danger" className="whitespace-nowrap">
      <CircleX className="h-3 w-3" /> 已回滚
    </Badge>
  )
}

function TxHash({ operation }: { operation: ChainOperation }) {
  const hash = operation.tx_hash?.trim()
  if (!hash) return <p className="mt-1.5 text-[11px] text-slate-400">未记录交易哈希</p>
  const url = explorerTxURL(operation.chain_id, hash)
  if (!url) return <p className="mt-1.5 break-all font-mono text-[10px] leading-4 text-slate-500">{hash}</p>
  return (
    <a
      href={url}
      target="_blank"
      rel="noreferrer"
      title={`在区块浏览器查看 ${hash}`}
      className="mt-1.5 inline-flex items-start gap-1 break-all font-mono text-[10px] leading-4 text-blue-700 hover:text-blue-900 hover:underline"
    >
      <ExternalLink className="mt-0.5 h-3 w-3 shrink-0" />
      <span className="break-all">{hash}</span>
    </a>
  )
}

function OperationCard({
  operation,
  accountName,
  onResolve,
}: {
  operation: ChainOperation
  accountName: string
  onResolve: (operation: ChainOperation) => void
}) {
  return (
    <Card className="p-4">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <p className="flex flex-wrap items-center gap-1.5 text-xs text-slate-400">
            <VenueChip venue={venueOf(operation)} />#{operation.id} · {kindLabel(operation.kind)}
          </p>
          <p className="mt-1.5 text-sm font-medium leading-6 text-slate-900">{accountName}</p>
          <p className="mt-0.5 text-[11px] text-slate-400">
            {operation.account_id === 0 ? '场所级授权' : `账户 #${operation.account_id}`}
            {operation.position_id != null && ` · 持仓 #${operation.position_id}`}
          </p>
        </div>
        <div className="flex shrink-0 flex-col items-end gap-2">
          <ChainOperationStatusBadge status={operation.status} />
          {operation.status === 'unknown' && (
            <Button variant="secondary" size="sm" onClick={() => onResolve(operation)}>
              核对
            </Button>
          )}
        </div>
      </div>

      <div className="mt-4 grid grid-cols-2 gap-3 border-t border-slate-100 pt-4">
        <div>
          <p className="text-[11px] text-slate-400">链</p>
          <p className="mt-1 text-xs text-slate-700">
            {chainLabel(operation.chain_id)} · {operation.chain_id}
          </p>
        </div>
        <div className="text-right">
          <p className="text-[11px] text-slate-400">Nonce</p>
          <p className="mt-1 text-xs tabular-nums text-slate-700">{operation.nonce ?? '—'}</p>
        </div>
      </div>

      <div className="mt-3">
        <p className="text-[11px] text-slate-400">From</p>
        <p className="mt-0.5 break-all font-mono text-[10px] leading-4 text-slate-600">
          {operation.from_address || '—'}
        </p>
        <TxHash operation={operation} />
      </div>

      {operation.market_id && (
        <p className="mt-3 break-all text-[10px] text-slate-400">
          市场 <span className="font-mono">{operation.market_id}</span>
        </p>
      )}

      <div className="mt-3 rounded-lg bg-slate-50 px-3 py-2">
        <p className="text-[11px] text-slate-400">原始原因</p>
        <p className="mt-0.5 text-xs leading-5 text-slate-600">{operation.reason || '无附加说明'}</p>
        {operation.resolution_note && (
          <p className="mt-1.5 text-xs leading-5 text-slate-600">核对说明：{operation.resolution_note}</p>
        )}
        {operation.resolved_by && (
          <p className="mt-1 text-[10px] text-slate-400">
            核对人 {operation.resolved_by}
            {operation.resolved_at ? ` · ${formatDate(operation.resolved_at)}` : ''}
          </p>
        )}
      </div>

      <div className="mt-3 flex flex-wrap items-center justify-between gap-2 text-[11px] text-slate-400">
        <span>创建 {formatDate(operation.created_at)}</span>
        <span>更新 {formatDate(operation.updated_at)}</span>
      </div>
    </Card>
  )
}

interface OperationsQuery {
  status: ChainOperationStatusFilter
  offset: number
  scope: VenueScope
  accountId: number
}

interface OperationsSnapshot {
  response: ChainOperationsResponse
  query: OperationsQuery
  observedAt: number
}

interface ResolveForm {
  status: 'confirmed' | 'reverted'
  actor: string
  reason: string
  txHash: string
}

const emptyResolveForm: ResolveForm = {
  status: 'confirmed',
  actor: '',
  reason: '',
  txHash: '',
}

export function OperationsView() {
  const [status, setStatus] = useState<ChainOperationStatusFilter>('unknown')
  const [offset, setOffset] = useState(0)
  // Venue-wide approvals are journalled with account_id 0. The account picker
  // normally selects one real account, so offer an explicit all-accounts scope
  // that reaches those rows without pretending 0 is a selectable account.
  const [allAccounts, setAllAccounts] = useState(false)
  const { account, accounts, accountId, selectAccount, isLoading: accountLoading } = useAccount()
  const { venues } = useVenues()
  const { scope, scopeRef, setScope } = useVenueScope(venues)
  const { toast } = useToast()

  // The status/offset/venue live in refs so the polling fetcher keeps a stable
  // identity. A filter change re-issues the query via refresh(), which keeps
  // the accepted rows on screen (stale) instead of dropping to skeletons.
  const statusRef = useRef(status)
  const offsetRef = useRef(offset)
  const fetchOperations = useCallback(
    async (signal: AbortSignal): Promise<OperationsSnapshot> => {
      const queryAccountId = allAccounts ? 0 : accountId
      if (queryAccountId === undefined) throw new Error('请先选择账户。')
      const query: OperationsQuery = {
        status: statusRef.current,
        offset: offsetRef.current,
        scope: scopeRef.current,
        accountId: queryAccountId,
      }
      const response = await api.chainOperations(
        {
          account_id: query.accountId,
          venue: query.scope === 'all' ? null : query.scope,
          status: query.status,
          limit: PAGE_SIZE,
          offset: query.offset,
        },
        signal,
      )
      return { response, query, observedAt: Date.now() }
    },
    [accountId, allAccounts, scopeRef],
  )
  const {
    data: snapshot,
    error,
    isLoading,
    isRefreshing,
    refresh,
  } = usePolling(fetchOperations, {
    enabled: allAccounts || accountId !== undefined,
  })

  const appliedQuery = useRef({ status, offset, scope })
  useEffect(() => {
    const previous = appliedQuery.current
    if (previous.status === status && previous.offset === offset && previous.scope === scope) return
    // A status or venue change invalidates page boundaries, so start at the
    // first page. The ref is updated synchronously to avoid a second fetch.
    if ((previous.status !== status || previous.scope !== scope) && offset !== 0) {
      offsetRef.current = 0
      appliedQuery.current = { status, offset: 0, scope }
      setOffset(0)
      refresh()
      return
    }
    appliedQuery.current = { status, offset, scope }
    refresh()
  }, [status, offset, scope, refresh])

  // Account switching resets pagination: a large offset from the previous
  // account may not exist in the new one. The fetcher identity change already
  // clears the accepted snapshot, so a reset at page 0 needs no extra refresh.
  const lastAccountId = useRef(accountId)
  useEffect(() => {
    if (lastAccountId.current === accountId) return
    lastAccountId.current = accountId
    setAllAccounts(false)
    if (offsetRef.current === 0) return
    offsetRef.current = 0
    appliedQuery.current = { ...appliedQuery.current, offset: 0 }
    setOffset(0)
    refresh()
  }, [accountId, refresh])

  const changeStatus = (next: ChainOperationStatusFilter) => {
    statusRef.current = next
    offsetRef.current = 0
    setStatus(next)
    setOffset(0)
  }
  const changeOffset = (next: number) => {
    // A failed page change leaves the old accepted snapshot on screen. Clicking
    // the same target again must retry it instead of being swallowed by an
    // identical setState.
    if (offsetRef.current === next) {
      refresh()
      return
    }
    offsetRef.current = next
    setOffset(next)
  }

  const accountNames = useMemo(() => new Map(accounts.map((item) => [item.id, item.name])), [accounts])
  const accountNameFor = useCallback(
    (id: number) => (id === 0 ? '场所级（全部账户）' : accountNames.get(id) || `账户 #${id}`),
    [accountNames],
  )

  const [target, setTarget] = useState<ChainOperation | null>(null)
  const [form, setForm] = useState<ResolveForm>(emptyResolveForm)
  const [formError, setFormError] = useState<string | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const submittingRef = useRef(false)

  const openResolve = useCallback((operation: ChainOperation) => {
    if (operation.status !== 'unknown') return
    setTarget(operation)
    setForm({
      status: 'confirmed',
      actor: '',
      reason: '',
      txHash: operation.tx_hash?.trim() || '',
    })
    setFormError(null)
  }, [])

  const closeResolve = useCallback(() => {
    if (submittingRef.current) return
    setTarget(null)
    setFormError(null)
  }, [])

  const handleResolve = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    if (!target || submittingRef.current) return
    const actor = form.actor.trim()
    const reason = form.reason.trim()
    const txHash = form.txHash.trim()
    if (!actor) {
      setFormError('请填写核对人。')
      return
    }
    if (!reason) {
      setFormError('请填写核对说明，说明判断依据。')
      return
    }
    if (form.status === 'confirmed' && !txHash) {
      setFormError('确认为已上链时必须填写交易哈希，可先在区块浏览器核对。')
      return
    }

    submittingRef.current = true
    setSubmitting(true)
    setFormError(null)
    const body: ResolveChainOperationBody = {
      status: form.status,
      actor,
      reason,
      ...(txHash ? { tx_hash: txHash } : {}),
    }
    try {
      await api.resolveChainOperation(target.id, body)
      toast({
        title: '核对结果已记录',
        description: `链上操作 #${target.id} 已标记为${form.status === 'confirmed' ? '已确认' : '已回滚'}，不会重发或广播任何交易。`,
        variant: 'success',
      })
      setTarget(null)
      refresh()
    } catch (caught) {
      setFormError(caught instanceof Error ? caught.message : '保存核对结果失败，请稍后重试。')
    } finally {
      submittingRef.current = false
      setSubmitting(false)
    }
  }

  const accepted = snapshot?.query
  const items = snapshot?.response.items || []
  const shownStatus = accepted?.status ?? status
  const outOfRange = Boolean(snapshot && accepted && accepted.offset > 0 && accepted.offset >= snapshot.response.total)
  const staleLabel = snapshot
    ? `${accountNameFor(snapshot.query.accountId)} · ${statusLabel(snapshot.query.status)} · ${scopeLabel(snapshot.query.scope)} · 观察于 ${formatFullDate(new Date(snapshot.observedAt).toISOString())}`
    : ''
  const currentTxURL = target ? explorerTxURL(target.chain_id, form.txHash) : null
  const lockedTxHash = Boolean(target?.tx_hash?.trim())

  return (
    <>
      <PageHeader
        eyebrow="CHAIN OPERATIONS"
        title="链上操作"
        description="核对授权与赎回交易的链上结果。状态不明的记录需人工在区块浏览器确认后登记；本页面不会重发、替换或广播任何交易。"
        actions={
          <Button variant="secondary" onClick={refresh} disabled={isRefreshing}>
            <RefreshCw className={cn('h-4 w-4', isRefreshing && 'animate-spin')} />
            刷新记录
          </Button>
        }
      />

      {error && snapshot && <InlineError message={`${error}（当前展示 ${staleLabel}）`} onRetry={refresh} />}

      <Card className="mb-4 p-3 sm:p-4">
        <div className="flex flex-col gap-3 xl:flex-row xl:items-end xl:justify-between">
          <div className="flex flex-col gap-3 sm:flex-row sm:flex-wrap sm:items-end">
            <div className="w-full space-y-1.5 sm:w-[220px]">
              <p className="text-[11px] font-medium text-slate-400">账户</p>
              <Select
                value={allAccounts ? 'all' : account ? String(account.id) : 'none'}
                onValueChange={(value) => {
                  if (value === 'all') {
                    setAllAccounts(true)
                    if (offsetRef.current !== 0) {
                      offsetRef.current = 0
                      setOffset(0)
                    }
                    return
                  }
                  setAllAccounts(false)
                  if (value !== 'none') selectAccount(Number(value))
                  if (offsetRef.current !== 0) {
                    offsetRef.current = 0
                    setOffset(0)
                  }
                }}
                disabled={accountLoading || accounts.length === 0}
              >
                <SelectTrigger className="h-9 text-xs" aria-label="选择账户">
                  <SelectValue placeholder="无账户" className="min-w-0 flex-1 truncate text-left" />
                </SelectTrigger>
                <SelectContent>
                  {accounts.length === 0 ? (
                    <SelectItem value="none" disabled>
                      无账户
                    </SelectItem>
                  ) : (
                    <>
                      <SelectItem value="all">全部账户 / 场所级</SelectItem>
                      {accounts.map((item) => (
                        <SelectItem key={item.id} value={String(item.id)}>
                          {item.name} · {venueMeta(item.venue).short} · {item.mode === 'live' ? '实盘' : '模拟'}
                        </SelectItem>
                      ))}
                    </>
                  )}
                </SelectContent>
              </Select>
            </div>

            <div className="w-full space-y-1.5 sm:w-[160px]">
              <p className="text-[11px] font-medium text-slate-400">状态</p>
              <Select value={status} onValueChange={(value) => changeStatus(value as ChainOperationStatusFilter)}>
                <SelectTrigger className="h-9 text-xs" aria-label="状态筛选">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {statusFilters.map((filter) => (
                    <SelectItem key={filter.value} value={filter.value}>
                      {filter.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>

            <div className="flex min-h-9 items-center pb-0.5">
              <VenueFilter available={venues} value={scope} onChange={setScope} />
            </div>
          </div>

          <p className="text-xs text-slate-500">
            显示 <span className="font-semibold tabular-nums text-slate-800">{formatNumber(items.length, 0)}</span> 条
            {snapshot ? ` / 共 ${formatNumber(snapshot.response.total, 0)} 条` : ''}
            {snapshot && <span className="ml-1 text-slate-400">（{staleLabel}）</span>}
          </p>
        </div>
      </Card>

      {accountLoading || isLoading ? (
        <Card>
          <LoadingBlock rows={6} />
        </Card>
      ) : error && !snapshot ? (
        <Card>
          <ErrorState message={error} onRetry={refresh} />
        </Card>
      ) : outOfRange && snapshot && accepted ? (
        <Card>
          <EmptyState
            icon={Link2}
            title="当前页码超出记录范围"
            description={`${statusLabel(accepted.status)} · ${scopeLabel(accepted.scope)} 共 ${formatNumber(snapshot.response.total, 0)} 条，当前页从第 ${formatNumber(accepted.offset + 1, 0)} 条开始。`}
            action={
              <Button variant="secondary" onClick={() => changeOffset(Math.max(0, accepted.offset - PAGE_SIZE))}>
                返回上一页
              </Button>
            }
          />
        </Card>
      ) : !items.length ? (
        <Card>
          <EmptyState
            icon={shownStatus === 'unknown' ? ShieldCheck : Link2}
            title={
              shownStatus === 'unknown'
                ? '暂无待核对的链上操作'
                : shownStatus === 'all'
                  ? '暂无链上操作记录'
                  : '此状态下暂无链上操作'
            }
            description={
              shownStatus === 'unknown'
                ? '所有授权与赎回交易都有明确结果。出现回执不明时，记录会出现在这里，等待人工在区块浏览器核对后登记。'
                : shownStatus === 'all'
                  ? '授权与赎回交易在签名、广播后都会记录在这里，包括交易哈希与链上状态。'
                  : '切换其他状态，或查看全部链上操作。'
            }
            action={
              shownStatus !== 'unknown' ? (
                <Button variant="secondary" onClick={() => changeStatus('unknown')}>
                  查看待核对
                </Button>
              ) : undefined
            }
          />
        </Card>
      ) : (
        <>
          <Card className="hidden overflow-hidden lg:block">
            <div className="overflow-x-auto">
              <table className="w-full min-w-[1120px] text-left text-sm">
                <thead className="border-b border-slate-100 bg-slate-50/70 text-xs text-slate-500">
                  <tr>
                    <th className="px-5 py-3.5 font-medium">操作</th>
                    <th className="px-4 py-3.5 font-medium">账户 / 场所</th>
                    <th className="px-4 py-3.5 font-medium">状态</th>
                    <th className="px-4 py-3.5 font-medium">链 / Nonce</th>
                    <th className="px-4 py-3.5 font-medium">地址 / 交易</th>
                    <th className="px-4 py-3.5 font-medium">原因 / 核对</th>
                    <th className="px-5 py-3.5 text-right font-medium">时间</th>
                  </tr>
                </thead>
                <tbody className={cn('divide-y divide-slate-100 transition-opacity', isRefreshing && 'opacity-60')}>
                  {items.map((operation) => (
                    <tr key={operation.id} className="align-top hover:bg-slate-50/60">
                      <td className="px-5 py-4">
                        <p className="font-medium text-slate-800">
                          #{operation.id} · {kindLabel(operation.kind)}
                        </p>
                        {operation.position_id != null && (
                          <p className="mt-1 text-[11px] text-slate-400">持仓 #{operation.position_id}</p>
                        )}
                      </td>
                      <td className="px-4 py-4">
                        <div className="flex flex-wrap items-center gap-1.5">
                          <p className="text-xs font-medium text-slate-700">{accountNameFor(operation.account_id)}</p>
                          <VenueChip venue={venueOf(operation)} />
                        </div>
                        <p className="mt-1 text-[11px] text-slate-400">
                          {operation.account_id === 0 ? '场所级授权' : `ID ${operation.account_id}`}
                        </p>
                      </td>
                      <td className="px-4 py-4">
                        <div className="flex flex-col items-start gap-2">
                          <ChainOperationStatusBadge status={operation.status} />
                          {operation.status === 'unknown' && (
                            <Button variant="secondary" size="sm" onClick={() => openResolve(operation)}>
                              核对
                            </Button>
                          )}
                        </div>
                      </td>
                      <td className="px-4 py-4">
                        <p className="text-xs text-slate-700">{chainLabel(operation.chain_id)}</p>
                        <p className="mt-0.5 text-[11px] text-slate-400">Chain {operation.chain_id}</p>
                        <p className="mt-1 text-[11px] tabular-nums text-slate-500">Nonce {operation.nonce ?? '—'}</p>
                      </td>
                      <td className="max-w-[300px] px-4 py-4">
                        <p className="break-all font-mono text-[10px] leading-4 text-slate-500">
                          <span className="text-slate-400">From </span>
                          {operation.from_address || '—'}
                        </p>
                        <TxHash operation={operation} />
                      </td>
                      <td className="max-w-[250px] px-4 py-4">
                        <p
                          className="line-clamp-2 text-xs leading-5 text-slate-600"
                          title={operation.reason || undefined}
                        >
                          {operation.reason || '无附加说明'}
                        </p>
                        {operation.resolution_note && (
                          <p
                            className="mt-1 line-clamp-2 text-xs leading-5 text-slate-500"
                            title={operation.resolution_note}
                          >
                            核对说明：{operation.resolution_note}
                          </p>
                        )}
                        {operation.resolved_by && (
                          <p className="mt-1 text-[10px] text-slate-400">
                            核对人 {operation.resolved_by}
                            {operation.resolved_at ? ` · ${formatDate(operation.resolved_at)}` : ''}
                          </p>
                        )}
                      </td>
                      <td className="px-5 py-4 text-right text-xs text-slate-500">
                        <p>创建 {formatDate(operation.created_at)}</p>
                        <p className="mt-1 text-[11px] text-slate-400">更新 {formatDate(operation.updated_at)}</p>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </Card>

          <div className="grid gap-3 sm:grid-cols-2 lg:hidden">
            {items.map((operation) => (
              <OperationCard
                key={operation.id}
                operation={operation}
                accountName={accountNameFor(operation.account_id)}
                onResolve={openResolve}
              />
            ))}
          </div>

          {snapshot && snapshot.response.total > PAGE_SIZE && accepted && (
            <div className="mt-4 flex flex-col items-center justify-between gap-3 text-xs text-slate-400 sm:flex-row">
              <p>
                当前显示第 {formatNumber(accepted.offset + 1, 0)}–{formatNumber(accepted.offset + items.length, 0)}{' '}
                条，共 {formatNumber(snapshot.response.total, 0)} 条记录
                <span className="text-slate-300"> · </span>
                {staleLabel}
              </p>
              <div className="flex gap-2">
                <Button
                  variant="secondary"
                  size="sm"
                  disabled={accepted.offset === 0 || isRefreshing}
                  onClick={() => changeOffset(Math.max(0, accepted.offset - PAGE_SIZE))}
                >
                  上一页
                </Button>
                <Button
                  variant="secondary"
                  size="sm"
                  disabled={accepted.offset + items.length >= snapshot.response.total || isRefreshing}
                  onClick={() => changeOffset(accepted.offset + PAGE_SIZE)}
                >
                  下一页
                </Button>
              </div>
            </div>
          )}
        </>
      )}

      <Dialog
        open={Boolean(target)}
        onOpenChange={(open) => {
          if (!open) closeResolve()
        }}
      >
        <DialogContent className="max-w-xl">
          <div>
            <DialogTitle>核对链上操作</DialogTitle>
            <DialogDescription>登记人工在区块浏览器确认后的真实结果，用于解除该操作的后续阻塞。</DialogDescription>
          </div>

          {target && (
            <form className="space-y-4" onSubmit={handleResolve}>
              <div className="rounded-xl border border-slate-200 bg-slate-50 px-4 py-3 text-xs leading-5 text-slate-600">
                <p className="font-medium text-slate-800">
                  #{target.id} · {kindLabel(target.kind)} · {venueMeta(venueOf(target)).label} ·{' '}
                  {chainLabel(target.chain_id)}
                </p>
                <p className="mt-1 break-all font-mono text-[10px] text-slate-500">
                  {target.from_address || '来源地址未记录'}
                </p>
                <p className="mt-1">原始原因：{target.reason || '无附加说明'}</p>
                {target.tx_hash && (
                  <p className="mt-1 break-all font-mono text-[10px] text-slate-500">原交易：{target.tx_hash}</p>
                )}
              </div>

              <div className="flex items-start gap-3 rounded-xl border border-amber-200 bg-amber-50 px-4 py-3 text-xs leading-5 text-amber-900">
                <ShieldAlert className="mt-0.5 h-4 w-4 shrink-0" />
                <p>
                  <span className="font-semibold">仅记录人工核对结果。</span>{' '}
                  此操作不会重发、替换或广播任何链上交易；请先在区块浏览器确认交易是否成功。
                </p>
              </div>

              <div className="space-y-1.5">
                <Label htmlFor="chain-resolve-status">
                  核对结果 <span className="text-rose-600">*</span>
                </Label>
                <Select
                  value={form.status}
                  onValueChange={(value) =>
                    setForm((current) => ({
                      ...current,
                      status: value as 'confirmed' | 'reverted',
                    }))
                  }
                >
                  <SelectTrigger id="chain-resolve-status">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {resolveStatuses.map((option) => (
                      <SelectItem key={option.value} value={option.value}>
                        {option.label}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>

              <div className="space-y-1.5">
                <Label htmlFor="chain-resolve-actor">
                  核对人 <span className="text-rose-600">*</span>
                </Label>
                <Input
                  id="chain-resolve-actor"
                  value={form.actor}
                  onChange={(event) =>
                    setForm((current) => ({
                      ...current,
                      actor: event.target.value,
                    }))
                  }
                  placeholder="填写核对人姓名或工号"
                  autoComplete="off"
                />
              </div>

              <div className="space-y-1.5">
                <Label htmlFor="chain-resolve-reason">
                  核对说明 <span className="text-rose-600">*</span>
                </Label>
                <textarea
                  id="chain-resolve-reason"
                  value={form.reason}
                  onChange={(event) =>
                    setForm((current) => ({
                      ...current,
                      reason: event.target.value,
                    }))
                  }
                  placeholder="说明核对依据，例如区块浏览器显示的确认数、失败原因或交易不存在。"
                  rows={3}
                  className="flex min-h-[96px] w-full rounded-lg border border-slate-200 bg-surface px-3 py-2 text-sm text-slate-900 shadow-sm outline-none transition placeholder:text-slate-400 focus:border-emerald-500 focus:ring-2 focus:ring-emerald-500/10 disabled:cursor-not-allowed disabled:bg-slate-50 disabled:opacity-60"
                />
              </div>

              <div className="space-y-1.5">
                <Label htmlFor="chain-resolve-tx">
                  交易哈希
                  {form.status === 'confirmed' && <span className="ml-1 text-rose-600">*</span>}
                </Label>
                <Input
                  id="chain-resolve-tx"
                  value={form.txHash}
                  onChange={(event) => {
                    if (!lockedTxHash)
                      setForm((current) => ({
                        ...current,
                        txHash: event.target.value,
                      }))
                  }}
                  readOnly={lockedTxHash}
                  placeholder="0x…"
                  autoComplete="off"
                  spellCheck={false}
                  className={cn('font-mono text-xs', lockedTxHash && 'bg-slate-50 text-slate-500')}
                />
                <p className="text-[11px] leading-4 text-slate-400">
                  {lockedTxHash
                    ? '原记录哈希不可修改；如原哈希有误，请在核对说明中写明正确哈希。'
                    : '该记录没有交易哈希；确认链上成功时必须填写。'}
                </p>
                {currentTxURL && (
                  <a
                    href={currentTxURL}
                    target="_blank"
                    rel="noreferrer"
                    className="inline-flex items-center gap-1 text-[11px] font-medium text-blue-700 hover:text-blue-900 hover:underline"
                  >
                    <ExternalLink className="h-3 w-3" />
                    在区块浏览器打开
                  </a>
                )}
              </div>

              {formError && (
                <p
                  role="alert"
                  className="rounded-lg border border-rose-200 bg-rose-50 px-3 py-2 text-xs leading-5 text-rose-700"
                >
                  {formError}
                </p>
              )}

              <div className="flex flex-col-reverse gap-2 pt-1 sm:flex-row sm:justify-end">
                <Button type="button" variant="secondary" onClick={closeResolve} disabled={submitting}>
                  取消
                </Button>
                <Button type="submit" variant="primary" loading={submitting} disabled={submitting}>
                  {submitting ? '提交中…' : '记录核对结果'}
                </Button>
              </div>
            </form>
          )}
        </DialogContent>
      </Dialog>
    </>
  )
}
