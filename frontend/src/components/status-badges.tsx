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

export function PositionStatusBadge({ status, exitKind }: { status: PositionStatus; exitKind?: string }) {
  if (exitKind === 'stop') return <Badge variant="warning"><ShieldAlert className="h-3 w-3" /> 已止损</Badge>
  if (status === 'open') return <Badge variant="info"><Clock3 className="h-3 w-3" /> 持有中</Badge>
  if (status === 'won') return <Badge variant="success"><CircleCheck className="h-3 w-3" /> 已胜出</Badge>
  if (status === 'split') return <Badge variant="warning"><AlertTriangle className="h-3 w-3" /> 平局退款</Badge>
  return <Badge variant="danger"><CircleX className="h-3 w-3" /> 未胜出</Badge>
}

export function ModeBadge({ mode, className }: { mode: AccountMode | string; className?: string }) {
  if (mode === 'live') {
    return <Badge variant="warning" className={className}><Radio className="h-3 w-3" /> 实盘 LIVE</Badge>
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
