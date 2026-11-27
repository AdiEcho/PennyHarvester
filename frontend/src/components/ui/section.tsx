import type { LucideIcon } from 'lucide-react'
import type { ReactNode } from 'react'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'

export function Section({
  icon: Icon,
  title,
  description,
  children,
  actions,
}: {
  icon: LucideIcon
  title: string
  description: string
  children: ReactNode
  actions?: ReactNode
}) {
  return (
    <Card>
      <CardHeader className="border-b border-slate-100">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="flex items-start gap-3">
            <div className="rounded-xl bg-slate-100 p-2.5 text-slate-500">
              <Icon className="h-[18px] w-[18px]" />
            </div>
            <div>
              <CardTitle>{title}</CardTitle>
              <CardDescription className="mt-1">{description}</CardDescription>
            </div>
          </div>
          {actions && <div>{actions}</div>}
        </div>
      </CardHeader>
      <CardContent className="pt-5 sm:pt-6">{children}</CardContent>
    </Card>
  )
}
