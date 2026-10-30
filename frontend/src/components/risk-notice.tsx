import { ShieldAlert } from 'lucide-react'
import { useAccount } from '@/lib/account-context'
import { cn } from '@/lib/utils'

export function RiskNotice({ className, compact = false }: { className?: string; compact?: boolean }) {
  const { account } = useAccount()
  const isLive = account?.mode === 'live'
  return (
    <div className={cn('flex items-start gap-2.5 rounded-xl border text-amber-900', isLive ? 'border-rose-200/80 bg-rose-50/80' : 'border-amber-200/80 bg-amber-50/70', compact ? 'p-3' : 'px-4 py-3', className)}>
      <ShieldAlert className={cn('mt-0.5 h-4 w-4 shrink-0', isLive ? 'text-rose-600' : 'text-amber-600')} aria-hidden="true" />
      <p className="text-xs leading-5">
        {isLive ? (
          <>
            <span className="font-semibold">实盘风险提示：价格 0.99 也不代表稳赚不赔。</span>
            {!compact && <span className="text-rose-800/80"> 实盘订单会直接动用真实资金；一旦事件爆冷反转，可能会亏损绝大部分本金。</span>}
          </>
        ) : (
          <>
            <span className="font-semibold">风险提示：价格 0.99 也不代表稳赚，模拟表现不等于实盘收益。</span>
            {!compact && <span className="text-amber-800/80"> 盘面参考价不代表最终一定能按此成交或结算，模拟数据仅供验证策略思路。</span>}
          </>
        )}
      </p>
    </div>
  )
}
