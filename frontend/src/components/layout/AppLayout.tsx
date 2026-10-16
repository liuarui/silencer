/**
 * 主布局：左侧导航（可收起）+ 顶部状态栏 + 内容区
 * Tailwind layout with light and dark themes selected by data-theme.
 */
import { useEffect, useState, type ReactNode } from 'react'
import { NavLink } from 'react-router-dom'
import { useVisibility } from '@/lib/useVisibility'
import {
  LayoutDashboard,
  ScrollText,
  ChartNoAxesColumnIncreasing,
  ShieldCheck,
  Settings2,
  PanelLeft,
  Play,
  Square,
  Moon,
  Sun,
  ScanText,
  Waypoints,
  AlertCircle,
  Languages,
  AlertTriangle,
  X,
  LogOut,
} from 'lucide-react'
import { useQueryClient } from '@tanstack/react-query'
import { startProxy, stopProxy } from '@/api/proxy'
import { emergencyDisableOriginCheck } from '@/api/settings'
import { useAuthStore } from '@/stores/authStore'
import { Button } from '@/components/ui/button'
import { ErrorBoundary } from '@/components/common/ErrorBoundary'
import { PlaintextSwitch } from '@/components/common/PlaintextSwitch'
import { Logo } from '@/components/brand/Logo'
import { cn } from '@/lib/utils'
import { toast } from '@/lib/toast'
import { silencerFetch } from '@/lib/silencer-fetch'

import { useI18n } from '@/lib/i18n'
import { useLocation } from 'react-router-dom'
import { getStoredWallpaperConfig } from '@/lib/wallpaper'
import { useProxyStatus } from '@/features/proxy/hooks/useProxyStatus'

// 导航项：label 用 i18n key（t('nav.' + key)），随语言切换
const NAV_ITEMS = [
  { to: '/', key: 'dashboard', icon: LayoutDashboard },
  { to: '/clients', key: 'clients', icon: Waypoints },
  { to: '/logs', key: 'logs', icon: ScrollText },
  { to: '/stats', key: 'stats', icon: ChartNoAxesColumnIncreasing },
  { to: '/words', key: 'words', icon: ScanText },
  { to: '/audit', key: 'audit', icon: ShieldCheck },
  { to: '/settings', key: 'settings', icon: Settings2 },
]

/** 主题：亮色优先，data-theme 属性切换（Tailwind darkMode selector 对齐）。
 *  初值直接读 main.tsx 已写入的属性，不再自己判断默认值——两处各判一次必然漂移。 */
function useTheme() {
  // 初始值直接读 html 属性（main.tsx 已同步初始化）
  const [dark, setDark] = useState(() => {
    return document.documentElement.getAttribute('data-theme') === 'dark'
  })
  useEffect(() => {
    document.documentElement.setAttribute('data-theme', dark ? 'dark' : 'light')
    try {
      localStorage.setItem('silencer_theme', dark ? 'dark' : 'light')
    } catch {
      // localStorage 不可用时仅内存态
    }
  }, [dark])
  return { dark, toggle: () => setDark((d) => !d) }
}

export function AppLayout({ children }: { children: ReactNode }) {
  const [collapsed, setCollapsed] = useState(() => {
    if (typeof window !== 'undefined' && window.innerWidth < 1024) {
      return true
    }
    try {
      return localStorage.getItem('silencer_sidebar_collapsed') === '1'
    } catch {
      return false
    }
  })
  const { dark, toggle } = useTheme()
  const queryClient = useQueryClient()
  const { username, setAuth } = useAuthStore()
  // Pause polling while the page is hidden, then refresh when it becomes visible.
  const { hidden } = useVisibility()

  // 个性化背景：统一由 getStoredWallpaperConfig 获取（未配置全新启动自动启用旗舰流光，支持跨窗口即时响应）
  const [wallpaperConfig, setWallpaperConfig] = useState(() => getStoredWallpaperConfig())
  const { bgImage, bgOpacity, bgBlur, cardOpacity } = wallpaperConfig

  // 跨窗口/跨标签同步：设置页改了背景，其他打开的页面即时刷新
  useEffect(() => {
    const onStorage = (e: StorageEvent) => {
      if (
        e.key === 'silencer_bg_image' ||
        e.key === 'silencer_bg_opacity' ||
        e.key === 'silencer_bg_blur' ||
        e.key === 'silencer_card_opacity'
      ) {
        setWallpaperConfig(getStoredWallpaperConfig())
      }
    }
    window.addEventListener('storage', onStorage)
    return () => window.removeEventListener('storage', onStorage)
  }, [])

  const hasCustomBg = Boolean(bgImage && bgOpacity > 0)

  const [proxyBusy, setProxyBusy] = useState(false)
  const [originBlocked, setOriginBlocked] = useState(false)
  const [rescuingOrigin, setRescuingOrigin] = useState(false)

  useEffect(() => {
    const onOriginRejected = () => setOriginBlocked(true)
    window.addEventListener('silencer:origin_rejected', onOriginRejected)
    return () => window.removeEventListener('silencer:origin_rejected', onOriginRejected)
  }, [])

  const handleRescueOrigin = async () => {
    setRescuingOrigin(true)
    try {
      await emergencyDisableOriginCheck()
      setOriginBlocked(false)
      toast(t('settings.toast.saved'))
      queryClient.invalidateQueries()
    } catch (e) {
      toast(String(e), 'error')
    } finally {
      setRescuingOrigin(false)
    }
  }

  // 代理状态轮询（方案 §5：TanStack Query refetchInterval 替代 setInterval）
  // 页面隐藏时停止轮询，正常 5s 轮询。
  // 在刚启动（status 未返回/starting）或操作中时，以 1000ms 快速探活，消除状态滞后！
  const { data: status, error: statusError } = useProxyStatus(hidden)

  const isRunning = status?.proxy_running ?? false
  const isStarting = (status === undefined && !statusError) || !!status?.proxy_starting || (proxyBusy && !isRunning)
  const isStopping = (proxyBusy && isRunning) || !!status?.proxy_stopping

  // /api/status 能成功返回本身就是面板可达的硬信号。
  const statusReached = !!status && !statusError
  const engineAvailable = statusReached
  const { lang, setLang, t, tf } = useI18n()
  const routeLocation = useLocation()

  // 页面标题（从路由映射，i18n）
  // 必须用 useLocation() 而不是全局 window.location：应用跑在 HashRouter 下，
  // window.location.pathname 恒为 "/"，导致顶栏标题在任何页面都显示「控制台」
  // useLocation 还能在路由变化时触发重渲染。
  const pageTitle = (() => {
    const p = routeLocation.pathname
    if (p === '/') return t('nav.dashboard')
    if (p === '/logs') return t('nav.logs')
    if (p === '/stats') return t('nav.stats')
    if (p === '/words') return t('nav.words')
    if (p === '/clients') return t('nav.clients')
    if (p === '/audit') return t('nav.audit')
    if (p === '/settings') return t('nav.settings')
    return t('layout.appName')
  })()


  const isLogsPage = routeLocation.pathname === '/logs'
  // 数据密集型页面：在大屏下放宽版心到 1536px，让表格和趋势图自然舒展；表单类页面保持 1200px
  const isWidePage = ['/', '/logs', '/stats', '/audit'].includes(routeLocation.pathname)

  const toggleProxy = async () => {
    if (proxyBusy) return
    setProxyBusy(true)
    try {
      if (isRunning) {
        await stopProxy()
        toast(t('layout.proxyStoppedToast'))
      } else {
        const r = await startProxy()
        if (!r.ok) {
          toast(r.message || r.error || t('layout.proxyStartFail'), 'error')
          return
        }
        toast(t('layout.proxyStartedToast'))
      }
      queryClient.invalidateQueries({ queryKey: ['proxyStatus'] })
    } catch (e) {
      toast(tf('layout.opFail', { e: String(e) }), 'error')
    } finally {
      setProxyBusy(false)
    }
  }

  // 真正引擎异常判定：若代理正在运行（isRunning 为 true）或状态接口能正常返回，
  // 说明引擎与代理真实存活可用，此时启动初期的瞬态 last_error 不应误报「引擎异常」红灯。
  const showEngineError = false

  return (
    <div
      className={cn('flex h-screen overflow-hidden bg-background text-foreground', hasCustomBg && 'has-custom-bg')}
      style={hasCustomBg ? ({ '--card-opacity': (cardOpacity / 100).toString() } as React.CSSProperties) : undefined}
    >
      {/* 个性化背景层：在所有内容之下，透明度与高斯模糊由用户控制 */}
      {bgImage && bgOpacity > 0 && (
        <div
          className="pointer-events-none fixed inset-0 z-0 bg-cover bg-center bg-no-repeat transition-[filter,transform,opacity] duration-300"
          style={{
            backgroundImage: `url("${bgImage}")`,
            opacity: bgOpacity / 100,
            filter: bgBlur > 0 ? `blur(${bgBlur}px)` : undefined,
            transform: bgBlur > 0 ? 'scale(1.04)' : undefined,
          }}
        />
      )}
      {/* ===== 侧边栏 ===== */}
      <aside
        className={cn(
          'relative flex shrink-0 flex-col border-r bg-card transition-all duration-300',
          collapsed ? 'w-[52px]' : 'w-56',
        )}
      >
        {/* Logo 区 */}
        <div className={cn('flex h-14 shrink-0 items-center gap-2.5 border-b px-3', collapsed && 'justify-center px-0')}>
          <Logo className="h-8 w-8 shrink-0 drop-shadow-[0_2px_8px_hsl(217_91%_60%/0.35)]" />
          {!collapsed && (
            <div className="min-w-0">
              <div className="flex items-center gap-1.5">
                <span className="truncate text-sm font-bold leading-tight">Silencer</span>
                {status?.version && (
                  <span className="rounded border bg-muted/60 px-1 py-0 font-mono text-[10px] text-muted-foreground">
                    v{status.version}
                  </span>
                )}
              </div>
              <div className="truncate text-[10px] font-medium leading-tight text-muted-foreground">
                {t('layout.slogan')}
              </div>
            </div>
          )}
        </div>

        {/* 导航 */}
        <nav className="flex-1 space-y-0.5 overflow-y-auto p-2">
          {NAV_ITEMS.map((item) => {
            const label = t(`nav.${item.key}`)
            return (
            <NavLink
              key={item.to}
              to={item.to}
              title={label}
              className={({ isActive }) =>
                cn(
                  'group relative flex items-center gap-2.5 rounded-xl px-2.5 py-2 text-[13px] font-medium transition-all duration-200',
                  isActive
                    ? 'bg-gradient-to-r from-primary/20 via-primary/10 to-primary/5 text-primary shadow-[inset_0_1px_0_rgb(255_255_255/0.10)]'
                    : 'text-muted-foreground hover:bg-gradient-to-r hover:from-muted/70 hover:to-muted/20 hover:text-foreground',
                  collapsed && 'justify-center px-0',
                )
              }
            >
              {({ isActive }) => (
                <>
                  {/* 激活指示条 */}
                  <span
                    className={cn(
                      'absolute left-0 top-1/2 h-4 w-[2px] -translate-y-1/2 rounded-full bg-primary transition-all duration-200',
                      isActive ? 'opacity-100' : 'opacity-0',
                    )}
                  />
                  <item.icon
                    aria-hidden="true"
                    className={cn(
                      'h-[17px] w-[17px] shrink-0 transition-transform duration-200 group-hover:scale-[1.04]',
                      isActive ? 'drop-shadow-[0_1px_3px_hsl(217_91%_60%/0.28)]' : 'opacity-85 group-hover:opacity-100',
                    )}
                    strokeWidth={1.8}
                    strokeLinecap="round"
                    strokeLinejoin="round"
                  />
                  {!collapsed && <span className="truncate">{label}</span>}
                </>
              )}
            </NavLink>
            )
          })}
        </nav>

        {/* 收起按钮 */}
        <button
          onClick={() => {
            setCollapsed((c) => {
              const next = !c
              try { localStorage.setItem('silencer_sidebar_collapsed', next ? '1' : '0') } catch {}
              return next
            })
          }}
          className={cn(
            'flex h-9 shrink-0 items-center justify-center border-t text-muted-foreground transition-colors hover:bg-muted hover:text-foreground',
          )}
          title={collapsed ? t('layout.expandSidebar') : t('layout.collapseSidebar')}
        >
          <PanelLeft className={cn('h-4 w-4 transition-transform duration-300', collapsed && 'rotate-180')} />
        </button>
      </aside>

      {/* ===== 主区 ===== */}
      <div className="flex min-w-0 flex-1 flex-col overflow-hidden">
        {/* 顶栏：毛玻璃 */}
        <header className="flex min-w-0 h-14 shrink-0 items-center gap-2 border-b bg-background/70 px-3 backdrop-blur-md sm:gap-3 sm:px-5">
          {/* 状态指示 */}
          <div className="flex items-center gap-2.5">
            <span className="relative flex h-2.5 w-2.5">
              {/* Avoid an always-on animation that would repaint continuously.
                  运行态用静态圆点 + 微光晕，异常/启动过渡态才用 pulse 提醒（少例不常驻）。 */}
              <span
                className={cn(
                  'relative inline-flex h-2.5 w-2.5 rounded-full',
                  showEngineError
                    ? 'bg-red-500 shadow-[0_0_8px_hsl(0_72%_51%/0.6)] animate-pulse'
                    : isStarting || isStopping
                      ? 'bg-amber-400 shadow-[0_0_8px_hsl(38_92%_50%/0.5)] animate-pulse'
                      : isRunning
                        ? 'bg-emerald-400 shadow-[0_0_6px_hsl(152_60%_45%/0.5)]'
                        : 'bg-slate-500',
                )}
              />
            </span>
            <span className="text-[13px] font-medium text-foreground">
              {showEngineError
                ? t('layout.engineErr')
                : isStarting
                  ? t('layout.startingProxy')
                  : isStopping
                    ? t('layout.stoppingProxy')
                    : isRunning
                      ? t('common.running')
                      : t('common.stopped')}
            </span>
          </div>

          {/* 页面标题（从路由映射，填充顶栏空白） */}
          <div className="ml-2 hidden items-baseline gap-2.5 sm:flex">
            <h1 className="text-[15px] font-bold tracking-tight">{pageTitle}</h1>
          </div>

          {/* fallback 徽章 */}
          {status && !isRunning && status.fallback_mode && (
            <span className="rounded-full border border-amber-500/30 bg-amber-500/10 px-2.5 py-0.5 text-[11px] font-medium text-amber-600 dark:text-amber-400">
              {t('layout.fallbackPrefix')}
              {status.fallback_mode === 'passthrough'
                ? t('layout.fallbackPass')
                : status.fallback_mode === 'error'
                  ? t('layout.fallback503')
                  : t('layout.fallbackBlock')}
            </span>
          )}

          <div className="ml-auto flex min-w-0 items-center gap-1 sm:gap-2">
            <PlaintextSwitch />
            <Button
              size="sm"
              variant="ghost"
              onClick={() => setLang(lang === 'zh' ? 'en' : 'zh')}
              title={lang === 'zh' ? t('layout.switchToEnglish') : t('layout.switchToChinese')}
              className="h-8 gap-1.5 px-2 text-xs font-medium text-muted-foreground hover:text-foreground"
            >
              <Languages className="h-3.5 w-3.5" />
              <span className="hidden sm:inline">{lang === 'zh' ? 'EN' : '中'}</span>
            </Button>
            <Button size="sm" variant="ghost" onClick={toggle} title={t('layout.toggleTheme')} className="h-8 w-8 px-0">
              {dark ? <Sun className="h-4 w-4" /> : <Moon className="h-4 w-4" />}
            </Button>
            <Button
              size="sm"
              variant="ghost"
              onClick={async () => {
                try { await silencerFetch('/api/auth/logout', { method: 'POST', noToken: true }) } finally { setAuth({ initialized: true, authenticated: false, username }) }
              }}
              title={t('layout.logout')}
              className="h-8 gap-1.5 px-2 text-xs text-muted-foreground hover:text-foreground"
            >
              <LogOut className="h-3.5 w-3.5" />
              <span className="hidden xl:inline">{username || t('layout.logout')}</span>
            </Button>
            <Button
              size="sm"
              variant={isRunning ? 'destructive' : 'default'}
              onClick={toggleProxy}
              disabled={!engineAvailable || isStarting || isStopping}
              loading={proxyBusy || isStarting || isStopping}
              className="h-8 gap-1.5 px-2.5 text-[12px] font-medium sm:px-3.5 sm:text-[13px]"
            >
              {!(proxyBusy || isStarting || isStopping) && (isRunning ? <Square className="h-3.5 w-3.5" /> : <Play className="h-3.5 w-3.5" />)}
              {isStarting
                ? t('layout.startingProxy')
                : isStopping
                  ? t('layout.stoppingProxy')
                  : isRunning
                    ? t('layout.stopProxy')
                    : t('layout.startProxy')}
            </Button>
          </div>
        </header>

        {originBlocked && (
          <div className="flex items-center justify-between border-b border-amber-500/40 bg-amber-500/15 px-4 py-2 text-xs text-amber-800 dark:text-amber-200">
            <div className="flex items-center gap-2">
              <AlertTriangle className="h-4 w-4 shrink-0 text-amber-600 dark:text-amber-400" />
              <span>{t('layout.originRejectedBanner')}</span>
            </div>
            <div className="flex items-center gap-2">
              <Button
                size="sm"
                variant="outline"
                className="h-6 border-amber-500/50 text-[11px] hover:bg-amber-500/20"
                onClick={handleRescueOrigin}
                disabled={rescuingOrigin}
              >
                {rescuingOrigin ? t('common.loading') : t('layout.disableOriginCheckNow')}
              </Button>
              <button
                type="button"
                onClick={() => setOriginBlocked(false)}
                className="text-muted-foreground hover:text-foreground"
              >
                <X className="h-3.5 w-3.5" />
              </button>
            </div>
          </div>
        )}

        {/* 内容区：Logs 页由自身定高容器进行虚拟滚动，其余页面由全宽 main 提供贴边自然滚动 */}
        <main
          className={cn(
            'min-h-0 flex-1',
            isLogsPage ? 'overflow-hidden' : 'overflow-y-auto'
          )}
        >
          <div
            key={routeLocation.pathname}
            className={cn(
              'animate-page-enter mx-auto w-full p-3 sm:p-5 lg:p-8',
              isWidePage ? 'max-w-[1536px]' : 'max-w-6xl',
              isLogsPage ? 'h-full overflow-hidden flex flex-col' : 'min-h-full'
            )}
          >
            {statusError && (
              <div className="mb-4 flex items-center gap-2 rounded-lg border border-red-500/40 bg-red-500/10 px-3 py-2 text-xs text-red-600 dark:text-red-400">
                <AlertCircle className="h-3.5 w-3.5 shrink-0" />
                {tf('layout.connFail', { err: String(statusError).slice(0, 120) })}
              </div>
            )}
            <ErrorBoundary>
              {children}
            </ErrorBoundary>
          </div>
        </main>
      </div>
    </div>
  )
}
