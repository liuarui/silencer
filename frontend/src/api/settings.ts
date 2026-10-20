/**
 * 配置与设置 API（真实后端契约：/api/config GET/POST + 配套接口）
 */
import { silencerFetch } from '@/lib/silencer-fetch'
import type { SilencerConfig, TodayStats } from '@/types/api'

export interface ChangePasswordResponse { ok: boolean; error?: string }
export function changePassword(currentPassword: string, newPassword: string): Promise<ChangePasswordResponse> {
  return silencerFetch<ChangePasswordResponse>('/api/auth/change-password', {
    method: 'POST',
    body: JSON.stringify({ current_password: currentPassword, new_password: newPassword }),
  })
}

export function getConfig(): Promise<SilencerConfig> {
  return silencerFetch<SilencerConfig>('/api/config')
}

export interface SaveConfigResponse {
  ok: boolean
  config: SilencerConfig
  warnings: string[]
  proxy_restarted: boolean
  error?: string
}

export function saveConfig(cfg: Partial<SilencerConfig>): Promise<SaveConfigResponse> {
  return silencerFetch<SaveConfigResponse>('/api/config', {
    method: 'POST',
    body: JSON.stringify(cfg),
  })
}

/** Update changed rules only; other tabs may have changed the remaining rules. */
export function saveBuiltinRules(changes: Record<string, boolean>): Promise<SaveConfigResponse> {
  return silencerFetch<SaveConfigResponse>('/api/config/builtin_rules', {
    method: 'POST',
    body: JSON.stringify(changes),
  })
}

/**
 * 配置增量操作。POST /api/config 只在顶层合并，凡是「值本身是容器」的字段
 * （audit / egress_proxy / sensitive / upstreams / target_domains …）提交整份
 * 快照都会覆盖掉并发写入；这里改为下发「路径 + 操作」，由服务端持锁局部修改。
 */
export type ConfigPatchOp =
  | 'create_if_absent' // 原子创建，拒绝覆盖已有值
  | 'set'          // 覆盖指定路径的值（path 省略 = 覆盖整个键）
  | 'merge'        // 目标是对象：批量更新其中的键
  | 'map_del'      // 目标是对象：删除 value 列出的键
  | 'list_add'     // 目标是数组：追加 value 中尚不存在的标量项
  | 'list_remove'  // 目标是数组：移除 value 中存在的标量项
  | 'list_upsert'  // 目标是对象数组：按 name 就地更新或追加（match 指定旧名）
  | 'list_del'     // 目标是对象数组：按 name 移除

export interface ConfigPatch {
  key: keyof SilencerConfig | string
  op: ConfigPatchOp
  /** 在 key 对应值内下钻的路径；省略或空数组表示操作该值本身 */
  path?: string[]
  value: unknown
  /** list_upsert 专用：要更新的条目原名（用于改名） */
  match?: string
}

export function patchConfig(patch: ConfigPatch, processingTarget?: string): Promise<SaveConfigResponse> {
  return silencerFetch<SaveConfigResponse>('/api/config/patch', {
    method: 'POST',
    body: JSON.stringify(patch), processingTarget,
  })
}

export function emergencyDisableOriginCheck(customToken?: string): Promise<{ ok: boolean; message: string; config?: SilencerConfig }> {
  return silencerFetch<{ ok: boolean; message: string; config?: SilencerConfig }>('/api/config/disable_origin_check', {
    method: 'POST',
    headers: customToken ? { 'X-Silencer-Token': customToken } : undefined,
  })
}

export function getTodayStats(range?: '7d' | '30d' | string): Promise<TodayStats> {
  const qs = range ? `?range=${range}` : ''
  return silencerFetch<TodayStats>(`/api/stats/today${qs}`)
}

export interface StatsHistoryPoint {
  ts: number
  label: string
  requests?: number
  mask_events?: number
  restored?: number
  alerts?: number
  tokens_prompt?: number
  tokens_completion?: number
  cache_read_tokens?: number
  cache_write_tokens?: number
  cache_observed_requests?: number
  cache_rate_observed_requests?: number
  cache_input_tokens?: number
  /** 审计信号事件数（注入审计/投毒检测） */
  audit_signals?: number
  /** 审计高危（HIGH/CRITICAL）信号数 */
  audit_high?: number
}
export interface StatsHistory {
  ok: boolean
  granularity: string
  days: number
  data: StatsHistoryPoint[]
}
export function getStatsHistory(days = 30, granularity: 'day' | 'hour' = 'day'): Promise<StatsHistory> {
  return silencerFetch<StatsHistory>(`/api/stats/history?days=${days}&granularity=${granularity}`)
}

export interface StatsModel {
  model: string
  requests: number
  prompt: number
  completion: number
  cache_read_tokens?: number
  cache_write_tokens?: number
  cache_observed_requests?: number
  cache_rate_observed_requests?: number
  cache_input_tokens?: number
  cache_hit_rate?: number | null
  cost_usd: number
  priced: boolean
  errors?: number
}

export function getStatsModels(days = 7): Promise<{ ok: boolean; days: number; models: StatsModel[] }> {
  return silencerFetch(`/api/stats/models?days=${days}`)
}

export interface PriceSyncState {
  syncing: boolean
  last_error: string
  synced_at: number
  model_count: number
  source: string
  builtin_count: number
  ok?: boolean
}

export function getPriceSyncStatus(): Promise<PriceSyncState> {
  return silencerFetch<PriceSyncState>('/api/prices/status')
}

export function syncPricesNow(): Promise<{ ok: boolean; error?: string; state?: PriceSyncState }> {
  return silencerFetch('/api/prices/sync', { method: 'POST', timeoutMs: 60000 })
}

export interface PriceEntry {
  model: string
  input: number
  output: number
  cache_read?: number
  cache_write?: number
}

export function getPriceList(): Promise<{ ok: boolean; count: number; models: PriceEntry[] }> {
  return silencerFetch('/api/prices/list')
}

export function getRestoreItems(limit = 200, range = 'today'): Promise<{
  total_events: number
  items: { label: string; preview: string; events: number; cred?: boolean; length?: number; original?: string }[]
}> {
  return silencerFetch(`/api/stats/today/restore-items?limit=${limit}&range=${encodeURIComponent(range)}`)
}

export function testUpstream(payload: {
  name?: string
  port?: number
  mode: 'port' | 'models' | 'chat'
  api_key?: string
  model?: string
  path_prefix?: string
  content?: string
}): Promise<{ ok: boolean; status?: number; mode?: 'port' | 'models' | 'chat'; probe_target?: 'gateway' | 'upstream'; message?: string; error?: string; [k: string]: unknown }> {
  return silencerFetch('/api/upstream/test', { method: 'POST', body: JSON.stringify(payload), timeoutMs: 60000 })
}

export function demoMask(text?: string): Promise<{
  ok: boolean
  masked?: string
  count?: number
  items?: { label: string; token: string }[]
  error?: string
}> {
  return silencerFetch('/api/demo/mask', { method: 'POST', body: JSON.stringify({ text: text ?? '' }) })
}

export function installCert(scope: string): Promise<{ ok: boolean; error?: string; output?: string }> {
  return silencerFetch('/api/cert', { method: 'POST', body: JSON.stringify({ scope }), timeoutMs: 30000 })
}

/** 一键恢复网络/系统代理（/api/restore） */
export function restoreNetwork(): Promise<{ ok: boolean; [k: string]: unknown }> {
  return silencerFetch('/api/restore', { method: 'POST', timeoutMs: 30000 })
}

/** 健康检查（/api/health） */
export interface StorageHealth {
  mode?: string
  queue_depth?: number
  queue_capacity?: number
  last_write_error?: boolean
  open_connections?: number
  in_use?: number
  idle?: number
  wait_count?: number
}
export function getHealth(): Promise<{ ok: boolean; storage?: StorageHealth; storage_error?: string; [k: string]: unknown }> {
  return silencerFetch('/api/health')
}

// ========== 配置备份与回滚（/api/config/backups · /api/config/restore） ==========
export interface ConfigBackup {
  file: string
  /** epoch 秒 */
  mtime: number
  size: number
  upstreams: number
  cats: number
  words: number
  domains: number
}

export interface ConfigBackupsResponse {
  ok: boolean
  backups: ConfigBackup[]
  current: { upstreams?: number; cats?: number; words?: number; domains?: number }
  error?: string
}

/** 列出可回滚的配置备份（含结构摘要，供用户判断回滚到哪一份） */
export function getConfigBackups(): Promise<ConfigBackupsResponse> {
  return silencerFetch<ConfigBackupsResponse>('/api/config/backups')
}

/** 从指定备份回滚配置；回滚本身也会先备份当前配置，滚错了还能滚回来 */
export function restoreConfigBackup(file: string): Promise<{
  ok: boolean
  warnings?: string[]
  proxy_restarted?: boolean
  restored_from?: string
  error?: string
}> {
  return silencerFetch('/api/config/restore', {
    method: 'POST',
    body: JSON.stringify({ file }),
  })
}
