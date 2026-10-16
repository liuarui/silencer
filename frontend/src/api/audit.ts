/**
 * 审计 API（真实后端契约：/api/audit/*）
 * - /api/audit/events 返回 {events, count}
 * - /api/audit/run 只有在构建提供主动探针执行器时才会启动；当前 Go 网关明确返回能力不可用
 */
import { silencerFetch } from '@/lib/silencer-fetch'
import type { AuditEventsResponse, AuditJob } from '@/types/api'

/**
 * 审计事件列表。
 *
 * floor/signal 直接对应后端 `/api/audit/events` 的服务端过滤：
 * - floor：严重度门槛（LOW/MEDIUM/HIGH/CRITICAL）。默认事件视图传用户档位，
 *   「高风险操作时间线」视图传 LOW（视图级放宽，不影响默认视图）；
 * - signal：信号类型（如 dangerous_action，时间线专用）。
 * 不传时后端不过滤（把所有已落库事件都返回）。
 */
export function getAuditEvents(
  since = 0,
  limit = 500,
  opts: { floor?: string; signal?: string } = {},
  signal?: AbortSignal,
): Promise<AuditEventsResponse> {
  const qs = new URLSearchParams({ since: String(since), limit: String(limit) })
  if (opts.floor) qs.set('floor', opts.floor)
  if (opts.signal) qs.set('signal', opts.signal)
  return silencerFetch<AuditEventsResponse>(`/api/audit/events?${qs.toString()}`, { signal })
}

export function getAuditJob(): Promise<AuditJob> {
  return silencerFetch<AuditJob>('/api/audit/job')
}

export interface RunAuditOptions {
  confirm?: boolean
  upstream_name?: string
  model?: string
  profile?: string
}

export function runAudit(opts: RunAuditOptions = {}): Promise<{ ok: boolean; error?: string }> {
  return silencerFetch('/api/audit/run', {
    method: 'POST',
    body: JSON.stringify({
      confirm: opts.confirm ?? true,
      ...(opts.upstream_name ? { upstream_name: opts.upstream_name } : {}),
      ...(opts.model ? { model: opts.model } : {}),
      ...(opts.profile ? { profile: opts.profile } : {}),
    }),
  })
}

export function cancelAudit(): Promise<{ ok: boolean }> {
  return silencerFetch('/api/audit/cancel', { method: 'POST' })
}

export function clearAudit(): Promise<{ ok: boolean }> {
  return silencerFetch('/api/audit/clear', { method: 'POST' })
}

export function getAuditReport(): Promise<{ ok: boolean; report?: string; error?: string }> {
  return silencerFetch('/api/audit/report/latest')
}
