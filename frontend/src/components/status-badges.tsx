import { AlertTriangle, Check, CircleCheck, CircleX, Clock3, FlaskConical, Radio, X } from 'lucide-react'
import type { AccountMode, OrderStatus, PositionStatus } from '@/types'
import { Badge } from '@/components/ui/badge'

export function OrderStatusBadge({ status }: { status: OrderStatus }) {
  if (status === 'filled') return <Badge variant="success"><Check className="h-3 w-3" /> 已成交</Badge>
  if (status === 'settled') return <Badge variant="info"><CircleCheck className="h-3 w-3" /> 已结算</Badge>
  if (status === 'pending') return <Badge variant="warning"><Clock3 className="h-3 w-3" /> 确认中</Badge>
  if (status === 'unknown') return <Badge variant="warning"><AlertTriangle className="h-3 w-3" /> 待核对</Badge>
  return <Badge variant="danger"><X className="h-3 w-3" /> 未成交</Badge>
}

export function PositionStatusBadge({ status }: { status: PositionStatus }) {
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

export function OrderModeBadge({ mode, kind }: { mode: AccountMode | string; kind: 'manual' | 'auto' }) {
  const isLive = mode === 'live'
  return <Badge variant={isLive ? 'warning' : 'success'}>{isLive ? <Radio className="h-3 w-3" /> : <FlaskConical className="h-3 w-3" />} {isLive ? '实盘' : '模拟'}{kind === 'auto' ? '自动' : '手动'}</Badge>
}
