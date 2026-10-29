import { useCallback, useEffect, useRef, useState } from 'react'

interface PollingOptions {
  interval?: number
  enabled?: boolean
}

export function usePolling<T>(
  fetcher: (signal: AbortSignal) => Promise<T>,
  { interval = 20000, enabled = true }: PollingOptions = {},
) {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [isLoading, setIsLoading] = useState(enabled)
  const [isRefreshing, setIsRefreshing] = useState(false)
  const [refreshKey, setRefreshKey] = useState(0)
  const dataRef = useRef<T | null>(null)
  const previousFetcher = useRef(fetcher)

  const refresh = useCallback(() => setRefreshKey((key) => key + 1), [])

  useEffect(() => {
    if (previousFetcher.current !== fetcher) {
      previousFetcher.current = fetcher
      dataRef.current = null
      setData(null)
      setError(null)
    }

    let active = true
    let timer: ReturnType<typeof setTimeout> | null = null
    let controller: AbortController | null = null

    const clearTimer = () => {
      if (timer) clearTimeout(timer)
      timer = null
    }

    const load = async () => {
      if (!active || !enabled || document.hidden) return
      clearTimer()
      controller?.abort()
      controller = new AbortController()
      const hasData = dataRef.current !== null
      setIsLoading(!hasData)
      setIsRefreshing(hasData)

      try {
        const result = await fetcher(controller.signal)
        if (!active) return
        dataRef.current = result
        setData(result)
        setError(null)
      } catch (caught) {
        if (!active || (caught instanceof DOMException && caught.name === 'AbortError')) return
        setError(caught instanceof Error ? caught.message : '数据加载失败，请重试。')
      } finally {
        if (active) {
          setIsLoading(false)
          setIsRefreshing(false)
          timer = setTimeout(load, interval)
        }
      }
    }

    const handleVisibility = () => {
      if (document.hidden) {
        clearTimer()
        controller?.abort()
        if (active) setIsRefreshing(false)
      } else {
        void load()
      }
    }

    if (enabled) void load()
    else {
      setIsLoading(false)
      setIsRefreshing(false)
    }
    document.addEventListener('visibilitychange', handleVisibility)

    return () => {
      active = false
      clearTimer()
      controller?.abort()
      document.removeEventListener('visibilitychange', handleVisibility)
    }
  }, [fetcher, interval, enabled, refreshKey])

  return { data, error, isLoading, isRefreshing, refresh }
}
