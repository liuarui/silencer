import { usePrivacyStore } from '@/stores/privacyStore'
import { PlaintextSwitch } from '@/components/common/PlaintextSwitch'
import { projectPrivateBodies } from '@/lib/privacy'
/**
 * 事件详情弹窗：/api/logs/detail 单条回源（slim 列表无明文，仅此处展示）
 * 明文只进详情（AGENTS.md 红线）；MASK 行 dialog=用户消息，RESTORE 行 dialog=助手回复
 */
import { useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { getLogDetail } from '@/api/logs'
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { Switch } from '@/components/ui/switch'
import { EventTypeIcon, getEventTypeMeta } from '@/components/events/EventTypeIcon'
import { CheckCircle2, ChevronDown } from 'lucide-react'
import type { SilencerEvent } from '@/types/api'
import dayjs from 'dayjs'
import { cn, copyText } from '@/lib/utils'
import { useI18n } from '@/lib/i18n'
import { getMatchedRules } from '@/lib/log-events'

function MetaItem({ k, v, wide }: { k: string; v: React.ReactNode; wide?: boolean }) {
  return (
    <div className={cn('rounded-lg bg-muted/50 p-2.5', wide && 'col-span-2')}>
      <div className="text-[11px] text-muted-foreground">{k}</div>
      <div className="mt-0.5 break-all text-[13px]">{v}</div>
    </div>
  )
}

function formatDuration(ms?: number | null): string {
  if (ms == null) return '—'
  if (ms < 1000) return `${Math.round(ms)}ms`
  const s = ms / 1000
  if (s < 60) return `${s.toFixed(1)}s`
  const m = Math.floor(s / 60)
  const rest = Math.round(s % 60)
  return `${m}m ${rest}s`
}

/**
 * 在正文里高亮「被还原回来的原文」。
 *
 * 存在的理由：还原是这个软件的核心动作，但对着一段几千字的回复，
 * 用户根本看不出哪几个字是刚被换回来的。上面的对照表告诉你「换了什么」，
 * 这里告诉你「换在哪」——两者合起来才构成一次可核对的还原。
 *
 * 只高亮 items 里带 original 的项：凭据类只有 preview + sha256 摘要，
 * 本来就没有明文可匹配（AGENTS.md 红线，凭据永不明文落库）。
 */
function highlightOriginals(text: string, originals: string[]): React.ReactNode {
  const uniq = [...new Set(originals.filter(Boolean))]
  if (!uniq.length) return text
  // 长的排前面：「张三丰」和「张三」同时存在时，先匹配长的，
  // 否则短的会把长的切成两半，高亮范围就错了
  uniq.sort((a, b) => b.length - a.length)
  const rx = new RegExp(uniq.map((s) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('|'), 'g')
  const out: React.ReactNode[] = []
  let last = 0
  for (const m of text.matchAll(rx)) {
    const i = m.index ?? 0
    if (i > last) out.push(text.slice(last, i))
    out.push(
      <mark
        key={`${i}-${m[0]}`}
        className="rounded bg-emerald-500/25 px-0.5 text-foreground ring-1 ring-emerald-500/40"
      >
        {m[0]}
      </mark>,
    )
    last = i + m[0].length
  }
  if (last < text.length) out.push(text.slice(last))
  return out
}

function highlightTerms(text: string, terms: string[], className: string): React.ReactNode {
  const uniq = [...new Set(terms.filter(Boolean))].sort((a, b) => b.length - a.length)
  if (!uniq.length) return text
  const rx = new RegExp(uniq.map((s) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('|'), 'g')
  const out: React.ReactNode[] = []
  let last = 0
  for (const match of text.matchAll(rx)) {
    const at = match.index ?? 0
    if (at > last) out.push(text.slice(last, at))
    out.push(<mark key={`${at}-${match[0]}`} className={className}>{match[0]}</mark>)
    last = at + match[0].length
  }
  if (last < text.length) out.push(text.slice(last))
  return out
}

function CopyBtn({ text }: { text: string }) {
  const { t } = useI18n()
  const [copied, setCopied] = useState(false)
  return (
    <Button
      size="sm"
      variant="ghost"
      className="h-6 px-2 text-[11px]"
      onClick={async () => {
        try {
          await copyText(text)
          setCopied(true)
          setTimeout(() => setCopied(false), 1500)
        } catch {
          // 剪贴板不可用时静默
        }
      }}
    >
      {copied ? t('detail.copied') : t('detail.copy')}
    </Button>
  )
}

export function EventDetailDialog({
  open,
  onOpenChange,
  seq,
}: {
  open: boolean
  onOpenChange: (v: boolean) => void
  seq: number | string | null
}) {
  const { t, tf } = useI18n()
  // 高亮默认关：先让用户看到未加工的原文，要核对时再点开。
  // 默认开会让每次打开详情都是一片荧光绿，反而看不出重点。
  const [hl, setHl] = useState(false)
  const { data, isLoading } = useQuery({
    queryKey: ['logDetail', seq],
    queryFn: () => getLogDetail(seq!),
    enabled: open && seq != null,
    retry: 1,
  })

  const showPlaintext = usePrivacyStore((state) => state.showPlaintext)
  const event: SilencerEvent | undefined = useMemo(() => data?.ok && data.event ? projectPrivateBodies(data.event, showPlaintext) : undefined, [data, showPlaintext])

  // 可高亮的原文：凭据类只有 preview + sha256，没有明文可匹配
  const originals = useMemo(
    () => ((event?.items ?? []) as { original?: string }[])
      .map((i) => i.original)
      .filter((s): s is string => typeof s === 'string' && s.length > 0),
    [event],
  )

  const restoredOriginals = useMemo(
    () => (event?.items ?? []).filter((item) => item.restored === true)
      .map((item) => item.original).filter((value): value is string => typeof value === 'string' && value.length > 0),
    [event],
  )

  const ruleLabels = useMemo(
    () => getMatchedRules(event),
    [event],
  )

  const stageInfo = useMemo(() => {
    if (!event) return null
    const meta = getEventTypeMeta(event.type)
    switch (event.type) {
      case 'RESTORE':
        return {
          label: t('detail.stageRestore'),
          className: 'bg-emerald-600 text-white hover:bg-emerald-600',
          desc: t('detail.stageRestoreDesc'),
        }
      case 'MASK':
        return {
          label: t('detail.stageMask'),
          className: 'bg-blue-600 text-white hover:bg-blue-600',
          desc: t('detail.stageMaskDesc'),
        }
      case 'BLOCK':
        return {
          label: t('evt.block'),
          className: 'bg-red-600 text-white hover:bg-red-600',
          desc: t('detail.stageBlockDesc'),
        }
      case 'ERR':
        return {
          label: t('evt.err'),
          className: 'bg-red-600 text-white hover:bg-red-600',
          desc: t('detail.stageErrDesc'),
        }
      case 'SCAN_WARN':
        return {
          label: t('evt.scanWarn'),
          className: 'bg-amber-600 text-white hover:bg-amber-600',
          desc: t('detail.stageScanWarnDesc'),
        }
      default:
        return {
          label: meta.labelKey ? t(meta.labelKey) : (meta.label || event.type),
          className: 'bg-slate-600 text-white hover:bg-slate-600',
          desc: t('detail.stageOtherDesc'),
        }
    }
  }, [event, t])

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[85vh] max-w-3xl overflow-y-auto">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            {event && <EventTypeIcon type={event.type} className="h-4 w-4" />}
            {t('detail.title')}
            {event && (
              <span className="text-xs font-normal text-muted-foreground">#{event.seq}</span>
            )}
          </DialogTitle>
        </DialogHeader>
        <div className="flex justify-end"><PlaintextSwitch /></div>

        {isLoading && (
          <div className="space-y-3">
            <Skeleton className="h-24 w-full" />
            <Skeleton className="h-40 w-full" />
          </div>
        )}

        {!isLoading && !event && (
          <p className="py-8 text-center text-sm text-muted-foreground">
            {t('detail.notFound')}
          </p>
        )}

        {event && (
          <div className="space-y-5">
            {event.noise_whitelisted && <Badge variant="secondary">{t('logs.noiseWhitelisted')}</Badge>}
            {/* 基本信息 */}
            <div className="grid grid-cols-2 gap-2 md:grid-cols-3">
              <MetaItem
                k={t('detail.type')}
                v={
                  <span className="font-mono text-xs font-semibold">{event.type}</span>
                }
              />
              <MetaItem k={t('detail.time')} v={dayjs(event.ts * 1000).format('YYYY-MM-DD HH:mm:ss')} />
              <MetaItem k={t('detail.session')} v={<code className="text-xs">{(event.sid || '—').slice(0, 16)}{event.sid ? '…' : ''}</code>} />
              <MetaItem k={t('detail.upstream')} v={<span className="text-xs font-medium">{event.upstream || (event as { client_app?: string }).client_app || '—'}</span>} />
              <MetaItem k={t('detail.model')} v={<code className="text-xs">{event.model || '—'}</code>} />
              <MetaItem
                k={t('detail.status')}
                v={
                  <Badge variant={!event.noise_whitelisted && event.http_status && event.http_status >= 400 ? 'destructive' : 'outline'}>
                    {event.http_status ?? event.status ?? '-'}
                  </Badge>
                }
              />
              <MetaItem
                k={t('detail.path')}
                wide
                v={<code className="text-xs">{event.method} {event.host}{event.path}</code>}
              />
              <MetaItem k={t('detail.duration')} v={formatDuration(event.total_ms ?? event.upstream_ms ?? event.first_byte_ms)} />
              {event.error && <MetaItem k={t('detail.failureReason')} wide v={t(`logs.failure.${event.error}`) === `logs.failure.${event.error}` ? event.error : t(`logs.failure.${event.error}`)} />}
              {event.failure_stage && <MetaItem k={t('detail.failureStage')} v={t(`logs.failureStage.${event.failure_stage}`) === `logs.failureStage.${event.failure_stage}` ? event.failure_stage : t(`logs.failureStage.${event.failure_stage}`)} />}
            </div>

            {/* 流式信息（stream_actual 与 stream_mode 背离提示） */}
            {event.stream_mode && (
              <div className="flex flex-wrap items-center gap-2 rounded-lg border bg-muted/40 p-2.5 text-xs">
                <span className="text-muted-foreground">{t('detail.stream')}</span>
                <Badge variant="outline">{event.stream_mode}</Badge>
                {event.stream_actual && (
                  <>
                    <span className="text-muted-foreground">{t('detail.actual')}</span>
                    <Badge
                      variant={event.stream_actual === 'stream' ? 'outline' : 'secondary'}
                      className={cn(
                        event.stream_actual === 'stream_error' &&
                          'border-amber-500/40 text-amber-600 dark:text-amber-400',
                      )}
                    >
                      {event.stream_actual === 'stream'
                        ? t('detail.streamMode')
                        : event.stream_actual === 'whole'
                          ? t('detail.wholeFallback')
                          : t('detail.streamError')}
                    </Badge>
                  </>
                )}
              </div>
            )}

            {/* msg / reason 提示（_reasoning_effort_hint 等排查信息） */}
            {(event.msg || event.reason) && (
              <div className="rounded-lg border border-amber-500/30 bg-amber-500/5 p-3 text-xs leading-relaxed text-amber-700 dark:text-amber-400">
                {event.msg && <div className="whitespace-pre-wrap">{event.msg}</div>}
                {event.reason && <div className="mt-1 text-muted-foreground">{event.reason}</div>}
              </div>
            )}

            {/* 顶栏链路全景图：直观告知用户本条请求是 脱敏请求(出站) 还是 还原回复(入站) 或 异常/直连 */}
            {stageInfo && (
              <div className="rounded-lg border bg-muted/20 p-3">
                <div className="flex items-center justify-between gap-2">
                  <div className="flex items-center gap-2">
                    <span className="text-xs font-semibold text-muted-foreground">{t('detail.pipelineStage')}</span>
                    <Badge
                      className={cn(
                        'text-xs font-mono',
                        stageInfo.className
                      )}
                    >
                      {stageInfo.label}
                    </Badge>
                  </div>
                  <div className="flex items-center gap-3 text-xs">
                    {(event.count ?? 0) > 0 && (
                      <span className="text-blue-600 dark:text-blue-400">
                        {t(event.type === 'RESTORE' ? 'detail.responsePlaceholders' : 'logs.colMasked')} <strong>{event.count}</strong>
                      </span>
                    )}
                    {(event.restored ?? 0) > 0 && (
                      <span className="text-emerald-600 dark:text-emerald-400">
                        {t('logs.colRestored')} <strong>{event.restored}</strong>
                      </span>
                    )}
                    {(event.unresolved ?? 0) > 0 && (
                      <span className="text-amber-600 dark:text-amber-400">
                        {t('logs.colUnresolved')} <strong>{event.unresolved}</strong>
                      </span>
                    )}
                    {(event.degraded ?? 0) > 0 && (
                      <span className="text-muted-foreground">
                        {t('logs.colDegraded')} <strong>{event.degraded}</strong>
                      </span>
                    )}
                  </div>
                </div>
              <p className="mt-1.5 text-[11px] text-muted-foreground leading-relaxed">
                {stageInfo.desc}
              </p>
              {(event.type === 'MASK' || event.type === 'RESTORE') && (
                <div className="mt-2 flex flex-wrap items-center gap-1.5 text-xs">
                  <span className="text-muted-foreground">{t('detail.matchedRules')}</span>
                  {ruleLabels.length > 0 ? ruleLabels.map((label) => (
                    <Badge key={label} variant="outline" className="font-mono text-[10px]">
                      {label}
                    </Badge>
                  )) : (
                    <span className="text-muted-foreground">
                      {event.matched_rules == null && ((event.count ?? 0) > 0 || (event.restored ?? 0) > 0)
                        ? t('detail.ruleSnapshotUnavailable')
                        : t('detail.noMatchedRules')}
                    </span>
                  )}
                </div>
              )}
            </div>
            )}

            {/* 脱敏/还原项目对照（明文 → 占位符 / 占位符 → 明文） */}
            {event.items && event.items.length > 0 ? (
              <details key={`${seq}-items`} className="group rounded-lg border">
                <summary className="flex cursor-pointer list-none items-center gap-2 rounded-lg px-3 py-2.5 text-[13px] font-semibold marker:hidden focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">
                  <ChevronDown className="h-4 w-4 shrink-0 -rotate-90 text-muted-foreground transition-transform group-open:rotate-0" aria-hidden="true" />
                  <span className="min-w-0 flex-1">
                    {event.type === 'RESTORE' && (event.restored ?? 0) > 0
                      ? tf('detail.restoreItemsWithTotal', { n: event.items.length, total: event.restored })
                      : tf(event.type === 'RESTORE' ? 'detail.restoreItems' : 'detail.maskItems', { n: event.items.length })}
                  </span>
                  <span className="shrink-0 text-xs font-normal text-muted-foreground group-open:hidden">{t('detail.expandItems')}</span>
                  <span className="hidden shrink-0 text-xs font-normal text-muted-foreground group-open:inline">{t('detail.collapseItems')}</span>
                </summary>
                <div className="space-y-2 border-t p-3">
                  {(event.items as { label: string; original?: string; preview?: string; tok?: string; hash?: string; length?: number; restored?: boolean; restore_status?: string; restored_count?: number; unresolved_count?: number; from_history?: boolean }[]).map(
                    (item, idx) => {
                      const isRestored = item.restored === true
                      const statusKey = item.restore_status === 'not_mentioned' ? 'detail.itemNotRestored'
                        : item.restore_status === 'unresolved' ? 'detail.itemUnresolved'
                        : item.restore_status === 'unmapped' ? 'detail.itemUnmapped'
                        : 'detail.itemUnknown'
                      return (
                        <div key={item.label + idx} className={cn("rounded-lg border bg-card p-3", isRestored && "border-emerald-500/30 bg-emerald-500/5")}>
                          <div className="flex items-center justify-between gap-2">
                            <div className="flex items-center gap-2">
                              {isRestored && (
                                <span className="flex items-center gap-1 rounded bg-emerald-500/15 px-1.5 py-0.5 text-[10px] font-medium text-emerald-600 dark:text-emerald-400" title={t('detail.itemRestored')}>
                                  <CheckCircle2 className="h-3 w-3" />
                                  <span>{t('detail.itemRestored')}{item.restored_count != null ? ` ×${item.restored_count}` : ''}</span>
                                </span>
                              )}
                              {event.type === 'RESTORE' && (!isRestored || (item.unresolved_count ?? 0) > 0) && (
                                <span className="flex items-center gap-1 rounded bg-muted px-1.5 py-0.5 text-[10px] font-medium text-muted-foreground" title={t(statusKey)}>
                                  <span className="h-1.5 w-1.5 rounded-full bg-muted-foreground/50" />
                                  <span>{t(statusKey)}{(item.unresolved_count ?? 0) > 0 ? ` ×${item.unresolved_count}` : ''}</span>
                                </span>
                              )}
                              <Badge variant="outline" className="font-mono text-[11px]">
                                {item.label}
                              </Badge>
                              {item.from_history && (
                                <Badge variant="secondary" className="text-[10px] font-normal text-muted-foreground">
                                  {t('detail.fromHistory')}
                                </Badge>
                              )}
                            </div>
                            {item.original != null && <CopyBtn text={item.original} />}
                          </div>
                          <div className="mt-2 space-y-1.5 font-mono text-xs leading-relaxed">
                            {item.original != null && (
                              <div className="flex items-start gap-2">
                                <span className="w-12 shrink-0 text-muted-foreground">{t('detail.original')}</span>
                                <span className="break-all font-semibold text-foreground">{item.original}</span>
                              </div>
                            )}
                            {item.preview != null && (
                              <div className="flex items-start gap-2">
                                <span className="w-12 shrink-0 text-muted-foreground">{t('detail.preview')}</span>
                                <span className="break-all text-muted-foreground">{item.preview}</span>
                              </div>
                            )}
                            {item.tok != null && (
                              <div className="flex items-start gap-2">
                                <span className="w-12 shrink-0 text-muted-foreground">{t('detail.placeholder')}</span>
                                <span className="break-all rounded bg-blue-500/10 px-1 text-blue-600 dark:text-blue-400">
                                  {item.tok}
                                </span>
                              </div>
                            )}
                            {item.hash != null && (
                              <div className="flex items-start gap-2 text-muted-foreground">
                                <span className="w-12 shrink-0">{t('detail.hash')}</span>
                                <span className="break-all">{item.hash}</span>
                                {item.length != null && (
                                  <span className="shrink-0">({tf('detail.length', { n: item.length })})</span>
                                )}
                              </div>
                            )}
                          </div>
                        </div>
                      )
                    },
                  )}
                </div>
              </details>
            ) : event.type === 'RESTORE' && (event.restored ?? 0) > 0 ? (
              <div className="rounded-lg border bg-muted/20 p-3 text-xs text-muted-foreground leading-relaxed">
                <div className="font-medium text-foreground">{t('detail.noItemsTitle')}</div>
                <p className="mt-1">{tf('detail.noItemsDesc', { n: event.restored })}</p>
              </div>
            ) : null}

            {event.detail_recorded && (event.raw || event.masked || event.upstream_response || event.restored_response) && (
              <div>
                <div className="mb-2 flex items-center justify-between">
                  <h3 className="text-[13px] font-semibold">{t('logs.ledgerTitle')}</h3>
                  <span className="text-[11px] text-muted-foreground">{t('logs.ledgerDescription')}</span>
                </div>
                {(event.truncated ?? 0) > 0 && <p className="mb-2 text-xs text-amber-600 dark:text-amber-400">{t('detail.bodyTruncated')}</p>}
                <div className="grid gap-3 md:grid-cols-2">
                  {([
                    ['raw', 'logs.ledger.raw', 'original'],
                    ['masked', 'logs.ledger.masked', 'placeholder'],
                    ['upstream_response', 'logs.ledger.upstreamResponse', 'placeholder'],
                    ['restored_response', 'logs.ledger.restoredResponse', 'original'],
                  ] as const).map(([key, label, mode]) => {
                    const body = event[key]
                    if (!body) return null
                    const terms = mode === 'original'
                      ? (key === 'restored_response' ? restoredOriginals : originals)
                      : (event.items ?? []).map((item) => ('tok' in item ? item.tok : undefined) || ('preview' in item ? item.preview : undefined) || '').filter(Boolean)
                    return (
                      <div key={key}>
                        <div className="mb-1 flex min-h-7 items-center justify-between gap-2">
                          <span className="text-[10px] text-muted-foreground">{t(label)}</span>
                          <div className="flex items-center gap-2">
                            {terms.length > 0 && (
                              <label className="flex cursor-pointer select-none items-center gap-1 text-[10px] text-muted-foreground" title={t('detail.highlightTitle')}>
                                <Switch checked={hl} onCheckedChange={setHl} className="scale-75" />
                                <span>{t('detail.highlight')}</span>
                              </label>
                            )}
                            <CopyBtn text={body} />
                          </div>
                        </div>
                        <pre className="max-h-52 overflow-auto whitespace-pre-wrap break-all rounded-lg border bg-muted/40 p-3 font-mono text-xs leading-relaxed">
                          {hl ? highlightTerms(body, terms, mode === 'original'
                            ? 'rounded bg-emerald-500/25 px-0.5 ring-1 ring-emerald-500/40'
                            : 'rounded bg-blue-500/15 px-0.5 text-blue-700 ring-1 ring-blue-500/30 dark:text-blue-300') : body}
                        </pre>
                      </div>
                    )
                  })}
                </div>
                {!hl && originals.length > 0 && (
                  <p className="mt-2 text-[11px] text-muted-foreground">{t('detail.highlightTitle')}</p>
                )}
              </div>
            )}

            {/* 用户请求原文（dialog_req） */}
            {event.dialog_req && (
              <div>
                <div className="mb-2 flex items-center justify-between">
                  <div className="flex items-center gap-2">
                    <h3 className="text-[13px] font-semibold">{t('detail.reqOriginal')}</h3>
                    <span className="text-[11px] text-muted-foreground">
                      {event.dialog_req.length.toLocaleString()} {t('detail.chars')} · {tf('detail.approxTokens', { n: Math.ceil(event.dialog_req.length / 3.5).toLocaleString() })}
                    </span>
                  </div>
                  <div className="flex items-center gap-2">
                    {originals.length > 0 && (
                      <label className="flex cursor-pointer select-none items-center gap-1.5 text-xs text-muted-foreground transition-colors hover:text-foreground" title={t('detail.highlightTitle')}>
                        <Switch
                          checked={hl}
                          onCheckedChange={setHl}
                          className="scale-75"
                        />
                        <span>{t('detail.highlight')}</span>
                      </label>
                    )}
                    <CopyBtn text={event.dialog_req} />
                  </div>
                </div>
                <pre className="max-h-60 overflow-auto whitespace-pre-wrap break-all rounded-lg border bg-muted/40 p-3 font-mono text-xs leading-relaxed">
                  {hl ? highlightOriginals(event.dialog_req, originals) : event.dialog_req}
                </pre>
              </div>
            )}
            {/* 对话内容（明文） */}
            {event.dialog && (
              <div>
                <div className="mb-2 flex items-center justify-between">
                  <div className="flex items-center gap-2">
                    <h3 className="text-[13px] font-semibold">
                      {event.type === 'MASK' ? t('detail.userMsg') : event.type === 'RESTORE' ? t('detail.assistantMsg') : t('detail.bodyText')}
                    </h3>
                    <span className="text-[11px] text-muted-foreground">
                      {event.dialog.length.toLocaleString()} {t('detail.chars')} · {tf('detail.approxTokens', { n: Math.ceil(event.dialog.length / 3.5).toLocaleString() })}
                    </span>
                  </div>
                  <div className="flex items-center gap-2">
                    {originals.length > 0 && (
                      <label className="flex cursor-pointer select-none items-center gap-1.5 text-xs text-muted-foreground transition-colors hover:text-foreground" title={t('detail.highlightTitle')}>
                        <Switch
                          checked={hl}
                          onCheckedChange={setHl}
                          className="scale-75"
                        />
                        <span>{t('detail.highlight')}</span>
                      </label>
                    )}
                    <CopyBtn text={event.dialog} />
                  </div>
                </div>
                <pre className="max-h-72 overflow-auto whitespace-pre-wrap break-all rounded-lg border bg-muted/40 p-3 font-mono text-xs leading-relaxed">
                  {hl ? highlightOriginals(event.dialog, originals) : event.dialog}
                </pre>
              </div>
            )}

            {/* 正文预览兜底（当无全量 dialog 时展示截断预览） */}
            {!event.dialog_req && !event.dialog && Boolean(event.req_preview || (event as { resp_preview?: string }).resp_preview) && (
              <div className="space-y-3">
                {event.req_preview && (
                  <div>
                    <h3 className="mb-2 text-[13px] font-semibold">{t('detail.reqOriginal')}</h3>
                    <pre className="max-h-60 overflow-auto whitespace-pre-wrap break-all rounded-lg border bg-muted/40 p-3 font-mono text-xs leading-relaxed">
                      {event.req_preview}
                    </pre>
                  </div>
                )}
                {(event as { resp_preview?: string }).resp_preview && (
                  <div>
                    <h3 className="mb-2 text-[13px] font-semibold">{t('detail.assistantMsg')}</h3>
                    <pre className="max-h-60 overflow-auto whitespace-pre-wrap break-all rounded-lg border bg-muted/40 p-3 font-mono text-xs leading-relaxed">
                      {(event as { resp_preview?: string }).resp_preview}
                    </pre>
                  </div>
                )}
              </div>
            )}
          </div>
        )}
      </DialogContent>
    </Dialog>
  )
}
