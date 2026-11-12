import { AlertTriangle, Check, CircleCheck, CircleX, Clock3, FlaskConical, Radio, ShieldAlert, X } from 'lucide-react'
import type { AccountMode, OrderStatus, PositionStatus } from '@/types'
import { venueMeta } from '@/lib/venue'
import { Badge } from '@/components/ui/badge'
import { cn } from '@/lib/utils'

// Venue chip: every row/number that belongs to a venue carries one, so a USDT
// amount can never be read as a USDC one. `full` spells the label out for card
// layouts; tables use the two-letter tag.
export function VenueChip({ venue, full = false, className }: { venue?: string | null; full?: boolean; className?: string }) {
  const meta = venueMeta(venue)
  return (
    <Badge
      variant={meta.tone}
      className={cn('px-2 py-0.5 text-[11px]', className)}
      title={`${meta.label} · ${meta.currency}`}
    >
      {full ? meta.label : meta.short}
      <span className="sr-only">{meta.label}</span>
    </Badge>
  )
}

export function OrderStatusBadge({ status }: { status: OrderStatus }) {
  if (status === 'filled') return <Badge variant="success"><Check className="h-3 w-3" /> 已成交</Badge>
  if (status === 'settled') return <Badge variant="info"><CircleCheck className="h-3 w-3" /> 已结算</Badge>
  if (status === 'pending') return <Badge variant="warning"><Clock3 className="h-3 w-3" /> 确认中</Badge>
  if (status === 'unknown') return <Badge variant="warning"><AlertTriangle className="h-3 w-3" /> 待核对</Badge>
  return <Badge variant="danger"><X className="h-3 w-3" /> 未成交</Badge>
}

// A position stays "open" until the venue publishes a resolution, which for a
// real election or game can land hours after the scheduled end date. The badge
// switches to 待场馆结算 once that time has passed so an overdue row does not
// read as a stuck one.
export function PositionStatusBadge({ status, exitKind, endDate }: { status: PositionStatus; exitKind?: string; endDate?: string | null }) {
  if (exitKind === 'stop') return <Badge variant="warning"><ShieldAlert className="h-3 w-3" /> 已止损</Badge>
  if (status === 'open') {
    const overdue = endDate ? new Date(endDate).getTime() <= Date.now() : false
    if (overdue) return <Badge variant="warning" title="到期时间已过，场馆尚未公布结算结果；结果公布后引擎会自动按结算价平仓。"><Clock3 className="h-3 w-3" /> 待场馆结算</Badge>
    return <Badge variant="info"><Clock3 className="h-3 w-3" /> 持有中</Badge>
  }
  if (status === 'won') return <Badge variant="success"><CircleCheck className="h-3 w-3" /> 已胜出</Badge>
  if (status === 'split') return <Badge variant="warning"><AlertTriangle className="h-3 w-3" /> 平局退款</Badge>
  return <Badge variant="danger"><CircleX className="h-3 w-3" /> 未胜出</Badge>
}

export function ModeBadge({ mode, testnet = false, className }: { mode: AccountMode | string; testnet?: boolean; className?: string }) {
  if (mode === 'live') {
    return <Badge variant="warning" className={className} title={testnet ? '实盘账户连接 Predict.fun 测试网，签名不涉及主网资金。' : undefined}><Radio className="h-3 w-3" /> {testnet ? '测试网 TESTNET' : '实盘 LIVE'}</Badge>
  }
  return <Badge variant="success" className={className}><FlaskConical className="h-3 w-3" /> 模拟 PAPER</Badge>
}

export function OrderModeBadge({ mode, kind }: { mode: AccountMode | string; kind: 'manual' | 'auto' | 'stop' }) {
  const isLive = mode === 'live'
  if (kind === 'stop') {
    return <Badge variant="warning"><ShieldAlert className="h-3 w-3" /> {isLive ? '实盘' : '模拟'}止损</Badge>
  }
  return <Badge variant={isLive ? 'warning' : 'success'}>{isLive ? <Radio className="h-3 w-3" /> : <FlaskConical className="h-3 w-3" />} {isLive ? '实盘' : '模拟'}{kind === 'auto' ? '自动' : '手动'}</Badge>
}
