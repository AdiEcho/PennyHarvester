import type { LucideIcon } from 'lucide-react'
import { AlertCircle, Inbox, RefreshCw } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { cn } from '@/lib/utils'

export function Skeleton({ className }: { className?: string }) {
  return <div className={cn('animate-pulse rounded-md bg-slate-200/70', className)} aria-hidden="true" />
}

export function LoadingBlock({ rows = 4 }: { rows?: number }) {
  return (
    <div className="space-y-4 p-5 sm:p-6" role="status" aria-label="正在加载数据">
      <Skeleton className="h-5 w-1/3" />
      {Array.from({ length: rows }).map((_, index) => (
        <div className="flex items-center gap-4" key={index}>
          <Skeleton className="h-10 flex-1" />
          <Skeleton className="hidden h-5 w-24 sm:block" />
          <Skeleton className="h-5 w-16" />
        </div>
      ))}
    </div>
  )
}

interface EmptyStateProps {
  title: string
  description: string
  icon?: LucideIcon
  action?: React.ReactNode
  compact?: boolean
}

export function EmptyState({ title, description, icon: Icon = Inbox, action, compact }: EmptyStateProps) {
  return (
    <div className={cn('flex flex-col items-center justify-center px-6 text-center', compact ? 'py-8' : 'py-14 sm:py-20')}>
      <div className="mb-4 rounded-2xl border border-slate-200 bg-slate-50 p-3 text-slate-400">
        <Icon className="h-6 w-6" strokeWidth={1.6} />
      </div>
      <h3 className="text-sm font-semibold text-slate-800">{title}</h3>
      <p className="mt-1.5 max-w-sm text-sm leading-6 text-slate-500">{description}</p>
      {action && <div className="mt-5">{action}</div>}
    </div>
  )
}

export function ErrorState({ message, onRetry, compact = false }: { message: string; onRetry?: () => void; compact?: boolean }) {
  return (
    <div className={cn('flex flex-col items-center justify-center px-5 text-center', compact ? 'py-6' : 'py-12')} role="alert">
      <div className="mb-3 rounded-full bg-rose-50 p-2.5 text-rose-600">
        <AlertCircle className="h-5 w-5" />
      </div>
      <p className="text-sm font-medium text-slate-800">暂时无法获取数据</p>
      <p className="mt-1 max-w-md break-words text-sm leading-6 text-slate-500">{message}</p>
      {onRetry && (
        <Button variant="secondary" size="sm" className="mt-4" onClick={onRetry}>
          <RefreshCw className="h-3.5 w-3.5" /> 重新加载
        </Button>
      )}
    </div>
  )
}
