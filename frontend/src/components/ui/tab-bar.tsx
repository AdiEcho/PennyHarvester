import { useRef } from 'react'
import type { LucideIcon } from 'lucide-react'
import { cn } from '@/lib/utils'

export interface TabItem<T extends string> {
  value: T
  label: string
  icon?: LucideIcon
  /** Optional trailing text, e.g. the currency or "已启用". */
  hint?: string
}

/**
 * Minimal tab bar (role="tablist" + roving tabindex + arrow keys). The project
 * has no router, so tabs are pure local state; panels are rendered by the caller
 * with role="tabpanel".
 */
export function TabBar<T extends string>({ items, value, onChange, label, className }: {
  items: TabItem<T>[]
  value: T
  onChange: (value: T) => void
  label: string
  className?: string
}) {
  const refs = useRef<(HTMLButtonElement | null)[]>([])
  const activeIndex = Math.max(0, items.findIndex((item) => item.value === value))

  const move = (delta: number, event: React.KeyboardEvent<HTMLDivElement>) => {
    event.preventDefault()
    const next = (activeIndex + delta + items.length) % items.length
    onChange(items[next].value)
    refs.current[next]?.focus()
  }

  return (
    <div
      role="tablist"
      aria-label={label}
      onKeyDown={(event) => {
        if (event.key === 'ArrowRight' || event.key === 'ArrowDown') move(1, event)
        else if (event.key === 'ArrowLeft' || event.key === 'ArrowUp') move(-1, event)
      }}
      className={cn('flex w-full gap-1 overflow-x-auto rounded-xl border border-slate-200 bg-surface p-1 sm:w-auto', className)}
    >
      {items.map((item, index) => {
        const Icon = item.icon
        const active = item.value === value
        return (
          <button
            key={item.value}
            ref={(node) => { refs.current[index] = node }}
            type="button"
            role="tab"
            aria-selected={active}
            tabIndex={active ? 0 : -1}
            onClick={() => onChange(item.value)}
            className={cn(
              'flex h-8 flex-1 items-center justify-center gap-1.5 whitespace-nowrap rounded-lg px-3 text-xs font-medium transition sm:flex-none',
              active ? 'bg-navy text-white shadow-sm' : 'text-slate-500 hover:bg-slate-50 hover:text-slate-800',
            )}
          >
            {Icon && <Icon className="h-3.5 w-3.5" />}
            {item.label}
            {item.hint && <span className={cn('tabular-nums text-[10px]', active ? 'text-white/60' : 'text-slate-400')}>{item.hint}</span>}
          </button>
        )
      })}
    </div>
  )
}
