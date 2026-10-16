import { lazy, Suspense, useEffect } from 'react'
import { Navigate, Route, Routes } from 'react-router-dom'
import { AppLayout } from '@/components/layout/AppLayout'
import { useAuthStore } from '@/stores/authStore'
import { readBrowserToken, saveBrowserToken, silencerFetch } from '@/lib/silencer-fetch'
import { TokenGate } from '@/components/common/TokenGate'

// 路由懒加载：将 779KB 单 chunk 拆为按页面按需加载
const DashboardPage = lazy(() => import('@/features/dashboard/pages/Dashboard'))
const LogsPage = lazy(() => import('@/features/logs/pages/Logs'))
const StatsPage = lazy(() => import('@/features/stats/pages/Stats'))
const SettingsPage = lazy(() => import('@/features/settings/pages/Settings'))
const AuditPage = lazy(() => import('@/features/audit/pages/Audit'))
const WordsPage = lazy(() => import('@/features/settings/pages/Words'))
const ClientsPage = lazy(() => import('@/features/clients/pages/Clients'))

function App() {
  const { setToken, authChecked, authenticated, setAuth } = useAuthStore()

  // 浏览器初始化：读取一次性链接 token，再查询 Web 控制台登录状态。
  useEffect(() => {
    const statusPromise = silencerFetch<{ initialized: boolean; authenticated: boolean; username?: string }>(
      '/api/auth/status', { noToken: true },
    ).then((status) => setAuth(status)).catch(() => setAuth({ initialized: false, authenticated: false }))
    const url = new URL(window.location.href)
    const fromUrl = (url.searchParams.get('token') || '').trim()
    let fromFragment = ''
    const hashRaw = url.hash.startsWith('#') ? url.hash.slice(1) : ''
    if (hashRaw) {
      const qIdx = hashRaw.indexOf('?')
      if (qIdx >= 0) {
        const qParams = new URLSearchParams(hashRaw.slice(qIdx + 1))
        fromFragment = (qParams.get('token') || '').trim()
        if (fromFragment) {
          qParams.delete('token')
          const restQ = qParams.toString()
          const routePath = hashRaw.slice(0, qIdx)
          url.hash = restQ ? `#${routePath}?${restQ}` : `#${routePath}`
        }
      } else {
        const fragmentParams = new URLSearchParams(hashRaw)
        fromFragment = (fragmentParams.get('token') || '').trim()
        if (fromFragment) {
          fragmentParams.delete('token')
          const rest = fragmentParams.toString()
          url.hash = rest ? `#${rest}` : ''
        }
      }
    }
    const fromLink = fromUrl || fromFragment
    if (fromLink) {
      saveBrowserToken(fromLink)
      url.searchParams.delete('token')
      window.history.replaceState(null, '', url.pathname + url.search + url.hash)
    }
    const devToken = import.meta.env.DEV ? (import.meta.env.VITE_SILENCER_TOKEN || '') : ''
    const token = fromLink || readBrowserToken() || devToken
    if (token) {
      setToken(token)
      silencerFetch('/api/status', { headers: { 'X-Silencer-Token': token } })
        .then(() => setAuth({ initialized: true, authenticated: true, username: 'admin' }))
        .catch(() => undefined)
    }
    void statusPromise
  }, [setToken, setAuth])

  if (!authChecked || !authenticated) {
    return <TokenGate />
  }

  return (
    <AppLayout>
      <Suspense fallback={null}>
        <Routes>
          <Route path="/" element={<DashboardPage />} />
          <Route path="/logs" element={<LogsPage />} />
          <Route path="/stats" element={<StatsPage />} />
          <Route path="/words" element={<WordsPage />} />
          <Route path="/clients" element={<ClientsPage />} />
          <Route path="/audit" element={<AuditPage />} />
          <Route path="/settings" element={<SettingsPage />} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </Suspense>
    </AppLayout>
  )
}

export default App
