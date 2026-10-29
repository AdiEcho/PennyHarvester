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
            <span className="font-semibold">真实资金风险：0.99 ≠ 无风险。</span>
            {!compact && <span className="text-rose-800/80"> 实盘订单会立即提交到 Polymarket CLOB；高价结果失败时可能损失接近全部本金。</span>}
          </>
        ) : (
          <>
            <span className="font-semibold">0.99 ≠ 无风险；模拟结果不代表真实收益。</span>
            {!compact && <span className="text-amber-800/80"> 当前价格不等于保证结算，请仅将结果用于策略研究。</span>}
          </>
        )}
      </p>
    </div>
  )
}
