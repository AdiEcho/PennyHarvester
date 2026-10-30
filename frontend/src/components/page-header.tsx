import type { ReactNode } from 'react'
import { AlertCircle, RefreshCw } from 'lucide-react'
import { Button } from '@/components/ui/button'

export function PageHeader({ eyebrow, title, description, actions }: {
  eyebrow?: string
  title: string
  description: string
  actions?: ReactNode
}) {
  return (
    <div className="mb-6 flex flex-col justify-between gap-4 sm:mb-7 sm:flex-row sm:items-end">
      <div>
        {eyebrow && <p className="mb-1.5 text-xs font-semibold uppercase tracking-[0.16em] text-emerald-700">{eyebrow}</p>}
        <h1 className="text-2xl font-semibold tracking-[-0.025em] text-slate-950 sm:text-[28px]">{title}</h1>
        <p className="mt-2 max-w-2xl text-sm leading-6 text-slate-500">{description}</p>
      </div>
      {actions && <div className="flex shrink-0 items-center gap-2">{actions}</div>}
    </div>
  )
}

export function InlineError({ message, onRetry, hasData = true }: { message: string; onRetry: () => void; hasData?: boolean }) {
  return (
    <div className="mb-5 flex flex-col gap-3 rounded-xl border border-rose-200 bg-rose-50 px-4 py-3 text-sm text-rose-800 sm:flex-row sm:items-center" role="alert">
      <div className="flex flex-1 items-start gap-2">
        <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" />
        <span className="break-words"><span className="font-medium">{hasData ? '数据同步遇到问题，当前显示的是上一轮数据。' : '数据加载失败。'}</span> {message}</span>
      </div>
      <Button variant="ghost" size="sm" className="self-start text-rose-700 hover:bg-rose-100 hover:text-rose-900" onClick={onRetry}>
        <RefreshCw className="h-3.5 w-3.5" /> 重试
      </Button>
    </div>
  )
}
