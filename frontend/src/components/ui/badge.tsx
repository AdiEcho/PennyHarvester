import type { HTMLAttributes } from 'react'
import { cva, type VariantProps } from 'class-variance-authority'
import { cn } from '@/lib/utils'

const badgeVariants = cva('inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs font-medium leading-none', {
  variants: {
    variant: {
      default: 'border-slate-200 bg-slate-100 text-slate-600',
      success: 'border-emerald-200 bg-emerald-50 text-emerald-700',
      warning: 'border-amber-200 bg-amber-50 text-amber-800',
      danger: 'border-rose-200 bg-rose-50 text-rose-700',
      info: 'border-blue-200 bg-blue-50 text-blue-700',
      navy: 'border-white/10 bg-white/10 text-white',
      lime: 'border-brand-lime/20 bg-brand-lime/10 text-brand-lime',
    },
  },
  defaultVariants: { variant: 'default' },
})

export interface BadgeProps extends HTMLAttributes<HTMLSpanElement>, VariantProps<typeof badgeVariants> {}

export function Badge({ className, variant, ...props }: BadgeProps) {
  return <span className={cn(badgeVariants({ variant }), className)} {...props} />
}
