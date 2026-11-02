import { useCallback, useEffect, useRef, useState } from 'react'
import { Globe2 } from 'lucide-react'
import { cn } from '@/lib/utils'
import { isVenue, venueMeta, type Venue } from '@/lib/venue'

export type VenueScope = Venue | 'all'

// The scope lives in the page URL so a refresh (or a shared link) keeps the
// selected venue. It is intentionally not part of the account context: accounts
// are already venue-scoped, the filter only narrows global lists.
const QUERY_KEY = 'venue'

export function defaultVenueScope(available: Venue[]): VenueScope {
  return available.length === 1 ? available[0] : 'all'
}

export function readVenueScope(available: Venue[]): VenueScope {
  if (typeof window === 'undefined') return defaultVenueScope(available)
  const raw = new URLSearchParams(window.location.search).get(QUERY_KEY)
  if (raw === 'all') return available.length > 1 ? 'all' : defaultVenueScope(available)
  if (isVenue(raw) && available.includes(raw)) return raw
  return defaultVenueScope(available)
}

export function writeVenueScope(scope: VenueScope) {
  if (typeof window === 'undefined') return
  const url = new URL(window.location.href)
  if (scope === 'all') url.searchParams.delete(QUERY_KEY)
  else url.searchParams.set(QUERY_KEY, scope)
  window.history.replaceState(null, '', `${url.pathname}${url.search}${url.hash}`)
}

/**
 * Scope state plus a ref the polling fetcher can read without changing its
 * identity. usePolling clears `data` when the fetcher changes, so the fetcher
 * must stay stable while the venue lives in a ref; callers trigger refresh() on
 * scope change to keep the previous rows on screen (isRefreshing, not a
 * skeleton) while the new page loads.
 */
export function useVenueScope(available: Venue[]) {
  const [scope, setScopeState] = useState<VenueScope>(() => readVenueScope(available))
  const scopeRef = useRef<VenueScope>(scope)

  const setScope = useCallback((next: VenueScope) => {
    scopeRef.current = next
    setScopeState(next)
    writeVenueScope(next)
  }, [])

  // A bookmarked scope may point at a venue the backend stopped exposing.
  useEffect(() => {
    if (scope === 'all' || available.length === 0 || available.includes(scope)) return
    setScope(defaultVenueScope(available))
  }, [available, scope, setScope])

  return { scope, scopeRef, setScope }
}

interface VenueFilterProps {
  available: Venue[]
  value: VenueScope
  onChange: (scope: VenueScope) => void
  className?: string
}

/**
 * Single-select venue filter rendered as a real radio group: arrow keys move
 * the selection, only the checked radio is tabbable.
 */
export function VenueFilter({ available, value, onChange, className }: VenueFilterProps) {
  const refs = useRef<(HTMLButtonElement | null)[]>([])
  const options: { value: VenueScope; label: string }[] = [
    { value: 'all', label: '全部' },
    ...available.map((venue) => ({ value: venue as VenueScope, label: venueMeta(venue).label })),
  ]
  // A single-venue deployment gains nothing from a one-item filter.
  if (available.length < 2) return null

  const activeIndex = Math.max(0, options.findIndex((option) => option.value === value))

  const move = (delta: number, event: React.KeyboardEvent<HTMLDivElement>) => {
    event.preventDefault()
    const next = (activeIndex + delta + options.length) % options.length
    onChange(options[next].value)
    refs.current[next]?.focus()
  }

  return (
    <div
      className={cn('flex flex-wrap items-center gap-1.5', className)}
      role="radiogroup"
      aria-label="场所筛选"
      onKeyDown={(event) => {
        if (event.key === 'ArrowRight' || event.key === 'ArrowDown') move(1, event)
        else if (event.key === 'ArrowLeft' || event.key === 'ArrowUp') move(-1, event)
      }}
    >
      <span className="mr-1 inline-flex items-center gap-1 text-[11px] font-medium text-slate-400">
        <Globe2 className="h-3.5 w-3.5" />场所
      </span>
      {options.map((option, index) => {
        const active = option.value === value
        const accent = option.value === 'all'
          ? 'border-emerald-200 bg-emerald-50 font-medium text-emerald-800'
          : venueMeta(option.value).accentClass
        return (
          <button
            key={option.value}
            ref={(node) => { refs.current[index] = node }}
            type="button"
            role="radio"
            aria-checked={active}
            tabIndex={active ? 0 : -1}
            onClick={() => onChange(option.value)}
            className={cn(
              'inline-flex items-center gap-1.5 rounded-full border px-3 py-1 text-xs transition',
              active ? accent : 'border-slate-200 bg-surface text-slate-600 hover:border-slate-300 hover:bg-slate-50',
            )}
          >
            {option.label}
          </button>
        )
      })}
    </div>
  )
}
