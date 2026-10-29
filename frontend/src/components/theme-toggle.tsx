import { Moon, Sun } from 'lucide-react'
import { useTheme } from '@/lib/theme-context'
import { cn } from '@/lib/utils'

export function ThemeToggle({ className }: { className?: string }) {
  const { theme, toggleTheme } = useTheme()
  const isDark = theme === 'dark'

  return (
    <button
      type="button"
      onClick={toggleTheme}
      className={cn(
        'relative inline-flex h-9 w-9 shrink-0 items-center justify-center rounded-lg text-white/80 transition-colors hover:bg-white/10 hover:text-white focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand-lime focus-visible:ring-offset-2 focus-visible:ring-offset-navy lg:text-slate-500 lg:hover:bg-slate-100 lg:hover:text-slate-950 lg:focus-visible:ring-emerald-500 lg:focus-visible:ring-offset-surface',
        className,
      )}
      aria-label={isDark ? '切换到浅色模式' : '切换到暗黑模式'}
      title={isDark ? '切换到浅色模式' : '切换到暗黑模式'}
    >
      <Sun
        className={cn('h-4 w-4 transition-all duration-200', isDark ? 'rotate-90 scale-0 opacity-0' : 'rotate-0 scale-100 opacity-100')}
        aria-hidden="true"
      />
      <Moon
        className={cn('absolute h-4 w-4 transition-all duration-200', isDark ? 'rotate-0 scale-100 opacity-100' : '-rotate-90 scale-0 opacity-0')}
        aria-hidden="true"
      />
    </button>
  )
}
