import { useCallback, useMemo } from 'react'
import { api } from '@/lib/api'
import { usePolling } from '@/hooks/usePolling'
import { VENUES, isVenue, type Venue } from '@/lib/venue'
import type { VenueCapabilities } from '@/types'

/**
 * Venue capabilities from GET /venues. Falls back to the static presentation
 * order so the UI keeps working before the endpoint answers (or when it fails):
 * an unreachable /venues must not hide an entire venue's data.
 */
export function useVenues() {
  const fetchVenues = useCallback((signal: AbortSignal) => api.getVenues(signal), [])
  const { data, error } = usePolling(fetchVenues, { interval: 60000 })
  const capabilities = useMemo<VenueCapabilities[]>(() => data ?? [], [data])
  const venues = useMemo<Venue[]>(() => {
    const fromApi = capabilities.map((item) => item.venue).filter(isVenue)
    return fromApi.length > 0 ? fromApi : VENUES
  }, [capabilities])
  const capabilityOf = useCallback(
    (venue: Venue) => capabilities.find((item) => item.venue === venue) ?? null,
    [capabilities],
  )
  return { venues, capabilities, capabilityOf, error }
}
