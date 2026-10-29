import { useState } from 'react'
import { AppShell, type View } from '@/components/app-shell'
import { AccountProvider } from '@/lib/account-context'
import { DashboardView } from '@/views/dashboard'
import { OpportunitiesView } from '@/views/opportunities'
import { PositionsView } from '@/views/positions'
import { OrdersView } from '@/views/orders'
import { SettingsView } from '@/views/settings'

export default function App() {
  const [view, setView] = useState<View>('dashboard')

  const navigate = (next: View) => {
    setView(next)
    window.scrollTo({ top: 0, behavior: 'smooth' })
  }

  return (
    <AccountProvider>
      <AppShell view={view} onNavigate={navigate}>
        {view === 'dashboard' && <DashboardView onOpenOrders={() => navigate('orders')} />}
        {view === 'opportunities' && <OpportunitiesView onOrderCreated={() => navigate('positions')} />}
        {view === 'positions' && <PositionsView />}
        {view === 'orders' && <OrdersView />}
        {view === 'settings' && <SettingsView />}
      </AppShell>
    </AccountProvider>
  )
}
