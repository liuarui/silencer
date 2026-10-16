import { usePrivacyStore, privateLogSearch } from '@/stores/privacyStore'
import { PlaintextSwitch } from '@/components/common/PlaintextSwitch'
import { displayOriginal, displayPreview } from '@/lib/privacy'
/**
 * 控制台：
 * - 服务状态横幅：脱敏状态 + 转发方式 + 运行时长
 * - 6 张统计卡：今日请求 / 已脱敏 / 已还原回复 / 告警(4子项) / Token(输入输出) / 今日费用估算
 * - 请求改写概况：原样转发比例 / 占位符复用率 / 首次改写位置
 * - 服务快捷操作：启停 / CA 证书 / 健康检查 / 紧急恢复
 * - 防护设置：脱敏失败策略 / 响应扫描 / 自启代理
 * - 最近事件列表
 * - 向导横幅 / 自动恢复横幅 / 引擎错误横幅
 */
import { useMemo, useState } from 'react'
import { useVisibility } from '@/lib/useVisibility'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate } from 'react-router-dom'
import {
  ShieldCheck,
  ShieldOff,
  Route,
  ScanText,
  Replace,
  TriangleAlert,
  Cpu,
  CircleDollarSign,
  TextSearch,
  AlertCircle,
  AlertTriangle,
  Globe,
  X,
  Clock,
  ArrowRight,
  HelpCircle,
  Loader2,
  Database,
} from 'lucide-react'
import { dismissAutoRecover } from '@/api/proxy'
import { getTodayStats, getConfig, saveConfig, getHealth, getStatsModels } from '@/api/settings'
import { getLogs } from '@/api/logs'
import { getMatchedRules, mergeMaskRestore, getDisplayEventType } from '@/lib/log-events'
import { EventTypeIcon, EVENT_TYPE_META } from '@/components/events/EventTypeIcon'
import { EventDetailDialog } from '@/components/events/EventDetailDialog'
import { Dialog, DialogContent, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Card, CardContent } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Switch } from '@/components/ui/switch'
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from '@/components/ui/tooltip'
import { Table, TableHeader, TableBody, TableRow, TableHead, TableCell } from '@/components/ui/table'
import { cn, formatCompactNumber, formatTokensShort, formatBytes, formatPercent } from '@/lib/utils'
import { groupWordsByIngress, ingressQuery } from '@/lib/ingress-groups'
import { toast } from '@/lib/toast'
import { useI18n } from '@/lib/i18n'
import dayjs from 'dayjs'
import { useProxyStatus } from '@/features/proxy/hooks/useProxyStatus'


/** 运行时长格式化：秒 → HH:MM:SS */
function formatUptime(sec: number): string {
  const h = Math.floor(sec / 3600)
  const m = Math.floor((sec % 3600) / 60)
  const s = Math.floor(sec % 60)
  return [h, m, s].map((n) => String(n).padStart(2, '0')).join(':')
}

/** 耗时格式统一：<1s 毫秒；<60s 秒(1 位小数)；≥60s 分:秒（与 Logs 页完全对齐） */
const fmtMs = (ms?: number | null) => {
  if (ms == null) return '—'
  if (ms < 1000) return `${Math.round(ms)}ms`
  const s = ms / 1000
  if (s < 60) return `${s.toFixed(1)}s`
  const m = Math.floor(s / 60)
  return `${m}m${Math.round(s % 60)}s`
}

function renderStatValue(v?: string | null) {
  if (typeof v === 'string' && v.startsWith('$')) {
    return (
      <>
        <span className="mr-0.5 text-[20px] font-normal text-muted-foreground/80">$</span>
        <span>{v.slice(1)}</span>
      </>
    )
  }
  return v ?? '—'
}

export default function Dashboard() {
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const { t, tf, lang } = useI18n()
  // 页面隐藏时停止轮询；可见时自动刷新
  const { hidden } = useVisibility()

  const { data: status } = useProxyStatus(hidden)

  const isStarting = (status === undefined) || !!status?.proxy_starting
  const isStopping = !!status?.proxy_stopping
  const [statsRange, setStatsRange] = useState<'today' | '7d' | '30d'>('today')
  const { data: stats, isError: statsError } = useQuery({
    queryKey: ['todayStats', statsRange],
    queryFn: () => getTodayStats(statsRange === 'today' ? undefined : statsRange),
    refetchInterval: hidden ? false : 10000,
  })
  const { data: cfg } = useQuery({ queryKey: ['config'], queryFn: getConfig, refetchInterval: hidden ? false : 120000 })

  // 写队列健康（drops/dead_letters/restarts > 0 时显示告警横幅）
  const { data: health, isError: healthQueryError } = useQuery({
    queryKey: ['health'],
    queryFn: getHealth,
    refetchInterval: hidden ? false : 60000,
  })
  const storageHealth = health?.storage
  const storageQueueDepth = storageHealth?.queue_depth ?? 0
  const storageQueueCapacity = storageHealth?.queue_capacity ?? 0
  const storageQueueHigh = storageQueueCapacity > 0 && storageQueueDepth >= storageQueueCapacity * 0.8
  const hasStorageIssues = healthQueryError || (health !== undefined && (
    !health.ok ||
    Boolean(health.storage_error) ||
    Boolean(storageHealth?.last_write_error) ||
    storageQueueHigh
  ))

  const running = status?.proxy_running ?? false
  const filterOn = status?.filter_enabled ?? true

  const { data: recentLogs } = useQuery({
    queryKey: ['recentLogs'],
    // 需拉足量原始事件再合并 MASK/RESTORE：若只拉 limit=10 条原始事件，
    // 合并成「一次请求一行」后可能只剩 5 行。拉 40 条再合并取最新 10 行，
    // 与 Logs 页口径一致（同 sid 链路合成一行、最新 ts 在前）。
    queryFn: () => getLogs({ limit: 40, latest: true, chains: true, slim: true }),
    refetchInterval: hidden ? false : 5000,
  })

  // 今日费用估算（数据源 /api/stats/models?days=1，价格估算非真实账单）
  const { data: costData } = useQuery({
    queryKey: ['statsModels', '1'],
    queryFn: () => getStatsModels(1),
    refetchInterval: hidden ? false : 60000,
  })

  const showPlaintext = usePrivacyStore((state) => state.showPlaintext)
  const [maskedOpen, setMaskedOpen] = useState(false)
  const [restoredOpen, setRestoredOpen] = useState(false)
  // 逐词原文默认打码，开启显示明文后可核对。
  // 行点击详情弹窗（复用 Logs 页的 EventDetailDialog）
  const [detailSeq, setDetailSeq] = useState<number | string | null>(null)
  const [detailOpen, setDetailOpen] = useState(false)


  const dismiss = async () => {
    try {
      await dismissAutoRecover()
      queryClient.invalidateQueries({ queryKey: ['proxyStatus'] })
    } catch {
      // 忽略
    }
  }

  const toggle = async (key: string, v: boolean) => {
    if (!cfg) return
    try {
      const r = await saveConfig({ [key]: v })
      if (!r.ok) toast(r.error || t('common.saveFail'), 'error')
      else toast(t('common.saved'))
      queryClient.invalidateQueries({ queryKey: ['config'] })
      queryClient.invalidateQueries({ queryKey: ['proxyStatus'] })
    } catch (e) {
      toast(tf('common.saveFailWith', { e: String(e) }), 'error')
    }
  }

  const dismissWizard = async () => {
    if (!cfg) return
    try {
      await saveConfig({ wizard_done: true })
      queryClient.invalidateQueries({ queryKey: ['proxyStatus'] })
    } catch {
      // 忽略
    }
  }

  const tokensTotal = (stats?.tokens.prompt ?? 0) + (stats?.tokens.completion ?? 0)

  // 前缀保真度：null = 本区间一条 MASK 事件都没有（空库 / 老库刚升级）。
  // 与「有样本但零改写率 0%」必须分开渲染——前者是没数据，后者是数据很差。
  const prefix = stats?.prefix ?? null

  // 最近事件：合并 MASK/RESTORE 成「一次请求一行」并取最新 10 行（与 Logs 页同口径）
  const recentMerged = useMemo(
    () => mergeMaskRestore(recentLogs?.events ?? []).slice(0, 10),
    [recentLogs?.events],
  )

  // 今日费用估算：读 /api/stats/models?days=1（today 自然日），只计已定价模型
  const todayCostModels = costData?.models ?? []
  const todayCost = todayCostModels.reduce((s, m) => s + (m.priced ? (m.cost_usd ?? 0) : 0), 0)
  const todayPricedCount = todayCostModels.filter((m) => m.priced).length
  // 有请求但一个模型都没定价 → 费用估算不可用（不展示假 0 账单）
  const todayCostUnavailable =
    todayCostModels.length > 0 && todayPricedCount === 0
  const todayCostReady = Boolean(costData)

  const unitSystem = t('stats.unitSystem') === 'si' ? 'si' : 'cjk'
  const reqNum = formatCompactNumber(stats?.requests ?? 0, unitSystem)
  const maskNum = formatCompactNumber(stats?.mask_events ?? 0, unitSystem)
  const restoreNum = formatCompactNumber(stats?.restored ?? 0, unitSystem)
  const alertNum = formatCompactNumber(stats?.alerts ?? 0, unitSystem)
  const tokenNum = formatCompactNumber(tokensTotal, unitSystem)
  const restoreExplanation = tf('dash.restoreExplanation', {
    restored: stats?.restore_by_status?.restored ?? stats?.restored ?? 0,
    noSensitive: stats?.restore_by_status?.no_sensitive_data ?? 0,
    noEcho: stats?.restore_by_status?.no_placeholder_in_response ?? 0,
    failed: stats?.restore_failed ?? 0,
  })

  // 6 张统计卡（i18n）
  const statCards = [
    {
      label: t('dash.reqToday'),
      value: reqNum.compact,
      fullValue: reqNum.full,
      isCompact: reqNum.isCompact,
      hint: t('dash.reqHint'),
      icon: Route, num: 'text-blue-600 dark:text-blue-400',
      iconBg: 'bg-blue-500/10 text-blue-600 dark:text-blue-400',
      to: '/logs',
    },
    {
      label: t('dash.maskedReq'),
      value: maskNum.compact,
      fullValue: maskNum.full,
      isCompact: maskNum.isCompact,
      hint: `${t('dash.maskedHint')} ${formatTokensShort(stats?.masked_items ?? 0, unitSystem)}`,
      hintTitle: `${t('dash.maskedHint')} ${(stats?.masked_items ?? 0).toLocaleString()}`,
      icon: ScanText, num: 'text-emerald-600 dark:text-emerald-400',
      iconBg: 'bg-emerald-500/10 text-emerald-600 dark:text-emerald-400',
      onClick: () => setMaskedOpen(true),
    },
    {
      label: t('dash.restored'),
      value: stats ? restoreNum.compact : '—',
      fullValue: restoreNum.full,
      isCompact: restoreNum.isCompact,
      hint: statsError ? t('dash.statsLoadFailed') : !stats ? t('common.loading')
        : stats.restored === 0 ? t('dash.noRestoredReplies') : t('dash.restoredHint'),
      hintTitle: restoreExplanation,
      icon: Replace, num: 'text-emerald-600 dark:text-emerald-400',
      iconBg: 'bg-teal-500/10 text-teal-600 dark:text-teal-400',
      onClick: () => setRestoredOpen(true),
    },
    {
      label: t('dash.alerts'),
      value: alertNum.compact,
      fullValue: alertNum.full,
      isCompact: alertNum.isCompact,
      sub: [
        { label: t('dash.blocked'), value: stats?.alert_counts?.block ?? 0 },
        { label: t('dash.restoreFail'), value: stats?.alert_counts?.restore_failed ?? 0 },
        { label: t('dash.errors'), value: stats?.alert_counts?.error ?? 0 },
        { label: t('dash.respWarn'), value: stats?.alert_counts?.response_warning ?? 0 },
      ],
      icon: TriangleAlert, num: 'text-amber-600 dark:text-amber-400',
      iconBg: 'bg-amber-500/10 text-amber-600 dark:text-amber-400',
      to: '/logs?request_outcome=abnormal',
    },
    {
      label: t('dash.tokenToday'),
      value: tokenNum.compact,
      fullValue: tokenNum.full,
      isCompact: tokenNum.isCompact,
      hint: `${t('dash.tokenHint')} ${formatTokensShort(stats?.tokens.prompt ?? 0, unitSystem)} · ${formatTokensShort(stats?.tokens.completion ?? 0, unitSystem)}`,
      hintTitle: `${t('dash.tokenHint')} ${(stats?.tokens.prompt ?? 0).toLocaleString()} · ${(stats?.tokens.completion ?? 0).toLocaleString()}`,
      icon: Cpu, num: 'text-violet-600 dark:text-violet-400',
      iconBg: 'bg-violet-500/10 text-violet-600 dark:text-violet-400',
      to: '/stats',
    },
    {
      // 今日费用估算：按模型价格 × 今日 token 用量估算，未定价模型不计入；
      // 请求加载中显示破折号，有请求但全部未定价时提示估算不可用（不展示假 0 账单）。
      label: t('dash.costToday'),
      value: !todayCostReady || todayCostUnavailable ? '—' : `$${Number(todayCost || 0).toFixed(4)}`,
      hint: !todayCostReady ? t('common.loading') : todayCostUnavailable ? t('dash.costUnavailable') : t('dash.costHint'),
      icon: CircleDollarSign, num: 'text-emerald-600 dark:text-emerald-400',
      iconBg: 'bg-emerald-500/10 text-emerald-600 dark:text-emerald-400',
      to: '/stats',
    },
  ]

  return (
    <div className="space-y-5">
      <h1 className="sr-only">{t('nav.dashboard')}</h1>
      {/* ===== 服务状态 ===== */}
      <div
        className={cn(
          'flex flex-wrap items-center gap-4 rounded-xl border p-5 transition-colors duration-200',
          isStarting || isStopping
            ? 'border-amber-500/25 bg-amber-500/[0.04]'
            : running && filterOn
              ? 'border-emerald-500/25 bg-emerald-500/[0.04]'
              : running
                ? 'border-amber-500/25 bg-amber-500/[0.04]'
                : 'border-border bg-muted/20',
        )}
      >
        <div
          className={cn(
            'flex h-10 w-10 shrink-0 items-center justify-center rounded-lg',
            isStarting || isStopping
              ? 'bg-amber-500/10 text-amber-600 dark:text-amber-400'
              : running && filterOn
                ? 'bg-emerald-500/10 text-emerald-600 dark:text-emerald-400'
                : running
                  ? 'bg-amber-500/10 text-amber-600 dark:text-amber-400'
                  : 'bg-muted text-muted-foreground',
          )}
        >
          {isStarting || isStopping ? (
            <Loader2 className="h-[18px] w-[18px] animate-spin" strokeWidth={1.8} />
          ) : running && filterOn ? (
            <ShieldCheck className="h-[18px] w-[18px]" strokeWidth={1.8} />
          ) : (
            <ShieldOff className="h-[18px] w-[18px]" strokeWidth={1.8} />
          )}
        </div>
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <span className="text-lg font-bold tracking-tight">
              {isStarting
                ? t('dash.startingTitle')
                : isStopping
                  ? t('layout.stoppingProxy')
                  : !running
                    ? t('dash.proxyStoppedTitle')
                    : filterOn
                      ? t('dash.filterOn')
                      : t('dash.filterOff')}
            </span>
            {running && !isStarting && (
              <span className="rounded-full border border-emerald-500/30 bg-emerald-500/10 px-2.5 py-0.5 text-xs font-medium text-emerald-600 dark:text-emerald-400">
                {t(`dash.captureMode.${status?.capture_mode ?? 'reverse'}`)}
              </span>
            )}
          </div>
          <p className="mt-1 text-[13px] text-muted-foreground">
            {isStarting
              ? t('dash.startingDesc')
              : running
                ? t('dash.maskingDesc')
                : status?.fallback_mode === 'passthrough'
                  ? t('dash.passthroughDesc')
                  : status?.fallback_mode === 'error'
                    ? t('dash.proxy503')
                    : t('dash.proxyStopped')}
          </p>
        </div>
        <div className="flex items-center gap-6 text-[13px] text-muted-foreground">
          <div className="text-right">
            <div className="flex items-center gap-1 text-base font-semibold tabular-nums text-foreground">
              <Clock className="h-3.5 w-3.5" />
              {isStarting ? t('dash.starting') : running ? formatUptime(status?.uptime ?? 0) : '—'}
            </div>
            <div className="text-xs">{t('dash.uptime')}</div>
          </div>
        </div>
      </div>

      {/* 引擎错误横幅 */}
      {status?.last_error && !status.auto_recover_fail && (
        <div className="flex items-center gap-2 rounded-xl border border-red-500/30 bg-red-500/10 px-4 py-3 text-sm text-red-600 dark:text-red-400">
          <AlertCircle className="h-4 w-4 shrink-0" />
          <span className="min-w-0 flex-1 truncate">{t('dash.engineError')}{status.last_error}</span>
        </div>
      )}

      {/* 自动恢复横幅 */}
      {status?.auto_recovered_at && !status.auto_recover_fail && (
        <div className="flex items-center gap-2 rounded-xl border border-emerald-500/30 bg-emerald-500/10 px-4 py-3 text-sm text-emerald-600 dark:text-emerald-400">
          <ShieldCheck className="h-4 w-4 shrink-0" />
          <span>{tf('dash.autoRecovered', { time: status.auto_recovered_at ? dayjs(status.auto_recovered_at).format('HH:mm:ss') : '—' })}</span>
          <Button size="icon" variant="ghost" className="ml-auto h-6 w-6 shrink-0" onClick={dismiss}>
            <X className="h-3.5 w-3.5" />
          </Button>
        </div>
      )}
      {status?.auto_recover_fail && (
        <div className="flex items-center gap-2 rounded-xl border border-red-500/30 bg-red-500/10 px-4 py-3 text-sm text-red-600 dark:text-red-400">
          <AlertCircle className="h-4 w-4 shrink-0" />
          <span className="min-w-0 flex-1 truncate">{t('dash.autoRecoverFail')}{status.auto_recover_fail}</span>
          <Button size="icon" variant="ghost" className="ml-auto h-6 w-6 shrink-0" onClick={dismiss}>
            <X className="h-3.5 w-3.5" />
          </Button>
        </div>
      )}

      {/* 向导横幅 */}
      {status?.wizard_recommended && (
        <div className="flex items-center gap-3 rounded-xl border bg-gradient-to-r from-primary/10 to-transparent p-4 backdrop-blur-md">
          <div className="flex h-9 w-9 shrink-0 items-center justify-center rounded-xl bg-primary/15 font-bold text-primary">1</div>
          <div className="min-w-0 flex-1 text-sm">
            <div className="font-semibold">{t('dash.wizardTitle')}</div>
            <div className="mt-0.5 text-xs text-muted-foreground">
              {t('dash.wizardSteps')}
            </div>
          </div>
          <Link to="/clients" className="shrink-0 rounded-lg border border-input bg-secondary px-3 py-1.5 text-xs font-medium hover:bg-accent">
            {t('dash.goConfig')}
          </Link>
          <Button size="icon" variant="ghost" className="h-7 w-7 shrink-0" onClick={dismissWizard}><X className="h-4 w-4" /></Button>
        </div>
      )}

      {/* 存储异常、写入错误或队列接近容量上限时显示 */}
      {hasStorageIssues && (
        <div className="flex items-center gap-2 rounded-xl border border-amber-500/40 bg-amber-500/10 p-3 text-xs">
          <AlertTriangle className="h-4 w-4 shrink-0 text-amber-600 dark:text-amber-400" />
          <span className="text-amber-700 dark:text-amber-300">
            {healthQueryError
              ? t('dash.healthCheckFailed')
              : (!health?.ok || health?.storage_error ? t('dash.storageUnavailable') : '')}
            {storageHealth?.last_write_error ? ` / ${t('dash.storageWriteError')}` : ''}
            {storageQueueHigh ? ` / ${tf('dash.storageQueueHigh', { queue: storageQueueDepth, capacity: storageQueueCapacity })}` : ''}
          </span>
        </div>
      )}

      {/* ===== 统计卡 ===== */}
      <div className="mb-1 flex items-center justify-between">
        <h2 className="text-sm font-semibold text-muted-foreground">
          {statsRange === 'today' ? t('dash.statsToday') : statsRange === '7d' ? t('dash.stats7d') : t('dash.stats30d')}
        </h2>
        <div className="flex items-center gap-1 rounded-lg bg-muted/60 p-0.5 text-xs">
          {([['today', t('dash.today')], ['7d', t('dash.7d')], ['30d', t('dash.30d')]] as const).map(([k, label]) => (
            <button
              key={k}
              type="button"
              onClick={() => setStatsRange(k)}
              className={cn('rounded-md px-2.5 py-1 font-medium transition-colors', statsRange === k ? 'bg-background text-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground')}
            >
              {label}
            </button>
          ))}
        </div>
      </div>
      {/* 6 卡：2-3 列为主，配合 max-w-[1200px] 容器保持 3 列两行优雅呈现 */}
      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
        {statCards.map((c) => {
          const interactive = !!(c.to || c.onClick)
          const card = (
            <Card
              className={cn(
                'h-full border bg-card shadow-[var(--shadow-card)] transition-[transform,box-shadow,border-color] duration-200',
                // hover 效果只给真能点的卡。Token 卡没有去处，给它抬起+手型
                // 等于骗一次点击——之前 to:'/logs' 也从没接到路由，
                // 三张卡看着可点实际点了没反应
                interactive &&
                  'cursor-pointer hover:-translate-y-0.5 hover:border-primary/30 hover:bg-accent/40 hover:shadow-[var(--shadow-card-hover)]',
              )}
              {...(c.onClick ? { onClick: c.onClick } : {})}
            >
              <CardContent className="flex h-full flex-col p-5">
                {/* 标题与数值各占一行：标题行只放标题和图标，数值行左对齐顶格，
                    5 张卡的数值基线才能横向对齐（标题长短不一时尤其明显） */}
                <div className="flex items-start justify-between gap-2">
                  <span className="min-w-0 flex-1 text-[13px] font-semibold leading-5 text-foreground/80">
                    {c.label}
                  </span>
                  <div className={cn('flex h-9 w-9 shrink-0 items-center justify-center rounded-lg', c.iconBg)}>
                    <c.icon className="h-[17px] w-[17px]" strokeWidth={1.8} />
                  </div>
                </div>
                <div
                  className={cn('mt-2.5 mb-2 text-[28px] font-bold leading-tight tabular-nums tracking-tight truncate', c.num)}
                  title={c.fullValue || c.value}
                >
                  {c.isCompact ? (
                    <TooltipProvider delayDuration={150}>
                      <Tooltip>
                        <TooltipTrigger asChild className="cursor-default">
                          <span>{renderStatValue(c.value)}</span>
                        </TooltipTrigger>
                        <TooltipContent>
                          <p className="font-mono text-xs">{c.fullValue}</p>
                        </TooltipContent>
                      </Tooltip>
                    </TooltipProvider>
                  ) : (
                    renderStatValue(c.value)
                  )}
                </div>
                {c.sub ? (
                  <div className="mt-auto grid grid-cols-2 gap-x-2 gap-y-0.5 border-t pt-2 text-xs text-muted-foreground" style={{ minHeight: 44 }}>
                    {c.sub.map((s) => (
                      <span key={s.label} className="truncate">
                        {s.label} <b className="font-medium tabular-nums text-foreground">{s.value}</b>
                      </span>
                    ))}
                  </div>
                ) : (
                  <p className="mt-auto border-t pt-2 text-[11px] font-medium text-muted-foreground/80 truncate" style={{ minHeight: 44 }} title={c.hintTitle || c.hint}>{c.hint}</p>
                )}
              </CardContent>
            </Card>
          )
          return c.to
            ? <Link key={c.label} to={c.to} className="block h-full">{card}</Link>
            : <div key={c.label} className="h-full">{card}</div>
        })}
      </div>

      {/* 上游 usage 返回的实际 Context Cache 数据，与本地改写诊断分开展示。 */}
      <Card className="border bg-card shadow-[var(--shadow-card)]">
        <CardContent className="p-5">
          <div className="flex flex-wrap items-center gap-3">
            <div className="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg bg-emerald-500/10 text-emerald-600 dark:text-emerald-400">
              <Database className="h-[17px] w-[17px]" strokeWidth={1.8} />
            </div>
            <div className="min-w-0 flex-1">
              <div className="text-[13px] font-semibold leading-5 text-foreground/80">{t('dash.upstreamCache')}</div>
              <div className="text-[11px] font-medium text-muted-foreground/80">{t('dash.upstreamCacheHint')}</div>
            </div>
            <span className="shrink-0 rounded-full border border-emerald-500/30 bg-emerald-500/10 px-2.5 py-0.5 text-[11px] font-medium text-emerald-600 dark:text-emerald-400">
              {stats?.cache?.observed_requests
                ? tf('dash.cacheSamples', { n: stats.cache.observed_requests.toLocaleString() })
                : t('dash.cacheNoSamples')}
            </span>
          </div>
          <div className="mt-4 grid grid-cols-1 gap-3 border-t pt-3 sm:grid-cols-3">
            <div className="min-w-0">
              <div className="text-[11px] font-medium text-muted-foreground">{t('dash.cacheHitRate')}</div>
              <div className="text-[20px] font-bold leading-tight tabular-nums text-emerald-600 dark:text-emerald-400">
                {stats?.cache?.hit_rate != null ? formatPercent(stats.cache.hit_rate, 1) : '—'}
              </div>
              <div className="text-[10px] text-muted-foreground/80">{t('dash.cacheRateHint')}</div>
              <div className="truncate text-[11px] text-muted-foreground/80">
                {stats?.cache?.rate_observed_requests
                  ? tf('dash.cacheRateSamples', { n: stats.cache.rate_observed_requests.toLocaleString() })
                  : t('dash.cacheNoRate')}
              </div>
            </div>
            <div className="min-w-0">
              <div className="text-[11px] font-medium text-muted-foreground">{t('dash.cacheRead')}</div>
              <div className="text-[20px] font-bold leading-tight tabular-nums">{formatTokensShort(stats?.cache?.read_tokens ?? 0, unitSystem)}</div>
              <div className="truncate text-[11px] text-muted-foreground/80" title={(stats?.cache?.read_tokens ?? 0).toLocaleString()}>
                {(stats?.cache?.read_tokens ?? 0).toLocaleString()} {t('dash.tokensUnit')}
              </div>
            </div>
            <div className="min-w-0">
              <div className="text-[11px] font-medium text-muted-foreground">{t('dash.cacheWrite')}</div>
              <div className="text-[20px] font-bold leading-tight tabular-nums">{formatTokensShort(stats?.cache?.write_tokens ?? 0, unitSystem)}</div>
              <div className="truncate text-[11px] text-muted-foreground/80" title={(stats?.cache?.write_tokens ?? 0).toLocaleString()}>
                {(stats?.cache?.write_tokens ?? 0).toLocaleString()} {t('dash.tokensUnit')}
              </div>
            </div>
          </div>
        </CardContent>
      </Card>

      {/* 请求改写概况：聚合 MASK 事件的 body_rewritten / first_diff_byte /
          suffix_reused 字段，帮助判断请求前缀和占位符复用情况；这些数据不等同于
          上游缓存命中率。刻意**不塞进上面那个 6 卡网格**——它的列数
          （2/3/6）是按 6 张卡调过的，第 7 张在 ≥1536px 宽屏上会单独落一行。
          第一列分母是 masks（全部样本）；第二、三列分母是 rewritten（改写过的
          请求）——`suffix_reused` 与 `first_diff_byte` 都只在回写分支才有意义，
          零改写透传的请求没签发票据、也没有差异位，用 masks 当分母会把指标
          稀释成「复用机制没生效」（实测 100 次请求 10 次命中、8 次复用，
          用 masks 显示 8%，真实是 80%）。
          `prefix` 为 null 时按 mask_events 区分两种「没有数据」：区间内压根没
          请求 vs 有请求但都早于该统计上线（升级当天就是后者，不能说成「脱敏
          没生效」）。 */}
      <Card className="border bg-card shadow-[var(--shadow-card)]">
        <CardContent className="p-5">
          <div className="flex flex-wrap items-center gap-3">
            <div className="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg bg-sky-500/10 text-sky-600 dark:text-sky-400">
              <TextSearch className="h-[17px] w-[17px]" strokeWidth={1.8} />
            </div>
            <div className="min-w-0 flex-1">
              <div className="flex items-center gap-1.5 text-[13px] font-semibold leading-5 text-foreground/80">
                <span>{t('dash.prefixFidelity')}</span>
                <TooltipProvider delayDuration={150}>
                  <Tooltip>
                    <TooltipTrigger asChild>
                      <HelpCircle className="h-3.5 w-3.5 cursor-help text-muted-foreground/60 hover:text-muted-foreground" />
                    </TooltipTrigger>
                    <TooltipContent side="top" className="max-w-[280px] text-xs leading-relaxed">
                      {t('dash.prefixFidelityTooltip')}
                    </TooltipContent>
                  </Tooltip>
                </TooltipProvider>
              </div>
              <div className="text-[11px] font-medium text-muted-foreground/80">{t('dash.prefixHint')}</div>
            </div>
            {prefix && (
              <span className="shrink-0 rounded-full border border-sky-500/30 bg-sky-500/10 px-2.5 py-0.5 text-[11px] font-medium text-sky-600 dark:text-sky-400">
                {tf('dash.prefixSamples', { n: prefix.masks.toLocaleString() })}
              </span>
            )}
          </div>
          <div className="mt-4 grid grid-cols-1 gap-3 border-t pt-3 sm:grid-cols-3">
            <div className="min-w-0">
              <div className="flex items-center gap-1 text-[11px] font-medium text-muted-foreground">
                <span className="truncate">{t('dash.prefixClean')}</span>
                <TooltipProvider delayDuration={150}>
                  <Tooltip>
                    <TooltipTrigger asChild>
                      <HelpCircle className="h-3 w-3 shrink-0 cursor-help text-muted-foreground/50 hover:text-muted-foreground" />
                    </TooltipTrigger>
                    <TooltipContent side="top" className="max-w-[260px] text-xs leading-relaxed">
                      {t('dash.prefixCleanTooltip')}
                    </TooltipContent>
                  </Tooltip>
                </TooltipProvider>
              </div>
              <div className="text-[20px] font-bold leading-tight tabular-nums text-sky-600 dark:text-sky-400">
                {prefix ? formatPercent(prefix.clean_rate, 1, { isNotFull: prefix.clean < prefix.masks }) : '—'}
              </div>
              <div className="truncate text-[11px] text-muted-foreground/80">
                {prefix
                  ? `${prefix.clean.toLocaleString()} / ${prefix.masks.toLocaleString()}`
                  : (stats?.mask_events ?? 0) > 0
                    ? t('dash.prefixNoSamples')
                    : t('dash.prefixNoData')}
              </div>
            </div>
            <div className="min-w-0">
              <div className="flex items-center gap-1 text-[11px] font-medium text-muted-foreground">
                <span className="truncate">{t('dash.prefixReuse')}</span>
                <TooltipProvider delayDuration={150}>
                  <Tooltip>
                    <TooltipTrigger asChild>
                      <HelpCircle className="h-3 w-3 shrink-0 cursor-help text-muted-foreground/50 hover:text-muted-foreground" />
                    </TooltipTrigger>
                    <TooltipContent side="top" className="max-w-[260px] text-xs leading-relaxed">
                      {t('dash.prefixReuseTooltip')}
                    </TooltipContent>
                  </Tooltip>
                </TooltipProvider>
              </div>
              <div className="text-[20px] font-bold leading-tight tabular-nums text-emerald-600 dark:text-emerald-400">
                {prefix?.reuse_rate != null
                  ? formatPercent(prefix.reuse_rate, 1, {
                      isNotFull: prefix.rewritten > 0 && prefix.suffix_reused < prefix.rewritten,
                    })
                  : '—'}
              </div>
              <div className="truncate text-[11px] text-muted-foreground/80">
                {prefix
                  ? prefix.rewritten > 0
                    ? `${prefix.suffix_reused.toLocaleString()} / ${prefix.rewritten.toLocaleString()}`
                    : t('dash.prefixNoPlaceholder')
                  : '—'}
              </div>
            </div>
            <div className="min-w-0">
              <div className="flex items-center gap-1 text-[11px] font-medium text-muted-foreground">
                <span className="truncate">{t('dash.prefixDiff')}</span>
                <TooltipProvider delayDuration={150}>
                  <Tooltip>
                    <TooltipTrigger asChild>
                      <HelpCircle className="h-3 w-3 shrink-0 cursor-help text-muted-foreground/50 hover:text-muted-foreground" />
                    </TooltipTrigger>
                    <TooltipContent side="top" className="max-w-[260px] text-xs leading-relaxed">
                      <div>{t('dash.prefixDiffTooltip')}</div>
                      {prefix?.avg_first_diff != null && (
                        <div className="mt-1 border-t border-border/50 pt-1 text-[10px] text-muted-foreground">
                          {tf('dash.prefixDiffExact', { n: prefix.avg_first_diff.toLocaleString() })}
                        </div>
                      )}
                    </TooltipContent>
                  </Tooltip>
                </TooltipProvider>
              </div>
              <div className="text-[20px] font-bold leading-tight tabular-nums">
                {prefix?.avg_first_diff != null ? formatBytes(prefix.avg_first_diff) : '—'}
              </div>
              <div className="truncate text-[11px] text-muted-foreground/80">
                {prefix
                  ? prefix.rewritten > 0
                    ? `${prefix.diff_samples.toLocaleString()} / ${prefix.rewritten.toLocaleString()}`
                    : t('dash.prefixNoRewrite')
                  : '—'}
              </div>
            </div>
          </div>
        </CardContent>
      </Card>


{/* 防护设置 */}
<Card className="overflow-hidden border bg-card shadow-[var(--shadow-card)]">
          <div className="flex items-center justify-between border-b p-5">
            <h3 className="flex items-center gap-2 text-base font-bold">
              <ShieldCheck className="h-[18px] w-[18px] text-emerald-500" strokeWidth={1.8} />
              {t('dash.security')}
            </h3>
            <span className="flex items-center gap-1.5 rounded-full border border-emerald-500/30 bg-emerald-500/10 px-2.5 py-0.5 text-[11px] font-medium text-emerald-600 dark:text-emerald-400">
              <span className="h-1.5 w-1.5 rounded-full bg-emerald-500" />
              {t('dash.live')}
            </span>
          </div>
          <CardContent className="space-y-3.5 p-5">
            {([
              ['fail_closed', t('dash.failClosed'), t('dash.failClosedDesc')],
              ['response_scan', t('dash.respScan'), t('dash.respScanDesc')],
            ] as [string, string, string][]).map(([k, title, desc]) => (
              <label key={k} className="flex cursor-pointer select-none items-center justify-between border-t pt-3.5 first:border-t-0 first:pt-0 transition-colors hover:text-foreground">
                <div className="flex min-w-0 items-center gap-1.5">
                  <span className="truncate text-sm font-semibold">{title}</span>
                  <TooltipProvider delayDuration={200}>
                    <Tooltip>
                      <TooltipTrigger asChild>
                        <span onClick={(e) => e.stopPropagation()}>
                          <HelpCircle className="h-3.5 w-3.5 shrink-0 cursor-help text-muted-foreground/60" />
                        </span>
                      </TooltipTrigger>
                      <TooltipContent className="max-w-[260px] text-xs">{desc}</TooltipContent>
                    </Tooltip>
                  </TooltipProvider>
                </div>
                <Switch
                  checked={Boolean((status as Record<string, unknown> | undefined)?.[k])}
                  onCheckedChange={(v) => toggle(k, v)}
                  className="scale-90"
                />
              </label>
            ))}
            {([
              ['auto_start_proxy', t('dash.autoStartProxy'), t('dash.autoStartProxyDesc')],
            ] as [string, string, string][]).map(([k, title, desc]) => (
              <label key={k} className="flex cursor-pointer select-none items-center justify-between border-t pt-3.5 transition-colors hover:text-foreground">
                <div className="flex min-w-0 items-center gap-1.5">
                  <span className="truncate text-sm font-semibold">{title}</span>
                  <TooltipProvider delayDuration={200}>
                    <Tooltip>
                      <TooltipTrigger asChild>
                        <span onClick={(e) => e.stopPropagation()}>
                          <HelpCircle className="h-3.5 w-3.5 shrink-0 cursor-help text-muted-foreground/60" />
                        </span>
                      </TooltipTrigger>
                      <TooltipContent className="max-w-[260px] text-xs">{desc}</TooltipContent>
                    </Tooltip>
                  </TooltipProvider>
                </div>
                <Switch
                  checked={Boolean((status as Record<string, unknown> | undefined)?.[k])}
                  onCheckedChange={(v) => toggle(k, v)}
                  className="scale-90"
                />
              </label>
            ))}
          </CardContent>
        </Card>

      {/* ===== 最近事件表格（与 Logs 页列对齐）===== */}
      <Card className="overflow-hidden border bg-card shadow-[var(--shadow-card)]">
        <div className="flex flex-wrap items-center justify-between gap-2 border-b bg-muted/20 p-4 sm:p-5">
          <div className="flex items-center gap-3">
            <h3 className="text-base font-bold">{t('dash.recentLogs')}</h3>
            <span className="rounded-full border bg-secondary px-2 py-0.5 font-mono text-[11px] text-muted-foreground">{t('dash.livePush')}</span>
          </div>
          <Link to="/logs" className="flex items-center gap-1 text-sm font-semibold text-primary hover:underline">
            {t('dash.viewAll')} <ArrowRight className="h-3.5 w-3.5" />
          </Link>
        </div>
        <div className="overflow-x-auto">
          <Table className="w-full text-sm">
            <TableHeader>
              <TableRow className="border-b bg-muted/50">
                <TableHead className="w-[68px] py-2.5 px-4 text-left text-xs font-medium uppercase tracking-wider text-muted-foreground">{t('logs.colTime')}</TableHead>
                <TableHead className="w-[92px] py-2.5 px-4 text-left text-xs font-medium uppercase tracking-wider text-muted-foreground">{t('logs.colResult')}</TableHead>
                <TableHead className="w-[88px] py-2.5 px-4 text-left text-xs font-medium uppercase tracking-wider text-muted-foreground">{t('logs.colUpstream')}</TableHead>
                <TableHead className="min-w-[200px] py-2.5 px-4 text-left text-xs font-medium uppercase tracking-wider text-muted-foreground">{t('logs.colModel')}</TableHead>
                <TableHead className="w-[140px] min-w-[120px] py-2.5 px-4 text-left text-xs font-medium uppercase tracking-wider text-muted-foreground">{t('logs.colSummary')}</TableHead>
                <TableHead className="w-[56px] py-2.5 px-4 text-center text-xs font-medium uppercase tracking-wider text-muted-foreground">{t('logs.colStatus')}</TableHead>
                <TableHead className="w-[58px] py-2.5 px-4 text-right text-xs font-medium uppercase tracking-wider text-muted-foreground">{t('logs.colDuration')}</TableHead>
                <TableHead className="w-[64px] py-2.5 px-4 text-right text-xs font-medium uppercase tracking-wider text-muted-foreground">{t('logs.colCost')}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {recentMerged.map((e) => (
                <TableRow key={e.detail_id ?? e.seq} className="cursor-pointer border-b text-[13px] transition-colors hover:bg-muted/40" onClick={() => { setDetailSeq(e._detailId ?? e._detailSeq ?? e.detail_id ?? e.seq); setDetailOpen(true) }}>
                  <TableCell className="py-2.5 px-4 whitespace-nowrap tabular-nums text-xs text-muted-foreground">
                    <div className="flex flex-col leading-tight">
                      <span>{e.ts ? dayjs(e.ts * 1000).format('MM-DD') : '—'}</span>
                      <span className="opacity-70">{e.ts ? dayjs(e.ts * 1000).format('HH:mm:ss') : ''}</span>
                    </div>
                  </TableCell>
                  <TableCell className="py-2.5 px-4">
                    <span className="flex items-center gap-1.5">
                      <EventTypeIcon type={getDisplayEventType(e)} className="h-3.5 w-3.5" />
                      <span className="text-[11px] font-medium">{e.noise_whitelisted ? t('logs.noiseWhitelisted') : t(EVENT_TYPE_META[getDisplayEventType(e)]?.labelKey ?? getDisplayEventType(e))}</span>
                      {e.stream_actual && (
                        <span className="ml-1 rounded bg-muted/60 px-1 py-0.5 font-mono text-[9px] text-muted-foreground">
                          {e.stream_actual === 'stream' ? t('logs.streaming') : t('logs.whole')}
                        </span>
                      )}
                    </span>
                  </TableCell>
                  {/* 上游：优先配置名 upstream，client_app（进程探测）仅作缺失回退 */}
                  <TableCell className="py-2.5 px-4 truncate text-xs text-muted-foreground" title={String(e.upstream || e.client_app || '')}>
                    {String(e.upstream || e.client_app || '—')}
                  </TableCell>
                  {/* 模型：完整 host/path 放 title（悬停可见），列只展示模型名 */}
                  <TableCell className="py-2.5 px-4 font-mono text-[11px]" title={`${e.host ?? ''}${e.path ?? ''}${e.model ? `\n${tf('logs.modelIn', { m: e.model })}` : ''}`}>
                    <div className="flex items-center gap-1.5 truncate">
                      <span className="truncate">{e.model || (e.upstream ? '—' : e.host) || '—'}</span>
                      {e.stream_actual && (
                        <span className="shrink-0 rounded bg-muted/60 px-1 py-0.5 font-mono text-[9px] text-muted-foreground">
                          {e.stream_actual === 'stream' ? t('logs.streaming') : t('logs.whole')}
                        </span>
                      )}
                    </div>
                </TableCell>
                {/* 脱敏/还原/未还原/兜底：与 Logs 页口径完全对齐（未还原用琥珀告警，兜底用中性） */}
                <TableCell className="py-2.5 px-4 text-xs">
                    {(() => {
                      const itemLabels = getMatchedRules(e)
                      const hasSummary = (e.count ?? 0) > 0 || (e.restored ?? 0) > 0 || (e.unresolved ?? 0) > 0 || (e.degraded ?? 0) > 0
                      return hasSummary || itemLabels.length > 0 ? (
                        <span className="flex min-w-0 flex-col gap-0.5">
                          <span className="flex min-w-0 items-center gap-1.5 overflow-hidden">
                            {(e.count ?? 0) > 0 && <span className="shrink-0 text-blue-600 dark:text-blue-400">{t('logs.colMasked')} {e.count}</span>}
                            {(e.restored ?? 0) > 0 && <span className="shrink-0 text-emerald-600 dark:text-emerald-400">{t('logs.colRestored')} {e.restored}</span>}
                            {(e.unresolved ?? 0) > 0 && <span className="shrink-0 text-amber-600 dark:text-amber-400" title={t('logs.unresolvedHint')}>{t('logs.colUnresolved')} {e.unresolved}</span>}
                            {(e.degraded ?? 0) > 0 && <span className="shrink-0 text-muted-foreground" title={t('logs.degradedHint')}>{t('logs.colDegraded')} {e.degraded}</span>}
                          </span>
                          {itemLabels.length > 0 && (
                            <span className="flex min-w-0 items-center gap-1 overflow-hidden" title={itemLabels.join(', ')}>
                              {itemLabels.slice(0, 2).map((label) => (
                                <span key={label} className="shrink-0 rounded bg-muted px-1 py-0.5 font-mono text-[9px] text-muted-foreground">
                                  {label}
                                </span>
                              ))}
                              {itemLabels.length > 2 && <span className="shrink-0 text-[9px] text-muted-foreground">+{itemLabels.length - 2}</span>}
                            </span>
                          )}
                        </span>
                      ) : (
                        <span className="text-muted-foreground/50">—</span>
                      )
                    })()}
                  </TableCell>
                  {/* 状态：http_status 状态码或 status 语义 */}
                  <TableCell className="py-2.5 px-4 text-center">
                    {e.http_status ? (
                      <Badge
                        variant="outline"
                        className={cn(
                          'px-1 py-0 text-[9px] font-mono',
                          !e.noise_whitelisted && e.http_status >= 400
                            ? 'border-red-500/30 bg-red-500/10 text-red-600 dark:text-red-400'
                            : e.http_status >= 200 && e.http_status < 300
                              ? 'border-emerald-500/30 bg-emerald-500/10 text-emerald-600 dark:text-emerald-400'
                              : 'border-amber-500/30 bg-amber-500/10 text-amber-600 dark:text-amber-400',
                        )}
                      >
                        {e.http_status}
                      </Badge>
                    ) : e.status ? (
                      <span className="truncate text-[9px] text-muted-foreground" title={String(e.status)}>
                        {e.status === 'no_placeholder_in_response' ? t('logs.statusNoEcho') : e.status === 'no_sensitive_data' ? t('logs.statusNoSensitive') : String(e.status).slice(0, 6)}
                      </span>
                    ) : (
                      <span className="text-muted-foreground/50">—</span>
                    )}
                  </TableCell>
                  {/* 耗时 */}
                  <TableCell className="py-2.5 px-4 text-right tabular-nums text-xs text-muted-foreground">
                    {fmtMs(e.total_ms ?? e.upstream_ms ?? (e as { mask_ms?: number }).mask_ms)}
                  </TableCell>
                  {/* 费用：RESTORE 事件由后端按 model×usage 估算，与 Logs 页口径一致 */}
                  <TableCell className="py-2.5 px-4 text-right tabular-nums text-[11px]">
                    {e.cost_usd != null && e.cost_usd > 0 ? (
                      <span className="text-emerald-600 dark:text-emerald-400">${e.cost_usd.toFixed(4)}</span>
                    ) : (
                      <span className="text-muted-foreground/50">—</span>
                    )}
                  </TableCell>
                </TableRow>
              ))}
              {recentMerged.length === 0 && (
                <TableRow>
                  <TableCell colSpan={8} className="py-10 text-center text-sm text-muted-foreground">
                    {t('dash.noEvents')}
                  </TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
        </div>
      </Card>

      <EventDetailDialog open={detailOpen} onOpenChange={setDetailOpen} seq={detailSeq} />

      {/* 出口代理提示 */}
      {Boolean(status?.egress_proxy?.enabled) && (status?.egress_proxy_users ?? []).length > 0 && (
        <div className="flex items-center gap-2 rounded-xl border border-amber-500/20 bg-amber-500/5 px-4 py-2.5 text-xs text-amber-600 dark:text-amber-400">
          <Globe className="h-3.5 w-3.5 shrink-0" />
          <span>{t('dash.egressOn')}{status!.egress_proxy_users.join(lang === 'en' ? ', ' : '、')}</span>
        </div>
      )}
      {/* egress 开了但没有任何客户端勾选：状态型提示常驻展示（不再走保存 toast，
          那样每存一次任意配置都会重复弹一遍，用户反馈被骚扰） */}
      {Boolean(status?.egress_proxy?.enabled) && (status?.egress_proxy_users ?? []).length === 0 && (
        <div className="flex flex-wrap items-center justify-between gap-2 rounded-xl border border-amber-500/30 bg-amber-500/10 px-4 py-2.5 text-xs text-amber-700 dark:text-amber-300">
          <div className="flex items-center gap-2">
            <AlertCircle className="h-4 w-4 shrink-0 text-amber-600 dark:text-amber-400" />
            <span>{t('dash.egressNoUserNotice')}</span>
          </div>
          <Link to="/settings" className="shrink-0 font-medium underline hover:text-foreground">
            {t('settings.clients.goToEgressSetting')}
          </Link>
        </div>
      )}
      {!status?.egress_proxy?.enabled && (status?.egress_proxy_users ?? []).length > 0 && (
        <div className="flex flex-wrap items-center justify-between gap-2 rounded-xl border border-amber-500/30 bg-amber-500/10 px-4 py-2.5 text-xs text-amber-700 dark:text-amber-300">
          <div className="flex items-center gap-2">
            <AlertCircle className="h-4 w-4 shrink-0 text-amber-600 dark:text-amber-400" />
            <span>{tf('dash.egressNotConfiguredNotice', { list: status!.egress_proxy_users.join(lang === 'en' ? ', ' : '、') })}</span>
          </div>
          <Link to="/settings" className="shrink-0 font-medium underline hover:text-foreground">
            {t('settings.clients.goToEgressSetting')}
          </Link>
        </div>
      )}

      {/* 今日脱敏词明细：与全局明文显示开关同步。 */}
      <Dialog open={maskedOpen} onOpenChange={setMaskedOpen}>
        <DialogContent className="max-h-[80vh] max-w-2xl overflow-y-auto">
          <DialogHeader>
            <DialogTitle>{t('dash.maskedDetail')}</DialogTitle>
            <PlaintextSwitch />
          </DialogHeader>
          <div className="space-y-2">
            {/* 按入口分组同屏（代理链路 / 浏览器扩展），各组各取 Top N。
                组头计数让「×N 与日志条数一致」可核对，跳转恒带 &ingress=。 */}
            {groupWordsByIngress(stats, 20)
              .filter((g) => g.ingress === 'proxy' || !!cfg?.ext_bridge_enabled)
              .map((g) => (
                <div key={g.ingress} className="space-y-2">
                  <div className="flex items-center gap-2 pt-1 text-[11px] font-semibold text-muted-foreground">
                    <span>
                      {g.ingress === 'ext'
                        ? tf('logs.ingress.groupExt', { n: g.total.toLocaleString() })
                        : tf('logs.ingress.groupProxy', { n: g.total.toLocaleString() })}
                    </span>
                    <span className="h-px flex-1 bg-border" />
                  </div>
                  {/* ext 组为空但开关是开的 → 恰是「扩展装了但一条都没走通」的信号，
                      保留组头并提示，不静默藏掉 */}
                  {g.items.length === 0 && (
                    <p className="py-3 text-center text-xs text-muted-foreground">
                      {t('logs.ingress.emptyGroup')}
                    </p>
                  )}
                  {g.items.map((w) => {
                    const display = w.original ? displayOriginal(w.original, showPlaintext) : displayPreview(w.word, showPlaintext)
                    const searchTerm = w.original ?? (w.word.startsWith('<') ? w.label : w.word)
                    return (
                      <div
                        key={`${g.ingress}:${w.label}:${w.word}`}
                        onClick={() => {
                          setMaskedOpen(false)
                          // 跳转必须带 ingress：否则词条按入口分组、日志列表却不认这个参数，
                          // 又回到「数字对不上」
                          navigate(`/logs?private_search=${privateLogSearch(searchTerm)}&fulltext=1${ingressQuery(g.ingress)}`)
                        }}
                        className="group flex cursor-pointer items-center justify-between gap-3 rounded-lg border bg-muted/30 px-3 py-2 text-sm transition-colors hover:border-primary/40 hover:bg-muted/60"
                        title={t('dash.clickToFilterLogs')}
                      >
                        <div className="flex min-w-0 items-center gap-2">
                          <span className="shrink-0 rounded border px-1.5 py-0.5 text-[10px] text-muted-foreground">{w.label}</span>
                          <span className="truncate font-mono text-xs" title={display}>{display}</span>
                        </div>
                        <div className="flex items-center gap-2">
                          <span className="shrink-0 tabular-nums text-xs text-muted-foreground">×{w.count.toLocaleString()}</span>
                          <ArrowRight className="h-3.5 w-3.5 text-muted-foreground/40 opacity-0 transition-opacity group-hover:opacity-100" />
                        </div>
                      </div>
                    )
                  })}
                </div>
              ))}
            {(stats?.top_words ?? []).length === 0 && (
              <p className="py-8 text-center text-sm text-muted-foreground">{t('dash.noMaskedToday')}</p>
            )}
          </div>
        </DialogContent>
      </Dialog>

      {/* 还原统计只展示聚合数量，单条 RESTORE[redacted] 没有额外排查价值 */}
      <Dialog open={restoredOpen} onOpenChange={setRestoredOpen}>
        <DialogContent className="max-h-[80vh] max-w-2xl overflow-y-auto">
          <DialogHeader>
            <DialogTitle>{t('dash.restoredDetail')} · {t(statsRange === 'today' ? 'dash.statsToday' : statsRange === '7d' ? 'dash.stats7d' : 'dash.stats30d')}</DialogTitle>
          </DialogHeader>
          <div className="space-y-2">
            <p className="text-xs leading-relaxed text-muted-foreground">{t('dash.restoreCountHint')}</p>
            <p className="text-xs leading-relaxed text-muted-foreground">{restoreExplanation}</p>
            <p className="text-xs leading-relaxed text-muted-foreground">{t('dash.restoreSummaryOnly')}</p>
          </div>
        </DialogContent>
      </Dialog>
    </div>
  )
}
