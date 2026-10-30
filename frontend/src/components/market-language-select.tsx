import { Languages } from 'lucide-react'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import type { MarketLanguage } from '@/lib/market-language'
import { cn } from '@/lib/utils'

export function MarketLanguageSelect({
  value,
  onChange,
  className,
}: {
  value: MarketLanguage
  onChange: (value: MarketLanguage) => void
  className?: string
}) {
  return (
    <Select
      value={value}
      onValueChange={(next) => {
        if (next === 'zh' || next === 'en') onChange(next)
      }}
    >
      <SelectTrigger
        className={cn('h-9 w-[118px] gap-1.5 border-slate-200 bg-surface px-2.5 text-xs font-medium text-slate-700 shadow-sm', className)}
        aria-label="市场语言"
        title="切换市场问题与方向的显示语言"
      >
        <Languages className="h-4 w-4 shrink-0 text-slate-400" aria-hidden="true" />
        <SelectValue className="min-w-0 flex-1 text-left" />
      </SelectTrigger>
      <SelectContent align="end">
        <SelectItem value="zh">中文</SelectItem>
        <SelectItem value="en">English</SelectItem>
      </SelectContent>
    </Select>
  )
}
