import { lazy, Suspense, useState } from 'react'
import { AuthGate } from '@/components/auth-gate'
import { AppShell, type View } from '@/components/app-shell'
import { AccountProvider } from '@/lib/account-context'
import { LoadingBlock } from '@/components/ui/feedback'

const DashboardView = lazy(() => import('@/views/dashboard').then((module) => ({ default: module.DashboardView })))
const OpportunitiesView = lazy(() => import('@/views/opportunities').then((module) => ({ default: module.OpportunitiesView })))
const PositionsView = lazy(() => import('@/views/positions').then((module) => ({ default: module.PositionsView })))
const OrdersView = lazy(() => import('@/views/orders').then((module) => ({ default: module.OrdersView })))
const OperationsView = lazy(() => import('@/views/operations').then((module) => ({ default: module.OperationsView })))
const SettingsView = lazy(() => import('@/views/settings').then((module) => ({ default: module.SettingsView })))
const SystemView = lazy(() => import('@/views/system').then((module) => ({ default: module.SystemView })))

export default function App() {
  const [view, setView] = useState<View>('dashboard')

  const navigate = (next: View) => {
    setView(next)
    window.scrollTo({ top: 0, behavior: 'smooth' })
  }

  const viewMap: Record<View, React.ReactElement> = {
    dashboard: <DashboardView onOpenOrders={() => navigate('orders')} />,
    opportunities: <OpportunitiesView onOrderCreated={() => navigate('positions')} />,
    positions: <PositionsView />,
    orders: <OrdersView />,
    operations: <OperationsView />,
    settings: <SettingsView />,
    system: <SystemView />,
  }

  return (
    <AuthGate>
      <AccountProvider>
        <AppShell view={view} onNavigate={navigate}>
          <Suspense fallback={<LoadingBlock />}>
            {viewMap[view]}
          </Suspense>
        </AppShell>
      </AccountProvider>
    </AuthGate>
  )
}
