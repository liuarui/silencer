import { usePrivacyStore, readPrivateLogSearch, privateLogSearch } from '@/stores/privacyStore'
import { displayOriginal, displayPreview, HIDDEN_TEXT } from '@/lib/privacy'
/** Retained log snapshots, one request per row, with shared outcome filters. */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useVirtualizer } from '@tanstack/react-virtual'
import { Download, Trash2, RefreshCw, Search, HelpCircle, Loader2, X, Globe } from 'lucide-react'
import { exportLogs, clearLogs } from '@/api/logs'
import { getAuditEvents } from '@/api/audit'
import { EventTypeIcon, EVENT_TYPE_META } from '@/components/events/EventTypeIcon'
import { EventDetailDialog } from '@/components/events/EventDetailDialog'
import { AuditEventDetailDialog, severityMeta, signalInfo } from '@/components/audit/AuditEventDetailDialog'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
  SelectGroup,
  SelectLabel,
} from '@/components/ui/select'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Badge } from '@/components/ui/badge'
import { toast } from '@/lib/toast'
import type { AuditEvent, AuditSeverity } from '@/types/api'
import dayjs from 'dayjs'
import { cn } from '@/lib/utils'
import { useI18n } from '@/lib/i18n'
import { getMatchedRules, mergeMaskRestore, getDisplayEventType, auditEventMatches, getLogPage, FILTER_ALL, type MergedEvent } from '@/lib/log-events'
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from '@/components/ui/tooltip'
import { useVisibility } from '@/lib/useVisibility'
import { RequestLedgerPanel } from '@/components/logs/RequestLedgerPanel'
import { useLogsQuery } from '../hooks/useLogsQuery'

/** 审计信号行合并进主列表（AGENTS.md：auditRows 前端合并，不改 events 表） */
// 稳定空数组引用：避免 data 为 undefined 时每 render 创建新空数组导致 useMemo 失效
const EMPTY_LIST: readonly never[] = []

/**
 * 入口筛选的「全部」哨兵。与 FILTER_ALL 同理：Radix Select 规定 SelectItem 的 value
 * 不能是空串（空串=未选择、显示 placeholder），用它当真实选项会跟组件语义打架。
 */
const INGRESS_ALL = '__all__'

interface AuditRow {
  id: number
  ts: number
  type: 'SCAN_WARN'
  sid: string
  method: string
  path: string
  host: string
  items: { label: string; preview: string }[]
  msg?: string
  status?: string
  http_status?: number
  total_ms?: number
  upstream_ms?: number
  seq: number
  count?: number
  restored?: number
  /** 查不到原文、原样透传出去的占位符数。>0 通常意味着模型自造了占位符 */
  unresolved?: number
  /** 靠宽松兜底（模型剥了花括号）修回来的占位符数。成功路径，但值得看见 */
  degraded?: number
  stream_actual?: string
  model?: string
  client_app?: string | number
  upstream?: string
  /** 审计信号严重度（列表行摘要徽标 + 详情弹窗） */
  severity?: AuditSeverity
  probe_id?: string
  /** 原始审计事件：日志行点击后原地打开审计详情弹窗 */
  _auditEvent?: AuditEvent
  /** 与合并后的事件行对齐：审计行没有回源 seq，但联合类型要能统一访问 */
  _detailSeq?: number
  _detailId?: string
  detail_id?: string
}

/** 日志表格列宽定义。
 *
 * **表头与数据行必须共用这一份**：两处各写一遍数值时，改一处忘一处就会让表头与内容
 * 逐列错位（此前就是这么漂移的）。同时给每个需要截断的 grid cell 都加 `min-w-0`
 * —— grid item 默认 `min-width:auto`，不加的话 `truncate` 完全不生效，长值会被
 * 行容器的 `overflow-hidden` **硬切断**（没有省略号，看着像文字被压重叠）。
 */
const LOG_GRID_COLS =
  'grid-cols-[68px_88px_112px_108px_minmax(96px,1fr)_minmax(190px,1.4fr)_54px_58px_72px]'
/** 表格最小宽度：上面这些列 + gap + 左右内边距能完整放下的下限；低于它由外层横向滚动接管。 */
const LOG_MIN_W = 'min-w-[940px]'

type LogRow = (AuditRow & { _audit: true }) | MergedEvent

export default function LogsPage() {
  const { t, tf } = useI18n()
  const [searchParams, setSearchParams] = useSearchParams()
  const queryClient = useQueryClient()
  const showPlaintext = usePrivacyStore((state) => state.showPlaintext)
  const privateSearchId = searchParams.get('private_search')
  const privateSearch = readPrivateLogSearch(privateSearchId)
  // 页面隐藏时停止轮询；可见时自动刷新
  const { hidden } = useVisibility()
  // 空串是 Radix Select 的保留值（它把 value === '' 当成「未选择，显示 placeholder」），
  // 用它当「全部」这个真实选项会跟组件语义打架：官方文档明确要求 SelectItem 的 value
  // 不能是空串。用哨兵值把「全部」和「未选择」区分开，符合 Radix Select 约束。
  const [filterType, setFilterType] = useState<string>(FILTER_ALL)
  const [sensitive, setSensitive] = useState(false)
  const [q, setQ] = useState('')
  const [searchInput, setSearchInput] = useState('')
  const [fulltext, setFulltext] = useState(false)
  /** 入口维度筛选（proxy / ext）。落 URL query，可分享、可回退 —— 与词榜跳转同口径 */
  const [ingress, setIngress] = useState<string>(INGRESS_ALL)
  const [detailSeq, setDetailSeq] = useState<number | string | null>(null)
  const [detailOpen, setDetailOpen] = useState(false)
  // 审计行详情：原地弹窗（不再跳转审计中心）
  const [auditDetail, setAuditDetail] = useState<AuditEvent | null>(null)
  const [confirmClear, setConfirmClear] = useState(false)
  const [tailOpen, setTailOpen] = useState(false)
  const [tailData, setTailData] = useState<string[]>([])
  const [hideNoise, setHideNoise] = useState(false)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(50)
  const scrollRef = useRef<HTMLDivElement>(null)

  const urlQ = privateSearchId ? privateSearch : (searchParams.get('q') || '').trim()
  useEffect(() => {
    setSearchInput(urlQ)
    setQ(urlQ)
    setPage(1)
  }, [urlQ])

  // URL search is independent of type/ingress navigation and browser history.
  useEffect(() => {
    const urlType = (searchParams.get('type') || '').trim().toUpperCase()
    const urlFulltext = searchParams.get('fulltext') === '1'
    const urlIngress = (searchParams.get('ingress') || '').trim().toLowerCase()
    const normalizedIngress = urlIngress === 'proxy' || urlIngress === 'ext' ? urlIngress : INGRESS_ALL
    const parsedType = urlType || FILTER_ALL
    setFilterType(parsedType === 'EXT_ALL' && normalizedIngress === 'proxy' ? FILTER_ALL : parsedType)
    setFulltext(urlFulltext)
    setPage(1)
    // 入口维度**按 URL 原值镜像**（而不是「有才设」）：它只由本页写入 URL，
    // 镜像才能让浏览器前进/后退真的切换筛选，也才能让「词条 ×N → 日志条数」对得上。
    setIngress(normalizedIngress)
  }, [searchParams])

  // 保留天数配置已移至高级设置页（Logs 筛选栏只留筛选控件）

  // 搜索防抖 250ms：只更新 q（驱动 queryKey），不再手动清空列表
  useEffect(() => {
    const t = setTimeout(() => {
      const nextQ = searchInput.trim()
      if (nextQ !== q) {
        setQ(nextQ)
        setPage(1)
      }
      if (!privateSearchId && nextQ !== urlQ) {
        setSearchParams((prev) => {
          const next = new URLSearchParams(prev)
          if (nextQ) next.set('q', nextQ)
          else next.delete('q')
          next.delete('private_search')
          return next
        }, { replace: true })
      }
    }, 250)
    return () => clearTimeout(t)
  }, [searchInput, q, urlQ, privateSearchId, setSearchParams])

  // Latest snapshots replace cached rows so late writes, clearing and expiry reconcile.
  const { logsQuery } = useLogsQuery({
    filterType,
    sensitive,
    hideNoise,
    q,
    fulltext,
    ingress,
    requestType: filterType === FILTER_ALL || filterType === 'EXT_ALL'
      ? undefined
      : filterType,
    requestIngress: ingress !== INGRESS_ALL
      ? ingress as 'proxy' | 'ext'
      : filterType === 'EXT_ALL'
        ? 'ext'
        : undefined,
    hidden,
    onTail: setTailData,
  })

  const events = logsQuery.data?.list ?? EMPTY_LIST
  const isFetching = logsQuery.isFetching

  // 导出 / 清空进行中（长耗时操作，需要即时反馈）
  const [exporting, setExporting] = useState(false)
  const [clearing, setClearing] = useState(false)

  // 审计信号合并（POISON 过滤项 = SCAN_WARN + 审计信号）
  // queryKey 与审计页分开：两处 queryFn 不同，共用 key 会被 TanStack 按 key 去重
  const includeAuditRows = ingress === INGRESS_ALL && !sensitive && (filterType === FILTER_ALL || filterType === 'SCAN_WARN')
  const auditQuery = useQuery({
    queryKey: ['auditEvents', 'logsMerge'],
    enabled: includeAuditRows,
    queryFn: async ({ signal }) => {
      // Fetch one extra row so the UI can distinguish exactly 500 events from
      // a truncated 500-row window without loading an unbounded audit history.
      const resp = await getAuditEvents(0, 501, {}, signal)
      const mapped: AuditRow[] = (resp?.events ?? []).map((r) => ({
        id: r.seq,
        ts: r.ts,
        type: 'SCAN_WARN' as const,
        sid: r.probe_id ?? r.sid ?? 'audit',
        method: r.method ?? 'AUDIT',
        path: r.path ?? '',
        host: r.host ?? '',
        items: [{ label: r.signal_type ?? t('logs.signalLabel'), preview: r.evidence ?? r.severity ?? '' }],
        msg: r.evidence,
        severity: r.severity,
        probe_id: r.probe_id,
        seq: r.seq,
        _auditEvent: r,
      }))
      return { rows: mapped.slice(0, 500), windowLimited: mapped.length > 500 }
    },
    staleTime: 0,
    refetchOnMount: 'always',
    refetchInterval: hidden ? false : 3000,
    refetchIntervalInBackground: false,
  })
  const auditRows = auditQuery.data?.rows ?? EMPTY_LIST

  // 合并展示：审计信号 + 事件按 sid 合并 MASK/RESTORE 成一条「往返链路」行，按 ts 降序
  // （用户明确要求：一次请求对应一条日志；CANCEL/DNS_ERROR/ERR 等非链路事件保持独立行）
  const merged = useMemo(() => {
    const audit: (AuditRow & { _audit: true })[] = auditRows
      .filter((row) => row._auditEvent && auditEventMatches(row._auditEvent, { ingress, filterType, sensitive, q }))
      .map((row) => ({ ...row, _audit: true as const }))
    // 普通事件：MASK/RESTORE 合并与排序交给共享函数（与 Dashboard 最近日志同口径）
    const ev = mergeMaskRestore(events, { filterType, hideNoise })
    // 按 ts 降序合并审计行与事件行
    return [...audit, ...ev].sort((a, b) => (b.ts ?? 0) - (a.ts ?? 0)) as LogRow[]
  }, [auditRows, events, filterType, hideNoise, ingress, sensitive, q])

  const visibleAuditCount = merged.filter((row) => row._audit).length
  const currentPage = getLogPage(page, merged.length, pageSize)
  useEffect(() => { if (page !== currentPage) setPage(currentPage) }, [page, currentPage])

  // Clamp before slicing so a smaller result set never renders a false empty page.
  const paged = useMemo(() => {
    const start = (currentPage - 1) * pageSize
    return merged.slice(start, start + pageSize)
  }, [merged, currentPage, pageSize])

  // 虚拟滚动（基于分页后的数据）
  const virtualizer = useVirtualizer({
    count: paged.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => 48,
    overscan: 8,
  })

  const refresh = useCallback(() => {
    setPage(1)
    void queryClient.resetQueries({ queryKey: ['logs'] })
    void queryClient.resetQueries({ queryKey: ['recentLogs'] })
    void queryClient.resetQueries({ queryKey: ['auditEvents', 'logsMerge'] })
    void queryClient.resetQueries({ queryKey: ['logDetail'] })
  }, [queryClient])

  const doExport = async () => {
    if (exporting) return
    setExporting(true)
    try {
      const resp = await exportLogs({
        chains: true,
        hideNoise,
        includeAudit: true,
        type: filterType === FILTER_ALL || filterType === 'EXT_ALL' ? undefined : filterType,
        sensitive,
        q,
        fulltext,
        ingress: ingress !== INGRESS_ALL
          ? ingress as 'proxy' | 'ext'
          : filterType === 'EXT_ALL'
            ? 'ext'
            : undefined,
      })
      const blob = await resp.blob()
      const filename = `silencer-events-${dayjs().format('YYYY-MM-DD-HHmmss')}.json`
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = filename
      document.body.appendChild(a)
      a.click()
      document.body.removeChild(a)
      URL.revokeObjectURL(url)
      const exported = JSON.parse(await blob.text()) as { window_limited?: boolean; has_more?: boolean }
      toast(t(exported.window_limited || exported.has_more ? 'logs.exportWindowLimited' : 'logs.exported'))
    } catch (e) {
      toast(tf('logs.exportFail', { e: String(e) }), 'error')
    } finally {
      setExporting(false)
    }
  }

  const doClear = async () => {
    if (clearing) return
    setClearing(true)
    try {
      await clearLogs()
      toast(t('logs.cleared'))
      setConfirmClear(false)
      setPage(1)  // 清空后列表骤减，停留高页码会渲染空页 + 错误的分页条
      refresh()
    } catch (e) {
      toast(tf('logs.clearFail', { e: String(e) }), 'error')
    } finally {
      setClearing(false)
    }
  }

  const openDetail = (row: (typeof merged)[number]) => {
    // 审计行：原地打开审计详情弹窗（信号说明/危害/证据/探针 ID）
    if (row._audit) {
      setAuditDetail((row as AuditRow)._auditEvent ?? null)
      return
    }
    // 合并行优先回源 RESTORE 事件（详情弹窗同时含用户消息与助手回复，完整链路）
    setDetailSeq(row._detailId ?? row._detailSeq ?? row.detail_id ?? row.seq)
    setDetailOpen(true)
  }

  // 耗时格式统一：<1s 毫秒；<60s 秒(1 位小数)；≥60s 分:秒（用户要求：时间长用分秒代替，不全是毫秒）
  const fmtMs = (ms?: number) => {
    if (ms == null) return '—'
    if (ms < 1000) return `${Math.round(ms)}ms`
    const s = ms / 1000
    if (s < 60) return `${s.toFixed(1)}s`
    const m = Math.floor(s / 60)
    return `${m}m${Math.round(s % 60)}s`
  }

  // 处理摘要列：事件行显示「脱敏 N / 还原 N」（无命中时回退路径/状态），审计行显示信号名 + 严重度
  const renderSummary = (row: (typeof merged)[number]) => {
    if (row._audit) {
      const ev = (row as AuditRow)._auditEvent
      const sig = ev?.signal_type ?? (row as AuditRow).path ?? ''
      const info = signalInfo(sig)
      const sev = (row as AuditRow).severity
      return (
        <span className="flex min-w-0 items-center gap-1.5" title={showPlaintext ? ev?.evidence ?? (row as AuditRow).msg ?? '' : HIDDEN_TEXT}>
          <span className="truncate text-[11px] font-medium text-amber-600 dark:text-amber-400">
            {t(info.labelKey ?? info.label ?? sig)}
          </span>
          {sev && (
            <Badge variant="outline" className={cn('shrink-0 rounded-full px-1 py-0 text-[9px]', severityMeta(sev).cls)}>
              {t(severityMeta(sev).labelKey)}
            </Badge>
          )}
        </span>
      )
    }
    // 普通事件：优先展示脱敏/还原计数（一次请求一条链路行的核心信息）
    const masked = (row.count ?? 0) > 0
    const restored = (row.restored ?? 0) > 0
    // 未还原的占位符：绝大多数情况是模型自己编了一个我们从没发出去过的占位符
    // （它把 {{IPPRIVATE_83fc6a}} 的后缀当成可计算的数字改写），少数是映射过期。
    // 不显示出来的话，用户只在终端里看到裸露的 {{...}} 而无从判断是谁的问题——
    // 0.1.12 就是因此误判成「还原坏了」，进而加了一段凭空推算 IP 的代码。
    const unresolved = (row.unresolved ?? 0) > 0
    // 兜底还原：模型把花括号剥了，靠宽松正则捞回来的。是成功，所以用中性色不报警，
    // 但要看得见——它是「模型正在改写输出格式」的前兆信号。
    const degraded = (row.degraded ?? 0) > 0
    // 提取本条记录捕获的敏感词类型标签（如 PHONE, CONNSTR 等），让列表直观展现脱敏项类别
    const itemLabels = getMatchedRules(row)
    const itemsPreviewText = (((row as MergedEvent).items || []) as { label?: string; preview?: string; original?: string }[])
      .map((it) => (it.label ? `${it.label}: ${it.original ? displayOriginal(it.original, showPlaintext) : displayPreview(it.preview ?? '—', showPlaintext)}` : ''))
      .filter(Boolean)
      .join(' | ')

    if (masked || restored || unresolved || degraded) {
      return (
        // 拆成上下两行（计数 / 标签）是**为根治重叠**：此前挤在单行里，标签组一旦被压缩，
        // 内部 `whitespace-nowrap` 的子元素会溢出自身边界，与后面的「未还原 N」糊在一起
        // （实测截图：`API_KEY` 与橙色 `未还原 4` 直接重叠）。拆行后同一行内不再存在
        // 「可收缩元素 与 shrink-0 元素 抢宽度」的竞争，溢出统一由 overflow-hidden 裁切。
        <span
          className="flex min-w-0 flex-col justify-center gap-0.5 text-[11px]"
          title={[itemsPreviewText, row.method, row.host, row.path].filter(Boolean).join(' · ')}
        >
          {/* 第一行：计数。关键数字，永不与标签抢宽度 */}
          <span className="flex min-w-0 items-center gap-1.5 overflow-hidden">
            {masked && <span className="shrink-0 whitespace-nowrap text-blue-600 dark:text-blue-400">{t('logs.colMasked')} {row.count}</span>}
            {restored && <span className="shrink-0 whitespace-nowrap text-emerald-600 dark:text-emerald-400">{t('logs.colRestored')} {row.restored}</span>}
            {unresolved && (
              <span className="shrink-0 whitespace-nowrap text-amber-600 dark:text-amber-400" title={t('logs.unresolvedHint')}>
                {t('logs.colUnresolved')} {row.unresolved}
              </span>
            )}
            {degraded && (
              <span className="shrink-0 whitespace-nowrap text-muted-foreground" title={t('logs.degradedHint')}>
                {t('logs.colDegraded')} {row.degraded}
              </span>
            )}
          </span>
          {/* 第二行：命中标签。超出仅显示前两个 + 折叠计数 */}
          {itemLabels.length > 0 && (
            <span className="flex min-w-0 items-center gap-1 overflow-hidden">
              {itemLabels.slice(0, 2).map((lb) => (
                <span key={lb} className="shrink-0 whitespace-nowrap rounded bg-muted px-1 py-0.2 font-mono text-[9px] text-muted-foreground">
                  {lb}
                </span>
              ))}
              {itemLabels.length > 2 && (
                <span className="shrink-0 whitespace-nowrap text-[9px] text-muted-foreground">+{itemLabels.length - 2}</span>
              )}
            </span>
          )}
        </span>
      )
    }
    // 无命中的行（PASS/BYPASS/SKIP/ERR…）：展示「—」
    const pathText = [row.method, row.host, row.path].filter(Boolean).join(' ')
    return (
      <span className="text-[11px] text-muted-foreground/50" title={pathText || row.status || ''}>
        —
      </span>
    )
  }


  return (
    <div className="flex h-full min-h-0 flex-col space-y-4">
      {/* 页头 */}
      <div className="flex items-end justify-between">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">{t('logs.pageTitle')}</h1>
          <p className="mt-1 text-sm font-medium text-muted-foreground">
            {t('logs.subtitle')}
          </p>
        </div>
      </div>

      <RequestLedgerPanel />

      {/* 筛选栏 */}
      <div className="flex flex-wrap items-center gap-2">
        <Select value={filterType} onValueChange={(v) => {
          setFilterType(v)
          setPage(1)
          if (v === 'EXT_ALL') {
            setIngress('ext')
          }
          setSearchParams((prev) => {
            const next = new URLSearchParams(prev)
            if (v === FILTER_ALL) next.delete('type')
            else next.set('type', v)
            if (v === 'EXT_ALL') next.set('ingress', 'ext')
            return next
          }, { replace: false })
        }}>
          <SelectTrigger className="h-8 w-[156px] text-xs">
            <SelectValue placeholder={t('logs.filterAll')} />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value={FILTER_ALL}>{t('logs.filterAll')}</SelectItem>
            <SelectItem value="EXT_ALL" className="font-medium text-blue-600 dark:text-blue-400">
              <div className="flex items-center gap-1.5">
                <Globe className="h-3.5 w-3.5" />
                <span>{t('logs.filterExt')}</span>
              </div>
            </SelectItem>
            <SelectGroup>
              <SelectLabel>{t('logs.filterHandled')}</SelectLabel>
              <SelectItem value="MASK">{t('logs.filterMasked')}</SelectItem>
              <SelectItem value="RESTORE">{t('logs.filterRestored')}</SelectItem>
            </SelectGroup>
            <SelectGroup>
              <SelectLabel>{t('logs.filterErrors')}</SelectLabel>
              <SelectItem value="BLOCK">{t('logs.filterBlocked')}</SelectItem>
              <SelectItem value="ERR">{t('logs.filterErr')}</SelectItem>
              <SelectItem value="SCAN_WARN">{t('logs.filterScanWarn')}</SelectItem>
            </SelectGroup>
            <SelectGroup>
              <SelectLabel>{t('logs.filterPassedCombined')}</SelectLabel>
              <SelectItem value="PASS_COMBINED">{t('logs.filterPassedAll')}</SelectItem>
            </SelectGroup>
            <SelectGroup>
              <SelectLabel className="text-[10px] text-muted-foreground">{t('logs.filterAdvanced')}</SelectLabel>
              <SelectItem value="PASS">{t('logs.filterPass')}</SelectItem>
              <SelectItem value="BYPASS">{t('logs.filterBypass')}</SelectItem>
              <SelectItem value="SKIP">{t('logs.filterSkip')}</SelectItem>
              <SelectItem value="CANCEL">{t('logs.filterCancel')}</SelectItem>
              <SelectItem value="DNS_ERROR">{t('logs.filterDns')}</SelectItem>
            </SelectGroup>
          </SelectContent>
        </Select>

        {/* 入口维度筛选：与首页/统计页的词榜分组**同一个值**，跳转时带着走 */}
        <Select
          value={ingress}
          onValueChange={(v) => {
            const nextFilterType = filterType === 'EXT_ALL' && v === 'proxy' ? FILTER_ALL : filterType
            setIngress(v)
            setFilterType(nextFilterType)
            setPage(1)
            // 落 URL query（可分享/可回退）——只做组件内 state 的话，
            // 首页词条按入口分组、日志列表却不认这个参数，又回到「数字对不上」。
            setSearchParams(
              (prev) => {
                const next = new URLSearchParams(prev)
                if (v === INGRESS_ALL) next.delete('ingress')
                else next.set('ingress', v)
                if (nextFilterType === FILTER_ALL) next.delete('type')
                else next.set('type', nextFilterType)
                return next
              },
              { replace: false },
            )
          }}
        >
          <SelectTrigger className="h-8 w-[132px] text-xs" data-testid="ingress-filter">
            <SelectValue placeholder={t('logs.ingress.all')} />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value={INGRESS_ALL}>{t('logs.ingress.all')}</SelectItem>
            <SelectItem value="proxy">{t('logs.ingress.proxy')}</SelectItem>
            <SelectItem value="ext">{t('logs.ingress.ext')}</SelectItem>
          </SelectContent>
        </Select>

        <div className="relative">
          <Search className="absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground" />
          <Input
            value={privateSearchId && !showPlaintext ? displayOriginal(searchInput, false) : searchInput}
            readOnly={Boolean(privateSearchId) && !showPlaintext}
            title={privateSearchId && !showPlaintext ? t('privacy.editSearchHint') : undefined}
            onChange={(e) => {
              const value = e.target.value
              setSearchInput(value)
              if (privateSearchId) {
                const id = privateLogSearch(value)
                setSearchParams((prev) => {
                  const next = new URLSearchParams(prev)
                  next.delete('q')
                  next.set('private_search', id)
                  return next
                }, { replace: true })
              }
            }}
            placeholder={t('logs.searchHost')}
            className="h-8 w-56 pl-8 pr-7 text-xs"
          />
          {searchInput && (
            <button
              type="button"
              onClick={() => {
                setSearchInput('')
                setQ('')
                // Clear only the search term; preserve the other filters.
                setSearchParams(
                  (prev) => {
                    const next = new URLSearchParams(prev)
                    next.delete('q')
                    next.delete('private_search')
                    return next
                  },
                  { replace: true },
                )
              }}
              className="absolute right-2 top-1/2 -translate-y-1/2 text-muted-foreground/60 hover:text-foreground"
              title={t('common.reset')}
            >
              <X className="h-3.5 w-3.5" />
            </button>
          )}
        </div>

        <label className="flex cursor-pointer select-none items-center gap-1.5 text-xs text-muted-foreground transition-colors hover:text-foreground">
          <Switch checked={fulltext} onCheckedChange={(v) => { setFulltext(v); setPage(1); setSearchParams((prev) => { const next = new URLSearchParams(prev); if (v) next.set('fulltext', '1'); else next.delete('fulltext'); return next }, { replace: true }) }} className="scale-75" />
          <span>{t('logs.fulltext')}</span>
        </label>
        <label className="flex cursor-pointer select-none items-center gap-1.5 text-xs text-muted-foreground transition-colors hover:text-foreground">
          <Switch checked={sensitive} onCheckedChange={(v) => { setSensitive(v); setPage(1) }} className="scale-75" />
          <span>{t('logs.sensitiveOnly')}</span>
        </label>
        <label className="flex cursor-pointer select-none items-center gap-1.5 text-xs text-muted-foreground transition-colors hover:text-foreground" title={t('logs.hideNoiseTitle')}>
          <Switch checked={hideNoise} onCheckedChange={(v) => { setHideNoise(v); setPage(1) }} className="scale-75" />
          <span>{t('logs.hideNoise')}</span>
        </label>

        <div className="ml-auto flex items-center gap-1.5">
          <Button size="sm" variant="ghost" className="h-8 px-2.5" onClick={refresh} title={t('logs.refresh')}>
            <RefreshCw className={cn('h-3.5 w-3.5', isFetching && 'animate-spin')} />
          </Button>
          <Button size="sm" variant="outline" className="h-8 px-2.5 text-xs" onClick={doExport} loading={exporting}>
            {!exporting && <Download className="mr-1 h-3.5 w-3.5" />} {t('logs.export')}
          </Button>
          <Button
            size="sm"
            variant="outline"
            className="h-8 px-2.5 text-xs text-red-600 hover:text-red-600 dark:text-red-400"
            onClick={() => setConfirmClear(true)}
            disabled={clearing}
          >
            {!clearing && <Trash2 className="mr-1 h-3.5 w-3.5" />} {t('common.clear')}
          </Button>
        </div>
      </div>

      {/* 状态颜色说明：问号 Tooltip（与全局统一，hover 查看） */}
      <div className="flex items-center gap-1.5 px-1">
        <TooltipProvider delayDuration={200}>
          <Tooltip>
            <TooltipTrigger asChild>
              <HelpCircle className="h-3.5 w-3.5 cursor-help text-muted-foreground/60" />
            </TooltipTrigger>
            <TooltipContent side="right" className="max-w-[320px] space-y-1">
              <div className="font-semibold">{t('logs.legendTitle')}</div>
              <div className="flex items-center gap-1.5"><Badge className="bg-blue-500/10 text-blue-600 hover:bg-blue-500/10 dark:text-blue-400">{t('logs.filterMasked')}</Badge>{t('logs.legendMasked')}</div>
              <div className="flex items-center gap-1.5"><Badge className="bg-emerald-500/10 text-emerald-600 hover:bg-emerald-500/10 dark:text-emerald-400">{t('logs.filterRestored')}</Badge>{t('logs.legendRestored')}</div>
              <div className="flex items-center gap-1.5"><Badge variant="outline">{t('logs.filterPass')}</Badge>{t('logs.legendPass')}</div>
              <div className="flex items-center gap-1.5"><Badge variant="outline">{t('logs.filterMissed')}</Badge>{t('logs.legendSkip')}</div>
              <div className="flex items-center gap-1.5"><Badge className="bg-amber-500/10 text-amber-600 hover:bg-amber-500/10 dark:text-amber-400">{t('logs.filterScanWarn')}</Badge>{t('logs.legendScanWarn')}</div>
              <div className="flex items-center gap-1.5"><Badge className="bg-red-500/10 text-red-600 hover:bg-red-500/10 dark:text-red-400">{t('logs.filterBlocked')}</Badge>{t('logs.legendBlock')}</div>
              <div className="flex items-center gap-1.5"><Badge className="bg-amber-500/15 text-amber-600 hover:bg-amber-500/15 dark:text-amber-400">{t('logs.poisonBadge')}</Badge>{t('logs.legendPoison')}</div>
            </TooltipContent>
          </Tooltip>
        </TooltipProvider>
      </div>

      {/* 表格（虚拟滚动） */}
      <div
        ref={scrollRef}
        className="min-h-0 flex-1 overflow-auto rounded-xl border bg-card shadow-[var(--shadow-card)]"
      >
        <div className={LOG_MIN_W}>
        {/* 表头（固定）：时间(两行 日期+时间) / 结果 / 入口 / 上游 / 模型(收紧) / 处理摘要(脱敏/还原 宽列) / 状态 / 耗时 / 费用 */}
        <div
          className={cn(
            'sticky top-0 z-10 grid gap-2 border-b bg-card px-3 py-2 text-[11px] font-semibold uppercase tracking-wide text-muted-foreground',
            LOG_GRID_COLS,
          )}
        >
          <span>{t('logs.colTime')}</span>
          <span>{t('logs.colResult')}</span>
          <span>{t('logs.colIngress')}</span>
          <span>{t('logs.colUpstream')}</span>
          <span>{t('logs.colModel')}</span>
          <span>{t('logs.colSummary')}</span>
          <span className="text-center">{t('logs.colStatus')}</span>
          <span className="text-right">{t('logs.colDuration')}</span>
          <span className="text-right">{t('logs.colCost')}</span>
        </div>

        <div className="relative" style={{ height: virtualizer.getTotalSize() }}>
          {virtualizer.getVirtualItems().map((vi) => {
            const row = paged[vi.index]
            if (!row) return null
            return (
              <div
                key={`${row._audit ? 'a' : 'e'}-${row.seq}-${vi.index}`}
                className={cn(
                  'absolute left-0 top-0 grid w-full items-center gap-2 overflow-hidden border-b px-3 text-[12px]',
                  LOG_GRID_COLS,
                  'transition-colors hover:bg-muted/40',
                  row._audit && 'bg-amber-500/5 hover:bg-amber-500/10',
                )}
                style={{ height: vi.size, transform: `translateY(${vi.start}px)` }}
                onClick={() => openDetail(row)}
              >
                {/* 时间：上日期下时间（用户要求，一行放不下就两行） */}
                <span className="flex flex-col leading-tight tabular-nums text-[11px] text-muted-foreground shrink-0">
                  <span>{row.ts ? dayjs(row.ts * 1000).format('MM-DD') : '—'}</span>
                  <span className="opacity-70">{row.ts ? dayjs(row.ts * 1000).format('HH:mm:ss') : ''}</span>
                </span>
                {/* 结果 */}
                <span className="flex items-center gap-1 min-w-0">
                  <EventTypeIcon type={getDisplayEventType(row)} className="h-3.5 w-3.5 shrink-0" />
                  <span className="truncate text-[11px] font-medium">{!row._audit && row.noise_whitelisted ? t('logs.noiseWhitelisted') : t(EVENT_TYPE_META[getDisplayEventType(row)]?.labelKey ?? getDisplayEventType(row))}</span>
                </span>
                {/* 入口：proxy=CLI 代理链路；ext=浏览器扩展 */}
                <div className="flex flex-col justify-center min-w-0">
                  {(row as { ingress?: string }).ingress === 'ext' ? (
                    <div className="flex flex-col gap-0.5">
                      <span className="inline-flex w-fit items-center gap-1 whitespace-nowrap rounded bg-blue-500/10 px-1.5 py-0.5 text-[10px] font-medium text-blue-600 dark:text-blue-400">
                        <Globe className="h-3 w-3 shrink-0" />
                        <span>{t('logs.badgeExt')}</span>
                      </span>
                    </div>
                  ) : (
                    <span className="min-w-0 truncate whitespace-nowrap text-[10px] text-muted-foreground">
                      {t('logs.ingress.proxy')}
                    </span>
                  )}
                </div>
                {/* 上游：优先配置名 upstream，client_app（进程探测）仅作缺失回退；扩展行展示目标站点 host */}
                <span className="min-w-0 truncate text-[11px] font-medium text-foreground/85" title={String(row.upstream || row.client_app || row.host || '')}>
                  {(row as { ingress?: string }).ingress === 'ext'
                    ? (row.host || 'Web AI')
                    : String(row.upstream || row.client_app || '—')}
                </span>
                {/* 模型：模型名 + 流式标签，完整 host/path 放 title（悬停可见）；扩展行展示模型或 Web 对话 */}
                <span
                  className="flex min-w-0 items-center gap-1.5"
                  title={`${row.host ?? ''}${row.path ?? ''}${row.model ? `\n${tf('logs.modelIn', { m: row.model })}` : ''}${(row as { _detailSeq?: number })._detailSeq ? `\n${tf('logs.restoredN', { n: row.restored ?? 0 })}` : ''}`}
                >
                  <code className="truncate font-mono text-[11px]">
                    {row.model || ((row as { ingress?: string }).ingress === 'ext' ? t('logs.webChat') : (row.upstream ? '—' : row.host) || '—')}
                  </code>
                  {row.stream_actual && (
                    <span className="shrink-0 rounded bg-muted/60 px-1 py-0.5 font-mono text-[9px] text-muted-foreground">
                      {row.stream_actual === 'stream' ? t('logs.streaming') : t('logs.whole')}
                    </span>
                  )}
                </span>
                {/* 处理摘要：脱敏/还原计数，审计行为信号名 + 严重度，无命中的行回退路径 */}
                {renderSummary(row)}
                {/* 状态：http_status 数字或 status 中文语义 */}
                <span className="min-w-0 text-center">
                  {row.http_status ? (
                    <Badge
                      variant="outline"
                      className={cn(
                        'px-1 py-0 text-[9px] font-mono',
                        !(!row._audit && row.noise_whitelisted) && row.http_status >= 400
                          ? 'border-red-500/30 bg-red-500/10 text-red-600 dark:text-red-400'
                          : row.http_status >= 200 && row.http_status < 300
                            ? 'border-emerald-500/30 bg-emerald-500/10 text-emerald-600 dark:text-emerald-400'
                            : 'border-amber-500/30 bg-amber-500/10 text-amber-600 dark:text-amber-400',
                      )}
                    >
                      {row.http_status}
                    </Badge>
                  ) : row.status ? (
                    <span className="min-w-0 truncate text-[9px] text-muted-foreground" title={String(row.status)}>
                      {row.status === 'no_placeholder_in_response' ? t('logs.statusNoEcho') : row.status === 'no_sensitive_data' ? t('logs.statusNoSensitive') : String(row.status).slice(0, 6)}
                    </span>
                  ) : (row as { ingress?: string }).ingress === 'ext' ? (
                    <Badge
                      variant="outline"
                      className="border-emerald-500/30 bg-emerald-500/10 px-1 py-0 text-[9px] font-mono text-emerald-600 dark:text-emerald-400"
                    >
                      200
                    </Badge>
                  ) : '—'}
                </span>
                {/* 耗时：合并行取整链路(RESTORE)，MASK 单行(未还原/阻断)取脱敏管线耗时 */}
                <span className="text-right tabular-nums text-[11px] text-muted-foreground">
                  {fmtMs(row.total_ms ?? row.upstream_ms ?? (row as { mask_ms?: number }).mask_ms)}
                </span>
                {/* 费用（RESTORE 事件由后端按模型×usage 估算） */}
                <span className="text-right tabular-nums text-[11px]">
                  {(row as { cost_usd?: number }).cost_usd != null && (row as { cost_usd?: number }).cost_usd! > 0
                    ? <span className="text-emerald-600 dark:text-emerald-400">${(row as { cost_usd?: number }).cost_usd!.toFixed(4)}</span>
                    : (row as { ingress?: string }).ingress === 'ext'
                      ? <span className="text-[10px] text-muted-foreground/40 font-mono">{t('logs.free')}</span>
                      : <span className="text-muted-foreground/50">—</span>}
                </span>
              </div>
            )
          })}
        </div>

        {paged.length === 0 && !logsQuery.isLoading && (
          <div className="flex flex-col items-center justify-center py-20 text-center">
            <div className="mb-3 flex h-11 w-11 items-center justify-center rounded-2xl bg-muted/60 text-muted-foreground/70">
              <Search className="h-5 w-5 stroke-[1.5]" />
            </div>
            <p className="text-sm font-semibold text-foreground/80">{t('logs.noEvents')}</p>
            <p className="mt-1 max-w-sm text-xs text-muted-foreground/70">{t('logs.noEventsHint')}</p>
          </div>
        )}
        {paged.length === 0 && logsQuery.isLoading && (
          <div className="flex items-center justify-center gap-2 py-16 text-sm text-muted-foreground">
            <Loader2 className="h-4 w-4 animate-spin" />
            {t('logs.loading')}
          </div>
        )}
        </div>
      </div>

      {/* 原始日志尾巴（引擎 stdout 白名单行） */}
      <div className="text-xs">
        <button type="button" className="text-muted-foreground hover:text-foreground" onClick={() => setTailOpen((v) => !v)}>
          {tailOpen ? '▾' : '▸'} {tf('logs.tailTitle', { n: tailData.length })}
        </button>
        {tailOpen && (
          <pre className="mt-2 max-h-48 overflow-auto whitespace-pre-wrap break-all rounded-lg border bg-muted/30 p-3 font-mono text-[11px] leading-relaxed text-muted-foreground">
            {(showPlaintext ? tailData.join(String.fromCharCode(10)) : tailData.length ? HIDDEN_TEXT : '') || t('logs.tailEmpty')}
          </pre>
        )}
      </div>

      {/* 底部分页栏 */}
      {logsQuery.data?.windowLimited && <p className="text-xs text-amber-600 dark:text-amber-400">{t('logs.windowLimit')}</p>}
      {includeAuditRows && auditQuery.data?.windowLimited && (
        <p className="text-xs text-amber-600 dark:text-amber-400">
          {t('logs.auditWindowLimit')}
        </p>
      )}
      <div className="flex items-center justify-between rounded-xl border bg-card px-4 py-2 text-xs">
        <div className="flex items-center gap-2">
          <span className="text-muted-foreground">{t('logs.total')} {merged.length} {t('logs.totalAudit')} {visibleAuditCount} {t('logs.auditSignals')}</span>
        </div>
        <div className="flex items-center gap-2">
          <Button size="sm" variant="outline" className="h-7 px-2.5" disabled={currentPage <= 1} onClick={() => setPage(Math.max(1, currentPage - 1))}>{t('logs.prev')}</Button>
          <span className="text-muted-foreground">{t('logs.page')} {currentPage} {t('logs.pageOf')} {Math.max(1, Math.ceil(merged.length / pageSize))} {t('logs.pages')}</span>
          <Button size="sm" variant="outline" className="h-7 px-2.5" disabled={currentPage >= Math.ceil(merged.length / pageSize)} onClick={() => setPage(currentPage + 1)}>{t('logs.next')}</Button>
          <Select value={String(pageSize)} onValueChange={(v) => { setPageSize(Number(v)); setPage(1) }}>
            <SelectTrigger className="h-7 w-[80px] text-xs"><SelectValue /></SelectTrigger>
            <SelectContent>
              <SelectItem value="50">{tf('logs.pageSizeItem', { n: 50 })}</SelectItem>
              <SelectItem value="100">{tf('logs.pageSizeItem', { n: 100 })}</SelectItem>
              <SelectItem value="200">{tf('logs.pageSizeItem', { n: 200 })}</SelectItem>
              <SelectItem value="500">{tf('logs.pageSizeItem', { n: 500 })}</SelectItem>
            </SelectContent>
          </Select>
        </div>
      </div>

      {/* 详情弹窗 */}
      <EventDetailDialog open={detailOpen} onOpenChange={setDetailOpen} seq={detailSeq} />

      {/* 审计行详情弹窗（原地查看，不再跳转审计中心） */}
      <AuditEventDetailDialog event={auditDetail} onOpenChange={(v) => !v && setAuditDetail(null)} />

      {/* 清空确认 */}
      <Dialog open={confirmClear} onOpenChange={setConfirmClear}>
        <DialogContent className="max-w-md">
          <DialogHeader>
            <DialogTitle>{t('logs.clearTitle')}</DialogTitle>
            <DialogDescription>
              {t('logs.clearDesc')}
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button size="sm" variant="outline" onClick={() => setConfirmClear(false)} disabled={clearing}>
              {t('common.cancel')}
            </Button>
            <Button size="sm" variant="destructive" onClick={doClear} loading={clearing}>
              {t('common.clear')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
