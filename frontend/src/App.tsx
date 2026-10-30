import { useState } from 'react'
import { AuthGate } from '@/components/auth-gate'
import { AppShell, type View } from '@/components/app-shell'
import { AccountProvider } from '@/lib/account-context'
import { DashboardView } from '@/views/dashboard'
import { OpportunitiesView } from '@/views/opportunities'
import { PositionsView } from '@/views/positions'
import { OrdersView } from '@/views/orders'
import { SettingsView } from '@/views/settings'
import { SystemView } from '@/views/system'

export default function App() {
  const [view, setView] = useState<View>('dashboard')

  const navigate = (next: View) => {
    setView(next)
    window.scrollTo({ top: 0, behavior: 'smooth' })
  }

  return (
    <AuthGate>
      <AccountProvider>
        <AppShell view={view} onNavigate={navigate}>
          {view === 'dashboard' && <DashboardView onOpenOrders={() => navigate('orders')} />}
          {view === 'opportunities' && <OpportunitiesView onOrderCreated={() => navigate('positions')} />}
          {view === 'positions' && <PositionsView />}
          {view === 'orders' && <OrdersView />}
          {view === 'settings' && <SettingsView />}
          {view === 'system' && <SystemView />}
        </AppShell>
      </AccountProvider>
    </AuthGate>
  )
}
