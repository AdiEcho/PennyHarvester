import { cn } from '@/lib/utils'
import { Input } from '@/components/ui/input'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import type { SelectKey } from '@/lib/settings-draft'

export function NumberField({ id, label, description, value, unit, error, min, max, step = 'any', placeholder, onChange }: {
  id: string
  label: string
  description: string
  value: string
  unit: string
  error?: string
  min?: number
  max?: number
  step?: string
  placeholder?: string
  onChange: (value: string) => void
}) {
  return (
    <div>
      <label htmlFor={id} className="text-sm font-medium text-slate-700">{label}</label>
      <p className="mt-1 min-h-8 text-xs leading-4 text-slate-400">{description}</p>
      <div className="relative mt-2">
        <Input id={id} type="number" inputMode="decimal" value={value} min={min} max={max} step={step} placeholder={placeholder} onChange={(event) => onChange(event.target.value)} aria-invalid={Boolean(error)} aria-describedby={error ? `${id}-error` : undefined} className={cn('pr-20 tabular-nums', error && 'border-rose-400 focus:border-rose-500 focus:ring-rose-500/10')} />
        <span className="pointer-events-none absolute right-3 top-1/2 -translate-y-1/2 text-xs text-slate-400">{unit}</span>
      </div>
      {error && <p id={`${id}-error`} className="mt-1.5 text-xs leading-4 text-rose-600" role="alert">{error}</p>}
    </div>
  )
}

export function SelectField({ id, label, description, value, options, error, onChange }: {
  id: SelectKey
  label: string
  description: string
  value: string
  options: { value: string; label: string }[]
  error?: string
  onChange: (value: string) => void
}) {
  return (
    <div>
      <label htmlFor={id} className="text-sm font-medium text-slate-700">{label}</label>
      <p className="mt-1 min-h-8 text-xs leading-4 text-slate-400">{description}</p>
      <Select value={value} onValueChange={onChange}>
        <SelectTrigger id={id} className={cn('mt-2', error && 'border-rose-400 focus:border-rose-500 focus:ring-rose-500/10')} aria-invalid={Boolean(error)} aria-describedby={error ? `${id}-error` : undefined}>
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          {options.map((option) => <SelectItem key={option.value} value={option.value}>{option.label}</SelectItem>)}
        </SelectContent>
      </Select>
      {error && <p id={`${id}-error`} className="mt-1.5 text-xs leading-4 text-rose-600" role="alert">{error}</p>}
    </div>
  )
}
