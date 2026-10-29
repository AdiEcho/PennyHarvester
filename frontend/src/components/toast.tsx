import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from 'react'
import { AlertCircle, CheckCircle2, Info, X } from 'lucide-react'
import { cn } from '@/lib/utils'

type ToastVariant = 'success' | 'error' | 'info'
interface ToastInput { title: string; description?: string; variant?: ToastVariant }
interface ToastItem extends ToastInput { id: number }
interface ToastContextValue { toast: (input: ToastInput) => void }

const ToastContext = createContext<ToastContextValue | null>(null)

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<ToastItem[]>([])

  const dismiss = useCallback((id: number) => {
    setToasts((current) => current.filter((item) => item.id !== id))
  }, [])

  const toast = useCallback((input: ToastInput) => {
    const id = Date.now() + Math.random()
    setToasts((current) => [...current.slice(-2), { ...input, id }])
    setTimeout(() => dismiss(id), input.variant === 'error' ? 6000 : 4000)
  }, [dismiss])

  const value = useMemo(() => ({ toast }), [toast])

  return (
    <ToastContext.Provider value={value}>
      {children}
      <div className="pointer-events-none fixed bottom-5 left-1/2 z-[100] flex w-[calc(100%-2rem)] max-w-md -translate-x-1/2 flex-col gap-2" aria-live="polite" aria-atomic="false">
        {toasts.map((item) => {
          const Icon = item.variant === 'error' ? AlertCircle : item.variant === 'info' ? Info : CheckCircle2
          return (
            <div key={item.id} className={cn(
              'pointer-events-auto flex animate-slide-in items-start gap-3 rounded-xl border bg-surface p-4 shadow-float',
              item.variant === 'error' ? 'border-rose-200' : item.variant === 'success' ? 'border-emerald-200' : 'border-slate-200',
            )} role={item.variant === 'error' ? 'alert' : 'status'}>
              <Icon className={cn('mt-0.5 h-5 w-5 shrink-0', item.variant === 'error' ? 'text-rose-600' : item.variant === 'info' ? 'text-blue-600' : 'text-emerald-600')} />
              <div className="min-w-0 flex-1">
                <p className="text-sm font-semibold text-slate-900">{item.title}</p>
                {item.description && <p className="mt-0.5 break-words text-sm leading-5 text-slate-500">{item.description}</p>}
              </div>
              <button onClick={() => dismiss(item.id)} className="rounded p-0.5 text-slate-400 hover:bg-slate-100 hover:text-slate-700" aria-label="关闭通知">
                <X className="h-4 w-4" />
              </button>
            </div>
          )
        })}
      </div>
    </ToastContext.Provider>
  )
}

export function useToast() {
  const context = useContext(ToastContext)
  if (!context) throw new Error('useToast 必须在 ToastProvider 内使用')
  return context
}
