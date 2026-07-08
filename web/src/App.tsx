import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { BrowserRouter, Navigate, Route, Routes } from 'react-router-dom'
import { Toaster } from '@/components/ui/sonner'
import { Layout } from '@/components/layout'
import LoginPage from '@/pages/login'
import DashboardPage from '@/pages/dashboard'
import ProvidersPage from '@/pages/providers'
import UsagePage from '@/pages/usage'
import DevicesPage from '@/pages/devices'
import EventsPage from '@/pages/events'
import SettingsPage from '@/pages/settings'

const queryClient = new QueryClient({
  defaultOptions: {
    queries: { retry: 1, staleTime: 15_000, refetchOnWindowFocus: false },
  },
})

export default function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <Routes>
          <Route path="/login" element={<LoginPage />} />
          <Route element={<Layout />}>
            <Route path="/" element={<DashboardPage />} />
            <Route path="/providers" element={<ProvidersPage />} />
            <Route path="/usage" element={<UsagePage />} />
            <Route path="/devices" element={<DevicesPage />} />
            <Route path="/events" element={<EventsPage />} />
            <Route path="/settings" element={<SettingsPage />} />
          </Route>
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </BrowserRouter>
      <Toaster position="top-center" richColors />
    </QueryClientProvider>
  )
}
