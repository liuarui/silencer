/**
 * 普通事件日志的共享合并与排序逻辑（Dashboard 最近日志 与 Logs 拦截日志共用）。
 *
 * 需求背景：一次 LLM 请求会产生 MASK（请求脱敏）+ RESTORE（回复还原）两条事件，
 * 若原样展示会在一张表里占两行、且顺序按接口返回倒序不稳定，用户要求「一次请求
 * 对应一条日志」。因此按 sid 把 MASK/RESTORE 合成一行「往返链路」。
 *
 * 合并语义（Logs 与 Dashboard 共用）：
 * - MASK 提供：ts/host/path/count(脱敏数)/upstream(客户端名)/mask_ms
 * - RESTORE 提供：restored/http_status/upstream_ms/total_ms/cost_usd/status/model
 * - unresolved/degraded 只在 RESTORE 事件上产生，必须带过来，否则合并行永远
 *   显示不出「未还原 / 兜底还原」。
 * - 详情回源优先 RESTORE 的 seq（_detailSeq），弹窗才能同时含请求+回复完整链路。
 * - 无 sid 的事件（CANCEL/DNS_ERROR/ERR/SCAN_WARN 等）不与任何行合并，保持独立。
 *
 * 审计行（SCAN_WARN 审计信号）由调用方在合并结果之上再并入，本模块不负责。
 */
import type { AuditEvent, EventType, SilencerEvent } from '@/types/api'

/** 合并结果里的普通事件行。_audit=false 与审计行区分；_detailSeq 供详情回源。 */
export type MergedEvent = SilencerEvent & { _audit: boolean; _detailSeq?: number; _detailId?: string; _eventTypes?: EventType[] }

export interface MergeOptions {
  /** 事件类型筛选。传 "全部" 哨兵（FILTER_ALL）时不过滤，与 Logs 页常量对齐。 */
  filterType?: string
  /** 隐藏正常放行；错误和取消始终保留。 */
  hideNoise?: boolean
}

/** Logs 页「全部类型」筛选项的值（哨兵，不会与事件类型撞）。 */
export const FILTER_ALL = '__ALL__'

/** 隐藏正常放行；取消和 DNS 错误仍需要在日志中可见。 */
const NOISE_TYPES = new Set(['SKIP', 'PASS', 'BYPASS'])

type DisplayInput = Pick<SilencerEvent, 'type'> & Partial<Pick<SilencerEvent, 'status' | 'http_status' | 'error' | 'display_type' | 'count' | 'mask_count' | 'restored' | 'unresolved' | 'degraded' | 'noise_whitelisted' | 'ingress' | 'failure_stage'>>

export function hasSensitiveEvidence(event: DisplayInput): boolean {
  return [event.count, event.mask_count, event.restored, event.unresolved, event.degraded].some((n) => (n ?? 0) > 0)
}

/** Shared by the type label, filters and noise suppression. */
export function getDisplayEventType(event: DisplayInput): EventType {
  if (event.type !== 'MASK' && event.type !== 'RESTORE') return event.type
  const code = event.http_status ?? Number(event.status)
  if (event.error === 'canceled' || event.error === 'deadline_exceeded' || code === 499) return 'CANCEL'
  if (event.error === 'dns_error') return 'DNS_ERROR'
  if (event.noise_whitelisted === true && event.ingress === 'proxy' && code === 404 && !event.error && !event.failure_stage && !(event.unresolved ?? 0) && event.status !== 'error' && (!Number(event.status) || Number(event.status) === 404)) return 'SKIP'
  if (event.error || event.status === 'error' || code >= 400) return 'ERR'
  if (hasSensitiveEvidence(event)) return event.type
  return event.display_type ?? 'PASS'
}

export function auditEventMatches(event: AuditEvent, opts: { filterType: string; ingress: string; sensitive: boolean; q: string }): boolean {
  if (opts.sensitive || opts.ingress !== '__all__') return false
  if (opts.filterType !== FILTER_ALL && opts.filterType !== 'SCAN_WARN') return false
  const query = opts.q.trim().toLowerCase()
  return !query || [event.sid, event.host, event.method, event.path, event.signal_type, event.severity, event.evidence, event.probe_id, event.request_hash, event.response_hash]
    .some((value) => typeof value === 'string' && value.toLowerCase().includes(query))
}

export function getLogPage(page: number, count: number, pageSize: number): number {
  return Math.max(1, Math.min(page, Math.max(1, Math.ceil(count / pageSize))))
}

export function getMatchedRules(event?: SilencerEvent): string[] {
  const labels = event?.matched_rules ?? event?.items?.map((item) => item.label) ?? []
  return Array.from(new Set(labels.filter((label) => typeof label === 'string' && label.length > 0)))
}

/**
 * 把普通事件列表合并为「一次请求一行」并按 ts 最新在前排序。
 *
 * @param events 原始事件列表（SilencerEvent[]）
 * @param opts 筛选选项；缺省时全部保留、不隐藏噪声
 * @returns 合并后的行，最新 ts 在前
 */
export function mergeMaskRestore(events: readonly SilencerEvent[], opts: MergeOptions = {}): MergedEvent[] {
  const { filterType, hideNoise } = opts

  const bySid = new Map<string, MergedEvent>()
  // 已成对（MASK+RESTORE 已合并）的 sid：再来的同类事件必须独立成行，绝不静默覆盖已成对的那行
  const paired = new Set<string>()
  const independent: MergedEvent[] = []
  for (const e of events) {
    const sid = e.sid || ''
    // 仅 MASK 与 RESTORE 参与会话往返链路合并；其余类型（ERR/BLOCK/SCAN_WARN/CANCEL 等）即便带 sid 也必须独立成行展示，绝不吞日志
    if (!sid || (e.type !== 'MASK' && e.type !== 'RESTORE')) {
      independent.push({ ...e, _audit: false, _detailSeq: undefined, _eventTypes: [e.type] })
      continue
    }
    const existing = bySid.get(sid)
    if (!existing) {
      bySid.set(sid, { ...e, _audit: false, _detailSeq: undefined, _eventTypes: [e.type] })
    } else if (paired.has(sid)) {
      // 该 sid 已合并成对，第三次及以后的事件独立成行，防止把已合并行的耗时/费用/还原数覆盖掉
      independent.push({ ...e, _audit: false, _detailSeq: undefined, _eventTypes: [e.type] })
    } else if (
      (existing.type === 'MASK' && e.type === 'RESTORE') ||
      (existing.type === 'RESTORE' && e.type === 'MASK')
    ) {
      // 真正成对的 MASK + RESTORE：合并为一条往返链路行
      const m = existing.type === 'MASK' ? existing : e
      const r = existing.type === 'RESTORE' ? existing : e
      const merged: MergedEvent = {
        ...m,
        _audit: false,
        matched_rules: Array.from(new Set([...getMatchedRules(m), ...getMatchedRules(r)])),
        http_status: r.http_status ?? m.http_status,
        upstream_ms: r.upstream_ms ?? m.upstream_ms,
        total_ms: r.total_ms ?? m.total_ms,
        status: r.status ?? m.status,
        restored: r.restored ?? m.restored,
        unresolved: r.unresolved ?? m.unresolved,
        degraded: r.degraded ?? m.degraded,
        stream_actual: r.stream_actual ?? m.stream_actual,
        cost_usd: r.cost_usd ?? m.cost_usd,
        model: r.model || m.model || '',
        _detailSeq: r.seq,
        _detailId: r.detail_id,
        _eventTypes: ['MASK', 'RESTORE'],
        display_type: r.display_type,
        noise_whitelisted: r.noise_whitelisted ?? m.noise_whitelisted,
        error: r.error || m.error,
        failure_stage: r.failure_stage || m.failure_stage,
        upstream: r.upstream || m.upstream || '',
      }
      bySid.set(sid, merged)
      paired.add(sid)
    } else {
      // 同一 sid 出现两个同类事件时，独立成行，防静默覆盖
      independent.push({ ...e, _audit: false, _detailSeq: undefined, _eventTypes: [e.type] })
    }
  }

  const ev = [...bySid.values(), ...independent].filter((e) => {
    const type = getDisplayEventType(e)
    if (hideNoise && NOISE_TYPES.has(type)) return false
    if (filterType === undefined || filterType === FILTER_ALL) return true
    if (filterType === 'EXT_ALL') return e.ingress === 'ext'
    if (filterType === 'MASK' || filterType === 'RESTORE') return (e._eventTypes ?? [e.type]).includes(filterType) && hasSensitiveEvidence(e)
    if (filterType === 'PASS_COMBINED') return NOISE_TYPES.has(type)
    return type === filterType
  }).map((e) => ({ ...e, type: filterType === 'RESTORE' && e._eventTypes?.includes('RESTORE') ? 'RESTORE' as const : e.type, _audit: false as const }))
  // 按 ts 降序（最新在前）
  return ev.sort((a, b) => (b.ts ?? 0) - (a.ts ?? 0) || b.seq - a.seq || (b.detail_id ?? '').localeCompare(a.detail_id ?? ''))
}
