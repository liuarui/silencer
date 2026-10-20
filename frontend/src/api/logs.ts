/**
 * 事件日志 API（真实后端契约：/api/logs 游标 + slim + 详情回源）
 */
import { silencerFetch } from '@/lib/silencer-fetch'
import type { LogDetailResponse, LogsResponse } from '@/types/api'

export interface LogsParams {
  /** Legacy timestamp cursor; current log pages use latest snapshots. */
  since?: number
  latest?: boolean
  chains?: boolean
  hideNoise?: boolean
  includeAudit?: boolean
  limit?: number
  /** 事件类型（精确过滤，如下推 ERR / BLOCK / SCAN_WARN / MASK / RESTORE 等） */
  type?: string
  /** Only events with sensitive evidence. */
  sensitive?: boolean
  /** 搜索词（结构化列） */
  q?: string
  /** 全文搜索（扫 payload，性能重） */
  fulltext?: boolean
  /**
   * 入口维度过滤：`proxy`（CLI 代理链路）/ `ext`（浏览器扩展链路）。
   * 与词榜分组**同口径**——首页词条跳转必须带上它，否则「词条 ×N」与
   * 「点进去的日志条数」对不上，用户会判定统计坏了。
   */
  ingress?: 'proxy' | 'ext' | 'capture'
  /** 列表轻量模式（默认 true，明文只走详情回源） */
  slim?: boolean
}

export function getLogs(params: LogsParams, signal?: AbortSignal): Promise<LogsResponse> {
  const search = new URLSearchParams()
  if (params.since) search.set('since', String(params.since))
  if (params.latest) search.set('latest', '1')
  if (params.chains) search.set('chains', '1')
  if (params.hideNoise) search.set('hide_noise', '1')
  if (params.includeAudit) search.set('include_audit', '1')
  if (params.limit) search.set('limit', String(params.limit))
  if (params.type) search.set('type', params.type)
  if (params.sensitive) search.set('sensitive', '1')
  if (params.q) search.set('q', params.q)
  if (params.fulltext) search.set('fulltext', '1')
  if (params.ingress) search.set('ingress', params.ingress)
  if (params.slim !== false) {
    search.set('slim', '1')
    search.set('events_only', '1')
  }
  const qs = search.toString()
  return silencerFetch<LogsResponse>(`/api/logs${qs ? `?${qs}` : ''}`, { signal })
}

/** 单条事件详情（包含正文预览和逐词明文） */
export function getLogDetail(reference: number | string): Promise<LogDetailResponse> {
  const query = new URLSearchParams({ [typeof reference === 'string' ? 'id' : 'seq']: String(reference) })
  return silencerFetch<LogDetailResponse>(`/api/logs/detail?${query.toString()}`)
}

/** Export retained event metadata, including per-token originals when recorded. */
export function exportLogs(params: Omit<LogsParams, 'since' | 'slim'>): Promise<Response> {
  const search = new URLSearchParams()
  search.set('latest', '1')
  if (params.chains) search.set('chains', '1')
  if (params.hideNoise) search.set('hide_noise', '1')
  if (params.includeAudit) search.set('include_audit', '1')
  if (params.type) search.set('type', params.type)
  if (params.sensitive) search.set('sensitive', '1')
  if (params.q) search.set('q', params.q)
  if (params.fulltext) search.set('fulltext', '1')
  if (params.limit) search.set('limit', String(params.limit))
  if (params.ingress) search.set('ingress', params.ingress)
  const qs = search.toString()
  return silencerFetch<Response>(`/api/logs/export${qs ? `?${qs}` : ''}`, { raw: true, timeoutMs: 0 })
}

export function clearLogs(): Promise<{ ok: boolean; error?: string }> {
  return silencerFetch('/api/logs/clear', { method: 'POST' })
}
