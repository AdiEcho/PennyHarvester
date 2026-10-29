import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { api } from '@/lib/api'
import { usePolling } from '@/hooks/usePolling'
import type { Account } from '@/types'

const STORAGE_KEY = 'pennyharvester.account'

interface AccountContextValue {
  accounts: Account[]
  account: Account | null
  accountId: number | undefined
  selectAccount: (id: number) => void
  refreshAccounts: () => void
  isLoading: boolean
  error: string | null
}

const AccountContext = createContext<AccountContextValue | null>(null)

export function AccountProvider({ children }: { children: ReactNode }) {
  const fetchAccounts = useCallback((signal: AbortSignal) => api.accounts(signal), [])
  const { data, error, isLoading, refresh } = usePolling(fetchAccounts, { interval: 30000 })
  const accounts = useMemo(() => data?.items || [], [data])
  const pendingRef = useRef<number | null>(null)
  const [selectedId, setSelectedId] = useState<number | null>(() => {
    const stored = Number(window.localStorage.getItem(STORAGE_KEY))
    return Number.isFinite(stored) && stored > 0 ? stored : null
  })

  useEffect(() => {
    if (!accounts.length) return
    const exists = selectedId !== null && accounts.some((item) => item.id === selectedId)
    if (exists) {
      pendingRef.current = null
      return
    }
    // A just-created account may not be in the current list yet; keep the
    // selection until the refreshed account list arrives.
    if (selectedId !== null && pendingRef.current === selectedId) return
    const fallback = accounts.find((item) => item.is_default) || accounts[0]
    setSelectedId(fallback.id)
    window.localStorage.setItem(STORAGE_KEY, String(fallback.id))
  }, [accounts, selectedId])

  const selectAccount = useCallback((id: number) => {
    pendingRef.current = id
    setSelectedId(id)
    window.localStorage.setItem(STORAGE_KEY, String(id))
  }, [])

  const account = useMemo(
    () => accounts.find((item) => item.id === selectedId) || null,
    [accounts, selectedId],
  )

  const value = useMemo<AccountContextValue>(() => ({
    accounts,
    account,
    accountId: account?.id,
    selectAccount,
    refreshAccounts: refresh,
    isLoading,
    error,
  }), [accounts, account, selectAccount, refresh, isLoading, error])

  return <AccountContext.Provider value={value}>{children}</AccountContext.Provider>
}

export function useAccount(): AccountContextValue {
  const value = useContext(AccountContext)
  if (!value) throw new Error('useAccount must be used inside AccountProvider')
  return value
}
