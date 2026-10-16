import { usePrivacyStore } from '@/stores/privacyStore'
import { HIDDEN_TEXT } from '@/lib/privacy'
import { useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Trash2, Radar, ShieldCheck, Info, HelpCircle, RefreshCw } from 'lucide-react'
import { getAuditEvents, clearAudit } from '@/api/audit'
import type { AuditEvent } from '@/types/api'
import { useVisibility } from '@/lib/useVisibility'
import { Card, CardContent } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Table, TableHeader, TableBody, TableRow, TableHead, TableCell } from '@/components/ui/table'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Skeleton } from '@/components/ui/skeleton'
import { AuditEventDetailDialog, severityMeta, signalInfo } from '@/components/audit/AuditEventDetailDialog'
import { AuditSettingsCard } from '@/components/audit/AuditSettingsCard'
import { cn } from '@/lib/utils'
import { toast } from '@/lib/toast'
import { useI18n } from '@/lib/i18n'
import dayjs from 'dayjs'

const AUDIT_WINDOW = 500

export default function AuditPage() {
  const { t, tf } = useI18n()
  const queryClient = useQueryClient()
  const showPlaintext = usePrivacyStore((state) => state.showPlaintext)
  const { hidden } = useVisibility()
  const [detailEvent, setDetailEvent] = useState<AuditEvent | null>(null)
  const [confirmClearAudit, setConfirmClearAudit] = useState(false)
  const [sevFilter, setSevFilter] = useState('__ALL__')
  const [sigFilter, setSigFilter] = useState('__ALL__')
  const { data: eventsData, isLoading: eventsLoading, isError: eventsError, error: eventsErrorObj, refetch: refetchEvents } = useQuery({
    queryKey: ['auditEvents'], queryFn: () => getAuditEvents(0, AUDIT_WINDOW, { floor: 'LOW' }),
    refetchInterval: hidden ? false : 3000,
  })
  const events = useMemo(() => eventsData?.events ?? [], [eventsData])
  const windowFull = events.length >= AUDIT_WINDOW
  const availableSignals = useMemo(() => [...new Set(events.map((e) => e.signal_type))].sort(), [events])
  const filteredEvents = useMemo(() => events.filter((ev) =>
    (sevFilter === '__ALL__' || ev.severity === sevFilter) &&
    (sigFilter === '__ALL__' || ev.signal_type === sigFilter)), [events, sevFilter, sigFilter])
  const clearMutation = useMutation({
    mutationFn: clearAudit,
    onSuccess: () => {
      toast(t('audit.cleared'))
      void queryClient.invalidateQueries({ queryKey: ['auditEvents'] })
      void queryClient.invalidateQueries({ queryKey: ['auditTimeline'] })
    },
    onError: (e: Error) => toast(tf('audit.clearFail', { e: e.message }), 'error'),
  })
  return (
    <div className="space-y-5">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">{t('nav.audit')}</h1>
          <p className="mt-1 text-sm text-muted-foreground">{t('audit.subtitle')}</p>
        </div>
        <Button size="sm" variant="outline" className="gap-1.5" onClick={() => setConfirmClearAudit(true)} disabled={clearMutation.isPending}>
          <Trash2 className="h-3.5 w-3.5" />{t('common.clear')}
        </Button>
      </div>
      <AuditSettingsCard />

      {/* 信号识别轻量提示 */}
      <div className="flex items-center gap-2 rounded-xl border border-blue-500/20 bg-blue-500/5 px-4 py-2.5 text-xs text-muted-foreground">
        <Info className="h-4 w-4 shrink-0 text-blue-500" />
        <span>{t('audit.judgeCompact')}</span>
      </div>

      {/* 审计事件列表 */}
      <Card className="border bg-card shadow-[var(--shadow-card)]">
        <CardContent className="p-4">
          <div className="mb-3 flex items-center gap-2">
            <Radar className="h-4 w-4 text-muted-foreground" />
            <span className="text-sm font-semibold">{t('audit.events')}</span>
            <Badge variant="outline" className="ml-auto text-xs">
              {eventsData?.count ?? 0} {t('audit.countSuffix')}
            </Badge>
          </div>

          {/* 筛选器：严重度 + 信号类型 */}
          {events.length > 0 && (
            <div className="mb-3 flex flex-wrap items-center gap-2">
              <Select value={sevFilter} onValueChange={setSevFilter}>
                <SelectTrigger className="h-8 w-36 text-xs"><SelectValue placeholder={t('audit.filterSeverity')} /></SelectTrigger>
                <SelectContent>
                  <SelectItem value="__ALL__">{t('audit.filterAllSeverity')}</SelectItem>
                  <SelectItem value="CRITICAL">{t('audit.sevCritical')}</SelectItem>
                  <SelectItem value="HIGH">{t('audit.sevHigh')}</SelectItem>
                  <SelectItem value="MEDIUM">{t('audit.sevMedium')}</SelectItem>
                  <SelectItem value="LOW">{t('audit.sevLow')}</SelectItem>
                </SelectContent>
              </Select>
              <Select value={sigFilter} onValueChange={setSigFilter}>
                <SelectTrigger className="h-8 w-44 text-xs"><SelectValue placeholder={t('audit.filterSignal')} /></SelectTrigger>
                <SelectContent>
                  <SelectItem value="__ALL__">{t('audit.filterAllSignals')}</SelectItem>
                  {availableSignals.map((s) => (
                    <SelectItem key={s} value={s}>{t(signalInfo(s).labelKey ?? s)}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
              {(sevFilter !== '__ALL__' || sigFilter !== '__ALL__') && (
                <Button size="sm" variant="ghost" className="h-8 px-2 text-xs" onClick={() => { setSevFilter('__ALL__'); setSigFilter('__ALL__') }}>
                  {t('common.reset')}
                </Button>
              )}
              {/* 窗口填满时把「筛选作用域」说出来（审计 L8）：筛选在前端做，
                  命中项若在窗口之外就看不到，此时「无匹配」不能读作「不存在」。 */}
              {windowFull && (sevFilter !== '__ALL__' || sigFilter !== '__ALL__') && (
                <span className="text-[11px] text-amber-600 dark:text-amber-500">
                  {tf('audit.filterWindowHint', { n: AUDIT_WINDOW })}
                </span>
              )}
            </div>
          )}

          {eventsLoading && events.length === 0 ? (
            <div className="space-y-2">
              {[0, 1, 2].map((i) => (
                <Skeleton key={i} className="h-10 w-full" />
              ))}
            </div>
          ) : eventsError ? (
            /* 读失败与「零发现」必须视觉可分（审计 L6）。这里刻意用红色警示而不是绿色：
               引擎没起来 / 请求被拒 / 后端 5xx 时，「什么都没读到」的正确解读是
               「审计这一层现在是瞎的」，而不是「一切正常」。 */
            <div className="flex flex-col items-center justify-center rounded-xl border border-dashed border-destructive/40 bg-destructive/5 py-12 text-center">
              <div className="flex h-12 w-12 items-center justify-center rounded-full bg-destructive/10 text-destructive">
                <HelpCircle className="h-6 w-6" />
              </div>
              <h4 className="mt-3 text-sm font-semibold text-foreground">{t('audit.loadFailTitle')}</h4>
              <p className="mt-1 max-w-md text-xs text-muted-foreground">
                {t('audit.loadFailDesc')}
              </p>
              {eventsErrorObj ? (
                <p className="mt-1 max-w-md break-all font-mono text-[11px] text-destructive/80">
                  {String((eventsErrorObj as Error)?.message || eventsErrorObj)}
                </p>
              ) : null}
              <Button
                size="sm"
                variant="outline"
                className="mt-4 gap-1.5 border-destructive/40 text-destructive hover:bg-destructive/10"
                onClick={() => { void refetchEvents() }}
              >
                <RefreshCw className="h-3.5 w-3.5" />
                {t('common.reload')}
              </Button>
            </div>
          ) : events.length === 0 ? (
            <div className="flex flex-col items-center justify-center rounded-xl border border-dashed border-emerald-500/30 bg-emerald-500/5 py-12 text-center">
              <div className="flex h-12 w-12 items-center justify-center rounded-full bg-emerald-500/10 text-emerald-600 dark:text-emerald-400">
                <ShieldCheck className="h-6 w-6" />
              </div>
              <h4 className="mt-3 text-sm font-semibold text-foreground">{t('audit.emptySafeTitle')}</h4>
              <p className="mt-1 max-w-md text-xs text-muted-foreground">
                {t('audit.emptySafeDesc')}
              </p>

            </div>
          ) : filteredEvents.length === 0 ? (
            <div className="py-10 text-center text-sm text-muted-foreground">
              <Radar className="mx-auto mb-3 h-8 w-8 text-muted-foreground/50" />
              {t('audit.noEventsAfterFilter')}
              {/* 窗口满时补一句限定语：筛选只作用于已加载的 N 条（审计 L8）。 */}
              {windowFull && (
                <p className="mt-1.5 text-xs text-amber-600 dark:text-amber-500">
                  {tf('audit.noEventsWindowNote', { n: AUDIT_WINDOW })}
                </p>
              )}
            </div>
          ) : (
            <div className="overflow-x-auto">
              <Table className="w-full text-left text-[13px]">
                <TableHeader>
                  <TableRow className="border-b text-xs text-muted-foreground">
                    <TableHead className="pb-2 pr-3 font-medium">{t('audit.colSeverity')}</TableHead>
                    <TableHead className="pb-2 pr-3 font-medium">{t('audit.colSignal')}</TableHead>
                    <TableHead className="pb-2 pr-3 font-medium">{t('audit.colTime')}</TableHead>
                    <TableHead className="pb-2 pr-3 font-medium">{t('audit.colRequest')}</TableHead>
                    <TableHead className="pb-2 font-medium">{t('audit.colEvidence')}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody className="divide-y divide-border/60">
                  {filteredEvents.map((ev) => {
                    const sev = severityMeta(ev.severity)
                    const req = [ev.method, ev.path].filter(Boolean).join(' ')
                    const info = signalInfo(ev.signal_type)
                    return (
                      <TableRow
                        key={ev.seq}
                        className="cursor-pointer align-top transition-colors hover:bg-muted/30"
                        onClick={() => setDetailEvent(ev)}
                        title={`${t('common.viewDetail')}：${t(info.labelKey ?? info.label ?? '')}`}
                      >
                        <TableCell className="py-2 pr-3">
                          <Badge variant="outline" className={cn('shrink-0 rounded-full text-[11px]', sev.cls)}>
                            {t(sev.labelKey)}
                          </Badge>
                        </TableCell>
                        <TableCell className="py-2 pr-3">
                          <span className="font-mono text-xs font-medium">{t(info.labelKey ?? info.label ?? '')}</span>
                          {ev.action && <Badge variant="outline" className="ml-2 text-[10px]">{t(`audit.guard.result.${ev.action}`)}</Badge>}
                        </TableCell>
                        <TableCell className="whitespace-nowrap py-2 pr-3 text-xs text-muted-foreground">
                          {dayjs(ev.ts * 1000).format('MM-DD HH:mm:ss')}
                        </TableCell>
                        <TableCell className="max-w-[200px] truncate py-2 pr-3 font-mono text-xs text-muted-foreground">
                          {req || ev.host || '—'}
                        </TableCell>
                        <TableCell className="max-w-[240px] truncate py-2 text-xs text-muted-foreground">
                          {ev.evidence ? showPlaintext ? ev.evidence : HIDDEN_TEXT : '—'}
                        </TableCell>
                      </TableRow>
                    )
                  })}
                </TableBody>
              </Table>
            </div>
          )}
        </CardContent>
      </Card>


      <AuditEventDetailDialog event={detailEvent} onOpenChange={(v) => !v && setDetailEvent(null)} />
      {/* 清空审计确认弹窗（审计 L8） */}
      <Dialog open={confirmClearAudit} onOpenChange={setConfirmClearAudit}>
        <DialogContent className="max-w-md">
          <DialogHeader>
            <DialogTitle>{t('audit.clearTitle')}</DialogTitle>
            <DialogDescription>
              {t('audit.clearDesc')}
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button size="sm" variant="outline" onClick={() => setConfirmClearAudit(false)} disabled={clearMutation.isPending}>
              {t('common.cancel')}
            </Button>
            <Button size="sm" variant="destructive" onClick={() => {
              setConfirmClearAudit(false)
              clearMutation.mutate()
            }} loading={clearMutation.isPending}>
              {t('common.clear')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

    </div>
  )
}
